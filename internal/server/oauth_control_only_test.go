package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/migrate"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/metrics"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/pkg/xredis"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/anthropic/claudecode"
	"github.com/looplj/axonhub/llm/transformer/antigravity"
	"github.com/looplj/axonhub/llm/transformer/openai/codex"
)

type oauthTestEnv struct {
	t                   *testing.T
	ctx                 context.Context
	upstream            *httptest.Server
	hc                  *httpclient.HttpClient
	oidcCfg             biz.OIDCConfig
	nodes               map[string]string
	oidcDiscoveryCalls  atomic.Int32
	copilotDeviceCalls  atomic.Int32
}

type oauthNodeInstance struct {
	server      *Server
	oidc        *biz.OIDCService
	auth        *biz.AuthService
	codex       *api.CodexHandlers
	claudecode  *api.ClaudeCodeHandlers
	antigravity *api.AntigravityHandlers
	copilot     *api.CopilotHandlers
}

func newOAuthTestEnv(t *testing.T) *oauthTestEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)

	provider := sdkmetric.NewMeterProvider()
	require.NoError(t, metrics.SetupMetrics(provider, "oauth-domain-test"))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	env := &oauthTestEnv{
		t:   t,
		ctx: authz.WithTestBypass(context.Background()),
		nodes: map[string]string{
			"lag":              "http://127.0.0.1:18091",
			"gcp":              "http://127.0.0.1:18092",
			"gcp-kiki-usw1":    "http://127.0.0.1:18093",
			"gcp-nailong-usw1": "http://127.0.0.1:18094",
			"tencent-new":      "http://127.0.0.1:18095",
			"tencent-old":      "http://127.0.0.1:18096",
		},
	}

	env.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		scheme := "http://" + r.Host
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			env.oidcDiscoveryCalls.Add(1)
			_, _ = fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":"https://idp.example.test/authorize","token_endpoint":%q,"userinfo_endpoint":%q,"jwks_uri":%q}`,
				scheme, scheme+"/oidc/token", scheme+"/oidc/userinfo", scheme+"/oidc/jwks")
		case "/oidc/token":
			_, _ = w.Write([]byte(`{"access_token":"oidc-access","token_type":"bearer","expires_in":3600}`))
		case "/oidc/userinfo":
			_, _ = w.Write([]byte(`{"sub":"sub-linked-owner","email":"owner@example.test","email_verified":true,"name":"Owner"}`))
		case "/codex/token":
			_, _ = w.Write([]byte(`{"access_token":"codex-access","refresh_token":"codex-refresh","expires_in":3600,"token_type":"bearer"}`))
		case "/claudecode/token":
			_, _ = w.Write([]byte(`{"access_token":"claude-access","refresh_token":"claude-refresh","expires_in":3600,"token_type":"bearer"}`))
		case "/antigravity/token":
			_, _ = w.Write([]byte(`{"access_token":"ag-access","refresh_token":"ag-refresh","expires_in":3600,"token_type":"bearer"}`))
		case "/copilot/device/code":
			env.copilotDeviceCalls.Add(1)
			_, _ = w.Write([]byte(`{"device_code":"dev-code-1","user_code":"USER-1234","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`))
		case "/copilot/access_token":
			_, _ = w.Write([]byte(`{"access_token":"gho_copilot_token","token_type":"bearer","scope":"read:user"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(env.upstream.Close)

	localBase, err := url.Parse(env.upstream.URL)
	require.NoError(t, err)
	localTransport := env.upstream.Client().Transport

	// Strictly local transport: any unmapped or non-loopback host fails immediately.
	env.hc = httpclient.NewHttpClientWithClient(&http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			targetURL := ""
			switch req.URL.String() {
			case codex.TokenURL:
				targetURL = env.upstream.URL + "/codex/token"
			case claudecode.TokenURL:
				targetURL = env.upstream.URL + "/claudecode/token"
			case antigravity.TokenURL:
				targetURL = env.upstream.URL + "/antigravity/token"
			case "https://github.com/login/device/code":
				targetURL = env.upstream.URL + "/copilot/device/code"
			case "https://github.com/login/oauth/access_token":
				targetURL = env.upstream.URL + "/copilot/access_token"
			default:
				if req.URL.Host == localBase.Host {
					targetURL = req.URL.String()
				}
			}
			if targetURL == "" {
				return nil, fmt.Errorf("unexpected outbound request in test: %s %s", req.Method, req.URL.String())
			}
			proxyReq, err := http.NewRequestWithContext(req.Context(), req.Method, targetURL, req.Body)
			if err != nil {
				return nil, err
			}
			proxyReq.Header = req.Header.Clone()
			return localTransport.RoundTrip(proxyReq)
		}),
	})
	t.Cleanup(env.hc.CloseIdleConnections)

	env.oidcCfg = biz.OIDCConfig{
		Providers: []biz.OIDCProvider{{
			ID:            "corp",
			Name:          "corp",
			DisplayName:   "Corp SSO",
			Issuer:        env.upstream.URL,
			IssuerURL:     env.upstream.URL,
			AuthURL:       "https://idp.example.test/authorize",
			TokenURL:      env.upstream.URL + "/oidc/token",
			UserInfoURL:   env.upstream.URL + "/oidc/userinfo",
			ClientID:      "corp-client",
			ClientSecret:  "corp-secret",
			EnablePKCE:    true,
			OIDCLoginOnly: true,
		}},
	}
	return env
}

// buildNode constructs the 5 OAuth/OIDC domains via Fx using the real production
// constructors and cluster.Config, then wires routes through SetupRoutes.
func (e *oauthTestEnv) buildNode(db *ent.Client, node cluster.Config, oauthCacheCfg, sysCacheCfg xcache.Config) *oauthNodeInstance {
	e.t.Helper()
	system := biz.NewSystemService(biz.SystemServiceParams{Ent: db, CacheConfig: sysCacheCfg})
	users := biz.NewUserService(biz.UserServiceParams{Ent: db, CacheConfig: sysCacheCfg})
	ip, err := middleware.NewIPAccessControlConfig(false, nil, "")
	require.NoError(e.t, err)
	srv := New(Config{RequestTimeout: 5 * time.Second, LLMRequestTimeout: 5 * time.Second})

	var (
		inst  oauthNodeInstance
		oidcH *api.OIDCHandlers
	)
	inst.server = srv
	app := fx.New(
		fx.NopLogger,
		fx.Supply(
			db,
			node,
			oauthCacheCfg,
			e.oidcCfg,
			e.hc,
			fx.Annotate("https://console.example.test", fx.ResultTags(`name:"public_url"`)),
		),
		fx.Provide(
			biz.NewOIDCService,
			func(oidcSvc *biz.OIDCService) *biz.AuthService {
				return biz.NewAuthService(biz.AuthServiceParams{
					Ent:           db,
					SystemService: system,
					UserService:   users,
					OIDCService:   oidcSvc,
				})
			},
			api.NewOIDCHandlers,
			api.NewCodexHandlers,
			api.NewClaudeCodeHandlers,
			api.NewAntigravityHandlers,
			api.NewCopilotHandlers,
		),
		fx.Populate(
			&inst.oidc,
			&inst.auth,
			&oidcH,
			&inst.codex,
			&inst.claudecode,
			&inst.antigravity,
			&inst.copilot,
		),
	)
	require.NoError(e.t, app.Err())

	SetupRoutes(srv, Handlers{
		OpenAI:      &api.OpenAIHandlers{ResponseWebSocketHandler: &api.ResponsesWebSocketHandler{}},
		System:      api.NewSystemHandlers(api.SystemHandlersParams{SystemService: system}),
		Auth:        api.NewAuthHandlers(api.AuthHandlersParams{AuthService: inst.auth}),
		Codex:       inst.codex,
		ClaudeCode:  inst.claudecode,
		Antigravity: inst.antigravity,
		Copilot:     inst.copilot,
		OIDC:        oidcH,
	}, db, Services{
		AuthService:   inst.auth,
		SystemService: system,
		Node:          node,
	}, ip)
	return &inst
}

func doJSONRequest(t *testing.T, srv *Server, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Engine.ServeHTTP(rec, req)
	return rec
}

func assertDataNodeRejectsAllOAuthManagementRoutes(t *testing.T, srv *Server, token, oidcState, exchangeCode, codexSession, claudeSession, agSession, copilotSession string) {
	t.Helper()
	for _, check := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/oauth/oidc/providers", nil},
		{http.MethodGet, "/oauth/oidc/authorize/corp", nil},
		{http.MethodGet, "/oauth/oidc/callback/corp?code=test-code&state=" + url.QueryEscape(oidcState), nil},
		{http.MethodPost, "/oauth/oidc/exchange", map[string]any{"code": exchangeCode}},
		{http.MethodGet, "/portal/oidc/link/corp", nil},
		{http.MethodGet, "/admin/oidc/link/corp", nil},
		{http.MethodPost, "/admin/codex/oauth/start", map[string]any{}},
		{http.MethodPost, "/admin/codex/oauth/exchange", map[string]any{"session_id": codexSession, "callback_url": "http://localhost:1455/auth/callback?code=c&state=" + codexSession}},
		{http.MethodPost, "/admin/claudecode/oauth/start", map[string]any{}},
		{http.MethodPost, "/admin/claudecode/oauth/exchange", map[string]any{"session_id": claudeSession, "callback_url": "http://localhost:54545/callback?code=c#" + claudeSession}},
		{http.MethodPost, "/admin/antigravity/oauth/start", map[string]any{}},
		{http.MethodPost, "/admin/antigravity/oauth/exchange", map[string]any{"session_id": agSession, "callback_url": "http://localhost:8000/callback?code=c&state=" + agSession}},
		{http.MethodPost, "/admin/copilot/oauth/start", map[string]any{}},
		{http.MethodPost, "/admin/copilot/oauth/poll", map[string]any{"session_id": copilotSession}},
	} {
		rec := doJSONRequest(t, srv, check.method, check.path, token, check.body)
		require.Equal(t, http.StatusNotFound, rec.Code, "path %s on data node must return 404", check.path)
		require.Contains(t, rec.Body.String(), `"control_node_required"`)
		require.Contains(t, rec.Body.String(), `"execution":"not_executed"`)
	}
}

func TestControlOnlyOAuthStateConstructionAndLifecycle(t *testing.T) {
	env := newOAuthTestEnv(t)
	mr := miniredis.RunT(t)
	defer mr.Close()

	rawRedis := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rawRedis.Close()

	var pingCount, writeCount atomic.Int32
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, _ ...string) bool {
		switch strings.ToUpper(cmd) {
		case "PING":
			pingCount.Add(1)
		case "SET", "SETEX", "PSETEX", "DEL", "EXPIRE", "PEXPIRE", "FLUSHDB", "FLUSHALL":
			writeCount.Add(1)
		}
		return false
	})

	db := enttest.NewEntClient(t, "sqlite3", "file:oauth-domain-"+uuid.NewString()+"?mode=memory&_fk=0")
	t.Cleanup(func() { _ = db.Close() })

	owner := db.User.Create().SetEmail("owner@example.test").SetPassword("synthetic").SetIsOwner(true).SetStatus(user.StatusActivated).SaveX(env.ctx)
	ssoUser := db.User.Create().SetEmail("sso@example.test").SetPassword("regular-password-hash").SetStatus(user.StatusActivated).SaveX(env.ctx)
	db.OIDCIdentity.Create().SetUserID(ssoUser.ID).SetIssuer(env.upstream.URL).SetSubject("sub-sso-1").SetEmail(ssoUser.Email).SetIdpName("Example IdP").SaveX(env.ctx)

	cacheCfg := xcache.Config{Mode: xcache.ModeRedis, Namespace: "task-oauth-domains", Redis: xredis.Config{Addr: mr.Addr()}}
	memCacheCfg := xcache.Config{Mode: xcache.ModeMemory}
	bootstrap := biz.NewSystemService(biz.SystemServiceParams{Ent: db, CacheConfig: memCacheCfg})
	require.NoError(t, bootstrap.SetSecretKey(env.ctx, "synthetic-oauth-domain-jwt-key"))

	// 1. Standalone and control ("lag") each construct all 5 OAuth/OIDC Redis state clients via Fx (5 PINGs each).
	pingCount.Store(0)
	standalone := env.buildNode(db, cluster.Config{}, cacheCfg, memCacheCfg)
	require.Equal(t, int32(5), pingCount.Load(), "standalone must initialize all 5 OAuth/OIDC Redis state clients")
	require.NotNil(t, standalone.oidc)

	pingCount.Store(0)
	discoveryBeforeControl := env.oidcDiscoveryCalls.Load()
	control := env.buildNode(db, cluster.Config{Enabled: true, NodeID: "lag", ControlNode: "lag", Nodes: env.nodes}, cacheCfg, memCacheCfg)
	require.Equal(t, int32(5), pingCount.Load(), "control node must initialize all 5 OAuth/OIDC Redis state clients")
	require.Equal(t, discoveryBeforeControl+1, env.oidcDiscoveryCalls.Load(), "control node performs OIDC provider discovery")

	// 2. All 5 non-control data nodes construct 0 OAuth/OIDC Redis state clients (0 PINGs, 0 IdP discovery calls),
	// even if their OAuth state Redis address is unreachable.
	unreachableCacheCfg := xcache.Config{Mode: xcache.ModeRedis, Namespace: "task-oauth-domains", Redis: xredis.Config{Addr: "127.0.0.1:1"}}
	pingCount.Store(0)
	discoveryBeforeData := env.oidcDiscoveryCalls.Load()
	dataNodes := make(map[string]*oauthNodeInstance, 5)
	for _, id := range []string{"gcp", "gcp-kiki-usw1", "gcp-nailong-usw1", "tencent-new", "tencent-old"} {
		dataNodes[id] = env.buildNode(db, cluster.Config{Enabled: true, NodeID: id, ControlNode: "lag", Nodes: env.nodes}, unreachableCacheCfg, memCacheCfg)
	}
	require.Zero(t, pingCount.Load(), "5 non-control data nodes must initialize 0 OAuth/OIDC Redis state clients")
	require.Equal(t, discoveryBeforeData, env.oidcDiscoveryCalls.Load(), "data nodes must not dial OIDC discovery during construction")

	// 3. Verify non-session OIDCService capabilities on data nodes and safe nil guards on management methods/handlers.
	gcpInst := dataNodes["gcp"]
	require.Equal(t, 1, gcpInst.oidc.CountProviders())
	require.True(t, gcpInst.oidc.IsUserRestrictedToOIDC(env.ctx, ssoUser))
	require.False(t, gcpInst.oidc.IsUserRestrictedToOIDC(env.ctx, owner))
	providers := gcpInst.oidc.GetProviders(contexts.WithUser(ent.NewContext(env.ctx, db), ssoUser))
	require.Len(t, providers, 1)
	require.True(t, providers[0].IsLinked)

	_, _, err := gcpInst.oidc.GetAuthorizeURL(env.ctx, "corp", "https://console.example.test")
	require.ErrorContains(t, err, "unavailable on data node")
	_, _, err = gcpInst.oidc.GetLinkAuthorizeURL(env.ctx, "corp", "https://console.example.test", owner.ID)
	require.ErrorContains(t, err, "unavailable on data node")
	_, _, err = gcpInst.oidc.Callback(env.ctx, "corp", "code", "state", "https://console.example.test")
	require.ErrorContains(t, err, "unavailable on data node")
	_, err = gcpInst.oidc.ExchangeCode(env.ctx, "exchange-code")
	require.ErrorContains(t, err, "unavailable on data node")

	beforeDeviceCalls := env.copilotDeviceCalls.Load()
	for _, directCall := range []func(*gin.Context){
		gcpInst.codex.StartOAuth, gcpInst.codex.Exchange,
		gcpInst.claudecode.StartOAuth, gcpInst.claudecode.Exchange,
		gcpInst.antigravity.StartOAuth, gcpInst.antigravity.Exchange,
		gcpInst.copilot.StartOAuth, gcpInst.copilot.PollOAuth,
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/direct", bytes.NewBufferString(`{"session_id":"s","callback_url":"http://localhost/cb?code=c&state=s"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		directCall(c)
		require.Equal(t, http.StatusNotFound, rec.Code)
	}
	require.Equal(t, beforeDeviceCalls, env.copilotDeviceCalls.Load(), "data node Copilot handler must not request device code")

	// 4. Create synthetic OAuth/OIDC states on control node across all 5 legacy prefixes.
	ownerJWT, err := control.auth.GenerateJWTToken(env.ctx, owner)
	require.NoError(t, err)

	oidcLinkRec := doJSONRequest(t, control.server, http.MethodGet, "/admin/oidc/link/corp", ownerJWT, nil)
	require.Equal(t, http.StatusOK, oidcLinkRec.Code)
	var oidcLinkResp struct {
		Data struct {
			State string `json:"state"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(oidcLinkRec.Body.Bytes(), &oidcLinkResp))
	require.NotEmpty(t, oidcLinkResp.Data.State)

	syntheticExchangeCode := "synthetic-exchange-code-1"
	oidcExchangeCache := xcache.NewFromConfigWithLegacyPrefixes[[]byte](cacheCfg, "oidc_")
	require.NoError(t, oidcExchangeCache.Set(env.ctx, "oidc_exchange:"+syntheticExchangeCode, fmt.Appendf(nil, "%d", owner.ID), xcache.WithExpiration(5*time.Minute)))

	var codexStart api.StartCodexOAuthResponse
	require.NoError(t, json.Unmarshal(doJSONRequest(t, control.server, http.MethodPost, "/admin/codex/oauth/start", ownerJWT, map[string]any{}).Body.Bytes(), &codexStart))
	var claudeStart api.StartClaudeCodeOAuthResponse
	require.NoError(t, json.Unmarshal(doJSONRequest(t, control.server, http.MethodPost, "/admin/claudecode/oauth/start", ownerJWT, map[string]any{}).Body.Bytes(), &claudeStart))
	var agStart api.StartAntigravityOAuthResponse
	require.NoError(t, json.Unmarshal(doJSONRequest(t, control.server, http.MethodPost, "/admin/antigravity/oauth/start", ownerJWT, map[string]any{"project_id": "proj-synthetic-1"}).Body.Bytes(), &agStart))
	var copilotStart api.StartCopilotOAuthResponse
	require.NoError(t, json.Unmarshal(doJSONRequest(t, control.server, http.MethodPost, "/admin/copilot/oauth/start", ownerJWT, map[string]any{}).Body.Bytes(), &copilotStart))

	trackedKeys := []string{
		"oidc_state:" + oidcLinkResp.Data.State,
		"oidc_pkce:" + oidcLinkResp.Data.State,
		"oidc_link_state:" + oidcLinkResp.Data.State,
		"oidc_exchange:" + syntheticExchangeCode,
		"codex:oauth:" + codexStart.SessionID,
		"claudecode:oauth:" + claudeStart.SessionID,
		"antigravity:oauth:" + agStart.SessionID,
		"copilot:oauth:" + copilotStart.SessionID,
	}
	initialValues := make(map[string]string, len(trackedKeys))
	initialTTLs := make(map[string]time.Duration, len(trackedKeys))
	for _, k := range trackedKeys {
		val, err := rawRedis.Get(env.ctx, k).Result()
		require.NoError(t, err)
		ttl := mr.TTL(k)
		require.Positive(t, ttl)
		initialValues[k] = val
		initialTTLs[k] = ttl
	}

	mr.FastForward(30 * time.Second)

	// 5. Data-node restart and 14 management route rejections: 0 Redis PINGs, 0 writes, keys & TTLs untouched.
	pingCount.Store(0)
	writeCount.Store(0)
	restartedData := env.buildNode(db, cluster.Config{Enabled: true, NodeID: "gcp", ControlNode: "lag", Nodes: env.nodes}, cacheCfg, memCacheCfg)
	require.Zero(t, pingCount.Load())
	assertDataNodeRejectsAllOAuthManagementRoutes(t, restartedData.server, ownerJWT, oidcLinkResp.Data.State, syntheticExchangeCode, codexStart.SessionID, claudeStart.SessionID, agStart.SessionID, copilotStart.SessionID)
	require.Zero(t, writeCount.Load())

	for _, k := range trackedKeys {
		val, err := rawRedis.Get(env.ctx, k).Result()
		require.NoError(t, err)
		require.Equal(t, initialValues[k], val)
		require.Equal(t, initialTTLs[k]-30*time.Second, mr.TTL(k))
	}

	// 6. Control node completes all flows and enforces single-use deletion.
	oidcCbRec := doJSONRequest(t, control.server, http.MethodGet, "/oauth/oidc/callback/corp?code=oidc-code&state="+url.QueryEscape(oidcLinkResp.Data.State), "", nil)
	require.Equal(t, http.StatusFound, oidcCbRec.Code)
	require.Contains(t, oidcCbRec.Header().Get("Location"), "/settings/profile?oidc_link=success")
	oidcCbReplay := doJSONRequest(t, control.server, http.MethodGet, "/oauth/oidc/callback/corp?code=oidc-code&state="+url.QueryEscape(oidcLinkResp.Data.State), "", nil)
	require.Contains(t, oidcCbReplay.Header().Get("Location"), "error=auth_failed")

	for _, flow := range []struct {
		path   string
		token  string
		body   map[string]any
		expect string
	}{
		{"/oauth/oidc/exchange", "", map[string]any{"code": syntheticExchangeCode}, `"token"`},
		{"/admin/codex/oauth/exchange", ownerJWT, map[string]any{"session_id": codexStart.SessionID, "callback_url": "http://localhost:1455/auth/callback?code=codex-code&state=" + codexStart.SessionID}, "codex-access"},
		{"/admin/claudecode/oauth/exchange", ownerJWT, map[string]any{"session_id": claudeStart.SessionID, "callback_url": "http://localhost:54545/callback?code=claude-code#" + claudeStart.SessionID}, "claude-access"},
		{"/admin/antigravity/oauth/exchange", ownerJWT, map[string]any{"session_id": agStart.SessionID, "callback_url": "http://localhost:8000/callback?code=ag-code&state=" + agStart.SessionID}, "ag-refresh|proj-synthetic-1"},
		{"/admin/copilot/oauth/poll", ownerJWT, map[string]any{"session_id": copilotStart.SessionID}, "gho_copilot_token"},
	} {
		rec := doJSONRequest(t, control.server, http.MethodPost, flow.path, flow.token, flow.body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), flow.expect)
		replay := doJSONRequest(t, control.server, http.MethodPost, flow.path, flow.token, flow.body)
		require.Equal(t, http.StatusBadRequest, replay.Code)
	}
	for _, k := range trackedKeys {
		require.Zero(t, rawRedis.Exists(env.ctx, k).Val())
	}
}

func TestClusterSharedPGAndRedisControlOnlyOAuthState(t *testing.T) {
	dsn, redisURL := os.Getenv("AXONHUB_TEST_PG_DSN"), os.Getenv("AXONHUB_TEST_REDIS_URL")
	if dsn == "" || redisURL == "" || os.Getenv("AXONHUB_TEST_PG_DISPOSABLE") != "1" {
		t.Skip("requires disposable PG18 and Redis")
	}
	env := newOAuthTestEnv(t)

	root, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	schema := "task_oauth_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = root.Exec("CREATE SCHEMA " + schema)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = root.Exec("DROP SCHEMA " + schema + " CASCADE")
		_ = root.Close()
	})

	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	scopedDSN := dsn + sep + "search_path=" + schema
	openDB := func() *ent.Client {
		db, err := sql.Open("pgx", scopedDSN)
		require.NoError(t, err)
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(1)
		return ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	}
	controlDB, dataDB := openDB(), openDB()
	t.Cleanup(func() {
		_ = controlDB.Close()
		_ = dataDB.Close()
	})
	require.NoError(t, controlDB.Schema.Create(env.ctx, migrate.WithForeignKeys(false)))

	owner := controlDB.User.Create().SetEmail("admin@example.test").SetPassword("synthetic").SetIsOwner(true).SetStatus(user.StatusActivated).SaveX(env.ctx)
	ssoUser := controlDB.User.Create().SetEmail("sso-pg@example.test").SetPassword("regular-password").SetStatus(user.StatusActivated).SaveX(env.ctx)
	controlDB.OIDCIdentity.Create().SetUserID(ssoUser.ID).SetIssuer(env.upstream.URL).SetSubject("sub-pg-sso").SetEmail(ssoUser.Email).SetIdpName("Cluster IdP").SaveX(env.ctx)

	cacheCfg := xcache.Config{Mode: xcache.ModeRedis, Namespace: schema, Redis: xredis.Config{URL: redisURL}}
	rawRedis, err := xredis.NewClient(cacheCfg.Redis)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rawRedis.Close()) })

	bootstrap := biz.NewSystemService(biz.SystemServiceParams{Ent: controlDB, CacheConfig: cacheCfg})
	require.NoError(t, bootstrap.SetSecretKey(env.ctx, "synthetic-pg-redis-oauth-jwt-key"))

	control := env.buildNode(controlDB, cluster.Config{Enabled: true, NodeID: "lag", ControlNode: "lag", Nodes: env.nodes}, cacheCfg, cacheCfg)
	data := env.buildNode(dataDB, cluster.Config{Enabled: true, NodeID: "gcp", ControlNode: "lag", Nodes: env.nodes}, cacheCfg, cacheCfg)
	require.True(t, data.oidc.IsUserRestrictedToOIDC(env.ctx, ssoUser))

	ownerJWT, err := control.auth.GenerateJWTToken(env.ctx, owner)
	require.NoError(t, err)

	// Start OIDC link + exchange code + Codex OAuth on control node, verify physical keys/TTLs across data-node restart & 14 route rejections, then consume on control.
	var oidcLinkResp struct {
		Data struct {
			State string `json:"state"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(doJSONRequest(t, control.server, http.MethodGet, "/admin/oidc/link/corp", ownerJWT, nil).Body.Bytes(), &oidcLinkResp))
	require.NotEmpty(t, oidcLinkResp.Data.State)

	exchangeCode := "pg-redis-exchange-" + uuid.NewString()
	oidcCache := xcache.NewFromConfigWithLegacyPrefixes[[]byte](cacheCfg, "oidc_")
	require.NoError(t, oidcCache.Set(env.ctx, "oidc_exchange:"+exchangeCode, fmt.Appendf(nil, "%d", owner.ID), xcache.WithExpiration(5*time.Minute)))

	var codexStart api.StartCodexOAuthResponse
	require.NoError(t, json.Unmarshal(doJSONRequest(t, control.server, http.MethodPost, "/admin/codex/oauth/start", ownerJWT, map[string]any{}).Body.Bytes(), &codexStart))
	require.NotEmpty(t, codexStart.SessionID)

	keys := []string{
		"oidc_state:" + oidcLinkResp.Data.State,
		"oidc_pkce:" + oidcLinkResp.Data.State,
		"oidc_link_state:" + oidcLinkResp.Data.State,
		"oidc_exchange:" + exchangeCode,
		"codex:oauth:" + codexStart.SessionID,
	}
	t.Cleanup(func() {
		require.NoError(t, rawRedis.Del(context.Background(), keys...).Err())
	})

	beforeVals := make(map[string]string, len(keys))
	beforeTTLs := make(map[string]time.Duration, len(keys))
	for _, k := range keys {
		val, err := rawRedis.Get(env.ctx, k).Result()
		require.NoError(t, err)
		ttl := rawRedis.TTL(env.ctx, k).Val()
		require.Positive(t, ttl)
		beforeVals[k] = val
		beforeTTLs[k] = ttl
	}

	restartedData := env.buildNode(dataDB, cluster.Config{Enabled: true, NodeID: "gcp", ControlNode: "lag", Nodes: env.nodes}, cacheCfg, cacheCfg)
	assertDataNodeRejectsAllOAuthManagementRoutes(t, data.server, ownerJWT, oidcLinkResp.Data.State, exchangeCode, codexStart.SessionID, "s", "s", "s")
	assertDataNodeRejectsAllOAuthManagementRoutes(t, restartedData.server, ownerJWT, oidcLinkResp.Data.State, exchangeCode, codexStart.SessionID, "s", "s", "s")

	for _, k := range keys {
		val, err := rawRedis.Get(env.ctx, k).Result()
		require.NoError(t, err)
		require.Equal(t, beforeVals[k], val)
		ttl := rawRedis.TTL(env.ctx, k).Val()
		require.Positive(t, ttl)
		require.LessOrEqual(t, ttl, beforeTTLs[k])
	}

	exRec := doJSONRequest(t, control.server, http.MethodPost, "/oauth/oidc/exchange", "", map[string]any{"code": exchangeCode})
	require.Equal(t, http.StatusOK, exRec.Code, exRec.Body.String())
	require.Equal(t, http.StatusBadRequest, doJSONRequest(t, control.server, http.MethodPost, "/oauth/oidc/exchange", "", map[string]any{"code": exchangeCode}).Code)

	codexEx := map[string]any{"session_id": codexStart.SessionID, "callback_url": "http://localhost:1455/auth/callback?code=c&state=" + codexStart.SessionID}
	require.Equal(t, http.StatusOK, doJSONRequest(t, control.server, http.MethodPost, "/admin/codex/oauth/exchange", ownerJWT, codexEx).Code)
	require.Equal(t, http.StatusBadRequest, doJSONRequest(t, control.server, http.MethodPost, "/admin/codex/oauth/exchange", ownerJWT, codexEx).Code)
}

type roundTripperFunc func(req *http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
