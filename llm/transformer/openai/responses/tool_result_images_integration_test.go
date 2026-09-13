package responses_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func TestChatBridgeToolImagesPreserveBindingsAndNativeResponses(t *testing.T) {
	body := []byte(`{"model":"test","tools":[
		{"type":"custom","name":"view_image"},
		{"type":"function","name":"inspect","parameters":{"type":"object"}}],
	"input":[
		{"type":"custom_tool_call","call_id":"call_first","name":"view_image","input":"first.png"},
		{"type":"custom_tool_call_output","call_id":"call_first","output":[
			{"type":"input_text","text":"exact first text"},
			{"type":"input_image","image_url":"https://example.invalid/first.png","detail":"high"}]},
		{"type":"function_call","call_id":"call_second","name":"inspect","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_second","output":[
			{"type":"input_image","image_url":"https://example.invalid/second.png","detail":"low"}]},
		{"type":"agent_message","author":"/root","recipient":"/root/worker","content":[{"type":"input_text","text":"Describe the images"}]}
	]}`)
	in := responses.NewInboundTransformer()
	source, err := in.TransformRequest(t.Context(), &httpclient.Request{Body: body})
	require.NoError(t, err)
	before, err := json.Marshal(source)
	require.NoError(t, err)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var chat openai.Request
		if json.NewDecoder(r.Body).Decode(&chat) != nil {
			http.Error(w, "invalid JSON", http.StatusBadRequest)
			return
		}
		for _, message := range chat.Messages {
			if message.Role == "tool" {
				for _, part := range message.Content.MultipleContent {
					if part.Type != "text" {
						http.Error(w, "tool content must be text", http.StatusBadRequest)
						return
					}
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chat_synthetic","model":"test","choices":[{"index":0,"message":{"role":"assistant","content":"Both images received"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	out, err := openai.NewOutboundTransformer(server.URL, "synthetic")
	require.NoError(t, err)
	wire, err := out.TransformRequest(t.Context(), source)
	require.NoError(t, err)
	var chat openai.Request
	require.NoError(t, json.Unmarshal(wire.Body, &chat))
	require.Len(t, chat.Messages, 7)
	require.Equal(t, "function", chat.Messages[0].ToolCalls[0].Type)
	require.JSONEq(t, `{"input":"first.png"}`, chat.Messages[0].ToolCalls[0].Function.Arguments)
	require.Equal(t, "call_first", *chat.Messages[1].ToolCallID)
	require.Equal(t, "exact first text", *chat.Messages[1].Content.MultipleContent[0].Text)
	require.Equal(t, "user", chat.Messages[2].Role)
	require.Equal(t, "high", *chat.Messages[2].Content.MultipleContent[1].ImageURL.Detail)
	require.Equal(t, "inspect", chat.Messages[3].ToolCalls[0].Function.Name, "expansion must not change later tool binding indexes")
	require.Equal(t, "call_second", *chat.Messages[4].ToolCallID)
	require.Equal(t, "low", *chat.Messages[5].Content.MultipleContent[1].ImageURL.Detail)
	require.Contains(t, *chat.Messages[6].Content.Content, "Describe the images")

	httpRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, wire.URL, bytes.NewReader(wire.Body))
	require.NoError(t, err)
	httpResponse, err := server.Client().Do(httpRequest)
	require.NoError(t, err)
	defer httpResponse.Body.Close()
	require.Equal(t, http.StatusOK, httpResponse.StatusCode)
	resultBody, err := io.ReadAll(httpResponse.Body)
	require.NoError(t, err)
	unified, err := out.TransformResponse(t.Context(), &httpclient.Response{StatusCode: httpResponse.StatusCode, Body: resultBody, Request: wire})
	require.NoError(t, err)
	result, err := in.TransformResponse(t.Context(), unified)
	require.NoError(t, err)
	require.Contains(t, string(result.Body), "Both images received")

	after, err := json.Marshal(source)
	require.NoError(t, err)
	require.Equal(t, before, after)
	native, err := responses.NewOutboundTransformer("https://example.invalid", "synthetic")
	require.NoError(t, err)
	nativeWire, err := native.TransformRequest(t.Context(), source)
	require.NoError(t, err)
	var nativeRequest responses.Request
	require.NoError(t, json.Unmarshal(nativeWire.Body, &nativeRequest))
	require.Contains(t, string(nativeWire.Body), `"type":"input_image"`)
	require.NotContains(t, string(nativeWire.Body), "attached below")
}
