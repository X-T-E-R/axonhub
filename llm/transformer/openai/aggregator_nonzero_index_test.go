package openai

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

// TestAggregateStreamChunksNonZeroChoiceIndex ensures that a stream whose only
// choice carries a non-zero index aggregates without panicking. choicesAggs is
// keyed by the choice index, so a sparse/non-zero-based index must not be
// looked up positionally.
func TestAggregateStreamChunksNonZeroChoiceIndex(t *testing.T) {
	chunk := `{"id":"chatcmpl-1","model":"gpt-4o-mini","object":"chat.completion.chunk","created":1,` +
		`"choices":[{"index":1,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`

	chunks := []*httpclient.StreamEvent{{Data: []byte(chunk)}}

	gotBytes, _, err := AggregateStreamChunks(context.Background(), chunks, DefaultTransformChunk)
	require.NoError(t, err)

	var got llm.Response
	require.NoError(t, json.Unmarshal(gotBytes, &got))
	require.Len(t, got.Choices, 1)
	require.Equal(t, 1, got.Choices[0].Index)
	require.NotNil(t, got.Choices[0].Message.Content.Content)
	require.Equal(t, "hi", *got.Choices[0].Message.Content.Content)
}

func TestAggregateStreamChunksNoUsage(t *testing.T) {
	chunk := `{"id":"chatcmpl-1","model":"gpt-4o-mini","object":"chat.completion.chunk","created":1,` +
		`"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`

	gotBytes, meta, err := AggregateStreamChunks(context.Background(), []*httpclient.StreamEvent{{Data: []byte(chunk)}}, DefaultTransformChunk)
	require.NoError(t, err)
	require.Nil(t, meta.Usage)

	var got llm.Response
	require.NoError(t, json.Unmarshal(gotBytes, &got))
	require.Nil(t, got.Usage)
	require.Len(t, got.Choices, 1)
	require.Equal(t, "hi", *got.Choices[0].Message.Content.Content)
}

func TestAggregateStreamChunksNonZeroToolCallIndex(t *testing.T) {
	chunks := []*httpclient.StreamEvent{
		{
			Data: []byte(`{"id":"chatcmpl-1","model":"gpt-4o-mini","object":"chat.completion.chunk","created":1,` +
				`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":1,"id":"call_1","type":"function","function":{"name":"search","arguments":"{\"q\":"}}]}}]}`),
		},
		{
			Data: []byte(`{"id":"chatcmpl-1","model":"gpt-4o-mini","object":"chat.completion.chunk","created":1,` +
				`"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"axonhub\"}"}}]},"finish_reason":"tool_calls"}]}`),
		},
	}

	gotBytes, _, err := AggregateStreamChunks(context.Background(), chunks, DefaultTransformChunk)
	require.NoError(t, err)

	var got llm.Response
	require.NoError(t, json.Unmarshal(gotBytes, &got))
	require.Len(t, got.Choices, 1)
	require.Len(t, got.Choices[0].Message.ToolCalls, 1)
	require.Equal(t, 1, got.Choices[0].Message.ToolCalls[0].Index)
	require.Equal(t, "call_1", got.Choices[0].Message.ToolCalls[0].ID)
	require.Equal(t, "search", got.Choices[0].Message.ToolCalls[0].Function.Name)
	require.Equal(t, `{"q":"axonhub"}`, got.Choices[0].Message.ToolCalls[0].Function.Arguments)
}

func TestAggregateStreamChunksMessageToolCallsOverridePartialDeltas(t *testing.T) {
	tests := []struct {
		name      string
		chunks    []*httpclient.StreamEvent
		arguments string
	}{
		{
			name: "complete message value without deltas",
			chunks: []*httpclient.StreamEvent{
				{
					Data: []byte(`{"id":"chatcmpl-terminal","model":"gpt-4o-mini","object":"chat.completion.chunk","created":1,` +
						`"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"index":0,"id":"call_terminal","type":"function","function":{"name":"search","arguments":"{\"q\":\"axonhub\"}"}}]},"finish_reason":"tool_calls"}]}`),
				},
			},
			arguments: `{"q":"axonhub"}`,
		},
		{
			name: "complete message value replaces partial delta",
			chunks: []*httpclient.StreamEvent{
				{
					Data: []byte(`{"id":"chatcmpl-terminal","model":"gpt-4o-mini","object":"chat.completion.chunk","created":1,` +
						`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_terminal","type":"function","function":{"name":"search","arguments":"{\"q\":"}}]}}]}`),
				},
				{
					Data: []byte(`{"id":"chatcmpl-terminal","model":"gpt-4o-mini","object":"chat.completion.chunk","created":1,` +
						`"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"index":0,"id":"call_terminal","type":"function","function":{"name":"search","arguments":"{\"q\":\"axonhub\"}"}}]},"finish_reason":"tool_calls"}]}`),
				},
			},
			arguments: `{"q":"axonhub"}`,
		},
		{
			name: "ordinary deltas still concatenate",
			chunks: []*httpclient.StreamEvent{
				{
					Data: []byte(`{"id":"chatcmpl-terminal","model":"gpt-4o-mini","object":"chat.completion.chunk","created":1,` +
						`"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_terminal","type":"function","function":{"name":"search","arguments":"{\"q\":"}}]}}]}`),
				},
				{
					Data: []byte(`{"id":"chatcmpl-terminal","model":"gpt-4o-mini","object":"chat.completion.chunk","created":1,` +
						`"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"axonhub\"}"}}]},"finish_reason":"tool_calls"}]}`),
				},
			},
			arguments: `{"q":"axonhub"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotBytes, _, err := AggregateStreamChunks(t.Context(), tt.chunks, DefaultTransformChunk)
			require.NoError(t, err)

			var got llm.Response
			require.NoError(t, json.Unmarshal(gotBytes, &got))
			require.Len(t, got.Choices, 1)
			require.Len(t, got.Choices[0].Message.ToolCalls, 1)
			require.Equal(t, "call_terminal", got.Choices[0].Message.ToolCalls[0].ID)
			require.Equal(t, "search", got.Choices[0].Message.ToolCalls[0].Function.Name)
			require.Equal(t, tt.arguments, got.Choices[0].Message.ToolCalls[0].Function.Arguments)
		})
	}
}

func TestAggregateStreamChunksJSONCompletionPreservesMessageAndUsage(t *testing.T) {
	chunk := &httpclient.StreamEvent{
		Type: httpclient.JSONStreamEventType,
		Data: []byte(`{
			"id":"chatcmpl-json","object":"chat.completion","created":42,"model":"command-r",
			"choices":[{"index":0,"message":{"role":"assistant","content":"complete reply","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"}},
				{"id":"call_2","type":"function","function":{"name":"list_files","arguments":"{\"path\":\"docs\"}"}}
			]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}
		}`),
	}

	gotBytes, meta, err := AggregateStreamChunks(t.Context(), []*httpclient.StreamEvent{chunk}, DefaultTransformChunk)
	require.NoError(t, err)
	require.Equal(t, "chatcmpl-json", meta.ID)

	var got llm.Response
	require.NoError(t, json.Unmarshal(gotBytes, &got))
	require.Equal(t, "chat.completion", got.Object)
	require.Len(t, got.Choices, 1)
	require.NotNil(t, got.Choices[0].Message)
	require.NotNil(t, got.Choices[0].Message.Content.Content)
	require.Equal(t, "complete reply", *got.Choices[0].Message.Content.Content)
	require.Len(t, got.Choices[0].Message.ToolCalls, 2)
	require.Equal(t, "call_1", got.Choices[0].Message.ToolCalls[0].ID)
	require.Equal(t, 0, got.Choices[0].Message.ToolCalls[0].Index)
	require.Equal(t, `{"path":"README.md"}`, got.Choices[0].Message.ToolCalls[0].Function.Arguments)
	require.Equal(t, "call_2", got.Choices[0].Message.ToolCalls[1].ID)
	require.Equal(t, 1, got.Choices[0].Message.ToolCalls[1].Index)
	require.Equal(t, "list_files", got.Choices[0].Message.ToolCalls[1].Function.Name)
	require.Equal(t, `{"path":"docs"}`, got.Choices[0].Message.ToolCalls[1].Function.Arguments)
	require.NotNil(t, got.Usage)
	require.Equal(t, int64(10), got.Usage.TotalTokens)
}

func TestAggregateStreamChunksJSONErrorPreservesProviderBody(t *testing.T) {
	body := []byte(`{"error":{"message":"quota exhausted","type":"insufficient_quota","code":"quota"}}`)

	got, meta, err := AggregateStreamChunks(t.Context(), []*httpclient.StreamEvent{{
		Type: httpclient.JSONStreamEventType,
		Data: body,
	}}, DefaultTransformChunk)

	require.NoError(t, err)
	require.Empty(t, meta.ID)
	require.JSONEq(t, string(body), string(got))
}
