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
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/samber/lo"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/migrate"
	"github.com/looplj/axonhub/internal/ent/pendingusage"
	"github.com/looplj/axonhub/internal/ent/promptprotectionrule"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/metrics"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/pkg/xredis"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/internal/server/gql"
	"github.com/looplj/axonhub/internal/server/maintenance"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

type redisCommandCounterHook struct {
	systemGets       atomic.Int64
	cyberBlockOps    atomic.Int64
	protectionOps    atomic.Int64
}

func (h *redisCommandCounterHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *redisCommandCounterHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func (h *redisCommandCounterHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		name := strings.ToLower(cmd.Name())
		args := cmd.Args()
		if len(args) > 1 {
			key := fmt.Sprint(args[1])
			if name == "get" && strings.Contains(key, ":cache:system:") {
				h.systemGets.Add(1)
			}
			if strings.Contains(key, "cyber-session-block:") {
				h.cyberBlockOps.Add(1)
			}
			if strings.Contains(key, "prompt_protection") {
				h.protectionOps.Add(1)
			}
		}
		return next(ctx, cmd)
	}
}

type sqlQueryInterceptDriver struct {
	dialect.Driver
	systemRetrySelects atomic.Int64
	requestWrites      atomic.Int64
	pendingWrites      atomic.Int64
	traceThreadWrites  atomic.Int64
}

func (d *sqlQueryInterceptDriver) Query(ctx context.Context, query string, args, v any) error {
	qLower := strings.ToLower(query)
	if strings.Contains(qLower, "systems") && !strings.Contains(qLower, "pg_notify") {
		d.systemRetrySelects.Add(1)
	}
	d.observeMutationSQL(qLower)
	return d.Driver.Query(ctx, query, args, v)
}

func (d *sqlQueryInterceptDriver) Exec(ctx context.Context, query string, args, v any) error {
	d.observeMutationSQL(strings.ToLower(query))
	return d.Driver.Exec(ctx, query, args, v)
}

func (d *sqlQueryInterceptDriver) PrimaryDriver() dialect.Driver {
	return d.Driver
}

func (d *sqlQueryInterceptDriver) observeMutationSQL(qLower string) {
	if strings.Contains(qLower, "insert into") || strings.Contains(qLower, "update ") {
		if strings.Contains(qLower, `"requests"`) || strings.Contains(qLower, `"request_executions"`) {
			d.requestWrites.Add(1)
		}
		if strings.Contains(qLower, `"pending_usages"`) {
			d.pendingWrites.Add(1)
		}
		if strings.Contains(qLower, `"traces"`) || strings.Contains(qLower, `"threads"`) {
			d.traceThreadWrites.Add(1)
		}
	}
}

func containsArg(args any, target string) bool {
	slice, ok := args.([]any)
	if !ok {
		return false
	}
	for _, item := range slice {
		if fmt.Sprint(item) == target {
			return true
		}
	}
	return false
}

func TestClusterCompactAccountingAndLocalSystemSnapshotPG18(t *testing.T) {
	dsn, redisURL := os.Getenv("AXONHUB_TEST_PG_DSN"), os.Getenv("AXONHUB_TEST_REDIS_URL")
	if dsn == "" || redisURL == "" || os.Getenv("AXONHUB_TEST_PG_DISPOSABLE") != "1" {
		t.Skip("requires disposable PG18 and Redis")
	}
	gin.SetMode(gin.TestMode)

	meterProvider := sdkmetric.NewMeterProvider()
	require.NoError(t, metrics.SetupMetrics(meterProvider, "compact-cluster-test"))
	t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })

	root, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	schema := "compact_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = root.Exec("CREATE SCHEMA " + schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = root.Exec("DROP SCHEMA " + schema + " CASCADE"); _ = root.Close() })

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	scopedDSN := dsn + sep + "search_path=" + schema

	openPlain := func() *ent.Client {
		db, openErr := sql.Open("pgx", scopedDSN)
		require.NoError(t, openErr)
		db.SetMaxOpenConns(6)
		db.SetMaxIdleConns(2)
		return ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	}
	openIntercepted := func() (*ent.Client, *sqlQueryInterceptDriver) {
		db, openErr := sql.Open("pgx", scopedDSN)
		require.NoError(t, openErr)
		db.SetMaxOpenConns(6)
		db.SetMaxIdleConns(2)
		base := entsql.OpenDB(dialect.Postgres, db)
		intercept := &sqlQueryInterceptDriver{Driver: base}
		return ent.NewClient(ent.Driver(intercept)), intercept
	}

	clientA := openPlain()
	clientB, bSQLMetrics := openIntercepted()
	t.Cleanup(func() { _ = clientA.Close(); _ = clientB.Close() })

	ctx := authz.WithTestBypass(context.Background())
	require.NoError(t, clientA.Schema.Create(ctx, migrate.WithForeignKeys(false)))

	proj := clientA.Project.Create().SetName("compact-group").SaveX(ctx)
	adminUser := clientA.User.Create().SetEmail("admin@compact.test").SetPassword("synthetic").SetIsOwner(true).SetStatus(user.StatusActivated).SaveX(ctx)
	clientA.DataStorage.Create().SetName("primary").SetDescription("pg").SetType(datastorage.TypeDatabase).SetPrimary(true).SetSettings(&objects.DataStorageSettings{}).SaveX(ctx)

	var upstreamFailMode atomic.Bool
	var upstreamCyberRefusal atomic.Bool
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		rawBody, _ := io.ReadAll(r.Body)
		if upstreamCyberRefusal.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"cybersecurity policy violation: malicious exploit request","type":"invalid_request_error","code":"cyber_policy"}}`)
			return
		}
		if upstreamFailMode.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"error":{"message":"sensitive internal upstream detail","type":"server_error","code":"upstream_502"}}`)
			return
		}
		if strings.Contains(string(rawBody), `"stream":true`) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_stream\",\"object\":\"response\",\"model\":\"priced-model\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
			fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"ok\"}\n\n")
			w.(http.Flusher).Flush()
			fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_stream\",\"object\":\"response\",\"model\":\"priced-model\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":10,\"output_tokens\":5,\"total_tokens\":15}}}\n\n")
			return
		}
		model := "priced-model"
		if strings.Contains(string(rawBody), `"unpriced-model"`) {
			model = "unpriced-model"
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"resp_sync","object":"response","model":%q,"status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`, model)
	}))
	defer upstream.Close()

	chB := clientA.Channel.Create().
		SetName("node-b-channel").
		SetStatus(channel.StatusEnabled).
		SetType(channel.TypeOpenaiResponses).
		SetBaseURL(upstream.URL).
		SetCredentials(objects.ChannelCredentials{APIKey: "sk-synthetic-b"}).
		SetSupportedModels([]string{"priced-model", "unpriced-model"}).
		SetDefaultTestModel("priced-model").
		SaveX(ctx)

	// Configure model pricing ONLY for priced-model; unpriced-model intentionally has no price -> total_cost = NULL.
	inputPrice := decimal.RequireFromString("0.001")
	outputPrice := decimal.RequireFromString("0.002")
	clientA.ChannelModelPrice.Create().
		SetChannelID(chB.ID).
		SetModelID("priced-model").
		SetReferenceID("ref-priced-v1").
		SetPrice(objects.ModelPrice{
			Items: []objects.ModelPriceItem{
				{ItemCode: objects.PriceItemCodeUsage, Pricing: objects.Pricing{Mode: objects.PricingModeUsagePerUnit, UsagePerUnit: &inputPrice}},
				{ItemCode: objects.PriceItemCodeCompletion, Pricing: objects.Pricing{Mode: objects.PricingModeUsagePerUnit, UsagePerUnit: &outputPrice}},
			},
		}).
		SaveX(ctx)

	// Create two API keys:
	//  1. compactKey: no quota configured -> uses compact local SQLite accounting on data node B;
	//  2. quotaKey: hard request quota = 2 -> keeps real-time authoritative PG path on data node B.
	compactKey := clientA.APIKey.Create().
		SetName("compact-non-quota").
		SetKey("ah-compact-non-quota").
		SetType(apikey.TypeServiceAccount).
		SetUserID(adminUser.ID).
		SetProjectID(proj.ID).
		SetScopes([]string{"write_requests"}).
		SaveX(ctx)

	maxReqs := int64(2)
	quotaKey := clientA.APIKey.Create().
		SetName("realtime-quota").
		SetKey("ah-realtime-quota").
		SetType(apikey.TypeServiceAccount).
		SetUserID(adminUser.ID).
		SetProjectID(proj.ID).
		SetScopes([]string{"write_requests"}).
		SetProfiles(&objects.APIKeyProfiles{
			ActiveProfile: "strict",
			Profiles: []objects.APIKeyProfile{
				{
					Name: "strict",
					Quota: &objects.APIKeyQuota{
						Requests: &maxReqs,
						Period:   objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime},
					},
				},
			},
		}).
		SaveX(ctx)

	la, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	lb, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	nodes := map[string]string{"a": "http://" + la.Addr().String(), "b": "http://" + lb.Addr().String()}
	bindings := map[int]cluster.ChannelBinding{chB.ID: {Owner: "b", Identity: "acct-b"}}

	cacheCfg := xcache.Config{Mode: xcache.ModeRedis, Namespace: schema, Redis: xredis.Config{URL: redisURL}}
	bootstrap := biz.NewSystemService(biz.SystemServiceParams{Ent: clientA, CacheConfig: cacheCfg})
	require.NoError(t, bootstrap.SetSecretKey(ctx, "cluster-compact-secret-key"))
	require.NoError(t, bootstrap.SetStoragePolicy(ctx, &biz.StoragePolicy{
		StoreRequestBody:          true,
		StoreExecutionRequestBody: lo.ToPtr(true),
		StoreResponseBody:         true,
		StoreChunks:               true,
	}))
	require.NoError(t, bootstrap.SetSecuritySettings(ctx, biz.SecuritySettings{
		CyberSessionBlockEnabled:    true,
		CyberSessionBlockTTLSeconds: 300,
	}))
	require.NoError(t, bootstrap.SetRetryPolicy(ctx, &biz.RetryPolicy{
		Enabled:             false,
		UpstreamErrorPolicy: biz.UpstreamErrorPolicy{Mode: biz.UpstreamErrorModePassthrough},
	}))

	tempDir := t.TempDir()
	outboxPathB := filepath.Join(tempDir, "node-b-compact.db")

	type nodeInstance struct {
		server     *httptest.Server
		system     *biz.SystemService
		auth       *biz.AuthService
		writer     *biz.ForwardingObservationWriter
		bodyWriter *biz.ManagedRequestBodyWriter
		compact    *biz.CompactAccountingService
		gate       *maintenance.Gate
		redisHook  *redisCommandCounterHook
		protection *biz.PromptProtectionRuleService
	}

	buildNode := func(client *ent.Client, id string, listener net.Listener, outboxPath string, syncInterval time.Duration) *nodeInstance {
		nodeCfg := cluster.Config{
			Enabled:     true,
			NodeID:      id,
			ControlNode: "a",
			Nodes:       nodes,
			Channels:    bindings,
			CompactAccounting: cluster.CompactAccountingConfig{
				Enabled:       id != "a",
				OutboxPath:    outboxPath,
				SyncInterval:  syncInterval,
				MaxBatchCount: 64,
				MaxBatchBytes: 256 << 10,
				MaxSpoolCount: 1024,
				MaxSpoolBytes: 16 << 20,
			},
		}

		system := biz.NewSystemService(biz.SystemServiceParams{Ent: client, CacheConfig: cacheCfg})
		redisOpt, parseErr := redis.ParseURL(redisURL)
		require.NoError(t, parseErr)
		rawRedis := redis.NewClient(redisOpt)
		t.Cleanup(func() { _ = rawRedis.Close() })
		hook := &redisCommandCounterHook{}
		rawRedis.AddHook(hook)
		system.Cache = xcache.NewRedisWithDomain[ent.System](rawRedis, schema, "system")
		system.CyberSessionBlockCache = xcache.NewRedisWithLegacyPrefixes[biz.CyberSessionBlockEntry](rawRedis, schema, []string{"cyber-session-block:"})

		require.NoError(t, system.Start(ctx))
		t.Cleanup(system.Stop)

		projects := biz.NewProjectService(biz.ProjectServiceParams{Ent: client, CacheConfig: cacheCfg})
		users := biz.NewUserService(biz.UserServiceParams{Ent: client, CacheConfig: cacheCfg})
		keys := biz.NewAPIKeyService(biz.APIKeyServiceParams{Ent: client, CacheConfig: cacheCfg, ProjectService: projects, KeyPrefix: "ah"})
		t.Cleanup(keys.Stop)
		auth := biz.NewAuthService(biz.AuthServiceParams{Ent: client, SystemService: system, APIKeyService: keys, UserService: users})
		hc := httpclient.NewHttpClient()
		t.Cleanup(hc.CloseIdleConnections)
		channels := biz.NewChannelService(biz.ChannelServiceParams{Ent: client, CacheConfig: cacheCfg, SystemService: system, HttpClient: hc, Node: nodeCfg})
		t.Cleanup(channels.Stop)
		models := biz.NewModelService(biz.ModelServiceParams{Ent: client, SystemService: system, ChannelService: channels})
		storage := biz.NewDataStorageService(biz.DataStorageServiceParams{Client: client, CacheConfig: cacheCfg, SystemService: system})
		usage := biz.NewUsageLogService(client, system, channels)
		live := biz.NewLiveStreamRegistry()
		body := biz.NewManagedRequestBodyWriter(biz.ManagedRequestBodyWriterConfig{}, client, system)
		writer := biz.NewForwardingObservationWriter(biz.ManagedRequestBodyWriterConfig{})
		requests := biz.NewRequestServiceWithObservationWriter(client, system, usage, storage, live, body, writer)
		compact := biz.NewCompactAccountingService(biz.CompactAccountingServiceParams{
			Ent:           client,
			SystemService: system,
			UsageLog:      usage,
			Node:          nodeCfg,
		})
		writer.SetCompactAccountingService(compact)

		require.NoError(t, body.Start(ctx))
		require.NoError(t, writer.Start(ctx))
		require.NoError(t, compact.Start(ctx))
		t.Cleanup(func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = compact.Stop(stopCtx)
			_ = writer.Stop(stopCtx)
			_ = body.Stop(stopCtx)
		})

		quota := biz.NewQuotaServiceWithObservationWriter(client, system, writer)
		prompts := biz.NewPromptService(biz.PromptServiceParams{Ent: client})
		protection := biz.NewPromptProtectionRuleService(biz.PromptProtectionRuleServiceParams{Ent: client, CacheConfig: cacheCfg})
		t.Cleanup(protection.Stop)
		selector := orchestrator.NewDefaultSelector(channels, models, system)
		videos := biz.NewVideoService(channels, requests)
		openai := api.NewOpenAIHandlers(api.OpenAIHandlersParams{
			VideoService: videos, AuthService: auth, ChannelService: channels, ModelService: models,
			DefaultSelector: selector, RequestService: requests, SystemService: system, UsageLogService: usage,
			PromptService: prompts, PromptProtectionRuleService: protection, QuotaService: quota, HttpClient: hc,
			LiveStreamRegistry: live, ChannelLimiterManager: orchestrator.NewChannelLimiterManager(), Client: client,
		})
		doubao := api.NewDoubaoHandlers(api.DoubaoHandlersParams{
			VideoService: videos, ChannelService: channels, ModelService: models, DefaultSelector: selector,
			RequestService: requests, SystemService: system, UsageLogService: usage, PromptService: prompts,
			PromptProtectionRuleService: protection, QuotaService: quota, HttpClient: hc, LiveStreamRegistry: live,
			ChannelLimiterManager: orchestrator.NewChannelLimiterManager(),
		})
		playground := api.NewPlaygroundHandlers(api.PlaygroundHandlersParams{
			ChannelService: channels, ModelService: models, DefaultSelector: selector, RequestService: requests,
			SystemService: system, UsageLogService: usage, PromptService: prompts, PromptProtectionRuleService: protection,
			QuotaService: quota, HttpClient: hc, LiveStreamRegistry: live, ChannelLimiterManager: orchestrator.NewChannelLimiterManager(),
		})
		tester := orchestrator.NewTestChannelOrchestrator(channels, requests, system, usage, protection, hc)
		providerQuota := biz.NewProviderQuotaService(biz.ProviderQuotaServiceParams{Ent: client, SystemService: system, HttpClient: hc, Node: nodeCfg})
		adminGQL := gql.NewGraphqlHandlers(gql.Dependencies{
			ProviderQuotaService: providerQuota, Ent: client, AuthService: auth, APIKeyService: keys,
			UserService: users, SystemService: system, ProjectService: projects, ChannelService: channels,
			RequestService: requests, DataStorageService: storage, UsageLogService: usage, ModelService: models, PromptProtectionRuleService: protection, HttpClient: hc,
		})
		traces := biz.NewTraceService(biz.TraceServiceParams{Ent: client, RequestService: requests})
		gate := maintenance.NewGate()
		gateway := New(Config{Debug: false, RequestTimeout: 5 * time.Second, LLMRequestTimeout: 5 * time.Second})
		ip, ipErr := middleware.NewIPAccessControlConfig(false, nil, "")
		require.NoError(t, ipErr)
		clusterHandlers := api.NewClusterHandlers(models, channels, tester, videos, providerQuota).WithCompactAccounting(compact)
		SetupRoutes(gateway, Handlers{
			OpenAI: openai, Doubao: doubao, Playground: playground, Graphql: adminGQL, Cluster: clusterHandlers,
		}, client, Services{
			TraceService: traces, ThreadService: biz.NewThreadService(client, traces),
			AuthService: auth, SystemService: system, Node: nodeCfg, Maintenance: gate,
		}, ip)

		srv := httptest.NewUnstartedServer(gateway.Engine)
		srv.Listener = listener
		srv.Start()
		t.Cleanup(srv.Close)
		return &nodeInstance{
			server: srv, system: system, auth: auth, writer: writer,
			bodyWriter: body, compact: compact, gate: gate, redisHook: hook, protection: protection,
		}
	}

	nodeA := buildNode(clientA, "a", la, "", 5*time.Minute)
	nodeB := buildNode(clientB, "b", lb, outboxPathB, time.Hour) // Long interval so we deterministically verify pre-sync & flush states.

	doInference := func(origin, apiKeySecret, body string, headers map[string]string) (int, string) {
		req, reqErr := http.NewRequest(http.MethodPost, origin+"/v1/responses", strings.NewReader(body))
		require.NoError(t, reqErr)
		req.Header.Set("Authorization", "Bearer "+apiKeySecret)
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res, doErr := http.DefaultClient.Do(req)
		require.NoError(t, doErr)
		defer res.Body.Close()
		raw, readErr := io.ReadAll(res.Body)
		require.NoError(t, readErr)
		return res.StatusCode, string(raw)
	}

	// =========================================================================
	// Part B Verification: Zero Redis GET & Zero per-request PG SELECT on hot +
	// error paths, PG LISTEN/NOTIFY propagation, and reconnect primary reload.
	// =========================================================================
	nodeB.redisHook.systemGets.Store(0)
	bSQLMetrics.systemRetrySelects.Store(0)
	bSQLMetrics.requestWrites.Store(0)
	bSQLMetrics.pendingWrites.Store(0)
	bSQLMetrics.traceThreadWrites.Store(0)

	// 1. Send priced sync, priced stream, and unpriced sync requests to data node B with Thread + Trace headers.
	code, respBody := doInference(nodes["b"], compactKey.Key, `{"model":"priced-model","input":"hello"}`, map[string]string{
		"AH-Thread-Id": "thread-compact-1",
		"AH-Trace-Id":  "trace-compact-1",
	})
	require.Equal(t, http.StatusOK, code, respBody)

	code, respBody = doInference(nodes["b"], compactKey.Key, `{"model":"priced-model","stream":true,"input":"hello stream"}`, map[string]string{
		"AH-Thread-Id": "thread-compact-1",
		"AH-Trace-Id":  "trace-compact-2",
	})
	require.Equal(t, http.StatusOK, code, respBody)
	require.Contains(t, respBody, "response.completed")

	code, respBody = doInference(nodes["b"], compactKey.Key, `{"model":"unpriced-model","input":"no price"}`, nil)
	require.Equal(t, http.StatusOK, code, respBody)

	// Assert Part B hot-path zero Redis GET & zero per-request PG RetryPolicy queries!
	require.Zero(t, nodeB.redisHook.systemGets.Load(), "data node hot path must issue zero Redis GETs for system settings")
	require.Zero(t, bSQLMetrics.systemRetrySelects.Load(), "data node hot path must issue zero per-request PG SELECTs for retry_policy")

	// Assert Part A non-quota compact bypass: zero remote PG writes to pending_usages, requests, request_executions, traces, threads!
	require.Zero(t, bSQLMetrics.pendingWrites.Load(), "compact path must not write remote pending_usages")
	require.Zero(t, bSQLMetrics.requestWrites.Load(), "compact path must not write remote requests or request_executions")
	require.Zero(t, bSQLMetrics.traceThreadWrites.Load(), "compact path must not write remote traces or threads")
	require.Zero(t, clientA.UsageLog.Query().CountX(ctx), "usage_logs must remain 0 before low-frequency batch flush")
	require.Zero(t, clientA.PendingUsage.Query().CountX(ctx))

	unackedBeforeSync, err := nodeB.compact.UnackedCount(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, unackedBeforeSync, "all 3 compact usage events must be durably stored in local SQLite outbox")

	// 2. Mutate RetryPolicy (UpstreamErrorPolicy=Custom) on control node A via GraphQL -> verify PG LISTEN/NOTIFY updates node B in <2s,
	//    and error path on node B masks upstream 502 with zero Redis GET & zero per-request PG SELECT!
	adminToken, err := nodeA.auth.GenerateJWTToken(ctx, adminUser)
	require.NoError(t, err)
	gqlBody := `{"query":"mutation { updateRetryPolicy(input:{enabled:false,upstreamErrorPolicy:{mode:\"custom\",customMessage:\"custom masked 502 from notify\"}}) }"}`
	gqlReq, err := http.NewRequest(http.MethodPost, nodes["a"]+"/admin/graphql", strings.NewReader(gqlBody))
	require.NoError(t, err)
	gqlReq.Header.Set("Authorization", "Bearer "+adminToken)
	gqlReq.Header.Set("Content-Type", "application/json")
	gqlRes, err := http.DefaultClient.Do(gqlReq)
	require.NoError(t, err)
	gqlRaw, _ := io.ReadAll(gqlRes.Body)
	_ = gqlRes.Body.Close()
	require.Equal(t, http.StatusOK, gqlRes.StatusCode, string(gqlRaw))
	require.Contains(t, string(gqlRaw), `"updateRetryPolicy":true`)

	require.Eventually(t, func() bool {
		pol, pErr := nodeB.system.RetryPolicy(ctx)
		return pErr == nil && pol.UpstreamErrorPolicy.CustomMessage == "custom masked 502 from notify"
	}, 3*time.Second, 25*time.Millisecond, "node B must receive RetryPolicy update via PG LISTEN/NOTIFY")

	// Reset counters before error-path request to prove upstream_error_policy.go also does 0 Redis GET and 0 PG SELECT.
	nodeB.redisHook.systemGets.Store(0)
	bSQLMetrics.systemRetrySelects.Store(0)
	upstreamFailMode.Store(true)
	errCode, errResp := doInference(nodes["b"], compactKey.Key, `{"model":"priced-model","input":"trigger 502"}`, nil)
	upstreamFailMode.Store(false)
	require.Equal(t, http.StatusBadGateway, errCode)
	require.Contains(t, errResp, "custom masked 502 from notify")
	require.NotContains(t, errResp, "sensitive internal upstream detail")
	require.Zero(t, nodeB.redisHook.systemGets.Load(), "upstream_error_policy path must issue zero Redis GETs")
	require.Zero(t, bSQLMetrics.systemRetrySelects.Load(), "upstream_error_policy path must issue zero PG SELECTs")

	// 3. Simulate PG LISTEN disconnect + missed window update -> reconnect must LISTEN-then-reload from primary PG!
	// First, update directly in PG WITHOUT pg_notify (simulating a missed notification while disconnected), then force reconnect.
	missedPolicyJSON := `{"enabled":false,"load_balancer_strategy":"adaptive","upstream_error_policy":{"mode":"custom","custom_message":"reloaded after reconnect"}}`
	_, err = root.Exec(fmt.Sprintf(`UPDATE %s.systems SET value = $1 WHERE key = $2`, schema), missedPolicyJSON, biz.SystemKeyRetryPolicy)
	require.NoError(t, err)
	nodeB.system.ForceSnapshotReconnectForTest()

	require.Eventually(t, func() bool {
		pol, pErr := nodeB.system.RetryPolicy(ctx)
		return pErr == nil && pol.UpstreamErrorPolicy.CustomMessage == "reloaded after reconnect"
	}, 5*time.Second, 50*time.Millisecond, "reconnect loop must reload missed updates from primary PG after re-issuing LISTEN")

	// 4. Verify CyberSessionBlock (shared Redis state) and PromptProtectionRules remain intact!
	cyberSessionID := "cyber-session-" + uuid.NewString()
	upstreamCyberRefusal.Store(true)
	cyberCode1, _ := doInference(nodes["b"], compactKey.Key, `{"model":"priced-model","input":"exploit"}`, map[string]string{
		"Session-Id": cyberSessionID,
	})
	upstreamCyberRefusal.Store(false)
	require.Equal(t, http.StatusBadRequest, cyberCode1)
	require.Positive(t, nodeB.redisHook.cyberBlockOps.Load(), "CyberSessionBlock must continue using shared Redis cache")

	callsBeforeBlocked := upstreamCalls.Load()
	cyberCode2, cyberResp2 := doInference(nodes["b"], compactKey.Key, `{"model":"priced-model","input":"follow up"}`, map[string]string{
		"Session-Id": cyberSessionID,
	})
	require.Equal(t, http.StatusForbidden, cyberCode2, cyberResp2)
	require.Equal(t, callsBeforeBlocked, upstreamCalls.Load(), "blocked cyber session must reject locally before upstream dispatch")

	// Create & enable a prompt protection rule on control node A and verify node B reloads and enforces it via Redis watcher!
	createdRule, err := nodeA.protection.CreateRule(ctx, ent.CreatePromptProtectionRuleInput{
		Name:    "block-secret-token",
		Pattern: "FORBIDDEN_SECRET_TOKEN",
		Settings: &objects.PromptProtectionSettings{
			Action: objects.PromptProtectionActionReject,
			Scopes: []objects.PromptProtectionScope{objects.PromptProtectionScopeUser},
		},
	})
	require.NoError(t, err)
	_, err = nodeA.protection.UpdateRuleStatus(ctx, createdRule.ID, promptprotectionrule.StatusEnabled)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		rules, rErr := nodeB.protection.ListEnabledRules(ctx)
		return rErr == nil && len(rules) == 1 && rules[0].Name == "block-secret-token"
	}, 3*time.Second, 25*time.Millisecond, "node B must reload enabled prompt protection rule via Redis watcher")
	ruleCode, _ := doInference(nodes["b"], compactKey.Key, `{"model":"priced-model","input":"here is FORBIDDEN_SECRET_TOKEN"}`, nil)
	require.Equal(t, http.StatusBadRequest, ruleCode, "node B must enforce enabled prompt protection rule")

	// =========================================================================
	// Part A Verification: Lost-ACK Idempotency, Central Projection & Stats,
	// Hard-Quota Realtime Path Preservation, and /drain Immediate Flush.
	// =========================================================================

	// 5. Simulate "control PG committed batch, but ACK lost on network":
	//    1st Flush executes real HTTP POST to node A (committing all 3 rows into PG usage_logs + compact_usage_batches),
	//    then returns a simulated network error before node B receives the ACK.
	var injectedLostAck atomic.Bool
	nodeB.compact.TransportHookForTest = func(reqCtx context.Context, req *http.Request, do func(*http.Request) (*http.Response, error)) (*http.Response, error) {
		resp, doErr := do(req)
		if doErr == nil && injectedLostAck.CompareAndSwap(false, true) {
			_ = resp.Body.Close()
			return nil, errors.New("simulated network reset after control PG commit before ACK")
		}
		return resp, doErr
	}

	err = nodeB.compact.Flush(ctx)
	require.ErrorContains(t, err, "simulated network reset")
	// Control PG already has the 3 usage_logs and 1 batch receipt, while node B still has 3 unacked events in SQLite!
	require.Equal(t, 3, clientA.UsageLog.Query().Where(usagelog.APIKeyIDEQ(compactKey.ID)).CountX(ctx))
	require.Equal(t, 1, clientA.CompactUsageBatch.Query().CountX(ctx))
	stillUnacked, err := nodeB.compact.UnackedCount(ctx)
	require.NoError(t, err)
	require.Equal(t, 3, stillUnacked)

	// Retry Flush (or startup replay): node B resends the exact sealed batch; node A returns idempotent ACK; 0 duplicate rows!
	require.NoError(t, nodeB.compact.Flush(ctx))
	nodeB.compact.TransportHookForTest = nil

	afterReplayUnacked, err := nodeB.compact.UnackedCount(ctx)
	require.NoError(t, err)
	require.Zero(t, afterReplayUnacked)
	require.Equal(t, 3, clientA.UsageLog.Query().Where(usagelog.APIKeyIDEQ(compactKey.ID)).CountX(ctx), "lost-ACK replay must never double-count usage_logs")
	require.Equal(t, 1, clientA.CompactUsageBatch.Query().CountX(ctx))

	// Check nullable request_id and null vs non-null cost in central PG18 usage_logs + GraphQL usageLogs query!
	persistedLogs := clientA.UsageLog.Query().Where(usagelog.APIKeyIDEQ(compactKey.ID)).Order(ent.Asc(usagelog.FieldID)).AllX(ctx)
	require.Len(t, persistedLogs, 3)
	var nullCostCount, pricedCostCount int
	for _, row := range persistedLogs {
		require.Zero(t, row.RequestID, "compact usage_logs must have NULL request_id")
		require.NotNil(t, row.EventID)
		if row.ModelID == "unpriced-model" {
			require.Nil(t, row.TotalCost, "unpriced model must preserve total_cost = NULL rather than 0")
			nullCostCount++
		} else {
			require.NotNil(t, row.TotalCost)
			require.Greater(t, *row.TotalCost, 0.0)
			pricedCostCount++
		}
	}
	require.Equal(t, 1, nullCostCount)
	require.Equal(t, 2, pricedCostCount)

	// Query GraphQL usageLogs & dashboard stats on control node A to verify nullable requestID & request edge serialize cleanly without errors.
	usageGQL := `{"query":"query { usageLogs(first: 10) { totalCount edges { node { id requestID modelID totalTokens totalCost request { id } } } } }"}`
	uReq, err := http.NewRequest(http.MethodPost, nodes["a"]+"/admin/graphql", strings.NewReader(usageGQL))
	require.NoError(t, err)
	uReq.Header.Set("Authorization", "Bearer "+adminToken)
	uReq.Header.Set("Content-Type", "application/json")
	uRes, err := http.DefaultClient.Do(uReq)
	require.NoError(t, err)
	uRaw, _ := io.ReadAll(uRes.Body)
	_ = uRes.Body.Close()
	require.Equal(t, http.StatusOK, uRes.StatusCode, string(uRaw))
	require.NotContains(t, string(uRaw), `"errors"`, string(uRaw))
	require.Contains(t, string(uRaw), `"totalCount":3`)
	require.Contains(t, string(uRaw), `"requestID":null`)
	require.Contains(t, string(uRaw), `"request":null`)

	// 6. Verify Hard-Quota APIKey keeps authoritative real-time PG enforcement on data node B!
	qCode1, qResp1 := doInference(nodes["b"], quotaKey.Key, `{"model":"priced-model","input":"quota 1"}`, nil)
	require.Equal(t, http.StatusOK, qCode1, qResp1)
	qCode2, qResp2 := doInference(nodes["b"], quotaKey.Key, `{"model":"priced-model","input":"quota 2"}`, nil)
	require.Equal(t, http.StatusOK, qCode2, qResp2)
	// Wait briefly for async pending_usage/usage_log handoff if needed, though local in-memory + PG snapshot in QuotaService enforces immediately!
	qCode3, qResp3 := doInference(nodes["b"], quotaKey.Key, `{"model":"priced-model","input":"quota 3 should reject"}`, nil)
	require.Equal(t, http.StatusForbidden, qCode3, qResp3)
	require.Contains(t, qResp3, "quota_exceeded")

	// Wait for nodeB observation writer to finish quotaKey's 2 authoritative requests so pending_usages is 0 before drain test.
	require.NoError(t, nodeB.writer.Wait(ctx))
	require.Equal(t, 2, clientA.UsageLog.Query().Where(usagelog.APIKeyIDEQ(quotaKey.ID)).CountX(ctx))

	// 7. Verify /drain lifecycle:
	//    Generate 2 more compact usage events on node B (unflushed because SyncInterval=1h),
	//    check maintenance /status reports unacked_compact_usage=2, then trigger POST /drain
	//    and verify it immediately flushes to control node A, reaches unacked_compact_usage=0, and transitions phase="drained"!
	codeD1, _ := doInference(nodes["b"], compactKey.Key, `{"model":"priced-model","input":"before drain 1"}`, nil)
	require.Equal(t, http.StatusOK, codeD1)
	codeD2, _ := doInference(nodes["b"], compactKey.Key, `{"model":"priced-model","input":"before drain 2"}`, nil)
	require.Equal(t, http.StatusOK, codeD2)

	localMaint := &maintenance.Local{
		Gate: nodeB.gate,
		Inspect: func(inspectCtx context.Context) (int, int, int) {
			unacked, _ := nodeB.compact.UnackedCount(inspectCtx)
			bCtx := authz.WithSystemBypass(inspectCtx, "test-maint-inspect")
			np, _ := clientB.PendingUsage.Query().Where(pendingusage.NodeIDEQ("b")).Count(bCtx)
			gp, _ := clientB.PendingUsage.Query().Count(bCtx)
			return unacked, np, gp
		},
		Drain: func(drainCtx context.Context) (maintenance.DrainResult, error) {
			res := maintenance.DrainResult{}
			if stopErr := nodeB.writer.Stop(drainCtx); stopErr != nil {
				return res, stopErr
			}
			if stopErr := nodeB.bodyWriter.Stop(drainCtx); stopErr != nil {
				return res, stopErr
			}
			res.WritersStopped = true
			flushErr := nodeB.compact.Flush(drainCtx)
			unacked, _ := nodeB.compact.UnackedCount(drainCtx)
			res.UnackedCompactUsage = unacked
			bCtx := authz.WithSystemBypass(drainCtx, "test-maint-drain")
			res.NodePending, _ = clientB.PendingUsage.Query().Where(pendingusage.NodeIDEQ("b")).Count(bCtx)
			res.GlobalPending, _ = clientB.PendingUsage.Query().Count(bCtx)
			if flushErr != nil {
				return res, flushErr
			}
			if res.UnackedCompactUsage != 0 || res.NodePending != 0 {
				return res, errors.New("pending accounting remains")
			}
			return res, nil
		},
	}

	maintSrv := httptest.NewServer(localMaint.Handler())
	defer maintSrv.Close()

	// Check /status before /drain: unacked_compact_usage must be 2.
	statusRes, err := http.Get(maintSrv.URL + "/status")
	require.NoError(t, err)
	var preDrainState maintenance.State
	require.NoError(t, json.NewDecoder(statusRes.Body).Decode(&preDrainState))
	_ = statusRes.Body.Close()
	require.Equal(t, "running", preDrainState.Phase)
	require.Equal(t, 2, preDrainState.UnackedCompactUsage)

	// Trigger POST /drain -> must immediately flush all 2 compact events and reach phase="drained".
	drainReq, err := http.NewRequest(http.MethodPost, maintSrv.URL+"/drain", nil)
	require.NoError(t, err)
	drainRes, err := http.DefaultClient.Do(drainReq)
	require.NoError(t, err)
	_ = drainRes.Body.Close()
	require.Equal(t, http.StatusAccepted, drainRes.StatusCode)

	require.Eventually(t, func() bool {
		st := localMaint.Status()
		return st.Phase == "drained" && st.Drain != nil && st.Drain.UnackedCompactUsage == 0 && st.Drain.NodePending == 0
	}, 5*time.Second, 25*time.Millisecond)

	// All 5 compactKey requests + 2 quotaKey requests = 7 total central usage_logs!
	require.Equal(t, 5, clientA.UsageLog.Query().Where(usagelog.APIKeyIDEQ(compactKey.ID)).CountX(ctx))
	require.Equal(t, 7, clientA.UsageLog.Query().CountX(ctx))

	// 8. Verify frozen SQLite export -> replacement data node import -> startup catch-up to PG18!
	//    Suppose control node is temporarily unreachable when draining an ephemeral data node:
	//    events remain durably in node B's SQLite outbox, we freeze & VACUUM INTO export it,
	//    stop the old node, import onto a replacement outbox path, and start a fresh service
	//    that automatically replays those exact records on startup!
	_, err = nodeB.compact.Outbox().Append(ctx, biz.CompactUsageEvent{
		APIKeyID:  compactKey.ID,
		ProjectID: proj.ID,
		ChannelID: chB.ID,
		ModelID:   "priced-model",
		Usage: llm.Usage{
			PromptTokens:     20,
			CompletionTokens: 10,
			TotalTokens:      30,
		},
		TotalCost: lo.ToPtr(0.04),
		Source:    string(usagelog.SourceAPI),
		Format:    "openai/responses",
	})
	require.NoError(t, err)
	_, err = nodeB.compact.Outbox().Append(ctx, biz.CompactUsageEvent{
		APIKeyID:  compactKey.ID,
		ProjectID: proj.ID,
		ChannelID: chB.ID,
		ModelID:   "unpriced-model",
		Usage: llm.Usage{
			PromptTokens:     8,
			CompletionTokens: 4,
			TotalTokens:      12,
		},
		TotalCost: nil,
		Source:    string(usagelog.SourceAPI),
		Format:    "openai/responses",
	})
	require.NoError(t, err)

	migUnacked, err := nodeB.compact.UnackedCount(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, migUnacked)

	exportDir := filepath.Join(tempDir, "export-bundle")
	manifest, err := nodeB.compact.Outbox().FreezeAndExport(ctx, exportDir)
	require.NoError(t, err)
	require.Equal(t, 2, manifest.UnackedCount)
	require.NoError(t, nodeB.compact.Stop(ctx))
	nodeB.server.Close()

	// Import frozen snapshot onto a replacement disk path and start a fresh CompactAccountingService
	// for node "b" (startup catch-up immediately syncs to control node A!).
	replacementOutboxPath := filepath.Join(tempDir, "replacement", "node-b-restored.db")
	importedStatus, err := biz.ImportFrozenCompactOutbox(ctx, exportDir, replacementOutboxPath)
	require.NoError(t, err)
	require.Equal(t, manifest.ProducerUUID, importedStatus.ProducerUUID)
	require.Equal(t, 2, importedStatus.UnackedCount)

	replacementCfg := cluster.Config{
		Enabled:     true,
		NodeID:      "b",
		ControlNode: "a",
		Nodes:       nodes,
		Channels:    bindings,
		CompactAccounting: cluster.CompactAccountingConfig{
			Enabled:       true,
			OutboxPath:    replacementOutboxPath,
			SyncInterval:  100 * time.Millisecond,
			MaxBatchCount: 64,
			MaxBatchBytes: 256 << 10,
			MaxSpoolCount: 1024,
			MaxSpoolBytes: 16 << 20,
		},
	}
	replacementCompact := biz.NewCompactAccountingService(biz.CompactAccountingServiceParams{
		Ent:           clientB,
		SystemService: nodeB.system,
		UsageLog:      biz.NewUsageLogService(clientB, nodeB.system, nil),
		Node:          replacementCfg,
	})
	require.NoError(t, replacementCompact.Start(ctx))
	defer func() { _ = replacementCompact.Stop(context.Background()) }()

	require.Eventually(t, func() bool {
		rem, uErr := replacementCompact.UnackedCount(ctx)
		return uErr == nil && rem == 0
	}, 5*time.Second, 25*time.Millisecond, "replacement node must automatically replay imported frozen outbox on startup")

	require.Equal(t, 7, clientA.UsageLog.Query().Where(usagelog.APIKeyIDEQ(compactKey.ID)).CountX(ctx))
	require.Equal(t, 9, clientA.UsageLog.Query().CountX(ctx))
}
