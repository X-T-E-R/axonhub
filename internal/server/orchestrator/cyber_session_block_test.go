package orchestrator

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	entchannel "github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

func TestIsCyberPolicyPayloadRequiresStructuredExactCode(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "error code", body: `{"error":{"code":" cyber_policy ","message":"refused"}}`, want: true},
		{name: "response error code", body: `{"type":"response.failed","response":{"error":{"code":"CYBER_POLICY"}}}`, want: true},
		{name: "empty top code falls back", body: `{"error":{"code":""},"response":{"error":{"code":"cyber_policy"}}}`, want: true},
		{name: "nonempty top code suppresses nested", body: `{"error":{"code":"different"},"response":{"error":{"code":"cyber_policy"}}}`, want: false},
		{name: "literal assistant text", body: `{"choices":[{"message":{"content":"cyber_policy"}}]}`, want: false},
		{name: "tool output JSON string", body: `{"output":"{\"error\":{\"code\":\"cyber_policy\"}}"}`, want: false},
		{name: "different code", body: `{"error":{"code":"policy_violation","message":"cyber_policy"}}`, want: false},
		{name: "malformed", body: `{"error":`, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isCyberPolicyPayload([]byte(tt.body)))
		})
	}
}

func TestCyberSemanticSessionIDPriority(t *testing.T) {
	req := &httpclient.Request{
		Body: []byte(`{"client_metadata":{"session_id":"native-session","x-codex-turn-metadata":"{\"session_id\":\"body-turn-session\"}"},"metadata":{"user_id":"{\"session_id\":\"claude-session\"}"}}`),
		Headers: http.Header{
			"X-Codex-Turn-Metadata": []string{`{"session_id":"header-turn-session"}`},
			"Session-Id":            []string{"compat-session"},
		},
	}
	require.Equal(t, "native-session", cyberSemanticSessionID(req, llm.APIFormatOpenAIResponse))

	req.Body = []byte(`{"client_metadata":{"x-codex-turn-metadata":{"session_id":"body-turn-session"}}}`)
	require.Equal(t, "body-turn-session", cyberSemanticSessionID(req, llm.APIFormatOpenAIResponse))

	req.Body = []byte(`{"metadata":{"user_id":"{\"session_id\":\"claude-session\"}"}}`)
	require.Equal(t, "header-turn-session", cyberSemanticSessionID(req, llm.APIFormatOpenAIResponse))

	req.Headers.Del("X-Codex-Turn-Metadata")
	require.Equal(t, "compat-session", cyberSemanticSessionID(req, llm.APIFormatOpenAIResponse))

	req.Headers.Del("Session-Id")
	require.Equal(t, "claude-session", cyberSemanticSessionID(req, llm.APIFormatAnthropicMessage))

	req.Body = []byte(`{"client_metadata":{"session_id":123}}`)
	req.Headers.Set("X-Session-Id", "typed-header-fallback")
	require.Equal(t, "typed-header-fallback", cyberSemanticSessionID(req, llm.APIFormatOpenAIResponse), "non-string body IDs must not be coerced")
}

func TestCyberSemanticSessionIDValidatesTurnMetadataAndSupportsUppercaseBodyKey(t *testing.T) {
	sharedHeaders := http.Header{"Session-Id": []string{"shared-header"}}
	for _, body := range []string{
		`{"client_metadata":{"x-codex-turn-metadata":{"session_id":123}}}`,
		`{"client_metadata":{"x-codex-turn-metadata":{"session_id":{"invalid":true}}}}`,
	} {
		malformed := &httpclient.Request{Body: []byte(body), Headers: sharedHeaders.Clone()}
		require.Equal(t, "shared-header", cyberSemanticSessionID(malformed, llm.APIFormatOpenAIResponse))
	}

	first := cyberLLMRequest(`{"client_metadata":{"X-Codex-Turn-Metadata":{"session_id":"body-a"}},"input":"same"}`)
	first.APIFormat = llm.APIFormatOpenAIResponse
	first.RawRequest.Headers = sharedHeaders.Clone()
	second := cyberLLMRequest(`{"client_metadata":{"X-Codex-Turn-Metadata":{"session_id":"body-b"}},"input":"same"}`)
	second.APIFormat = llm.APIFormatOpenAIResponse
	second.RawRequest.Headers = sharedHeaders.Clone()

	_, firstMark, ok := cyberSessionCacheKeys(cyberTestPersistenceState(7, 11), first)
	require.True(t, ok)
	_, secondMark, ok := cyberSessionCacheKeys(cyberTestPersistenceState(7, 11), second)
	require.True(t, ok)
	require.NotEqual(t, firstMark, secondMark)
}

func TestCyberTranscriptLookupMatchesOnlyStoredAncestry(t *testing.T) {
	state := cyberTestPersistenceState(7, 11)
	refused := cyberLLMRequest(`{"instructions":"be precise","messages":[{"role":"system","content":"rules"},{"role":"user","content":"first"}]}`)
	_, refusedMark, ok := cyberSessionCacheKeys(state, refused)
	require.True(t, ok)

	continued := cyberLLMRequest(`{"instructions":"be precise","messages":[{"content":"rules","role":"system"},{"content":"first","role":"user"},{"role":"assistant","content":"refused"},{"role":"user","content":"next"}]}`)
	continuedLookup, _, ok := cyberSessionCacheKeys(state, continued)
	require.True(t, ok)
	require.Contains(t, continuedLookup, refusedMark)

	rewrittenLatest := cyberLLMRequest(`{"instructions":"be precise","messages":[{"role":"system","content":"rules"},{"role":"user","content":"different"}]}`)
	rewrittenLookup, _, ok := cyberSessionCacheKeys(state, rewrittenLatest)
	require.True(t, ok)
	require.NotContains(t, rewrittenLookup, refusedMark)

	differentInstructions := cyberLLMRequest(`{"instructions":"different","messages":[{"role":"system","content":"rules"},{"role":"user","content":"first"}]}`)
	differentLookup, _, ok := cyberSessionCacheKeys(state, differentInstructions)
	require.True(t, ok)
	require.NotContains(t, differentLookup, refusedMark)
}

func TestCyberTranscriptLookupBoundsLongHistoryWithoutCoarseFallback(t *testing.T) {
	state := cyberTestPersistenceState(7, 11)
	items := make([]string, 257)
	for i := range items {
		items[i] = fmt.Sprintf(`{"role":"user","content":"item-%d"}`, i)
	}
	longRequest := cyberLLMRequest(`{"messages":[` + strings.Join(items, ",") + `]}`)
	lookup, _, ok := cyberSessionCacheKeys(state, longRequest)
	require.True(t, ok)
	require.Len(t, lookup, maxCyberFallbackMessages)

	_, firstMark, ok := cyberSessionCacheKeys(state, cyberLLMRequest(`{"messages":[`+items[0]+`]}`))
	require.True(t, ok)
	require.NotContains(t, lookup, firstMark, "the truncated oldest prefix must fail open")

	_, secondMark, ok := cyberSessionCacheKeys(state, cyberLLMRequest(`{"messages":[`+items[0]+`,`+items[1]+`]}`))
	require.True(t, ok)
	require.Contains(t, lookup, secondMark, "a retained recent ancestry prefix must still match")
}

func TestCyberSessionCacheKeysRequireTrustedKeyAndKeepSemanticIDsExclusive(t *testing.T) {
	request := cyberLLMRequest(`{"client_metadata":{"session_id":"semantic"},"messages":[{"role":"user","content":"hello"}],"prompt_cache_key":"reusable"}`)
	request.APIFormat = llm.APIFormatOpenAIResponse
	lookup, mark, ok := cyberSessionCacheKeys(cyberTestPersistenceState(7, 11), request)
	require.True(t, ok)
	require.Equal(t, []string{mark}, lookup)
	_, transcriptMark, ok := cyberSessionCacheKeys(
		cyberTestPersistenceState(7, 11),
		cyberLLMRequest(`{"messages":[{"role":"user","content":"hello"}],"prompt_cache_key":"reusable"}`),
	)
	require.True(t, ok)
	require.NotContains(t, lookup, transcriptMark, "an explicit semantic session miss must not fall through to transcript identity")

	otherSession := cyberLLMRequest(`{"client_metadata":{"session_id":"other"},"messages":[{"role":"user","content":"hello"}],"prompt_cache_key":"reusable"}`)
	otherSession.APIFormat = llm.APIFormatOpenAIResponse
	_, otherMark, ok := cyberSessionCacheKeys(cyberTestPersistenceState(7, 11), otherSession)
	require.True(t, ok)
	require.NotEqual(t, mark, otherMark)

	otherKeyLookup, otherKeyMark, ok := cyberSessionCacheKeys(cyberTestPersistenceState(7, 12), request)
	require.True(t, ok)
	require.Equal(t, []string{otherKeyMark}, otherKeyLookup)
	require.NotEqual(t, mark, otherKeyMark)

	_, _, ok = cyberSessionCacheKeys(&PersistenceState{}, request)
	require.False(t, ok)

	firstPromptCacheUse := cyberLLMRequest(`{"prompt_cache_key":"shared-cache-key","messages":[{"role":"user","content":"one"}]}`)
	secondPromptCacheUse := cyberLLMRequest(`{"prompt_cache_key":"shared-cache-key","messages":[{"role":"user","content":"two"}]}`)
	_, firstPromptMark, ok := cyberSessionCacheKeys(cyberTestPersistenceState(7, 11), firstPromptCacheUse)
	require.True(t, ok)
	secondPromptLookup, secondPromptMark, ok := cyberSessionCacheKeys(cyberTestPersistenceState(7, 11), secondPromptCacheUse)
	require.True(t, ok)
	require.NotEqual(t, firstPromptMark, secondPromptMark)
	require.NotContains(t, secondPromptLookup, firstPromptMark)

	threadOnly := cyberLLMRequest(`{"model":"gpt-4"}`)
	threadOnly.RawRequest.Headers.Set("AH-Thread-Id", "database-thread")
	_, _, ok = cyberSessionCacheKeys(cyberTestPersistenceState(7, 11), threadOnly)
	require.False(t, ok, "database thread IDs are not upstream semantic conversation IDs")
}

func TestCyberPolicyObservationStreamMarksWithoutChangingEvent(t *testing.T) {
	service := newCyberCacheOnlySystemService()
	blocking := newCyberSessionRequestState(service, 60)
	blocking.setKeys([]string{"block-key"}, "block-key")
	event := &httpclient.StreamEvent{Type: "response.failed", Data: []byte(`{"response":{"error":{"code":"cyber_policy"}}}`)}
	stream := &cyberPolicyObservationStream{
		Stream:   streams.SliceStream([]*httpclient.StreamEvent{event}),
		ctx:      context.Background(),
		blocking: blocking,
	}

	require.True(t, stream.Next())
	require.Same(t, event, stream.Current())
	requireCyberSessionBlocked(t, service, "block-key")
}

func TestCyberPolicyObservationMarksStructuredSuccessResponse(t *testing.T) {
	service := newCyberCacheOnlySystemService()
	blocking := newCyberSessionRequestState(service, 60)
	blocking.setKeys([]string{"success-key"}, "success-key")
	middleware := &cyberSessionObservationMiddleware{blocking: blocking}
	response := &httpclient.Response{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"type":"response.failed","response":{"error":{"code":"cyber_policy","message":"refused"}}}`),
	}

	result, err := middleware.OnOutboundRawResponse(context.Background(), response)
	require.NoError(t, err)
	require.Same(t, response, result)
	requireCyberSessionBlocked(t, service, "success-key")
}

func TestCyberPolicyObservationStopsCurrentRetryWithoutIdentity(t *testing.T) {
	blocking := newCyberSessionRequestState(newCyberCacheOnlySystemService(), 60)
	middleware := &cyberSessionObservationMiddleware{blocking: blocking}
	middleware.OnOutboundRawError(context.Background(), &httpclient.Error{
		StatusCode: http.StatusForbidden,
		Body:       []byte(`{"error":{"code":"cyber_policy"}}`),
	})

	require.True(t, blocking.retryBlocked(context.Background()))
}

func TestCyberSessionLocalReadDoesNotRefreshTTL(t *testing.T) {
	ctx := context.Background()
	service := newCyberCacheOnlySystemService()
	require.NoError(t, service.SetCyberSessionBlock(ctx, "ttl-key", 300*time.Millisecond))
	time.Sleep(100 * time.Millisecond)
	_, blocked := service.CyberSessionBlocks(ctx, []string{"ttl-key"})
	require.True(t, blocked)
	require.NoError(t, service.SetCyberSessionBlock(ctx, "ttl-key", time.Second), "a repeated mark must not refresh the first block")
	time.Sleep(250 * time.Millisecond)
	_, blocked = service.CyberSessionBlocks(ctx, []string{"ttl-key"})
	require.False(t, blocked)
}

func TestCyberPolicyDoesNotApplyChannelOrKeyFailurePolicy(t *testing.T) {
	testCyberPolicyFailurePolicy(t, true)
}

func TestCyberPolicyDisabledPreservesExistingFailurePolicyBehavior(t *testing.T) {
	testCyberPolicyFailurePolicy(t, false)
}

func testCyberPolicyFailurePolicy(t *testing.T, enabled bool) {
	t.Helper()
	databaseName := fmt.Sprintf("file:cyber-failure-policy-%t?mode=memory&_fk=0", enabled)
	client := enttest.NewEntClient(t, "sqlite3", databaseName)
	defer client.Close()
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	minFailures := 1
	upstreamKey := "sk-cyber-account"
	channelRow := client.Channel.Create().
		SetName("Cyber policy channel").
		SetType(entchannel.TypeOpenai).
		SetBaseURL("https://api.openai.example.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{upstreamKey}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetStatus(entchannel.StatusEnabled).
		SetSettings(&objects.ChannelSettings{FailurePolicy: &objects.ChannelFailurePolicy{
			KeyProfiles: []objects.FailurePolicyProfile{{
				ID:      "disable-key-on-403",
				Name:    "Disable key on 403",
				Sources: []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
				Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
					MinFailureCount: &minFailures,
					StatusCodes:     []int{http.StatusForbidden},
				},
				Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDisableKey}},
			}},
			ChannelProfiles: []objects.FailurePolicyProfile{{
				ID:      "disable-channel-on-403",
				Name:    "Disable channel on 403",
				Sources: []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
				Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
					MinFailureCount: &minFailures,
					StatusCodes:     []int{http.StatusForbidden},
				},
				Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDisableChannel}},
			}},
		}}).
		SaveX(ctx)

	systemService := biz.NewSystemService(biz.SystemServiceParams{CacheConfig: xcache.Config{Mode: xcache.ModeMemory}, Ent: client})
	channelService := biz.NewChannelService(biz.ChannelServiceParams{
		CacheConfig:   xcache.Config{Mode: xcache.ModeMemory},
		Ent:           client,
		SystemService: systemService,
		HttpClient:    httpclient.NewHttpClient(),
	})
	defer channelService.Stop()
	require.NoError(t, channelService.ReloadEnabledChannelsCache(ctx))
	loaded := channelService.GetEnabledChannel(channelRow.ID)
	require.NotNil(t, loaded)
	state := &PersistenceState{
		ChannelService: channelService,
		CurrentCandidate: &ChannelModelsCandidate{
			Channel: loaded,
			Models:  []biz.ChannelModelEntry{{RequestModel: "gpt-4", ActualModel: "gpt-4"}},
		},
	}
	if enabled {
		state.cyberSession = newCyberSessionRequestState(systemService, 60)
	}
	outbound := &PersistentOutboundTransformer{wrapped: loaded.Outbound, state: state}
	middleware := &performanceRecording{outbound: outbound}
	ctx = contexts.WithChannelAPIKey(ctx, upstreamKey)
	_, err := middleware.OnOutboundRawRequest(ctx, &httpclient.Request{})
	require.NoError(t, err)
	cyberErr := &httpclient.Error{
		StatusCode: http.StatusForbidden,
		Body:       []byte(`{"error":{"code":"cyber_policy"}}`),
	}
	if enabled {
		(&cyberSessionObservationMiddleware{blocking: state.cyberSession}).OnOutboundRawError(ctx, cyberErr)
	}
	middleware.OnOutboundRawError(ctx, cyberErr)

	updated := client.Channel.GetX(ctx, channelRow.ID)
	if enabled {
		require.Equal(t, entchannel.StatusEnabled, updated.Status)
		require.Equal(t, []string{upstreamKey}, updated.Credentials.APIKeys)
		require.Empty(t, updated.DisabledAPIKeys)
		require.False(t, state.FailurePolicyRoutingChanged)
	} else {
		require.Equal(t, entchannel.StatusDisabled, updated.Status)
		require.Len(t, updated.DisabledAPIKeys, 1)
		require.Equal(t, upstreamKey, updated.DisabledAPIKeys[0].Key)
		require.True(t, state.FailurePolicyRoutingChanged)
	}
}

func TestCyberResponsesFailedThenEmptyDoesNotAffectChannelHealth(t *testing.T) {
	fixture := newResponsesStreamingFixture(t, []*httpclient.StreamEvent{
		{
			Type: "response.created",
			Data: []byte(`{"type":"response.created","response":{"id":"resp_cyber","object":"response","created_at":1,"model":"gpt-4","status":"in_progress","output":[]}}`),
		},
		{
			Type: "response.failed",
			Data: []byte(`{"type":"response.failed","response":{"id":"resp_cyber","object":"response","created_at":1,"model":"gpt-4","status":"failed","output":[],"error":{"code":"cyber_policy","message":"refused"}}}`),
		},
	})
	channelRow := fixture.client.Channel.Query().OnlyX(fixture.ctx)
	minFailures := 1
	channelRow = fixture.client.Channel.UpdateOne(channelRow).
		SetStatus(entchannel.StatusEnabled).
		SetSettings(&objects.ChannelSettings{FailurePolicy: &objects.ChannelFailurePolicy{
			ChannelProfiles: []objects.FailurePolicyProfile{{
				ID:      "disable-on-empty",
				Name:    "Disable on transformed empty response",
				Sources: []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
				Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
					MinFailureCount: &minFailures,
					StatusCodes:     []int{http.StatusInternalServerError},
				},
				Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDisableChannel}},
			}},
		}}).
		SaveX(fixture.ctx)
	require.NoError(t, fixture.orchestrator.SystemService.SetSecuritySettings(fixture.ctx, biz.SecuritySettings{
		BlockedIPs:                  []string{},
		ShowRequestLogIPBanIcon:     true,
		CyberSessionBlockEnabled:    true,
		CyberSessionBlockTTLSeconds: 60,
	}))
	require.NoError(t, fixture.orchestrator.SystemService.SetRetryPolicy(fixture.ctx, &biz.RetryPolicy{
		Enabled:                true,
		EmptyResponseDetection: true,
		LoadBalancerStrategy:   biz.LoadBalancerStrategyAdaptive,
	}))

	_, err := fixture.orchestrator.Process(fixture.ctx, buildTestResponsesRequest())
	require.ErrorIs(t, err, pipeline.ErrEmptyResponse)
	updated := fixture.client.Channel.GetX(fixture.ctx, channelRow.ID)
	require.Equal(t, entchannel.StatusEnabled, updated.Status)
}

func TestChatCompletionOrchestrator_CyberSessionBlockStopsExecutorAndPersistsLocalFailure(t *testing.T) {
	ctx := authz.WithTestBypass(context.Background())
	client := enttest.NewEntClient(t, "sqlite3", "file:cyber-session-block?mode=memory&_fk=0")
	defer client.Close()
	ctx = ent.NewContext(ctx, client)

	project := createTestProject(t, ctx, client)
	apiKey := client.APIKey.Create().SetName("key-one").SetKey("ah-one").SetProjectID(project.ID).SaveX(ctx)
	channel := createTestChannel(t, ctx, client)
	channel = client.Channel.UpdateOne(channel).SetStatus(entchannel.StatusEnabled).SaveX(ctx)
	initialChannelStatus := channel.Status
	executor := &mockExecutor{err: &httpclient.Error{
		StatusCode: http.StatusForbidden,
		Status:     http.StatusText(http.StatusForbidden),
		Body:       []byte(`{"error":{"code":"cyber_policy","message":"refused"}}`),
	}}
	outbound, err := openai.NewOutboundTransformer(channel.BaseURL, channel.Credentials.APIKey)
	require.NoError(t, err)
	orch, _ := newFailedResponsePersistenceOrchestrator(t, ctx, client, channel, outbound, executor)
	require.NoError(t, orch.SystemService.SetSecuritySettings(ctx, biz.SecuritySettings{
		BlockedIPs:                  []string{},
		ShowRequestLogIPBanIcon:     true,
		CyberSessionBlockEnabled:    true,
		CyberSessionBlockTTLSeconds: 60,
	}))
	require.NoError(t, orch.SystemService.SetRetryPolicy(ctx, &biz.RetryPolicy{
		Enabled:                 true,
		MaxSingleChannelRetries: 1,
		LoadBalancerStrategy:    biz.LoadBalancerStrategyAdaptive,
	}))

	requestCtx := ent.NewContext(context.Background(), client)
	requestCtx = contexts.WithAPIKey(contexts.WithProjectID(requestCtx, project.ID), apiKey)
	first := buildTestRequest("gpt-4", "blocked prompt", false)
	first.Headers.Set("Session-Id", "conversation-one")
	_, err = orch.Process(requestCtx, first)
	require.Error(t, err)
	require.EqualValues(t, 1, executor.requestCalls.Load(), "the cyber hit must close retry/failover admission")
	_, firstMark, ok := cyberSessionCacheKeys(&PersistenceState{APIKey: apiKey}, &llm.Request{RawRequest: first})
	require.True(t, ok)
	requireCyberSessionBlocked(t, orch.SystemService, firstMark)

	second := buildTestRequest("gpt-4", "different turn content", false)
	second.Headers.Set("Session-Id", "conversation-one")
	_, err = orch.Process(requestCtx, second)
	require.Error(t, err)
	var responseErr *llm.ResponseError
	require.ErrorAs(t, err, &responseErr)
	require.Equal(t, cyberSessionBlockedErrorCode, responseErr.Detail.Code)
	require.EqualValues(t, 1, executor.requestCalls.Load(), "local block must not invoke the executor")

	requests := client.Request.Query().Order(ent.Asc(request.FieldID)).AllX(ctx)
	require.Len(t, requests, 2)
	require.Equal(t, request.StatusFailed, requests[1].Status)
	require.Contains(t, string(requests[1].ResponseBody), cyberSessionBlockedErrorCode)
	executions := client.RequestExecution.Query().Order(ent.Asc(requestexecution.FieldID)).AllX(ctx)
	require.Len(t, executions, 1, "local block must not create an upstream execution")
	require.Equal(t, requestexecution.StatusFailed, executions[0].Status)
	channelAfterBlock := client.Channel.GetX(ctx, channel.ID)
	require.Equal(t, initialChannelStatus, channelAfterBlock.Status)

	differentConversation := buildTestRequest("gpt-4", "other thread", false)
	differentConversation.Headers.Set("Session-Id", "conversation-two")
	_, err = orch.Process(requestCtx, differentConversation)
	require.Error(t, err)
	require.EqualValues(t, 2, executor.requestCalls.Load(), "another conversation under the same key must reach upstream")
	_, differentConversationMark, ok := cyberSessionCacheKeys(&PersistenceState{APIKey: apiKey}, &llm.Request{RawRequest: differentConversation})
	require.True(t, ok)
	requireCyberSessionBlocked(t, orch.SystemService, differentConversationMark)

	otherKey := client.APIKey.Create().SetName("key-two").SetKey("ah-two").SetProjectID(project.ID).SaveX(ctx)
	otherKeyCtx := ent.NewContext(context.Background(), client)
	otherKeyCtx = contexts.WithAPIKey(contexts.WithProjectID(otherKeyCtx, project.ID), otherKey)
	otherKeyRequest := buildTestRequest("gpt-4", "blocked prompt", false)
	otherKeyRequest.Headers.Set("Session-Id", "conversation-one")
	_, err = orch.Process(otherKeyCtx, otherKeyRequest)
	require.Error(t, err)
	require.EqualValues(t, 3, executor.requestCalls.Load(), "another API key must reach upstream")
	_, otherKeyMark, ok := cyberSessionCacheKeys(&PersistenceState{APIKey: otherKey}, &llm.Request{RawRequest: otherKeyRequest})
	require.True(t, ok)
	requireCyberSessionBlocked(t, orch.SystemService, otherKeyMark)

	otherProject := client.Project.Create().SetName("Other Project").SaveX(ctx)
	otherProjectKey := client.APIKey.Create().SetName("other-project-key").SetKey("ah-other-project").SetProjectID(otherProject.ID).SaveX(ctx)
	otherProjectCtx := ent.NewContext(context.Background(), client)
	otherProjectCtx = contexts.WithAPIKey(contexts.WithProjectID(otherProjectCtx, otherProject.ID), otherProjectKey)
	otherProjectRequest := buildTestRequest("gpt-4", "blocked prompt", false)
	otherProjectRequest.Headers.Set("Session-Id", "conversation-one")
	_, err = orch.Process(otherProjectCtx, otherProjectRequest)
	require.Error(t, err)
	require.EqualValues(t, 4, executor.requestCalls.Load(), "another project must reach upstream")
	_, otherProjectMark, ok := cyberSessionCacheKeys(&PersistenceState{APIKey: otherProjectKey}, &llm.Request{RawRequest: otherProjectRequest})
	require.True(t, ok)
	requireCyberSessionBlocked(t, orch.SystemService, otherProjectMark)

	require.NoError(t, orch.SystemService.SetSecuritySettings(ctx, biz.SecuritySettings{
		BlockedIPs:                  []string{},
		ShowRequestLogIPBanIcon:     true,
		CyberSessionBlockEnabled:    false,
		CyberSessionBlockTTLSeconds: 60,
	}))
	disabledRequest := buildTestRequest("gpt-4", "blocked prompt", false)
	disabledRequest.Headers.Set("Session-Id", "conversation-one")
	_, err = orch.Process(requestCtx, disabledRequest)
	require.Error(t, err)
	require.EqualValues(t, 5, executor.requestCalls.Load(), "disabled admission must ignore an existing cache entry")

	require.NoError(t, orch.SystemService.SetSecuritySettings(ctx, biz.SecuritySettings{
		BlockedIPs:                  []string{},
		ShowRequestLogIPBanIcon:     true,
		CyberSessionBlockEnabled:    true,
		CyberSessionBlockTTLSeconds: 60,
	}))
	orch.SystemService.CyberSessionBlockCache = xcache.NewNoop[biz.CyberSessionBlockEntry]()
	uncached := buildTestRequest("gpt-4", "uncached refusal", false)
	uncached.Headers.Set("Session-Id", "cache-write-fails")
	_, err = orch.Process(requestCtx, uncached)
	require.Error(t, err)
	require.EqualValues(t, 6, executor.requestCalls.Load(), "an observed refusal must stop this request's retries even when cache storage misses")
	_, err = orch.Process(requestCtx, uncached)
	require.Error(t, err)
	require.EqualValues(t, 7, executor.requestCalls.Load(), "cache uncertainty must fail open for a later request")

}

func TestChatCompletionOrchestrator_CyberStreamBlocksNextCall(t *testing.T) {
	ctx := authz.WithTestBypass(context.Background())
	client := enttest.NewEntClient(t, "sqlite3", "file:cyber-session-stream-block?mode=memory&_fk=0")
	defer client.Close()
	ctx = ent.NewContext(ctx, client)

	project := createTestProject(t, ctx, client)
	apiKey := client.APIKey.Create().SetName("stream-key").SetKey("ah-stream").SetProjectID(project.ID).SaveX(ctx)
	channel := createTestChannel(t, ctx, client)
	executor := &mockExecutor{streamEvents: []*httpclient.StreamEvent{
		{Type: "error", Data: []byte(`{"error":{"code":"cyber_policy","message":"refused"}}`)},
	}}
	outbound, err := openai.NewOutboundTransformer(channel.BaseURL, channel.Credentials.APIKey)
	require.NoError(t, err)
	orch, _ := newFailedResponsePersistenceOrchestrator(t, ctx, client, channel, outbound, executor)
	require.NoError(t, orch.SystemService.SetSecuritySettings(ctx, biz.SecuritySettings{
		BlockedIPs:                  []string{},
		ShowRequestLogIPBanIcon:     true,
		CyberSessionBlockEnabled:    true,
		CyberSessionBlockTTLSeconds: 60,
	}))

	requestCtx := contexts.WithAPIKey(contexts.WithProjectID(ctx, project.ID), apiKey)
	first := buildTestRequest("gpt-4", "stream prompt", true)
	first.Headers.Set("Session-Id", "stream-conversation")
	_, err = orch.Process(requestCtx, first)
	require.Error(t, err)
	_, firstMark, ok := cyberSessionCacheKeys(&PersistenceState{APIKey: apiKey}, &llm.Request{RawRequest: first})
	require.True(t, ok)
	requireCyberSessionBlocked(t, orch.SystemService, firstMark)

	second := buildTestRequest("gpt-4", "next turn", false)
	second.Headers.Set("Session-Id", "stream-conversation")
	_, err = orch.Process(requestCtx, second)
	require.Error(t, err)
	require.EqualValues(t, 1, executor.requestCalls.Load(), "the local block must prevent a second executor call")
}

func cyberTestPersistenceState(projectID, apiKeyID int) *PersistenceState {
	return &PersistenceState{APIKey: &ent.APIKey{ID: apiKeyID, ProjectID: projectID}}
}

func cyberLLMRequest(body string) *llm.Request {
	return &llm.Request{RawRequest: &httpclient.Request{Body: []byte(body), Headers: make(http.Header)}}
}

func newCyberCacheOnlySystemService() *biz.SystemService {
	return biz.NewSystemService(biz.SystemServiceParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
	})
}

func requireCyberSessionBlocked(t *testing.T, service *biz.SystemService, key string) {
	t.Helper()
	require.Eventually(t, func() bool {
		_, blocked := service.CyberSessionBlock(context.Background(), key)
		return blocked
	}, time.Second, 5*time.Millisecond)
}
