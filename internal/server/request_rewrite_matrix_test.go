package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/prompt"
	"github.com/looplj/axonhub/internal/ent/promptprotectionrule"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/metrics"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
)

type capturedUpstreamAttempt struct {
	ChannelTag string
	Path       string
	Headers    http.Header
	Body       []byte
}

type rewriteMatrixHarness struct {
	t          *testing.T
	ctx        context.Context
	db         *ent.Client
	server     *httptest.Server
	project    *ent.Project
	apiKey     string
	system     *biz.SystemService
	channels   *biz.ChannelService
	protection *biz.PromptProtectionRuleService
	usage      *biz.UsageLogService
	requests   *biz.RequestService

	mu       sync.Mutex
	attempts []capturedUpstreamAttempt
}

func newRewriteMatrixHarness(t *testing.T, primaryHandler, backupHandler http.HandlerFunc) (*rewriteMatrixHarness, *httptest.Server, *httptest.Server) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	meterProvider := sdkmetric.NewMeterProvider()
	require.NoError(t, metrics.SetupMetrics(meterProvider, "rewrite-matrix-test"))
	t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })

	h := &rewriteMatrixHarness{t: t}

	record := func(tag string, next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			h.mu.Lock()
			h.attempts = append(h.attempts, capturedUpstreamAttempt{
				ChannelTag: tag,
				Path:       r.URL.Path,
				Headers:    r.Header.Clone(),
				Body:       bytes.Clone(body),
			})
			h.mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(body))
			next(w, r)
		}
	}

	primarySrv := httptest.NewServer(record("primary", primaryHandler))
	t.Cleanup(primarySrv.Close)

	backupSrv := httptest.NewServer(record("backup", backupHandler))
	t.Cleanup(backupSrv.Close)

	db := enttest.NewEntClient(t, "sqlite3", "file:rewrite-matrix-"+uuid.NewString()+"?mode=memory&_fk=1")
	t.Cleanup(func() { _ = db.Close() })

	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), db)
	proj := db.Project.Create().SetName("rewrite-proj").SetStatus(project.StatusActive).SaveX(ctx)
	owner := db.User.Create().SetEmail(uuid.NewString() + "@example.com").SetPassword("synthetic").SetIsOwner(true).SetStatus(user.StatusActivated).SaveX(ctx)
	db.DataStorage.Create().SetName("primary").SetDescription("rewrite matrix").SetType(datastorage.TypeDatabase).SetPrimary(true).SetStatus(datastorage.StatusActive).SetSettings(&objects.DataStorageSettings{}).SaveX(ctx)

	cacheCfg := xcache.Config{Mode: xcache.ModeMemory}
	system := biz.NewSystemService(biz.SystemServiceParams{CacheConfig: cacheCfg, Ent: db})
	require.NoError(t, system.SetStoragePolicy(ctx, &biz.StoragePolicy{
		StoreRequestBody:  false,
		StoreResponseBody: false,
		StoreChunks:       false,
	}))
	require.NoError(t, system.SetRetryPolicy(ctx, &biz.RetryPolicy{
		Enabled:                 true,
		MaxChannelRetries:       2,
		MaxSingleChannelRetries: 0,
		RetryDelayMs:            1,
		LoadBalancerStrategy:    biz.LoadBalancerStrategyFailover,
	}))

	keyVal, err := biz.GenerateAPIKey("ah")
	require.NoError(t, err)
	db.APIKey.Create().
		SetName("rewrite-key").
		SetKey(keyVal).
		SetType(apikey.TypeServiceAccount).
		SetStatus(apikey.StatusEnabled).
		SetUserID(owner.ID).
		SetProjectID(proj.ID).
		SetScopes([]string{string(scopes.ScopeReadChannels), string(scopes.ScopeWriteRequests)}).
		SetProfiles(&objects.APIKeyProfiles{}).
		SaveX(ctx)

	projects := biz.NewProjectService(biz.ProjectServiceParams{CacheConfig: cacheCfg, Ent: db})
	keys := biz.NewAPIKeyService(biz.APIKeyServiceParams{CacheConfig: cacheCfg, Ent: db, ProjectService: projects, KeyPrefix: "ah"})
	t.Cleanup(keys.Stop)
	users := biz.NewUserService(biz.UserServiceParams{CacheConfig: cacheCfg, Ent: db})
	auth := &biz.AuthService{APIKeyService: keys, SystemService: system, UserService: users}
	hc := httpclient.NewHttpClient()
	t.Cleanup(hc.CloseIdleConnections)
	channels := biz.NewChannelService(biz.ChannelServiceParams{CacheConfig: cacheCfg, Ent: db, SystemService: system, WebhookNotifier: biz.NewWebhookNotifier(system, hc), HttpClient: hc})
	t.Cleanup(channels.Stop)
	models := biz.NewModelService(biz.ModelServiceParams{ChannelService: channels, SystemService: system, Ent: db})
	selector := orchestrator.NewDefaultSelector(channels, models, system)
	storage := biz.NewDataStorageService(biz.DataStorageServiceParams{SystemService: system, CacheConfig: cacheCfg, Client: db})
	usage := biz.NewUsageLogService(db, system, channels)
	live := biz.NewLiveStreamRegistry()
	requests := biz.NewRequestService(db, system, usage, storage, live)
	prompts := biz.NewPromptService(biz.PromptServiceParams{Ent: db})
	protection := biz.NewPromptProtectionRuleService(biz.PromptProtectionRuleServiceParams{CacheConfig: cacheCfg, Ent: db})
	t.Cleanup(protection.Stop)
	quota := biz.NewQuotaService(db, system)
	traces := biz.NewTraceService(biz.TraceServiceParams{Ent: db, RequestService: requests})
	limiters := orchestrator.NewChannelLimiterManager()

	openaiH := api.NewOpenAIHandlers(api.OpenAIHandlersParams{
		AuthService: auth, ChannelService: channels, ModelService: models, DefaultSelector: selector,
		RequestService: requests, SystemService: system, UsageLogService: usage, PromptService: prompts,
		PromptProtectionRuleService: protection, QuotaService: quota, HttpClient: hc,
		LiveStreamRegistry: live, ChannelLimiterManager: limiters, Client: db,
	})
	anthropicH := api.NewAnthropicHandlers(api.AnthropicHandlersParams{
		ChannelService: channels, ModelService: models, DefaultSelector: selector,
		RequestService: requests, SystemService: system, UsageLogService: usage, PromptService: prompts,
		PromptProtectionRuleService: protection, QuotaService: quota, HttpClient: hc,
		LiveStreamRegistry: live, ChannelLimiterManager: limiters,
	})
	geminiH := api.NewGeminiHandlers(api.GeminiHandlersParams{
		ChannelService: channels, ModelService: models, DefaultSelector: selector,
		RequestService: requests, SystemService: system, UsageLogService: usage, PromptService: prompts,
		PromptProtectionRuleService: protection, QuotaService: quota, HttpClient: hc,
		LiveStreamRegistry: live, ChannelLimiterManager: limiters,
	})

	gateway := New(Config{Debug: true, RequestTimeout: 10 * time.Second, LLMRequestTimeout: 10 * time.Second})
	ipAccess, err := middleware.NewIPAccessControlConfig(false, nil, "")
	require.NoError(t, err)
	SetupRoutes(gateway, Handlers{OpenAI: openaiH, Anthropic: anthropicH, Gemini: geminiH}, db, Services{
		TraceService: traces, ThreadService: biz.NewThreadService(db, traces), AuthService: auth, SystemService: system,
	}, ipAccess)

	srv := httptest.NewServer(gateway.Engine)
	t.Cleanup(srv.Close)

	h.ctx = ctx
	h.db = db
	h.server = srv
	h.project = proj
	h.apiKey = keyVal
	h.system = system
	h.channels = channels
	h.protection = protection
	h.usage = usage
	h.requests = requests
	return h, primarySrv, backupSrv
}

func (h *rewriteMatrixHarness) addChannel(name string, chType channel.Type, baseURL string, weight int, supported []string, settings *objects.ChannelSettings) *ent.Channel {
	h.t.Helper()
	if settings == nil {
		settings = &objects.ChannelSettings{}
	}
	if settings.Proxy == nil {
		settings.Proxy = &httpclient.ProxyConfig{Type: httpclient.ProxyTypeDisabled}
	}
	ch := h.db.Channel.Create().
		SetName(name).
		SetType(chType).
		SetStatus(channel.StatusEnabled).
		SetBaseURL(baseURL).
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-upstream-" + name}).
		SetSupportedModels(supported).
		SetDefaultTestModel(supported[0]).
		SetOrderingWeight(weight).
		SetSettings(settings).
		SaveX(h.ctx)
	require.NoError(h.t, h.channels.ReloadEnabledChannelsCache(h.ctx))
	return ch
}

func (h *rewriteMatrixHarness) resetAttempts() {
	h.mu.Lock()
	h.attempts = nil
	h.mu.Unlock()
}

func (h *rewriteMatrixHarness) snapshotAttempts() []capturedUpstreamAttempt {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]capturedUpstreamAttempt, len(h.attempts))
	copy(out, h.attempts)
	return out
}

func (h *rewriteMatrixHarness) postRaw(path string, body []byte, headers map[string]string) (*http.Response, []byte) {
	h.t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.server.URL+path, bytes.NewReader(body))
	require.NoError(h.t, err)
	req.Header.Set("Authorization", "Bearer "+h.apiKey)
	req.Header.Set("x-api-key", h.apiKey)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.server.Client().Do(req)
	require.NoError(h.t, err)
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	require.NoError(h.t, err)
	return resp, out
}

func encodeGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(data)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func encodeZstd(t *testing.T, data []byte) []byte {
	t.Helper()
	enc, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	defer enc.Close()
	return enc.EncodeAll(data, nil)
}

func encodeDCZ(t *testing.T, dict, data []byte) []byte {
	t.Helper()
	magic := []byte{0x5e, 0x2a, 0x4d, 0x18, 0x20, 0, 0, 0}
	hash := sha256.Sum256(dict)
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderDictRaw(0, dict), zstd.WithEncoderCRC(true))
	require.NoError(t, err)
	defer enc.Close()
	frame := enc.EncodeAll(data, nil)
	out := make([]byte, 0, len(magic)+len(hash)+len(frame))
	out = append(out, magic...)
	out = append(out, hash[:]...)
	out = append(out, frame...)
	return out
}

func writeAnthropicSSE(w http.ResponseWriter, text string, inputTokens, cacheReadTokens, outputTokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
	fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_m1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"upstream\",\"content\":[],\"usage\":{\"input_tokens\":%d,\"cache_read_input_tokens\":%d,\"output_tokens\":1}}}\n\n", inputTokens, cacheReadTokens)
	fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n")
	fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"plan\"}}\n\n")
	fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text)
	fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n")
	fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":%d}}\n\n", outputTokens)
	fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}

func writeOpenAIChatSSE(w http.ResponseWriter, model, text string, promptTokens, completionTokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n", model)
	fmt.Fprintf(w, "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", model, text)
	fmt.Fprintf(w, "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"created\":1700000000,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n", model, promptTokens, completionTokens, promptTokens+completionTokens)
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func writeOpenAIResponsesSSE(w http.ResponseWriter, model, text string, inTokens, outTokens int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\",\"model\":%q}}\n\n", model)
	fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", text)
	fmt.Fprintf(w, "event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"text\":%q}\n\n", text)
	fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"model\":%q,\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":%q}]}],\"usage\":{\"input_tokens\":%d,\"output_tokens\":%d,\"total_tokens\":%d}}}\n\n", model, text, inTokens, outTokens, inTokens+outTokens)
}

// TestFullPipeline_WireRewriteEncodingsAndFailoverMatrix exercises the complete
// Gin + middleware + orchestrator + outbound + persistence pipeline across:
// - Encodings: plain, gzip, zstd, multi-dict DCZ
// - Protocols: Anthropic /v1/messages, OpenAI /v1/chat/completions, OpenAI /v1/responses
// - Rewrites: model remap, OverrideParameters/Headers with Metadata templates & delete/rename,
//   Prompt injection, PromptProtection masking/rejection, auto_reasoning_effort,
//   x-anthropic-billing-header stripping, structural web_search_20250305 detection,
//   message-threads-2026-08-12 continuation, forced stream_options.include_usage
// - Failover: heterogeneous PassThrough->PassThrough (502 + pre-content ping->error + empty EOF)
//   and Shallow PassThrough -> Cross-Protocol with TransformOptions (R2).
func TestFullPipeline_WireRewriteEncodingsAndFailoverMatrix(t *testing.T) {
	t.Run("Encodings_Remap_MetadataTemplateOverrides_and_HeterogeneousFailover", func(t *testing.T) {
		var failMode string
		h, primarySrv, backupSrv := newRewriteMatrixHarness(
			t,
			func(w http.ResponseWriter, r *http.Request) {
				switch failMode {
				case "http_502":
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadGateway)
					_, _ = io.WriteString(w, `{"error":{"type":"api_error","message":"primary 502"}}`)
				case "pre_content_error":
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
					_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_fail\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
					_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"pre-content overload\"}}\n\n")
				case "empty_eof":
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
				default:
					writeAnthropicSSE(w, "from-primary", 120, 30, 45)
				}
			},
			func(w http.ResponseWriter, r *http.Request) {
				writeAnthropicSSE(w, "from-backup", 200, 40, 60)
			},
		)

		h.addChannel("anth-primary", channel.TypeAnthropic, primarySrv.URL, 100, []string{"claude-primary-mapped"}, &objects.ChannelSettings{
			PassThroughBody: lo.ToPtr(true),
			ModelMappings:   []objects.ModelMapping{{From: "claude-logical", To: "claude-primary-mapped"}},
			BodyOverrideOperations: []objects.OverrideOperation{
				{Op: objects.OverrideOpSet, Path: "temperature", Value: "0.1"},
				{Op: objects.OverrideOpSet, Path: "metadata.routed_user", Value: `{{index .Metadata "user_id"}}`},
				{Op: objects.OverrideOpDelete, Path: "drop_me"},
				{Op: objects.OverrideOpDelete, Path: "drop_null"},
				{Op: objects.OverrideOpRename, From: "legacy_flag", To: "modern_flag"},
			},
			HeaderOverrideOperations: []objects.OverrideOperation{
				{Op: objects.OverrideOpSet, Path: "X-Channel-Tag", Value: "primary-{{.Model}}"},
				{Op: objects.OverrideOpSet, Path: "X-User-Template", Value: `{{index .Metadata "user_id"}}`},
			},
		})
		h.addChannel("anth-backup", channel.TypeAnthropic, backupSrv.URL, 50, []string{"claude-backup-mapped"}, &objects.ChannelSettings{
			PassThroughBody: lo.ToPtr(true),
			ModelMappings:   []objects.ModelMapping{{From: "claude-logical", To: "claude-backup-mapped"}},
			BodyOverrideOperations: []objects.OverrideOperation{
				{Op: objects.OverrideOpSet, Path: "temperature", Value: "0.9"},
				{Op: objects.OverrideOpSet, Path: "metadata.backup_for", Value: `{{index .Metadata "user_id"}}`},
			},
			HeaderOverrideOperations: []objects.OverrideOperation{
				{Op: objects.OverrideOpSet, Path: "X-Channel-Tag", Value: "backup-{{.Model}}"},
				{Op: objects.OverrideOpSet, Path: "X-User-Template", Value: `{{index .Metadata "user_id"}}`},
			},
		})

		baseReq := []byte(`{"model":"claude-logical","max_tokens":256,"stream":true,"metadata":{"user_id":"usr_meta_42"},"drop_me":"bye","drop_null":null,"legacy_flag":"v1","messages":[{"role":"user","content":"mentions web_search_20250305 as plain text"}]}`)

		// 1. Seed two distinct DCZ dictionaries so multi-dict DCZ cache is exercised.
		dictA := []byte(`{"model":"claude-logical","max_tokens":128,"stream":true,"metadata":{"user_id":"seed_a"},"messages":[{"role":"user","content":"seed dictionary A payload for multi-dict dcz"}]}`)
		dictB := []byte(`{"model":"claude-logical","max_tokens":128,"stream":true,"metadata":{"user_id":"seed_b"},"messages":[{"role":"user","content":"seed dictionary B payload for multi-dict dcz"}]}`)
		failMode = ""
		respSeedA, _ := h.postRaw("/anthropic/v1/messages", encodeGzip(t, dictA), map[string]string{"Content-Encoding": "gzip", "X-Dict-Store": "1"})
		require.Equal(t, http.StatusOK, respSeedA.StatusCode)
		hashA := sha256.Sum256(dictA)
		require.Equal(t, hex.EncodeToString(hashA[:]), respSeedA.Header.Get("X-Dict-Stored"))

		respSeedB, _ := h.postRaw("/anthropic/v1/messages", encodeZstd(t, dictB), map[string]string{"Content-Encoding": "zstd", "X-Dict-Store": "1"})
		require.Equal(t, http.StatusOK, respSeedB.StatusCode)
		hashB := sha256.Sum256(dictB)
		require.Equal(t, hex.EncodeToString(hashB[:]), respSeedB.Header.Get("X-Dict-Stored"))

		// 2. Verify all 4 inbound encodings (plain, gzip, zstd, dcz-A, dcz-B) deliver exact rewritten wire bytes.
		encodings := []struct {
			name     string
			encoding string
			payload  []byte
		}{
			{"plain", "", baseReq},
			{"gzip", "gzip", encodeGzip(t, baseReq)},
			{"zstd", "zstd", encodeZstd(t, baseReq)},
			{"dcz_dictA", "dcz", encodeDCZ(t, dictA, baseReq)},
			{"dcz_dictB", "dcz", encodeDCZ(t, dictB, baseReq)},
		}
		for _, tc := range encodings {
			t.Run("encoding="+tc.name, func(t *testing.T) {
				h.resetAttempts()
				failMode = ""
				hdrs := map[string]string{}
				if tc.encoding != "" {
					hdrs["Content-Encoding"] = tc.encoding
				}
				resp, body := h.postRaw("/anthropic/v1/messages", tc.payload, hdrs)
				require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
				require.Contains(t, string(body), "from-primary")

				atts := h.snapshotAttempts()
				require.Len(t, atts, 1)
				wire := atts[0]
				require.Equal(t, "primary", wire.ChannelTag)
				require.Equal(t, "claude-primary-mapped", gjson.GetBytes(wire.Body, "model").String())
				require.Equal(t, 0.1, gjson.GetBytes(wire.Body, "temperature").Float())
				require.Equal(t, "usr_meta_42", gjson.GetBytes(wire.Body, "metadata.routed_user").String(), "shallow Metadata must populate override templates")
				require.False(t, gjson.GetBytes(wire.Body, "drop_me").Exists())
				require.False(t, gjson.GetBytes(wire.Body, "drop_null").Exists(), "explicit null delete must remove key")
				require.False(t, gjson.GetBytes(wire.Body, "legacy_flag").Exists())
				require.Equal(t, "v1", gjson.GetBytes(wire.Body, "modern_flag").String())
				require.Equal(t, "primary-claude-primary-mapped", wire.Headers.Get("X-Channel-Tag"))
				require.Equal(t, "usr_meta_42", wire.Headers.Get("X-User-Template"))
				// R8: plain text mention of web_search_20250305 must NOT inject web-search beta header.
				require.NotContains(t, wire.Headers.Get("Anthropic-Beta"), "web-search-2025-03-05")
			})
		}

		// 3. Verify 502, pre-content ping->error (R4), and empty EOF (R4) all fail over cleanly to backup with distinct wire rewrites.
		for _, mode := range []string{"http_502", "pre_content_error", "empty_eof"} {
			t.Run("failover="+mode, func(t *testing.T) {
				h.resetAttempts()
				failMode = mode
				resp, body := h.postRaw("/anthropic/v1/messages", encodeZstd(t, baseReq), map[string]string{"Content-Encoding": "zstd"})
				require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
				require.Contains(t, string(body), "from-backup")
				require.NotContains(t, string(body), "pre-content overload")

				atts := h.snapshotAttempts()
				require.Len(t, atts, 2, "must attempt primary then fail over to backup")
				require.Equal(t, "primary", atts[0].ChannelTag)
				require.Equal(t, "claude-primary-mapped", gjson.GetBytes(atts[0].Body, "model").String())
				require.Equal(t, 0.1, gjson.GetBytes(atts[0].Body, "temperature").Float())
				require.Equal(t, "primary-claude-primary-mapped", atts[0].Headers.Get("X-Channel-Tag"))

				require.Equal(t, "backup", atts[1].ChannelTag)
				require.Equal(t, "claude-backup-mapped", gjson.GetBytes(atts[1].Body, "model").String())
				require.Equal(t, 0.9, gjson.GetBytes(atts[1].Body, "temperature").Float())
				require.Equal(t, "usr_meta_42", gjson.GetBytes(atts[1].Body, "metadata.backup_for").String())
				require.Equal(t, "backup-claude-backup-mapped", atts[1].Headers.Get("X-Channel-Tag"))
				require.Equal(t, "usr_meta_42", atts[1].Headers.Get("X-User-Template"))
			})
		}

		// 4. Verify UsageLog & Request completion persisted accurately for DirectPassThrough Anthropic stream (R5).
		require.Eventually(t, func() bool {
			logs, err := h.db.UsageLog.Query().Order(ent.Desc(usagelog.FieldID)).Limit(1).All(h.ctx)
			return err == nil && len(logs) == 1 && logs[0].PromptTokens == 240 && logs[0].CompletionTokens == 60
		}, 3*time.Second, 20*time.Millisecond, "direct Anthropic stream UsageLog must merge message_start input+cache and message_delta output tokens")
		reqs, err := h.db.Request.Query().Order(ent.Desc(request.FieldID)).Limit(1).All(h.ctx)
		require.NoError(t, err)
		require.Len(t, reqs, 1)
		require.Equal(t, request.StatusCompleted, reqs[0].Status)
	})

	t.Run("R2_ShallowPassThrough_To_CrossProtocolWithTransformOptions_Failover", func(t *testing.T) {
		h, primarySrv, backupSrv := newRewriteMatrixHarness(
			t,
			func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"error":{"type":"api_error","message":"anthropic primary down"}}`)
			},
			func(w http.ResponseWriter, r *http.Request) {
				writeOpenAIChatSSE(w, "gpt-backup-cross", "cross-protocol-ok", 80, 20)
			},
		)

		h.addChannel("anth-primary-shallow", channel.TypeAnthropic, primarySrv.URL, 100, []string{"claude-primary"}, &objects.ChannelSettings{
			PassThroughBody: lo.ToPtr(true),
			ModelMappings:   []objects.ModelMapping{{From: "shared-model", To: "claude-primary"}},
		})
		h.addChannel("openai-backup-transform", channel.TypeOpenai, backupSrv.URL+"/v1", 50, []string{"gpt-backup-cross"}, &objects.ChannelSettings{
			ModelMappings: []objects.ModelMapping{{From: "shared-model", To: "gpt-backup-cross"}},
			TransformOptions: objects.TransformOptions{
				ForceArrayInstructions:       true,
				ForceArrayInputs:             true,
				ReplaceDeveloperRoleWithSystem: true,
			},
		})

		reqBody := []byte(`{"model":"shared-model","max_tokens":128,"stream":true,"system":"be concise","tools":[{"name":"get_weather","description":"weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}}}}],"messages":[{"role":"user","content":"weather in Tokyo?"}]}`)
		resp, out := h.postRaw("/anthropic/v1/messages", reqBody, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(out))
		require.Contains(t, string(out), "cross-protocol-ok")

		atts := h.snapshotAttempts()
		require.Len(t, atts, 2)
		require.Equal(t, "primary", atts[0].ChannelTag)
		require.Equal(t, "claude-primary", gjson.GetBytes(atts[0].Body, "model").String())

		// R2 assertion: Attempt #2 MUST contain full materialized Messages & Tools!
		require.Equal(t, "backup", atts[1].ChannelTag)
		require.Equal(t, "gpt-backup-cross", gjson.GetBytes(atts[1].Body, "model").String())
		msgs := gjson.GetBytes(atts[1].Body, "messages").Array()
		require.GreaterOrEqual(t, len(msgs), 2, "materialized cross-protocol attempt must preserve system + user messages: %s", string(atts[1].Body))
		require.Contains(t, string(atts[1].Body), "weather in Tokyo?")
		require.Equal(t, "get_weather", gjson.GetBytes(atts[1].Body, "tools.0.function.name").String())
	})

	t.Run("R3_AuthoritativePostPolicyBody_PromptInjection_Mask_Reject_BillingHeader_AutoEffort", func(t *testing.T) {
		h, primarySrv, _ := newRewriteMatrixHarness(
			t,
			func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/messages") {
					writeAnthropicSSE(w, "anth-policy-ok", 50, 0, 10)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/responses") {
					writeOpenAIResponsesSSE(w, "gpt-5", "resp-policy-ok", 50, 10)
					return
				}
				writeOpenAIChatSSE(w, "gpt-5", "chat-policy-ok", 50, 10)
			},
			func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
		)

		// Enable AutoReasoningEffort globally.
		ms := h.system.ModelSettingsOrDefault(h.ctx)
		ms.AutoReasoningEffort = true
		require.NoError(t, h.system.SetModelSettings(h.ctx, *ms))

		h.addChannel("anth-pt", channel.TypeAnthropic, primarySrv.URL, 100, []string{"claude-sonnet-4"}, &objects.ChannelSettings{
			PassThroughBody: lo.ToPtr(true),
		})
		h.addChannel("openai-pt", channel.TypeOpenai, primarySrv.URL+"/v1", 100, []string{"gpt-5"}, &objects.ChannelSettings{
			PassThroughBody: lo.ToPtr(true),
		})
		h.addChannel("responses-pt", channel.TypeOpenaiResponses, primarySrv.URL+"/v1", 100, []string{"gpt-5-resp"}, &objects.ChannelSettings{
			PassThroughBody: lo.ToPtr(true),
		})

		// Add system prompt prepend + append for gpt-5.
		h.db.Prompt.Create().
			SetProjectID(h.project.ID).
			SetName("prepend-sys").
			SetRole("system").
			SetContent("INJECTED_PREFIX_POLICY").
			SetStatus(prompt.StatusEnabled).
			SetOrder(1).
			SetSettings(objects.PromptSettings{
				Action: objects.PromptAction{Type: objects.PromptActionTypePrepend},
				Conditions: []objects.PromptActivationConditionComposite{{
					Conditions: []objects.PromptActivationCondition{{
						Type:         objects.PromptActivationConditionTypeModelPattern,
						ModelPattern: lo.ToPtr("gpt-5"),
					}},
				}},
			}).SaveX(h.ctx)
		h.db.Prompt.Create().
			SetProjectID(h.project.ID).
			SetName("append-sys").
			SetRole("system").
			SetContent("INJECTED_SUFFIX_POLICY").
			SetStatus(prompt.StatusEnabled).
			SetOrder(2).
			SetSettings(objects.PromptSettings{
				Action: objects.PromptAction{Type: objects.PromptActionTypeAppend},
				Conditions: []objects.PromptActivationConditionComposite{{
					Conditions: []objects.PromptActivationCondition{{
						Type:         objects.PromptActivationConditionTypeModelPattern,
						ModelPattern: lo.ToPtr("gpt-5"),
					}},
				}},
			}).SaveX(h.ctx)

		// Add PromptProtection mask rule + reject rule via service (updates live cache).
		maskRule, err := h.protection.CreateRule(h.ctx, ent.CreatePromptProtectionRuleInput{
			Name:    "mask-secret",
			Pattern: `SECRET-[0-9]+`,
			Settings: &objects.PromptProtectionSettings{
				Action:      objects.PromptProtectionActionMask,
				Replacement: "[REDACTED_SECRET]",
				Scopes:      []objects.PromptProtectionScope{objects.PromptProtectionScopeUser},
			},
		})
		require.NoError(t, err)
		_, err = h.protection.UpdateRuleStatus(h.ctx, maskRule.ID, promptprotectionrule.StatusEnabled)
		require.NoError(t, err)
		rejRule, err := h.protection.CreateRule(h.ctx, ent.CreatePromptProtectionRuleInput{
			Name:    "reject-banned",
			Pattern: `FORBIDDEN_TOKEN_999`,
			Settings: &objects.PromptProtectionSettings{
				Action: objects.PromptProtectionActionReject,
				Scopes: []objects.PromptProtectionScope{objects.PromptProtectionScopeUser},
			},
		})
		require.NoError(t, err)
		_, err = h.protection.UpdateRuleStatus(h.ctx, rejRule.ID, promptprotectionrule.StatusEnabled)
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			rules, err := h.protection.ListEnabledRules(h.ctx)
			return err == nil && len(rules) == 2
		}, 2*time.Second, 10*time.Millisecond)

		// 3a. OpenAI Chat with PassThroughBody=true: prompt injection + secret mask + auto_reasoning_effort suffix (-high).
		h.resetAttempts()
		chatBody := []byte(`{"model":"gpt-5-high","stream":true,"messages":[{"role":"user","content":"my key is SECRET-12345"}]}`)
		resp, out := h.postRaw("/v1/chat/completions", chatBody, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(out))
		atts := h.snapshotAttempts()
		require.Len(t, atts, 1)
		wire := string(atts[0].Body)
		require.Equal(t, "gpt-5", gjson.Get(wire, "model").String())
		require.Equal(t, "high", gjson.Get(wire, "reasoning_effort").String(), "auto_reasoning_effort must appear in upstream wire")
		require.Contains(t, wire, "INJECTED_PREFIX_POLICY")
		require.Contains(t, wire, "INJECTED_SUFFIX_POLICY")
		require.Contains(t, wire, "[REDACTED_SECRET]")
		require.NotContains(t, wire, "SECRET-12345", "masked secret MUST NOT leak to upstream wire even with PassThroughBody=true")

		// 3b. OpenAI Responses with PassThroughBody=true + auto_reasoning_effort suffix (-medium) + masking.
		h.resetAttempts()
		respBody := []byte(`{"model":"gpt-5-resp-medium","stream":true,"input":[{"role":"user","content":[{"type":"input_text","text":"token SECRET-777"}]}]}`)
		resp, out = h.postRaw("/v1/responses", respBody, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(out))
		atts = h.snapshotAttempts()
		require.Len(t, atts, 1)
		wire = string(atts[0].Body)
		require.Equal(t, "gpt-5-resp", gjson.Get(wire, "model").String())
		require.Equal(t, "medium", gjson.Get(wire, "reasoning.effort").String())
		require.Contains(t, wire, "[REDACTED_SECRET]")
		require.NotContains(t, wire, "SECRET-777")

		// 3c. Reject rule blocks before any upstream call.
		h.resetAttempts()
		rejBody := []byte(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"please run FORBIDDEN_TOKEN_999"}]}`)
		resp, _ = h.postRaw("/v1/chat/completions", rejBody, nil)
		require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		require.Empty(t, h.snapshotAttempts(), "rejected request must make zero upstream calls")

		// 3d. Anthropic x-anthropic-billing-header cch=...; stripped on PassThroughBody=true!
		h.resetAttempts()
		anthBody := []byte(`{"model":"claude-sonnet-4","max_tokens":128,"stream":true,"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1; cch=abc123;"},{"type":"text","text":"normal system"}],"messages":[{"role":"user","content":"hello"}]}`)
		resp, out = h.postRaw("/anthropic/v1/messages", anthBody, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(out))
		atts = h.snapshotAttempts()
		require.Len(t, atts, 1)
		require.NotContains(t, string(atts[0].Body), "cch=abc123", "x-anthropic-billing-header cch must be stripped on PassThroughBody=true")
	})

	t.Run("AnthropicMessageThreads_And_ForcedStreamIncludeUsageIsolation", func(t *testing.T) {
		var anthCalls int
		h, primarySrv, backupSrv := newRewriteMatrixHarness(
			t,
			func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/messages") {
					anthCalls++
					writeAnthropicSSE(w, fmt.Sprintf("turn-%d-reply", anthCalls), 40, 10, 15)
					return
				}
				// Primary OpenAI channel fails after forcing streaming.
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, `{"error":{"type":"api_error","message":"primary down"}}`)
			},
			func(w http.ResponseWriter, r *http.Request) {
				writeOpenAIChatSSE(w, "gpt-stream-test", "stream-aggregated-ok", 12, 8)
			},
		)

		h.addChannel("anth-thread", channel.TypeAnthropic, primarySrv.URL, 100, []string{"claude-thread"}, &objects.ChannelSettings{
			PassThroughBody: lo.ToPtr(true),
		})
		// OpenAI channel with Policies.Stream=Require (non-stream client -> forced stream + include_usage -> aggregated JSON).
		chForce := h.addChannel("oai-force-stream", channel.TypeOpenai, backupSrv.URL+"/v1", 100, []string{"gpt-stream-test"}, &objects.ChannelSettings{})
		chForce.Update().SetPolicies(objects.ChannelPolicies{Stream: objects.CapabilityPolicyRequire}).SaveX(h.ctx)
		require.NoError(t, h.channels.ReloadEnabledChannelsCache(h.ctx))

		// 1. Anthropic message-threads-2026-08-12 create + continue turn.
		h.resetAttempts()
		turn1 := []byte(`{"model":"claude-thread","max_tokens":128,"stream":true,"thread":{"type":"create"},"messages":[{"role":"user","content":"remember secret banana"}]}`)
		resp1, out1 := h.postRaw("/anthropic/v1/messages", turn1, map[string]string{"Anthropic-Beta": "message-threads-2026-08-12"})
		require.Equal(t, http.StatusOK, resp1.StatusCode, string(out1))
		// Check first attempt removed "thread" field from upstream wire.
		atts := h.snapshotAttempts()
		require.Len(t, atts, 1)
		require.False(t, gjson.GetBytes(atts[0].Body, "thread").Exists(), "thread wrapper must be stripped before upstream")

		// 2. Non-streaming OpenAI Chat -> Policies.Stream=Require forces stream=true + stream_options.include_usage=true
		// on upstream wire and aggregates the SSE stream back to a non-streaming JSON response.
		h.resetAttempts()
		nonStreamReq := []byte(`{"model":"gpt-stream-test","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
		resp2, out2 := h.postRaw("/v1/chat/completions", nonStreamReq, nil)
		require.Equal(t, http.StatusOK, resp2.StatusCode, string(out2))
		require.Contains(t, string(out2), "stream-aggregated-ok")

		atts = h.snapshotAttempts()
		require.Len(t, atts, 1)
		require.True(t, gjson.GetBytes(atts[0].Body, "stream").Bool(), "channel with Stream=Require must force stream=true")
		require.True(t, gjson.GetBytes(atts[0].Body, "stream_options.include_usage").Bool(), "channel with Stream=Require must force stream_options.include_usage=true")
	})

	t.Run("DuplicateKeys_And_ContentTypeParity", func(t *testing.T) {
		h, primarySrv, backupSrv := newRewriteMatrixHarness(
			t,
			func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/messages"):
					writeAnthropicSSE(w, "dup-ok", 10, 0, 5)
				case strings.HasSuffix(r.URL.Path, "/responses"):
					writeOpenAIResponsesSSE(w, "gpt-resp-mapped", "dup-resp-ok", 10, 5)
				default:
					writeOpenAIChatSSE(w, "gpt-mapped", "dup-chat-ok", 10, 5)
				}
			},
			func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/responses") {
					writeOpenAIResponsesSSE(w, "gpt-resp-fullpt-mapped", "fullpt-resp-dup-ok", 10, 5)
					return
				}
				writeAnthropicSSE(w, "fullpt-dup-ok", 10, 0, 5)
			},
		)

		// Ordinary PassThroughBody=true Anthropic channel with model mapping: claude-logical -> claude-mapped-pri
		h.addChannel("anth-dup-pt", channel.TypeAnthropic, primarySrv.URL, 100, []string{"claude-mapped-pri"}, &objects.ChannelSettings{
			PassThroughBody: lo.ToPtr(true),
			ModelMappings:   []objects.ModelMapping{{From: "claude-logical", To: "claude-mapped-pri"}},
		})
		// AxonHub FullPassThrough=true channel with model mapping: claude-fullpt -> claude-fullpt-mapped
		h.addChannel("axon-fullpt-dup", channel.TypeAxonhub, backupSrv.URL, 100, []string{"claude-fullpt-mapped"}, &objects.ChannelSettings{
			FullPassThrough: true,
			ModelMappings:   []objects.ModelMapping{{From: "claude-fullpt", To: "claude-fullpt-mapped"}},
		})
		// OpenAI Chat + Responses channels for Content-Type validation parity & omitted-CT duplicate-key tests
		h.addChannel("oai-ct-chat", channel.TypeOpenai, primarySrv.URL+"/v1", 100, []string{"gpt-ct"}, &objects.ChannelSettings{
			PassThroughBody: lo.ToPtr(true),
		})
		h.addChannel("oai-ct-resp", channel.TypeOpenaiResponses, primarySrv.URL+"/v1", 100, []string{"gpt-resp-ct", "gpt-resp-mapped"}, &objects.ChannelSettings{
			PassThroughBody: lo.ToPtr(true),
			ModelMappings:   []objects.ModelMapping{{From: "gpt-resp-logical", To: "gpt-resp-mapped"}},
		})
		h.addChannel("axon-fullpt-resp-dup", channel.TypeAxonhub, backupSrv.URL, 100, []string{"gpt-resp-fullpt-mapped"}, &objects.ChannelSettings{
			FullPassThrough: true,
			ModelMappings:   []objects.ModelMapping{{From: "gpt-resp-fullpt", To: "gpt-resp-fullpt-mapped"}},
		})

		// 1. Duplicate top-level "model" key: first value equals mapped target, last value is logical alias.
		// Gateway resolves last-wins ("claude-logical"), remaps to "claude-mapped-pri", and must emit canonical
		// JSON so a last-wins upstream decoder observes "claude-mapped-pri" (not the raw duplicate "claude-logical").
		h.resetAttempts()
		dupBody := []byte(`{"model":"claude-mapped-pri","model":"claude-logical","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		resp, out := h.postRaw("/anthropic/v1/messages", dupBody, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(out))
		atts := h.snapshotAttempts()
		require.Len(t, atts, 1)
		var decoded map[string]any
		require.NoError(t, json.Unmarshal(atts[0].Body, &decoded))
		require.Equal(t, "claude-mapped-pri", decoded["model"], "upstream last-wins JSON decode must observe remapped model")
		require.Equal(t, 1, strings.Count(string(atts[0].Body), `"model"`), "canonical AST output must not retain duplicate model keys")

		// 1b. Case-folded duplicate top-level key ("model" + "Model") must also mark RawBodyAmbiguous
		// and emit canonical remapped JSON instead of passing through the mixed-case duplicate.
		h.resetAttempts()
		caseDupBody := []byte(`{"model":"claude-mapped-pri","Model":"claude-logical","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		resp, out = h.postRaw("/anthropic/v1/messages", caseDupBody, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(out))
		atts = h.snapshotAttempts()
		require.Len(t, atts, 1)
		decoded = nil
		require.NoError(t, json.Unmarshal(atts[0].Body, &decoded))
		require.Equal(t, "claude-mapped-pri", decoded["model"], "case-folded duplicate model keys must emit canonical remapped model")
		require.NotContains(t, string(atts[0].Body), `"Model"`, "canonical AST must not retain mixed-case duplicate Model key")

		// 2. Same duplicate-key test under AxonHub FullPassThrough=true.
		h.resetAttempts()
		dupFullPT := []byte(`{"model":"claude-fullpt-mapped","model":"claude-fullpt","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		resp, out = h.postRaw("/anthropic/v1/messages", dupFullPT, nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(out))
		atts = h.snapshotAttempts()
		require.Len(t, atts, 1)
		decoded = nil
		require.NoError(t, json.Unmarshal(atts[0].Body, &decoded))
		require.Equal(t, "claude-fullpt-mapped", decoded["model"], "FullPassThrough must emit canonical AST for duplicate-key bodies")
		require.Equal(t, 1, strings.Count(string(atts[0].Body), `"model"`))

		// 2b. /v1/responses with OMITTED Content-Type + duplicate "model" keys must remain accepted
		// (matching llm/transformer/openai/responses/inbound.go) AND still mark RawBodyAmbiguous so both
		// ordinary PassThroughBody and AxonHub FullPassThrough emit canonical remapped JSON.
		for _, tc := range []struct {
			name       string
			logical    string
			wantMapped string
		}{
			{"responses_omitted_ct_passthrough", "gpt-resp-logical", "gpt-resp-mapped"},
			{"responses_omitted_ct_full_passthrough", "gpt-resp-fullpt", "gpt-resp-fullpt-mapped"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h.resetAttempts()
				body := fmt.Sprintf(`{"model":%q,"model":%q,"stream":true,"input":"hi"}`, tc.wantMapped, tc.logical)
				req, err := http.NewRequest(http.MethodPost, h.server.URL+"/v1/responses", strings.NewReader(body))
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer "+h.apiKey)
				// Deliberately omit Content-Type header.
				res, err := h.server.Client().Do(req)
				require.NoError(t, err)
				outBytes, readErr := io.ReadAll(res.Body)
				_ = res.Body.Close()
				require.NoError(t, readErr)
				require.Equal(t, http.StatusOK, res.StatusCode, string(outBytes))

				atts := h.snapshotAttempts()
				require.Len(t, atts, 1)
				var dec map[string]any
				require.NoError(t, json.Unmarshal(atts[0].Body, &dec))
				require.Equal(t, tc.wantMapped, dec["model"], "omitted-CT Responses duplicate-key body must emit remapped model")
				require.Equal(t, 1, strings.Count(string(atts[0].Body), `"model"`), "canonical AST must not retain duplicate model keys")
			})
		}

		// 3. Invalid/missing Content-Type must match full inbound transformer validation parity:
		// OpenAI Chat and Anthropic reject both empty and non-JSON Content-Type; OpenAI Responses rejects non-JSON.
		for _, tc := range []struct {
			name string
			path string
			body string
			ct   string
		}{
			{"chat_empty_ct", "/v1/chat/completions", `{"model":"gpt-ct","stream":true,"messages":[{"role":"user","content":"hi"}]}`, ""},
			{"chat_text_plain", "/v1/chat/completions", `{"model":"gpt-ct","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "text/plain"},
			{"anthropic_empty_ct", "/anthropic/v1/messages", `{"model":"claude-logical","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, ""},
			{"anthropic_text_plain", "/anthropic/v1/messages", `{"model":"claude-logical","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hi"}]}`, "text/plain"},
			{"responses_text_plain", "/v1/responses", `{"model":"gpt-resp-ct","stream":true,"input":"hi"}`, "text/plain"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				h.resetAttempts()
				req, err := http.NewRequest(http.MethodPost, h.server.URL+tc.path, strings.NewReader(tc.body))
				require.NoError(t, err)
				req.Header.Set("Authorization", "Bearer "+h.apiKey)
				if tc.ct != "" {
					req.Header.Set("Content-Type", tc.ct)
				}
				res, err := h.server.Client().Do(req)
				require.NoError(t, err)
				defer res.Body.Close()
				_, _ = io.ReadAll(res.Body)
				require.NotEqual(t, http.StatusOK, res.StatusCode, "empty/non-JSON Content-Type must be rejected")
				require.Empty(t, h.snapshotAttempts(), "invalid Content-Type must not reach upstream")
			})
		}
	})

	t.Run("DirectPassThrough_PostContentError_PreTerminalEOF_And_ResponsesIncomplete", func(t *testing.T) {
		var (
			mode               string
			openSocketCanceled = make(chan struct{}, 1)
		)
		h, primarySrv, _ := newRewriteMatrixHarness(
			t,
			func(w http.ResponseWriter, r *http.Request) {
				if mode == "post_hang_recovery_ok" {
					writeAnthropicSSE(w, "slot-released-ok", 10, 0, 5)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fl, _ := w.(http.Flusher)
				switch mode {
				case "post_content_error":
					_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_pce\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-err\",\"content\":[],\"usage\":{\"input_tokens\":30,\"output_tokens\":2}}}\n\n")
					_, _ = fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
					_, _ = fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n")
					if fl != nil {
						fl.Flush()
					}
					_, _ = fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"mid-stream boom\"}}\n\n")
					if fl != nil {
						fl.Flush()
					}
				case "post_content_error_open_socket":
					_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_hang\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-hang\",\"content\":[],\"usage\":{\"input_tokens\":30,\"output_tokens\":2}}}\n\n")
					_, _ = fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
					_, _ = fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"before-hang\"}}\n\n")
					if fl != nil {
						fl.Flush()
					}
					_, _ = fmt.Fprint(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"mid-stream hang boom\"}}\n\n")
					if fl != nil {
						fl.Flush()
					}
					// Keep transport open until AxonHub closes/cancels upstream.
					<-r.Context().Done()
					select {
					case openSocketCanceled <- struct{}{}:
					default:
					}
				case "pre_terminal_eof":
					_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_eof\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-err\",\"content\":[],\"usage\":{\"input_tokens\":30,\"output_tokens\":5}}}\n\n")
					_, _ = fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
					_, _ = fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"cut off\"}}\n\n")
					if fl != nil {
						fl.Flush()
					}
					// Clean EOF without message_stop.
				case "responses_incomplete":
					_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_inc\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"gpt-inc\",\"output\":[]}}\n\n")
					_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"m1\",\"output_index\":0,\"content_index\":0,\"delta\":\"partial\"}\n\n")
					_, _ = fmt.Fprint(w, "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_inc\",\"object\":\"response\",\"status\":\"incomplete\",\"model\":\"gpt-inc\",\"output\":[],\"usage\":{\"input_tokens\":77,\"output_tokens\":33,\"total_tokens\":110}}}\n\n")
					if fl != nil {
						fl.Flush()
					}
				}
			},
			func(w http.ResponseWriter, r *http.Request) {},
		)

		chAnth := h.addChannel("anth-direct-err", channel.TypeAnthropic, primarySrv.URL, 100, []string{"claude-err"}, &objects.ChannelSettings{
			PassThroughBody: lo.ToPtr(true),
		})
		chAnthHang := h.addChannel("anth-direct-hang", channel.TypeAnthropic, primarySrv.URL, 100, []string{"claude-hang"}, &objects.ChannelSettings{
			PassThroughBody: lo.ToPtr(true),
			RateLimit: &objects.ChannelRateLimit{
				MaxConcurrent: lo.ToPtr(int64(1)),
			},
		})
		chResp := h.addChannel("resp-direct-inc", channel.TypeOpenaiResponses, primarySrv.URL+"/v1", 100, []string{"gpt-inc"}, &objects.ChannelSettings{
			PassThroughBody: lo.ToPtr(true),
		})

		// (a) Post-content SSE error frame: forwarded once to client AND persisted as failed execution + channel failure metric.
		mode = "post_content_error"
		resp, out := h.postRaw("/anthropic/v1/messages", []byte(`{"model":"claude-err","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hi"}]}`), nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, string(out), "mid-stream boom", "raw SSE error frame must still be forwarded downstream")

		require.Eventually(t, func() bool {
			execs := h.db.RequestExecution.Query().Where(requestexecution.ChannelIDEQ(chAnth.ID)).AllX(h.ctx)
			if len(execs) == 0 {
				return false
			}
			m, err := h.channels.GetChannelMetrics(h.ctx, chAnth.ID)
			return err == nil && execs[len(execs)-1].Status == requestexecution.StatusFailed && m.FailureCount >= 1 && m.ConsecutiveFailures >= 1
		}, 3*time.Second, 25*time.Millisecond, "post-content SSE error must persist failed execution and record channel failure")

		// (b) Pre-terminal EOF after partial usage: must NOT record bogus success or reset ConsecutiveFailures.
		mode = "pre_terminal_eof"
		resp, out = h.postRaw("/anthropic/v1/messages", []byte(`{"model":"claude-err","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hi"}]}`), nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, string(out), "cut off")

		// Give async metric goroutine a brief window, then confirm SuccessCount remained 0 and ConsecutiveFailures did not reset.
		time.Sleep(100 * time.Millisecond)
		mAnth, err := h.channels.GetChannelMetrics(h.ctx, chAnth.ID)
		require.NoError(t, err)
		require.Zero(t, mAnth.SuccessCount, "pre-terminal EOF with partial usage must never record bogus success")
		require.GreaterOrEqual(t, mAnth.ConsecutiveFailures, int64(1), "pre-terminal EOF must not reset ConsecutiveFailures")

		// (c) OpenAI Responses response.incomplete with usage: must NOT record bogus success on channel metrics.
		mode = "responses_incomplete"
		resp, out = h.postRaw("/v1/responses", []byte(`{"model":"gpt-inc","stream":true,"input":"hi"}`), nil)
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Contains(t, string(out), "response.incomplete")

		time.Sleep(100 * time.Millisecond)
		mResp, err := h.channels.GetChannelMetrics(h.ctx, chResp.ID)
		require.NoError(t, err)
		require.Zero(t, mResp.SuccessCount, "response.incomplete must not latch channel performance success")

		// (d) Post-content SSE error where upstream holds the HTTP socket open waiting on r.Context().Done():
		// downstream must finish promptly with the single error event, upstream socket must close,
		// RequestExecution must be marked StatusFailed, and the MaxConcurrent=1 admission slot must be released.
		mode = "post_content_error_open_socket"
		startHang := time.Now()
		resp, out = h.postRaw("/anthropic/v1/messages", []byte(`{"model":"claude-hang","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hi"}]}`), nil)
		require.Less(t, time.Since(startHang), 3*time.Second, "post-commit SSE error must terminate downstream promptly even when upstream holds socket open")
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, 1, strings.Count(string(out), "mid-stream hang boom"), "raw SSE error frame must be forwarded exactly once")

		select {
		case <-openSocketCanceled:
		case <-time.After(3 * time.Second):
			t.Fatal("upstream open socket was not canceled promptly after post-commit SSE error")
		}

		require.Eventually(t, func() bool {
			execs := h.db.RequestExecution.Query().Where(requestexecution.ChannelIDEQ(chAnthHang.ID)).AllX(h.ctx)
			if len(execs) == 0 {
				return false
			}
			m, err := h.channels.GetChannelMetrics(h.ctx, chAnthHang.ID)
			return err == nil && execs[len(execs)-1].Status == requestexecution.StatusFailed && m.FailureCount >= 1
		}, 3*time.Second, 25*time.Millisecond, "open-socket post-content SSE error must persist failed execution and channel failure")

		// Follow-up request on the same MaxConcurrent=1 channel must succeed immediately, proving the admission slot was released.
		mode = "post_hang_recovery_ok"
		resp, out = h.postRaw("/anthropic/v1/messages", []byte(`{"model":"claude-hang","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"after-hang"}]}`), nil)
		require.Equal(t, http.StatusOK, resp.StatusCode, string(out))
		require.Contains(t, string(out), "slot-released-ok")
	})
}
