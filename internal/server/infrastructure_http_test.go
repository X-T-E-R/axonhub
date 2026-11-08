package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/metrics"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/llm/httpclient"
)

func startManagedControlBridgeForTest(t *testing.T, snapPath string) string {
	t.Helper()
	script, err := filepath.Abs("../../../axon-edge-worker/control/axon-bridge.mjs")
	require.NoError(t, err)
	dataDir := filepath.Join(t.TempDir(), "managed-control-state")
	cmd := exec.Command("node", script, "--port", "0", "--data", dataDir, "--komodo-snapshot", snapPath)
	cmd.Dir = filepath.Dir(filepath.Dir(script))
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})

	scanner := bufio.NewScanner(stdout)
	require.True(t, scanner.Scan(), "bridge exited early: %s", stderr.String())
	var ready struct {
		URL string `json:"url"`
	}
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &ready))
	require.NotEmpty(t, ready.URL)
	return strings.TrimRight(ready.URL, "/")
}

func TestInfrastructureHTTP(t *testing.T) {
	snapPath := filepath.Clean("C:/Programs/AI/MLAIGateWay/outputs/task51-komodo-reconcile-20261108/core-final.json")
	bridgeURL := startManagedControlBridgeForTest(t, snapPath)

	provider := sdkmetric.NewMeterProvider()
	require.NoError(t, metrics.SetupMetrics(provider, "infra-test"))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	client := enttest.NewEntClient(t, "sqlite3", "file:infra-http-"+uuid.NewString()+"?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })
	ctx := authz.WithTestBypass(context.Background())

	owner := client.User.Create().SetEmail("owner@example.test").SetPassword("synthetic").SetIsOwner(true).SetStatus(user.StatusActivated).SaveX(ctx)
	reader := client.User.Create().SetEmail("reader@example.test").SetPassword("synthetic").SetScopes([]string{string(scopes.ScopeReadChannels)}).SetStatus(user.StatusActivated).SaveX(ctx)
	noPerm := client.User.Create().SetEmail("noperm@example.test").SetPassword("synthetic").SetScopes([]string{string(scopes.ScopeReadDashboard)}).SetStatus(user.StatusActivated).SaveX(ctx)

	cache := xcache.Config{Mode: xcache.ModeMemory}
	system := biz.NewSystemService(biz.SystemServiceParams{Ent: client, CacheConfig: cache})
	require.NoError(t, system.SetSecretKey(ctx, "synthetic-infra-jwt-key"))
	users := biz.NewUserService(biz.UserServiceParams{Ent: client, CacheConfig: cache})
	auth := biz.NewAuthService(biz.AuthServiceParams{Ent: client, SystemService: system, UserService: users})

	ownerJWT, err := auth.GenerateJWTToken(ctx, owner)
	require.NoError(t, err)
	readerJWT, err := auth.GenerateJWTToken(ctx, reader)
	require.NoError(t, err)
	noPermJWT, err := auth.GenerateJWTToken(ctx, noPerm)
	require.NoError(t, err)

	nodes := map[string]string{
		"lag":              "http://127.0.0.1:18091",
		"gcp":              "http://127.0.0.1:18092",
		"gcp-kiki-usw1":    "http://127.0.0.1:18093",
		"gcp-nailong-usw1": "http://127.0.0.1:18094",
		"tencent-new":      "http://127.0.0.1:18095",
		"tencent-old":      "http://127.0.0.1:18096",
	}
	makeNode := func(id string) *Server {
		node := cluster.Config{Enabled: true, NodeID: id, ControlNode: "lag", Nodes: nodes, Channels: map[int]cluster.ChannelBinding{}}.WithRuntime()
		hc := httpclient.NewHttpClient()
		svc := biz.NewChannelService(biz.ChannelServiceParams{Node: node, Ent: client, CacheConfig: cache, SystemService: system, HttpClient: hc})
		t.Cleanup(svc.Stop)
		models := biz.NewModelService(biz.ModelServiceParams{Ent: client, SystemService: system, ChannelService: svc})
		gateway := New(Config{RequestTimeout: 20 * time.Second, LLMRequestTimeout: 20 * time.Second})
		ip, err := middleware.NewIPAccessControlConfig(false, nil, "")
		require.NoError(t, err)
		SetupRoutes(gateway, Handlers{
			OpenAI:  &api.OpenAIHandlers{ResponseWebSocketHandler: &api.ResponsesWebSocketHandler{}},
			Cluster: api.NewClusterHandlers(models, svc, nil, nil, nil),
		}, client, Services{AuthService: auth, SystemService: system, Node: node}, ip)
		return gateway
	}

	control := makeNode("lag")
	dataNode := makeNode("gcp")

	request := func(gateway *Server, method, path, token string, body any) *httptest.ResponseRecorder {
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
		gateway.Engine.ServeHTTP(rec, req)
		return rec
	}

	// 1. Auth and data-node guard checks
	require.Equal(t, http.StatusUnauthorized, request(control, http.MethodGet, "/admin/cluster/infrastructure/state", "", nil).Code)
	require.Equal(t, http.StatusForbidden, request(control, http.MethodGet, "/admin/cluster/infrastructure/state", noPermJWT, nil).Code)
	require.Equal(t, http.StatusNotFound, request(dataNode, http.MethodGet, "/admin/cluster/infrastructure/state", ownerJWT, nil).Code)

	// 2. Unconfigured AXONHUB_INFRA_CONTROL_URL returns clean 503 without spawning processes or leaking internal paths
	t.Setenv("AXONHUB_INFRA_CONTROL_URL", "")
	unconfRes := request(control, http.MethodGet, "/admin/cluster/infrastructure/state", readerJWT, nil)
	require.Equal(t, http.StatusServiceUnavailable, unconfRes.Code)
	require.Contains(t, unconfRes.Body.String(), `"infrastructure_control_unconfigured"`)

	// 3. Upstream redirect is rejected with 502
	redirectSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example.com/api/v1/state", http.StatusFound)
	}))
	t.Cleanup(redirectSrv.Close)
	t.Setenv("AXONHUB_INFRA_CONTROL_URL", redirectSrv.URL)
	redirRes := request(control, http.MethodGet, "/admin/cluster/infrastructure/state", readerJWT, nil)
	require.Equal(t, http.StatusBadGateway, redirRes.Code)
	require.Contains(t, redirRes.Body.String(), `"infrastructure_control_redirect_rejected"`)

	// 4. Point to the explicit managed loopback control bridge
	t.Setenv("AXONHUB_INFRA_CONTROL_URL", bridgeURL)
	stateRes := request(control, http.MethodGet, "/admin/cluster/infrastructure/state?refresh=1", readerJWT, nil)
	require.Equal(t, http.StatusOK, stateRes.Code, stateRes.Body.String())
	require.Contains(t, stateRes.Body.String(), `"dmit-154-31-115-2"`)
	require.Contains(t, stateRes.Body.String(), `"6af022d9094b6a5212e75dea"`)
	require.Contains(t, stateRes.Body.String(), `"400c7c56-4ae8-4edb-912e-a4db24ee9455"`)

	var stateBody struct {
		Revision int `json:"revision"`
	}
	require.NoError(t, json.Unmarshal(stateRes.Body.Bytes(), &stateBody))

	// 5. Reader cannot mutate rules
	rulePayload := map[string]any{
		"revision": stateBody.Revision,
		"rule": map[string]any{
			"id":         "r-native-test",
			"name":       "Native test rule",
			"enabled":    true,
			"priority":   40,
			"match":      map[string]any{"method": "POST", "pathPrefix": "/v1/chat/completions", "model": "unowned-test-model"},
			"targetNode": "lag",
		},
	}
	require.Equal(t, http.StatusForbidden, request(control, http.MethodPut, "/admin/cluster/infrastructure/rules/r-native-test", readerJWT, rulePayload).Code)

	// 6. Owner can save rule and explain
	putRes := request(control, http.MethodPut, "/admin/cluster/infrastructure/rules/r-native-test", ownerJWT, rulePayload)
	require.Equal(t, http.StatusOK, putRes.Code, putRes.Body.String())

	expRes := request(control, http.MethodPost, "/admin/cluster/infrastructure/explain", readerJWT, map[string]any{
		"method": "POST",
		"path":   "/v1/chat/completions",
		"model":  "unowned-test-model",
		"source": "draft",
	})
	require.Equal(t, http.StatusOK, expRes.Code, expRes.Body.String())
	require.Contains(t, expRes.Body.String(), `"ruleId":"r-native-test"`)

	// 7. Reverse-proxy Basic Auth + Host rewrite + X-Axon-Authorization forwarding (simulating https://console.zjuam.art -> 127.0.0.1:18751)
	parsedBridge, err := url.Parse(bridgeURL)
	require.NoError(t, err)
	var observedCallerBearer string
	caddySim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "admin" || pass != "synthetic-console-pass-123" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthorized"}}`))
			return
		}
		observedCallerBearer = r.Header.Get("X-Axon-Authorization")
		outReq, err := http.NewRequestWithContext(r.Context(), r.Method, bridgeURL+r.URL.RequestURI(), r.Body)
		require.NoError(t, err)
		outReq.Host = parsedBridge.Host
		outReq.Header = r.Header.Clone()
		outReq.Header.Del("Authorization")
		resp, err := http.DefaultClient.Do(outReq)
		require.NoError(t, err)
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(caddySim.Close)

	t.Setenv("AXONHUB_INFRA_CONTROL_URL", caddySim.URL)
	// Without AXONHUB_INFRA_CONTROL_AUTH_FILE, Caddy returns 401
	noBasicRes := request(control, http.MethodGet, "/admin/cluster/infrastructure/state", readerJWT, nil)
	require.Equal(t, http.StatusUnauthorized, noBasicRes.Code)

	// Unreadable AXONHUB_INFRA_CONTROL_AUTH_FILE returns 503 infrastructure_control_auth_unconfigured
	t.Setenv("AXONHUB_INFRA_CONTROL_AUTH_FILE", filepath.Join(t.TempDir(), "missing-auth.password"))
	missingAuthRes := request(control, http.MethodGet, "/admin/cluster/infrastructure/state", readerJWT, nil)
	require.Equal(t, http.StatusServiceUnavailable, missingAuthRes.Code)
	require.Contains(t, missingAuthRes.Body.String(), `"infrastructure_control_auth_unconfigured"`)

	// Valid password-only file (matching /opt/task51-tencent-sites/console/private/admin.password format) succeeds
	passFile := filepath.Join(t.TempDir(), "admin.password")
	require.NoError(t, os.WriteFile(passFile, []byte("synthetic-console-pass-123\n"), 0o600))
	t.Setenv("AXONHUB_INFRA_CONTROL_AUTH_FILE", passFile)
	authedRes := request(control, http.MethodGet, "/admin/cluster/infrastructure/state", readerJWT, nil)
	require.Equal(t, http.StatusOK, authedRes.Code, authedRes.Body.String())
	require.Contains(t, authedRes.Body.String(), `"r-native-test"`)
	require.Equal(t, "Bearer "+readerJWT, observedCallerBearer)
}
