package orchestrator

import (
	"bytes"
	"context"
	"strings"
	"sync"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
)

// StreamLivenessAttempt describes the selected provider attempt. Interrupt and
// ConfirmSemanticCompletion become available after response headers are accepted.
// It intentionally excludes request and response bodies.
type StreamLivenessAttempt struct {
	ChannelID   int
	ChannelName string
	KeepAlive   *objects.ChannelSSEKeepAlive
	// Interrupt targets only the concurrency-safe raw decoder. It must not
	// close terminal/persistence wrappers while their reader owns Next/Current.
	Interrupt func() error
	// ConfirmSemanticCompletion promotes a previously observed semantic success
	// only after the downstream terminal grace expires. This keeps a later
	// provider error authoritative when it arrives during the grace window.
	ConfirmSemanticCompletion func() bool
	// RecordClientTerminalFlushed records the point after the official terminal
	// frame has been written and flushed to the downstream client.
	RecordClientTerminalFlushed func()
}

type StreamSemanticStatus uint8

const (
	StreamSemanticNone StreamSemanticStatus = iota
	StreamSemanticSucceeded
	StreamSemanticFailed
	StreamSemanticIncomplete
)

// StreamLivenessObserver receives transport-only milestones. Implementations
// must not turn these callbacks into semantic stream events or retry signals.
type StreamLivenessObserver interface {
	OnUpstreamAttemptSelected(StreamLivenessAttempt)
	OnUpstreamResponseHeaders(StreamLivenessAttempt)
	OnRawSSE(*httpclient.StreamEvent)
	OnTransformableEvent(*llm.Response)
	OnUpstreamError(error)
}

type streamLivenessSelectionMiddleware struct {
	*pipeline.DummyMiddleware
	state    *PersistenceState
	observer StreamLivenessObserver
}

type downstreamCommitObserver interface {
	IsDownstreamCommitted() bool
}

type streamLivenessMiddleware struct {
	*pipeline.DummyMiddleware
	state    *PersistenceState
	observer StreamLivenessObserver
}

type livenessObservedStream[T any] struct {
	stream        streams.Stream[T]
	onCurrent     func(T)
	onTerminalErr func(error)
	closeOnce     sync.Once
	closeErr      error
}

func (s *livenessObservedStream[T]) Next() bool {
	if s.stream.Next() {
		return true
	}
	if err := s.stream.Err(); err != nil && s.onTerminalErr != nil {
		s.onTerminalErr(err)
	}
	return false
}
func (s *livenessObservedStream[T]) Current() T {
	current := s.stream.Current()
	s.onCurrent(current)
	return current
}
func (s *livenessObservedStream[T]) Err() error {
	return s.stream.Err()
}
func (s *livenessObservedStream[T]) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.stream.Close() })
	return s.closeErr
}

func newStreamLivenessMiddleware(state *PersistenceState, observer StreamLivenessObserver) pipeline.Middleware {
	return &streamLivenessMiddleware{
		DummyMiddleware: &pipeline.DummyMiddleware{},
		state:           state,
		observer:        observer,
	}
}

func newStreamLivenessSelectionMiddleware(state *PersistenceState, observer StreamLivenessObserver) pipeline.Middleware {
	return &streamLivenessSelectionMiddleware{
		DummyMiddleware: &pipeline.DummyMiddleware{},
		state:           state,
		observer:        observer,
	}
}

func (m *streamLivenessSelectionMiddleware) Name() string {
	return "stream_liveness_selection"
}

func (m *streamLivenessSelectionMiddleware) OnOutboundRawRequest(
	_ context.Context,
	request *httpclient.Request,
) (*httpclient.Request, error) {
	if downstreamWantsStream(m.state) {
		m.observer.OnUpstreamAttemptSelected(streamLivenessAttemptFromState(m.state))
	}
	return request, nil
}

func (m *streamLivenessMiddleware) Name() string {
	return "stream_liveness"
}

func (m *streamLivenessMiddleware) downstreamWantsStream() bool {
	return downstreamWantsStream(m.state)
}

func downstreamWantsStream(state *PersistenceState) bool {
	return state != nil && state.OriginalRequestStream != nil && *state.OriginalRequestStream
}

func streamLivenessAttemptFromState(state *PersistenceState) StreamLivenessAttempt {
	attempt := StreamLivenessAttempt{}
	if state == nil || state.CurrentCandidate == nil || state.CurrentCandidate.Channel == nil {
		return attempt
	}

	channel := state.CurrentCandidate.Channel
	attempt.ChannelID = channel.ID
	attempt.ChannelName = channel.Name
	if channel.Settings != nil {
		attempt.KeepAlive = channel.Settings.SSEKeepAlive
	}
	return attempt
}

func (m *streamLivenessMiddleware) OnOutboundRawStream(
	ctx context.Context,
	stream streams.Stream[*httpclient.StreamEvent],
) (streams.Stream[*httpclient.StreamEvent], error) {
	if !m.downstreamWantsStream() {
		return stream, nil
	}

	attempt := streamLivenessAttemptFromState(m.state)
	if interruptible, ok := stream.(streams.Interruptible); ok {
		attempt.Interrupt = func() error {
			if m.state != nil {
				m.state.beginSemanticCompletionInterrupt()
			}
			return interruptible.Interrupt()
		}
	}
	attempt.ConfirmSemanticCompletion = func() bool {
		return m.state != nil && m.state.confirmStreamCompletion()
	}
	attempt.RecordClientTerminalFlushed = func() {
		if m.state != nil {
			m.state.recordStreamLifecycle(ctx, "client_terminal_flushed")
		}
	}
	m.observer.OnUpstreamResponseHeaders(attempt)

	return &livenessObservedStream[*httpclient.StreamEvent]{
		stream:    stream,
		onCurrent: m.observer.OnRawSSE,
		onTerminalErr: func(err error) {
			if m.state != nil {
				m.state.reserveRawStreamFailure(err)
			}
		},
	}, nil
}

// ClassifyStreamSemanticTerminal recognizes protocol-level terminal meaning.
// Usage by itself is deliberately ignored: providers may report partial usage
// before a stream has reached a terminal state.
func ClassifyStreamSemanticTerminal(event *httpclient.StreamEvent) StreamSemanticStatus {
	if event == nil {
		return StreamSemanticNone
	}

	if bytes.Equal(bytes.TrimSpace(event.Data), []byte("[DONE]")) {
		return StreamSemanticSucceeded
	}

	typeName := event.Type
	if typeName == "" {
		typeName = gjson.GetBytes(event.Data, "type").String()
	}
	switch typeName {
	case "response.completed":
		if status := gjson.GetBytes(event.Data, "response.status").String(); status != "" {
			return classifySemanticStatus(status)
		}
		return StreamSemanticSucceeded
	case "message_stop":
		return StreamSemanticSucceeded
	case "response.failed", "error":
		return StreamSemanticFailed
	case "response.incomplete", "response.cancelled", "response.canceled":
		return StreamSemanticIncomplete
	}

	if status := gjson.GetBytes(event.Data, "response.status").String(); status != "" {
		return classifySemanticStatus(status)
	}
	if gjson.GetBytes(event.Data, "object").String() == "response" || gjson.GetBytes(event.Data, "output").IsArray() {
		if status := gjson.GetBytes(event.Data, "status").String(); status != "" {
			return classifySemanticStatus(status)
		}
	}
	if typeName == "message_delta" {
		return classifyFinishReason(gjson.GetBytes(event.Data, "delta.stop_reason").String())
	}
	if stopReason := gjson.GetBytes(event.Data, "stop_reason").String(); stopReason != "" {
		return classifyFinishReason(stopReason)
	}

	choices := gjson.GetBytes(event.Data, "choices")
	if choices.IsArray() {
		return classifyJSONFinishReasons(choices.Array(), "finish_reason")
	}

	candidates := gjson.GetBytes(event.Data, "candidates")
	if candidates.IsArray() {
		return classifyJSONFinishReasons(candidates.Array(), "finishReason")
	}

	return StreamSemanticNone
}

func classifyJSONFinishReasons(items []gjson.Result, field string) StreamSemanticStatus {
	if len(items) == 0 {
		return StreamSemanticNone
	}

	status := StreamSemanticNone
	for _, item := range items {
		finishReason := item.Get(field)
		if !finishReason.Exists() || finishReason.Type == gjson.Null || finishReason.String() == "" {
			return StreamSemanticNone
		}
		current := classifyFinishReason(finishReason.String())
		if current == StreamSemanticNone {
			return StreamSemanticNone
		}
		if status == StreamSemanticNone {
			status = current
		} else if status != current {
			return StreamSemanticIncomplete
		}
	}

	return status
}

func ClassifyLLMStreamSemanticTerminal(response *llm.Response) StreamSemanticStatus {
	if response == nil {
		return StreamSemanticNone
	}
	if response.Error != nil {
		return StreamSemanticFailed
	}
	if len(response.Choices) == 0 {
		return StreamSemanticNone
	}

	status := StreamSemanticNone
	for _, choice := range response.Choices {
		if choice.FinishReason == nil || *choice.FinishReason == "" {
			return StreamSemanticNone
		}
		current := classifyFinishReason(*choice.FinishReason)
		if current == StreamSemanticNone {
			return StreamSemanticNone
		}
		if status == StreamSemanticNone {
			status = current
		} else if status != current {
			return StreamSemanticIncomplete
		}
	}

	return status
}

func classifySemanticStatus(status string) StreamSemanticStatus {
	switch status {
	case "completed":
		return StreamSemanticSucceeded
	case "failed", "error":
		return StreamSemanticFailed
	case "incomplete", "cancelled", "canceled":
		return StreamSemanticIncomplete
	default:
		return StreamSemanticNone
	}
}

func classifyFinishReason(reason string) StreamSemanticStatus {
	switch strings.ToLower(reason) {
	case "stop", "tool_calls", "function_call", "end_turn", "stop_sequence", "tool_use":
		return StreamSemanticSucceeded
	case "error", "failed":
		return StreamSemanticFailed
	case "length", "max_tokens", "content_filter", "cancelled", "canceled":
		return StreamSemanticIncomplete
	default:
		return StreamSemanticNone
	}
}

func (m *streamLivenessMiddleware) OnOutboundLlmStream(
	_ context.Context,
	stream streams.Stream[*llm.Response],
) (streams.Stream[*llm.Response], error) {
	if !m.downstreamWantsStream() {
		return stream, nil
	}

	return &livenessObservedStream[*llm.Response]{
		stream:    stream,
		onCurrent: m.observer.OnTransformableEvent,
	}, nil
}

func (m *streamLivenessMiddleware) OnOutboundRawError(_ context.Context, err error) {
	if !m.downstreamWantsStream() {
		return
	}
	if pipeline.IsDeferredStreamError(err) && m.state != nil {
		m.state.reserveDeferredStreamFailure(err)
	}
	m.observer.OnUpstreamError(err)
}
