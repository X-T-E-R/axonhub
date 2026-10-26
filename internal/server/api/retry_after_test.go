package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func TestResponsesHTTPRetryAfterPublicResponse(t *testing.T) {
	for _, tt := range []struct {
		name, value, want string
		delay             time.Duration
	}{
		{name: "ten seconds", value: "10", want: "10"},
		{name: "delayed headers", value: "10", want: "10", delay: 20 * time.Millisecond},
		{name: "missing"},
		{name: "invalid", value: "soon"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const body = `{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"Try again in 10 seconds."}}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.delay > 0 {
					// Keepalive is enabled below; a Responses route must wait for
					// these headers rather than commit HTTP 200 prematurely.
					time.Sleep(tt.delay)
				}
				if tt.value != "" {
					w.Header().Set("Retry-After", tt.value)
				}
				w.Header().Set("Authorization", "fixture-secret")
				w.Header().Set("Set-Cookie", "session=fixture-secret")
				w.Header().Set("X-Upstream-Debug", "not-public")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(body))
			}))
			defer upstream.Close()
			client := enttest.NewEntClient(t, "sqlite3", fmt.Sprintf("file:retry-after-%s?mode=memory&_fk=0", strings.ReplaceAll(tt.name, " ", "-")))
			defer client.Close()
			ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
			project := client.Project.Create().SetName("retry fixture").SaveX(ctx)
			ctx = contexts.WithProjectID(ctx, project.ID)
			row := client.Channel.Create().SetType(channel.TypeOpenaiResponses).SetName("retry fixture").
				SetBaseURL(upstream.URL).SetCredentials(objects.ChannelCredentials{APIKey: "fixture"}).
				SetSupportedModels([]string{"fixture"}).SetDefaultTestModel("fixture").
				SetSettings(&objects.ChannelSettings{PassThroughBody: lo.ToPtr(true)}).SaveX(ctx)
			channels, requests, system, usage := setupSpeechAPITestServices(t, client)
			require.NoError(t, system.SetRetryPolicy(ctx, &biz.RetryPolicy{Enabled: false}))
			outbound, err := responses.NewOutboundTransformer(upstream.URL, "fixture")
			require.NoError(t, err)
			orch := orchestrator.NewChatCompletionOrchestrator(channels, nil, requests, httpclient.NewHttpClient(), responses.NewInboundTransformer(),
				system, usage, nil, nil, nil, requests.LiveStreamRegistry, orchestrator.NewChannelLimiterManager(), nil).
				WithChannelSelector(&speechAPISelector{candidates: []*orchestrator.ChannelModelsCandidate{{
					Channel: &biz.Channel{Channel: row, Outbound: outbound},
					Models:  []biz.ChannelModelEntry{{RequestModel: "fixture", ActualModel: "fixture", Source: "direct"}},
				}}})
			orch.PromptProvider = observationNoPrompts{}
			orch.PromptProtecter = nil
			handler := NewChatCompletionHandlers(orch)
			handler.sseHeartbeatFormat = sseHeartbeatOpenAI
			handler.sseKeepAlive = SSEKeepAliveConfig{Enabled: true, Interval: time.Millisecond}
			router := gin.New()
			router.POST("/v1/responses", handler.ChatCompletion)
			recorder := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"fixture","input":"hello","stream":true}`)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, req)
			require.Equal(t, http.StatusTooManyRequests, recorder.Code)
			require.JSONEq(t, body, recorder.Body.String())
			require.Equal(t, tt.want, recorder.Result().Header.Get("Retry-After"))
			for _, name := range []string{"Authorization", "Set-Cookie", "X-Upstream-Debug"} {
				require.Empty(t, recorder.Result().Header.Get(name))
			}
		})
	}
}

func TestUpstreamErrorPolicyPreservesRetryAfter(t *testing.T) {
	ctx, system := setupUpstreamErrorPolicyTest(t, biz.UpstreamErrorPolicy{
		Mode: biz.UpstreamErrorModeCustom, CustomMessage: "Please try again later.",
	})
	outbound, err := responses.NewOutboundTransformer("https://unused.invalid", "fixture")
	require.NoError(t, err)
	inbound := responses.NewInboundTransformer()
	raw := &httpclient.Error{
		StatusCode: http.StatusTooManyRequests,
		Headers:    http.Header{"Retry-After": {"10"}, "Set-Cookie": {"session=fixture-secret"}},
		Body:       []byte(`{"error":{"type":"rate_limit_error","code":"provider_rate_limit","message":"upstream message"}}`),
	}
	for _, source := range []error{raw, outbound.TransformError(ctx, raw)} {
		public := inbound.TransformError(ctx, applyUpstreamErrorPolicy(ctx, pipeline.WrapUpstreamError(source), system))
		require.Equal(t, http.StatusTooManyRequests, public.StatusCode)
		require.Equal(t, http.Header{"Retry-After": {"10"}}, public.Headers)
		require.JSONEq(t, `{"error":{"type":"rate_limit_error","code":"provider_rate_limit","message":"Please try again later."}}`, string(public.Body))
	}
}

// Native provider events are opaque at these writers: future advice fields must
// survive without being interpreted as gateway retry or replay instructions.
func TestResponsesRawErrorAdvicePreserved(t *testing.T) {
	for _, raw := range []string{
		`{"type":"error","status":429,"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Try again."},"headers":{"retry-after":"10"},"opaque":{"n":9007199254740993}}`,
		`{"type":"response.failed","response":{"id":"resp_fixture","status":"failed","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Try again.","headers":{"retry-after":"10"}}},"opaque":{"n":9007199254740993}}`,
	} {
		var payload map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(raw), &payload))
		var eventType string
		require.NoError(t, json.Unmarshal(payload["type"], &eventType))
		t.Run("SSE/"+eventType, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			WriteSSEStream(c, streams.SliceStream([]*httpclient.StreamEvent{{Type: eventType, Data: []byte(raw)}}))
			require.Equal(t, "event:"+eventType+"\ndata: "+raw+"\n\n", recorder.Body.String())
		})
		t.Run("WS/"+eventType, func(t *testing.T) {
			h := wsTestHandler(func(context.Context, *httpclient.Request, orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
				return orchestrator.ChatCompletionResult{ChatCompletionStream: streams.SliceStream([]*httpclient.StreamEvent{{Type: eventType, Data: []byte(raw)}})}, nil
			})
			conn := wsTestDial(t, wsTestServer(t, h))
			require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "fixture", "input": "hello"}))
			got := wsRead(t, conn)
			require.Equal(t, payload, got)
		})
	}
}

// Codex consumes SSE advice at response.error.headers; WS type:error consumes
// top-level headers together with the actual HTTP failure status.
func TestResponsesSyntheticErrorRetryAfter(t *testing.T) {
	const body = `{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"Try again in 10 seconds."}}`
	outbound, err := responses.NewOutboundTransformer("https://unused.invalid", "fixture")
	require.NoError(t, err)
	for _, tt := range []struct {
		name, value, want string
	}{
		{name: "ten seconds", value: "10", want: "10"},
		{name: "missing"},
		{name: "invalid", value: "soon"},
	} {
		raw := &httpclient.Error{
			StatusCode: http.StatusTooManyRequests, Body: []byte(body),
			Headers: http.Header{"Retry-After": {tt.value}, "Set-Cookie": {"session=fixture-secret"}, "Authorization": {"fixture-secret"}},
		}
		source := pipeline.WrapUpstreamError(outbound.TransformError(t.Context(), raw))
		t.Run("SSE/"+tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{}, sseHeartbeatOpenAI, true)
			session.responsesAPI = true
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			commitSSEHeaders(c)
			c.Writer.Flush()
			public := responses.NewInboundTransformer().TransformError(ctx, source)
			require.NoError(t, session.writeError(c, public))
			require.Equal(t, http.StatusOK, recorder.Code)
			require.Empty(t, recorder.Result().Header.Get("Retry-After"))
			wire := recorder.Body.String()
			if tt.want == "" {
				require.Equal(t, "event:error\ndata: "+string(public.Body)+"\n\n", wire)
			} else {
				require.True(t, strings.HasPrefix(wire, "event:response.failed\ndata:"), wire)
				payload := strings.TrimSpace(strings.TrimPrefix(wire, "event:response.failed\ndata:"))
				require.JSONEq(t, `{"type":"response.failed","response":{"object":"response","status":"failed","error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"Try again in 10 seconds.","headers":{"retry-after":"10"}}}}`, payload)
			}
			require.NotContains(t, wire, "fixture-secret")
		})
		t.Run("WS/"+tt.name, func(t *testing.T) {
			h := wsTestHandler(func(_ context.Context, _ *httpclient.Request, observer orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
				observer.OnUpstreamAttemptSelected(orchestrator.StreamLivenessAttempt{})
				return orchestrator.ChatCompletionResult{}, source
			})
			conn := wsTestDial(t, wsTestServer(t, h))
			require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "fixture", "input": "hello"}))
			event := wsRead(t, conn)
			require.Equal(t, "error", wsEventString(event, "type"))
			require.Equal(t, "429", string(event["status"]))
			require.JSONEq(t, `{"code":"rate_limit_exceeded","type":"rate_limit_error","param":"","message":"Generation failed. Delivery may be unknown; do not automatically replay."}`, string(event["error"]))
			if tt.want == "" {
				require.NotContains(t, event, "headers")
			} else {
				require.JSONEq(t, `{"retry-after":"10"}`, string(event["headers"]))
			}
			require.NotContains(t, event, "execution")
		})
	}
}
