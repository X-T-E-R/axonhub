package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

type recordingStreamLivenessObserver struct {
	selectedAttempts []StreamLivenessAttempt
	attempts         []StreamLivenessAttempt
	rawEvents        int
	transformable    int
	upstreamErr      error
}

func (o *recordingStreamLivenessObserver) OnUpstreamAttemptSelected(attempt StreamLivenessAttempt) {
	o.selectedAttempts = append(o.selectedAttempts, attempt)
}

type countingEventStream struct {
	streams.Stream[*httpclient.StreamEvent]
	closeCount int
}

func (s *countingEventStream) Close() error {
	s.closeCount++
	return s.Stream.Close()
}

func (o *recordingStreamLivenessObserver) OnUpstreamResponseHeaders(attempt StreamLivenessAttempt) {
	o.attempts = append(o.attempts, attempt)
}

func (o *recordingStreamLivenessObserver) OnRawSSE(*httpclient.StreamEvent) { o.rawEvents++ }
func (o *recordingStreamLivenessObserver) OnTransformableEvent(*llm.Response) {
	o.transformable++
}
func (o *recordingStreamLivenessObserver) OnUpstreamError(err error) { o.upstreamErr = err }

func TestStreamLivenessMiddleware_ReportsTransportMilestonesWithoutChangingEvents(t *testing.T) {
	enabled := true
	wantsStream := true
	intervalSeconds := 20
	settings := &objects.ChannelSSEKeepAlive{
		Enabled:         &enabled,
		IntervalSeconds: &intervalSeconds,
	}
	state := &PersistenceState{
		OriginalRequestStream: &wantsStream,
		CurrentCandidate: &ChannelModelsCandidate{
			Channel: &biz.Channel{Channel: &ent.Channel{
				ID:       7,
				Name:     "DeepSeek Pro",
				Settings: &objects.ChannelSettings{SSEKeepAlive: settings},
			}},
		},
	}
	observer := &recordingStreamLivenessObserver{}
	middleware := &streamLivenessMiddleware{
		state:    state,
		observer: observer,
	}

	rawEvent := &httpclient.StreamEvent{Type: "ping", Data: []byte(`{"type":"ping"}`)}
	rawSource := &countingEventStream{Stream: streams.SliceStream([]*httpclient.StreamEvent{rawEvent})}
	raw, err := middleware.OnOutboundRawStream(context.Background(), rawSource)
	require.NoError(t, err)
	require.True(t, raw.Next())
	require.Same(t, rawEvent, raw.Current())
	require.NoError(t, raw.Close())
	require.NoError(t, raw.Close())

	llmEvent := &llm.Response{Object: "chat.completion.chunk"}
	transformed, err := middleware.OnOutboundLlmStream(context.Background(), streams.SliceStream([]*llm.Response{llmEvent}))
	require.NoError(t, err)
	require.True(t, transformed.Next())
	require.Same(t, llmEvent, transformed.Current())

	upstreamErr := errors.New("upstream unavailable")
	middleware.OnOutboundRawError(context.Background(), upstreamErr)

	require.Len(t, observer.attempts, 1)
	require.Equal(t, 7, observer.attempts[0].ChannelID)
	require.Equal(t, "DeepSeek Pro", observer.attempts[0].ChannelName)
	require.Same(t, settings, observer.attempts[0].KeepAlive)
	require.NotNil(t, observer.attempts[0].ConfirmSemanticCompletion)
	require.False(t, state.StreamCompleted)
	require.True(t, observer.attempts[0].ConfirmSemanticCompletion())
	require.True(t, state.StreamCompleted)
	require.Equal(t, 1, observer.rawEvents)
	require.Equal(t, 1, observer.transformable)
	require.ErrorIs(t, observer.upstreamErr, upstreamErr)
	require.Equal(t, 1, rawSource.closeCount)
}

func TestStreamLivenessSelectionMiddleware_ReportsChannelPolicyBeforeProviderSetup(t *testing.T) {
	disabled := false
	wantsStream := true
	settings := &objects.ChannelSSEKeepAlive{Enabled: &disabled}
	state := &PersistenceState{
		OriginalRequestStream: &wantsStream,
		CurrentCandidate: &ChannelModelsCandidate{
			Channel: &biz.Channel{Channel: &ent.Channel{
				ID:       7,
				Name:     "DeepSeek Pro",
				Settings: &objects.ChannelSettings{SSEKeepAlive: settings},
			}},
		},
	}
	observer := &recordingStreamLivenessObserver{}
	middleware := newStreamLivenessSelectionMiddleware(state, observer)
	request := &httpclient.Request{}

	got, err := middleware.OnOutboundRawRequest(context.Background(), request)
	require.NoError(t, err)
	require.Same(t, request, got)
	require.Len(t, observer.selectedAttempts, 1)
	require.Equal(t, 7, observer.selectedAttempts[0].ChannelID)
	require.Equal(t, "DeepSeek Pro", observer.selectedAttempts[0].ChannelName)
	require.Same(t, settings, observer.selectedAttempts[0].KeepAlive)
	require.Empty(t, observer.attempts, "provider response headers have not arrived")

	wantsStream = false
	_, err = middleware.OnOutboundRawRequest(context.Background(), request)
	require.NoError(t, err)
	require.Len(t, observer.selectedAttempts, 1, "non-streaming request must not start SSE liveness")
}

func TestStreamLivenessMiddleware_DoesNotReportProviderOnlyAutoAggregateStream(t *testing.T) {
	wantsStream := false
	observer := &recordingStreamLivenessObserver{}
	middleware := &streamLivenessMiddleware{
		state: &PersistenceState{
			OriginalRequestStream: &wantsStream,
		},
		observer: observer,
	}
	source := streams.SliceStream([]*httpclient.StreamEvent{{Data: []byte(`{"provider":"chunk"}`)}})

	got, err := middleware.OnOutboundRawStream(context.Background(), source)
	require.NoError(t, err)
	require.Same(t, source, got)
	require.Empty(t, observer.attempts)

	middleware.OnOutboundRawError(context.Background(), errors.New("provider stream failed"))
	require.NoError(t, observer.upstreamErr)
}

func TestClassifyStreamSemanticTerminal_ProtocolAwareStatuses(t *testing.T) {
	tests := []struct {
		name   string
		event  *httpclient.StreamEvent
		status StreamSemanticStatus
	}{
		{
			name:   "chat tool calls completed",
			event:  &httpclient.StreamEvent{Data: []byte(`{"choices":[{"finish_reason":"tool_calls"}],"usage":{"completion_tokens":9}}`)},
			status: StreamSemanticSucceeded,
		},
		{
			name:   "responses completed snapshot",
			event:  &httpclient.StreamEvent{Type: "response.in_progress", Data: []byte(`{"type":"response.in_progress","response":{"status":"completed","usage":{"output_tokens":9},"output":[{"status":"completed"}]}}`)},
			status: StreamSemanticSucceeded,
		},
		{
			name:   "responses failed",
			event:  &httpclient.StreamEvent{Type: "response.failed", Data: []byte(`{"type":"response.failed","response":{"status":"failed"}}`)},
			status: StreamSemanticFailed,
		},
		{
			name:   "response completed event carrying incomplete status",
			event:  &httpclient.StreamEvent{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"status":"incomplete"}}`)},
			status: StreamSemanticIncomplete,
		},
		{
			name:   "responses incomplete",
			event:  &httpclient.StreamEvent{Type: "response.incomplete", Data: []byte(`{"type":"response.incomplete","response":{"status":"incomplete"}}`)},
			status: StreamSemanticIncomplete,
		},
		{
			name:   "chat length is incomplete",
			event:  &httpclient.StreamEvent{Data: []byte(`{"choices":[{"finish_reason":"length"}]}`)},
			status: StreamSemanticIncomplete,
		},
		{
			name:   "usage alone is not terminal",
			event:  &httpclient.StreamEvent{Data: []byte(`{"choices":[],"usage":{"completion_tokens":9}}`)},
			status: StreamSemanticNone,
		},
		{
			name:   "ordinary delta is not terminal",
			event:  &httpclient.StreamEvent{Data: []byte(`{"choices":[{"delta":{"content":"working"},"finish_reason":null}]}`)},
			status: StreamSemanticNone,
		},
		{
			name:   "mixed completed and unfinished choices are not terminal",
			event:  &httpclient.StreamEvent{Data: []byte(`{"choices":[{"finish_reason":"stop"},{"finish_reason":null}]}`)},
			status: StreamSemanticNone,
		},
		{
			name:   "mixed success and incomplete choices are incomplete",
			event:  &httpclient.StreamEvent{Data: []byte(`{"choices":[{"finish_reason":"stop"},{"finish_reason":"length"}]}`)},
			status: StreamSemanticIncomplete,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.status, ClassifyStreamSemanticTerminal(tt.event))
		})
	}
}

func TestClassifyLLMStreamSemanticTerminal_DistinguishesSuccessAndIncomplete(t *testing.T) {
	toolCalls := "tool_calls"
	length := "length"
	require.Equal(t, StreamSemanticSucceeded, ClassifyLLMStreamSemanticTerminal(&llm.Response{
		Choices: []llm.Choice{{FinishReason: &toolCalls}},
		Usage:   &llm.Usage{CompletionTokens: 9},
	}))
	require.Equal(t, StreamSemanticIncomplete, ClassifyLLMStreamSemanticTerminal(&llm.Response{
		Choices: []llm.Choice{{FinishReason: &length}},
	}))
	require.Equal(t, StreamSemanticNone, ClassifyLLMStreamSemanticTerminal(&llm.Response{
		Usage: &llm.Usage{CompletionTokens: 9},
	}))
}

func TestPersistenceState_SemanticConfirmationAndDeferredFailureAreFirstWriterWins(t *testing.T) {
	confirmed := &PersistenceState{}
	require.True(t, confirmed.confirmStreamCompletion())
	require.True(t, confirmed.beginSemanticCompletionInterrupt())
	require.True(t, confirmed.isSemanticCompletionInterruptStarted())

	providerAfterInterrupt := &PersistenceState{}
	require.True(t, providerAfterInterrupt.confirmStreamCompletion())
	require.True(t, providerAfterInterrupt.beginSemanticCompletionInterrupt())
	providerRaceErr := context.Canceled
	require.True(t, providerAfterInterrupt.reserveRawStreamFailure(providerRaceErr))
	recorded, _, _ := providerAfterInterrupt.deferredStreamFailure()
	require.ErrorIs(t, recorded, providerRaceErr)

	providerErr := errors.New("provider failed during grace")
	failed := &PersistenceState{}
	require.True(t, failed.reserveRawStreamFailure(providerErr))
	require.False(t, failed.confirmStreamCompletion())
	require.False(t, failed.StreamCompleted)
	recorded, _, _ = failed.deferredStreamFailure()
	require.ErrorIs(t, recorded, providerErr)

	errorBeforeInterrupt := &PersistenceState{}
	require.True(t, errorBeforeInterrupt.confirmStreamCompletion())
	require.True(t, errorBeforeInterrupt.reserveRawStreamFailure(providerErr),
		"a genuine error observed before the deliberate interrupt must still win")
	require.False(t, errorBeforeInterrupt.beginSemanticCompletionInterrupt())

	transformFailure := &PersistenceState{}
	require.True(t, transformFailure.confirmStreamCompletion())
	transformFailure.reserveDeferredStreamFailure(errors.New("final transform failed before interrupt"))
	require.False(t, transformFailure.beginSemanticCompletionInterrupt(),
		"a deferred transform error reserved by the first liveness middleware must supersede confirmation")
}
