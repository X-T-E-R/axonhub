package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

func TestInfrastructureHTTP(t *testing.T) {
	t.Setenv("AXONHUB_INFRA_DATA_DIR", filepath.Join(t.TempDir(), "infra-state"))
	snapPath := filepath.Clean("../../outputs/task51-komodo-reconcile-20261108/core-final.json")
	t.Setenv("TASK51_KOMODO_SNAPSHOT_FILE", snapPath)

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

	// Auth and data-node guard checks
	require.Equal(t, http.StatusUnauthorized, request(control, http.MethodGet, "/admin/cluster/infrastructure/state", "", nil).Code)
	require.Equal(t, http.StatusForbidden, request(control, http.MethodGet, "/admin/cluster/infrastructure/state", noPermJWT, nil).Code)
	require.Equal(t, http.StatusNotFound, request(dataNode, http.MethodGet, "/admin/cluster/infrastructure/state", ownerJWT, nil).Code)

	// Reader can read state, topology, explain
	stateRes := request(control, http.MethodGet, "/admin/cluster/infrastructure/state?refresh=1", readerJWT, nil)
	require.Equal(t, http.StatusOK, stateRes.Code, stateRes.Body.String())
	require.Contains(t, stateRes.Body.String(), `"dmit-154-31-115-2"`)
	require.Contains(t, stateRes.Body.String(), `"6af022d9094b6a5212e75dea"`)
	require.Contains(t, stateRes.Body.String(), `"400c7c56-4ae8-4edb-912e-a4db24ee9455"`)

	var stateBody struct {
		Revision int `json:"revision"`
	}
	require.NoError(t, json.Unmarshal(stateRes.Body.Bytes(), &stateBody))

	// Reader cannot mutate rules
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

	// Owner can save rule, explain, preview, create release, activate and rollback
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
}
