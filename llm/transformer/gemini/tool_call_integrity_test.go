package gemini

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

func geminiIntegrityChunk(index int, id, name, arguments string) *llm.Response {
	return &llm.Response{
		ID: "resp_gemini_client",
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

func TestInboundToolCallIntegrity_InterleavedTerminalCorrection(t *testing.T) {
	finish := "tool_calls"
	source := streams.SliceStream([]*llm.Response{
		geminiIntegrityChunk(0, "call_a", "spawn_agent", `{"task":`),
		geminiIntegrityChunk(1, "call_b", "wait_agent", `{"ms":`),
		geminiIntegrityChunk(0, "", "", `"old"}`),
		geminiIntegrityChunk(1, "", "", `1}`),
		{
			Choices: []llm.Choice{{
				Index: 0,
				Message: &llm.Message{ToolCalls: []llm.ToolCall{{
					Index: 0,
					ID:    "call_a",
					Type:  "function",
					Function: llm.FunctionCall{
						Name:      "spawn_agent",
						Arguments: `{"task":"corrected"}`,
					},
				}}},
				FinishReason: &finish,
			}},
		},
	})

	stream, err := NewInboundTransformer().TransformStream(t.Context(), source)
	require.NoError(t, err)
	raw, err := streams.All(stream)
	require.NoError(t, err)

	got := map[string]*FunctionCall{}
	for _, event := range raw {
		var response GenerateContentResponse
		require.NoError(t, json.Unmarshal(event.Data, &response))
		for _, candidate := range response.Candidates {
			if candidate.Content == nil {
				continue
			}
			for _, part := range candidate.Content.Parts {
				if part.FunctionCall != nil {
					got[part.FunctionCall.ID] = part.FunctionCall
				}
			}
		}
	}
	require.Equal(t, "spawn_agent", got["call_a"].Name)
	require.Equal(t, "corrected", got["call_a"].Args["task"])
	require.Equal(t, "wait_agent", got["call_b"].Name)
	require.EqualValues(t, 1, got["call_b"].Args["ms"])
}

func TestInboundToolCallIntegrity_TruncatedFailsInsteadOfRepair(t *testing.T) {
	finish := "tool_calls"
	source := streams.SliceStream([]*llm.Response{
		geminiIntegrityChunk(0, "call_a", "spawn_agent", `{"task":`),
		{Choices: []llm.Choice{{Index: 0, FinishReason: &finish}}},
	})

	stream, err := NewInboundTransformer().TransformStream(t.Context(), source)
	require.NoError(t, err)
	_, err = streams.All(stream)
	require.ErrorIs(t, err, transformer.ErrIncompleteToolCall)
}
