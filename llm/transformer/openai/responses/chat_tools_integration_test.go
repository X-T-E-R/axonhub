package responses_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func TestChatBridgePreservesCodexCustomTool(t *testing.T) {
	in := responses.NewInboundTransformer()
	out, err := openai.NewOutboundTransformer("https://example.invalid/v1", "synthetic")
	require.NoError(t, err)
	r, err := in.TransformRequest(t.Context(), &httpclient.Request{
		Body: []byte(`{"model":"test","input":"Use the tool","tools":[{"type":"custom","name":"apply_patch","description":"Apply a patch","format":{"type":"grammar","syntax":"lark","definition":"start: \"patch\""}}]}`),
	})
	require.NoError(t, err)
	wire, err := out.TransformRequest(t.Context(), r)
	require.NoError(t, err)
	var chat openai.Request
	require.NoError(t, json.Unmarshal(wire.Body, &chat))
	require.Len(t, chat.Tools, 1, "a custom tool must be lowered, not silently deleted")
	require.Equal(t, "function", chat.Tools[0].Type)
	var schema map[string]any
	require.NoError(t, json.Unmarshal(chat.Tools[0].Function.Parameters, &schema))
	require.Equal(t, "object", schema["type"])

	decoded, err := out.TransformResponse(t.Context(), &httpclient.Response{
		Request: wire, StatusCode: http.StatusOK,
		Body: []byte(`{"id":"synthetic","model":"test","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_patch","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"patch\"}"}}]},"finish_reason":"tool_calls"}]}`),
	})
	require.NoError(t, err)
	result, err := in.TransformResponse(t.Context(), decoded)
	require.NoError(t, err)
	var resp responses.Response
	require.NoError(t, json.Unmarshal(result.Body, &resp))
	require.Equal(t, "custom_tool_call", resp.Output[0].Type)
	require.Equal(t, "patch", *resp.Output[0].Input)
	require.Equal(t, "call_patch", resp.Output[0].CallID)
}

func TestChatBridgeCustomStreamAndHistory(t *testing.T) {
	in := responses.NewInboundTransformer()
	out, err := openai.NewOutboundTransformer("https://example.invalid/v1", "synthetic")
	require.NoError(t, err)
	r, err := in.TransformRequest(t.Context(), &httpclient.Request{
		Headers: http.Header{"X-Openai-Internal-Codex-Responses-Lite": []string{"true"}},
		Body:    []byte(`{"model":"test","stream":true,"input":"Use the tool","tools":[{"type":"custom","name":"apply_patch","description":"Apply a patch"}]}`),
	})
	require.NoError(t, err)
	wire, err := out.TransformRequest(t.Context(), r)
	require.NoError(t, err)
	chunks := []string{
		`{"id":"synthetic","model":"test","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"Checking"},"finish_reason":null}]}`,
		`{"id":"synthetic","model":"test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_patch","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"a\\n"}}]},"finish_reason":null}]}`,
		`{"id":"synthetic","model":"test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"b\"}"}}]},"finish_reason":null}]}`,
		`{"id":"synthetic","model":"test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`{"id":"synthetic","model":"test","choices":[],"usage":{"prompt_tokens":8,"completion_tokens":4,"total_tokens":12}}`,
		`[DONE]`,
	}
	events := make([]*httpclient.StreamEvent, 0, len(chunks))
	for _, chunk := range chunks {
		events = append(events, &httpclient.StreamEvent{Data: []byte(chunk)})
	}
	decoded, err := out.TransformStream(t.Context(), wire, streams.SliceStream(events))
	require.NoError(t, err)
	result, err := in.TransformStream(t.Context(), decoded)
	require.NoError(t, err)
	var completed responses.Response
	var inputDeltas string
	for result.Next() {
		var event responses.StreamEvent
		require.NoError(t, json.Unmarshal(result.Current().Data, &event))
		if event.Type == responses.StreamEventTypeCustomToolCallInputDelta {
			inputDeltas += event.Delta
		}
		if event.Type == responses.StreamEventTypeResponseCompleted {
			completed = *event.Response
		}
	}
	require.NoError(t, result.Err())
	require.Equal(t, "a\nb", inputDeltas)
	require.NotNil(t, completed.Usage)
	var custom responses.Item
	for _, item := range completed.Output {
		if item.Type == "custom_tool_call" {
			custom = item
		}
	}
	require.Equal(t, "call_patch", custom.CallID)
	require.Equal(t, "a\nb", *custom.Input, "first custom delta must not be duplicated")

	history := map[string]any{
		"model": "test", "tools": []any{map[string]any{"type": "custom", "name": "apply_patch"}},
		"input": []any{custom, map[string]any{"type": "custom_tool_call_output", "call_id": custom.CallID, "output": "ok"}},
	}
	body, err := json.Marshal(history)
	require.NoError(t, err)
	next, err := in.TransformRequest(t.Context(), &httpclient.Request{Body: body})
	require.NoError(t, err)
	nextWire, err := out.TransformRequest(t.Context(), next)
	require.NoError(t, err)
	var chat openai.Request
	require.NoError(t, json.Unmarshal(nextWire.Body, &chat))
	require.Equal(t, "function", chat.Messages[0].ToolCalls[0].Type)
	require.JSONEq(t, `{"input":"a\nb"}`, chat.Messages[0].ToolCalls[0].Function.Arguments)
	require.Equal(t, custom.CallID, *chat.Messages[1].ToolCallID)
}

func TestChatBridgeRestoresNamespace(t *testing.T) {
	in := responses.NewInboundTransformer()
	out, err := openai.NewOutboundTransformer("https://example.invalid/v1", "synthetic")
	require.NoError(t, err)
	r, err := in.TransformRequest(t.Context(), &httpclient.Request{
		Body: []byte(`{"model":"test","input":"Use the tool","tools":[{"type":"namespace","name":"functions","description":"Operate only on supplied values.","tools":[{"type":"function","name":"echo","parameters":{"type":"object","properties":{"value":{"type":"string"}}}}]}]}`),
	})
	require.NoError(t, err)
	wire, err := out.TransformRequest(t.Context(), r)
	require.NoError(t, err)
	require.Contains(t, string(wire.Body), "Operate only on supplied values.")
	decoded, err := out.TransformResponse(t.Context(), &httpclient.Response{
		Request: wire, StatusCode: http.StatusOK,
		Body: []byte(`{"id":"synthetic","model":"test","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_echo","type":"function","function":{"name":"functions__echo","arguments":"{\"value\":\"ok\"}"}}]},"finish_reason":"tool_calls"}]}`),
	})
	require.NoError(t, err)
	result, err := in.TransformResponse(t.Context(), decoded)
	require.NoError(t, err)
	var resp responses.Response
	require.NoError(t, json.Unmarshal(result.Body, &resp))
	require.Equal(t, "function_call", resp.Output[0].Type)
	require.Equal(t, "echo", resp.Output[0].Name)
	require.Equal(t, "functions", resp.Output[0].Namespace)
}

func TestChatBridgeLiteCatalog(t *testing.T) {
	// Codex build_responses_request + create_tools_json_for_responses_lite:
	// tools live in input.additional_tools, and custom tools have namespaces.
	body := []byte(`{
		"model":"gpt-5.6-terra","stream":true,"parallel_tool_calls":false,"tool_choice":"auto",
		"reasoning":{"effort":"high","context":"all_turns"},
		"input":[
			{"type":"additional_tools","id":"at_synthetic","role":"developer","tools":[
				{"type":"namespace","name":"functions","description":"Operate only on supplied values.","tools":[{"type":"function","name":"echo","parameters":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}}]},
				{"type":"namespace","name":"editor","description":"Use only workspace-relative paths.","tools":[{"type":"custom","name":"patch","description":"Apply patch","format":{"type":"grammar","syntax":"lark","definition":"start: \"patch\""}}]}
			]},
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"Preserve this instruction"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Call editor.patch"}]}
		]
	}`)
	in := responses.NewInboundTransformer()
	r, err := in.TransformRequest(t.Context(), &httpclient.Request{Body: body})
	require.NoError(t, err)
	out, err := openai.NewOutboundTransformer("https://example.invalid/v1", "synthetic")
	require.NoError(t, err)
	wire, err := out.TransformRequest(t.Context(), r)
	require.NoError(t, err)
	var chat openai.Request
	require.NoError(t, json.Unmarshal(wire.Body, &chat))
	require.Len(t, chat.Tools, 2)
	require.Equal(t, "functions__echo", chat.Tools[0].Function.Name)
	require.Contains(t, chat.Tools[0].Function.Description, "Operate only on supplied values.")
	require.JSONEq(t, `{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`, string(chat.Tools[0].Function.Parameters))
	require.Equal(t, "editor__patch", chat.Tools[1].Function.Name)
	require.Contains(t, chat.Tools[1].Function.Description, `start: "patch"`)
	require.Contains(t, chat.Tools[1].Function.Description, "Use only workspace-relative paths.")
	require.Contains(t, chat.Tools[1].Function.Description, "Apply patch")
	require.NotNil(t, chat.ParallelToolCalls)
	require.False(t, *chat.ParallelToolCalls)
	require.Len(t, chat.Messages, 2)
	require.Contains(t, string(wire.Body), "Preserve this instruction")
	require.NotContains(t, string(wire.Body), "additional_tools")

	reply := []byte(`{"id":"synthetic","model":"test","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_patch","type":"function","function":{"name":"editor__patch","arguments":"{\"input\":\"patch\"}"}}]},"finish_reason":"tool_calls"}]}`)
	decoded, err := out.TransformResponse(t.Context(), &httpclient.Response{Request: wire, StatusCode: 200, Body: reply})
	require.NoError(t, err)
	result, err := in.TransformResponse(t.Context(), decoded)
	require.NoError(t, err)
	var response responses.Response
	require.NoError(t, json.Unmarshal(result.Body, &response))
	require.Equal(t, "custom_tool_call", response.Output[0].Type)
	require.Equal(t, "editor", response.Output[0].Namespace)
	require.Equal(t, "patch", response.Output[0].Name)
	require.Equal(t, "patch", *response.Output[0].Input)

	var next map[string]any
	require.NoError(t, json.Unmarshal(body, &next))
	next["input"] = append(next["input"].([]any), response.Output[0], map[string]any{
		"type": "custom_tool_call_output", "call_id": "call_patch", "output": "synthetic result",
	})
	nextBody, err := json.Marshal(next)
	require.NoError(t, err)
	nextRequest, err := in.TransformRequest(t.Context(), &httpclient.Request{Body: nextBody})
	require.NoError(t, err)
	nextWire, err := out.TransformRequest(t.Context(), nextRequest)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(nextWire.Body, &chat))
	require.Equal(t, "editor__patch", chat.Messages[2].ToolCalls[0].Function.Name)
	require.JSONEq(t, `{"input":"patch"}`, chat.Messages[2].ToolCalls[0].Function.Arguments)
	require.Equal(t, "call_patch", *chat.Messages[3].ToolCallID)

	// Native Responses forwarding must retain the Lite item, not add a second
	// top-level catalog or mutate the input request used by another candidate.
	native, err := responses.NewOutboundTransformer("https://example.invalid/v1", "synthetic")
	require.NoError(t, err)
	nativeWire, err := native.TransformRequest(t.Context(), r)
	require.NoError(t, err)
	var nativeBody map[string]any
	require.NoError(t, json.Unmarshal(nativeWire.Body, &nativeBody))
	tools, _ := nativeBody["tools"].([]any)
	require.Empty(t, tools)
	items := nativeBody["input"].([]any)
	require.Equal(t, "additional_tools", items[0].(map[string]any)["type"])
	require.Len(t, items[0].(map[string]any)["tools"], 2)
	require.Equal(t, "Use only workspace-relative paths.", items[0].(map[string]any)["tools"].([]any)[1].(map[string]any)["description"])

	next["tool_choice"] = map[string]any{"type": "custom", "namespace": "editor", "name": "patch"}
	namedBody, err := json.Marshal(next)
	require.NoError(t, err)
	namedRequest, err := in.TransformRequest(t.Context(), &httpclient.Request{Body: namedBody})
	require.NoError(t, err)
	namedChat, err := out.TransformRequest(t.Context(), namedRequest)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(namedChat.Body, &chat))
	require.Equal(t, "function", chat.ToolChoice.NamedToolChoice.Type)
	require.Equal(t, "editor__patch", chat.ToolChoice.NamedToolChoice.Function.Name)
	namedNative, err := native.TransformRequest(t.Context(), namedRequest)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(namedNative.Body, &nativeBody))
	require.Equal(t, next["tool_choice"], nativeBody["tool_choice"])
}

func TestChatBridgeNamespaceCustomSSEItemDone(t *testing.T) {
	in := responses.NewInboundTransformer()
	out, err := openai.NewOutboundTransformer("https://example.invalid/v1", "synthetic")
	require.NoError(t, err)
	r, err := in.TransformRequest(t.Context(), &httpclient.Request{Body: []byte(`{"model":"test","input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"editor","tools":[{"type":"custom","name":"patch"}]}]},{"role":"user","content":"patch"}]}`)})
	require.NoError(t, err)
	wire, err := out.TransformRequest(t.Context(), r)
	require.NoError(t, err)
	events := []*httpclient.StreamEvent{
		{Data: []byte(`{"id":"synthetic","model":"test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_patch","type":"function","function":{"name":"editor__patch","arguments":"{\"input\":\"patch\"}"}}]},"finish_reason":null}]}`)},
		{Data: []byte(`{"id":"synthetic","model":"test","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)},
		{Data: []byte(`[DONE]`)},
	}
	decoded, err := out.TransformStream(t.Context(), wire, streams.SliceStream(events))
	require.NoError(t, err)
	result, err := in.TransformStream(t.Context(), decoded)
	require.NoError(t, err)
	var done *responses.Item
	var replay []*httpclient.StreamEvent
	for result.Next() {
		event := result.Current()
		replay = append(replay, event)
		var value responses.StreamEvent
		require.NoError(t, json.Unmarshal(event.Data, &value))
		if value.Type == responses.StreamEventTypeOutputItemDone {
			done = value.Item
		}
	}
	require.NoError(t, result.Err())
	require.NotNil(t, done, "Codex dispatches output_item.done, not merely completed.output")
	require.Equal(t, "custom_tool_call", done.Type)
	require.Equal(t, "editor", done.Namespace)
	require.Equal(t, "patch", done.Name)
	require.Equal(t, "patch", *done.Input)

	native, err := responses.NewOutboundTransformer("https://example.invalid/v1", "synthetic")
	require.NoError(t, err)
	nativeStream, err := native.TransformStream(t.Context(), nil, streams.SliceStream(replay))
	require.NoError(t, err)
	roundTrip, err := in.TransformStream(t.Context(), nativeStream)
	require.NoError(t, err)
	done = nil
	for roundTrip.Next() {
		var value responses.StreamEvent
		event := roundTrip.Current()
		require.NoError(t, json.Unmarshal(event.Data, &value))
		if value.Type == responses.StreamEventTypeOutputItemDone {
			done = value.Item
		}
	}
	require.NoError(t, roundTrip.Err())
	require.NotNil(t, done)
	require.Equal(t, "editor", done.Namespace)
	require.Equal(t, "patch", *done.Input)
}
