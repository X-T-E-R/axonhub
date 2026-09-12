package orchestrator

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func TestPersistentOutboundTransformer_CommandCodeLiteHistoryToChat(t *testing.T) {
	ctx := t.Context()
	inbound := responses.NewInboundTransformer()
	chatOutbound, err := openai.NewOutboundTransformer("https://synthetic.invalid/v1", "synthetic-key")
	require.NoError(t, err)

	// Keep a non-capable primary outbound in place so this test proves that the
	// candidate's selected outbound, rather than API format alone, controls the
	// history guard.
	fallback := &mockTransformer{apiFormat: llm.APIFormatOpenAIChatCompletion}
	channel := &biz.Channel{
		Channel:  &ent.Channel{ID: 1, Name: "synthetic-chat"},
		Outbound: fallback,
		Outbounds: map[string]transformer.Outbound{
			llm.APIFormatOpenAIChatCompletion.String(): chatOutbound,
		},
	}
	candidate := &ChannelModelsCandidate{
		Channel:   channel,
		APIFormat: llm.APIFormatOpenAIChatCompletion.String(),
		Models: []biz.ChannelModelEntry{{
			RequestModel: "gpt-5.6-terra",
			ActualModel:  "gpt-5.6-terra",
		}},
	}
	state := &PersistenceState{
		ChannelModelsCandidates: []*ChannelModelsCandidate{candidate},
		CurrentCandidateIndex:   0,
		CurrentModelIndex:       0,
	}
	persistent := &PersistentOutboundTransformer{state: state}

	// Establish the first turn through the persistent outbound wrapper. The
	// following request then re-enters the same selected candidate with the
	// accumulated Codex history, just as a real Responses continuation does.
	initialBody := []byte(`{
		"model":"gpt-5.6-terra",
		"stream":true,
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"Start the synthetic turn."}]}]
	}`)
	initialRequest := &httpclient.Request{
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    initialBody,
	}
	initialLLMRequest, err := inbound.TransformRequest(ctx, initialRequest)
	require.NoError(t, err)
	initialWire, err := persistent.TransformRequest(ctx, initialLLMRequest)
	require.NoError(t, err)
	require.Same(t, chatOutbound, persistent.wrapped)
	require.Contains(t, string(initialWire.Body), "Start the synthetic turn.")
	require.Equal(t, initialBody, initialRequest.Body, "the first input body must remain untouched")

	requestBody := []byte(`{
		"model":"gpt-5.6-terra",
		"stream":true,
		"parallel_tool_calls":false,
		"reasoning":{"effort":"high","context":"all_turns"},
		"input":[
			{"type":"additional_tools","id":"at_synthetic","role":"developer","tools":[
				{"type":"namespace","name":"functions","description":"Run workspace commands.","tools":[
					{"type":"custom","name":"exec","description":"Run a workspace command.","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}
				]},
				{"type":"namespace","name":"collaboration","description":"Coordinate with the parent agent.","tools":[
					{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}},
					{"type":"function","name":"send_message","parameters":{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}},
					{"type":"function","name":"followup_task","parameters":{"type":"object","properties":{"message":{"type":"string"}},"required":["message"]}}
				]}
			]},
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"Preserve the command context."}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Inspect the synthetic workspace."}]},
			{"type":"agent_message","id":"amsg_parent","author":"/root","recipient":"/root/worker","content":[
				{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\n"},
				{"type":"encrypted_content","encrypted_content":"synthetic plain brief"}
			]},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"Synthetic summary"}],"content":[{"type":"reasoning_text","text":"Synthetic explicit reasoning"}],"encrypted_content":"synthetic opaque signature"},
			{"type":"custom_tool_call","id":"ct_exec","call_id":"call_exec","name":"exec","namespace":"functions","input":"pwd"},
			{"type":"custom_tool_call_output","call_id":"call_exec","name":"exec","output":"Directory: synthetic/workspace\\nREADME.md\\nsrc/"},
			{"type":"agent_message","id":"amsg_followup","author":"/root","recipient":"/root/worker","content":[{"type":"input_text","text":"Followup: report the directory result."}]},
			{"type":"function_call","id":"fc_spawn","call_id":"call_spawn","name":"spawn_agent","namespace":"collaboration","arguments":"{\"message\":\"Delegate a synthetic check.\"}","encrypted_function_args":[]},
			{"type":"function_call_output","call_id":"call_spawn","output":"delegated synthetic check"},
			{"type":"function_call","id":"fc_send","call_id":"call_send","name":"send_message","namespace":"collaboration","arguments":"{\"message\":\"Directory result is ready.\"}","encrypted_function_args":[]},
			{"type":"function_call","id":"fc_followup","call_id":"call_followup","name":"followup_task","namespace":"collaboration","arguments":"{\"message\":\"Continue from the directory result.\"}","encrypted_function_args":[]}
		]
	}`)
	inboundRequest := &httpclient.Request{
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    requestBody,
	}
	llmRequest, err := inbound.TransformRequest(ctx, inboundRequest)
	require.NoError(t, err)
	require.Equal(t, llm.APIFormatOpenAIResponse, llmRequest.APIFormat)
	require.NotNil(t, llmRequest.ProviderExtensions)
	require.NotNil(t, llmRequest.ProviderExtensions.OpenAIResponses)
	require.NotNil(t, llmRequest.ProviderExtensions.OpenAIResponses.Request)
	require.Len(t, llmRequest.ProviderExtensions.OpenAIResponses.Request.RawInputItems, 1)

	beforeMessages, err := json.Marshal(llmRequest.Messages)
	require.NoError(t, err)

	wire, err := persistent.TransformRequest(ctx, llmRequest)
	require.NoError(t, err)
	require.Same(t, chatOutbound, persistent.wrapped)
	require.Equal(t, requestBody, inboundRequest.Body, "the incoming request body must remain untouched")
	afterMessages, err := json.Marshal(llmRequest.Messages)
	require.NoError(t, err)
	require.JSONEq(t, string(beforeMessages), string(afterMessages), "outbound conversion must not mutate the unified history")

	var chatRequest openai.Request
	require.NoError(t, json.Unmarshal(wire.Body, &chatRequest))
	require.Len(t, chatRequest.Tools, 4, "the Lite catalog must be lowered into Chat function tools")
	require.NotContains(t, string(wire.Body), "additional_tools")
	require.Contains(t, string(wire.Body), "Run workspace commands.")
	require.NotContains(t, string(wire.Body), "synthetic opaque signature")
	require.NotNil(t, chatRequest.ParallelToolCalls)
	require.False(t, *chatRequest.ParallelToolCalls)

	var (
		parentAgent     *openai.Message
		followupAgent   *openai.Message
		execCall        *openai.ToolCall
		execOutput      *openai.Message
		reasoning       *openai.Message
		collaborationID = map[string]bool{}
	)
	for i := range chatRequest.Messages {
		message := &chatRequest.Messages[i]
		if message.Content.Content != nil {
			switch {
			case strings.Contains(*message.Content.Content, "synthetic plain brief"):
				parentAgent = message
			case strings.Contains(*message.Content.Content, "Followup: report the directory result."):
				followupAgent = message
			case strings.Contains(*message.Content.Content, "Directory: synthetic/workspace"):
				execOutput = message
			}
		}
		if message.ReasoningContent != nil && *message.ReasoningContent == "Synthetic explicit reasoning" {
			reasoning = message
		}
		for j := range message.ToolCalls {
			call := &message.ToolCalls[j]
			if call.ID == "call_exec" {
				execCall = call
			}
			if call.ID == "call_spawn" || call.ID == "call_send" || call.ID == "call_followup" {
				collaborationID[call.ID] = true
			}
		}
	}
	require.NotNil(t, parentAgent)
	require.Equal(t, "user", parentAgent.Role)
	require.Contains(t, *parentAgent.Content.Content, "Agent message from /root to /root/worker:")
	require.NotNil(t, followupAgent)
	require.Equal(t, "user", followupAgent.Role)
	require.NotNil(t, reasoning)
	require.NotNil(t, execCall)
	require.Equal(t, "function", execCall.Type)
	require.Equal(t, "functions__exec", execCall.Function.Name)
	require.JSONEq(t, `{"input":"pwd"}`, execCall.Function.Arguments)
	require.NotNil(t, execOutput)
	require.Equal(t, "tool", execOutput.Role)
	require.NotNil(t, execOutput.ToolCallID)
	require.Equal(t, "call_exec", *execOutput.ToolCallID)
	require.Equal(t, map[string]bool{"call_spawn": true, "call_send": true, "call_followup": true}, collaborationID)

	// Feed a synthetic Chat completion through the same per-request binding and
	// then back to Responses. A bare exec alias is accepted only because this
	// catalog has one exec binding; collaboration calls regain their namespace
	// and the explicit empty encrypted-argument marker.
	chatResponse := &httpclient.Response{
		Request:    wire,
		StatusCode: http.StatusOK,
		Body:       []byte(`{"id":"synthetic-chat-response","model":"gpt-5.6-terra","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_exec_next","type":"function","function":{"name":"exec","arguments":"{\"input\":\"ls\"}"}},{"id":"call_followup_next","type":"function","function":{"name":"followup_task","arguments":"{\"message\":\"Continue\"}"}}]},"finish_reason":"tool_calls"}]}`),
	}
	decoded, err := chatOutbound.TransformResponse(ctx, chatResponse)
	require.NoError(t, err)
	responsesResponse, err := inbound.TransformResponse(ctx, decoded)
	require.NoError(t, err)
	var responsesBody responses.Response
	require.NoError(t, json.Unmarshal(responsesResponse.Body, &responsesBody))
	require.Len(t, responsesBody.Output, 2)
	require.Equal(t, "custom_tool_call", responsesBody.Output[0].Type)
	require.Equal(t, "functions", responsesBody.Output[0].Namespace)
	require.Equal(t, "exec", responsesBody.Output[0].Name)
	require.Equal(t, "ls", *responsesBody.Output[0].Input)
	require.Equal(t, "function_call", responsesBody.Output[1].Type)
	require.Equal(t, "collaboration", responsesBody.Output[1].Namespace)
	require.Equal(t, "followup_task", responsesBody.Output[1].Name)
	require.NotNil(t, responsesBody.Output[1].EncryptedFunctionArgs)
	require.Empty(t, responsesBody.Output[1].EncryptedFunctionArgs)
	var encodedResponse map[string]any
	require.NoError(t, json.Unmarshal(responsesResponse.Body, &encodedResponse))
	output := encodedResponse["output"].([]any)
	marker, ok := output[1].(map[string]any)["encrypted_function_args"]
	require.True(t, ok)
	require.Equal(t, []any{}, marker)
}
