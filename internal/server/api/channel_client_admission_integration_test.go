package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/gql"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func TestChannelClientAdmissionActualPipelineAndGraphQL(t *testing.T) {
	gin.SetMode(gin.TestMode)

	client := enttest.NewEntClient(t, "sqlite3", "file:cpr-admission-"+uuid.NewString()+"?mode=memory&_fk=1")
	defer client.Close()

	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
	proj := client.Project.Create().SetName("cpr-project").SetStatus(project.StatusActive).SaveX(ctx)
	owner := client.User.Create().
		SetEmail(uuid.NewString() + "@example.com").
		SetPassword("test").
		SetStatus(user.StatusActivated).
		SetIsOwner(true).
		SaveX(ctx)
	key := client.APIKey.Create().
		SetName("cpr-client-key").
		SetKey("ah-cpr-admission-test").
		SetType(apikey.TypeUser).
		SetStatus(apikey.StatusEnabled).
		SetProjectID(proj.ID).
		SetUserID(owner.ID).
		SaveX(ctx)
	client.DataStorage.Create().
		SetName("primary").
		SetDescription("test primary").
		SetType(datastorage.TypeDatabase).
		SetPrimary(true).
		SetStatus(datastorage.StatusActive).
		SetSettings(&objects.DataStorageSettings{}).
		SaveX(ctx)

	cache := xcache.Config{Mode: xcache.ModeMemory}
	system := biz.NewSystemService(biz.SystemServiceParams{CacheConfig: cache, Ent: client})
	require.NoError(t, system.SetModelSettings(ctx, biz.SystemModelSettings{
		FallbackToChannelsOnModelNotFound: true,
		QueryAllChannelModels:             true,
	}))
	require.NoError(t, system.SetRetryPolicy(ctx, &biz.RetryPolicy{Enabled: true, MaxChannelRetries: 1, MaxSingleChannelRetries: 1}))
	require.NoError(t, system.SetStoragePolicy(ctx, &biz.StoragePolicy{StoreRequestBody: true, StoreResponseBody: true, StoreChunks: true}))

	projectService := biz.NewProjectService(biz.ProjectServiceParams{CacheConfig: cache, Ent: client})
	keyService := biz.NewAPIKeyService(biz.APIKeyServiceParams{CacheConfig: cache, Ent: client, ProjectService: projectService, KeyPrefix: "ah"})
	defer keyService.Stop()
	auth := &biz.AuthService{APIKeyService: keyService}

	type providerCall struct {
		path      string
		userAgent string
		customHdr string
		body      []byte
	}
	var providerCalls atomic.Int32
	receivedCalls := make(chan providerCall, 16)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := providerCalls.Add(1)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		receivedCalls <- providerCall{
			path:      r.URL.Path,
			userAgent: r.Header.Get("User-Agent"),
			customHdr: r.Header.Get("X-Preserve-Header"),
			body:      body,
		}
		if strings.HasSuffix(r.URL.Path, "/responses/compact") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"cmp_%d","object":"response.compaction","created_at":1,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"compacted"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`, n)
			return
		}
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		if stream, _ := payload["stream"].(bool); stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_%d\",\"object\":\"response\",\"model\":\"gpt-6.1-sol\",\"status\":\"completed\",\"created_at\":1,\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg_%d\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n", n, n)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"resp_%d","object":"response","model":"gpt-6.1-sol","status":"completed","created_at":1,"output":[{"type":"message","role":"assistant","id":"msg_%d","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`, n, n)
	}))
	defer upstream.Close()

	// Seed channel with both a legacy User-Agent override and another custom header override,
	// mirroring live channel state before managed GraphQL activation.
	chEntity := client.Channel.Create().
		SetType(channel.TypeOpenaiResponses).
		SetName("MyRs-Test").
		SetBaseURL(upstream.URL + "/v1").
		SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"upstream-cpr-key"}}).
		SetSupportedModels([]string{"gpt-6.1-sol"}).
		SetDefaultTestModel("gpt-6.1-sol").
		SetPolicies(objects.ChannelPolicies{
			Stream: objects.CapabilityPolicyUnlimited,
		}).
		SetSettings(&objects.ChannelSettings{
			PassThroughUserAgent: lo.ToPtr(true),
			PassThroughBody:      lo.ToPtr(true),
			DisableRetries:       true,
			TransformOptions: objects.TransformOptions{
				CodexAgentToolAliases: true,
			},
			HeaderOverrideOperations: []objects.OverrideOperation{
				{
					Op:    objects.OverrideOpSet,
					Path:  "User-Agent",
					Value: "Codex Desktop/0.142.3 (Windows 10.0.22635; x86_64) unknown (Codex Desktop; 26.623.61825)",
				},
				{
					Op:    objects.OverrideOpSet,
					Path:  "X-Preserve-Header",
					Value: "keep-me",
				},
			},
		}).
		SaveX(ctx)

	httpClient := httpclient.NewHttpClient()
	channelService := biz.NewChannelService(biz.ChannelServiceParams{
		CacheConfig:   cache,
		Ent:           client,
		SystemService: system,
		HttpClient:    httpClient,
	})
	defer channelService.Stop()
	modelService := biz.NewModelService(biz.ModelServiceParams{Ent: client, ChannelService: channelService, SystemService: system})
	defaultSelector := orchestrator.NewDefaultSelector(channelService, modelService, system)
	usage := biz.NewUsageLogService(client, system, channelService)
	storage := &biz.DataStorageService{AbstractService: &biz.AbstractService{}, SystemService: system, Cache: xcache.NewFromConfig[ent.DataStorage](cache)}
	requests := biz.NewRequestService(client, system, usage, storage, biz.NewLiveStreamRegistry())
	limiter := orchestrator.NewChannelLimiterManager()

	chatProcessor := orchestrator.NewChatCompletionOrchestrator(
		channelService, defaultSelector, requests, httpClient,
		openai.NewInboundTransformer(), system, usage, nil, nil, nil, nil, limiter, nil,
	)
	chatProcessor.PromptProtecter = nil
	chatProcessor.PromptProvider = wsPromptProvider{}

	responsesProcessor := orchestrator.NewChatCompletionOrchestrator(
		channelService, defaultSelector, requests, httpClient,
		responses.NewInboundTransformer(), system, usage, nil, nil, nil, nil, limiter, nil,
	)
	responsesProcessor.PromptProtecter = nil
	responsesProcessor.PromptProvider = wsPromptProvider{}

	compactProcessor := orchestrator.NewChatCompletionOrchestrator(
		channelService, defaultSelector, requests, httpClient,
		responses.NewCompactInboundTransformer(), system, usage, nil, nil, nil, nil, limiter, nil,
	)
	compactProcessor.PromptProtecter = nil
	compactProcessor.PromptProvider = wsPromptProvider{}

	wsHandler := newResponsesWebSocketHandler(&ChatCompletionHandlers{ChatCompletionOrchestrator: responsesProcessor}, auth)

	gqlHandlers := gql.NewGraphqlHandlers(gql.Dependencies{
		Ent:             client,
		AuthService:     auth,
		APIKeyService:   keyService,
		SystemService:   system,
		ChannelService:  channelService,
		RequestService:  requests,
		ProjectService:  projectService,
		UsageLogService: usage,
		ModelService:    modelService,
		DefaultSelector: defaultSelector,
		HttpClient:      httpClient,
	})

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Request = c.Request.WithContext(ent.NewContext(c.Request.Context(), client))
	})
	router.POST("/admin/graphql", func(c *gin.Context) {
		reqCtx := authz.WithTestBypass(c.Request.Context())
		c.Request = c.Request.WithContext(reqCtx)
		gqlHandlers.Graphql.ServeHTTP(c.Writer, c.Request)
	})
	apiGroup := router.Group("")
	apiGroup.Use(middleware.WithAPIKeyConfig(auth, nil))
	apiGroup.GET("/v1/responses", wsHandler.Serve)
	apiGroup.POST("/v1/responses", (&ChatCompletionHandlers{ChatCompletionOrchestrator: responsesProcessor}).ChatCompletion)
	apiGroup.POST("/v1/responses/compact", (&ChatCompletionHandlers{ChatCompletionOrchestrator: compactProcessor}).ChatCompletion)
	apiGroup.POST("/v1/chat/completions", (&ChatCompletionHandlers{ChatCompletionOrchestrator: chatProcessor}).ChatCompletion)

	server := httptest.NewServer(router)
	defer server.Close()

	// 1. Activate admission policy and remove ONLY the User-Agent override via canonical GraphQL updateChannel
	channelGlobalID := fmt.Sprintf("gid://axonhub/Channel/%d", chEntity.ID)
	execGraphQL := func(query string, variables map[string]any) map[string]any {
		payload, err := json.Marshal(map[string]any{
			"query":     query,
			"variables": variables,
		})
		require.NoError(t, err)
		resp, err := http.Post(server.URL+"/admin/graphql", "application/json", bytes.NewReader(payload))
		require.NoError(t, err)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		var out map[string]any
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		require.Empty(t, out["errors"], "unexpected GraphQL errors: %v", out["errors"])
		data, _ := out["data"].(map[string]any)
		return data
	}

	updateMutation := `mutation UpdateChannel($id: ID!, $input: UpdateChannelInput!) {
		updateChannel(id: $id, input: $input) {
			id
			name
			policies {
				stream
				allowRequestFormats
				denyRequestFormats
				codexOfficialClient
			}
			settings {
				passThroughUserAgent
				passThroughBody
				disableRetries
				transformOptions {
					codexAgentToolAliases
				}
				headerOverrideOperations {
					op
					path
					value
				}
			}
		}
	}`

	data := execGraphQL(updateMutation, map[string]any{
		"id": channelGlobalID,
		"input": map[string]any{
			"policies": map[string]any{
				"stream":              "unlimited",
				"allowRequestFormats": []string{"openai/responses", "openai/responses_compact"},
				"codexOfficialClient": true,
			},
			"settings": map[string]any{
				"passThroughUserAgent": true,
				"passThroughBody":      true,
				"disableRetries":       true,
				"transformOptions": map[string]any{
					"codexAgentToolAliases": true,
				},
				"headerOverrideOperations": []map[string]any{
					{
						"op":    "set",
						"path":  "X-Preserve-Header",
						"value": "keep-me",
					},
				},
			},
		},
	})
	updatedNode, _ := data["updateChannel"].(map[string]any)
	require.Equal(t, channelGlobalID, updatedNode["id"])

	// 2. Partial GraphQL update of policies.stream only must preserve allowRequestFormats and codexOfficialClient
	partialData := execGraphQL(updateMutation, map[string]any{
		"id": channelGlobalID,
		"input": map[string]any{
			"policies": map[string]any{
				"stream": "unlimited",
			},
		},
	})
	partialNode, _ := partialData["updateChannel"].(map[string]any)
	partialPolicies, _ := partialNode["policies"].(map[string]any)
	require.Equal(t, []any{"openai/responses", "openai/responses_compact"}, partialPolicies["allowRequestFormats"])
	require.Equal(t, true, partialPolicies["codexOfficialClient"])
	partialSettings, _ := partialNode["settings"].(map[string]any)
	require.Equal(t, true, partialSettings["passThroughUserAgent"])
	headerOps, _ := partialSettings["headerOverrideOperations"].([]any)
	require.Len(t, headerOps, 1)

	require.Eventually(t, func() bool {
		loaded := channelService.GetEnabledChannels()
		return len(loaded) == 1 &&
			loaded[0].Policies.RequiresCodexOfficialClient() &&
			len(loaded[0].GetHeaderOverrideOperations()) == 1
	}, 2*time.Second, 10*time.Millisecond)

	const codexCLIUA = "codex_cli_rs/0.159.2 (Windows 10.0.26200; x86_64)"

	// 3. Real WebSocket with official Codex UA succeeds and forwards byte-identical UA + preserved custom header to provider
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	goodConn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"Authorization": []string{"Bearer " + key.Key},
		"User-Agent":    []string{codexCLIUA},
	})
	require.NoError(t, err)
	defer goodConn.Close()

	require.NoError(t, goodConn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6.1-sol","input":"hello from codex ws"}`)))
	wsEvent := wsRead(t, goodConn)
	require.Equal(t, "response.completed", wsEventString(wsEvent, "type"))
	require.Equal(t, int32(1), providerCalls.Load())
	call1 := <-receivedCalls
	require.Equal(t, "/v1/responses", call1.path)
	require.Equal(t, codexCLIUA, call1.userAgent)
	require.Equal(t, "keep-me", call1.customHdr)

	// 4. Real WebSocket with non-Codex UA (including spoofed Originator and unknown codex_ trailer)
	// never touches provider and returns execution: "not_executed"
	badConn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{
		"Authorization": []string{"Bearer " + key.Key},
		"User-Agent":    []string{"curl/8.7.1 (codex_unknown; 1.0.0)"},
		"Originator":    []string{"codex_cli_rs"},
	})
	require.NoError(t, err)
	defer badConn.Close()

	require.NoError(t, badConn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6.1-sol","input":"blocked ws"}`)))
	wsRejected := wsRead(t, badConn)
	require.Equal(t, "error", wsEventString(wsRejected, "type"))
	require.Equal(t, "client_not_allowed", wsErrorCode(wsRejected))
	require.Equal(t, "not_executed", wsEventString(wsRejected, "execution"))
	require.Equal(t, int32(1), providerCalls.Load(), "rejected WS frame must not touch upstream provider")

	// 5. Real HTTP /v1/chat/completions with official Codex UA is rejected BEFORE format conversion / upstream send
	chatReq, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"hi"}]}`))
	require.NoError(t, err)
	chatReq.Header.Set("Authorization", "Bearer "+key.Key)
	chatReq.Header.Set("Content-Type", "application/json")
	chatReq.Header.Set("User-Agent", codexCLIUA)
	chatResp, err := http.DefaultClient.Do(chatReq)
	require.NoError(t, err)
	chatBody, err := io.ReadAll(chatResp.Body)
	require.NoError(t, err)
	require.NoError(t, chatResp.Body.Close())
	require.Equal(t, http.StatusBadRequest, chatResp.StatusCode, string(chatBody))
	require.Contains(t, string(chatBody), "unsupported_request_format")
	require.Equal(t, int32(1), providerCalls.Load(), "rejected HTTP chat request must not touch upstream provider")

	// 6. Real HTTP /v1/responses/compact with official Codex UA succeeds and forwards byte-identical UA to provider
	compactReq, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses/compact", strings.NewReader(`{"model":"gpt-6.1-sol","input":"compact this"}`))
	require.NoError(t, err)
	compactReq.Header.Set("Authorization", "Bearer "+key.Key)
	compactReq.Header.Set("Content-Type", "application/json")
	compactReq.Header.Set("User-Agent", codexCLIUA)
	compactResp, err := http.DefaultClient.Do(compactReq)
	require.NoError(t, err)
	compactBody, err := io.ReadAll(compactResp.Body)
	require.NoError(t, err)
	require.NoError(t, compactResp.Body.Close())
	require.Equal(t, http.StatusOK, compactResp.StatusCode, string(compactBody))
	require.Contains(t, string(compactBody), "response.compaction")
	require.Equal(t, int32(2), providerCalls.Load())
	call2 := <-receivedCalls
	require.Equal(t, "/v1/responses/compact", call2.path)
	require.Equal(t, codexCLIUA, call2.userAgent)
	require.Equal(t, "keep-me", call2.customHdr)
}

func TestCPRAdmissionActivationScriptLiveGraphQLCheckApplyCheck(t *testing.T) {
	scriptPath := strings.TrimSpace(os.Getenv("AXON_CPR_ADMISSION_TEST_SCRIPT"))
	if scriptPath == "" {
		t.Skip("skipping cross-repo activation script harness: set AXON_CPR_ADMISSION_TEST_SCRIPT to run")
	}
	scriptInfo, err := os.Stat(scriptPath)
	require.NoError(t, err, "AXON_CPR_ADMISSION_TEST_SCRIPT is set but invalid: %s", scriptPath)
	require.False(t, scriptInfo.IsDir(), "AXON_CPR_ADMISSION_TEST_SCRIPT must point to a file: %s", scriptPath)

	var pythonBin string
	for _, candidate := range []string{"python", "python3"} {
		bin, lookErr := exec.LookPath(candidate)
		if lookErr != nil {
			continue
		}
		probeCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		probeErr := exec.CommandContext(probeCtx, bin, "-c", "import sys").Run()
		cancel()
		if probeErr == nil {
			pythonBin = bin
			break
		}
	}
	require.NotEmpty(t, pythonBin, "working python/python3 executable is required when AXON_CPR_ADMISSION_TEST_SCRIPT is set")

	gin.SetMode(gin.TestMode)

	client := enttest.NewEntClient(t, "sqlite3", "file:cpr-script-e2e-"+uuid.NewString()+"?mode=memory&_fk=1")
	defer client.Close()

	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
	cache := xcache.Config{Mode: xcache.ModeMemory}
	system := biz.NewSystemService(biz.SystemServiceParams{CacheConfig: cache, Ent: client})
	projectService := biz.NewProjectService(biz.ProjectServiceParams{CacheConfig: cache, Ent: client})
	keyService := biz.NewAPIKeyService(biz.APIKeyServiceParams{CacheConfig: cache, Ent: client, ProjectService: projectService, KeyPrefix: "ah"})
	defer keyService.Stop()
	auth := &biz.AuthService{APIKeyService: keyService}
	httpClient := httpclient.NewHttpClient()

	makeRichSettings := func(includeUAOverride bool) *objects.ChannelSettings {
		headerOps := []objects.OverrideOperation{
			{
				Op:        objects.OverrideOpSet,
				Path:      "X-Custom-Tenant",
				Value:     "secret-tenant-token-do-not-echo",
				Condition: "model == 'gpt-6.1-sol'",
			},
			{
				Op:   objects.OverrideOpCopy,
				From: "User-Agent",
				To:   "X-Original-UA",
			},
		}
		if includeUAOverride {
			headerOps = append([]objects.OverrideOperation{
				{
					Op:    objects.OverrideOpSet,
					Path:  "User-Agent",
					Value: "Codex Desktop/0.142.3 (Windows 10.0.22635; x86_64) unknown (Codex Desktop; 26.623.61825)",
				},
			}, headerOps...)
		}
		idx := -1
		return &objects.ChannelSettings{
			ExtraModelPrefix:        "cpr",
			ModelMappings:           []objects.ModelMapping{{From: "gpt-6-sol", To: "gpt-6.1-sol"}},
			AutoTrimedModelPrefixes: []string{"openai"},
			HideOriginalModels:      true,
			HideMappedModels:        false,
			LowercaseModelID:        true,
			Proxy: &httpclient.ProxyConfig{
				Type:                   httpclient.ProxyTypeURL,
				URL:                    "http://proxy-user:super-secret-pass@10.0.0.1:8080",
				Username:               "proxy-user",
				Password:               "super-secret-pass",
				DisableConnectionReuse: true,
			},
			TransformOptions: objects.TransformOptions{
				ForceArrayInstructions:         true,
				ForceArrayInputs:               false,
				ReplaceDeveloperRoleWithSystem: false,
				CodexAgentToolAliases:          true,
				ReasoningEffortMapping:         []llm.ReasoningEffortMapping{{From: "xhigh", To: "high"}},
			},
			HeaderOverrideOperations: headerOps,
			BodyOverrideOperations: []objects.OverrideOperation{
				{
					Op:        objects.OverrideOpArrayRemove,
					Path:      "tools",
					Condition: "true",
					Match:     &objects.OverrideMatch{Path: "type", Eq: "web_search"},
					Index:     &idx,
					Splat:     lo.ToPtr(false),
				},
			},
			PassThroughUserAgent:       lo.ToPtr(true),
			PassThroughBody:            lo.ToPtr(true),
			DisableRetries:             true,
			FullPassThrough:            false,
			StoreExecutionRequestBody:  lo.ToPtr(true),
			StoreExecutionResponseBody: lo.ToPtr(false),
			StoreExecutionStreamChunks: lo.ToPtr(true),
			SSEKeepAlive: &objects.ChannelSSEKeepAlive{
				Enabled:         lo.ToPtr(true),
				IntervalSeconds: lo.ToPtr(15),
			},
			RateLimit: &objects.ChannelRateLimit{
				RPM:            lo.ToPtr(int64(600)),
				TPM:            lo.ToPtr(int64(120000)),
				MaxConcurrent:  lo.ToPtr(int64(32)),
				QueueSize:      lo.ToPtr(int64(64)),
				QueueTimeoutMs: lo.ToPtr(int64(5000)),
			},
			RetryableStatusCodes:   []int{408, 502, 504},
			RetryableErrorPatterns: []objects.RetryableErrorPattern{{Pattern: "upstream_overloaded", Regex: false}},
			ProviderQuota: &objects.ChannelProviderQuotaSettings{
				OpencodeGo: &objects.OpenCodeGoQuotaSettings{
					WorkspaceID: "ws-123",
					AuthCookie:  "cookie-secret-val",
				},
			},
			KeySelection: &objects.ChannelKeySelection{
				Strategy:                 objects.ChannelKeySelectionStrategyCacheAffinity,
				LikelyAffinityTTLMinutes: lo.ToPtr(10),
				ExactAffinityTTLMinutes:  lo.ToPtr(30),
			},
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled:           true,
				Preset:            objects.ChannelBalanceProbePresetCustom,
				Experimental:      false,
				PreferredCurrency: "USD",
				PrimarySelection:  objects.ChannelBalancePrimarySelectionPreferredCurrency,
				IncludeStatuses:   []objects.ChannelKeyStatus{objects.ChannelKeyStatusActive},
				TimeoutMs:         3000,
			},
			FailurePolicy: &objects.ChannelFailurePolicy{
				Mode: objects.ChannelFailurePolicyModeOverride,
				KeyProfiles: []objects.FailurePolicyProfile{
					{
						ID:                "fp-1",
						Name:              "401-disable",
						Enabled:           lo.ToPtr(true),
						Sources:           []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
						ConditionCombiner: objects.FailurePolicyConditionCombinerAnd,
						Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
							MinFailureCount:      lo.ToPtr(2),
							Success:              lo.ToPtr(false),
							StatusCodes:          []int{401},
							ReasonContains:       "invalid_api_key",
							AllCheckedKeysFailed: lo.ToPtr(false),
							KeyStatuses:          []objects.ChannelKeyStatus{objects.ChannelKeyStatusActive},
						},
						Actions: []objects.FailurePolicyAction{
							{
								Type: objects.FailurePolicyActionBackoffKey,
								Backoff: &objects.ChannelKeyHealthCheckBackoff{
									Mode:               objects.ChannelKeyHealthCheckBackoffModeExponential,
									IntervalMinutes:    5,
									MaxIntervalMinutes: 60,
									Multiplier:         2.0,
								},
							},
						},
					},
				},
			},
		}
	}

	// Seed channels up to ID 65 so exact IDs 52, 56, 60, 65 exist in SQLite with real configurations
	for id := 1; id <= 65; id++ {
		switch id {
		case 52:
			ch := client.Channel.Create().
				SetType(channel.TypeOpenaiResponses).
				SetName("MyRs").
				SetBaseURL("http://10.77.48.4:8080/v1").
				SetStatus(channel.StatusDisabled).
				SetCredentials(objects.ChannelCredentials{APIKeys: []string{"test-key-52"}}).
				SetSupportedModels([]string{"gpt-6.1-sol"}).
				SetDefaultTestModel("gpt-6.1-sol").
				SetPolicies(objects.ChannelPolicies{Stream: objects.CapabilityPolicyUnlimited}).
				SetSettings(makeRichSettings(false)).
				SaveX(ctx)
			require.Equal(t, 52, ch.ID)
		case 56, 60, 65:
			pol := objects.ChannelPolicies{Stream: objects.CapabilityPolicyUnlimited}
			if id == 60 {
				pol.Stream = objects.CapabilityPolicyRequire
			}
			if id == 65 {
				pol.DenyRequestFormats = []string{"anthropic/messages"}
			}
			var settings *objects.ChannelSettings
			if id == 60 {
				// Exercise sparse/null nested settings alongside User-Agent override + copy-out
				settings = &objects.ChannelSettings{
					PassThroughUserAgent: lo.ToPtr(false),
					PassThroughBody:      lo.ToPtr(true),
					DisableRetries:       true,
					HeaderOverrideOperations: []objects.OverrideOperation{
						{
							Op:    objects.OverrideOpSet,
							Path:  "User-Agent",
							Value: "Codex Desktop/0.142.3 (Windows 10.0.22635; x86_64) unknown (Codex Desktop; 26.623.61825)",
						},
						{
							Op:   objects.OverrideOpCopy,
							From: "User-Agent",
							To:   "X-Original-UA",
						},
					},
				}
			} else {
				settings = makeRichSettings(true)
			}
			ch := client.Channel.Create().
				SetType(channel.TypeOpenaiResponses).
				SetName(fmt.Sprintf("MyRs-%d", id)).
				SetBaseURL("http://10.77.48.4:8080/v1").
				SetStatus(channel.StatusEnabled).
				SetCredentials(objects.ChannelCredentials{APIKeys: []string{fmt.Sprintf("test-key-%d", id)}}).
				SetSupportedModels([]string{"gpt-6.1-sol"}).
				SetDefaultTestModel("gpt-6.1-sol").
				SetPolicies(pol).
				SetSettings(settings).
				SaveX(ctx)
			require.Equal(t, id, ch.ID)
		default:
			ch := client.Channel.Create().
				SetType(channel.TypeOpenai).
				SetName(fmt.Sprintf("filler-%d", id)).
				SetBaseURL("http://127.0.0.1:9999/v1").
				SetStatus(channel.StatusDisabled).
				SetCredentials(objects.ChannelCredentials{APIKeys: []string{"filler"}}).
				SetSupportedModels([]string{"m"}).
				SetDefaultTestModel("m").
				SaveX(ctx)
			require.Equal(t, id, ch.ID)
		}
	}

	channelService := biz.NewChannelService(biz.ChannelServiceParams{
		CacheConfig:   cache,
		Ent:           client,
		SystemService: system,
		HttpClient:    httpClient,
	})
	defer channelService.Stop()
	modelService := biz.NewModelService(biz.ModelServiceParams{Ent: client, ChannelService: channelService, SystemService: system})
	defaultSelector := orchestrator.NewDefaultSelector(channelService, modelService, system)
	usage := biz.NewUsageLogService(client, system, channelService)
	storage := &biz.DataStorageService{AbstractService: &biz.AbstractService{}, SystemService: system, Cache: xcache.NewFromConfig[ent.DataStorage](cache)}
	requests := biz.NewRequestService(client, system, usage, storage, biz.NewLiveStreamRegistry())

	gqlHandlers := gql.NewGraphqlHandlers(gql.Dependencies{
		Ent:             client,
		AuthService:     auth,
		APIKeyService:   keyService,
		SystemService:   system,
		ChannelService:  channelService,
		RequestService:  requests,
		ProjectService:  projectService,
		UsageLogService: usage,
		ModelService:    modelService,
		DefaultSelector: defaultSelector,
		HttpClient:      httpClient,
	})

	const syntheticOwnerToken = "synthetic-owner-jwt-for-local-test"
	router := gin.New()
	router.POST("/admin/graphql", func(c *gin.Context) {
		if c.GetHeader("Authorization") != "Bearer "+syntheticOwnerToken {
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		reqCtx := ent.NewContext(authz.WithTestBypass(c.Request.Context()), client)
		c.Request = c.Request.WithContext(reqCtx)
		gqlHandlers.Graphql.ServeHTTP(c.Writer, c.Request)
	})

	server := httptest.NewServer(router)
	defer server.Close()

	jwtPath := filepath.Join(t.TempDir(), "owner.jwt")
	require.NoError(t, os.WriteFile(jwtPath, []byte(syntheticOwnerToken+"\n"), 0o600))

	filteredEnv := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "AXON_OWNER_JWT", "AXON_OWNER_JWT_PATH", "AXON_ORIGIN":
			continue
		default:
			filteredEnv = append(filteredEnv, entry)
		}
	}

	runScript := func(extraArgs ...string) map[string]any {
		cmdCtx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
		defer cancel()
		args := append([]string{scriptPath, "--origin", server.URL, "--jwt-path", jwtPath, "--token-env", ""}, extraArgs...)
		cmd := exec.CommandContext(cmdCtx, pythonBin, args...)
		cmd.Env = filteredEnv
		outBytes, err := cmd.CombinedOutput()
		require.NoError(t, err, "script failed: %s", string(outBytes))
		outStr := string(outBytes)
		require.NotContains(t, outStr, "super-secret-pass")
		require.NotContains(t, outStr, "secret-tenant-token-do-not-echo")
		require.NotContains(t, outStr, "cookie-secret-val")
		require.NotContains(t, outStr, syntheticOwnerToken)
		var parsed map[string]any
		require.NoError(t, json.Unmarshal(outBytes, &parsed), "invalid json: %s", outStr)
		return parsed
	}

	// Step 1: check (read-only) against real GraphQL __schema and seeded SQLite rows
	checkBefore := runScript("--mode", "check")
	require.Equal(t, "checked", checkBefore["phase"])
	require.Equal(t, true, checkBefore["schemaVerified"])
	channelsBefore, _ := checkBefore["channels"].([]any)
	require.Len(t, channelsBefore, 4)

	// Step 2: apply against real GraphQL updateChannel and readback verification
	applyRes := runScript("--apply")
	require.Equal(t, "verified", applyRes["phase"])
	require.Equal(t, []any{float64(56), float64(60), float64(65)}, applyRes["updatedChannelIds"])

	// Step 3: second check after apply must report noop (needsWrite == false) for all channels
	checkAfter := runScript("--mode", "check")
	require.Equal(t, "checked", checkAfter["phase"])
	channelsAfter, _ := checkAfter["channels"].([]any)
	require.Len(t, channelsAfter, 4)
	for _, item := range channelsAfter {
		chMap, _ := item.(map[string]any)
		require.Equal(t, false, chMap["needsWrite"], "expected needsWrite=false after apply for %v", chMap)
	}

	// Verify DB state directly: 52 still disabled, 56/60/65 have exact policies and preserved non-UA header overrides
	ch52 := client.Channel.GetX(ctx, 52)
	require.Equal(t, channel.StatusDisabled, ch52.Status)

	for _, cid := range []int{56, 60, 65} {
		row := client.Channel.GetX(ctx, cid)
		require.Equal(t, channel.StatusEnabled, row.Status)
		require.Equal(t, []string{"openai/responses", "openai/responses_compact"}, row.Policies.AllowRequestFormats)
		require.NotNil(t, row.Policies.CodexOfficialClient)
		require.True(t, *row.Policies.CodexOfficialClient)
		require.NotNil(t, row.Settings.PassThroughUserAgent)
		require.True(t, *row.Settings.PassThroughUserAgent)
		for _, op := range row.Settings.HeaderOverrideOperations {
			require.NotEqual(t, "user-agent", strings.ToLower(op.Path))
		}
	}
	ch56 := client.Channel.GetX(ctx, 56)
	require.Len(t, ch56.Settings.HeaderOverrideOperations, 2)
	require.Equal(t, "X-Custom-Tenant", ch56.Settings.HeaderOverrideOperations[0].Path)
	require.Equal(t, "copy", ch56.Settings.HeaderOverrideOperations[1].Op)
	require.Equal(t, "User-Agent", ch56.Settings.HeaderOverrideOperations[1].From)
	require.Equal(t, "X-Original-UA", ch56.Settings.HeaderOverrideOperations[1].To)
	require.Equal(t, "http://proxy-user:super-secret-pass@10.0.0.1:8080", ch56.Settings.Proxy.URL)
	require.Equal(t, "ws-123", ch56.Settings.ProviderQuota.OpencodeGo.WorkspaceID)
	require.Equal(t, int64(32), *ch56.Settings.RateLimit.MaxConcurrent)
}
