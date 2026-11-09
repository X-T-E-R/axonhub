package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/gemini"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

func setupErrorPassthroughHandler(
	t *testing.T,
	ctx context.Context,
	client *ent.Client,
	chType channel.Type,
	baseURL string,
	passThroughBody bool,
	inbound transformer.Inbound,
	outbound transformer.Outbound,
	retryPolicy *biz.RetryPolicy,
) *ChatCompletionHandlers {
	t.Helper()
	row := client.Channel.Create().
		SetType(chType).
		SetName("error-passthrough-fixture").
		SetBaseURL(baseURL).
		SetCredentials(objects.ChannelCredentials{APIKey: "fixture-key"}).
		SetSupportedModels([]string{"fixture-model"}).
		SetDefaultTestModel("fixture-model").
		SetSettings(&objects.ChannelSettings{PassThroughBody: lo.ToPtr(passThroughBody)}).
		SaveX(ctx)

	channels, requests, system, usage := setupSpeechAPITestServices(t, client)
	if retryPolicy == nil {
		retryPolicy = &biz.RetryPolicy{Enabled: false}
	}
	require.NoError(t, system.SetRetryPolicy(ctx, retryPolicy))

	orch := orchestrator.NewChatCompletionOrchestrator(
		channels,
		nil,
		requests,
		httpclient.NewHttpClient(),
		inbound,
		system,
		usage,
		nil,
		nil,
		nil,
		requests.LiveStreamRegistry,
		orchestrator.NewChannelLimiterManager(),
		nil,
	).WithChannelSelector(&speechAPISelector{
		candidates: []*orchestrator.ChannelModelsCandidate{{
			Channel: &biz.Channel{Channel: row, Outbound: outbound},
			Models:  []biz.ChannelModelEntry{{RequestModel: "fixture-model", ActualModel: "fixture-model", Source: "direct"}},
		}},
	})
	orch.PromptProvider = observationNoPrompts{}
	orch.PromptProtecter = nil
	return NewChatCompletionHandlers(orch)
}

func TestErrorPassthrough_SameProtocolAnthropicPreservesBodyUnknownFieldsAndHeaders(t *testing.T) {
	const rawErrorBody = `{
		"type": "error",
		"error": {
			"type": "invalid_request_error",
			"code": "invalid_tool_result",
			"message": "messages.3.content.0: unexpected tool_use_id",
			"param": "messages.3",
			"details": [{"reason":"TOOL_MISMATCH","extra":42}],
			"unknown_vendor_nested": {"keep": true}
		},
		"request_id": "req_anth_body_001",
		"trace_id": "tr_anth_001",
		"unknown_top_level": "survives"
	}`

	for _, passThroughBody := range []bool{false, true} {
		t.Run(fmt.Sprintf("passThroughBody=%v", passThroughBody), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Request-Id", "req_anth_hdr_001")
				w.Header().Set("X-Request-Id", "xreq_anth_hdr_001")
				w.Header().Set("Retry-After", "7")
				w.Header().Set("Set-Cookie", "secret=must-not-leak")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(rawErrorBody))
			}))
			defer upstream.Close()

			client := enttest.NewEntClient(t, "sqlite3", fmt.Sprintf("file:err-pass-anth-%v?mode=memory&_fk=0", passThroughBody))
			defer client.Close()
			ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
			project := client.Project.Create().SetName("p").SaveX(ctx)
			ctx = contexts.WithProjectID(ctx, project.ID)

			outbound, err := anthropic.NewOutboundTransformer(upstream.URL, "fixture-key")
			require.NoError(t, err)
			handler := setupErrorPassthroughHandler(
				t, ctx, client, channel.TypeAnthropic, upstream.URL, passThroughBody,
				anthropic.NewInboundTransformer(), outbound, nil,
			)

			router := gin.New()
			router.POST("/v1/messages", handler.ChatCompletion)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"fixture-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)

			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.JSONEq(t, rawErrorBody, rec.Body.String())
			require.Equal(t, "req_anth_hdr_001", rec.Result().Header.Get("Request-Id"))
			require.Equal(t, "xreq_anth_hdr_001", rec.Result().Header.Get("X-Request-Id"))
			require.Equal(t, "7", rec.Result().Header.Get("Retry-After"))
			require.Empty(t, rec.Result().Header.Get("Set-Cookie"))
		})
	}
}

func TestErrorPassthrough_SameProtocolGeminiPreservesDetailsAndUnknownFields(t *testing.T) {
	const rawGeminiError = `{
		"error": {
			"code": 400,
			"message": "Invalid argument: tool_config",
			"status": "INVALID_ARGUMENT",
			"details": [
				{
					"@type": "type.googleapis.com/google.rpc.BadRequest",
					"fieldViolations": [{"field": "tool_config", "description": "unknown mode"}]
				}
			],
			"vendor_extension": {" node": "g-1"}
		}
	}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_gemini_hdr_1")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(rawGeminiError))
	}))
	defer upstream.Close()

	client := enttest.NewEntClient(t, "sqlite3", "file:err-pass-gemini?mode=memory&_fk=0")
	defer client.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
	project := client.Project.Create().SetName("p").SaveX(ctx)
	ctx = contexts.WithProjectID(ctx, project.ID)

	outbound, err := gemini.NewOutboundTransformer(upstream.URL, "fixture-key")
	require.NoError(t, err)
	handler := setupErrorPassthroughHandler(
		t, ctx, client, channel.TypeGemini, upstream.URL, false,
		gemini.NewInboundTransformer(), outbound, nil,
	)

	router := gin.New()
	router.POST("/v1beta/models/*model", func(c *gin.Context) {
		handler.ChatCompletion(c)
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/fixture-model:generateContent", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.JSONEq(t, rawGeminiError, rec.Body.String())
	require.Equal(t, "req_gemini_hdr_1", rec.Result().Header.Get("X-Request-Id"))
}

func TestErrorPassthrough_CrossProtocolAnthropicToOpenAIPreservesFieldsWithoutRawLeak(t *testing.T) {
	const rawAnthropicError = `{
		"type": "error",
		"error": {
			"type": "invalid_request_error",
			"code": "invalid_prompt",
			"message": "system prompt exceeds limit",
			"param": "system",
			"details": [{"limit": 10000}]
		},
		"request_id": "req_cross_anth_1"
	}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_cross_hdr_1")
		w.Header().Set("Retry-After", "9")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(rawAnthropicError))
	}))
	defer upstream.Close()

	client := enttest.NewEntClient(t, "sqlite3", "file:err-cross-anth-oai?mode=memory&_fk=0")
	defer client.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
	project := client.Project.Create().SetName("p").SaveX(ctx)
	ctx = contexts.WithProjectID(ctx, project.ID)

	outbound, err := anthropic.NewOutboundTransformer(upstream.URL, "fixture-key")
	require.NoError(t, err)
	handler := setupErrorPassthroughHandler(
		t, ctx, client, channel.TypeAnthropic, upstream.URL, false,
		openai.NewInboundTransformer(), outbound, nil,
	)

	router := gin.New()
	router.POST("/v1/chat/completions", handler.ChatCompletion)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"fixture-model","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Equal(t, "req_cross_hdr_1", rec.Result().Header.Get("Request-Id"))
	require.Equal(t, "9", rec.Result().Header.Get("Retry-After"))
	require.JSONEq(t, `{
		"error": {
			"message": "system prompt exceeds limit",
			"type": "invalid_request_error",
			"code": "invalid_prompt",
			"param": "system",
			"request_id": "req_cross_anth_1",
			"details": [{"limit": 10000}]
		}
	}`, rec.Body.String())
}

func TestErrorPassthrough_ExplicitRewritePolicyOverridesRawPassthrough(t *testing.T) {
	const rawErrorBody = `{
		"type": "error",
		"error": {
			"type": "rate_limit_error",
			"code": "provider_rate_limit",
			"message": "sensitive upstream account limit details",
			"secret_field": "must_be_redacted"
		},
		"request_id": "req_policy_123"
	}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "15")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(rawErrorBody))
	}))
	defer upstream.Close()

	client := enttest.NewEntClient(t, "sqlite3", "file:err-policy-override?mode=memory&_fk=0")
	defer client.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
	project := client.Project.Create().SetName("p").SaveX(ctx)
	ctx = contexts.WithProjectID(ctx, project.ID)

	outbound, err := anthropic.NewOutboundTransformer(upstream.URL, "fixture-key")
	require.NoError(t, err)
	handler := setupErrorPassthroughHandler(
		t, ctx, client, channel.TypeAnthropic, upstream.URL, true,
		anthropic.NewInboundTransformer(), outbound,
		&biz.RetryPolicy{
			Enabled: false,
			UpstreamErrorPolicy: biz.UpstreamErrorPolicy{
				Mode:          biz.UpstreamErrorModeCustom,
				CustomMessage: "Custom gateway error message",
			},
		},
	)

	router := gin.New()
	router.POST("/v1/messages", handler.ChatCompletion)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"fixture-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "15", rec.Result().Header.Get("Retry-After"))
	require.NotContains(t, rec.Body.String(), "sensitive upstream account limit details")
	require.NotContains(t, rec.Body.String(), "must_be_redacted")
	require.JSONEq(t, `{
		"type": "rate_limit_error",
		"request_id": "req_policy_123",
		"error": {
			"type": "rate_limit_error",
			"code": "provider_rate_limit",
			"message": "Custom gateway error message"
		}
	}`, rec.Body.String())
}

func TestErrorPassthrough_EmptyOrNonJSONUpstreamBodyProducesStructuredFallback(t *testing.T) {
	for _, tt := range []struct {
		name        string
		upstreamBody string
		wantMessage string
	}{
		{
			name:        "empty body falls back to status text",
			upstreamBody: "",
			wantMessage: "Bad Gateway",
		},
		{
			name:        "plain text body preserved in structured fallback",
			upstreamBody: "upstream gateway timeout: backend unreachable",
			wantMessage: "upstream gateway timeout: backend unreachable",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Request-Id", "req_fallback_1")
				w.WriteHeader(http.StatusBadGateway)
				if tt.upstreamBody != "" {
					_, _ = w.Write([]byte(tt.upstreamBody))
				}
			}))
			defer upstream.Close()

			client := enttest.NewEntClient(t, "sqlite3", fmt.Sprintf("file:err-fallback-%s?mode=memory&_fk=0", strings.ReplaceAll(tt.name, " ", "-")))
			defer client.Close()
			ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
			project := client.Project.Create().SetName("p").SaveX(ctx)
			ctx = contexts.WithProjectID(ctx, project.ID)

			outbound, err := anthropic.NewOutboundTransformer(upstream.URL, "fixture-key")
			require.NoError(t, err)
			handler := setupErrorPassthroughHandler(
				t, ctx, client, channel.TypeAnthropic, upstream.URL, false,
				anthropic.NewInboundTransformer(), outbound, nil,
			)

			router := gin.New()
			router.POST("/v1/messages", handler.ChatCompletion)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"fixture-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)

			require.Equal(t, http.StatusBadGateway, rec.Code)
			require.Equal(t, "req_fallback_1", rec.Result().Header.Get("X-Request-Id"))
			require.True(t, json.Valid(rec.Body.Bytes()))
			var parsed map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &parsed))
			errObj, ok := parsed["error"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, tt.wantMessage, errObj["message"])
			require.Equal(t, "req_fallback_1", parsed["request_id"])
		})
	}
}

func TestErrorPassthrough_CommittedSSErenouncesFakeTerminalAndPreservesLateError(t *testing.T) {
	const rawErrorBody = `{"type":"error","error":{"type":"overloaded_error","code":"server_overloaded","message":"Upstream overloaded after heartbeat","details":[{"retryable":true}]},"request_id":"req_late_sse_1"}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(25 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(rawErrorBody))
	}))
	defer upstream.Close()

	client := enttest.NewEntClient(t, "sqlite3", "file:err-late-sse?mode=memory&_fk=0")
	defer client.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
	project := client.Project.Create().SetName("p").SaveX(ctx)
	ctx = contexts.WithProjectID(ctx, project.ID)

	outbound, err := anthropic.NewOutboundTransformer(upstream.URL, "fixture-key")
	require.NoError(t, err)
	handler := setupErrorPassthroughHandler(
		t, ctx, client, channel.TypeAnthropic, upstream.URL, false,
		anthropic.NewInboundTransformer(), outbound, nil,
	)
	handler.sseHeartbeatFormat = sseHeartbeatAnthropic
	handler.sseKeepAlive = SSEKeepAliveConfig{Enabled: true, Interval: 5 * time.Millisecond}

	router := gin.New()
	router.POST("/v1/messages", handler.ChatCompletion)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"fixture-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	// Heartbeat already committed HTTP 200, so the late upstream HTTP error must be
	// delivered as an SSE error event with the full upstream JSON body, without any
	// fake message_stop or [DONE].
	require.Equal(t, http.StatusOK, rec.Code)
	wire := rec.Body.String()
	require.Contains(t, wire, "event:error\ndata: "+rawErrorBody)
	require.NotContains(t, wire, "message_stop")
	require.NotContains(t, wire, "[DONE]")
}

func TestErrorPassthrough_MidStreamErrorPreservesFieldsAndNeverEmitsFakeSuccess(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"fixture-model\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
		_, _ = fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"code\":\"mid_stream_overload\",\"message\":\"Provider overloaded mid-stream\",\"param\":\"stream\",\"details\":[{\"node\":\"n1\"}]},\"request_id\":\"req_mid_1\"}\n\n")
	}))
	defer upstream.Close()

	client := enttest.NewEntClient(t, "sqlite3", "file:err-mid-stream?mode=memory&_fk=0")
	defer client.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
	project := client.Project.Create().SetName("p").SaveX(ctx)
	ctx = contexts.WithProjectID(ctx, project.ID)

	outbound, err := anthropic.NewOutboundTransformer(upstream.URL, "fixture-key")
	require.NoError(t, err)
	handler := setupErrorPassthroughHandler(
		t, ctx, client, channel.TypeAnthropic, upstream.URL, false,
		openai.NewInboundTransformer(), outbound, nil,
	)

	router := gin.New()
	router.POST("/v1/chat/completions", handler.ChatCompletion)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"fixture-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, int32(1), calls.Load())
	wire := rec.Body.String()
	require.Contains(t, wire, "event:error\n")
	require.Contains(t, wire, `"code":"mid_stream_overload"`)
	require.Contains(t, wire, `"type":"overloaded_error"`)
	require.Contains(t, wire, `"message":"Provider overloaded mid-stream"`)
	require.Contains(t, wire, `"request_id":"req_mid_1"`)
	require.Contains(t, wire, `"details":[{"node":"n1"}]`)
	require.NotContains(t, wire, "[DONE]")
}
