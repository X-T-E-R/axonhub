package responses

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/internal/pkg/xtest"
	"github.com/looplj/axonhub/llm/streams"
)

func TestOutboundTransformer_StreamTransformation_WithTestData(t *testing.T) {
	trans, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	tests := []struct {
		name                 string
		inputStreamFile      string // OpenAI Responses API stream format
		expectedStreamFile   string // Expected LLM stream format
		expectedResponseFile string // Final LLM response format
	}{
		{
			name:                 "stream transformation with text and multiple tool calls",
			inputStreamFile:      "tool-2.stream.jsonl",
			expectedStreamFile:   "llm-tool-2.stream.jsonl",
			expectedResponseFile: "llm-tool-2.response.json",
		},
		{
			name:                 "stream transformation with encrypted reasoning",
			inputStreamFile:      "encrypted_content.stream.jsonl",
			expectedStreamFile:   "llm-encrypted_content.stream.jsonl",
			expectedResponseFile: "llm-encrypted_content.response.json",
		},
		{
			name:                 "stream transformation with custom tool call",
			inputStreamFile:      "custom_tool.stream.jsonl",
			expectedStreamFile:   "llm-custom_tool.stream.jsonl",
			expectedResponseFile: "llm-custom_tool.stream.response.json",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expectedEvents, err := xtest.LoadLlmResponses(t, tt.expectedStreamFile)
			require.NoError(t, err)

			// Load the input file (OpenAI Responses API format events)
			responsesAPIEvents, err := xtest.LoadStreamChunks(t, tt.inputStreamFile)
			require.NoError(t, err)

			// Transform the stream (OpenAI Responses API -> LLM format)
			transformedStream, err := trans.TransformStream(t.Context(), nil, streams.SliceStream(responsesAPIEvents))
			require.NoError(t, err)
			require.NoError(t, transformedStream.Err())

			// Collect all transformed events
			actualLLMResponses, err := streams.All(transformedStream)
			require.NoError(t, err)

			// Stream transformation may not be 1:1, so we verify key properties instead of exact count
			require.NotEmpty(t, actualLLMResponses, "Should have at least one response")

			// Verify the last event is DONE
			lastEvent := actualLLMResponses[len(actualLLMResponses)-1]
			require.Equal(t, llm.DoneResponse, lastEvent, "Last event should be DONE")

			// Verify non-DONE events have valid structure
			for _, resp := range actualLLMResponses {
				if resp != llm.DoneResponse {
					// Verify each response has the correct object type
					require.Contains(t, []string{"chat.completion", "chat.completion.chunk"}, resp.Object,
						"Response should be chat.completion or chat.completion.chunk")
				}
			}

			expectedWithoutToolPayloads, expectedToolPayloads, _ := splitStreamedToolPayloads(expectedEvents)
			actualWithoutToolPayloads, actualToolPayloads, actualToolPayloadChunks :=
				splitStreamedToolPayloads(actualLLMResponses)

			// exclude the last DONE event
			require.Len(t, actualWithoutToolPayloads, len(expectedWithoutToolPayloads))
			for i, expectedEvent := range expectedWithoutToolPayloads[:len(expectedWithoutToolPayloads)-1] {
				if !xtest.Equal(expectedEvent, actualWithoutToolPayloads[i]) {
					t.Fatalf("event %d mismatch:\n%s", i, cmp.Diff(expectedEvent, actualWithoutToolPayloads[i]))
				}
			}
			require.Equal(t, expectedToolPayloads, actualToolPayloads)
			for key, chunks := range actualToolPayloadChunks {
				require.Equal(t, 1, chunks, "tool payload %s must be published once at terminal", key)
			}

			// Verify the final response against expectedResponseFile
			if tt.expectedResponseFile != "" {
				// Find the last non-DONE response
				var lastResponse *llm.Response

				for i := len(actualLLMResponses) - 1; i >= 0; i-- {
					if actualLLMResponses[i] != llm.DoneResponse {
						lastResponse = actualLLMResponses[i]

						break
					}
				}

				require.NotNil(t, lastResponse, "Expected at least one non-DONE response")

				// Load expected final response from file
				var expectedFinalResponse llm.Response

				err := xtest.LoadTestData(t, tt.expectedResponseFile, &expectedFinalResponse)
				require.NoError(t, err)

				// Compare model and ID from the last response
				require.Equal(t, expectedFinalResponse.Model, lastResponse.Model,
					"Final response model should match")
				require.Equal(t, expectedFinalResponse.ID, lastResponse.ID,
					"Final response ID should match")
			}
		})
	}
}

func splitStreamedToolPayloads(
	responses []*llm.Response,
) ([]*llm.Response, map[string]string, map[string]int) {
	withoutPayloads := make([]*llm.Response, 0, len(responses))
	payloads := make(map[string]string)
	chunks := make(map[string]int)

	for _, response := range responses {
		if response == nil || response == llm.DoneResponse || len(response.Choices) != 1 ||
			response.Choices[0].Delta == nil || len(response.Choices[0].Delta.ToolCalls) == 0 {
			withoutPayloads = append(withoutPayloads, response)
			continue
		}

		payloadOnly := true
		for _, toolCall := range response.Choices[0].Delta.ToolCalls {
			key := fmt.Sprintf("function:%d", toolCall.Index)
			payload := toolCall.Function.Arguments
			if toolCall.ResponseCustomToolCall != nil {
				key = fmt.Sprintf("custom:%d", toolCall.Index)
				payload = toolCall.ResponseCustomToolCall.Input
			}
			if payload == "" {
				payloadOnly = false
				continue
			}
			payloads[key] += payload
			chunks[key]++
			if toolCall.ID != "" {
				payloadOnly = false
			}
		}
		if !payloadOnly {
			withoutPayloads = append(withoutPayloads, response)
		}
	}

	return withoutPayloads, payloads, chunks
}

func TestOutboundTransformer_StreamTransformation_ErrorEvent(t *testing.T) {
	trans, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	responsesAPIEvents, err := xtest.LoadStreamChunks(t, "error.response.stream.jsonl")
	require.NoError(t, err)

	transformedStream, err := trans.TransformStream(t.Context(), nil, streams.SliceStream(responsesAPIEvents))
	require.NoError(t, err)

	_, err = streams.All(transformedStream)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Something went wrong")
}

func TestOutboundTransformer_TransformStream_RecoversFunctionCallArgumentsFromTerminalEvents(t *testing.T) {
	tests := []struct {
		name                   string
		events                 []*httpclient.StreamEvent
		expectedArgumentChunks []string
	}{
		{
			name: "done event resolves item_id to call_id when deltas are missing",
			events: []*httpclient.StreamEvent{
				{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_done","object":"response","created_at":1700000000,"model":"gpt-5","status":"in_progress","output":[]}}`)},
				{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_done","type":"function_call","status":"in_progress","arguments":"","call_id":"call_done","name":"Read"}}`)},
				{Type: "response.function_call_arguments.done", Data: []byte(`{"type":"response.function_call_arguments.done","item_id":"fc_item_done","output_index":0,"arguments":"{\"file_path\":\"AGENTS.md\"}"}`)},
				{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_item_done","type":"function_call","status":"completed","arguments":"{\"file_path\":\"AGENTS.md\"}","call_id":"call_done","name":"Read"}}`)},
				{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_done","object":"response","created_at":1700000000,"model":"gpt-5","status":"completed","output":[{"id":"fc_item_done","type":"function_call","status":"completed","arguments":"{\"file_path\":\"AGENTS.md\"}","call_id":"call_done","name":"Read"}]}}`)},
			},
			expectedArgumentChunks: []string{`{"file_path":"AGENTS.md"}`},
		},
		{
			name: "done event resolves call_id and emits only the missing suffix",
			events: []*httpclient.StreamEvent{
				{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_delta","object":"response","created_at":1700000000,"model":"gpt-5","status":"in_progress","output":[]}}`)},
				{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_delta","type":"function_call","status":"in_progress","arguments":"","call_id":"call_delta","name":"Read"}}`)},
				{Type: "response.function_call_arguments.delta", Data: []byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_item_delta","output_index":0,"delta":"{\"file_path\":"}`)},
				{Type: "response.function_call_arguments.done", Data: []byte(`{"type":"response.function_call_arguments.done","call_id":"call_delta","output_index":0,"arguments":"{\"file_path\":\"AGENTS.md\"}"}`)},
				{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_delta","object":"response","created_at":1700000000,"model":"gpt-5","status":"completed","output":[{"id":"fc_item_delta","type":"function_call","status":"completed","arguments":"{\"file_path\":\"AGENTS.md\"}","call_id":"call_delta","name":"Read"}]}}`)},
			},
			expectedArgumentChunks: []string{`{"file_path":"AGENTS.md"}`},
		},
		{
			name: "output item snapshot recovers arguments when argument events are missing",
			events: []*httpclient.StreamEvent{
				{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_item","object":"response","created_at":1700000000,"model":"gpt-5","status":"in_progress","output":[]}}`)},
				{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_snapshot","type":"function_call","status":"in_progress","arguments":"","call_id":"call_item","name":"Read"}}`)},
				{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_item_snapshot","type":"function_call","status":"completed","arguments":"{\"file_path\":\"AGENTS.md\"}","call_id":"call_item","name":"Read"}}`)},
				{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_item","object":"response","created_at":1700000000,"model":"gpt-5","status":"completed","output":[{"id":"fc_item_snapshot","type":"function_call","status":"completed","arguments":"{\"file_path\":\"AGENTS.md\"}","call_id":"call_item","name":"Read"}]}}`)},
			},
			expectedArgumentChunks: []string{`{"file_path":"AGENTS.md"}`},
		},
		{
			name: "completed response recovers arguments when delta and done events are missing",
			events: []*httpclient.StreamEvent{
				{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_final","object":"response","created_at":1700000000,"model":"gpt-5","status":"in_progress","output":[]}}`)},
				{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_final","type":"function_call","status":"in_progress","arguments":"","call_id":"call_final","name":"Read"}}`)},
				{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_final","object":"response","created_at":1700000000,"model":"gpt-5","status":"completed","output":[{"id":"fc_item_final","type":"function_call","status":"completed","arguments":"{\"file_path\":\"AGENTS.md\"}","call_id":"call_final","name":"Read"}]}}`)},
			},
			expectedArgumentChunks: []string{`{"file_path":"AGENTS.md"}`},
		},
		{
			name: "completed response falls back to buffered arguments when terminal snapshots omit them",
			events: []*httpclient.StreamEvent{
				{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_fallback","object":"response","created_at":1700000000,"model":"gpt-5","status":"in_progress","output":[]}}`)},
				{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_fallback","type":"function_call","status":"in_progress","call_id":"call_fallback","name":"Read"}}`)},
				{Type: "response.function_call_arguments.delta", Data: []byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_item_fallback","output_index":0,"delta":"{\"file_path\":\"AGENTS.md\"}"}`)},
				{Type: "response.function_call_arguments.done", Data: []byte(`{"type":"response.function_call_arguments.done","item_id":"fc_item_fallback","output_index":0}`)},
				{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_item_fallback","type":"function_call","status":"completed","call_id":"call_fallback","name":"Read"}}`)},
				{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_fallback","object":"response","created_at":1700000000,"model":"gpt-5","status":"completed","output":[{"id":"fc_item_fallback","type":"function_call","status":"completed","call_id":"call_fallback","name":"Read"}]}}`)},
			},
			expectedArgumentChunks: []string{`{"file_path":"AGENTS.md"}`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trans, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
			require.NoError(t, err)

			stream, err := trans.TransformStream(t.Context(), nil, streams.SliceStream(tt.events))
			require.NoError(t, err)

			responses, err := streams.All(stream)
			require.NoError(t, err)

			var argumentChunks []string
			var initialCallID string
			for _, response := range responses {
				if response == llm.DoneResponse || len(response.Choices) == 0 || response.Choices[0].Delta == nil {
					continue
				}
				for _, toolCall := range response.Choices[0].Delta.ToolCalls {
					if toolCall.ID != "" {
						initialCallID = toolCall.ID
					}
					if toolCall.Function.Arguments != "" {
						require.Equal(t, 0, toolCall.Index)
						argumentChunks = append(argumentChunks, toolCall.Function.Arguments)
					}
				}
			}

			require.Equal(t, tt.expectedArgumentChunks, argumentChunks)
			require.Equal(t, strings.Join(tt.expectedArgumentChunks, ""), strings.Join(argumentChunks, ""))
			require.NotEmpty(t, initialCallID)
		})
	}
}

func TestOutboundTransformer_TransformStream_RecoversMissingFunctionCallIdentityFromTerminalEvent(t *testing.T) {
	trans, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	events := []*httpclient.StreamEvent{
		{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_identity","object":"response","created_at":1700000000,"model":"gpt-5","status":"in_progress","output":[]}}`)},
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_identity","type":"function_call","status":"in_progress","arguments":"","call_id":"call_identity","name":""}}`)},
		{Type: "response.function_call_arguments.done", Data: []byte(`{"type":"response.function_call_arguments.done","item_id":"fc_item_identity","output_index":0,"name":"Read","namespace":"project","arguments":""}`)},
		{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_identity","object":"response","created_at":1700000000,"model":"gpt-5","status":"completed","output":[{"id":"fc_item_identity","type":"function_call","status":"completed","arguments":"","call_id":"call_identity","name":"Read","namespace":"project"}]}}`)},
	}

	stream, err := trans.TransformStream(t.Context(), nil, streams.SliceStream(events))
	require.NoError(t, err)

	responses, err := streams.All(stream)
	require.NoError(t, err)

	var identityDeltas []llm.FunctionCall
	for _, response := range responses {
		if response == llm.DoneResponse || len(response.Choices) == 0 || response.Choices[0].Delta == nil {
			continue
		}
		for _, toolCall := range response.Choices[0].Delta.ToolCalls {
			if toolCall.Function.Name != "" || toolCall.Function.Namespace != "" {
				identityDeltas = append(identityDeltas, toolCall.Function)
			}
		}
	}

	require.Equal(t, []llm.FunctionCall{{Name: "Read", Namespace: "project"}}, identityDeltas)
}

func TestOutboundTransformer_TransformStream_RecoversCustomToolInputFromTerminalEvents(t *testing.T) {
	tests := []struct {
		name                string
		events              []*httpclient.StreamEvent
		expectedCallID      string
		expectedInputChunks []string
	}{
		{
			name: "done event recovers missing delta by item_id and repeated terminals are idempotent",
			events: []*httpclient.StreamEvent{
				{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_custom_done","object":"response","created_at":1700000000,"model":"gpt-5","status":"in_progress","output":[]}}`)},
				{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"ct_item_done","type":"custom_tool_call","status":"in_progress","input":"","call_id":"call_custom_done","name":"apply_patch"}}`)},
				{Type: "response.custom_tool_call_input.done", Data: []byte(`{"type":"response.custom_tool_call_input.done","item_id":"ct_item_done","output_index":0,"input":"*** Begin Patch\n*** End Patch"}`)},
				{Type: "response.custom_tool_call_input.done", Data: []byte(`{"type":"response.custom_tool_call_input.done","item_id":"ct_item_done","output_index":0,"input":"*** Begin Patch\n*** End Patch"}`)},
				{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"ct_item_done","type":"custom_tool_call","status":"completed","input":"*** Begin Patch\n*** End Patch","call_id":"call_custom_done","name":"apply_patch"}}`)},
				{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_custom_done","object":"response","created_at":1700000000,"model":"gpt-5","status":"completed","output":[{"id":"ct_item_done","type":"custom_tool_call","status":"completed","input":"*** Begin Patch\n*** End Patch","call_id":"call_custom_done","name":"apply_patch"}]}}`)},
			},
			expectedCallID:      "call_custom_done",
			expectedInputChunks: []string{"*** Begin Patch\n*** End Patch"},
		},
		{
			name: "done event resolves call_id and emits only the missing suffix",
			events: []*httpclient.StreamEvent{
				{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_custom_delta","object":"response","created_at":1700000000,"model":"gpt-5","status":"in_progress","output":[]}}`)},
				{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"ct_item_delta","type":"custom_tool_call","status":"in_progress","input":"","call_id":"call_custom_delta","name":"apply_patch"}}`)},
				{Type: "response.custom_tool_call_input.delta", Data: []byte(`{"type":"response.custom_tool_call_input.delta","item_id":"ct_item_delta","output_index":0,"delta":"*** Begin Patch\n"}`)},
				{Type: "response.custom_tool_call_input.done", Data: []byte(`{"type":"response.custom_tool_call_input.done","call_id":"call_custom_delta","output_index":0,"input":"*** Begin Patch\n*** End Patch"}`)},
				{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"ct_item_delta","type":"custom_tool_call","status":"completed","input":"*** Begin Patch\n*** End Patch","call_id":"call_custom_delta","name":"apply_patch"}}`)},
				{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_custom_delta","object":"response","created_at":1700000000,"model":"gpt-5","status":"completed","output":[{"id":"ct_item_delta","type":"custom_tool_call","status":"completed","input":"*** Begin Patch\n*** End Patch","call_id":"call_custom_delta","name":"apply_patch"}]}}`)},
			},
			expectedCallID:      "call_custom_delta",
			expectedInputChunks: []string{"*** Begin Patch\n*** End Patch"},
		},
		{
			name: "output item snapshot recovers input when input events are missing",
			events: []*httpclient.StreamEvent{
				{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_custom_item","object":"response","created_at":1700000000,"model":"gpt-5","status":"in_progress","output":[]}}`)},
				{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"ct_item_snapshot","type":"custom_tool_call","status":"in_progress","input":"","call_id":"call_custom_item","name":"apply_patch"}}`)},
				{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"ct_item_snapshot","type":"custom_tool_call","status":"completed","input":"*** Begin Patch\n*** End Patch","call_id":"call_custom_item","name":"apply_patch"}}`)},
				{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_custom_item","object":"response","created_at":1700000000,"model":"gpt-5","status":"completed","output":[]}}`)},
			},
			expectedCallID:      "call_custom_item",
			expectedInputChunks: []string{"*** Begin Patch\n*** End Patch"},
		},
		{
			name: "completed response recovers input when delta and done events are missing",
			events: []*httpclient.StreamEvent{
				{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_custom_final","object":"response","created_at":1700000000,"model":"gpt-5","status":"in_progress","output":[]}}`)},
				{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"ct_item_final","type":"custom_tool_call","status":"in_progress","input":"","call_id":"call_custom_final","name":"apply_patch"}}`)},
				{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_custom_final","object":"response","created_at":1700000000,"model":"gpt-5","status":"completed","output":[{"id":"ct_item_final","type":"custom_tool_call","status":"completed","input":"*** Begin Patch\n*** End Patch","call_id":"call_custom_final","name":"apply_patch"}]}}`)},
			},
			expectedCallID:      "call_custom_final",
			expectedInputChunks: []string{"*** Begin Patch\n*** End Patch"},
		},
		{
			name: "completed response falls back to buffered input when terminal snapshots omit it",
			events: []*httpclient.StreamEvent{
				{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_custom_fallback","object":"response","created_at":1700000000,"model":"gpt-5","status":"in_progress","output":[]}}`)},
				{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"ct_item_fallback","type":"custom_tool_call","status":"in_progress","call_id":"call_custom_fallback","name":"apply_patch"}}`)},
				{Type: "response.custom_tool_call_input.delta", Data: []byte(`{"type":"response.custom_tool_call_input.delta","item_id":"ct_item_fallback","output_index":0,"delta":"*** Begin Patch\n*** End Patch"}`)},
				{Type: "response.custom_tool_call_input.done", Data: []byte(`{"type":"response.custom_tool_call_input.done","item_id":"ct_item_fallback","output_index":0}`)},
				{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"ct_item_fallback","type":"custom_tool_call","status":"completed","call_id":"call_custom_fallback","name":"apply_patch"}}`)},
				{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_custom_fallback","object":"response","created_at":1700000000,"model":"gpt-5","status":"completed","output":[{"id":"ct_item_fallback","type":"custom_tool_call","status":"completed","call_id":"call_custom_fallback","name":"apply_patch"}]}}`)},
			},
			expectedCallID:      "call_custom_fallback",
			expectedInputChunks: []string{"*** Begin Patch\n*** End Patch"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			trans, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
			require.NoError(t, err)

			stream, err := trans.TransformStream(t.Context(), nil, streams.SliceStream(tt.events))
			require.NoError(t, err)

			responses, err := streams.All(stream)
			require.NoError(t, err)

			var inputChunks []string
			for _, response := range responses {
				if response == llm.DoneResponse || len(response.Choices) == 0 || response.Choices[0].Delta == nil {
					continue
				}
				for _, toolCall := range response.Choices[0].Delta.ToolCalls {
					if toolCall.ResponseCustomToolCall == nil {
						continue
					}
					require.Equal(t, llm.ToolTypeResponsesCustomTool, toolCall.Type)
					require.Equal(t, tt.expectedCallID, toolCall.ResponseCustomToolCall.CallID)
					if toolCall.ResponseCustomToolCall.Input != "" {
						require.Equal(t, 0, toolCall.Index)
						inputChunks = append(inputChunks, toolCall.ResponseCustomToolCall.Input)
					}
				}
			}

			require.Equal(t, tt.expectedInputChunks, inputChunks)
			require.Equal(t, strings.Join(tt.expectedInputChunks, ""), strings.Join(inputChunks, ""))
		})
	}
}

func TestResponsesStreamRoundTrip_BuffersProvisionalToolPayloadsUntilTerminal(t *testing.T) {
	events := []*httpclient.StreamEvent{
		{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_corrected","object":"response","created_at":1700000000,"model":"gpt-5","status":"in_progress","output":[]}}`)},
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_item_corrected","type":"function_call","status":"in_progress","arguments":"","call_id":"call_corrected","name":"Read"}}`)},
		{Type: "response.function_call_arguments.delta", Data: []byte(`{"type":"response.function_call_arguments.delta","item_id":"fc_item_corrected","output_index":0,"delta":"{\"file_path\":\"WRONG\"}"}`)},
		{Type: "response.function_call_arguments.done", Data: []byte(`{"type":"response.function_call_arguments.done","item_id":"fc_item_corrected","output_index":0,"arguments":"{\"file_path\":\"AGENTS.md\"}"}`)},
		{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_item_corrected","type":"function_call","status":"completed","arguments":"{\"file_path\":\"AGENTS.md\"}","call_id":"call_corrected","name":"Read"}}`)},
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":1,"item":{"id":"ct_item_corrected","type":"custom_tool_call","status":"in_progress","input":"","call_id":"call_custom_corrected","name":"apply_patch"}}`)},
		{Type: "response.custom_tool_call_input.delta", Data: []byte(`{"type":"response.custom_tool_call_input.delta","item_id":"ct_item_corrected","output_index":1,"delta":"WRONG INPUT"}`)},
		{Type: "response.custom_tool_call_input.done", Data: []byte(`{"type":"response.custom_tool_call_input.done","item_id":"ct_item_corrected","output_index":1,"input":"*** Begin Patch\n*** End Patch"}`)},
		{Type: "response.custom_tool_call_input.done", Data: []byte(`{"type":"response.custom_tool_call_input.done","item_id":"ct_item_corrected","output_index":1,"input":"*** Begin Patch\n*** End Patch"}`)},
		{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":1,"item":{"id":"ct_item_corrected","type":"custom_tool_call","status":"completed","input":"*** Begin Patch\n*** End Patch","call_id":"call_custom_corrected","name":"apply_patch"}}`)},
		{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_corrected","object":"response","created_at":1700000000,"model":"gpt-5","status":"completed","output":[{"id":"fc_item_corrected","type":"function_call","status":"completed","arguments":"{\"file_path\":\"AGENTS.md\"}","call_id":"call_corrected","name":"Read"},{"id":"ct_item_corrected","type":"custom_tool_call","status":"completed","input":"*** Begin Patch\n*** End Patch","call_id":"call_custom_corrected","name":"apply_patch"}]}}`)},
	}

	outbound, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)
	unifiedStream, err := outbound.TransformStream(t.Context(), nil, streams.SliceStream(events))
	require.NoError(t, err)
	unified, err := streams.All(unifiedStream)
	require.NoError(t, err)

	var functionPayloads []string
	var customPayloads []string
	for _, response := range unified {
		if response == llm.DoneResponse || len(response.Choices) == 0 || response.Choices[0].Delta == nil {
			continue
		}
		for _, toolCall := range response.Choices[0].Delta.ToolCalls {
			if toolCall.Function.Arguments != "" {
				functionPayloads = append(functionPayloads, toolCall.Function.Arguments)
			}
			if toolCall.ResponseCustomToolCall != nil && toolCall.ResponseCustomToolCall.Input != "" {
				customPayloads = append(customPayloads, toolCall.ResponseCustomToolCall.Input)
			}
		}
	}
	require.Equal(t, []string{`{"file_path":"AGENTS.md"}`}, functionPayloads)
	require.Equal(t, []string{"*** Begin Patch\n*** End Patch"}, customPayloads)
	serializedUnified, err := json.Marshal(unified)
	require.NoError(t, err)
	require.NotContains(t, string(serializedUnified), "authoritative")

	inboundStream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream(unified))
	require.NoError(t, err)
	inboundEvents, err := collectResponsesStreamEvents(inboundStream)
	require.NoError(t, err)

	var functionDeltas []string
	var functionDone []string
	var customDeltas []string
	var customDone []string
	var itemDone []Item
	for _, event := range inboundEvents {
		switch event.Type {
		case StreamEventTypeFunctionCallArgumentsDelta:
			functionDeltas = append(functionDeltas, event.Delta)
		case StreamEventTypeFunctionCallArgumentsDone:
			functionDone = append(functionDone, event.Arguments)
		case StreamEventTypeCustomToolCallInputDelta:
			customDeltas = append(customDeltas, event.Delta)
		case StreamEventTypeCustomToolCallInputDone:
			customDone = append(customDone, event.Input)
		case StreamEventTypeOutputItemDone:
			if event.Item != nil && (event.Item.Type == "function_call" || event.Item.Type == "custom_tool_call") {
				itemDone = append(itemDone, *event.Item)
			}
		}
	}

	require.Equal(t, []string{`{"file_path":"AGENTS.md"}`}, functionDeltas)
	require.Equal(t, []string{`{"file_path":"AGENTS.md"}`}, functionDone)
	require.Equal(t, []string{"*** Begin Patch\n*** End Patch"}, customDeltas)
	require.Equal(t, []string{"*** Begin Patch\n*** End Patch"}, customDone)
	require.Len(t, itemDone, 2)
	require.Equal(t, `{"file_path":"AGENTS.md"}`, itemDone[0].Arguments)
	require.Equal(t, "*** Begin Patch\n*** End Patch", lo.FromPtr(itemDone[1].Input))

	completed := inboundEvents[len(inboundEvents)-1]
	require.Equal(t, StreamEventTypeResponseCompleted, completed.Type)
	require.NotNil(t, completed.Response)
	require.Len(t, completed.Response.Output, 2)
	require.Equal(t, `{"file_path":"AGENTS.md"}`, completed.Response.Output[0].Arguments)
	require.Equal(t, "*** Begin Patch\n*** End Patch", lo.FromPtr(completed.Response.Output[1].Input))
}

func TestResponsesStreamRoundTrip_CompletedOnlyCreatesParallelToolCalls(t *testing.T) {
	events := []*httpclient.StreamEvent{
		{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_completed_only","object":"response","created_at":1700000000,"model":"gpt-5","status":"in_progress","output":[]}}`)},
		{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_completed_only","object":"response","created_at":1700000000,"model":"gpt-5","status":"completed","output":[{"id":"fc_completed_only","type":"function_call","status":"completed","arguments":"{\"path\":\"AGENTS.md\"}","call_id":"call_completed_only","name":"Read"},{"id":"ct_completed_only","type":"custom_tool_call","status":"completed","input":"*** Begin Patch\n*** End Patch","call_id":"call_custom_completed_only","name":"apply_patch"}]}}`)},
	}

	outbound, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)
	unifiedStream, err := outbound.TransformStream(t.Context(), nil, streams.SliceStream(events))
	require.NoError(t, err)
	unified, err := streams.All(unifiedStream)
	require.NoError(t, err)

	var starts []llm.ToolCall
	var finishReasons []string
	for _, response := range unified {
		if response == llm.DoneResponse || len(response.Choices) == 0 {
			continue
		}
		choice := response.Choices[0]
		if choice.FinishReason != nil {
			finishReasons = append(finishReasons, *choice.FinishReason)
		}
		if choice.Delta == nil {
			continue
		}
		for _, toolCall := range choice.Delta.ToolCalls {
			if toolCall.ID != "" {
				starts = append(starts, toolCall)
			}
		}
	}
	require.Len(t, starts, 2)
	require.Equal(t, []int{0, 1}, []int{starts[0].Index, starts[1].Index})
	require.Equal(t, "call_completed_only", starts[0].ID)
	require.Equal(t, "call_custom_completed_only", starts[1].ID)
	require.Equal(t, []string{"tool_calls"}, finishReasons)

	inboundStream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream(unified))
	require.NoError(t, err)
	inboundEvents, err := collectResponsesStreamEvents(inboundStream)
	require.NoError(t, err)

	addedByOutputIndex := make(map[int]Item)
	terminalItemIDsByOutputIndex := make(map[int]string)
	terminalValuesByOutputIndex := make(map[int]string)
	doneItemsByOutputIndex := make(map[int]Item)
	for _, event := range inboundEvents {
		if event.Type == StreamEventTypeOutputItemAdded && event.Item != nil {
			addedByOutputIndex[event.OutputIndex] = *event.Item
		}
		if event.Type == StreamEventTypeFunctionCallArgumentsDone && event.ItemID != nil {
			terminalItemIDsByOutputIndex[event.OutputIndex] = *event.ItemID
			terminalValuesByOutputIndex[event.OutputIndex] = event.Arguments
		}
		if event.Type == StreamEventTypeCustomToolCallInputDone && event.ItemID != nil {
			terminalItemIDsByOutputIndex[event.OutputIndex] = *event.ItemID
			terminalValuesByOutputIndex[event.OutputIndex] = event.Input
		}
		if event.Type == StreamEventTypeOutputItemDone && event.Item != nil &&
			(event.Item.Type == "function_call" || event.Item.Type == "custom_tool_call") {
			doneItemsByOutputIndex[event.OutputIndex] = *event.Item
		}
	}

	require.Len(t, addedByOutputIndex, 2)
	require.Equal(t, "function_call", addedByOutputIndex[0].Type)
	require.Equal(t, "call_completed_only", addedByOutputIndex[0].CallID)
	require.Equal(t, "custom_tool_call", addedByOutputIndex[1].Type)
	require.Equal(t, "call_custom_completed_only", addedByOutputIndex[1].CallID)
	require.Equal(t, addedByOutputIndex[0].ID, terminalItemIDsByOutputIndex[0])
	require.Equal(t, addedByOutputIndex[1].ID, terminalItemIDsByOutputIndex[1])
	require.Equal(t, `{"path":"AGENTS.md"}`, terminalValuesByOutputIndex[0])
	require.Equal(t, "*** Begin Patch\n*** End Patch", terminalValuesByOutputIndex[1])
	require.Equal(t, `{"path":"AGENTS.md"}`, doneItemsByOutputIndex[0].Arguments)
	require.Equal(t, "*** Begin Patch\n*** End Patch", lo.FromPtr(doneItemsByOutputIndex[1].Input))

	completed := inboundEvents[len(inboundEvents)-1]
	require.Equal(t, StreamEventTypeResponseCompleted, completed.Type)
	require.NotNil(t, completed.Response)
	require.Len(t, completed.Response.Output, 2)
	require.Equal(t, `{"path":"AGENTS.md"}`, completed.Response.Output[0].Arguments)
	require.Equal(t, "*** Begin Patch\n*** End Patch", lo.FromPtr(completed.Response.Output[1].Input))
}

func collectResponsesStreamEvents(
	stream streams.Stream[*httpclient.StreamEvent],
) ([]StreamEvent, error) {
	var events []StreamEvent
	for stream.Next() {
		var event StreamEvent
		if err := json.Unmarshal(stream.Current().Data, &event); err != nil {
			return nil, err
		}
		events = append(events, event)
	}

	return events, stream.Err()
}

func TestOutboundTransformer_TransformStream_UsesFinalEncryptedContentPerReasoningItem(t *testing.T) {
	trans, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	events := []*httpclient.StreamEvent{
		{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_reasoning_multi","object":"response","created_at":1700000000,"model":"gpt-5","status":"in_progress","output":[]}}`)},
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"gAAAA_added_1"}}`)},
		{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"gAAAA_done_1"}}`)},
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":1,"item":{"id":"rs_2","type":"reasoning","summary":[],"encrypted_content":"gAAAA_added_2"}}`)},
		{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":1,"item":{"id":"rs_2","type":"reasoning","summary":[],"encrypted_content":"gAAAA_done_2"}}`)},
		{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_reasoning_multi","object":"response","created_at":1700000000,"model":"gpt-5","status":"completed","output":[]}}`)},
	}

	stream, err := trans.TransformStream(t.Context(), nil, streams.SliceStream(events))
	require.NoError(t, err)

	responses, err := streams.All(stream)
	require.NoError(t, err)

	var signatures []string
	var sourceIDs []string
	for _, resp := range responses {
		if resp == llm.DoneResponse || len(resp.Choices) == 0 || resp.Choices[0].Delta == nil {
			continue
		}
		if resp.Choices[0].Delta.ReasoningSignature == nil {
			continue
		}

		signatures = append(signatures, *resp.Choices[0].Delta.ReasoningSignature)
		metadata, ok := getResponsesReasoningItemMetadata(resp.TransformerMetadata)
		require.True(t, ok)
		require.True(t, metadata.Done)
		sourceIDs = append(sourceIDs, metadata.ID)
	}

	require.Equal(t, []string{"gAAAA_done_1", "gAAAA_done_2"}, signatures)
	require.Equal(t, []string{"rs_1", "rs_2"}, sourceIDs)
}

func TestOutboundTransformer_TransformStream_ResponseCancelledCompletes(t *testing.T) {
	trans, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	events := []*httpclient.StreamEvent{
		{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_cancelled","object":"response","created_at":1700000000,"model":"gpt-5","status":"in_progress","output":[]}}`)},
		{Type: "response.cancelled", Data: []byte(`{"type":"response.cancelled","response":{"id":"resp_cancelled","object":"response","created_at":1700000000,"model":"gpt-5","status":"canceled","output":[]}}`)},
	}

	stream, err := trans.TransformStream(t.Context(), nil, streams.SliceStream(events))
	require.NoError(t, err)

	responses, err := streams.All(stream)
	require.NoError(t, err)
	require.Len(t, responses, 3)
	require.Equal(t, llm.DoneResponse, responses[2])
	require.Equal(t, "resp_cancelled", responses[1].ID)
	require.Equal(t, "gpt-5", responses[1].Model)
	require.Equal(t, int64(1700000000), responses[1].Created)
	require.NotEmpty(t, responses[1].Choices)
	require.NotNil(t, responses[1].Choices[0].FinishReason)
	require.Equal(t, "cancelled", *responses[1].Choices[0].FinishReason)
}

func TestOutboundTransformer_TransformStream_PreservesFinalItemAnnotations(t *testing.T) {
	trans, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	events := []*httpclient.StreamEvent{
		{
			Type: "response.created",
			Data: []byte(`{
				"type":"response.created",
				"response":{
					"id":"resp_stream_annotations",
					"object":"response",
					"created_at":1700000000,
					"model":"gpt-4o-search-preview",
					"status":"in_progress",
					"output":[]
				}
			}`),
		},
		{
			Type: "response.output_item.added",
			Data: []byte(`{
				"type":"response.output_item.added",
				"output_index":0,
				"item":{
					"id":"msg_stream_annotations",
					"type":"message",
					"status":"in_progress",
					"role":"assistant"
				}
			}`),
		},
		{
			Type: "response.content_part.added",
			Data: []byte(`{
				"type":"response.content_part.added",
				"item_id":"msg_stream_annotations",
				"output_index":0,
				"content_index":0,
				"part":{
					"type":"output_text",
					"text":""
				}
			}`),
		},
		{
			Type: "response.output_text.delta",
			Data: []byte(`{
				"type":"response.output_text.delta",
				"item_id":"msg_stream_annotations",
				"output_index":0,
				"content_index":0,
				"delta":"Search result"
			}`),
		},
		{
			Type: "response.output_text.done",
			Data: []byte(`{
				"type":"response.output_text.done",
				"item_id":"msg_stream_annotations",
				"output_index":0,
				"content_index":0,
				"text":"Search result"
			}`),
		},
		{
			Type: "response.output_item.done",
			Data: []byte(`{
				"type":"response.output_item.done",
				"output_index":0,
				"item":{
					"id":"msg_stream_annotations",
					"type":"message",
					"status":"completed",
					"role":"assistant",
					"content":[{
						"type":"output_text",
						"text":"Search result",
						"annotations":[{
							"type":"url_citation",
							"start_index":0,
							"end_index":6,
							"url_citation":{
								"url":"https://example.com/result",
								"title":"Example Result"
							}
						}]
					}]
				}
			}`),
		},
		{
			Type: "response.completed",
			Data: []byte(`{
				"type":"response.completed",
				"response":{
					"id":"resp_stream_annotations",
					"object":"response",
					"created_at":1700000000,
					"model":"gpt-4o-search-preview",
					"status":"completed",
					"output":[]
				}
			}`),
		},
	}

	stream, err := trans.TransformStream(context.Background(), nil, streams.SliceStream(events))
	require.NoError(t, err)

	actual, err := streams.All(stream)
	require.NoError(t, err)
	require.NotEmpty(t, actual)

	var found []llm.Annotation
	for _, resp := range actual {
		if resp == llm.DoneResponse {
			continue
		}
		for _, choice := range resp.Choices {
			if choice.Message != nil && len(choice.Message.Annotations) > 0 {
				found = choice.Message.Annotations
				break
			}
			if choice.Delta != nil && len(choice.Delta.Annotations) > 0 {
				found = choice.Delta.Annotations
				break
			}
		}
		if len(found) > 0 {
			break
		}
	}

	require.Len(t, found, 1)
	require.Equal(t, "url_citation", found[0].Type)
	require.NotNil(t, found[0].URLCitation)
	require.Equal(t, "https://example.com/result", found[0].URLCitation.URL)
	require.Equal(t, "Example Result", found[0].URLCitation.Title)
	require.NotNil(t, found[0].StartIndex)
	require.NotNil(t, found[0].EndIndex)
	require.EqualValues(t, 0, *found[0].StartIndex)
	require.EqualValues(t, 6, *found[0].EndIndex)
}

func TestOutboundTransformer_TransformStream_PreservesWebSearchMetadataOnAnnotationChunk(t *testing.T) {
	trans, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	events := []*httpclient.StreamEvent{
		{
			Type: "response.created",
			Data: []byte(`{
				"type":"response.created",
				"response":{
					"id":"resp_stream_web_search_annotations",
					"object":"response",
					"created_at":1700000000,
					"model":"gpt-4o-search-preview",
					"status":"in_progress",
					"output":[]
				}
			}`),
		},
		{
			Type: "response.output_item.done",
			Data: []byte(`{
				"type":"response.output_item.done",
				"output_index":0,
				"item":{
					"id":"ws_123",
					"type":"web_search_call",
					"status":"completed",
					"action":{
						"type":"search",
						"query":"latest ai news",
						"sources":[{"type":"url","url":"https://example.com/source","title":"Example Source"}]
					}
				}
			}`),
		},
		{
			Type: "response.output_item.done",
			Data: []byte(`{
				"type":"response.output_item.done",
				"output_index":1,
				"item":{
					"id":"msg_stream_web_search_annotations",
					"type":"message",
					"status":"completed",
					"role":"assistant",
					"content":[{
						"type":"output_text",
						"text":"Search result",
						"annotations":[{
							"type":"url_citation",
							"url_citation":{
								"url":"https://example.com/result",
								"title":"Example Result"
							}
						}]
					}]
				}
			}`),
		},
		{
			Type: "response.completed",
			Data: []byte(`{
				"type":"response.completed",
				"response":{
					"id":"resp_stream_web_search_annotations",
					"object":"response",
					"created_at":1700000000,
					"model":"gpt-4o-search-preview",
					"status":"completed",
					"output":[]
				}
			}`),
		},
	}

	stream, err := trans.TransformStream(context.Background(), nil, streams.SliceStream(events))
	require.NoError(t, err)

	actual, err := streams.All(stream)
	require.NoError(t, err)
	require.NotEmpty(t, actual)

	var annotationChunk *llm.Response
	for _, resp := range actual {
		if resp == llm.DoneResponse {
			continue
		}
		for _, choice := range resp.Choices {
			if choice.Delta != nil && len(choice.Delta.Annotations) > 0 {
				annotationChunk = resp
				break
			}
		}
		if annotationChunk != nil {
			break
		}
	}

	require.NotNil(t, annotationChunk)
	require.NotNil(t, annotationChunk.TransformerMetadata)
	calls, ok := annotationChunk.TransformerMetadata[responsesWebSearchCallsTransformerMetadataKey]
	require.True(t, ok)
	require.NotNil(t, calls)
}

func TestOutboundTransformer_TransformStream_PreservesWebSearchMetadataWithoutAnnotations(t *testing.T) {
	trans, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	events := []*httpclient.StreamEvent{
		{
			Type: "response.created",
			Data: []byte(`{
				"type":"response.created",
				"response":{
					"id":"resp_stream_web_search_no_annotations",
					"object":"response",
					"created_at":1700000000,
					"model":"gpt-4o-search-preview",
					"status":"in_progress",
					"output":[]
				}
			}`),
		},
		{
			Type: "response.output_item.done",
			Data: []byte(`{
				"type":"response.output_item.done",
				"output_index":0,
				"item":{
					"id":"ws_456",
					"type":"web_search_call",
					"status":"completed",
					"action":{
						"type":"search",
						"query":"latest ai news",
						"sources":[{"type":"url","url":"https://example.com/source","title":"Example Source"}]
					}
				}
			}`),
		},
		{
			Type: "response.output_item.done",
			Data: []byte(`{
				"type":"response.output_item.done",
				"output_index":1,
				"item":{
					"id":"msg_stream_web_search_no_annotations",
					"type":"message",
					"status":"completed",
					"role":"assistant",
					"content":[{
						"type":"output_text",
						"text":"Search result without inline citations"
					}]
				}
			}`),
		},
		{
			Type: "response.completed",
			Data: []byte(`{
				"type":"response.completed",
				"response":{
					"id":"resp_stream_web_search_no_annotations",
					"object":"response",
					"created_at":1700000000,
					"model":"gpt-4o-search-preview",
					"status":"completed",
					"output":[]
				}
			}`),
		},
	}

	stream, err := trans.TransformStream(context.Background(), nil, streams.SliceStream(events))
	require.NoError(t, err)

	actual, err := streams.All(stream)
	require.NoError(t, err)
	require.NotEmpty(t, actual)

	var metadataChunk *llm.Response
	for _, resp := range actual {
		if resp == llm.DoneResponse {
			continue
		}
		if resp.TransformerMetadata != nil {
			if _, ok := resp.TransformerMetadata[responsesWebSearchCallsTransformerMetadataKey]; ok {
				metadataChunk = resp
				break
			}
		}
	}

	require.NotNil(t, metadataChunk)
	calls, ok := metadataChunk.TransformerMetadata[responsesWebSearchCallsTransformerMetadataKey]
	require.True(t, ok)
	require.NotNil(t, calls)
}

func TestOutboundTransformer_TransformStream_PreservesPreviousResponseID(t *testing.T) {
	trans, err := NewOutboundTransformer("https://api.openai.com", "test-api-key")
	require.NoError(t, err)

	events := []*httpclient.StreamEvent{
		{
			Type: "response.created",
			Data: []byte(`{
				"type":"response.created",
				"response":{
					"id":"resp_stream_prev",
					"object":"response",
					"created_at":1700000000,
					"model":"gpt-5.4",
					"status":"in_progress",
					"previous_response_id":"resp_prev_123",
					"output":[]
				}
			}`),
		},
		{
			Type: "response.completed",
			Data: []byte(`{
				"type":"response.completed",
				"response":{
					"id":"resp_stream_prev",
					"object":"response",
					"created_at":1700000000,
					"model":"gpt-5.4",
					"status":"completed",
					"previous_response_id":"resp_prev_123",
					"output":[],
					"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}
				}
			}`),
		},
	}

	stream, err := trans.TransformStream(context.Background(), nil, streams.SliceStream(events))
	require.NoError(t, err)

	actual, err := streams.All(stream)
	require.NoError(t, err)
	require.Len(t, actual, 4)

	require.NotNil(t, actual[0].PreviousResponseID)
	require.Equal(t, "resp_prev_123", *actual[0].PreviousResponseID)

	require.NotNil(t, actual[1].PreviousResponseID)
	require.Equal(t, "resp_prev_123", *actual[1].PreviousResponseID)

	require.NotNil(t, actual[2].PreviousResponseID)
	require.Equal(t, "resp_prev_123", *actual[2].PreviousResponseID)
	require.Equal(t, llm.DoneResponse, actual[3])
}
