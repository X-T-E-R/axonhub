package orchestrator

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/model"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/auth"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/pipeline/stream"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/antigravity"
	"github.com/looplj/axonhub/llm/transformer/gemini"
	geminioai "github.com/looplj/axonhub/llm/transformer/gemini/openai"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func TestApplyModelReasoningBounds(t *testing.T) {
	t.Parallel()

	budget := int64(1234)
	tests := []struct {
		name         string
		settings     *objects.ModelSettings
		effort       string
		budget       *int64
		wantEffort   string
		wantBudget   *int64
		wantEnforced bool
		wantChanged  bool
	}{
		{name: "both bounds unset", settings: &objects.ModelSettings{}, effort: "ultra", budget: &budget, wantEffort: "ultra", wantBudget: &budget},
		{name: "lower bound", settings: &objects.ModelSettings{MinReasoningEffort: "medium"}, effort: "low", budget: &budget, wantEffort: "medium", wantEnforced: true, wantChanged: true},
		{name: "upper bound", settings: &objects.ModelSettings{MaxReasoningEffort: "high"}, effort: "ultra", budget: &budget, wantEffort: "high", wantEnforced: true, wantChanged: true},
		{name: "in range", settings: &objects.ModelSettings{MinReasoningEffort: "low", MaxReasoningEffort: "ultra"}, effort: "persistent", budget: &budget, wantEffort: "ultra", wantEnforced: true, wantChanged: true},
		{name: "persistent in range", settings: &objects.ModelSettings{MinReasoningEffort: "none", MaxReasoningEffort: "persistent"}, effort: "persistent", budget: &budget, wantEffort: "persistent", wantBudget: &budget, wantEnforced: true},
		{name: "missing uses minimum", settings: &objects.ModelSettings{MinReasoningEffort: "minimal", MaxReasoningEffort: "high"}, budget: &budget, wantEffort: "minimal", wantEnforced: true, wantChanged: true},
		{name: "missing uses maximum when only maximum set", settings: &objects.ModelSettings{MaxReasoningEffort: "xhigh"}, budget: &budget, wantEffort: "xhigh", wantEnforced: true, wantChanged: true},
		{name: "custom effort preserved", settings: &objects.ModelSettings{MinReasoningEffort: "low", MaxReasoningEffort: "high"}, effort: "provider-custom", budget: &budget, wantEffort: "provider-custom", wantBudget: &budget},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &llm.Request{Model: "logical-model", ReasoningEffort: tt.effort, ReasoningBudget: tt.budget}
			enforced, changed := applyModelReasoningBounds(context.Background(), req, tt.settings)
			require.Equal(t, tt.wantEnforced, enforced)
			require.Equal(t, tt.wantChanged, changed)
			require.Equal(t, tt.wantEffort, req.ReasoningEffort)
			require.Equal(t, tt.wantBudget, req.ReasoningBudget)
		})
	}
}

func TestApplyModelReasoningBoundsClearsHigherPriorityReasoningOverridesWhenChanged(t *testing.T) {
	t.Parallel()

	req := &llm.Request{
		Model:           "logical-model",
		ReasoningEffort: "ultra",
		ReasoningBudget: new(int64),
		ExtraBody:       []byte(`{"google":{"thinking_config":{"thinking_level":"ultra","thinking_budget":999,"include_thoughts":true}},"other":"kept"}`),
		TransformerMetadata: map[string]any{
			anthropic.TransformerMetadataKeyThinkingType:       "adaptive",
			anthropic.TransformerMetadataKeyOutputConfigEffort: "ultra",
			anthropic.TransformerMetadataKeyThinkingDisplay:    "summarized",
		},
	}

	enforced, changed := applyModelReasoningBounds(context.Background(), req, &objects.ModelSettings{MaxReasoningEffort: "high"})

	require.True(t, enforced)
	require.True(t, changed)
	require.Equal(t, "high", req.ReasoningEffort)
	require.Nil(t, req.ReasoningBudget)
	require.JSONEq(t, `{"google":{},"other":"kept"}`, string(req.ExtraBody))
	require.NotContains(t, req.TransformerMetadata, anthropic.TransformerMetadataKeyThinkingType)
	require.NotContains(t, req.TransformerMetadata, anthropic.TransformerMetadataKeyOutputConfigEffort)
	require.Equal(t, "summarized", req.TransformerMetadata[anthropic.TransformerMetadataKeyThinkingDisplay])
}

func TestBoundedReasoningWireReconciliation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		apiFormat   llm.APIFormat
		changed     bool
		generated   string
		overridden  string
		assertFinal func(*testing.T, []byte)
	}{
		{
			name:       "chat restores mapped effort and removes changed budget",
			apiFormat:  llm.APIFormatOpenAIChatCompletion,
			changed:    true,
			generated:  `{"model":"upstream","reasoning_effort":"medium"}`,
			overridden: `{"model":"upstream","reasoning_effort":"ultra","reasoning_budget":999}`,
			assertFinal: func(t *testing.T, body []byte) {
				require.Equal(t, "medium", gjson.GetBytes(body, "reasoning_effort").String())
				require.False(t, gjson.GetBytes(body, "reasoning_budget").Exists())
			},
		},
		{
			name:       "responses restores in-range effort without changing explicit budget",
			apiFormat:  llm.APIFormatOpenAIResponse,
			generated:  `{"model":"upstream","reasoning":{"effort":"medium"}}`,
			overridden: `{"model":"upstream","reasoning":{"effort":"ultra","max_tokens":999}}`,
			assertFinal: func(t *testing.T, body []byte) {
				require.Equal(t, "medium", gjson.GetBytes(body, "reasoning.effort").String())
				require.Equal(t, int64(999), gjson.GetBytes(body, "reasoning.max_tokens").Int())
			},
		},
		{
			name:       "responses removes budget when level changed",
			apiFormat:  llm.APIFormatOpenAIResponse,
			changed:    true,
			generated:  `{"model":"upstream","reasoning":{"effort":"low"}}`,
			overridden: `{"model":"upstream","reasoning":{"effort":"ultra","max_tokens":999}}`,
			assertFinal: func(t *testing.T, body []byte) {
				require.Equal(t, "low", gjson.GetBytes(body, "reasoning.effort").String())
				require.False(t, gjson.GetBytes(body, "reasoning.max_tokens").Exists())
			},
		},
		{
			name:       "anthropic restores converted thinking fragment",
			apiFormat:  llm.APIFormatAnthropicMessage,
			changed:    true,
			generated:  `{"model":"upstream","thinking":{"type":"enabled","budget_tokens":4096,"display":"generated"}}`,
			overridden: `{"model":"upstream","thinking":{"type":"enabled","budget_tokens":99999,"display":"kept"},"output_config":{"effort":"max","other":"kept"}}`,
			assertFinal: func(t *testing.T, body []byte) {
				require.Equal(t, int64(4096), gjson.GetBytes(body, "thinking.budget_tokens").Int())
				require.False(t, gjson.GetBytes(body, "output_config.effort").Exists())
				require.Equal(t, "kept", gjson.GetBytes(body, "thinking.display").String())
				require.Equal(t, "kept", gjson.GetBytes(body, "output_config.other").String())
			},
		},
		{
			name:       "anthropic in-range effort protects converted budget only",
			apiFormat:  llm.APIFormatAnthropicMessage,
			generated:  `{"model":"upstream","thinking":{"type":"enabled","budget_tokens":5000,"display":"generated"}}`,
			overridden: `{"model":"upstream","thinking":{"type":"enabled","budget_tokens":99999,"display":"kept"},"output_config":{"other":"kept"}}`,
			assertFinal: func(t *testing.T, body []byte) {
				require.Equal(t, int64(5000), gjson.GetBytes(body, "thinking.budget_tokens").Int())
				require.Equal(t, "kept", gjson.GetBytes(body, "thinking.display").String())
				require.Equal(t, "kept", gjson.GetBytes(body, "output_config.other").String())
			},
		},
		{
			name:       "gemini restores converted thinking config",
			apiFormat:  llm.APIFormatGeminiContents,
			changed:    true,
			generated:  `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"high","includeThoughts":true}}}`,
			overridden: `{"generationConfig":{"thinkingConfig":{"thinkingLevel":"low","thinkingBudget":1,"includeThoughts":false}}}`,
			assertFinal: func(t *testing.T, body []byte) {
				require.Equal(t, "high", gjson.GetBytes(body, "generationConfig.thinkingConfig.thinkingLevel").String())
				require.False(t, gjson.GetBytes(body, "generationConfig.thinkingConfig.thinkingBudget").Exists())
				require.True(t, gjson.GetBytes(body, "generationConfig.thinkingConfig.includeThoughts").Exists())
				require.False(t, gjson.GetBytes(body, "generationConfig.thinkingConfig.includeThoughts").Bool())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := &PersistenceState{ReasoningBoundsEnforced: true, ReasoningBoundsChanged: tt.changed}
			outbound := &PersistentOutboundTransformer{state: state}
			capture := captureBoundedReasoningWire(outbound)
			reconcile := reconcileBoundedReasoningWire(outbound)
			generated := &httpclient.Request{APIFormat: string(tt.apiFormat), Body: []byte(tt.generated)}

			_, err := capture.OnOutboundRawRequest(context.Background(), generated)
			require.NoError(t, err)

			finalRequest := &httpclient.Request{APIFormat: string(tt.apiFormat), Body: []byte(tt.overridden)}
			finalRequest, err = reconcile.OnOutboundRawRequest(context.Background(), finalRequest)
			require.NoError(t, err)
			tt.assertFinal(t, finalRequest.Body)
			require.Equal(t, tt.generated, string(generated.Body), "capturing must not mutate the generated provider request")
		})
	}
}

func TestReasoningBoundsFullPipelinePreservesFinalProviderWire(t *testing.T) {
	tests := []struct {
		name              string
		channelType       channel.Type
		apiFormat         llm.APIFormat
		requestPath       string
		requestBody       string
		responseBody      string
		settings          *objects.ModelSettings
		overrideOps       []objects.OverrideOperation
		fullPassThrough   bool
		buildTransformers func(t *testing.T) (transformer.Inbound, transformer.Outbound)
		assertProvider    func(*testing.T, []byte)
	}{
		{
			name:         "chat pass-through mapping and override",
			channelType:  channel.TypeOpenai,
			apiFormat:    llm.APIFormatOpenAIChatCompletion,
			requestPath:  "/v1/chat/completions",
			requestBody:  `{"model":"logical-model","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"low","reasoning_budget":999,"custom_field":"kept"}`,
			responseBody: string(buildMockOpenAIResponse("chatcmpl-bounds", "upstream-model", "ok", 1, 1)),
			settings:     &objects.ModelSettings{MinReasoningEffort: "medium", MaxReasoningEffort: "high"},
			overrideOps:  []objects.OverrideOperation{{Op: objects.OverrideOpSet, Path: "reasoning_effort", Value: "ultra"}},
			buildTransformers: func(t *testing.T) (transformer.Inbound, transformer.Outbound) {
				outbound, err := openai.NewOutboundTransformerWithConfig(&openai.Config{
					PlatformType:           openai.PlatformOpenAI,
					BaseURL:                "https://api.example.com/v1",
					APIKeyProvider:         auth.NewStaticKeyProvider("test-key"),
					ReasoningEffortMapping: []llm.ReasoningEffortMapping{{From: "medium", To: "high"}},
				})
				require.NoError(t, err)
				return openai.NewInboundTransformer(), outbound
			},
			assertProvider: func(t *testing.T, body []byte) {
				require.Equal(t, "upstream-model", gjson.GetBytes(body, "model").String())
				require.Equal(t, "high", gjson.GetBytes(body, "reasoning_effort").String())
				require.False(t, gjson.GetBytes(body, "reasoning_budget").Exists())
				require.Equal(t, "kept", gjson.GetBytes(body, "custom_field").String())
			},
		},
		{
			name:            "responses pass-through and override",
			channelType:     channel.TypeAxonhub,
			apiFormat:       llm.APIFormatOpenAIResponse,
			requestPath:     "/v1/responses",
			requestBody:     `{"model":"logical-model","input":"hello","reasoning":{"effort":"ultra","max_tokens":999},"metadata":{"kept":"yes"}}`,
			responseBody:    `{"id":"resp-bounds","object":"response","created_at":1700000000,"status":"completed","model":"upstream-model","output":[]}`,
			settings:        &objects.ModelSettings{MaxReasoningEffort: "high"},
			fullPassThrough: true,
			overrideOps: []objects.OverrideOperation{
				{Op: objects.OverrideOpSet, Path: "reasoning.effort", Value: "low"},
				{Op: objects.OverrideOpSet, Path: "reasoning.max_tokens", Value: "1"},
			},
			buildTransformers: func(t *testing.T) (transformer.Inbound, transformer.Outbound) {
				outbound, err := responses.NewOutboundTransformer("https://api.example.com/v1", "test-key")
				require.NoError(t, err)
				return responses.NewInboundTransformer(), outbound
			},
			assertProvider: func(t *testing.T, body []byte) {
				require.Equal(t, "upstream-model", gjson.GetBytes(body, "model").String())
				require.Equal(t, "high", gjson.GetBytes(body, "reasoning.effort").String())
				require.False(t, gjson.GetBytes(body, "reasoning.max_tokens").Exists())
				require.Equal(t, "yes", gjson.GetBytes(body, "metadata.kept").String())
			},
		},
		{
			name:         "messages pass-through and override",
			channelType:  channel.TypeAnthropic,
			apiFormat:    llm.APIFormatAnthropicMessage,
			requestPath:  "/anthropic/v1/messages",
			requestBody:  `{"model":"logical-model","max_tokens":1024,"messages":[{"role":"user","content":"hello"}],"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"max","other":"kept"},"metadata":{"user_id":"kept"}}`,
			responseBody: `{"id":"msg-bounds","type":"message","role":"assistant","model":"upstream-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`,
			settings:     &objects.ModelSettings{MaxReasoningEffort: "high"},
			overrideOps: []objects.OverrideOperation{
				{Op: objects.OverrideOpSet, Path: "thinking.budget_tokens", Value: "1"},
				{Op: objects.OverrideOpSet, Path: "thinking.display", Value: "kept"},
				{Op: objects.OverrideOpSet, Path: "output_config.effort", Value: "low"},
			},
			buildTransformers: func(t *testing.T) (transformer.Inbound, transformer.Outbound) {
				outbound, err := anthropic.NewOutboundTransformer("https://api.example.com", "test-key")
				require.NoError(t, err)
				return anthropic.NewInboundTransformer(), outbound
			},
			assertProvider: func(t *testing.T, body []byte) {
				require.Equal(t, "upstream-model", gjson.GetBytes(body, "model").String())
				require.Equal(t, "enabled", gjson.GetBytes(body, "thinking.type").String())
				require.Equal(t, int64(30000), gjson.GetBytes(body, "thinking.budget_tokens").Int())
				require.False(t, gjson.GetBytes(body, "output_config.effort").Exists())
				require.Equal(t, "kept", gjson.GetBytes(body, "thinking.display").String())
				require.Equal(t, "kept", gjson.GetBytes(body, "output_config.other").String())
				require.Equal(t, "kept", gjson.GetBytes(body, "metadata.user_id").String())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, client := setupTest(t)
			project := createTestProject(t, ctx, client)
			passThrough := true
			channelRow, err := client.Channel.Create().
				SetType(tt.channelType).
				SetName("reasoning bounds " + tt.name).
				SetBaseURL("https://api.example.com").
				SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
				SetSupportedModels([]string{"upstream-model"}).
				SetDefaultTestModel("upstream-model").
				SetSettings(&objects.ChannelSettings{PassThroughBody: &passThrough, FullPassThrough: tt.fullPassThrough, BodyOverrideOperations: tt.overrideOps}).
				SetStatus(channel.StatusEnabled).
				Save(ctx)
			require.NoError(t, err)

			inbound, outbound := tt.buildTransformers(t)
			bizChannel := &biz.Channel{Channel: channelRow, Outbound: outbound}
			selector := &staticChannelSelector{candidates: []*ChannelModelsCandidate{{
				Channel:       bizChannel,
				Models:        []biz.ChannelModelEntry{{RequestModel: "logical-model", ActualModel: "upstream-model"}},
				APIFormat:     string(tt.apiFormat),
				ModelSettings: tt.settings,
			}}}
			channelService, requestService, systemService, usageLogService := setupTestServices(t, client)
			executor := &mockExecutor{response: &httpclient.Response{
				StatusCode: http.StatusOK,
				Headers:    http.Header{"Content-Type": []string{"application/json"}},
				Body:       []byte(tt.responseBody),
			}}
			orchestrator := &ChatCompletionOrchestrator{
				channelSelector:       selector,
				Inbound:               inbound,
				RequestService:        requestService,
				ChannelService:        channelService,
				PromptProvider:        &stubPromptProvider{},
				SystemService:         systemService,
				UsageLogService:       usageLogService,
				PipelineFactory:       pipeline.NewFactory(executor),
				ModelMapper:           NewModelMapper(),
				channelLimiterManager: NewChannelLimiterManager(),
				Middlewares:           []pipeline.Middleware{stream.EnsureUsage()},
			}
			request := &httpclient.Request{
				Method:  http.MethodPost,
				URL:     tt.requestPath,
				Path:    tt.requestPath,
				Headers: http.Header{"Content-Type": []string{"application/json"}},
				Body:    []byte(tt.requestBody),
			}
			originalBody := append([]byte(nil), request.Body...)
			ctx = contexts.WithProjectID(ctx, project.ID)

			result, err := orchestrator.Process(ctx, request)
			require.NoError(t, err)
			require.NotNil(t, result.ChatCompletion)
			require.Equal(t, "logical-model", gjson.GetBytes(result.ChatCompletion.Body, "model").String())
			require.Equal(t, originalBody, request.Body)
			require.NotNil(t, executor.lastRequest)
			tt.assertProvider(t, executor.lastRequest.Body)

			executions, err := client.RequestExecution.Query().All(ctx)
			require.NoError(t, err)
			require.Len(t, executions, 1)
			storedProviderBody, err := requestService.LoadRequestExecutionRequestBody(ctx, executions[0])
			require.NoError(t, err)
			require.JSONEq(t, string(executor.lastRequest.Body), string(storedProviderBody))
			require.True(t, executions[0].PassThroughApplied)
		})
	}
}

func TestReasoningBoundsUseMappedConfiguredModelAndRemainModelIsolated(t *testing.T) {
	ctx, client := setupTest(t)
	project := createTestProject(t, ctx, client)
	channelRow, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("reasoning bounds aliases").
		SetBaseURL("https://api.example.com").
		SetCredentials(objects.ChannelCredentials{APIKey: "test-key"}).
		SetSupportedModels([]string{"bounded-upstream", "unbounded-upstream"}).
		SetDefaultTestModel("bounded-upstream").
		SetStatus(channel.StatusEnabled).
		Save(ctx)
	require.NoError(t, err)

	createModel := func(modelID, actualModel string, settings *objects.ModelSettings) {
		settings.Associations = []*objects.ModelAssociation{{
			Type:         "channel_model",
			ChannelModel: &objects.ChannelModelAssociation{ChannelID: channelRow.ID, ModelID: actualModel},
		}}
		_, err := client.Model.Create().
			SetDeveloper("test").
			SetModelID(modelID).
			SetType(model.TypeChat).
			SetName(modelID).
			SetIcon("test").
			SetGroup("test").
			SetModelCard(&objects.ModelCard{}).
			SetSettings(settings).
			SetStatus(model.StatusEnabled).
			Save(ctx)
		require.NoError(t, err)
	}
	createModel("bounded-model", "bounded-upstream", &objects.ModelSettings{MinReasoningEffort: "medium"})
	createModel("unbounded-model", "unbounded-upstream", &objects.ModelSettings{})

	user, err := client.User.Create().SetEmail("bounds@example.com").SetPassword("password").Save(ctx)
	require.NoError(t, err)
	apiKey, err := client.APIKey.Create().
		SetName("reasoning bounds key").
		SetKey("sk-reasoning-bounds").
		SetProjectID(project.ID).
		SetUserID(user.ID).
		SetProfiles(&objects.APIKeyProfiles{
			ActiveProfile: "default",
			Profiles: []objects.APIKeyProfile{{
				Name:          "default",
				ModelMappings: []objects.ModelMapping{{From: "public-alias", To: "bounded-model"}},
			}},
		}).
		Save(ctx)
	require.NoError(t, err)

	channelService := newTestChannelServiceForChannels(client)
	modelService := newTestModelService(client)
	systemService := newTestSystemService(client)
	selector := NewDefaultSelector(channelService, modelService, systemService)

	state := &PersistenceState{APIKey: apiKey, ModelMapper: NewModelMapper(), CandidateSelector: selector}
	inbound := &PersistentInboundTransformer{state: state}
	req := &llm.Request{Model: "public-alias", ReasoningEffort: "low", ReasoningBudget: lo.ToPtr(int64(999))}
	req, err = applyModelMapping(inbound).OnInboundLlmRequest(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "bounded-model", req.Model)
	req, err = selectCandidates(inbound, nil, systemService).OnInboundLlmRequest(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "medium", req.ReasoningEffort)
	require.Nil(t, req.ReasoningBudget)
	require.True(t, state.ReasoningBoundsEnforced)
	require.Equal(t, "bounded-upstream", state.ChannelModelsCandidates[0].Models[0].ActualModel)

	unboundedState := &PersistenceState{CandidateSelector: selector}
	unboundedReq := &llm.Request{Model: "unbounded-model", ReasoningEffort: "low", ReasoningBudget: lo.ToPtr(int64(999))}
	unboundedReq, err = selectCandidates(&PersistentInboundTransformer{state: unboundedState}, nil, systemService).OnInboundLlmRequest(ctx, unboundedReq)
	require.NoError(t, err)
	require.Equal(t, "low", unboundedReq.ReasoningEffort)
	require.NotNil(t, unboundedReq.ReasoningBudget)
	require.False(t, unboundedState.ReasoningBoundsEnforced)
	require.Equal(t, "unbounded-upstream", unboundedState.ChannelModelsCandidates[0].Models[0].ActualModel)
}

func TestReconcileBoundedResponseStreamModel(t *testing.T) {
	state := &PersistenceState{ReasoningBoundsActive: true, ReasoningBoundsEnforced: true, RequestedModel: "public-model"}
	outbound := &PersistentOutboundTransformer{state: state}
	source := streams.SliceStream([]*httpclient.StreamEvent{
		{Type: "chat", Data: []byte(`{"model":"upstream-model"}`)},
		{Type: "response.created", Data: []byte(`{"response":{"model":"upstream-model"}}`)},
		{Type: "message_start", Data: []byte(`{"message":{"model":"upstream-model"}}`)},
	})

	stream, err := reconcileBoundedResponseStreamModel(outbound).OnInboundRawStream(context.Background(), source)
	require.NoError(t, err)
	paths := []string{"model", "response.model", "message.model"}
	for _, path := range paths {
		require.True(t, stream.Next())
		require.Equal(t, "public-model", gjson.GetBytes(stream.Current().Data, path).String())
	}
	require.False(t, stream.Next())
}

func TestReasoningBoundsAntigravityEnvelopeSurvivesOverride(t *testing.T) {
	httpClient := httpclient.NewHttpClientWithClient(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(bytes.NewBufferString(
				`{"access_token":"mock-access-token","token_type":"Bearer","expires_in":3600}`,
			)),
		}, nil
	})})
	provider, err := antigravity.NewTransformer(antigravity.Config{
		BaseURL: "https://api.example.com",
		APIKey:  "refresh-token|project-id",
		Project: "project-id",
	}, antigravity.WithHTTPClient(httpClient))
	require.NoError(t, err)

	req := &llm.Request{
		Model:           "gemini-3-pro",
		ReasoningEffort: "high",
		ExtraBody:       []byte(`{"google":{"thinking_config":{"thinking_level":"high","thinking_budget":999,"include_thoughts":false}}}`),
		Messages: []llm.Message{{
			Role:    "user",
			Content: llm.MessageContent{Content: lo.ToPtr("hello")},
		}},
	}
	enforced, changed := applyModelReasoningBounds(context.Background(), req, &objects.ModelSettings{MaxReasoningEffort: "low"})
	require.True(t, enforced)
	require.True(t, changed)

	channelSettings := &objects.ChannelSettings{BodyOverrideOperations: []objects.OverrideOperation{
		{Op: objects.OverrideOpSet, Path: "request.generationConfig.thinkingConfig.thinkingLevel", Value: "high"},
		{Op: objects.OverrideOpSet, Path: "request.generationConfig.thinkingConfig.thinkingBudget", Value: "999"},
	}}
	channelRow := &ent.Channel{ID: 1, Name: "antigravity", Settings: channelSettings}
	state := &PersistenceState{
		LlmRequest:              req,
		OriginalModel:           req.Model,
		ReasoningBoundsActive:   true,
		ReasoningBoundsEnforced: enforced,
		ReasoningBoundsChanged:  changed,
		ChannelModelsCandidates: []*ChannelModelsCandidate{{
			Channel: &biz.Channel{Channel: channelRow, Outbound: provider},
			Models:  []biz.ChannelModelEntry{{RequestModel: req.Model, ActualModel: req.Model}},
		}},
	}
	captureBoundedReasoningRequest(state, req)
	outbound := &PersistentOutboundTransformer{state: state}
	providerRequest, err := outbound.TransformRequest(context.Background(), req)
	require.NoError(t, err)
	require.Empty(t, providerRequest.APIFormat)
	require.Equal(t, "gemini-3-pro", providerRequest.Metadata["antigravity_model"])

	providerRequest, err = captureBoundedReasoningWire(outbound).OnOutboundRawRequest(context.Background(), providerRequest)
	require.NoError(t, err)
	providerRequest, err = applyOverrideRequestBody(outbound).OnOutboundRawRequest(context.Background(), providerRequest)
	require.NoError(t, err)
	providerRequest, err = reconcileBoundedReasoningWire(outbound).OnOutboundRawRequest(context.Background(), providerRequest)
	require.NoError(t, err)

	require.Equal(t, "low", gjson.GetBytes(providerRequest.Body, "request.generationConfig.thinkingConfig.thinkingLevel").String())
	require.False(t, gjson.GetBytes(providerRequest.Body, "request.generationConfig.thinkingConfig.thinkingBudget").Exists())
	require.True(t, gjson.GetBytes(providerRequest.Body, "request.generationConfig.thinkingConfig.includeThoughts").Exists())
	require.False(t, gjson.GetBytes(providerRequest.Body, "request.generationConfig.thinkingConfig.includeThoughts").Bool())
}

func TestReasoningBoundsGeminiPreservesIncludeThoughts(t *testing.T) {
	provider, err := gemini.NewOutboundTransformer("https://api.example.com", "test-key")
	require.NoError(t, err)
	state := &PersistenceState{}
	inbound := &PersistentInboundTransformer{wrapped: gemini.NewInboundTransformer(), state: state}
	req, err := inbound.TransformRequest(context.Background(), &httpclient.Request{
		Path: "/v1beta/models/gemini-3-pro:generateContent",
		Body: []byte(`{
			"contents":[{"role":"user","parts":[{"text":"hello"}]}],
			"generationConfig":{"thinkingConfig":{"thinkingLevel":"high","thinkingBudget":999,"includeThoughts":false}}
		}`),
	})
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(req.ExtraBody, "google.thinking_config.include_thoughts").Exists(), "actual Gemini inbound omits false from ExtraBody")
	enforced, changed := applyModelReasoningBounds(context.Background(), req, &objects.ModelSettings{MaxReasoningEffort: "low"})
	require.True(t, enforced)
	require.True(t, changed)

	channelSettings := &objects.ChannelSettings{BodyOverrideOperations: []objects.OverrideOperation{
		{Op: objects.OverrideOpSet, Path: "generationConfig.thinkingConfig.thinkingLevel", Value: "high"},
		{Op: objects.OverrideOpSet, Path: "generationConfig.thinkingConfig.thinkingBudget", Value: "999"},
	}}
	state.LlmRequest = req
	state.OriginalModel = req.Model
	state.ReasoningBoundsActive = true
	state.ReasoningBoundsEnforced = enforced
	state.ReasoningBoundsChanged = changed
	state.ChannelModelsCandidates = []*ChannelModelsCandidate{{
		Channel: &biz.Channel{Channel: &ent.Channel{ID: 1, Name: "gemini", Settings: channelSettings}, Outbound: provider},
		Models:  []biz.ChannelModelEntry{{RequestModel: req.Model, ActualModel: req.Model}},
	}}
	captureBoundedReasoningRequest(state, req)
	outbound := &PersistentOutboundTransformer{state: state}
	providerRequest, err := outbound.TransformRequest(context.Background(), req)
	require.NoError(t, err)
	providerRequest, err = captureBoundedReasoningWire(outbound).OnOutboundRawRequest(context.Background(), providerRequest)
	require.NoError(t, err)
	providerRequest, err = applyOverrideRequestBody(outbound).OnOutboundRawRequest(context.Background(), providerRequest)
	require.NoError(t, err)
	providerRequest, err = reconcileBoundedReasoningWire(outbound).OnOutboundRawRequest(context.Background(), providerRequest)
	require.NoError(t, err)

	require.Equal(t, "low", gjson.GetBytes(providerRequest.Body, "generationConfig.thinkingConfig.thinkingLevel").String())
	require.False(t, gjson.GetBytes(providerRequest.Body, "generationConfig.thinkingConfig.thinkingBudget").Exists())
	require.True(t, gjson.GetBytes(providerRequest.Body, "generationConfig.thinkingConfig.includeThoughts").Exists())
	require.False(t, gjson.GetBytes(providerRequest.Body, "generationConfig.thinkingConfig.includeThoughts").Bool())
}

func TestReasoningBoundsGeminiOpenAICrossFormatPreservesThinkingProperties(t *testing.T) {
	tests := []struct {
		name                string
		thinkingConfig      string
		settings            *objects.ModelSettings
		wantEffort          string
		wantBudget          int64
		wantBudgetExists    bool
		wantIncludeThoughts bool
	}{
		{
			name:                "clamped level preserves explicit false",
			thinkingConfig:      `{"thinkingLevel":"high","thinkingBudget":999,"includeThoughts":false}`,
			settings:            &objects.ModelSettings{MaxReasoningEffort: "low"},
			wantEffort:          "low",
			wantIncludeThoughts: false,
		},
		{
			name:                "clamped level preserves explicit true",
			thinkingConfig:      `{"thinkingLevel":"high","thinkingBudget":999,"includeThoughts":true}`,
			settings:            &objects.ModelSettings{MaxReasoningEffort: "low"},
			wantEffort:          "low",
			wantIncludeThoughts: true,
		},
		{
			name:                "in-range budget remains authoritative",
			thinkingConfig:      `{"thinkingBudget":1024,"includeThoughts":false}`,
			settings:            &objects.ModelSettings{MaxReasoningEffort: "high"},
			wantBudget:          1024,
			wantBudgetExists:    true,
			wantIncludeThoughts: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, err := geminioai.NewOutboundTransformer("https://api.example.com", "test-key")
			require.NoError(t, err)
			state := &PersistenceState{}
			inbound := &PersistentInboundTransformer{wrapped: gemini.NewInboundTransformer(), state: state}
			req, err := inbound.TransformRequest(context.Background(), &httpclient.Request{
				Path: "/v1beta/models/gemini-3-pro:generateContent",
				Body: []byte(`{
					"contents":[{"role":"user","parts":[{"text":"hello"}]}],
					"generationConfig":{"thinkingConfig":` + tt.thinkingConfig + `}
				}`),
			})
			require.NoError(t, err)
			enforced, changed := applyModelReasoningBounds(context.Background(), req, tt.settings)
			require.True(t, enforced)

			channelSettings := &objects.ChannelSettings{BodyOverrideOperations: []objects.OverrideOperation{
				{Op: objects.OverrideOpSet, Path: "reasoning_effort", Value: "ultra"},
				{Op: objects.OverrideOpSet, Path: "extra_body.google.thinking_config.thinking_budget", Value: "99999"},
			}}
			candidate := &ChannelModelsCandidate{
				Channel: &biz.Channel{Channel: &ent.Channel{
					ID:       1,
					Name:     "gemini-openai",
					Type:     channel.TypeGeminiOpenai,
					Settings: channelSettings,
				}, Outbound: provider},
				Models: []biz.ChannelModelEntry{{RequestModel: req.Model, ActualModel: req.Model}},
			}
			state.LlmRequest = req
			state.OriginalModel = req.Model
			state.ReasoningBoundsActive = true
			state.ReasoningBoundsEnforced = enforced
			state.ReasoningBoundsChanged = changed
			state.ChannelModelsCandidates = []*ChannelModelsCandidate{candidate}
			captureBoundedReasoningRequest(state, req)
			outbound := &PersistentOutboundTransformer{state: state}
			providerRequest, err := outbound.TransformRequest(context.Background(), req)
			require.NoError(t, err)
			providerRequest, err = captureBoundedReasoningWire(outbound).OnOutboundRawRequest(context.Background(), providerRequest)
			require.NoError(t, err)
			providerRequest, err = applyOverrideRequestBody(outbound).OnOutboundRawRequest(context.Background(), providerRequest)
			require.NoError(t, err)
			providerRequest, err = reconcileBoundedReasoningWire(outbound).OnOutboundRawRequest(context.Background(), providerRequest)
			require.NoError(t, err)

			require.Equal(t, tt.wantEffort, gjson.GetBytes(providerRequest.Body, "reasoning_effort").String())
			budget := gjson.GetBytes(providerRequest.Body, "extra_body.google.thinking_config.thinking_budget")
			require.Equal(t, tt.wantBudgetExists, budget.Exists())
			if tt.wantBudgetExists {
				require.Equal(t, tt.wantBudget, budget.Int())
			}
			includeThoughts := gjson.GetBytes(providerRequest.Body, "extra_body.google.thinking_config.include_thoughts")
			require.True(t, includeThoughts.Exists())
			require.Equal(t, tt.wantIncludeThoughts, includeThoughts.Bool())
		})
	}
}

func TestReasoningBoundsPreserveOriginalAutoSuffixModelInResponses(t *testing.T) {
	ctx, client := setupTest(t)
	systemService := newTestSystemService(client)
	require.NoError(t, systemService.SetModelSettings(ctx, biz.SystemModelSettings{AutoReasoningEffort: true}))

	state := &PersistenceState{
		APIKey: &ent.APIKey{
			Name: "mapped-key",
			Profiles: &objects.APIKeyProfiles{
				ActiveProfile: "default",
				Profiles: []objects.APIKeyProfile{{
					Name:          "default",
					ModelMappings: []objects.ModelMapping{{From: "public-model", To: "logical-model"}},
				}},
			},
		},
		ModelMapper: NewModelMapper(),
		CandidateSelector: &staticChannelSelector{candidates: []*ChannelModelsCandidate{{
			Channel:       &biz.Channel{Channel: &ent.Channel{ID: 1, Name: "bounded"}},
			Models:        []biz.ChannelModelEntry{{RequestModel: "logical-model", ActualModel: "upstream-model"}},
			ModelSettings: &objects.ModelSettings{MaxReasoningEffort: "low"},
		}}},
	}
	inbound, outbound := NewPersistentTransformers(state, responses.NewInboundTransformer())
	rawRequest := &httpclient.Request{
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"public-model-high","input":"hello"}`),
	}
	req, err := inbound.TransformRequest(ctx, rawRequest)
	require.NoError(t, err)
	require.Equal(t, "public-model-high", state.RequestedModel)

	req, err = applyAutoReasoningEffort(systemService).OnInboundLlmRequest(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "public-model", req.Model)
	require.Equal(t, "high", req.ReasoningEffort)
	req, err = applyModelMapping(inbound).OnInboundLlmRequest(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "logical-model", req.Model)
	req, err = selectCandidates(inbound, nil, systemService).OnInboundLlmRequest(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "low", req.ReasoningEffort)

	nonStream, err := reconcileBoundedResponseModel(outbound).OnInboundRawResponse(ctx, &httpclient.Response{
		Body: []byte(`{"model":"upstream-model"}`),
	})
	require.NoError(t, err)
	require.Equal(t, "public-model-high", gjson.GetBytes(nonStream.Body, "model").String())

	stream, err := reconcileBoundedResponseStreamModel(outbound).OnInboundRawStream(ctx, streams.SliceStream([]*httpclient.StreamEvent{
		{Type: "response.created", Data: []byte(`{"response":{"model":"upstream-model"}}`)},
	}))
	require.NoError(t, err)
	require.True(t, stream.Next())
	require.Equal(t, "public-model-high", gjson.GetBytes(stream.Current().Data, "response.model").String())
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
