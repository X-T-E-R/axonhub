package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func TestObservationTerminalUsageDoesNotRequireNormalizedDrain(t *testing.T) {
	t.Run("responses terminal contains a large output", func(t *testing.T) {
		var captured streamObservationMeta
		event := &httpclient.StreamEvent{Data: []byte(`{"type":"response.completed","response":{"id":"resp-terminal","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"` + strings.Repeat("x", 1<<20) + `"}]}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":3}}}}`)}
		captured.observe(event)
		require.Less(t, len(captured.final.Data), 1024)
		_, meta, err := responses.AggregateStreamChunks(t.Context(), captured.chunks())
		require.NoError(t, err)
		require.Equal(t, "resp-terminal", meta.ID)
		require.Equal(t, int64(18), meta.Usage.TotalTokens)
		require.Equal(t, int64(3), meta.Usage.PromptTokensDetails.CachedTokens)
	})
	t.Run("anthropic merges several small billing updates", func(t *testing.T) {
		var captured streamObservationMeta
		for _, data := range []string{
			`{"type":"message_start","message":{"id":"msg-terminal","usage":{"input_tokens":11,"output_tokens":0,"cache_read_input_tokens":3},"content":[]}}`,
			`{"type":"message_delta","usage":{"output_tokens":4,"cache_creation_input_tokens":2}}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}`,
		} {
			captured.observe(&httpclient.StreamEvent{Data: []byte(data)})
		}
		require.Len(t, captured.chunks(), 2)
		_, meta, err := anthropic.AggregateStreamChunks(t.Context(), captured.chunks(), anthropic.PlatformDirect)
		require.NoError(t, err)
		require.Equal(t, "msg-terminal", meta.ID)
		require.Equal(t, int64(16), meta.Usage.PromptTokens)
		require.Equal(t, int64(7), meta.Usage.CompletionTokens)
		require.Equal(t, int64(23), meta.Usage.TotalTokens)
	})
}

func TestObservationMalformedAnthropicStartDoesNotInterruptStream(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, start := range []struct{ name, eventType, data string }{
			{"missing", "", `{"type":"message_start"}`},
			{"null", "", `{"type":"message_start","message":null}`},
			{"delta-header-missing", "message_delta", `{"type":"message_start"}`},
			{"delta-header-null", "message_delta", `{"type":"message_start","message":null}`},
			{"response-header-null", "response.completed", `{"type":"message_start","message":null}`},
			{"duplicate-json-type", "message_delta", `{"type":"message_delta","type":"message_start","message":null}`},
		} {
			for _, deltas := range []int{1, 2} {
				t.Run(fmt.Sprintf("enabled=%v/start=%s/deltas=%d", enabled, start.name, deltas), func(t *testing.T) {
					writer := biz.NewForwardingObservationWriter(biz.ManagedRequestBodyWriterConfig{})
					require.NoError(t, writer.Start(t.Context()))
					ctx := writer.WithScope(t.Context())
					t.Cleanup(func() {
						biz.EndForwardingObservation(ctx)
						stop, cancel := context.WithTimeout(context.Background(), time.Second)
						defer cancel()
						require.NoError(t, writer.Stop(stop))
					})
					channel := &biz.Channel{Channel: &ent.Channel{ID: 1, Settings: &objects.ChannelSettings{
						StoreExecutionResponseBody: lo.ToPtr(enabled), StoreExecutionStreamChunks: lo.ToPtr(enabled),
					}}}
					state := &PersistenceState{CurrentCandidate: &ChannelModelsCandidate{Channel: channel}}
					provider, err := anthropic.NewOutboundTransformer("https://unused.invalid", "fixture")
					require.NoError(t, err)
					events := []*httpclient.StreamEvent{{Type: start.eventType, Data: []byte(start.data)}}
					for i := 1; i <= deltas; i++ {
						events = append(events, &httpclient.StreamEvent{Data: []byte(fmt.Sprintf(`{"type":"message_delta","usage":{"output_tokens":%d}}`, i))})
					}
					events = append(events, &httpclient.StreamEvent{Data: []byte(`{"type":"message_stop"}`)})
					svc := &biz.RequestService{SystemService: &biz.SystemService{}}
					stream := NewOutboundPersistentStream(ctx, streams.SliceStream(events), nil, nil, svc, nil, provider, nil, state)
					// Cover the enabled-but-unavailable shortcut as well as disabled
					// capture. Only optional evidence is unavailable, not raw frames.
					stream.observation.Unavailable = enabled
					require.NotPanics(t, func() {
						seen := 0
						for stream.Next() {
							require.Same(t, events[seen], stream.Current())
							seen++
						}
						require.Equal(t, len(events), seen)
						require.True(t, stream.streamCompleted)
						require.NoError(t, stream.Close())
					})
					_, meta, err := provider.AggregateStreamChunks(ctx, nil, stream.usageMeta.chunks())
					require.NoError(t, err)
					require.Equal(t, int64(deltas), meta.Usage.CompletionTokens)
				})
			}
		}
	}
}

func TestObservationMetadataUsesJSONType(t *testing.T) {
	var captured streamObservationMeta
	captured.observe(&httpclient.StreamEvent{
		Type: "response.completed",
		Data: []byte(`{"type":"message_start","message":{"id":"msg-json-type","usage":{"input_tokens":5}}}`),
	})
	captured.observe(&httpclient.StreamEvent{
		Type: "message_start",
		Data: []byte(`{"type":"message_delta","usage":{"output_tokens":2}}`),
	})
	require.Len(t, captured.chunks(), 2)
	_, meta, err := anthropic.AggregateStreamChunks(t.Context(), captured.chunks(), anthropic.PlatformDirect)
	require.NoError(t, err)
	require.Equal(t, "msg-json-type", meta.ID)
	require.Equal(t, int64(7), meta.Usage.TotalTokens)
}
