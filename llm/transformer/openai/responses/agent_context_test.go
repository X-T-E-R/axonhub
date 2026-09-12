package responses_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func TestChatBridgeAgentMessagesAndVisibleReasoning(t *testing.T) {
	input := []byte(`{"model":"test","input":[
		{"type":"message","role":"developer","content":"Keep instructions"},
		{"type":"agent_message","id":"amsg_synthetic","author":"/root","recipient":"/root/worker",
		 "internal_chat_message_metadata_passthrough":{"turn_id":"turn_synthetic","timestamp":123,"content_classification":"task"},
		 "future_metadata":{"keep":["all","fields"]},"content":[
			{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\n"},
			{"type":"encrypted_content","encrypted_content":"Inspect the synthetic workspace and report."}]},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"Brief summary"}],"content":[{"type":"reasoning_text","text":"Explicit detailed reasoning"}],"encrypted_content":"opaque-signature"},
		{"type":"function_call","call_id":"call_read","name":"read","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_read","output":"synthetic file"},
		{"type":"agent_message","id":"amsg_followup","author":"/root","recipient":"/root/worker","content":[{"type":"input_text","text":"Followup: report the result now."}]}
	]}`)
	in := responses.NewInboundTransformer()
	r, err := in.TransformRequest(t.Context(), &httpclient.Request{Body: input})
	require.NoError(t, err)
	out, err := openai.NewOutboundTransformer("https://example.invalid/v1", "synthetic")
	require.NoError(t, err)
	wire, err := out.TransformRequest(t.Context(), r)
	require.NoError(t, err)
	var chat openai.Request
	require.NoError(t, json.Unmarshal(wire.Body, &chat))
	require.Len(t, chat.Messages, 5)
	require.Equal(t, "user", chat.Messages[1].Role)
	require.Contains(t, *chat.Messages[1].Content.Content, "/root/worker")
	require.Contains(t, *chat.Messages[1].Content.Content, "Inspect the synthetic workspace and report.")
	require.Equal(t, "Explicit detailed reasoning", *chat.Messages[2].ReasoningContent)
	require.Contains(t, *chat.Messages[4].Content.Content, "Followup: report the result now.")
	require.NotContains(t, string(wire.Body), "opaque-signature")
	require.NotContains(t, string(wire.Body), "ResponsesAgentMessage")

	native, err := responses.NewOutboundTransformer("https://example.invalid/v1", "synthetic")
	require.NoError(t, err)
	nativeWire, err := native.TransformRequest(t.Context(), r)
	require.NoError(t, err)
	var nativeRequest responses.Request
	require.NoError(t, json.Unmarshal(nativeWire.Body, &nativeRequest))
	agents := []responses.Item{}
	for _, item := range nativeRequest.Input.Items {
		if item.Type == "agent_message" {
			agents = append(agents, item)
		}
	}
	require.Len(t, agents, 2, "native forwarding must not duplicate rendered and original envelopes")
	require.Equal(t, "amsg_synthetic", agents[0].ID)
	require.Equal(t, "/root", agents[0].Author)
	require.Equal(t, "/root/worker", agents[0].Recipient)
	require.Equal(t, "Inspect the synthetic workspace and report.", *agents[0].Content.Items[1].EncryptedContent)
	require.Equal(t, "amsg_followup", agents[1].ID)
	var sourceEnvelope struct {
		Input []json.RawMessage `json:"input"`
	}
	require.NoError(t, json.Unmarshal(input, &sourceEnvelope))
	restoredEnvelope, err := json.Marshal(agents[0])
	require.NoError(t, err)
	require.JSONEq(t, string(sourceEnvelope.Input[1]), string(restoredEnvelope))
	require.NotContains(t, string(wire.Body), "internal_chat_message_metadata_passthrough")
	require.NotContains(t, string(wire.Body), "future_metadata")
}

func TestChatBridgeCollaborationPlaintextMarker(t *testing.T) {
	in := responses.NewInboundTransformer()
	r, err := in.TransformRequest(t.Context(), &httpclient.Request{Body: []byte(`{"model":"test","input":"Dispatch","tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string"}}}}]}]}`)})
	require.NoError(t, err)
	out, err := openai.NewOutboundTransformer("https://example.invalid/v1", "synthetic")
	require.NoError(t, err)
	wire, err := out.TransformRequest(t.Context(), r)
	require.NoError(t, err)
	events := []*httpclient.StreamEvent{
		{Data: []byte(`{"id":"synthetic","model":"test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_spawn","type":"function","function":{"name":"spawn_agent","arguments":"{\"message\":\"Plain brief\"}"}}]},"finish_reason":"tool_calls"}]}`)},
		{Data: []byte(`[DONE]`)},
	}
	source, err := out.TransformStream(t.Context(), wire, streams.SliceStream(events))
	require.NoError(t, err)
	result, err := in.TransformStream(t.Context(), source)
	require.NoError(t, err)
	done, completed := false, false
	for result.Next() {
		var event map[string]any
		require.NoError(t, json.Unmarshal(result.Current().Data, &event))
		var item map[string]any
		switch event["type"] {
		case "response.output_item.done":
			item = event["item"].(map[string]any)
			done = true
		case "response.completed":
			output := event["response"].(map[string]any)["output"].([]any)
			item = output[0].(map[string]any)
			completed = true
		}
		if item != nil {
			require.Equal(t, "spawn_agent", item["name"])
			require.Equal(t, "collaboration", item["namespace"])
			marker, exists := item["encrypted_function_args"]
			require.True(t, exists, "Codex requires present empty list, not missing/null")
			require.Equal(t, []any{}, marker)
		}
	}
	require.NoError(t, result.Err())
	require.True(t, done)
	require.True(t, completed)
}
