package server

import (
	"bytes"
	"compress/gzip"
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
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/enttest"
	entmodel "github.com/looplj/axonhub/internal/ent/model"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/metrics"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

type nativeDecisionFixture struct {
	ctx                                context.Context
	db                                 *ent.Client
	server                             *httptest.Server
	key, blockedKey, projectBlockedKey *ent.APIKey
	channels                           []*ent.Channel
	requests                           *biz.RequestService
	tester                             *orchestrator.TestChannelOrchestrator
	calls                              atomic.Int32
	started, canceled                  chan struct{}
	received                           chan *http.Request
	bodies                             chan []byte
}

func newNativeDecisionFixture(t *testing.T) *nativeDecisionFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	meterProvider := sdkmetric.NewMeterProvider()
	require.NoError(t, metrics.SetupMetrics(meterProvider, "native-test"))
	t.Cleanup(func() { require.NoError(t, meterProvider.Shutdown(context.Background())) })
	f := &nativeDecisionFixture{started: make(chan struct{}, 1), canceled: make(chan struct{}, 1), received: make(chan *http.Request, 32), bodies: make(chan []byte, 32)}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		f.received <- r
		f.bodies <- body
		var fields map[string]json.RawMessage
		if json.Unmarshal(body, &fields) != nil {
			w.WriteHeader(400)
			return
		}
		var model string
		_ = json.Unmarshal(fields["model"], &model)
		expectedKey := map[string]string{"jev-1.14.0": "typesafe-key", "typesafe/jev-1.14.0": "command-key", "gpt-6-luna": "openai-key", "agr-flash": "agr-key"}[model]
		if r.Header.Get("Authorization") != "Bearer "+expectedKey {
			t.Errorf("wrong upstream auth for %s: %q", model, r.Header.Get("Authorization"))
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "native-synthetic")
		w.Header().Set("X-TypeSafe-Request-Id", "ts-synthetic")
		if strings.Contains(string(body), "wait_for_cancel") {
			f.started <- struct{}{}
			<-r.Context().Done()
			f.canceled <- struct{}{}
			return
		}
		if r.Header.Get("X-Cmd-Zdr") == "1" {
			w.WriteHeader(422)
			fmt.Fprint(w, `{"error":{"message":"No ZDR provider","code":"cmd_zdr_no_providers","type":"invalid_request_error","param":null}}`)
			return
		}
		if v := fields["test_error"]; len(v) > 0 {
			var status int
			_ = json.Unmarshal(v, &status)
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(status)
			if model == "jev-1.14.0" {
				fmt.Fprint(w, `{"detail":[{"loc":["body","state"],"msg":"synthetic","type":"missing"}]}`)
			} else {
				fmt.Fprint(w, `{"error":{"message":"synthetic","type":"server_error","code":"synthetic_code","param":null}}`)
			}
			return
		}
		usage := `"usage":{"input_tokens":83,"output_tokens":0}`
		if string(fields["usage_mode"]) == `"zero"` {
			usage = `"usage":{"input_tokens":0,"output_tokens":0}`
		}
		if string(fields["usage_mode"]) == `"missing"` {
			usage = `"usage_source":"upstream_missing"`
		}
		if string(fields["usage_mode"]) == `"partial"` {
			usage = `"usage":{"input_tokens":83}`
		}
		if model == "gpt-6-luna" {
			if string(fields["usage_mode"]) == `"full"` || fields["usage_mode"] == nil {
				usage = `"usage":{"input_tokens":2048,"input_tokens_details":{"cached_tokens":1024,"cache_write_tokens":512},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":2048}`
			}
			fmt.Fprintf(w, `{"model":%q,"answers":[{"type":"predicate","name":null,"probability":0},{"type":"choice","name":"typed","choice":true,"confidence":0,"probabilities":[{"value":true,"probability":0},{"value":"true","probability":1}]},{"type":"refusal","name":null}],%s,"future":9007199254740993}`, model, usage)
		} else {
			answers := `{"id.with.dot":{"type":"noul","noul":0},"department":{"type":"choice","choice":"billing","confidence":0.85,"probabilities":{"billing":0.9,"technical":0.05,"other":0.05}},"rank":{"type":"score","score":0.25,"confidence":0,"legend":{"0":"first","1":"second"},"probabilities":{"0":0.75,"1":0.25}}}`
			if string(fields["empty_answers"]) == "true" {
				answers = `{}`
			}
			fmt.Fprintf(w, `{"model":%q,"answers":%s,%s,"truncated":false,"routing":{"model":"checkpoint","n":9007199254740993},"x_jev_confidence":0.7}`, model, answers, usage)
		}
	}))
	t.Cleanup(upstream.Close)
	f.db = enttest.NewEntClient(t, "sqlite3", "file:native-decision-"+uuid.NewString()+"?mode=memory&_fk=1")
	t.Cleanup(func() { require.NoError(t, f.db.Close()) })
	f.ctx = ent.NewContext(authz.WithTestBypass(t.Context()), f.db)
	proj := f.db.Project.Create().SetName("native-project").SetStatus(project.StatusActive).SaveX(f.ctx)
	owner := f.db.User.Create().SetEmail(uuid.NewString() + "@example.com").SetPassword("synthetic").SetStatus(user.StatusActivated).SaveX(f.ctx)
	f.db.DataStorage.Create().SetName("primary").SetDescription("synthetic native fixture").SetType(datastorage.TypeDatabase).SetPrimary(true).SetStatus(datastorage.StatusActive).SetSettings(&objects.DataStorageSettings{}).SaveX(f.ctx)
	cache := xcache.Config{Mode: xcache.ModeMemory}
	system := biz.NewSystemService(biz.SystemServiceParams{CacheConfig: cache, Ent: f.db})
	require.NoError(t, system.SetStoragePolicy(f.ctx, &biz.StoragePolicy{StoreRequestBody: true, StoreResponseBody: true}))
	// Exercise empty-answer detection while keeping error attempts deterministic.
	policy := system.RetryPolicyOrDefault(f.ctx)
	policy.Enabled = true
	policy.MaxChannelRetries = 0
	policy.MaxSingleChannelRetries = 0
	policy.EmptyResponseDetection = true
	require.NoError(t, system.SetRetryPolicy(f.ctx, policy))
	for _, spec := range []struct {
		alias, actual, key, base, path string
		format                         llm.APIFormat
	}{
		{"direct", "jev-1.14.0", "typesafe-key", upstream.URL, "/v1/systemone", llm.APIFormatTypeSafeSystemOne},
		{"command", "typesafe/jev-1.14.0", "command-key", upstream.URL + "/provider", "/v1/systemone", llm.APIFormatCommandCodeSystemOne},
		{"openai", "gpt-6-luna", "openai-key", upstream.URL + "/v1", "/decisions", llm.APIFormatOpenAIDecisions},
		{"agr", "agr-flash", "agr-key", upstream.URL, "/v1/systemone", llm.APIFormatTypeSafeSystemOne},
	} {
		row := f.db.Channel.Create().SetType(channel.TypeOpenai).SetStatus(channel.StatusEnabled).SetName(spec.alias).SetBaseURL(spec.base).
			SetCredentials(objects.ChannelCredentials{APIKey: spec.key}).SetSupportedModels([]string{spec.actual}).SetDefaultTestModel(spec.actual).
			SetEndpoints([]objects.ChannelEndpoint{{APIFormat: spec.format.String(), Path: spec.path}}).
			SetSettings(&objects.ChannelSettings{ModelMappings: []objects.ModelMapping{{From: spec.alias, To: spec.actual}}, Proxy: &httpclient.ProxyConfig{Type: httpclient.ProxyTypeDisabled}}).SaveX(f.ctx)
		f.channels = append(f.channels, row)
		f.db.Model.Create().SetDeveloper("synthetic").SetModelID(spec.alias).SetType(entmodel.TypeChat).SetName(spec.alias).SetIcon("synthetic").SetGroup("decisions").SetModelCard(&objects.ModelCard{}).SetStatus(entmodel.StatusEnabled).
			SetSettings(&objects.ModelSettings{Associations: []*objects.ModelAssociation{{Type: "channel_model", ChannelModel: &objects.ChannelModelAssociation{ChannelID: row.ID, ModelID: spec.actual}}}}).SaveX(f.ctx)
	}
	createKey := func(name string, projectID int, profiles *objects.APIKeyProfiles) *ent.APIKey {
		key, err := biz.GenerateAPIKey("ah")
		require.NoError(t, err)
		return f.db.APIKey.Create().SetName(name).SetKey(key).SetType(apikey.TypeServiceAccount).SetStatus(apikey.StatusEnabled).SetUserID(owner.ID).SetProjectID(projectID).
			SetScopes([]string{string(scopes.ScopeReadChannels), string(scopes.ScopeWriteRequests)}).SetProfiles(profiles).SaveX(f.ctx)
	}
	f.key = createKey("native", proj.ID, &objects.APIKeyProfiles{})
	f.blockedKey = createKey("blocked-model", proj.ID, &objects.APIKeyProfiles{ActiveProfile: "deny", Profiles: []objects.APIKeyProfile{{Name: "deny", ModelIDs: []string{"not-allowed"}}}})
	deniedProj := f.db.Project.Create().SetName("channel-denied").SetStatus(project.StatusActive).SetProfiles(&objects.ProjectProfiles{ActiveProfile: "deny", Profiles: []objects.ProjectProfile{{Name: "deny", ChannelIDs: []int{f.channels[2].ID}}}}).SaveX(f.ctx)
	f.projectBlockedKey = createKey("blocked-project", deniedProj.ID, &objects.APIKeyProfiles{})
	projects := biz.NewProjectService(biz.ProjectServiceParams{CacheConfig: cache, Ent: f.db})
	keys := biz.NewAPIKeyService(biz.APIKeyServiceParams{CacheConfig: cache, Ent: f.db, ProjectService: projects, KeyPrefix: "ah"})
	t.Cleanup(keys.Stop)
	auth := &biz.AuthService{APIKeyService: keys, SystemService: system}
	hc := httpclient.NewHttpClient()
	t.Cleanup(hc.CloseIdleConnections)
	channels := biz.NewChannelService(biz.ChannelServiceParams{CacheConfig: cache, Ent: f.db, SystemService: system, WebhookNotifier: biz.NewWebhookNotifier(system, hc), HttpClient: hc})
	t.Cleanup(channels.Stop)
	require.NoError(t, channels.ReloadEnabledChannelsCache(f.ctx))
	models := biz.NewModelService(biz.ModelServiceParams{ChannelService: channels, SystemService: system, Ent: f.db})
	selector := orchestrator.NewDefaultSelector(channels, models, system)
	storage := biz.NewDataStorageService(biz.DataStorageServiceParams{SystemService: system, CacheConfig: cache, Client: f.db})
	usage := biz.NewUsageLogService(f.db, system, channels)
	live := biz.NewLiveStreamRegistry()
	requests := biz.NewRequestService(f.db, system, usage, storage, live)
	f.requests = requests
	prompts := biz.NewPromptService(biz.PromptServiceParams{Ent: f.db})
	protection := biz.NewPromptProtectionRuleService(biz.PromptProtectionRuleServiceParams{CacheConfig: cache, Ent: f.db})
	t.Cleanup(protection.Stop)
	f.tester = orchestrator.NewTestChannelOrchestrator(channels, requests, system, usage, protection, hc)
	traces := biz.NewTraceService(biz.TraceServiceParams{Ent: f.db, RequestService: requests})
	openai := api.NewOpenAIHandlers(api.OpenAIHandlersParams{AuthService: auth, ChannelService: channels, ModelService: models, DefaultSelector: selector, RequestService: requests, SystemService: system, UsageLogService: usage, PromptService: prompts, PromptProtectionRuleService: protection, QuotaService: biz.NewQuotaService(f.db, system), HttpClient: hc, LiveStreamRegistry: live, ChannelLimiterManager: orchestrator.NewChannelLimiterManager(), Client: f.db})
	gateway := New(Config{Debug: true, RequestTimeout: 5 * time.Second, LLMRequestTimeout: 5 * time.Second})
	ipAccess, err := middleware.NewIPAccessControlConfig(false, nil, "")
	require.NoError(t, err)
	SetupRoutes(gateway, Handlers{OpenAI: openai}, f.db, Services{TraceService: traces, ThreadService: biz.NewThreadService(f.db, traces), AuthService: auth, SystemService: system}, ipAccess)
	f.server = httptest.NewServer(gateway.Engine)
	t.Cleanup(f.server.Close)
	return f
}

func (f *nativeDecisionFixture) post(t *testing.T, path, body, key string, headers http.Header) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.server.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	for name, values := range headers {
		req.Header[name] = values
	}
	resp, err := f.server.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, resp.Header, data
}
func nativeSystemBody(model, extra string) string {
	return fmt.Sprintf(`{"model":%q,"state":{"n":9007199254740993},"questions":{"id.with.dot":{"type":"noul","instructions":{"json":true}},"rank":{"type":"score","criteria":["first","second"]}},"future":9007199254740993%s}`, model, extra)
}
func nativeOpenAIBody(extra string) string {
	return `{"model":"openai","input":[{"role":"user","content":[{"type":"input_json","data":{"n":9007199254740993}},{"type":"input_image","image_url":"https://example.invalid/media.png","detail":"original"}]}],"questions":[{"type":"predicate","instructions":[{"type":"input_json","data":{"test":true}}]},{"type":"choice","name":"typed","choices":[{"value":true},{"value":"true"}]},{"type":"score","levels":[{"label":"first"},{"label":"second"}]}],"prompt_cache_key":"cache","prompt_cache_options":{"mode":"implicit","ttl":"30m"}` + extra + `}`
}

func TestNativeDecisionRoutesPersistence(t *testing.T) {
	f := newNativeDecisionFixture(t)
	for i, alias := range []string{"direct", "command", "openai", "agr"} {
		t.Run(alias, func(t *testing.T) {
			path, body := "/v1/systemone", nativeSystemBody(alias, "")
			if alias == "openai" {
				path, body = "/v1/decisions", nativeOpenAIBody("")
			}
			status, headers, result := f.post(t, path, body, f.key.Key, nil)
			require.Equal(t, 200, status, string(result))
			require.Contains(t, string(result), "9007199254740993")
			require.Equal(t, "native-synthetic", headers.Get("X-Request-Id"))
			require.Contains(t, string(result), `"model":"`+alias+`"`)
			wireReq, wire := <-f.received, <-f.bodies
			require.NotContains(t, string(wire), `"messages"`)
			require.Contains(t, string(wire), `9007199254740993`)
			if alias == "command" {
				require.Equal(t, "/provider/v1/systemone", wireReq.URL.Path)
			} else if alias == "openai" {
				require.Equal(t, "/v1/decisions", wireReq.URL.Path)
				require.Contains(t, string(result), `"type":"refusal"`)
				require.Contains(t, string(result), `"choice":true`)
			} else {
				require.Equal(t, "/v1/systemone", wireReq.URL.Path)
			}
			saved := f.db.Request.Query().Where(request.ChannelIDEQ(f.channels[i].ID)).OnlyX(f.ctx)
			require.Equal(t, request.StatusCompleted, saved.Status)
			require.Equal(t, f.key.ID, saved.APIKeyID)
			require.Equal(t, f.key.ProjectID, saved.ProjectID)
			storedBody, err := f.requests.LoadRequestBody(f.ctx, saved)
			require.NoError(t, err)
			require.Equal(t, body, string(storedBody))
			storedResponse, err := f.requests.LoadResponseBody(f.ctx, saved)
			require.NoError(t, err)
			require.Equal(t, string(result), string(storedResponse))
			require.Equal(t, alias, saved.ModelID)
			execution := saved.QueryExecutions().OnlyX(f.ctx)
			require.Equal(t, requestexecution.StatusCompleted, execution.Status)
			storedWire, err := f.requests.LoadRequestExecutionRequestBody(f.ctx, execution)
			require.NoError(t, err)
			require.Equal(t, string(wire), string(storedWire))
			require.Equal(t, f.channels[i].SupportedModels[0], execution.ModelID)
			usage := saved.QueryUsageLogs().OnlyX(f.ctx)
			require.Equal(t, f.key.ID, usage.APIKeyID)
			require.Equal(t, f.channels[i].ID, usage.ChannelID)
			require.EqualValues(t, 0, usage.CompletionTokens)
			if alias == "openai" {
				require.EqualValues(t, 2048, usage.TotalTokens)
				require.EqualValues(t, 1024, usage.PromptCachedTokens)
				require.EqualValues(t, 512, usage.PromptWriteCachedTokens)
			} else {
				require.EqualValues(t, 83, usage.TotalTokens)
			}
		})
	}
	require.EqualValues(t, 4, f.calls.Load())
}
func TestNativeDecisionUsageAndEmptyAnswers(t *testing.T) {
	f := newNativeDecisionFixture(t)
	for _, mode := range []string{"zero", "missing", "partial"} {
		before := f.db.Request.Query().CountX(f.ctx)
		status, _, result := f.post(t, "/v1/systemone", nativeSystemBody("direct", `,"empty_answers":true,"usage_mode":"`+mode+`"`), f.key.Key, nil)
		require.Equal(t, 200, status, string(result))
		require.Contains(t, string(result), `"answers":{}`)
		saved := f.db.Request.Query().Order(ent.Desc(request.FieldID)).FirstX(f.ctx)
		require.Equal(t, before+1, f.db.Request.Query().CountX(f.ctx))
		require.Equal(t, request.StatusCompleted, saved.Status)
		logs := saved.QueryUsageLogs().AllX(f.ctx)
		if mode == "zero" {
			require.Len(t, logs, 1)
			require.EqualValues(t, 0, logs[0].TotalTokens)
		} else {
			require.Empty(t, logs)
		}
	}
	require.EqualValues(t, 3, f.calls.Load())
}
func TestNativeDecisionPermissionCapabilityAndErrors(t *testing.T) {
	f := newNativeDecisionFixture(t)
	for _, tc := range []struct{ path, body, key string }{
		{"/v1/systemone", nativeSystemBody("direct", ""), "bad-key"},
		{"/v1/systemone", nativeSystemBody("direct", ""), f.blockedKey.Key},
		{"/v1/systemone", nativeSystemBody("direct", ""), f.projectBlockedKey.Key},
		{"/v1/decisions", strings.Replace(nativeOpenAIBody(""), `"openai"`, `"direct"`, 1), f.key.Key},
		{"/v1/chat/completions", `{"model":"direct","messages":[{"role":"user","content":"not a decision"}]}`, f.key.Key},
	} {
		status, _, body := f.post(t, tc.path, tc.body, tc.key, nil)
		require.GreaterOrEqual(t, status, 400, string(body))
	}
	require.EqualValues(t, 0, f.calls.Load())
	for _, status := range []int{400, 422, 429, 502, 529} {
		got, headers, body := f.post(t, "/v1/systemone", nativeSystemBody("direct", fmt.Sprintf(`,"test_error":%d`, status)), f.key.Key, nil)
		require.Equal(t, status, got, string(body))
		require.Contains(t, string(body), `"detail"`)
		require.Equal(t, "3", headers.Get("Retry-After"))
	}
	status, _, body := f.post(t, "/v1/systemone", nativeSystemBody("command", ""), f.key.Key, http.Header{"X-Cmd-Zdr": {"1"}})
	require.Equal(t, 422, status, string(body))
	require.Contains(t, string(body), `"code":"cmd_zdr_no_providers"`)
	status, headers, body := f.post(t, "/v1/decisions", nativeOpenAIBody(`,"test_error":429`), f.key.Key, nil)
	require.Equal(t, 429, status, string(body))
	require.Contains(t, string(body), `"code":"synthetic_code"`)
	require.Equal(t, "3", headers.Get("Retry-After"))
	require.EqualValues(t, 7, f.calls.Load())
	executions := f.db.RequestExecution.Query().AllX(f.ctx)
	require.Len(t, executions, 7)
	for _, execution := range executions {
		require.Equal(t, requestexecution.StatusFailed, execution.Status)
	}
}
func TestNativeDecisionCancellationAndCompressedClient(t *testing.T) {
	f := newNativeDecisionFixture(t)
	original := nativeSystemBody("direct", "")
	var compressed bytes.Buffer
	gz := gzip.NewWriter(&compressed)
	_, err := gz.Write([]byte(original))
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	req, err := http.NewRequest(http.MethodPost, f.server.URL+"/v1/systemone", &compressed)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.key.Key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := f.server.Client().Do(req)
	require.NoError(t, err)
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode, string(data))
	wireReq := <-f.received
	<-f.bodies
	require.Empty(t, wireReq.Header.Get("Content-Encoding"))
	ctx, cancel := context.WithCancel(t.Context())
	req, err = http.NewRequestWithContext(ctx, http.MethodPost, f.server.URL+"/v1/systemone", strings.NewReader(nativeSystemBody("direct", `,"wait_for_cancel":true`)))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.key.Key)
	req.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				done <- fmt.Errorf("panic: %v", recovered)
			}
		}()
		r, e := f.server.Client().Do(req)
		if r != nil {
			r.Body.Close()
		}
		done <- e
	}()
	select {
	case <-f.started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream did not start")
	}
	cancel()
	require.Error(t, <-done)
	select {
	case <-f.canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream cancellation not propagated")
	}
	require.Eventually(t, func() bool {
		return f.db.Request.Query().Where(request.StatusEQ(request.StatusCanceled)).CountX(f.ctx) == 1
	}, 3*time.Second, 10*time.Millisecond)
	require.EqualValues(t, 2, f.calls.Load())
}

// Opt in with a local interpreter containing integration_test/native_decisions/requirements.txt.
// All providers and credentials in this test are synthetic; no model API is called.
func TestNativeDecisionOfficialSDKs(t *testing.T) {
	python := os.Getenv("AXON_NATIVE_DECISION_PYTHON")
	if python == "" {
		t.Skip("set AXON_NATIVE_DECISION_PYTHON to the isolated SDK interpreter")
	}
	f := newNativeDecisionFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	script := filepath.Join("..", "..", "integration_test", "native_decisions", "native_decisions_sdk_client.py")
	cmd := exec.CommandContext(ctx, python, script, f.server.URL)
	cmd.Env = append(os.Environ(), "AXON_NATIVE_TEST_KEY="+f.key.Key)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	require.Contains(t, string(output), `"typesafe_direct":true`)
	require.Contains(t, string(output), `"typesafe_command":true`)
	require.Contains(t, string(output), `"openai_decisions":true`)
	require.EqualValues(t, 3, f.calls.Load())
	for _, expectedPath := range []string{"/v1/systemone", "/provider/v1/systemone", "/v1/decisions"} {
		wire, body := <-f.received, <-f.bodies
		require.Equal(t, expectedPath, wire.URL.Path)
		require.Contains(t, string(body), "9007199254740993")
		require.NotContains(t, string(body), `"messages"`)
	}
	rows := f.db.Request.Query().AllX(f.ctx)
	require.Len(t, rows, 3)
	for _, row := range rows {
		require.Equal(t, request.StatusCompleted, row.Status)
		require.Equal(t, f.key.ProjectID, row.ProjectID)
		require.Equal(t, f.key.ID, row.APIKeyID)
		require.Equal(t, requestexecution.StatusCompleted, row.QueryExecutions().OnlyX(f.ctx).Status)
		require.EqualValues(t, 0, row.QueryUsageLogs().OnlyX(f.ctx).CompletionTokens)
	}
}

func TestNativeDecisionChannelAndKeyProbe(t *testing.T) {
	f := newNativeDecisionFixture(t)
	for _, channel := range f.channels {
		id := objects.GUID{Type: "Channel", ID: channel.ID}
		result, err := f.tester.TestChannel(f.ctx, id, nil, &httpclient.ProxyConfig{Type: httpclient.ProxyTypeDisabled})
		require.NoError(t, err)
		require.True(t, result.Success, result.Error)
		keyResult, err := f.tester.TestSingleAPIKey(f.ctx, id, channel.Credentials.APIKey, nil, &httpclient.ProxyConfig{Type: httpclient.ProxyTypeDisabled})
		require.NoError(t, err)
		require.True(t, keyResult.Success, keyResult.Error)
		for range 2 {
			wire, body := <-f.received, <-f.bodies
			require.NotContains(t, string(body), `"messages"`)
			require.NotContains(t, string(body), `"stream"`)
			if channel.Name == "openai" {
				require.Equal(t, "/v1/decisions", wire.URL.Path)
			} else {
				require.Contains(t, wire.URL.Path, "/v1/systemone")
			}
		}
	}
	require.EqualValues(t, 8, f.calls.Load())
}

func TestNativeDecisionUnsupportedNeverSent(t *testing.T) {
	f := newNativeDecisionFixture(t)
	questions := make(map[string]any)
	for i := range 101 {
		questions[fmt.Sprint(i)] = map[string]string{"type": "noul"}
	}
	bulk, err := json.Marshal(map[string]any{"model": "command", "state": "text", "questions": questions})
	require.NoError(t, err)
	for _, tc := range []struct {
		body    string
		headers http.Header
	}{
		{string(bulk), nil},
		{nativeSystemBody("direct", `,"assets":{}`), nil},
		{strings.Replace(nativeSystemBody("direct", ""), `"type":"noul"`, `"type":"box"`, 1), nil},
		{nativeSystemBody("direct", `,"stream":true`), nil},
		{nativeSystemBody("direct", ""), http.Header{"Typesafe-Danger-Zone-Images": {"true"}}},
		{nativeSystemBody("command", ""), http.Header{"Typesafe-Danger-Zone-Future-Text": {"enabled"}}},
	} {
		status, _, body := f.post(t, "/v1/systemone", tc.body, f.key.Key, tc.headers)
		require.GreaterOrEqual(t, status, 400, string(body))
		require.Less(t, status, 500, string(body))
	}
	require.EqualValues(t, 0, f.calls.Load())
	status, _, body := f.post(t, "/v1/systemone", strings.Replace(string(bulk), `"model":"command"`, `"model":"direct"`, 1), f.key.Key, nil)
	require.Equal(t, 200, status, string(body)) // Direct TypeSafe has no independent 100-question cap.
	require.EqualValues(t, 1, f.calls.Load())
}

func TestNativeDecisionGatewayDeadline(t *testing.T) {
	f := newNativeDecisionFixture(t)
	start := time.Now()
	status, _, body := f.post(t, "/v1/systemone", nativeSystemBody("direct", `,"wait_for_cancel":true`), f.key.Key, nil)
	require.GreaterOrEqual(t, status, 400, string(body))
	require.Less(t, time.Since(start), 8*time.Second)
	select {
	case <-f.canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("gateway deadline did not cancel upstream")
	}
	require.EqualValues(t, 1, f.calls.Load())
}
