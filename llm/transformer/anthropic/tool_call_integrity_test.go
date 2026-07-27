package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

func integrityToolChunk(index int, id, name, arguments string) *llm.Response {
	return &llm.Response{
		ID:     "msg_integrity",
		Model:  "claude-test",
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

func collectIntegrityEvents(t *testing.T, input []*llm.Response) ([]StreamEvent, error) {
	t.Helper()
	stream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream(input))
	require.NoError(t, err)
	raw, err := streams.All(stream)
	if err != nil {
		return nil, err
	}

	events := make([]StreamEvent, 0, len(raw))
	for _, item := range raw {
		var event StreamEvent
		require.NoError(t, json.Unmarshal(item.Data, &event))
		events = append(events, event)
	}
	return events, nil
}

func TestInboundToolCallIntegrity_InterleavedAndTerminalCorrection(t *testing.T) {
	finish := "tool_calls"
	corrected := llm.ToolCall{
		Index: 0,
		ID:    "call_spawn",
		Type:  "function",
		Function: llm.FunctionCall{
			Name:      "spawn_agent",
			Arguments: `{"task":"corrected"}`,
		},
	}
	input := []*llm.Response{
		integrityToolChunk(0, "call_spawn", "spawn_agent", `{"task":`),
		integrityToolChunk(1, "call_wait", "wait_agent", `{"ms":`),
		integrityToolChunk(0, "", "", `"provisional"}`),
		integrityToolChunk(1, "", "", `1}`),
		{
			Choices: []llm.Choice{{
				Index:        0,
				Message:      &llm.Message{ToolCalls: []llm.ToolCall{corrected}},
				FinishReason: &finish,
			}},
		},
	}

	events, err := collectIntegrityEvents(t, input)
	require.NoError(t, err)

	type call struct {
		id, name, arguments string
	}
	var calls []call
	for i := 0; i < len(events); i++ {
		if events[i].Type != "content_block_start" ||
			events[i].ContentBlock == nil ||
			!isAnthropicToolUseLike(events[i].ContentBlock.Type) {
			continue
		}
		start := events[i].ContentBlock
		require.Less(t, i+2, len(events))
		arguments := ""
		for i++; i < len(events) && events[i].Type != "content_block_stop"; i++ {
			require.Equal(t, "content_block_delta", events[i].Type)
			require.NotNil(t, events[i].Delta)
			require.NotNil(t, events[i].Delta.PartialJSON)
			arguments += *events[i].Delta.PartialJSON
		}
		require.Less(t, i, len(events))
		calls = append(calls, call{
			id:        start.ID,
			name:      *start.Name,
			arguments: arguments,
		})
	}

	require.Equal(t, []call{
		{id: "call_spawn", name: "spawn_agent", arguments: `{"task":"corrected"}`},
		{id: "call_wait", name: "wait_agent", arguments: `{"ms":1}`},
	}, calls)
}

func TestInboundToolCallIntegrity_TruncatedFailsWithoutRepair(t *testing.T) {
	_, err := collectIntegrityEvents(t, []*llm.Response{
		integrityToolChunk(0, "call_spawn", "spawn_agent", `{"task":`),
	})
	require.ErrorIs(t, err, transformer.ErrIncompleteToolCall)
}

func TestInboundToolCallIntegrity_PreservesReadArguments(t *testing.T) {
	finish := "tool_calls"
	events, err := collectIntegrityEvents(t, []*llm.Response{
		integrityToolChunk(0, "call_read", "Read", `{"file_path":"a.go","pages":""}`),
		{Choices: []llm.Choice{{Index: 0, FinishReason: &finish}}},
	})
	require.NoError(t, err)

	for _, event := range events {
		if event.Type == "content_block_delta" && event.Delta != nil &&
			event.Delta.PartialJSON != nil {
			require.Equal(t, `{"file_path":"a.go","pages":""}`, *event.Delta.PartialJSON)
			return
		}
	}
	t.Fatal("missing tool input delta")
}

func TestOutboundToolCallIntegrity_CleanEOFBeforeMessageStopFails(t *testing.T) {
	events := []*httpclient.StreamEvent{
		{Type: "message_start", Data: []byte(
			`{"type":"message_start","message":{"id":"msg_1","model":"claude-test","usage":{"input_tokens":1,"output_tokens":0}}}`,
		)},
		{Type: "content_block_start", Data: []byte(
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_1","name":"spawn_agent","input":{}}}`,
		)},
		{Type: "content_block_stop", Data: []byte(
			`{"type":"content_block_stop","index":0}`,
		)},
		{Data: []byte("[DONE]")},
	}

	_, err := streams.All(newOutboundStream(streams.SliceStream(events), PlatformDirect))
	require.ErrorIs(t, err, transformer.ErrToolCallIntegrity)
}
