package orchestrator

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	entchannel "github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	responsestransformer "github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func TestCodexAgentToolAliases_FullCatalogHistoryChoiceAndResponseRoundTrip(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.6-sol",
		"tools":[
			{"type":"namespace","name":"collaboration","description":"agents","tools":[
				{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true},"secret":{"type":"string","encrypted":true},"task_name":{"type":"string"}}}},
				{"type":"function","name":"send_message","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}},
				{"type":"function","name":"followup_task","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}},
				{"type":"function","name":"interrupt_agent","parameters":{"type":"object"}}
			]},
			{"type":"namespace","name":"gateway_collaboration","tools":[{"type":"function","name":"unrelated"}]},
			{"type":"function","name":"weather","parameters":{"type":"object"}}
		],
		"tool_choice":{"type":"function","namespace":"collaboration","name":"spawn_agent"},
		"input":[
			{"type":"function_call","id":"fc_old","call_id":"call_old","namespace":"collaboration","name":"send_message","arguments":"{\"message\":\"plain\"}","encrypted_function_args":[]},
			{"type":"function_call","id":"fc_encrypted","call_id":"call_encrypted","namespace":"collaboration","name":"followup_task","arguments":"ciphertext","encrypted_function_args":["message"]},
			{"type":"function_call_output","call_id":"call_old","output":"ok"},
			{"type":"reasoning","id":"rs_old","encrypted_content":"reasoning-ciphertext"}
		],
		"metadata":{"large_number":12345678901234567890}
	}`)
	original := append([]byte(nil), body...)

	aliased, binding, err := aliasCodexAgentTools(body)
	require.NoError(t, err)
	require.NotNil(t, binding)
	require.Equal(t, "gateway_collaboration_2", binding.namespace)
	require.Equal(t, original, body, "the inbound request bytes must not be mutated")
	require.JSONEq(t, `{
		"model":"gpt-5.6-sol",
		"tools":[
			{"type":"namespace","name":"collaboration","description":"agents","tools":[{"type":"function","name":"interrupt_agent","parameters":{"type":"object"}}]},
			{"type":"namespace","name":"gateway_collaboration_2","description":"agents","tools":[
				{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string"},"secret":{"type":"string","encrypted":true},"task_name":{"type":"string"}}}},
				{"type":"function","name":"send_message","parameters":{"type":"object","properties":{"message":{"type":"string"}}}},
				{"type":"function","name":"followup_task","parameters":{"type":"object","properties":{"message":{"type":"string"}}}}
			]},
			{"type":"namespace","name":"gateway_collaboration","tools":[{"type":"function","name":"unrelated"}]},
			{"type":"function","name":"weather","parameters":{"type":"object"}}
		],
		"tool_choice":{"type":"function","namespace":"gateway_collaboration_2","name":"spawn_agent"},
		"input":[
			{"type":"function_call","id":"fc_old","call_id":"call_old","namespace":"gateway_collaboration_2","name":"send_message","arguments":"{\"message\":\"plain\"}"},
			{"type":"function_call","id":"fc_encrypted","call_id":"call_encrypted","namespace":"collaboration","name":"followup_task","arguments":"ciphertext","encrypted_function_args":["message"]},
			{"type":"function_call_output","call_id":"call_old","output":"ok"},
			{"type":"reasoning","id":"rs_old","encrypted_content":"reasoning-ciphertext"}
		],
		"metadata":{"large_number":12345678901234567890}
	}`, string(aliased))

	providerResponse := []byte(`{
		"id":"resp_test",
		"output":[
			{"type":"function_call","id":"fc_new","call_id":"call_new","namespace":"gateway_collaboration_2","name":"spawn_agent","arguments":"{\"message\":\"dispatch\"}"},
			{"type":"function_call","id":"fc_cipher","call_id":"call_cipher","namespace":"gateway_collaboration_2","name":"send_message","arguments":"ciphertext","encrypted_function_args":["message"]},
			{"type":"function_call","id":"fc_other","call_id":"call_other","namespace":"other","name":"spawn_agent","arguments":"{}"},
			{"type":"reasoning","id":"rs_new","encrypted_content":"response-ciphertext"}
		],
		"tools":[
			{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"interrupt_agent"}]},
			{"type":"namespace","name":"gateway_collaboration_2","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string"}}}}]}
		],
		"tool_choice":{"type":"function","namespace":"gateway_collaboration_2","name":"spawn_agent"}
	}`)
	restored, changed := restoreCodexAgentTools(providerResponse, binding)
	require.True(t, changed)
	require.JSONEq(t, `{
		"id":"resp_test",
		"output":[
			{"type":"function_call","id":"fc_new","call_id":"call_new","namespace":"collaboration","name":"spawn_agent","arguments":"{\"message\":\"dispatch\"}","encrypted_function_args":[]},
			{"type":"function_call","id":"fc_cipher","call_id":"call_cipher","namespace":"collaboration","name":"send_message","arguments":"ciphertext","encrypted_function_args":["message"]},
			{"type":"function_call","id":"fc_other","call_id":"call_other","namespace":"other","name":"spawn_agent","arguments":"{}"},
			{"type":"reasoning","id":"rs_new","encrypted_content":"response-ciphertext"}
		],
		"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"interrupt_agent"},{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}}]}],
		"tool_choice":{"type":"function","namespace":"collaboration","name":"spawn_agent"}
	}`, string(restored))
}

func TestCodexAgentToolAliases_LiteAdditionalToolsAndNoMatchBytes(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}}]}]},{"role":"user","content":"dispatch"}]}`)

	aliased, binding, err := aliasCodexAgentTools(body)
	require.NoError(t, err)
	require.NotNil(t, binding)
	require.JSONEq(t, `{"model":"gpt-5.6-sol","input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"gateway_collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string"}}}}]}]},{"role":"user","content":"dispatch"}]}`, string(aliased))

	noMatch := []byte(" {\n  \"tools\": [{\"type\":\"function\",\"name\":\"weather\"}]\n}\n")
	unchanged, noBinding, err := aliasCodexAgentTools(noMatch)
	require.NoError(t, err)
	require.Nil(t, noBinding)
	require.Equal(t, noMatch, unchanged)
}

func TestCodexAgentToolAliases_MixedCatalogBindsOnlyMovedTools(t *testing.T) {
	body := []byte(`{
		"tools":[{"type":"namespace","name":"collaboration","tools":[
			{"type":"function","name":"spawn_agent","parameters":{"properties":{"message":{"type":"string","encrypted":true}}}},
			{"type":"function","name":"send_message","parameters":{"properties":{"message":{"type":"string","encrypted":false}}}}
		]}],
		"tool_choice":{"type":"allowed_tools","tools":[
			{"type":"function","namespace":"collaboration","name":"spawn_agent"},
			{"type":"function","namespace":"collaboration","name":"send_message"}
		]},
		"input":[
			{"type":"function_call","namespace":"collaboration","name":"spawn_agent","arguments":"{}","encrypted_function_args":[]},
			{"type":"function_call","namespace":"collaboration","name":"send_message","arguments":"{}","encrypted_function_args":[]}
		]
	}`)

	aliased, binding, err := aliasCodexAgentTools(body)
	require.NoError(t, err)
	require.NotNil(t, binding)
	require.True(t, binding.hasTool("spawn_agent"))
	require.False(t, binding.hasTool("send_message"))
	require.JSONEq(t, `{
		"tools":[
			{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"send_message","parameters":{"properties":{"message":{"type":"string","encrypted":false}}}}]},
			{"type":"namespace","name":"gateway_collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"properties":{"message":{"type":"string"}}}}]}
		],
		"tool_choice":{"type":"allowed_tools","tools":[
			{"type":"function","namespace":"gateway_collaboration","name":"spawn_agent"},
			{"type":"function","namespace":"collaboration","name":"send_message"}
		]},
		"input":[
			{"type":"function_call","namespace":"gateway_collaboration","name":"spawn_agent","arguments":"{}"},
			{"type":"function_call","namespace":"collaboration","name":"send_message","arguments":"{}","encrypted_function_args":[]}
		]
	}`, string(aliased))

	restored, changed := restoreCodexAgentTools([]byte(`{"output":[
		{"type":"function_call","namespace":"gateway_collaboration","name":"spawn_agent","arguments":"{}"},
		{"type":"function_call","namespace":"gateway_collaboration","name":"send_message","arguments":"{}"}
	]}`), binding)
	require.True(t, changed)
	require.JSONEq(t, `{"output":[
		{"type":"function_call","namespace":"collaboration","name":"spawn_agent","arguments":"{}","encrypted_function_args":[]},
		{"type":"function_call","namespace":"gateway_collaboration","name":"send_message","arguments":"{}"}
	]}`, string(restored))
}

func TestCodexAgentToolAliases_FullPassThroughRequestAndCapturedResponse(t *testing.T) {
	rawBody := []byte(`{"model":"client-model","tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true}}}}]}]}`)
	channel := &biz.Channel{Channel: &ent.Channel{
		ID:      1,
		Name:    "upstream-axonhub",
		Type:    entchannel.TypeAxonhub,
		BaseURL: "https://upstream.example/base",
		Settings: &objects.ChannelSettings{
			FullPassThrough:  true,
			PassThroughBody:  lo.ToPtr(true),
			TransformOptions: objects.TransformOptions{CodexAgentToolAliases: true},
		},
	}}
	state := &PersistenceState{
		CurrentCandidate: &ChannelModelsCandidate{Channel: channel},
		LlmRequest: &llm.Request{
			APIFormat: llm.APIFormatOpenAIResponse,
			Model:     "mapped-model",
			RawRequest: &httpclient.Request{
				Method:    http.MethodPost,
				Path:      "/v1/responses",
				APIFormat: string(llm.APIFormatOpenAIResponse),
				Body:      rawBody,
				Headers:   http.Header{"Content-Type": []string{"application/json"}},
			},
		},
	}
	outbound := &PersistentOutboundTransformer{state: state}
	aliases := newCodexAgentToolAliasState(outbound)

	request, err := applyAxonHubFullPassThroughRequest(outbound).OnOutboundRawRequest(t.Context(), &httpclient.Request{
		Method:    http.MethodPost,
		URL:       "https://placeholder.example/v1/responses",
		APIFormat: string(llm.APIFormatOpenAIResponse),
		Body:      []byte(`{"model":"mapped-model"}`),
	})
	require.NoError(t, err)
	require.Equal(t, "https://upstream.example/base/v1/responses", request.URL)
	require.Contains(t, string(request.Body), `"model":"mapped-model"`)

	request, err = applyPassThroughRequestBody(outbound, nil).OnOutboundRawRequest(t.Context(), request)
	require.NoError(t, err)
	request, err = aliases.requestMiddleware().OnOutboundRawRequest(t.Context(), request)
	require.NoError(t, err)
	require.Contains(t, string(request.Body), `"name":"gateway_collaboration"`)
	require.NotContains(t, string(request.Body), `"encrypted":true`)
	require.Contains(t, string(rawBody), `"name":"collaboration"`)
	require.Contains(t, string(rawBody), `"encrypted":true`)

	provider := &httpclient.Response{StatusCode: http.StatusOK, Body: []byte(`{"output":[{"type":"function_call","id":"fc_1","call_id":"call_1","namespace":"gateway_collaboration","name":"spawn_agent","arguments":"{}"}]}`)}
	_, err = captureRawProviderResponse(outbound, nil).OnOutboundRawResponse(t.Context(), provider)
	require.NoError(t, err)
	selected, err := applyPassThroughResponse(outbound, nil).OnInboundRawResponse(t.Context(), &httpclient.Response{StatusCode: http.StatusOK, Body: []byte(`{"output":[]}`)})
	require.NoError(t, err)
	restored, err := aliases.responseMiddleware().OnInboundRawResponse(t.Context(), selected)
	require.NoError(t, err)
	require.JSONEq(t, `{"output":[{"type":"function_call","id":"fc_1","call_id":"call_1","namespace":"gateway_collaboration","name":"spawn_agent","arguments":"{}"}]}`, string(state.RawProviderResponse.Body))
	require.JSONEq(t, `{"output":[{"type":"function_call","id":"fc_1","call_id":"call_1","namespace":"collaboration","name":"spawn_agent","arguments":"{}","encrypted_function_args":[]}]}`, string(restored.Body))
}

func TestCodexAgentToolAliases_StreamRestoresRelevantEventsAndPreservesError(t *testing.T) {
	binding := &codexAgentToolAliasBinding{namespace: "gateway_collaboration", tools: map[string]struct{}{"send_message": {}}}
	wantErr := errors.New("upstream stream failed")
	upstream := &errorAfterEventsStream{items: []*httpclient.StreamEvent{
		{Type: "response.output_item.added", LastEventID: "1", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","namespace":"gateway_collaboration","name":"send_message","arguments":""}}`)},
		{Type: "response.function_call_arguments.done", LastEventID: "2", Data: []byte(`{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"namespace":"gateway_collaboration","name":"send_message","arguments":"{\"message\":\"hi\"}"}`)},
		{Type: "response.completed", LastEventID: "3", Data: []byte(`{"type":"response.completed","response":{"id":"resp_1","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","namespace":"gateway_collaboration","name":"send_message","arguments":"{\"message\":\"hi\"}"}]}}`)},
	}, err: wantErr}
	stream := &codexAgentToolAliasStream{Stream: upstream, binding: binding}

	var events []*httpclient.StreamEvent
	for stream.Next() {
		events = append(events, stream.Current())
	}
	require.ErrorIs(t, stream.Err(), wantErr)
	require.Len(t, events, 3)
	require.Equal(t, "1", events[0].LastEventID)
	require.JSONEq(t, `{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","namespace":"collaboration","name":"send_message","arguments":"","encrypted_function_args":[]}}`, string(events[0].Data))
	require.JSONEq(t, `{"type":"response.function_call_arguments.done","item_id":"fc_1","output_index":0,"namespace":"collaboration","name":"send_message","arguments":"{\"message\":\"hi\"}"}`, string(events[1].Data))
	require.JSONEq(t, `{"type":"response.completed","response":{"id":"resp_1","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","namespace":"collaboration","name":"send_message","arguments":"{\"message\":\"hi\"}","encrypted_function_args":[]}]}}`, string(events[2].Data))
}

func TestCodexAgentToolAliases_DisabledMiddlewareLeavesBytesAndBindingEmpty(t *testing.T) {
	body := []byte(`{"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"properties":{"message":{"type":"string","encrypted":true}}}}]}]}`)
	channel := &biz.Channel{Channel: &ent.Channel{Settings: &objects.ChannelSettings{}}}
	outbound := &PersistentOutboundTransformer{state: &PersistenceState{CurrentCandidate: &ChannelModelsCandidate{Channel: channel}}}
	aliases := newCodexAgentToolAliasState(outbound)
	request := &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIResponse), Body: body}

	result, err := aliases.requestMiddleware().OnOutboundRawRequest(t.Context(), request)
	require.NoError(t, err)
	require.Same(t, request, result)
	require.Equal(t, body, result.Body)
	require.Nil(t, aliases.getBinding())
}

func TestCodexAgentToolAliases_RetryBindingsDoNotLeakIntoEstablishedStream(t *testing.T) {
	channel := &biz.Channel{Channel: &ent.Channel{Settings: &objects.ChannelSettings{
		TransformOptions: objects.TransformOptions{CodexAgentToolAliases: true},
	}}}
	outbound := &PersistentOutboundTransformer{state: &PersistenceState{CurrentCandidate: &ChannelModelsCandidate{Channel: channel}}}
	aliases := newCodexAgentToolAliasState(outbound)
	requestMiddleware := aliases.requestMiddleware()

	firstBody := []byte(`{"tools":[{"type":"namespace","name":"gateway_collaboration","tools":[]},{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"properties":{"message":{"type":"string","encrypted":true}}}}]}]}`)
	_, err := requestMiddleware.OnOutboundRawRequest(t.Context(), &httpclient.Request{
		APIFormat: string(llm.APIFormatOpenAIResponse), Body: firstBody,
	})
	require.NoError(t, err)
	firstSource := &errorAfterEventsStream{items: []*httpclient.StreamEvent{{
		Type: "response.output_item.done",
		Data: []byte(`{"type":"response.output_item.done","item":{"type":"function_call","namespace":"gateway_collaboration_2","name":"spawn_agent","arguments":"{}"}}`),
	}}}
	firstStream, err := aliases.streamMiddleware().OnInboundRawStream(t.Context(), firstSource)
	require.NoError(t, err)

	secondBody := []byte(`{"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"send_message","parameters":{"properties":{"message":{"type":"string","encrypted":true}}}}]}]}`)
	_, err = requestMiddleware.OnOutboundRawRequest(t.Context(), &httpclient.Request{
		APIFormat: string(llm.APIFormatOpenAIResponse), Body: secondBody,
	})
	require.NoError(t, err)
	require.Equal(t, "gateway_collaboration", aliases.getBinding().namespace)

	require.True(t, firstStream.Next())
	require.JSONEq(t, `{"type":"response.output_item.done","item":{"type":"function_call","namespace":"collaboration","name":"spawn_agent","arguments":"{}","encrypted_function_args":[]}}`, string(firstStream.Current().Data))
	require.Equal(t, firstBody, []byte(`{"tools":[{"type":"namespace","name":"gateway_collaboration","tools":[]},{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"spawn_agent","parameters":{"properties":{"message":{"type":"string","encrypted":true}}}}]}]}`))
	require.Equal(t, secondBody, []byte(`{"tools":[{"type":"namespace","name":"collaboration","tools":[{"type":"function","name":"send_message","parameters":{"properties":{"message":{"type":"string","encrypted":true}}}}]}]}`))

	_, err = requestMiddleware.OnOutboundRawRequest(t.Context(), &httpclient.Request{
		APIFormat: string(llm.APIFormatOpenAIResponse), Body: []byte(`not-json`),
	})
	require.Error(t, err)
	require.Nil(t, aliases.getBinding())
}

type codexAgentToolAliasPipelineArtifacts struct {
	ClientEvents          []*httpclient.StreamEvent
	ProviderRequestBody   []byte
	ExecutionResponseBody []byte
	ExecutionChunks       []objects.JSONRawMessage
}

func TestCodexAgentToolAliases_FullPipelineTransformsClientButStoresProviderStream(t *testing.T) {
	runCodexAgentToolAliasFullPipeline(t)
}

func runCodexAgentToolAliasFullPipeline(t *testing.T) codexAgentToolAliasPipelineArtifacts {
	t.Helper()
	ctx := authz.WithTestBypass(t.Context())
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })
	ctx = ent.NewContext(ctx, client)
	project := createTestProject(t, ctx, client)
	ctx = contexts.WithProjectID(ctx, project.ID)

	settings := &objects.ChannelSettings{
		FullPassThrough:            true,
		PassThroughBody:            lo.ToPtr(false),
		StoreExecutionResponseBody: lo.ToPtr(true),
		StoreExecutionStreamChunks: lo.ToPtr(true),
		TransformOptions: objects.TransformOptions{
			CodexAgentToolAliases: true,
		},
	}
	channelRow, err := client.Channel.Create().
		SetType(entchannel.TypeAxonhub).
		SetName("Codex alias upstream").
		SetBaseURL("https://upstream.example").
		SetCredentials(objects.ChannelCredentials{APIKey: "synthetic"}).
		SetSupportedModels([]string{"gpt-5.6-sol"}).
		SetDefaultTestModel("gpt-5.6-sol").
		SetSettings(settings).
		Save(ctx)
	require.NoError(t, err)

	outbound, err := responsestransformer.NewOutboundTransformer(channelRow.BaseURL, "synthetic")
	require.NoError(t, err)
	bizChannel := &biz.Channel{Channel: channelRow, Outbound: outbound}
	selector := &staticChannelSelector{candidates: channelsToTestCandidates([]*biz.Channel{bizChannel}, "gpt-5.6-sol")}
	require.Len(t, selector.candidates, 1)

	providerEvents := []*httpclient.StreamEvent{
		{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_pipeline","object":"response","created_at":1700000000,"model":"gpt-5.6-sol","status":"in_progress","output":[]}}`)},
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_pipeline","type":"function_call","status":"in_progress","arguments":"","call_id":"call_pipeline","name":"spawn_agent","namespace":"gateway_collaboration"}}`)},
		{Type: "response.function_call_arguments.done", Data: []byte(`{"type":"response.function_call_arguments.done","item_id":"fc_pipeline","output_index":0,"arguments":"{\"task_name\":\"consumer_probe\",\"agent_type\":\"explore\",\"message\":\"BRIEF CONSUMER_PLAINTEXT_3921\"}"}`)},
		{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"fc_pipeline","type":"function_call","status":"completed","arguments":"{\"task_name\":\"consumer_probe\",\"agent_type\":\"explore\",\"message\":\"BRIEF CONSUMER_PLAINTEXT_3921\"}","call_id":"call_pipeline","name":"spawn_agent","namespace":"gateway_collaboration"}}`)},
		{Type: "response.output_item.added", Data: []byte(`{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_encrypted_pipeline","type":"function_call","status":"in_progress","arguments":"","call_id":"call_encrypted_pipeline","name":"send_message","namespace":"gateway_collaboration"}}`)},
		{Type: "response.function_call_arguments.done", Data: []byte(`{"type":"response.function_call_arguments.done","item_id":"fc_encrypted_pipeline","output_index":1,"arguments":"{}"}`)},
		{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":1,"item":{"id":"fc_encrypted_pipeline","type":"function_call","status":"completed","arguments":"{}","call_id":"call_encrypted_pipeline","name":"send_message","namespace":"gateway_collaboration","encrypted_function_args":["message"]}}`)},
		{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_pipeline","object":"response","created_at":1700000000,"model":"gpt-5.6-sol","status":"completed","output":[{"id":"fc_pipeline","type":"function_call","status":"completed","arguments":"{\"task_name\":\"consumer_probe\",\"agent_type\":\"explore\",\"message\":\"BRIEF CONSUMER_PLAINTEXT_3921\"}","call_id":"call_pipeline","name":"spawn_agent","namespace":"gateway_collaboration"},{"id":"fc_encrypted_pipeline","type":"function_call","status":"completed","arguments":"{}","call_id":"call_encrypted_pipeline","name":"send_message","namespace":"gateway_collaboration","encrypted_function_args":["message"]}]}}`)},
	}
	executor := &mockExecutor{streamEvents: providerEvents}
	orchestrator := newTestOrchestrator(t, selector, client, executor)
	orchestrator.Inbound = responsestransformer.NewInboundTransformer()

	requestBody := []byte(`{
		"model":"gpt-5.6-sol","stream":true,"store":false,"input":"Dispatch",
		"tools":[{"type":"namespace","name":"collaboration","tools":[
			{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true},"task_name":{"type":"string"}},"required":["message","task_name"]}},
			{"type":"function","name":"send_message","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true},"target":{"type":"string"}},"required":["message","target"]}}
		]}]
	}`)
	result, err := orchestrator.Process(ctx, &httpclient.Request{
		Method: http.MethodPost,
		URL:    "/v1/responses",
		Path:   "/v1/responses",
		Headers: http.Header{
			"Content-Type": []string{"application/json"},
		},
		Body: requestBody,
	})
	require.NoError(t, err)
	require.NotNil(t, result.ChatCompletionStream)
	require.Contains(t, string(executor.lastRequest.Body), `"name":"gateway_collaboration"`)
	require.NotContains(t, string(executor.lastRequest.Body), `"encrypted":true`)
	require.Contains(t, string(requestBody), `"name":"collaboration"`)
	require.Contains(t, string(requestBody), `"encrypted":true`)

	clientDone := make(map[string]map[string]any)
	clientCompleted := make(map[string]map[string]any)
	clientEvents := make([]*httpclient.StreamEvent, 0, len(providerEvents))
	for result.ChatCompletionStream.Next() {
		event := result.ChatCompletionStream.Current()
		clientEvents = append(clientEvents, &httpclient.StreamEvent{
			LastEventID: event.LastEventID,
			Type:        event.Type,
			Data:        append([]byte(nil), event.Data...),
			Size:        event.Size,
		})
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(event.Data, &decoded))
		if decoded["type"] == "response.output_item.done" {
			item := decoded["item"].(map[string]any)
			clientDone[item["name"].(string)] = item
		}
		if decoded["type"] == "response.completed" {
			response := decoded["response"].(map[string]any)
			for _, rawItem := range response["output"].([]any) {
				item := rawItem.(map[string]any)
				if item["type"] == "function_call" {
					clientCompleted[item["name"].(string)] = item
				}
			}
		}
	}
	require.NoError(t, result.ChatCompletionStream.Err())
	require.NoError(t, result.ChatCompletionStream.Close())
	clientItem := clientDone["spawn_agent"]
	require.NotNil(t, clientItem)
	require.Equal(t, "collaboration", clientItem["namespace"])
	require.Equal(t, "spawn_agent", clientItem["name"])
	require.Equal(t, []any{}, clientItem["encrypted_function_args"])
	require.JSONEq(t, `{"task_name":"consumer_probe","agent_type":"explore","message":"BRIEF CONSUMER_PLAINTEXT_3921"}`, clientItem["arguments"].(string))
	encryptedItem := clientDone["send_message"]
	require.NotNil(t, encryptedItem)
	require.Equal(t, "collaboration", encryptedItem["namespace"])
	require.Equal(t, []any{"message"}, encryptedItem["encrypted_function_args"])
	require.Equal(t, `{}`, encryptedItem["arguments"])
	completedPlaintext := clientCompleted["spawn_agent"]
	require.NotNil(t, completedPlaintext)
	require.Equal(t, "collaboration", completedPlaintext["namespace"])
	require.Equal(t, []any{}, completedPlaintext["encrypted_function_args"])
	completedEncrypted := clientCompleted["send_message"]
	require.NotNil(t, completedEncrypted)
	require.Equal(t, "collaboration", completedEncrypted["namespace"])
	require.Equal(t, []any{"message"}, completedEncrypted["encrypted_function_args"])

	var execution *ent.RequestExecution
	require.Eventually(t, func() bool {
		var queryErr error
		execution, queryErr = client.RequestExecution.Query().Only(ctx)
		return queryErr == nil && len(execution.ResponseBody) > 0 && len(execution.ResponseChunks) > 0
	}, 3*time.Second, 20*time.Millisecond)
	require.Contains(t, string(execution.ResponseBody), `"namespace":"gateway_collaboration"`)
	require.NotContains(t, string(execution.ResponseBody), `"encrypted_function_args":[]`)
	require.Contains(t, string(execution.ResponseBody), `"encrypted_function_args":["message"]`)
	storedChunks, err := json.Marshal(execution.ResponseChunks)
	require.NoError(t, err)
	require.Contains(t, string(storedChunks), `gateway_collaboration`)
	require.Contains(t, string(storedChunks), `encrypted_function_args`)

	return codexAgentToolAliasPipelineArtifacts{
		ClientEvents:          clientEvents,
		ProviderRequestBody:   append([]byte(nil), executor.lastRequest.Body...),
		ExecutionResponseBody: append([]byte(nil), execution.ResponseBody...),
		ExecutionChunks:       append([]objects.JSONRawMessage(nil), execution.ResponseChunks...),
	}
}
