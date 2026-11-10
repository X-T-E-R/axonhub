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

type oauthDomainInstance struct {
	node        cluster.Config
	server      *Server
	oidc        *biz.OIDCService
	oidcHandler *api.OIDCHandlers
	codex       *api.CodexHandlers
	claudecode  *api.ClaudeCodeHandlers
	antigravity *api.AntigravityHandlers
	copilot     *api.CopilotHandlers
}

func TestControlOnlyOAuthStateConstructionAndLifecycle(t *testing.T) {
	gin.SetMode(gin.TestMode)

	provider := sdkmetric.NewMeterProvider()
	require.NoError(t, metrics.SetupMetrics(provider, "oauth-domain-test"))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	mr := miniredis.RunT(t)
	defer mr.Close()

	rawRedis := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer rawRedis.Close()

	var pingCount atomic.Int32
	var writeCount atomic.Int32
	mr.Server().SetPreHook(func(peer *server.Peer, cmd string, args ...string) bool {
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
	ctx := authz.WithTestBypass(context.Background())

	owner := db.User.Create().
		SetEmail("owner@example.test").
		SetPassword("synthetic").
		SetIsOwner(true).
		SetStatus(user.StatusActivated).
		SaveX(ctx)
	ssoUser := db.User.Create().
		SetEmail("sso@example.test").
		SetPassword("regular-password-hash").
		SetStatus(user.StatusActivated).
		SaveX(ctx)
	db.OIDCIdentity.Create().
		SetUserID(ssoUser.ID).
		SetIssuer("https://idp.example.test").
		SetSubject("sub-sso-1").
		SetEmail(ssoUser.Email).
		SetIdpName("Example IdP").
		SaveX(ctx)

	var oidcDiscoveryCalls atomic.Int32
	var oauthTokenCalls atomic.Int32
	var copilotDeviceCalls atomic.Int32
	var copilotTokenCalls atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			oidcDiscoveryCalls.Add(1)
			scheme := "http://"
			_, _ = fmt.Fprintf(w, `{
				"issuer": %q,
				"authorization_endpoint": "https://idp.example.test/authorize",
				"token_endpoint": %q,
				"userinfo_endpoint": %q,
				"jwks_uri": %q
			}`, scheme+r.Host, scheme+r.Host+"/oidc/token", scheme+r.Host+"/oidc/userinfo", scheme+r.Host+"/oidc/jwks")
		case "/oidc/token":
			oauthTokenCalls.Add(1)
			_, _ = w.Write([]byte(`{"access_token":"oidc-access","token_type":"bearer","expires_in":3600}`))
		case "/oidc/userinfo":
			_, _ = w.Write([]byte(`{"sub":"sub-linked-owner","email":"owner@example.test","email_verified":true,"name":"Owner"}`))
		case "/codex/token":
			oauthTokenCalls.Add(1)
			_, _ = w.Write([]byte(`{"access_token":"codex-access","refresh_token":"codex-refresh","expires_in":3600,"token_type":"bearer"}`))
		case "/claudecode/token":
			oauthTokenCalls.Add(1)
			_, _ = w.Write([]byte(`{"access_token":"claude-access","refresh_token":"claude-refresh","expires_in":3600,"token_type":"bearer"}`))
		case "/antigravity/token":
			oauthTokenCalls.Add(1)
			_, _ = w.Write([]byte(`{"access_token":"ag-access","refresh_token":"ag-refresh","expires_in":3600,"token_type":"bearer"}`))
		case "/copilot/device/code":
			copilotDeviceCalls.Add(1)
			_, _ = w.Write([]byte(`{"device_code":"dev-code-1","user_code":"USER-1234","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`))
		case "/copilot/access_token":
			copilotTokenCalls.Add(1)
			_, _ = w.Write([]byte(`{"access_token":"gho_copilot_token","token_type":"bearer","scope":"read:user"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	hc := httpclient.NewHttpClientWithClient(&http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			targetURL := req.URL.String()
			switch targetURL {
			case codex.TokenURL:
				targetURL = upstream.URL + "/codex/token"
			case claudecode.TokenURL:
				targetURL = upstream.URL + "/claudecode/token"
			case antigravity.TokenURL:
				targetURL = upstream.URL + "/antigravity/token"
			case "https://github.com/login/device/code":
				targetURL = upstream.URL + "/copilot/device/code"
			case "https://github.com/login/oauth/access_token":
				targetURL = upstream.URL + "/copilot/access_token"
			}
			if targetURL != req.URL.String() {
				proxyReq, err := http.NewRequestWithContext(req.Context(), req.Method, targetURL, req.Body)
				if err != nil {
					return nil, err
				}
				proxyReq.Header = req.Header.Clone()
				return http.DefaultTransport.RoundTrip(proxyReq)
			}
			return http.DefaultTransport.RoundTrip(req)
		}),
	})
	t.Cleanup(hc.CloseIdleConnections)

	cacheCfg := xcache.Config{
		Mode:      xcache.ModeRedis,
		Namespace: "task-oauth-domains",
		Redis:     xredis.Config{Addr: mr.Addr()},
	}
	memCacheCfg := xcache.Config{Mode: xcache.ModeMemory}
	system := biz.NewSystemService(biz.SystemServiceParams{Ent: db, CacheConfig: memCacheCfg})
	require.NoError(t, system.SetSecretKey(ctx, "synthetic-oauth-domain-jwt-key"))
	users := biz.NewUserService(biz.UserServiceParams{Ent: db, CacheConfig: memCacheCfg})

	oidcCfg := biz.OIDCConfig{
		Providers: []biz.OIDCProvider{
			{
				ID:            "corp",
				Name:          "corp",
				DisplayName:   "Corp SSO",
				Issuer:        "https://idp.example.test",
				IssuerURL:     upstream.URL,
				AuthURL:       "https://idp.example.test/authorize",
				TokenURL:      upstream.URL + "/oidc/token",
				UserInfoURL:   upstream.URL + "/oidc/userinfo",
				ClientID:      "corp-client",
				ClientSecret:  "corp-secret",
				EnablePKCE:    true,
				OIDCLoginOnly: true,
			},
		},
	}

	nodes := map[string]string{
		"lag":              "http://127.0.0.1:18091",
		"gcp":              "http://127.0.0.1:18092",
		"gcp-kiki-usw1":    "http://127.0.0.1:18093",
		"gcp-nailong-usw1": "http://127.0.0.1:18094",
		"tencent-new":      "http://127.0.0.1:18095",
		"tencent-old":      "http://127.0.0.1:18096",
	}

	buildDomains := func(node cluster.Config, cfg xcache.Config) *oauthDomainInstance {
		oidcSvc, err := biz.NewOIDCService(biz.OIDCServiceParams{
			Config:      oidcCfg,
			CacheConfig: cfg,
			DB:          db,
			Node:        node,
		})
		require.NoError(t, err)
		auth := biz.NewAuthService(biz.AuthServiceParams{
			Ent:           db,
			SystemService: system,
			UserService:   users,
			OIDCService:   oidcSvc,
		})
		oidcH := api.NewOIDCHandlers(api.OIDCHandlerParams{
			OIDCService: oidcSvc,
			AuthService: auth,
			PublicURL:   "https://console.example.test",
			Node:        node,
		})
		codexH := api.NewCodexHandlers(api.CodexHandlersParams{
			CacheConfig: cfg,
			HttpClient:  hc,
			Node:        node,
		})
		claudeH := api.NewClaudeCodeHandlers(api.ClaudeCodeHandlersParams{
			CacheConfig: cfg,
			HttpClient:  hc,
			Node:        node,
		})
		agH := api.NewAntigravityHandlers(api.AntigravityHandlersParams{
			CacheConfig: cfg,
			HttpClient:  hc,
			Node:        node,
		})
		copilotH := api.NewCopilotHandlers(api.CopilotHandlersParams{
			CacheConfig: cfg,
			HttpClient:  hc,
			Node:        node,
		})

		srv := New(Config{RequestTimeout: 5 * time.Second, LLMRequestTimeout: 5 * time.Second})
		ip, err := middleware.NewIPAccessControlConfig(false, nil, "")
		require.NoError(t, err)
		SetupRoutes(srv, Handlers{
			OpenAI:      &api.OpenAIHandlers{ResponseWebSocketHandler: &api.ResponsesWebSocketHandler{}},
			System:      api.NewSystemHandlers(api.SystemHandlersParams{SystemService: system}),
			Auth:        api.NewAuthHandlers(api.AuthHandlersParams{AuthService: auth}),
			Codex:       codexH,
			ClaudeCode:  claudeH,
			Antigravity: agH,
			Copilot:     copilotH,
			OIDC:        oidcH,
		}, db, Services{
			AuthService:   auth,
			SystemService: system,
			Node:          node,
		}, ip)

		return &oauthDomainInstance{
			node:        node,
			server:      srv,
			oidc:        oidcSvc,
			oidcHandler: oidcH,
			codex:       codexH,
			claudecode:  claudeH,
			antigravity: agH,
			copilot:     copilotH,
		}
	}

	// 1. Standalone node constructs all 5 OAuth/OIDC Redis state clients (5 PINGs).
	pingCount.Store(0)
	standalone := buildDomains(cluster.Config{}, cacheCfg)
	require.Equal(t, int32(5), pingCount.Load(), "standalone must initialize all 5 OAuth/OIDC Redis state clients")
	require.NotNil(t, standalone.oidc)

	// 2. Control node ("lag") constructs all 5 OAuth/OIDC Redis state clients (5 PINGs).
	pingCount.Store(0)
	discoveryBeforeControl := oidcDiscoveryCalls.Load()
	controlNode := cluster.Config{Enabled: true, NodeID: "lag", ControlNode: "lag", Nodes: nodes}
	control := buildDomains(controlNode, cacheCfg)
	require.Equal(t, int32(5), pingCount.Load(), "control node must initialize all 5 OAuth/OIDC Redis state clients")
	require.Equal(t, discoveryBeforeControl+1, oidcDiscoveryCalls.Load(), "control node performs OIDC provider discovery")

	// 3. All 5 non-control data nodes construct 0 OAuth/OIDC Redis state clients (0 PINGs, 0 IdP discovery calls),
	// even if their OAuth state Redis address were unreachable.
	unreachableCacheCfg := xcache.Config{
		Mode:      xcache.ModeRedis,
		Namespace: "task-oauth-domains",
		Redis:     xredis.Config{Addr: "127.0.0.1:1"},
	}
	pingCount.Store(0)
	discoveryBeforeData := oidcDiscoveryCalls.Load()
	dataNodes := make(map[string]*oauthDomainInstance, 5)
	for _, id := range []string{"gcp", "gcp-kiki-usw1", "gcp-nailong-usw1", "tencent-new", "tencent-old"} {
		n := cluster.Config{Enabled: true, NodeID: id, ControlNode: "lag", Nodes: nodes}
		dataNodes[id] = buildDomains(n, unreachableCacheCfg)
	}
	require.Zero(t, pingCount.Load(), "5 non-control data nodes must initialize 0 OAuth/OIDC Redis state clients (delta -25 across 5 nodes)")
	require.Equal(t, discoveryBeforeData, oidcDiscoveryCalls.Load(), "data nodes must not dial OIDC discovery during construction")

	// 4. Verify non-session OIDCService capabilities still work on data nodes, and direct calls to
	// management methods fail safely without nil panic or fake success.
	gcpInst := dataNodes["gcp"]
	require.Equal(t, 1, gcpInst.oidc.CountProviders())
	require.True(t, gcpInst.oidc.IsUserRestrictedToOIDC(ctx, ssoUser), "data node OIDCService must preserve IsUserRestrictedToOIDC")
	require.False(t, gcpInst.oidc.IsUserRestrictedToOIDC(ctx, owner))
	providers := gcpInst.oidc.GetProviders(contexts.WithUser(ent.NewContext(ctx, db), ssoUser))
	require.Len(t, providers, 1)
	require.Equal(t, "corp", providers[0].ID)
	require.True(t, providers[0].IsLinked)

	_, _, err := gcpInst.oidc.GetAuthorizeURL(ctx, "corp", "https://console.example.test")
	require.ErrorContains(t, err, "unavailable on data node")
	_, _, err = gcpInst.oidc.GetLinkAuthorizeURL(ctx, "corp", "https://console.example.test", owner.ID)
	require.ErrorContains(t, err, "unavailable on data node")
	_, _, err = gcpInst.oidc.Callback(ctx, "corp", "code", "state", "https://console.example.test")
	require.ErrorContains(t, err, "unavailable on data node")
	_, err = gcpInst.oidc.ExchangeCode(ctx, "exchange-code")
	require.ErrorContains(t, err, "unavailable on data node")

	// Direct handler invocations on data node (even if called outside DataNodeRoutes) must not panic or touch upstream/Redis.
	beforeDeviceCalls := copilotDeviceCalls.Load()
	for _, directCall := range []func(*gin.Context){
		gcpInst.codex.StartOAuth,
		gcpInst.codex.Exchange,
		gcpInst.claudecode.StartOAuth,
		gcpInst.claudecode.Exchange,
		gcpInst.antigravity.StartOAuth,
		gcpInst.antigravity.Exchange,
		gcpInst.copilot.StartOAuth,
		gcpInst.copilot.PollOAuth,
	} {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/direct", bytes.NewBufferString(`{"session_id":"s","callback_url":"http://localhost/cb?code=c&state=s"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		directCall(c)
		require.Equal(t, http.StatusNotFound, rec.Code)
	}
	require.Equal(t, beforeDeviceCalls, copilotDeviceCalls.Load(), "data node Copilot handler must not request device code from GitHub")

	// 5. Create synthetic OAuth/OIDC states on control node across all 5 legacy prefixes.
	authSvc := biz.NewAuthService(biz.AuthServiceParams{Ent: db, SystemService: system, UserService: users, OIDCService: control.oidc})
	ownerJWT, err := authSvc.GenerateJWTToken(ctx, owner)
	require.NoError(t, err)

	doJSON := func(srv *Server, method, path, token string, body any) *httptest.ResponseRecorder {
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

	// 5a. OIDC authorize + link state + synthetic exchange code on control node
	oidcLinkRec := doJSON(control.server, http.MethodGet, "/admin/oidc/link/corp", ownerJWT, nil)
	require.Equal(t, http.StatusOK, oidcLinkRec.Code, oidcLinkRec.Body.String())
	var oidcLinkResp struct {
		Data struct {
			URL   string `json:"url"`
			State string `json:"state"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(oidcLinkRec.Body.Bytes(), &oidcLinkResp))
	require.NotEmpty(t, oidcLinkResp.Data.State)

	syntheticExchangeCode := "synthetic-exchange-code-1"
	oidcExchangeCache := xcache.NewFromConfigWithLegacyPrefixes[[]byte](cacheCfg, "oidc_")
	require.NoError(t, oidcExchangeCache.Set(ctx, "oidc_exchange:"+syntheticExchangeCode, fmt.Appendf(nil, "%d", owner.ID), xcache.WithExpiration(5*time.Minute)))

	// 5b. Codex start on control node
	codexStartRec := doJSON(control.server, http.MethodPost, "/admin/codex/oauth/start", ownerJWT, map[string]any{})
	require.Equal(t, http.StatusOK, codexStartRec.Code, codexStartRec.Body.String())
	var codexStart api.StartCodexOAuthResponse
	require.NoError(t, json.Unmarshal(codexStartRec.Body.Bytes(), &codexStart))
	require.NotEmpty(t, codexStart.SessionID)

	// 5c. ClaudeCode start on control node
	claudeStartRec := doJSON(control.server, http.MethodPost, "/admin/claudecode/oauth/start", ownerJWT, map[string]any{})
	require.Equal(t, http.StatusOK, claudeStartRec.Code, claudeStartRec.Body.String())
	var claudeStart api.StartClaudeCodeOAuthResponse
	require.NoError(t, json.Unmarshal(claudeStartRec.Body.Bytes(), &claudeStart))
	require.NotEmpty(t, claudeStart.SessionID)

	// 5d. Antigravity start on control node
	agStartRec := doJSON(control.server, http.MethodPost, "/admin/antigravity/oauth/start", ownerJWT, map[string]any{"project_id": "proj-synthetic-1"})
	require.Equal(t, http.StatusOK, agStartRec.Code, agStartRec.Body.String())
	var agStart api.StartAntigravityOAuthResponse
	require.NoError(t, json.Unmarshal(agStartRec.Body.Bytes(), &agStart))
	require.NotEmpty(t, agStart.SessionID)

	// 5e. Copilot device flow start on control node
	copilotStartRec := doJSON(control.server, http.MethodPost, "/admin/copilot/oauth/start", ownerJWT, map[string]any{})
	require.Equal(t, http.StatusOK, copilotStartRec.Code, copilotStartRec.Body.String())
	var copilotStart api.StartCopilotOAuthResponse
	require.NoError(t, json.Unmarshal(copilotStartRec.Body.Bytes(), &copilotStart))
	require.NotEmpty(t, copilotStart.SessionID)

	// Verify raw physical keys and positive TTLs in Redis before data-node upgrade/rollback
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
		val, err := rawRedis.Get(ctx, k).Result()
		require.NoError(t, err, "expected physical key %s to exist in Redis", k)
		ttl := mr.TTL(k)
		require.Positive(t, ttl, "expected physical key %s to have positive TTL", k)
		initialValues[k] = val
		initialTTLs[k] = ttl
	}

	// Advance time slightly so any accidental TTL renewal would be observable
	mr.FastForward(30 * time.Second)

	// 6. Simulate data node upgrade/restart and hit management OAuth/OIDC routes on data node:
	// must reject with 404 control_node_required, 0 Redis PINGs, 0 Redis writes, and preserve keys & TTLs.
	pingCount.Store(0)
	writeCount.Store(0)
	restartedData := buildDomains(cluster.Config{Enabled: true, NodeID: "gcp", ControlNode: "lag", Nodes: nodes}, cacheCfg)
	require.Zero(t, pingCount.Load(), "data node restart with shared Redis config must not PING Redis for OAuth/OIDC domains")

	for _, check := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodGet, "/oauth/oidc/providers", nil},
		{http.MethodGet, "/oauth/oidc/authorize/corp", nil},
		{http.MethodGet, "/oauth/oidc/callback/corp?code=test-code&state=" + url.QueryEscape(oidcLinkResp.Data.State), nil},
		{http.MethodPost, "/oauth/oidc/exchange", map[string]any{"code": syntheticExchangeCode}},
		{http.MethodGet, "/portal/oidc/link/corp", nil},
		{http.MethodGet, "/admin/oidc/link/corp", nil},
		{http.MethodPost, "/admin/codex/oauth/start", map[string]any{}},
		{http.MethodPost, "/admin/codex/oauth/exchange", map[string]any{"session_id": codexStart.SessionID, "callback_url": "http://localhost:1455/auth/callback?code=c&state=" + codexStart.SessionID}},
		{http.MethodPost, "/admin/claudecode/oauth/start", map[string]any{}},
		{http.MethodPost, "/admin/claudecode/oauth/exchange", map[string]any{"session_id": claudeStart.SessionID, "callback_url": "http://localhost:54545/callback?code=c#" + claudeStart.SessionID}},
		{http.MethodPost, "/admin/antigravity/oauth/start", map[string]any{}},
		{http.MethodPost, "/admin/antigravity/oauth/exchange", map[string]any{"session_id": agStart.SessionID, "callback_url": "http://localhost:8000/callback?code=c&state=" + agStart.SessionID}},
		{http.MethodPost, "/admin/copilot/oauth/start", map[string]any{}},
		{http.MethodPost, "/admin/copilot/oauth/poll", map[string]any{"session_id": copilotStart.SessionID}},
	} {
		rec := doJSON(restartedData.server, check.method, check.path, ownerJWT, check.body)
		require.Equal(t, http.StatusNotFound, rec.Code, "path %s on data node must return 404", check.path)
		require.Contains(t, rec.Body.String(), `"control_node_required"`)
		require.Contains(t, rec.Body.String(), `"execution":"not_executed"`)
	}
	require.Zero(t, writeCount.Load(), "data node rejections must perform 0 Redis writes")

	for _, k := range trackedKeys {
		val, err := rawRedis.Get(ctx, k).Result()
		require.NoError(t, err, "key %s must not be lost after data node lifecycle", k)
		require.Equal(t, initialValues[k], val, "key %s value must be untouched", k)
		require.Equal(t, initialTTLs[k]-30*time.Second, mr.TTL(k), "key %s TTL must not be renewed or altered by data node", k)
	}

	// 7. Complete all in-flight OAuth/OIDC flows on the control node and verify single-use consumption.
	// 7a. OIDC Callback (consumes oidc_state, oidc_pkce, oidc_link_state) and exchange code single-use redemption
	oidcCbRec := doJSON(control.server, http.MethodGet, "/oauth/oidc/callback/corp?code=oidc-code&state="+url.QueryEscape(oidcLinkResp.Data.State), "", nil)
	require.Equal(t, http.StatusFound, oidcCbRec.Code)
	require.Contains(t, oidcCbRec.Header().Get("Location"), "/settings/profile?oidc_link=success")
	for _, consumedKey := range []string{
		"oidc_state:" + oidcLinkResp.Data.State,
		"oidc_pkce:" + oidcLinkResp.Data.State,
		"oidc_link_state:" + oidcLinkResp.Data.State,
	} {
		_, err := rawRedis.Get(ctx, consumedKey).Result()
		require.Error(t, err, "consumed OIDC state key %s must be deleted after callback", consumedKey)
	}
	oidcCbReplay := doJSON(control.server, http.MethodGet, "/oauth/oidc/callback/corp?code=oidc-code&state="+url.QueryEscape(oidcLinkResp.Data.State), "", nil)
	require.Equal(t, http.StatusFound, oidcCbReplay.Code)
	require.Contains(t, oidcCbReplay.Header().Get("Location"), "error=auth_failed")

	oidcExRec := doJSON(control.server, http.MethodPost, "/oauth/oidc/exchange", "", map[string]any{"code": syntheticExchangeCode})
	require.Equal(t, http.StatusOK, oidcExRec.Code, oidcExRec.Body.String())
	require.Contains(t, oidcExRec.Body.String(), `"token"`)
	oidcExReplay := doJSON(control.server, http.MethodPost, "/oauth/oidc/exchange", "", map[string]any{"code": syntheticExchangeCode})
	require.Equal(t, http.StatusBadRequest, oidcExReplay.Code)

	// 7b. Codex exchange single-use redemption
	codexExBody := map[string]any{
		"session_id":   codexStart.SessionID,
		"callback_url": "http://localhost:1455/auth/callback?code=codex-code&state=" + codexStart.SessionID,
	}
	codexExRec := doJSON(control.server, http.MethodPost, "/admin/codex/oauth/exchange", ownerJWT, codexExBody)
	require.Equal(t, http.StatusOK, codexExRec.Code, codexExRec.Body.String())
	require.Contains(t, codexExRec.Body.String(), "codex-access")
	codexExReplay := doJSON(control.server, http.MethodPost, "/admin/codex/oauth/exchange", ownerJWT, codexExBody)
	require.Equal(t, http.StatusBadRequest, codexExReplay.Code)

	// 7c. ClaudeCode exchange single-use redemption
	claudeExBody := map[string]any{
		"session_id":   claudeStart.SessionID,
		"callback_url": "http://localhost:54545/callback?code=claude-code#" + claudeStart.SessionID,
	}
	claudeExRec := doJSON(control.server, http.MethodPost, "/admin/claudecode/oauth/exchange", ownerJWT, claudeExBody)
	require.Equal(t, http.StatusOK, claudeExRec.Code, claudeExRec.Body.String())
	require.Contains(t, claudeExRec.Body.String(), "claude-access")
	claudeExReplay := doJSON(control.server, http.MethodPost, "/admin/claudecode/oauth/exchange", ownerJWT, claudeExBody)
	require.Equal(t, http.StatusBadRequest, claudeExReplay.Code)

	// 7d. Antigravity exchange single-use redemption
	agExBody := map[string]any{
		"session_id":   agStart.SessionID,
		"callback_url": "http://localhost:8000/callback?code=ag-code&state=" + agStart.SessionID,
	}
	agExRec := doJSON(control.server, http.MethodPost, "/admin/antigravity/oauth/exchange", ownerJWT, agExBody)
	require.Equal(t, http.StatusOK, agExRec.Code, agExRec.Body.String())
	require.Contains(t, agExRec.Body.String(), "ag-refresh|proj-synthetic-1")
	agExReplay := doJSON(control.server, http.MethodPost, "/admin/antigravity/oauth/exchange", ownerJWT, agExBody)
	require.Equal(t, http.StatusBadRequest, agExReplay.Code)

	// 7e. Copilot poll single-use redemption
	copilotPollBody := map[string]any{"session_id": copilotStart.SessionID}
	copilotPollRec := doJSON(control.server, http.MethodPost, "/admin/copilot/oauth/poll", ownerJWT, copilotPollBody)
	require.Equal(t, http.StatusOK, copilotPollRec.Code, copilotPollRec.Body.String())
	require.Contains(t, copilotPollRec.Body.String(), "gho_copilot_token")
	copilotPollReplay := doJSON(control.server, http.MethodPost, "/admin/copilot/oauth/poll", ownerJWT, copilotPollBody)
	require.Equal(t, http.StatusBadRequest, copilotPollReplay.Code)
}

func TestClusterSharedPGAndRedisControlOnlyOAuthState(t *testing.T) {
	dsn, redisURL := os.Getenv("AXONHUB_TEST_PG_DSN"), os.Getenv("AXONHUB_TEST_REDIS_URL")
	if dsn == "" || redisURL == "" || os.Getenv("AXONHUB_TEST_PG_DISPOSABLE") != "1" {
		t.Skip("requires disposable PG18 and Redis")
	}
	gin.SetMode(gin.TestMode)

	provider := sdkmetric.NewMeterProvider()
	require.NoError(t, metrics.SetupMetrics(provider, "cluster-oauth-pg-redis-test"))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

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

	ctx := authz.WithTestBypass(context.Background())
	require.NoError(t, controlDB.Schema.Create(ctx, migrate.WithForeignKeys(false)))

	owner := controlDB.User.Create().
		SetEmail("admin@example.test").
		SetPassword("synthetic").
		SetIsOwner(true).
		SetStatus(user.StatusActivated).
		SaveX(ctx)
	ssoUser := controlDB.User.Create().
		SetEmail("sso-pg@example.test").
		SetPassword("regular-password").
		SetStatus(user.StatusActivated).
		SaveX(ctx)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			scheme := "http://"
			_, _ = fmt.Fprintf(w, `{
				"issuer": %q,
				"authorization_endpoint": "https://idp.cluster.test/authorize",
				"token_endpoint": %q,
				"userinfo_endpoint": %q,
				"jwks_uri": %q
			}`, scheme+r.Host, scheme+r.Host+"/oidc/token", scheme+r.Host+"/oidc/userinfo", scheme+r.Host+"/oidc/jwks")
		case "/oidc/token":
			_, _ = w.Write([]byte(`{"access_token":"oidc-access","token_type":"bearer","expires_in":3600}`))
		case "/oidc/userinfo":
			_, _ = w.Write([]byte(`{"sub":"sub-pg-owner","email":"admin@example.test","email_verified":true,"name":"Admin"}`))
		case "/codex/token":
			_, _ = w.Write([]byte(`{"access_token":"codex-pg-access","refresh_token":"codex-pg-refresh","expires_in":3600,"token_type":"bearer"}`))
		case "/claudecode/token":
			_, _ = w.Write([]byte(`{"access_token":"claude-pg-access","refresh_token":"claude-pg-refresh","expires_in":3600,"token_type":"bearer"}`))
		case "/antigravity/token":
			_, _ = w.Write([]byte(`{"access_token":"ag-pg-access","refresh_token":"ag-pg-refresh","expires_in":3600,"token_type":"bearer"}`))
		case "/copilot/device/code":
			_, _ = w.Write([]byte(`{"device_code":"dev-pg-1","user_code":"PG-1234","verification_uri":"https://github.com/login/device","expires_in":900,"interval":5}`))
		case "/copilot/access_token":
			_, _ = w.Write([]byte(`{"access_token":"gho_pg_token","token_type":"bearer","scope":"read:user"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	controlDB.OIDCIdentity.Create().
		SetUserID(ssoUser.ID).
		SetIssuer(upstream.URL).
		SetSubject("sub-pg-sso").
		SetEmail(ssoUser.Email).
		SetIdpName("Cluster IdP").
		SaveX(ctx)

	hc := httpclient.NewHttpClientWithClient(&http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			targetURL := req.URL.String()
			switch targetURL {
			case codex.TokenURL:
				targetURL = upstream.URL + "/codex/token"
			case claudecode.TokenURL:
				targetURL = upstream.URL + "/claudecode/token"
			case antigravity.TokenURL:
				targetURL = upstream.URL + "/antigravity/token"
			case "https://github.com/login/device/code":
				targetURL = upstream.URL + "/copilot/device/code"
			case "https://github.com/login/oauth/access_token":
				targetURL = upstream.URL + "/copilot/access_token"
			}
			if targetURL != req.URL.String() {
				proxyReq, err := http.NewRequestWithContext(req.Context(), req.Method, targetURL, req.Body)
				if err != nil {
					return nil, err
				}
				proxyReq.Header = req.Header.Clone()
				return http.DefaultTransport.RoundTrip(proxyReq)
			}
			return http.DefaultTransport.RoundTrip(req)
		}),
	})
	t.Cleanup(hc.CloseIdleConnections)

	cacheCfg := xcache.Config{
		Mode:      xcache.ModeRedis,
		Namespace: schema,
		Redis:     xredis.Config{URL: redisURL},
	}
	rawRedis, err := xredis.NewClient(cacheCfg.Redis)
	require.NoError(t, err)
	defer rawRedis.Close()

	bootstrap := biz.NewSystemService(biz.SystemServiceParams{Ent: controlDB, CacheConfig: cacheCfg})
	require.NoError(t, bootstrap.SetSecretKey(ctx, "synthetic-pg-redis-oauth-jwt-key"))

	nodes := map[string]string{
		"lag": "http://127.0.0.1:28091",
		"gcp": "http://127.0.0.1:28092",
	}
	oidcCfg := biz.OIDCConfig{
		Providers: []biz.OIDCProvider{{
			ID:            "corp",
			Name:          "corp",
			DisplayName:   "Corp SSO",
			Issuer:        upstream.URL,
			IssuerURL:     upstream.URL,
			AuthURL:       "https://idp.cluster.test/authorize",
			TokenURL:      upstream.URL + "/oidc/token",
			UserInfoURL:   upstream.URL + "/oidc/userinfo",
			ClientID:      "corp-client",
			ClientSecret:  "corp-secret",
			EnablePKCE:    true,
			OIDCLoginOnly: true,
		}},
	}

	buildNode := func(dbClient *ent.Client, id string) (*Server, *biz.OIDCService, *biz.AuthService) {
		node := cluster.Config{Enabled: true, NodeID: id, ControlNode: "lag", Nodes: nodes}
		sys := biz.NewSystemService(biz.SystemServiceParams{Ent: dbClient, CacheConfig: cacheCfg})
		usr := biz.NewUserService(biz.UserServiceParams{Ent: dbClient, CacheConfig: cacheCfg})
		oidcSvc, err := biz.NewOIDCService(biz.OIDCServiceParams{
			Config:      oidcCfg,
			CacheConfig: cacheCfg,
			DB:          dbClient,
			Node:        node,
		})
		require.NoError(t, err)
		auth := biz.NewAuthService(biz.AuthServiceParams{
			Ent:           dbClient,
			SystemService: sys,
			UserService:   usr,
			OIDCService:   oidcSvc,
		})
		srv := New(Config{RequestTimeout: 5 * time.Second, LLMRequestTimeout: 5 * time.Second})
		ip, err := middleware.NewIPAccessControlConfig(false, nil, "")
		require.NoError(t, err)
		SetupRoutes(srv, Handlers{
			OpenAI:      &api.OpenAIHandlers{ResponseWebSocketHandler: &api.ResponsesWebSocketHandler{}},
			System:      api.NewSystemHandlers(api.SystemHandlersParams{SystemService: sys}),
			Auth:        api.NewAuthHandlers(api.AuthHandlersParams{AuthService: auth}),
			Codex:       api.NewCodexHandlers(api.CodexHandlersParams{CacheConfig: cacheCfg, HttpClient: hc, Node: node}),
			ClaudeCode:  api.NewClaudeCodeHandlers(api.ClaudeCodeHandlersParams{CacheConfig: cacheCfg, HttpClient: hc, Node: node}),
			Antigravity: api.NewAntigravityHandlers(api.AntigravityHandlersParams{CacheConfig: cacheCfg, HttpClient: hc, Node: node}),
			Copilot:     api.NewCopilotHandlers(api.CopilotHandlersParams{CacheConfig: cacheCfg, HttpClient: hc, Node: node}),
			OIDC:        api.NewOIDCHandlers(api.OIDCHandlerParams{OIDCService: oidcSvc, AuthService: auth, PublicURL: "https://console.cluster.test", Node: node}),
		}, dbClient, Services{
			AuthService:   auth,
			SystemService: sys,
			Node:          node,
		}, ip)
		return srv, oidcSvc, auth
	}

	controlSrv, _, controlAuth := buildNode(controlDB, "lag")
	dataSrv, dataOIDC, _ := buildNode(dataDB, "gcp")

	// Data node retains non-session OIDC restriction check against shared PG18
	require.True(t, dataOIDC.IsUserRestrictedToOIDC(ctx, ssoUser))

	ownerJWT, err := controlAuth.GenerateJWTToken(ctx, owner)
	require.NoError(t, err)

	doJSON := func(srv *Server, method, path, token string, body any) *httptest.ResponseRecorder {
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

	// 1. Create synthetic OAuth/OIDC states on control node across all 5 legacy prefixes
	oidcLinkRec := doJSON(controlSrv, http.MethodGet, "/admin/oidc/link/corp", ownerJWT, nil)
	require.Equal(t, http.StatusOK, oidcLinkRec.Code, oidcLinkRec.Body.String())
	var oidcLinkResp struct {
		Data struct {
			State string `json:"state"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(oidcLinkRec.Body.Bytes(), &oidcLinkResp))
	require.NotEmpty(t, oidcLinkResp.Data.State)

	exchangeCode := "pg-redis-exchange-" + uuid.NewString()
	oidcCache := xcache.NewFromConfigWithLegacyPrefixes[[]byte](cacheCfg, "oidc_")
	require.NoError(t, oidcCache.Set(ctx, "oidc_exchange:"+exchangeCode, fmt.Appendf(nil, "%d", owner.ID), xcache.WithExpiration(5*time.Minute)))

	codexRec := doJSON(controlSrv, http.MethodPost, "/admin/codex/oauth/start", ownerJWT, map[string]any{})
	require.Equal(t, http.StatusOK, codexRec.Code)
	var codexStart api.StartCodexOAuthResponse
	require.NoError(t, json.Unmarshal(codexRec.Body.Bytes(), &codexStart))

	claudeRec := doJSON(controlSrv, http.MethodPost, "/admin/claudecode/oauth/start", ownerJWT, map[string]any{})
	require.Equal(t, http.StatusOK, claudeRec.Code)
	var claudeStart api.StartClaudeCodeOAuthResponse
	require.NoError(t, json.Unmarshal(claudeRec.Body.Bytes(), &claudeStart))

	agRec := doJSON(controlSrv, http.MethodPost, "/admin/antigravity/oauth/start", ownerJWT, map[string]any{"project_id": "proj-pg-1"})
	require.Equal(t, http.StatusOK, agRec.Code)
	var agStart api.StartAntigravityOAuthResponse
	require.NoError(t, json.Unmarshal(agRec.Body.Bytes(), &agStart))

	copilotRec := doJSON(controlSrv, http.MethodPost, "/admin/copilot/oauth/start", ownerJWT, map[string]any{})
	require.Equal(t, http.StatusOK, copilotRec.Code)
	var copilotStart api.StartCopilotOAuthResponse
	require.NoError(t, json.Unmarshal(copilotRec.Body.Bytes(), &copilotStart))

	keys := []string{
		"oidc_state:" + oidcLinkResp.Data.State,
		"oidc_pkce:" + oidcLinkResp.Data.State,
		"oidc_link_state:" + oidcLinkResp.Data.State,
		"oidc_exchange:" + exchangeCode,
		"codex:oauth:" + codexStart.SessionID,
		"claudecode:oauth:" + claudeStart.SessionID,
		"antigravity:oauth:" + agStart.SessionID,
		"copilot:oauth:" + copilotStart.SessionID,
	}
	t.Cleanup(func() { _ = rawRedis.Del(context.Background(), keys...).Err() })

	beforeVals := make(map[string]string, len(keys))
	beforeTTLs := make(map[string]time.Duration, len(keys))
	for _, k := range keys {
		val, err := rawRedis.Get(ctx, k).Result()
		require.NoError(t, err, "expected physical key %s in shared Redis", k)
		ttl := rawRedis.TTL(ctx, k).Val()
		require.Positive(t, ttl)
		beforeVals[k] = val
		beforeTTLs[k] = ttl
	}

	// 2. Data node upgrade/restart and management requests: rejected with 404 control_node_required,
	// keys and TTLs in shared Redis remain intact.
	restartedDataSrv, _, _ := buildNode(dataDB, "gcp")
	for _, srv := range []*Server{dataSrv, restartedDataSrv} {
		for _, path := range []struct {
			method string
			path   string
			body   any
		}{
			{http.MethodGet, "/oauth/oidc/providers", nil},
			{http.MethodGet, "/oauth/oidc/authorize/corp", nil},
			{http.MethodGet, "/oauth/oidc/callback/corp?code=c&state=" + url.QueryEscape(oidcLinkResp.Data.State), nil},
			{http.MethodPost, "/oauth/oidc/exchange", map[string]any{"code": exchangeCode}},
			{http.MethodGet, "/portal/oidc/link/corp", nil},
			{http.MethodGet, "/admin/oidc/link/corp", nil},
			{http.MethodPost, "/admin/codex/oauth/start", map[string]any{}},
			{http.MethodPost, "/admin/codex/oauth/exchange", map[string]any{"session_id": codexStart.SessionID, "callback_url": "http://localhost:1455/auth/callback?code=c&state=" + codexStart.SessionID}},
			{http.MethodPost, "/admin/claudecode/oauth/start", map[string]any{}},
			{http.MethodPost, "/admin/claudecode/oauth/exchange", map[string]any{"session_id": claudeStart.SessionID, "callback_url": "http://localhost:54545/callback?code=c#" + claudeStart.SessionID}},
			{http.MethodPost, "/admin/antigravity/oauth/start", map[string]any{}},
			{http.MethodPost, "/admin/antigravity/oauth/exchange", map[string]any{"session_id": agStart.SessionID, "callback_url": "http://localhost:8000/callback?code=c&state=" + agStart.SessionID}},
			{http.MethodPost, "/admin/copilot/oauth/start", map[string]any{}},
			{http.MethodPost, "/admin/copilot/oauth/poll", map[string]any{"session_id": copilotStart.SessionID}},
		} {
			rec := doJSON(srv, path.method, path.path, ownerJWT, path.body)
			require.Equal(t, http.StatusNotFound, rec.Code)
			require.Contains(t, rec.Body.String(), `"control_node_required"`)
		}
	}

	for _, k := range keys {
		val, err := rawRedis.Get(ctx, k).Result()
		require.NoError(t, err)
		require.Equal(t, beforeVals[k], val)
		ttl := rawRedis.TTL(ctx, k).Val()
		require.Positive(t, ttl)
		require.LessOrEqual(t, ttl, beforeTTLs[k])
	}

	// 3. Control node completes all flows and enforces single-use deletion
	cbRec := doJSON(controlSrv, http.MethodGet, "/oauth/oidc/callback/corp?code=oidc-code&state="+url.QueryEscape(oidcLinkResp.Data.State), "", nil)
	require.Equal(t, http.StatusFound, cbRec.Code)
	require.Contains(t, cbRec.Header().Get("Location"), "/settings/profile?oidc_link=success")

	exRec := doJSON(controlSrv, http.MethodPost, "/oauth/oidc/exchange", "", map[string]any{"code": exchangeCode})
	require.Equal(t, http.StatusOK, exRec.Code, exRec.Body.String())
	require.Equal(t, http.StatusBadRequest, doJSON(controlSrv, http.MethodPost, "/oauth/oidc/exchange", "", map[string]any{"code": exchangeCode}).Code)

	codexEx := map[string]any{"session_id": codexStart.SessionID, "callback_url": "http://localhost:1455/auth/callback?code=c&state=" + codexStart.SessionID}
	require.Equal(t, http.StatusOK, doJSON(controlSrv, http.MethodPost, "/admin/codex/oauth/exchange", ownerJWT, codexEx).Code)
	require.Equal(t, http.StatusBadRequest, doJSON(controlSrv, http.MethodPost, "/admin/codex/oauth/exchange", ownerJWT, codexEx).Code)

	claudeEx := map[string]any{"session_id": claudeStart.SessionID, "callback_url": "http://localhost:54545/callback?code=c#" + claudeStart.SessionID}
	require.Equal(t, http.StatusOK, doJSON(controlSrv, http.MethodPost, "/admin/claudecode/oauth/exchange", ownerJWT, claudeEx).Code)
	require.Equal(t, http.StatusBadRequest, doJSON(controlSrv, http.MethodPost, "/admin/claudecode/oauth/exchange", ownerJWT, claudeEx).Code)

	agEx := map[string]any{"session_id": agStart.SessionID, "callback_url": "http://localhost:8000/callback?code=c&state=" + agStart.SessionID}
	require.Equal(t, http.StatusOK, doJSON(controlSrv, http.MethodPost, "/admin/antigravity/oauth/exchange", ownerJWT, agEx).Code)
	require.Equal(t, http.StatusBadRequest, doJSON(controlSrv, http.MethodPost, "/admin/antigravity/oauth/exchange", ownerJWT, agEx).Code)

	copilotPoll := map[string]any{"session_id": copilotStart.SessionID}
	require.Equal(t, http.StatusOK, doJSON(controlSrv, http.MethodPost, "/admin/copilot/oauth/poll", ownerJWT, copilotPoll).Code)
	require.Equal(t, http.StatusBadRequest, doJSON(controlSrv, http.MethodPost, "/admin/copilot/oauth/poll", ownerJWT, copilotPoll).Code)

	for _, k := range keys {
		require.Zero(t, rawRedis.Exists(ctx, k).Val(), "key %s must be consumed after control-node completion", k)
	}
}

type roundTripperFunc func(req *http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
