package aisdk

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

func toolChunk(index int, id, name, arguments string) *llm.Response {
	return &llm.Response{
		ID:     "resp_integrity",
		Object: "chat.completion.chunk",
		Choices: []llm.Choice{{
			Index: 0,
			Delta: &llm.Message{ToolCalls: []llm.ToolCall{{
				Index: index,
				ID:    id,
				Type:  "function",
				Function: llm.FunctionCall{
					Name:      name,
					Arguments: arguments,
				},
			}}},
		}},
	}
}

func TestDataStreamToolCallIntegrity_InterleavedFragments(t *testing.T) {
	finishReason := "tool_calls"
	source := streams.SliceStream([]*llm.Response{
		toolChunk(0, "call_spawn", "spawn_agent", `{"task":`),
		toolChunk(1, "call_wait", "wait_agent", `{"ms":`),
		toolChunk(0, "", "", `"audit"}`),
		toolChunk(1, "", "", `1}`),
		{Choices: []llm.Choice{{Index: 0, FinishReason: &finishReason}}},
	})

	stream, err := NewDataStreamTransformer().TransformStream(t.Context(), source)
	require.NoError(t, err)
	events, err := streams.All(stream)
	require.NoError(t, err)

	got := map[string]StreamEvent{}
	for _, event := range events {
		if event == nil || string(event.Data) == "[DONE]" {
			continue
		}
		var decoded StreamEvent
		require.NoError(t, json.Unmarshal(event.Data, &decoded))
		if decoded.Type == "tool-input-available" {
			got[decoded.ToolCallID] = decoded
		}
	}

	require.Equal(t, "spawn_agent", got["call_spawn"].ToolName)
	require.JSONEq(t, `{"task":"audit"}`, string(got["call_spawn"].Input))
	require.Equal(t, "wait_agent", got["call_wait"].ToolName)
	require.JSONEq(t, `{"ms":1}`, string(got["call_wait"].Input))
}

func TestDataStreamToolCallIntegrity_IncompleteArgumentsFail(t *testing.T) {
	finishReason := "tool_calls"
	source := streams.SliceStream([]*llm.Response{
		toolChunk(0, "call_empty", "spawn_agent", ""),
		{Choices: []llm.Choice{{Index: 0, FinishReason: &finishReason}}},
	})

	stream, err := NewDataStreamTransformer().TransformStream(t.Context(), source)
	require.NoError(t, err)
	_, err = streams.All(stream)
	require.ErrorIs(t, err, transformer.ErrIncompleteToolCall)
}

func TestDataStreamToolCallIntegrity_LateProviderIDIsPreserved(t *testing.T) {
	finishReason := "tool_calls"
	source := streams.SliceStream([]*llm.Response{
		toolChunk(0, "", "spawn_agent", `{"task":`),
		toolChunk(0, "call_late", "", `"audit"}`),
		{Choices: []llm.Choice{{Index: 0, FinishReason: &finishReason}}},
	})

	stream, err := NewDataStreamTransformer().TransformStream(t.Context(), source)
	require.NoError(t, err)
	events, err := streams.All(stream)
	require.NoError(t, err)

	for _, event := range events {
		if event == nil || string(event.Data) == "[DONE]" {
			continue
		}
		var decoded StreamEvent
		require.NoError(t, json.Unmarshal(event.Data, &decoded))
		if decoded.Type == "tool-input-available" {
			require.Equal(t, "call_late", decoded.ToolCallID)
			require.JSONEq(t, `{"task":"audit"}`, string(decoded.Input))
			return
		}
	}
	t.Fatal("missing tool-input-available")
}

func TestDataStreamToolCallIntegrity_AcceptsArbitraryJSONValues(t *testing.T) {
	for _, arguments := range []string{`[]`, `"scalar"`, `42`, `true`, `null`} {
		t.Run(arguments, func(t *testing.T) {
			finishReason := "tool_calls"
			source := streams.SliceStream([]*llm.Response{
				toolChunk(0, "call_json", "emit_value", arguments),
				{Choices: []llm.Choice{{Index: 0, FinishReason: &finishReason}}},
			})

			stream, err := NewDataStreamTransformer().TransformStream(t.Context(), source)
			require.NoError(t, err)
			events, err := streams.All(stream)
			require.NoError(t, err)

			for _, event := range events {
				if event == nil || string(event.Data) == "[DONE]" {
					continue
				}
				var decoded StreamEvent
				require.NoError(t, json.Unmarshal(event.Data, &decoded))
				if decoded.Type == "tool-input-available" {
					require.JSONEq(t, arguments, string(decoded.Input))
					return
				}
			}
			t.Fatal("missing tool-input-available")
		})
	}
}

func TestDataStreamToolCallIntegrity_CleanEOFBeforeFinishFails(t *testing.T) {
	source := streams.SliceStream([]*llm.Response{
		toolChunk(0, "call_truncated", "spawn_agent", `{"task":"audit"}`),
	})

	stream, err := NewDataStreamTransformer().TransformStream(t.Context(), source)
	require.NoError(t, err)
	_, err = streams.All(stream)
	require.ErrorIs(t, err, transformer.ErrToolCallIntegrity)
}

func TestDataStreamToolCallIntegrity_TerminalNameCorrectionFails(t *testing.T) {
	finishReason := "tool_calls"
	source := streams.SliceStream([]*llm.Response{
		toolChunk(0, "call_1", "spawn_agent", `{}`),
		{
			Choices: []llm.Choice{{
				Index: 0,
				Message: &llm.Message{ToolCalls: []llm.ToolCall{{
					Index: 0,
					ID:    "call_1",
					Type:  "function",
					Function: llm.FunctionCall{
						Name:      "wait_agent",
						Arguments: `{}`,
					},
				}}},
				FinishReason: &finishReason,
			}},
		},
	})

	stream, err := NewDataStreamTransformer().TransformStream(t.Context(), source)
	require.NoError(t, err)
	_, err = streams.All(stream)
	require.ErrorIs(t, err, transformer.ErrToolCallIntegrity)
}

func TestDataStreamToolCallIntegrity_ChangedIDAtSameIndexFails(t *testing.T) {
	source := streams.SliceStream([]*llm.Response{
		toolChunk(0, "call_a", "spawn_agent", `{}`),
		toolChunk(0, "call_b", "", ""),
	})

	stream, err := NewDataStreamTransformer().TransformStream(t.Context(), source)
	require.NoError(t, err)
	_, err = streams.All(stream)
	require.ErrorIs(t, err, transformer.ErrToolCallIntegrity)
}
