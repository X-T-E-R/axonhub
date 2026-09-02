package responses

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

func TestOutboundToolCallIntegrity_ConflictingTerminalSnapshotFails(t *testing.T) {
	outbound, err := NewOutboundTransformer("https://api.openai.com", "test")
	require.NoError(t, err)
	events := []*httpclient.StreamEvent{
		{Data: []byte(`{"type":"response.created","response":{"id":"resp_1","model":"gpt-test","status":"in_progress","output":[]}}`)},
		{Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"spawn_agent","arguments":""}}`)},
		{Data: []byte(`{"type":"response.function_call_arguments.delta","item_id":"item_1","output_index":0,"delta":"{\"task\":\"provisional\"}"}`)},
		{Data: []byte(`{"type":"response.function_call_arguments.done","item_id":"item_1","output_index":0,"arguments":"{\"task\":\"first-terminal\"}"}`)},
		{Data: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"spawn_agent","arguments":"{\"task\":\"second-terminal\"}"}}`)},
		{Data: []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-test","status":"completed","output":[{"id":"item_1","type":"function_call","call_id":"call_1","name":"spawn_agent","arguments":"{\"task\":\"authoritative\"}"}]}}`)},
	}

	stream, err := outbound.TransformStream(t.Context(), nil, streams.SliceStream(events))
	require.NoError(t, err)
	_, err = streams.All(stream)
	require.ErrorIs(t, err, transformer.ErrToolCallIntegrity)
}

func TestOutboundToolCallIntegrity_CompletedEmptyArgumentsFail(t *testing.T) {
	outbound, err := NewOutboundTransformer("https://api.openai.com", "test")
	require.NoError(t, err)
	events := []*httpclient.StreamEvent{
		{Data: []byte(`{"type":"response.created","response":{"id":"resp_1","model":"gpt-test","status":"in_progress","output":[]}}`)},
		{Data: []byte(`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-test","status":"completed","output":[{"id":"item_1","type":"function_call","call_id":"call_1","name":"spawn_agent","arguments":""}]}}`)},
	}

	stream, err := outbound.TransformStream(t.Context(), nil, streams.SliceStream(events))
	require.NoError(t, err)
	_, err = streams.All(stream)
	require.ErrorIs(t, err, transformer.ErrIncompleteToolCall)
}

func TestOutboundToolCallIntegrity_AbnormalTerminalAllowsEmptyArguments(t *testing.T) {
	for status, wantFinishReason := range map[string]string{
		"failed":     "error",
		"incomplete": "length",
		"cancelled":  "cancelled",
	} {
		t.Run(status, func(t *testing.T) {
			outbound, err := NewOutboundTransformer("https://api.openai.com", "test")
			require.NoError(t, err)
			events := []*httpclient.StreamEvent{
				{Data: []byte(`{"type":"response.created","response":{"id":"resp_1","model":"gpt-test","status":"in_progress","output":[]}}`)},
				{Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"spawn_agent","arguments":""}}`)},
				{Data: []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_1","model":"gpt-test","status":%q,"output":[]}}`, status))},
			}

			stream, err := outbound.TransformStream(t.Context(), nil, streams.SliceStream(events))
			require.NoError(t, err)
			responses, err := streams.All(stream)
			require.NoError(t, err)
			require.NotEmpty(t, responses)
			var finishReasons []string
			for _, response := range responses {
				if len(response.Choices) > 0 && response.Choices[0].FinishReason != nil {
					finishReasons = append(finishReasons, *response.Choices[0].FinishReason)
				}
			}
			require.Contains(t, finishReasons, wantFinishReason)
		})
	}
}

func TestOutboundToolCallIntegrity_TerminalNameCorrectionFails(t *testing.T) {
	terminalEvents := map[string]string{
		"arguments.done": `{"type":"response.function_call_arguments.done","item_id":"item_1","call_id":"call_1","name":"wait_agent","arguments":"{}"}`,
		"item.done":      `{"type":"response.output_item.done","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"wait_agent","arguments":"{}"}}`,
		"completed":      `{"type":"response.completed","response":{"id":"resp_1","model":"gpt-test","status":"completed","output":[{"id":"item_1","type":"function_call","call_id":"call_1","name":"wait_agent","arguments":"{}"}]}}`,
	}

	for name, terminalEvent := range terminalEvents {
		t.Run(name, func(t *testing.T) {
			outbound, err := NewOutboundTransformer("https://api.openai.com", "test")
			require.NoError(t, err)
			events := []*httpclient.StreamEvent{
				{Data: []byte(`{"type":"response.created","response":{"id":"resp_1","model":"gpt-test","status":"in_progress","output":[]}}`)},
				{Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"spawn_agent","arguments":""}}`)},
				{Data: []byte(terminalEvent)},
			}

			stream, err := outbound.TransformStream(t.Context(), nil, streams.SliceStream(events))
			require.NoError(t, err)
			_, err = streams.All(stream)
			require.ErrorIs(t, err, transformer.ErrToolCallIntegrity)
		})
	}
}

func TestOutboundToolCallIntegrity_TerminalCallIDCorrectionFails(t *testing.T) {
	outbound, err := NewOutboundTransformer("https://api.openai.com", "test")
	require.NoError(t, err)
	events := []*httpclient.StreamEvent{
		{Data: []byte(`{"type":"response.created","response":{"id":"resp_1","model":"gpt-test","status":"in_progress","output":[]}}`)},
		{Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"spawn_agent","arguments":""}}`)},
		{Data: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"item_1","type":"function_call","call_id":"call_2","name":"spawn_agent","arguments":"{}"}}`)},
	}

	stream, err := outbound.TransformStream(t.Context(), nil, streams.SliceStream(events))
	require.NoError(t, err)
	_, err = streams.All(stream)
	require.ErrorIs(t, err, transformer.ErrToolCallIntegrity)
}

func TestInboundToolCallIntegrity_InterleavedCompletedSnapshots(t *testing.T) {
	finish := "tool_calls"
	chunk := func(index int, id, name, arguments string) *llm.Response {
		return &llm.Response{
			ID: "resp_client",
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
	source := streams.SliceStream([]*llm.Response{
		chunk(0, "call_a", "spawn_agent", `{"task":`),
		chunk(1, "call_b", "wait_agent", `{"ms":`),
		chunk(0, "", "", `"old"}`),
		chunk(1, "", "", `1}`),
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

	got := map[string]Item{}
	for _, event := range raw {
		var decoded StreamEvent
		require.NoError(t, json.Unmarshal(event.Data, &decoded))
		if decoded.Type == StreamEventTypeOutputItemDone &&
			decoded.Item != nil && decoded.Item.Type == "function_call" {
			got[decoded.Item.CallID] = *decoded.Item
		}
	}
	require.Equal(t, "spawn_agent", got["call_a"].Name)
	require.JSONEq(t, `{"task":"corrected"}`, got["call_a"].Arguments)
	require.Equal(t, "wait_agent", got["call_b"].Name)
	require.JSONEq(t, `{"ms":1}`, got["call_b"].Arguments)
}

func TestInboundToolCallIntegrity_CleanEOFEmitsFailedEvent(t *testing.T) {
	source := streams.SliceStream([]*llm.Response{
		{
			ID: "resp_client",
			Choices: []llm.Choice{{
				Index: 0,
				Delta: &llm.Message{ToolCalls: []llm.ToolCall{{
					Index: 0,
					ID:    "call_1",
					Type:  "function",
					Function: llm.FunctionCall{
						Name:      "spawn_agent",
						Arguments: `{"task":`,
					},
				}}},
			}},
		},
	})

	stream, err := NewInboundTransformer().TransformStream(t.Context(), source)
	require.NoError(t, err)
	raw, err := streams.All(stream)
	require.NoError(t, err)
	require.NotEmpty(t, raw)

	var failed StreamEvent
	require.NoError(t, json.Unmarshal(raw[len(raw)-1].Data, &failed))
	require.Equal(t, StreamEventTypeResponseFailed, failed.Type)
	require.NotNil(t, failed.Response)
	require.NotNil(t, failed.Response.Error)
	require.Equal(t, "tool_call_integrity", failed.Response.Error.Code)
}
