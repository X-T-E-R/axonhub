package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/hook"
	"github.com/looplj/axonhub/internal/ent/migrate"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/metrics"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/pkg/xredis"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/internal/server/gql"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/internal/server/scheduler"
	"github.com/looplj/axonhub/internal/server/video_storage"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/stretchr/testify/require"
	"github.com/zhenzou/executors"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Two complete HTTP/WS service graphs, each with its own client pool, auth,
// channel/provider cache and writer. Only PG18 and Redis are shared.
func TestClusterTwoHTTPServicesAuthorizationAndOwnership(t *testing.T) {
	dsn, redisURL := os.Getenv("AXONHUB_TEST_PG_DSN"), os.Getenv("AXONHUB_TEST_REDIS_URL")
	if dsn == "" || redisURL == "" || os.Getenv("AXONHUB_TEST_PG_DISPOSABLE") != "1" {
		t.Skip("requires disposable PG18 and Redis")
	}
	gin.SetMode(gin.TestMode)
	meterProvider := sdkmetric.NewMeterProvider()
	require.NoError(t, metrics.SetupMetrics(meterProvider, "cluster-test"))
	t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })
	root, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	schema := "task49_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = root.Exec("CREATE SCHEMA " + schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = root.Exec("DROP SCHEMA " + schema + " CASCADE"); _ = root.Close() })
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	dsn += sep + "search_path=" + schema
	open := func() *ent.Client {
		db, err := sql.Open("pgx", dsn)
		require.NoError(t, err)
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(1)
		return ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	}
	a, b := open(), open()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	ctx := authz.WithTestBypass(context.Background())
	require.NoError(t, a.Schema.Create(ctx, migrate.WithForeignKeys(false)))
	group := a.Project.Create().SetName("group").SaveX(ctx)
	owner := a.User.Create().SetEmail("admin@example.test").SetPassword("synthetic").SetIsOwner(true).SetStatus(user.StatusActivated).SaveX(ctx)
	personal := a.User.Create().SetEmail("portal@example.test").SetPassword("synthetic").SetAccessMode(user.AccessModePortal).SetStatus(user.StatusActivated).SaveX(ctx)
	member := a.UserProject.Create().SetUserID(personal.ID).SetProjectID(group.ID).SaveX(ctx)
	a.DataStorage.Create().SetName("primary").SetDescription("synthetic").SetType(datastorage.TypeDatabase).SetPrimary(true).SetSettings(&objects.DataStorageSettings{}).SaveX(ctx)
	var calls, videoCalls, deletes, modelCalls atomic.Int32
	var completeVideos atomic.Bool
	var upstream *httptest.Server
	upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			modelCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"object":"list","data":[{"id":"m","object":"model"}]}`)
			return
		}
		if r.URL.Path == "/asset.mp4" {
			fmt.Fprint(w, "synthetic-video")
			return
		}
		if strings.Contains(r.URL.Path, "/videos") || strings.Contains(r.URL.Path, "/contents/generations/tasks") {
			videoCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodDelete {
				deletes.Add(1)
				if strings.HasSuffix(r.URL.Path, "/unknown-delete") {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
					return
				}
				w.WriteHeader(204)
				return
			}
			id := "opaque-openai"
			if strings.Contains(r.URL.Path, "tasks") {
				id = "opaque-doubao"
			}
			if r.Method == http.MethodGet {
				id = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				if completeVideos.Load() {
					if strings.Contains(r.URL.Path, "tasks") {
						fmt.Fprintf(w, `{"id":%q,"status":"succeeded","model":"seedance","content":{"video_url":%q}}`, id, upstream.URL+"/asset.mp4")
					} else {
						fmt.Fprintf(w, `{"id":%q,"status":"completed","model":"video","video_url":%q}`, id, upstream.URL+"/asset.mp4")
					}
				} else {
					fmt.Fprintf(w, `{"id":%q,"status":"running","model":"video"}`, id)
				}
				return
			}
			fmt.Fprintf(w, `{"id":%q,"status":"queued"}`, id)
			return
		}
		calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			if strings.Contains(r.URL.Path, "chat/completions") {
				fmt.Fprint(w, "data: {\"id\":\"chat_test\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"partial\"}}]}\n\ndata: {\"id\":\"chat_test\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
				w.(http.Flusher).Flush()
				fmt.Fprint(w, "data: {\"id\":\"chat_test\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
				return
			}
			fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"model\":\"m\",\"status\":\"in_progress\",\"output\":[]}}\n\nevent: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_test\",\"output_index\":0,\"content_index\":0,\"delta\":\"partial\"}\n\n")
			w.(http.Flusher).Flush()
			fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"object\":\"response\",\"model\":\"m\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_http","object":"response","model":"m","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`)
	}))
	defer upstream.Close()
	makeChannel := func(name, key, model string) *ent.Channel {
		return a.Channel.Create().SetName(name).SetStatus(channel.StatusEnabled).SetType(channel.TypeOpenaiResponses).SetBaseURL(upstream.URL).SetCredentials(objects.ChannelCredentials{APIKey: key}).SetSupportedModels([]string{model}).SetTags([]string{"allowed"}).SetDefaultTestModel(model).SaveX(ctx)
	}
	ca, cb := makeChannel("a", "synthetic-a", "other"), makeChannel("b", "synthetic-b", "m")
	fallback := makeChannel("b-fallback", "synthetic-fallback", "m")
	a.Channel.UpdateOne(fallback).SetStatus(channel.StatusDisabled).SaveX(ctx)
	videoChannel := a.Channel.Create().SetName("b-video").SetStatus(channel.StatusEnabled).SetType(channel.TypeOpenai).SetBaseURL(upstream.URL).SetCredentials(objects.ChannelCredentials{APIKey: "synthetic-video"}).SetSupportedModels([]string{"video"}).SetTags([]string{"allowed"}).SetDefaultTestModel("video").SaveX(ctx)
	doubaoChannel := a.Channel.Create().SetName("b-doubao").SetStatus(channel.StatusEnabled).SetType(channel.TypeDoubao).SetBaseURL(upstream.URL).SetCredentials(objects.ChannelCredentials{APIKey: "synthetic-doubao"}).SetSupportedModels([]string{"seedance"}).SetTags([]string{"allowed"}).SetDefaultTestModel("video").SaveX(ctx)
	lagVideoChannel := a.Channel.Create().SetName("a-video").SetStatus(channel.StatusEnabled).SetType(channel.TypeOpenai).SetBaseURL(upstream.URL).SetCredentials(objects.ChannelCredentials{APIKey: "synthetic-lag-video"}).SetSupportedModels([]string{"lag-video"}).SetTags([]string{"allowed"}).SetDefaultTestModel("video").SaveX(ctx)
	jwtToken := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": "synthetic-reset"}})
	syntheticJWT, err := jwtToken.SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)
	resetChannel := a.Channel.Create().SetName("b-reset").SetStatus(channel.StatusDisabled).SetType(channel.TypeCodex).SetBaseURL(upstream.URL).SetCredentials(objects.ChannelCredentials{OAuth: &objects.OAuthCredentials{AccessToken: syntheticJWT}}).SetSupportedModels([]string{"reset"}).SetDefaultTestModel("reset").SaveX(ctx)
	var controlQuotaCalls, ownerCreditGets, ownerResets atomic.Int32
	var unknownReset atomic.Bool
	ids := []string{"alias", "m", "other", "video", "seedance", "lag-video"}
	profile := &objects.ProjectProfiles{ActiveProfile: "active", Profiles: []objects.ProjectProfile{{Name: "active", ModelIDs: &ids, ChannelTags: []string{"allowed"}}}}
	a.Project.UpdateOne(group).SetProfiles(profile).SaveX(ctx)
	key := a.APIKey.Create().SetName("personal").SetKey("ah-cluster-personal").SetType(apikey.TypePersonal).SetUserID(personal.ID).SetProjectID(group.ID).SetPortalManaged(true).SetScopes([]string{"read_channels", "write_requests"}).SetProfiles(&objects.APIKeyProfiles{ActiveProfile: "active", Profiles: []objects.APIKeyProfile{{Name: "active", ModelMappings: []objects.ModelMapping{{From: "alias", To: "m"}}}}}).SaveX(ctx)
	legacyGroup := a.Project.Create().SetName("legacy").SaveX(ctx)
	legacy := a.APIKey.Create().SetName("legacy").SetKey("ah-cluster-legacy").SetType(apikey.TypeUser).SetUserID(99999).SetProjectID(legacyGroup.ID).SetScopes([]string{"read_channels", "write_requests"}).SaveX(ctx)
	la, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	lb, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	nodes := map[string]string{"a": "http://" + la.Addr().String(), "b": "http://" + lb.Addr().String()}
	bindings := map[int]cluster.ChannelBinding{ca.ID: {Owner: "a", Identity: "account-a"}, cb.ID: {Owner: "b", Identity: "account-b"}, videoChannel.ID: {Owner: "b", Identity: "video-b"}, doubaoChannel.ID: {Owner: "b", Identity: "doubao-b"}, lagVideoChannel.ID: {Owner: "a", Identity: "video-a"}}
	bindings[resetChannel.ID] = cluster.ChannelBinding{Owner: "b", Identity: "reset-b"}
	bindings[fallback.ID] = cluster.ChannelBinding{Owner: "b", Identity: "fallback-b"}
	cache := xcache.Config{Mode: xcache.ModeRedis, Namespace: schema, Redis: xredis.Config{URL: redisURL}}
	bootstrap := biz.NewSystemService(biz.SystemServiceParams{Ent: a, CacheConfig: cache})
	require.NoError(t, bootstrap.SetSecretKey(ctx, "synthetic-cluster-jwt-key"))
	cleanup := []biz.CleanupOption{{ResourceType: "requests", Enabled: false, CleanupDays: 7}}
	require.NoError(t, bootstrap.SetStoragePolicy(ctx, &biz.StoragePolicy{StoreRequestBody: true, StoreExecutionRequestBody: new(true), StoreResponseBody: true, StoreChunks: true, LivePreview: true, CleanupOptions: cleanup}))
	type instance struct {
		server   *httptest.Server
		channels *biz.ChannelService
		requests *biz.RequestService
		system   *biz.SystemService
		auth     *biz.AuthService
		writer   *biz.ForwardingObservationWriter
		scan     func(context.Context)
	}
	build := func(client *ent.Client, id string, listener net.Listener) *instance {
		node := cluster.Config{Enabled: true, NodeID: id, ControlNode: "a", Nodes: nodes, Channels: bindings}
		system := biz.NewSystemService(biz.SystemServiceParams{Ent: client, CacheConfig: cache})
		projects := biz.NewProjectService(biz.ProjectServiceParams{Ent: client, CacheConfig: cache})
		users := biz.NewUserService(biz.UserServiceParams{Ent: client, CacheConfig: cache})
		keys := biz.NewAPIKeyService(biz.APIKeyServiceParams{Ent: client, CacheConfig: cache, ProjectService: projects, KeyPrefix: "ah"})
		t.Cleanup(keys.Stop)
		auth := biz.NewAuthService(biz.AuthServiceParams{Ent: client, SystemService: system, APIKeyService: keys, UserService: users})
		hc := httpclient.NewHttpClient()
		t.Cleanup(hc.CloseIdleConnections)
		channels := biz.NewChannelService(biz.ChannelServiceParams{Ent: client, CacheConfig: cache, SystemService: system, HttpClient: hc, Node: node})
		t.Cleanup(channels.Stop)
		models := biz.NewModelService(biz.ModelServiceParams{Ent: client, SystemService: system, ChannelService: channels})
		storage := biz.NewDataStorageService(biz.DataStorageServiceParams{Client: client, CacheConfig: cache, SystemService: system})
		usage := biz.NewUsageLogService(client, system, channels)
		live := biz.NewLiveStreamRegistry()
		body := biz.NewManagedRequestBodyWriter(biz.ManagedRequestBodyWriterConfig{}, client, system)
		writer := biz.NewForwardingObservationWriter(biz.ManagedRequestBodyWriterConfig{})
		requests := biz.NewRequestServiceWithObservationWriter(client, system, usage, storage, live, body, writer)
		require.NoError(t, body.Start(ctx))
		require.NoError(t, writer.Start(ctx))
		t.Cleanup(func() {
			stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, writer.Stop(stop))
			require.NoError(t, body.Stop(stop))
		})
		quota := biz.NewQuotaServiceWithObservationWriter(client, system, writer)
		prompts := biz.NewPromptService(biz.PromptServiceParams{Ent: client})
		protection := biz.NewPromptProtectionRuleService(biz.PromptProtectionRuleServiceParams{Ent: client, CacheConfig: cache})
		t.Cleanup(protection.Stop)
		selector := orchestrator.NewDefaultSelector(channels, models, system)
		videos := biz.NewVideoService(channels, requests)
		openai := api.NewOpenAIHandlers(api.OpenAIHandlersParams{VideoService: videos, AuthService: auth, ChannelService: channels, ModelService: models, DefaultSelector: selector, RequestService: requests, SystemService: system, UsageLogService: usage, PromptService: prompts, PromptProtectionRuleService: protection, QuotaService: quota, HttpClient: hc, LiveStreamRegistry: live, ChannelLimiterManager: orchestrator.NewChannelLimiterManager(), Client: client})
		doubao := api.NewDoubaoHandlers(api.DoubaoHandlersParams{VideoService: videos, ChannelService: channels, ModelService: models, DefaultSelector: selector, RequestService: requests, SystemService: system, UsageLogService: usage, PromptService: prompts, PromptProtectionRuleService: protection, QuotaService: quota, HttpClient: hc, LiveStreamRegistry: live, ChannelLimiterManager: orchestrator.NewChannelLimiterManager()})
		playground := api.NewPlaygroundHandlers(api.PlaygroundHandlersParams{ChannelService: channels, ModelService: models, DefaultSelector: selector, RequestService: requests, SystemService: system, UsageLogService: usage, PromptService: prompts, PromptProtectionRuleService: protection, QuotaService: quota, HttpClient: hc, LiveStreamRegistry: live, ChannelLimiterManager: orchestrator.NewChannelLimiterManager()})
		executor := &clusterScanExecutor{}
		schedule := scheduler.New(executor)
		worker := video_storage.NewWorker(video_storage.Params{Ent: client, SystemService: system, DataStorageService: storage, VideoService: videos})
		require.NoError(t, worker.RegisterScheduledTasks(ctx, schedule))
		tester := orchestrator.NewTestChannelOrchestrator(channels, requests, system, usage, protection, hc)
		quotaHTTP := httpclient.NewHttpClientWithClient(&http.Client{Transport: clusterTestTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host != "chatgpt.com" {
				return nil, fmt.Errorf("unexpected quota host %s", r.URL.Host)
			}
			if id != "b" {
				controlQuotaCalls.Add(1)
				return nil, errors.New("non-owner quota I/O")
			}
			raw := `{}`
			if strings.HasSuffix(r.URL.Path, "/rate-limit-reset-credits") {
				ownerCreditGets.Add(1)
				raw = `{"credits":[{"id":"synthetic-credit","status":"available"}],"available_count":1}`
			} else if r.Method == http.MethodPost {
				ownerResets.Add(1)
				if unknownReset.Load() {
					return nil, io.EOF
				}
				raw = `{"code":"reset","windows_reset":1}`
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw))}, nil
		})})
		t.Cleanup(quotaHTTP.CloseIdleConnections)
		providerQuota := biz.NewProviderQuotaService(biz.ProviderQuotaServiceParams{Ent: client, SystemService: system, HttpClient: quotaHTTP, Node: node})
		admin := gql.NewGraphqlHandlers(gql.Dependencies{ProviderQuotaService: providerQuota, Ent: client, AuthService: auth, APIKeyService: keys, UserService: users, SystemService: system, ProjectService: projects, ChannelService: channels, RequestService: requests, DataStorageService: storage, UsageLogService: usage, ModelService: models, HttpClient: hc})
		traces := biz.NewTraceService(biz.TraceServiceParams{Ent: client, RequestService: requests})
		gateway := New(Config{Debug: true, RequestTimeout: 5 * time.Second, LLMRequestTimeout: 5 * time.Second})
		ip, err := middleware.NewIPAccessControlConfig(false, nil, "")
		require.NoError(t, err)
		SetupRoutes(gateway, Handlers{OpenAI: openai, Doubao: doubao, Playground: playground, Graphql: admin, Cluster: api.NewClusterHandlers(models, channels, tester, videos, providerQuota)}, client, Services{TraceService: traces, ThreadService: biz.NewThreadService(client, traces), AuthService: auth, SystemService: system, Node: node}, ip)
		server := httptest.NewUnstartedServer(gateway.Engine)
		server.Listener = listener
		server.Start()
		t.Cleanup(server.Close)
		return &instance{server, channels, requests, system, auth, writer, executor.scan}
	}
	ia := build(a, "a", la)
	requestHTTP := func(origin, method, path, credential, body string) (int, []byte) {
		r, err := http.NewRequest(method, origin+path, strings.NewReader(body))
		require.NoError(t, err)
		r.Header.Set("Authorization", "Bearer "+credential)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Channel-ID", fmt.Sprint(cb.ID))
		res, err := http.DefaultClient.Do(r)
		require.NoError(t, err)
		defer res.Body.Close()
		raw, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return res.StatusCode, raw
	}
	// The real control-plane mutation updates one shared policy, not a node-local
	// setting. Start B after the settings window so both writers admit with it.
	token, err := ia.auth.GenerateJWTToken(ctx, owner)
	require.NoError(t, err)
	code, raw := requestHTTP(nodes["a"], "POST", "/admin/graphql", token, `{"query":"mutation { updateStoragePolicy(input:{storeRequestBody:false,storeExecutionRequestBody:false,storeResponseBody:false,storeChunks:false,livePreview:false}) }"}`)
	require.Equal(t, 200, code, string(raw))
	require.Contains(t, string(raw), `"updateStoragePolicy":true`)
	require.NotContains(t, string(raw), `"errors"`)
	ib := build(b, "b", lb)
	for _, instance := range []*instance{ia, ib} {
		policy, err := instance.system.StoragePolicyFresh(ctx)
		require.NoError(t, err)
		require.False(t, policy.StoreRequestBody)
		require.NotNil(t, policy.StoreExecutionRequestBody)
		require.False(t, *policy.StoreExecutionRequestBody)
		require.False(t, policy.StoreResponseBody)
		require.False(t, policy.StoreChunks)
		require.False(t, policy.LivePreview)
		require.Equal(t, cleanup, policy.CleanupOptions, "the five-field patch preserves existing cleanup settings")
	}
	require.NotSame(t, ia.channels, ib.channels)
	require.Len(t, ia.channels.GetEnabledChannels(), 5)
	require.Len(t, ib.channels.GetEnabledChannels(), 5)
	require.Nil(t, ia.channels.GetEnabledChannel(cb.ID).Outbound)
	require.NotNil(t, ib.channels.GetEnabledChannel(cb.ID).Outbound)
	code, raw = requestHTTP(nodes["a"], "POST", "/admin/graphql", token, `{"query":"query { storagePolicy { storeRequestBody storeExecutionRequestBody storeResponseBody storeChunks livePreview } }"}`)
	require.Equal(t, 200, code, string(raw))
	require.JSONEq(t, `{"data":{"storagePolicy":{"storeRequestBody":false,"storeExecutionRequestBody":false,"storeResponseBody":false,"storeChunks":false,"livePreview":false}}}`, string(raw))
	code, raw = requestHTTP(nodes["a"], "GET", "/v1/axon/route?model=alias", key.Key, "")
	require.Equal(t, 200, code, string(raw))
	require.Contains(t, string(raw), `"owner":"b"`)
	code, raw = requestHTTP(nodes["a"], "POST", "/v1/responses", key.Key, `{"model":"alias","input":"hello"}`)
	require.NotEqual(t, 200, code, string(raw))
	require.Zero(t, calls.Load(), "a forged channel header cannot select a remote provider")
	code, raw = requestHTTP(nodes["b"], "POST", "/v1/responses", key.Key, `{"model":"alias","input":"hello"}`)
	require.Equal(t, 200, code, string(raw))
	require.Equal(t, int32(1), calls.Load())
	code, raw = requestHTTP(nodes["a"], "GET", "/admin/cluster/manifest", token, "")
	require.Equal(t, 200, code, string(raw))
	require.Contains(t, string(raw), `"model":"alias"`)
	code, _ = requestHTTP(nodes["b"], "GET", "/admin/cluster/manifest", token, "")
	require.Equal(t, 404, code)
	// A public alias can be globally ambiguous yet definitive for each fresh Key.
	aliasKey := a.APIKey.Create().SetName("alias-other").SetKey("ah-cluster-alias-other").SetType(apikey.TypeUser).SetUserID(99999).SetProjectID(legacyGroup.ID).SetScopes([]string{"read_channels", "write_requests"}).SetProfiles(&objects.APIKeyProfiles{ActiveProfile: "active", Profiles: []objects.APIKeyProfile{{Name: "active", ModelIDs: []string{"alias"}, ModelMappings: []objects.ModelMapping{{From: "alias", To: "other"}}}}}).SaveX(ctx)
	code, raw = requestHTTP(nodes["a"], "GET", "/admin/cluster/manifest", token, "")
	require.Equal(t, 200, code, string(raw))
	require.Contains(t, string(raw), `"model":"alias","owners":["a","b"],"status":"ambiguous"`)
	code, raw = requestHTTP(nodes["a"], "GET", "/v1/axon/route?model=alias", key.Key, "")
	require.Equal(t, 200, code, string(raw))
	require.Contains(t, string(raw), `"owner":"b"`)
	code, raw = requestHTTP(nodes["a"], "GET", "/v1/axon/route?model=alias", aliasKey.Key, "")
	require.Equal(t, 200, code, string(raw))
	require.Contains(t, string(raw), `"owner":"a"`)
	a.APIKey.UpdateOne(aliasKey).SetStatus(apikey.StatusDisabled).SaveX(ctx)
	// Same-Key cross-owner candidates remain a precise 409 until placement aligns.
	a.Channel.UpdateOne(ca).SetSupportedModels([]string{"other", "m"}).SaveX(ctx)
	require.NoError(t, ia.channels.ReloadEnabledChannelsCache(ctx))
	code, raw = requestHTTP(nodes["a"], "GET", "/v1/axon/route?model=alias", key.Key, "")
	require.Equal(t, 409, code, string(raw))
	a.Channel.UpdateOneID(ca.ID).SetSupportedModels([]string{"other"}).SaveX(ctx)
	require.NoError(t, ia.channels.ReloadEnabledChannelsCache(ctx))
	// Revoking the tag in PG is effective even with B's old provider snapshot.
	a.Channel.UpdateOne(cb).SetTags([]string{"revoked"}).SaveX(ctx)
	code, raw = requestHTTP(nodes["b"], "POST", "/v1/responses", key.Key, `{"model":"alias","input":"blocked"}`)
	require.NotEqual(t, 200, code, string(raw))
	require.Equal(t, int32(1), calls.Load())
	a.Channel.UpdateOneID(cb.ID).SetTags([]string{"allowed"}).SaveX(ctx)
	require.NoError(t, ib.channels.ReloadEnabledChannelsCache(ctx))
	// Next generation on an existing real WS reauthenticates against PG.
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(nodes["b"], "http")+"/v1/responses", http.Header{"Authorization": {"Bearer " + key.Key}})
	require.NoError(t, err)
	defer ws.Close()
	a.APIKey.UpdateOne(key).SetStatus(apikey.StatusDisabled).SaveX(ctx)
	require.NoError(t, ws.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"alias","input":"blocked"}`)))
	_, raw, err = ws.ReadMessage()
	require.NoError(t, err)
	require.Contains(t, string(raw), `"execution":"not_executed"`)
	require.Equal(t, int32(1), calls.Load())
	a.APIKey.UpdateOneID(key.ID).SetStatus(apikey.StatusEnabled).SaveX(ctx)
	a.UserProject.DeleteOne(member).ExecX(ctx)
	code, _ = requestHTTP(nodes["b"], "POST", "/v1/responses", key.Key, `{"model":"alias","input":"blocked"}`)
	require.NotEqual(t, 200, code)
	require.Equal(t, int32(1), calls.Load())
	code, raw = requestHTTP(nodes["b"], "POST", "/v1/responses", legacy.Key, `{"model":"m","input":"legacy"}`)
	require.Equal(t, 200, code, string(raw))
	require.Equal(t, int32(2), calls.Load())
	// A data-node restart cannot fail another owner's old processing request.
	old := a.Request.Create().SetProjectID(group.ID).SetModelID("m").SetRequestBody([]byte("{}")).SetStatus(request.StatusProcessing).SetCreatedAt(time.Now().Add(-2 * time.Hour)).SaveX(ctx)
	require.NoError(t, ib.requests.ClearStaleProcessingOnStartup(ctx))
	require.Equal(t, request.StatusProcessing, a.Request.GetX(ctx, old.ID).Status)
	require.NoError(t, ia.writer.Wait(ctx))
	require.NoError(t, ib.writer.Wait(ctx))
	// A real owner RPC is authenticated on B, not implemented with A's provider.
	code, raw = requestHTTP(nodes["a"], "POST", "/admin/graphql", token, fmt.Sprintf(`{"query":"mutation { testChannel(input:{channelID:\"gid://axonhub/Channel/%d\"}) { success } }"}`, cb.ID))
	require.Equal(t, 200, code, string(raw))
	require.NotContains(t, string(raw), `"errors"`, string(raw))
	require.Equal(t, int32(3), calls.Load())
	// Fresh scopes and JWT state are enforced by the real registered RPC chain.
	noScope := a.APIKey.Create().SetName("no-scope").SetKey("ah-cluster-no-scope").SetProjectID(group.ID).SetType(apikey.TypeServiceAccount).SetScopes([]string{}).SaveX(ctx)
	code, _ = requestHTTP(nodes["b"], "POST", "/__axon/openapi/channel-test", noScope.Key, fmt.Sprintf(`{"operation":"channel","channel_id":%d}`, cb.ID))
	require.Equal(t, 403, code)
	require.Equal(t, int32(3), calls.Load())
	a.User.UpdateOne(owner).SetStatus(user.StatusDeactivated).SaveX(ctx)
	code, _ = requestHTTP(nodes["b"], "POST", "/__axon/admin/channel-test", token, fmt.Sprintf(`{"operation":"channel","channel_id":%d}`, cb.ID))
	require.Equal(t, 401, code)
	require.Equal(t, int32(3), calls.Load())
	a.User.UpdateOneID(owner.ID).SetStatus(user.StatusActivated).SaveX(ctx)
	code, raw = requestHTTP(nodes["a"], "POST", fmt.Sprintf("/admin/playground/chat?channel_id=gid://axonhub/Channel/%d", cb.ID), token, `{"model":"m","messages":[{"role":"user","parts":[{"type":"text","text":"synthetic playground"}]}],"stream":true}`)
	require.Equal(t, 200, code, string(raw))
	require.Equal(t, int32(4), calls.Load(), "remote playground executes once on its owner")
	code, raw = requestHTTP(nodes["a"], "POST", "/admin/graphql", token, fmt.Sprintf(`{"query":"query { fetchModels(input:{channelID:\"gid://axonhub/Channel/%d\",channelType:\"openai_responses\",baseURL:\"%s\"}) { models { id } error } }"}`, cb.ID, upstream.URL))
	require.Equal(t, 200, code, string(raw))
	require.NotContains(t, string(raw), `"errors"`)
	require.Contains(t, string(raw), `"id":"m"`)
	code, raw = requestHTTP(nodes["a"], "POST", "/admin/graphql", token, fmt.Sprintf(`{"query":"mutation { syncChannelModels(channelID:\"gid://axonhub/Channel/%d\") { supportedModels } }"}`, cb.ID))
	require.Equal(t, 200, code, string(raw))
	require.NotContains(t, string(raw), `"errors"`)
	require.Equal(t, int32(2), modelCalls.Load(), "management fetch/sync execute on the credential owner")

	// The shared existing policy suppresses HTTP/SSE body, headers and chunks on
	// both database clients while actual usage still reaches the shared ledger.
	code, raw = requestHTTP(nodes["b"], "POST", "/v1/responses", legacy.Key, `{"model":"m","input":"body-must-stay-at-execution-node","stream":true}`)
	require.Equal(t, 200, code, string(raw))
	require.Contains(t, string(raw), "resp_test")
	require.NoError(t, ia.writer.Wait(ctx))
	require.NoError(t, ib.writer.Wait(ctx))
	for _, instance := range []*instance{ia, ib} {
		policy, err := instance.system.StoragePolicyFresh(ctx)
		require.NoError(t, err)
		require.False(t, policy.StoreRequestBody)
		require.False(t, *policy.StoreExecutionRequestBody)
		require.False(t, policy.StoreResponseBody)
		require.False(t, policy.StoreChunks)
		require.False(t, policy.LivePreview)
	}
	for _, observed := range a.Request.Query().AllX(ctx) {
		require.Contains(t, []string{"", "{}"}, string(observed.RequestBody))
		require.Contains(t, []string{"", "{}"}, string(observed.RequestHeaders))
		require.Empty(t, observed.ResponseBody)
		require.Empty(t, observed.ResponseChunks)
		require.Nil(t, observed.RequestBodyPayloadID)
	}
	for _, execution := range b.RequestExecution.Query().AllX(ctx) {
		require.Contains(t, []string{"", "{}"}, string(execution.RequestBody))
		require.Contains(t, []string{"", "{}"}, string(execution.RequestHeaders))
		require.Empty(t, execution.ResponseBody)
		require.Empty(t, execution.ResponseChunks)
		require.Nil(t, execution.RequestBodyPayloadID)
	}
	require.Zero(t, a.ObservabilityPayload.Query().CountX(ctx))
	require.Positive(t, b.UsageLog.Query().CountX(ctx))
	require.Zero(t, a.PendingUsage.Query().CountX(ctx))

	// Both video protocols create at B; task operations landing at A use durable
	// channel ownership and reauthenticate at B, including old A-owned tasks.
	a.UserProject.Create().SetUserID(personal.ID).SetProjectID(group.ID).SaveX(ctx)
	for _, create := range []struct{ path, body, id string }{
		{"/v1/videos", `{"model":"video","prompt":"synthetic"}`, "opaque-openai"},
		{"/doubao/v3/contents/generations/tasks", `{"model":"seedance","content":[{"type":"text","text":"synthetic"}]}`, "opaque-doubao"},
	} {
		code, raw = requestHTTP(nodes["b"], "POST", create.path, key.Key, create.body)
		require.Equal(t, 200, code, string(raw))
		require.Contains(t, string(raw), create.id)
	}
	require.NoError(t, ib.writer.Wait(ctx))
	remote := a.Request.Query().Where(request.ExternalID("opaque-openai")).OnlyX(ctx)
	require.Equal(t, videoChannel.ID, remote.ChannelID)
	local := a.Request.Create().SetProjectID(legacyGroup.ID).SetAPIKeyID(legacy.ID).SetChannelID(lagVideoChannel.ID).SetModelID("lag-video").SetExternalID("old-lag-task").SetFormat("openai/video").SetRequestBody([]byte("{}")).SetStatus(request.StatusProcessing).SaveX(ctx)
	beforeVideos := videoCalls.Load()
	for _, protocol := range []string{"openai", "doubao"} {
		code, raw = requestHTTP(nodes["a"], "GET", "/v1/axon/task-route?protocol="+protocol+"&method=GET&task_id=opaque-openai", key.Key, "")
		require.Equal(t, 200, code, string(raw))
		require.Contains(t, string(raw), `"owner":"b"`)
	}
	require.Equal(t, beforeVideos, videoCalls.Load(), "owner resolution never contacts the provider")
	for _, path := range []string{"/v1/videos/opaque-openai", "/doubao/v3/contents/generations/tasks/opaque-openai", "/doubao/v3/contents/generations/tasks/opaque-doubao"} {
		code, raw = requestHTTP(nodes["a"], "GET", path, key.Key, "")
		require.Equal(t, 200, code, string(raw))
	}
	code, raw = requestHTTP(nodes["b"], "GET", "/v1/videos/old-lag-task", legacy.Key, "")
	require.Equal(t, 200, code, string(raw))
	code, raw = requestHTTP(nodes["b"], "GET", "/v1/axon/task-route?protocol=openai&method=GET&task_id=old-lag-task", legacy.Key, "")
	require.Equal(t, 200, code, string(raw))
	require.Contains(t, string(raw), `"owner":"a"`)

	otherUser := a.User.Create().SetEmail("other@example.test").SetPassword("synthetic").SetAccessMode(user.AccessModePortal).SetStatus(user.StatusActivated).SaveX(ctx)
	a.UserProject.Create().SetUserID(otherUser.ID).SetProjectID(group.ID).SaveX(ctx)
	otherKey := a.APIKey.Create().SetName("other").SetKey("ah-cluster-other").SetType(apikey.TypePersonal).SetPortalManaged(true).SetUserID(otherUser.ID).SetProjectID(group.ID).SetScopes([]string{"read_channels", "write_requests"}).SaveX(ctx)
	beforeVideos = videoCalls.Load()
	for _, origin := range []string{nodes["a"], nodes["b"]} {
		for _, path := range []string{"/v1/videos/opaque-openai", "/v1/axon/task-route?protocol=openai&method=GET&task_id=opaque-openai"} {
			code, raw = requestHTTP(origin, "GET", path, otherKey.Key, "")
			require.Equal(t, 404, code, string(raw))
			require.NotContains(t, string(raw), "opaque-openai")
		}
	}
	a.Channel.UpdateOne(videoChannel).SetTags([]string{"revoked"}).SaveX(ctx)
	code, _ = requestHTTP(nodes["a"], "DELETE", "/v1/videos/opaque-openai", key.Key, "")
	require.Equal(t, 404, code)
	a.Channel.UpdateOneID(videoChannel.ID).SetTags([]string{"allowed"}).SetDefaultTestModel("video").SaveX(ctx)
	a.APIKey.UpdateOneID(key.ID).SetStatus(apikey.StatusDisabled).SaveX(ctx)
	code, _ = requestHTTP(nodes["a"], "GET", "/v1/videos/opaque-openai", key.Key, "")
	require.Equal(t, 401, code)
	a.APIKey.UpdateOneID(key.ID).SetStatus(apikey.StatusEnabled).SaveX(ctx)
	require.Equal(t, beforeVideos, videoCalls.Load(), "denials must not touch upstream")

	// The registered background callbacks partition by the persisted channel ID.
	// A never polls B's tasks; B polls and saves exactly its two tasks.
	directory := t.TempDir()
	store := a.DataStorage.Create().SetName("video-files").SetDescription("synthetic").SetType(datastorage.TypeFs).SetSettings(&objects.DataStorageSettings{Directory: &directory}).SaveX(ctx)
	require.NoError(t, ia.system.SetVideoStorageSettings(ctx, biz.VideoStorageSettings{Enabled: true, DataStorageID: store.ID, ScanLimit: 20}))
	completeVideos.Store(true)
	ia.scan(ctx)
	require.Equal(t, beforeVideos+1, videoCalls.Load())
	require.True(t, a.Request.GetX(ctx, local.ID).ContentSaved)
	require.False(t, a.Request.GetX(ctx, remote.ID).ContentSaved)
	ib.scan(ctx)
	require.Equal(t, beforeVideos+3, videoCalls.Load())
	require.True(t, a.Request.GetX(ctx, remote.ID).ContentSaved)
	ia.scan(ctx)
	ib.scan(ctx)
	require.Equal(t, beforeVideos+3, videoCalls.Load(), "saved tasks are not downloaded twice")

	for _, path := range []string{"/v1/videos/opaque-openai", "/doubao/v3/contents/generations/tasks/opaque-doubao"} {
		code, raw = requestHTTP(nodes["a"], "DELETE", path, key.Key, "")
		require.Equal(t, 204, code, string(raw))
	}
	require.Equal(t, int32(2), deletes.Load(), "each DELETE is sent once to the actual owner")
	require.NoError(t, ib.writer.Wait(ctx))
	// Existing terminal-state fencing preserves completed history after deletion.
	require.Equal(t, request.StatusCompleted, a.Request.GetX(ctx, remote.ID).Status)
	active := a.Request.Create().SetProjectID(group.ID).SetAPIKeyID(key.ID).SetChannelID(videoChannel.ID).SetModelID("video").SetExternalID("active-task").SetFormat("openai/video").SetRequestBody([]byte("{}")).SetStatus(request.StatusProcessing).SaveX(ctx)
	code, raw = requestHTTP(nodes["a"], "DELETE", "/v1/videos/active-task", key.Key, "")
	require.Equal(t, 204, code, string(raw))
	require.NoError(t, ib.writer.Wait(ctx))
	require.Equal(t, request.StatusCanceled, a.Request.GetX(ctx, active.ID).Status)
	require.Equal(t, int32(3), deletes.Load())
	a.Request.Create().SetProjectID(group.ID).SetAPIKeyID(key.ID).SetChannelID(videoChannel.ID).SetModelID("video").SetExternalID("unknown-delete").SetFormat("openai/video").SetRequestBody([]byte("{}")).SetStatus(request.StatusProcessing).SaveX(ctx)
	code, raw = requestHTTP(nodes["a"], "DELETE", "/v1/videos/unknown-delete", key.Key, "")
	require.Equal(t, 500, code, string(raw))
	require.Equal(t, int32(4), deletes.Load(), "an upstream-delivered DELETE with an unknown result is never replayed")

	t.Run("disabled management test keeps original scope and owner", func(t *testing.T) {
		before := calls.Load()
		a.Channel.UpdateOneID(cb.ID).SetStatus(channel.StatusDisabled).SaveX(ctx)
		code, raw := requestHTTP(nodes["b"], "POST", "/v1/responses", legacy.Key, `{"model":"m","input":"blocked"}`)
		require.NotEqual(t, 200, code, string(raw))
		require.Equal(t, before, calls.Load())
		code, raw = requestHTTP(nodes["a"], "POST", "/admin/graphql", token, fmt.Sprintf(`{"query":"mutation { testChannel(input:{channelID:\"gid://axonhub/Channel/%d\"}) { success } }"}`, cb.ID))
		require.Equal(t, 200, code, string(raw))
		require.Contains(t, string(raw), `"success":true`)
		require.Equal(t, before+1, calls.Load())
		a.Channel.UpdateOneID(cb.ID).SetStatus(channel.StatusEnabled).SaveX(ctx)
	})

	t.Run("quota reset only on owner", func(t *testing.T) {
		query := fmt.Sprintf(`{"query":"mutation { resetChannelQuotaNow(channelID:\"gid://axonhub/Channel/%d\") }"}`, resetChannel.ID)
		code, raw := requestHTTP(nodes["a"], "POST", "/admin/graphql", token, query)
		require.Equal(t, 200, code, string(raw))
		require.Contains(t, string(raw), `"resetChannelQuotaNow":true`)
		require.Zero(t, controlQuotaCalls.Load())
		require.Equal(t, int32(1), ownerResets.Load())
		require.Equal(t, int32(1), ownerCreditGets.Load())
		for _, origin := range []string{nodes["a"], nodes["b"]} {
			code, _ = requestHTTP(origin, "POST", "/__axon/openapi/channel-quota-reset", noScope.Key, fmt.Sprintf(`{"channel_id":%d}`, resetChannel.ID))
			require.Equal(t, 403, code)
		}
		a.User.UpdateOneID(owner.ID).SetStatus(user.StatusDeactivated).SaveX(ctx)
		code, _ = requestHTTP(nodes["b"], "POST", "/__axon/admin/channel-quota-reset", token, fmt.Sprintf(`{"channel_id":%d}`, resetChannel.ID))
		require.Equal(t, 401, code)
		a.User.UpdateOneID(owner.ID).SetStatus(user.StatusActivated).SaveX(ctx)
		require.Equal(t, int32(1), ownerResets.Load())
		unknownReset.Store(true)
		code, raw = requestHTTP(nodes["a"], "POST", "/admin/graphql", token, query)
		require.Equal(t, 200, code)
		require.Contains(t, string(raw), `"errors"`)
		require.Equal(t, int32(2), ownerResets.Load(), "unknown consume result is not replayed")
		require.Zero(t, controlQuotaCalls.Load())
	})

	// F1: only the pending create is unavailable; all original request/terminal
	// persistence stays real. The recovery job must own the very same fact.
	a.Channel.UpdateOneID(fallback.ID).SetStatus(channel.StatusEnabled).SaveX(ctx)
	require.NoError(t, ib.channels.ReloadEnabledChannelsCache(ctx))
	retry := ib.system.RetryPolicyOrDefault(ctx)
	retry.Enabled, retry.MaxChannelRetries, retry.MaxSingleChannelRetries, retry.RetryDelayMs = true, 2, 2, 0
	require.NoError(t, ib.system.SetRetryPolicy(ctx, retry))
	var unavailable atomic.Bool
	var publicationAttempts atomic.Int32
	var factsMu sync.Mutex
	frozenFacts := map[string]string{}
	b.PendingUsage.Use(func(next ent.Mutator) ent.Mutator {
		return hook.PendingUsageFunc(func(ctx context.Context, m *ent.PendingUsageMutation) (ent.Value, error) {
			if m.Op().Is(ent.OpCreate) {
				publicationAttempts.Add(1)
				id, _ := m.ID()
				stamp, _ := m.CapturedAt()
				tokens, _ := m.TotalTokens()
				cost, hasCost := m.TotalCost()
				usage, _ := m.Usage()
				items, _ := m.CostItems()
				price, _ := m.PriceReferenceID()
				raw, marshalErr := json.Marshal([]any{stamp, tokens, cost, hasCost, usage, items, price})
				if marshalErr != nil {
					return nil, marshalErr
				}
				factsMu.Lock()
				if frozen, exists := frozenFacts[id]; exists && frozen != string(raw) {
					t.Errorf("usage %s changed across publication retries", id)
				} else {
					frozenFacts[id] = string(raw)
				}
				factsMu.Unlock()
				if unavailable.Load() {
					return nil, errors.New("synthetic pending publication unavailable")
				}
			}
			return next.Mutate(ctx, m)
		})
	})
	for _, protocol := range []string{"http", "sse", "ws", "chat_sse"} {
		t.Run("executed accounting failure "+protocol, func(t *testing.T) {
			require.NoError(t, ib.writer.Wait(ctx))
			if protocol == "chat_sse" {
				a.Channel.UpdateOneID(cb.ID).SetStatus(channel.StatusDisabled).SaveX(ctx)
				a.Channel.UpdateOneID(fallback.ID).SetType(channel.TypeOpenai).SaveX(ctx)
				require.NoError(t, ib.channels.ReloadEnabledChannelsCache(ctx))
			}
			beforeCalls, beforeUsage := calls.Load(), a.UsageLog.Query().CountX(ctx)
			beforeIDs := a.Request.Query().IDsX(ctx)
			beforeAttempts := publicationAttempts.Load()
			unavailable.Store(true)
			defer unavailable.Store(false)
			if protocol == "ws" {
				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(nodes["b"], "http")+"/v1/responses", http.Header{"Authorization": {"Bearer " + legacy.Key}})
				require.NoError(t, err)
				defer conn.Close()
				require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
				require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"m","input":"accounting"}`)))
				for {
					_, raw, err = conn.ReadMessage()
					require.NoError(t, err)
					require.NotContains(t, string(raw), `"execution":"not_executed"`)
					require.NotContains(t, string(raw), `"type":"response.completed"`)
					var event map[string]any
					require.NoError(t, json.Unmarshal(raw, &event))
					if event["type"] == "response.failed" || event["type"] == "error" {
						require.Contains(t, string(raw), `"code":"accounting_failed"`)
						break
					}
				}
			} else {
				body := `{"model":"m","input":"accounting"}`
				if protocol == "sse" {
					body = `{"model":"m","input":"accounting","stream":true}`
				}
				path := "/v1/responses"
				if protocol == "chat_sse" {
					path, body = "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"accounting"}],"stream":true}`
				}
				code, raw = requestHTTP(nodes["b"], "POST", path, legacy.Key, body)
				if protocol == "http" {
					require.Equal(t, 500, code, string(raw))
				} else {
					require.Equal(t, 200, code, "the partial stream is already established")
					require.Contains(t, string(raw), "partial")
					require.True(t, strings.Contains(string(raw), "response.failed") || strings.Contains(string(raw), "event:error"), string(raw))
					require.Contains(t, string(raw), "accounting")
					require.NotContains(t, string(raw), "Internal server error")
					require.NotContains(t, string(raw), `"type":"response.completed"`)
					require.NotContains(t, string(raw), "[DONE]")
				}
				require.NotContains(t, string(raw), `"execution":"not_executed"`)
			}
			require.Equal(t, beforeCalls+1, calls.Load(), "accounting failure must not replay provider")
			require.Zero(t, a.PendingUsage.Query().CountX(ctx))
			require.Equal(t, beforeUsage, a.UsageLog.Query().CountX(ctx))
			unavailable.Store(false)
			drain, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			require.NoError(t, ib.writer.Wait(drain))
			require.Equal(t, beforeUsage+1, a.UsageLog.Query().CountX(ctx))
			require.Zero(t, a.PendingUsage.Query().CountX(ctx))
			require.Equal(t, beforeCalls+1, calls.Load())
			created := a.Request.Query().Where(request.IDNotIn(beforeIDs...)).OnlyX(ctx)
			require.Equal(t, request.StatusFailed, created.Status)
			usage := a.UsageLog.Query().Order(ent.Desc("id")).FirstX(ctx)
			require.Equal(t, int64(5), usage.TotalTokens)
			require.Nil(t, usage.TotalCost, "unknown price must remain NULL")
			require.Greater(t, publicationAttempts.Load()-beforeAttempts, int32(1), "same fact must be retried after recovery")
			t.Logf("%s provider=1 recovered usage=1 pending=0 terminal=failed publication_attempts=%d", protocol, publicationAttempts.Load()-beforeAttempts)
		})
	}
	factsMu.Lock()
	require.Len(t, frozenFacts, 4, "one stable usage ID per executed generation")
	factsMu.Unlock()
}

type clusterTestTransport func(*http.Request) (*http.Response, error)

func (f clusterTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Capture the actual scheduled worker callback without waiting a minute or
// adding a production test-only entry point. All other executor methods are idle.
type clusterScanExecutor struct {
	executors.ScheduledExecutor
	scan func(context.Context)
}

func (e *clusterScanExecutor) ScheduleFuncAtFixRate(fn func(context.Context), _ time.Duration) (executors.CancelFunc, error) {
	e.scan = fn
	return func() {}, nil
}
