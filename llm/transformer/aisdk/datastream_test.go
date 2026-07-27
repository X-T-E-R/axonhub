package aisdk

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestDataStreamTransformer_TransformRequest(t *testing.T) {
	transformer := NewDataStreamTransformer()
	ctx := context.Background()

	tests := []struct {
		name     string
		input    *httpclient.Request
		expected *llm.Request
		wantErr  bool
	}{
		{
			name: "basic text message",
			input: &httpclient.Request{
				Method: "POST",
				URL:    "/api/chat",
				Headers: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: []byte(`{
					"messages": [
						{
							"role": "user",
							"content": "Hello, world!"
						}
					],
					"model": "gpt-4",
					"stream": true
				}`),
			},
			expected: &llm.Request{
				Model: "gpt-4",
				Messages: []llm.Message{
					{
						Role: "user",
						Content: llm.MessageContent{
							Content: lo.ToPtr("Hello, world!"),
						},
					},
				},
				Stream: lo.ToPtr(true),
			},
			wantErr: false,
		},
		{
			name: "message with tools",
			input: &httpclient.Request{
				Method: "POST",
				URL:    "/api/chat",
				Headers: http.Header{
					"Content-Type": []string{"application/json"},
				},
				Body: []byte(`{
					"messages": [
						{
							"role": "user",
							"content": "What's the weather like?"
						}
					],
					"model": "gpt-4",
					"tools": [
						{
							"type": "function",
							"function": {
								"name": "get_weather",
								"description": "Get current weather",
								"parameters": {
									"type": "object",
									"properties": {
										"location": {
											"type": "string",
											"description": "The city name"
										}
									},
									"required": ["location"]
								}
							}
						}
					]
				}`),
			},
			expected: &llm.Request{
				Model: "gpt-4",
				Messages: []llm.Message{
					{
						Role: "user",
						Content: llm.MessageContent{
							Content: lo.ToPtr("What's the weather like?"),
						},
					},
				},
				Tools: []llm.Tool{
					{
						Type: "function",
						Function: llm.Function{
							Name:        "get_weather",
							Description: "Get current weather",
						},
					},
				},
				Stream: lo.ToPtr(true),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := transformer.TransformRequest(ctx, tt.input)

			if tt.wantErr {
				require.Error(t, err)
				require.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, llm.RequestTypeChat, result.RequestType)
				require.Equal(t, llm.APIFormatAiSDKDataStream, result.APIFormat)
				require.Equal(t, tt.expected.Model, result.Model)
				require.Equal(t, len(tt.expected.Messages), len(result.Messages))

				if len(tt.expected.Messages) > 0 {
					require.Equal(t, tt.expected.Messages[0].Role, result.Messages[0].Role)

					if tt.expected.Messages[0].Content.Content != nil {
						require.Equal(t, *tt.expected.Messages[0].Content.Content, *result.Messages[0].Content.Content)
					}
				}

				if len(tt.expected.Tools) > 0 {
					require.Equal(t, len(tt.expected.Tools), len(result.Tools))
					require.Equal(t, tt.expected.Tools[0].Type, result.Tools[0].Type)
					require.Equal(t, tt.expected.Tools[0].Function.Name, result.Tools[0].Function.Name)
				}
			}
		})
	}
}

func TestDataStreamTransformer_TransformResponse(t *testing.T) {
	transformer := NewDataStreamTransformer()
	ctx := context.Background()

	// Data stream protocol should not support non-streaming responses
	resp := &llm.Response{
		ID:     "test-id",
		Object: "chat.completion",
		Model:  "gpt-4",
	}

	result, err := transformer.TransformResponse(ctx, resp)
	require.Error(t, err)
	require.Nil(t, result)
	require.Contains(t, err.Error(), "data stream protocol only supports streaming responses")
}

func TestDataStreamTransformer_AggregateStreamChunks_ToolInputs(t *testing.T) {
	transformer := NewDataStreamTransformer()
	chunks := []*httpclient.StreamEvent{
		{Data: []byte(`{"type":"start","messageId":"msg_tools"}`)},
		{Data: []byte(`{"type":"tool-input-start","toolCallId":"call_city","toolName":"get_city"}`)},
		{Data: []byte(`{"type":"tool-input-delta","toolCallId":"call_city","inputTextDelta":"{\"userId\":\"stale"}`)},
		{Data: []byte(`{"type":"tool-input-start","toolCallId":"call_language","toolName":"get_language"}`)},
		{Data: []byte(`{"type":"tool-input-delta","toolCallId":"call_language","inputTextDelta":"{\"userId\":\"123\"}"}`)},
		{Data: []byte(`{"type":"tool-input-available","toolCallId":"call_city","toolName":"get_city","input":{"userId":"123"}}`)},
		{Data: []byte(`{"type":"tool-input-available","toolCallId":"call_language","toolName":"get_language","input":{"userId":"456"}}`)},
	}

	resultBytes, meta, err := transformer.AggregateStreamChunks(t.Context(), chunks)
	require.NoError(t, err)
	require.Equal(t, "msg_tools", meta.ID)

	var result UIMessage
	require.NoError(t, json.Unmarshal(resultBytes, &result))
	require.Equal(t, "assistant", result.Role)
	require.Len(t, result.Parts, 2)

	require.Equal(t, "tool-get_city", result.Parts[0].Type)
	require.Equal(t, "call_city", result.Parts[0].ToolCallID)
	require.Equal(t, "input-available", result.Parts[0].State)
	require.Empty(t, result.Parts[0].InputTextDelta)
	require.Equal(t, map[string]any{"userId": "123"}, result.Parts[0].Input)

	require.Equal(t, "tool-get_language", result.Parts[1].Type)
	require.Equal(t, "call_language", result.Parts[1].ToolCallID)
	require.Equal(t, "input-available", result.Parts[1].State)
	require.Empty(t, result.Parts[1].InputTextDelta)
	require.Equal(t, map[string]any{"userId": "456"}, result.Parts[1].Input)
}

func TestDataStreamTransformer_AggregateStreamChunks_ToolInputMissingEvents(t *testing.T) {
	transformer := NewDataStreamTransformer()

	t.Run("available without start", func(t *testing.T) {
		resultBytes, _, err := transformer.AggregateStreamChunks(t.Context(), []*httpclient.StreamEvent{
			{Data: []byte(`{"type":"tool-input-available","toolCallId":"call_1","toolName":"lookup","input":null}`)},
		})
		require.NoError(t, err)
		require.JSONEq(t,
			`{"role":"assistant","content":null,"parts":[{"type":"tool-lookup","state":"input-available","toolCallId":"call_1","input":null}]}`,
			string(resultBytes),
		)
	})

	t.Run("complete deltas without available", func(t *testing.T) {
		resultBytes, _, err := transformer.AggregateStreamChunks(t.Context(), []*httpclient.StreamEvent{
			{Data: []byte(`{"type":"tool-input-delta","toolCallId":"call_2","inputTextDelta":"{\"city\":"}`)},
			{Data: []byte(`{"type":"tool-input-start","toolCallId":"call_2","toolName":"weather"}`)},
			{Data: []byte(`{"type":"tool-input-delta","toolCallId":"call_2","inputTextDelta":"\"Paris\"}"}`)},
		})
		require.NoError(t, err)
		require.JSONEq(t,
			`{"role":"assistant","content":null,"parts":[{"type":"tool-weather","state":"input-streaming","toolCallId":"call_2","input":{"city":"Paris"}}]}`,
			string(resultBytes),
		)
	})

	t.Run("incomplete deltas remain inspectable", func(t *testing.T) {
		resultBytes, _, err := transformer.AggregateStreamChunks(t.Context(), []*httpclient.StreamEvent{
			{Data: []byte(`{"type":"tool-input-start","toolCallId":"call_3","toolName":"weather"}`)},
			{Data: []byte(`{"type":"tool-input-delta","toolCallId":"call_3","inputTextDelta":"{\"city\":\"Par"}`)},
		})
		require.NoError(t, err)
		require.JSONEq(t,
			`{"role":"assistant","content":null,"parts":[{"type":"tool-weather","state":"input-streaming","toolCallId":"call_3","inputTextDelta":"{\"city\":\"Par"}]}`,
			string(resultBytes),
		)
	})
}

func TestSetDataStreamHeaders(t *testing.T) {
	headers := make(http.Header)
	SetDataStreamHeaders(headers)

	require.Equal(t, "v1", headers.Get("X-Vercel-Ai-Ui-Message-Stream"))
	require.Equal(t, "text/event-stream", headers.Get("Content-Type"))
	require.Equal(t, "no-cache", headers.Get("Cache-Control"))
	require.Equal(t, "keep-alive", headers.Get("Connection"))
}

func TestStreamPartTypes(t *testing.T) {
	tests := []struct {
		name     string
		part     any
		expected string
	}{
		{
			name: "message start part",
			part: UIMessagePart{
				Type: "start",
				ID:   "msg_123",
			},
			expected: `{"type":"start","id":"msg_123"}`,
		},
		{
			name: "text start part",
			part: UIMessagePart{
				Type: "text-start",
				ID:   "text_123",
			},
			expected: `{"type":"text-start","id":"text_123"}`,
		},
		{
			name: "text delta part",
			part: UIMessagePart{
				Type:  "text-delta",
				ID:    "text_123",
				Delta: "Hello",
			},
			expected: `{"type":"text-delta","id":"text_123","delta":"Hello"}`,
		},
		{
			name: "text end part",
			part: UIMessagePart{
				Type: "text-end",
				ID:   "text_123",
			},
			expected: `{"type":"text-end","id":"text_123"}`,
		},
		{
			name: "tool input start part",
			part: UIMessagePart{
				Type:       "tool-input-start",
				ToolCallID: "call_123",
				ToolName:   "get_weather",
			},
			expected: `{"type":"tool-input-start","toolCallId":"call_123","toolName":"get_weather"}`,
		},
		{
			name: "tool input delta part",
			part: UIMessagePart{
				Type:           "tool-input-delta",
				ToolCallID:     "call_123",
				InputTextDelta: "San Francisco",
			},
			expected: `{"type":"tool-input-delta","toolCallId":"call_123","inputTextDelta":"San Francisco"}`,
		},
		{
			name: "tool input available part",
			part: UIMessagePart{
				Type:       "tool-input-available",
				ToolCallID: "call_123",
				ToolName:   "get_weather",
				Input:      map[string]string{"location": "San Francisco"},
			},
			expected: `{"type":"tool-input-available","toolCallId":"call_123","toolName":"get_weather","input":{"location":"San Francisco"}}`,
		},
		{
			name: "finish step part",
			part: UIMessagePart{
				Type: "finish-step",
			},
			expected: `{"type":"finish-step"}`,
		},
		{
			name: "finish message part",
			part: UIMessagePart{
				Type: "finish",
			},
			expected: `{"type":"finish"}`,
		},
		{
			name: "error part",
			part: UIMessagePart{
				Type:      "error",
				ErrorText: "Something went wrong",
			},
			expected: `{"type":"error","errorText":"Something went wrong"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			jsonBytes, err := json.Marshal(tt.part)
			require.NoError(t, err)
			require.JSONEq(t, tt.expected, string(jsonBytes))
		})
	}
}

func TestGenerateTextID(t *testing.T) {
	id1 := generateTextID()
	id2 := generateTextID()

	// IDs should be different
	require.NotEqual(t, id1, id2)

	// IDs should start with "msg_"
	require.True(t, strings.HasPrefix(id1, "msg_"))
	require.True(t, strings.HasPrefix(id2, "msg_"))

	// IDs should not contain hyphens (UUID hyphens are removed)
	require.False(t, strings.Contains(id1, "-"))
	require.False(t, strings.Contains(id2, "-"))
}

func TestDataStreamTransformer_TransformError(t *testing.T) {
	transformer := NewDataStreamTransformer()
	ctx := context.Background()

	tests := []struct {
		name           string
		input          error
		expectedStatus int
		expectedBody   string
	}{
		{
			name:           "nil error",
			input:          nil,
			expectedStatus: http.StatusInternalServerError,
			expectedBody:   `{"message":"internal server error","type":"internal_server_error"}`,
		},
		{
			name: "http client error",
			input: &httpclient.Error{
				StatusCode: http.StatusBadRequest,
				Status:     "Bad Request",
				Body:       []byte(`{"message":"bad request","type":"invalid_request"}`),
			},
			expectedStatus: http.StatusBadRequest,
			expectedBody:   `{"message":"bad request","type":"invalid_request"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := transformer.TransformError(ctx, tt.input)
			require.NotNil(t, result)
			require.Equal(t, tt.expectedStatus, result.StatusCode)
			require.JSONEq(t, tt.expectedBody, string(result.Body))
		})
	}
}
