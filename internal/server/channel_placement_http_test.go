package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/metrics"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/api"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/internal/server/gql"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/llm/httpclient"
)

// The browser fixture owns UI behavior; this drives the actual authenticated
// Axon routes and target RPC with a disposable database and local listeners.
func TestChannelPlacementHTTP(t *testing.T) {
	provider := sdkmetric.NewMeterProvider()
	require.NoError(t, metrics.SetupMetrics(provider, "placement-test"))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	client := enttest.NewEntClient(t, "sqlite3", "file:placement-http-"+uuid.NewString()+"?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })
	ctx := authz.WithTestBypass(context.Background())
	owner := client.User.Create().SetEmail("owner@example.test").SetPassword("synthetic").SetIsOwner(true).SetStatus(user.StatusActivated).SaveX(ctx)
	reader := client.User.Create().SetEmail("reader@example.test").SetPassword("synthetic").SetScopes([]string{string(scopes.ScopeReadChannels)}).SetStatus(user.StatusActivated).SaveX(ctx)
	makeChannel := func(name string, status channel.Status) *ent.Channel {
		return client.Channel.Create().SetName(name).SetType(channel.TypeOpenai).SetStatus(status).
			SetCredentials(objects.ChannelCredentials{APIKey: "synthetic-shared-provider-key"}).SetSupportedModels([]string{"fixture"}).SetDefaultTestModel("fixture").SaveX(ctx)
	}
	disabled := makeChannel("fixture-disabled", channel.StatusDisabled)
	archived := makeChannel("fixture-archived", channel.StatusArchived)
	cache := xcache.Config{Mode: xcache.ModeMemory}
	system := biz.NewSystemService(biz.SystemServiceParams{Ent: client, CacheConfig: cache})
	require.NoError(t, system.SetSecretKey(ctx, "synthetic-placement-jwt-key"))
	users := biz.NewUserService(biz.UserServiceParams{Ent: client, CacheConfig: cache})
	auth := biz.NewAuthService(biz.AuthServiceParams{Ent: client, SystemService: system, UserService: users})
	ownerJWT, err := auth.GenerateJWTToken(ctx, owner)
	require.NoError(t, err)
	readerJWT, err := auth.GenerateJWTToken(ctx, reader)
	require.NoError(t, err)

	target := httptest.NewUnstartedServer(nil)
	defer target.Close()
	nodes := map[string]string{"control": "http://127.0.0.1:1", "source": "http://127.0.0.1:2", "target": "http://" + target.Listener.Addr().String()}
	bindings := map[int]cluster.ChannelBinding{disabled.ID: {Owner: "control", Identity: "shared"}, archived.ID: {Owner: "control", Identity: "shared"}}
	makeNode := func(id string) (*Server, *biz.ChannelService) {
		node := cluster.Config{Enabled: true, NodeID: id, ControlNode: "control", Nodes: nodes, Channels: bindings}.WithRuntime()
		hc := httpclient.NewHttpClient()
		svc := biz.NewChannelService(biz.ChannelServiceParams{Node: node, Ent: client, CacheConfig: cache, SystemService: system, HttpClient: hc})
		t.Cleanup(svc.Stop)
		models := biz.NewModelService(biz.ModelServiceParams{Ent: client, SystemService: system, ChannelService: svc})
		gateway := New(Config{RequestTimeout: 5 * time.Second, LLMRequestTimeout: 5 * time.Second})
		ip, err := middleware.NewIPAccessControlConfig(false, nil, "")
		require.NoError(t, err)
		SetupRoutes(gateway, Handlers{
			OpenAI:  &api.OpenAIHandlers{ResponseWebSocketHandler: &api.ResponsesWebSocketHandler{}},
			Cluster: api.NewClusterHandlers(models, svc, nil, nil, nil),
			Graphql: gql.NewGraphqlHandlers(gql.Dependencies{Ent: client, SystemService: system, ChannelService: svc, ModelService: models, HttpClient: hc}),
		}, client, Services{AuthService: auth, SystemService: system, Node: node}, ip)
		return gateway, svc
	}
	targetGateway, targetSvc := makeNode("target")
	target.Config.Handler = targetGateway.Engine
	target.Start()
	control, controlSvc := makeNode("control")
	request := func(gateway *Server, method, path, token string, body any) *httptest.ResponseRecorder {
		t.Helper()
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		gateway.Engine.ServeHTTP(rec, req)
		return rec
	}
	path := fmt.Sprintf("/admin/cluster/placement/%d", disabled.ID)
	previewPath := path + "/preview?target=target"
	require.Equal(t, http.StatusUnauthorized, request(control, http.MethodGet, path, "", nil).Code)
	require.Equal(t, http.StatusForbidden, request(control, http.MethodGet, previewPath, readerJWT, nil).Code)
	require.Equal(t, http.StatusForbidden, request(control, http.MethodPost, "/admin/cluster/placement", readerJWT, nil).Code)
	require.Equal(t, http.StatusNotFound, request(targetGateway, http.MethodGet, path, ownerJWT, nil).Code)
	preview := request(control, http.MethodGet, previewPath, ownerJWT, nil)
	require.Equal(t, http.StatusOK, preview.Code, preview.Body.String())
	require.NotContains(t, preview.Body.String(), "identities")
	require.NotContains(t, preview.Body.String(), "synthetic-shared-provider-key")
	require.Contains(t, preview.Body.String(), "fixture-archived")

	move := biz.PlacementSaveInput{ChannelID: disabled.ID, Target: "target", RequestID: "fixture-save", ChannelIDs: []int{disabled.ID, archived.ID}}
	saved := request(control, http.MethodPost, "/admin/cluster/placement", ownerJWT, move)
	require.Equal(t, http.StatusOK, saved.Code, saved.Body.String())
	var observation biz.PlacementObservation
	require.NoError(t, json.Unmarshal(saved.Body.Bytes(), &observation))
	require.Equal(t, "target", observation.Owner)
	require.NotNil(t, observation.Target)
	require.True(t, observation.Target.Ready)
	require.Nil(t, observation.WorkerAck)
	require.True(t, targetSvc.OwnsChannel(archived.ID))
	manifest := request(control, http.MethodGet, "/admin/cluster/manifest", ownerJWT, nil)
	require.Equal(t, http.StatusOK, manifest.Code, manifest.Body.String())
	require.Contains(t, manifest.Body.String(), `"placement_version":1`)
	require.Equal(t, "no-store", manifest.Header().Get("Cache-Control"))
	ack := biz.PlacementWorkerAck{Version: 1, WorkerRevision: "fixture-worker"}
	require.Equal(t, http.StatusForbidden, request(control, http.MethodPost, "/admin/cluster/placement/worker-ack", readerJWT, ack).Code)
	acked := request(control, http.MethodPost, "/admin/cluster/placement/worker-ack", ownerJWT, ack)
	require.Equal(t, http.StatusOK, acked.Code, acked.Body.String())
	observed := request(control, http.MethodGet, path, readerJWT, nil)
	require.Equal(t, http.StatusOK, observed.Code, observed.Body.String())
	require.Contains(t, observed.Body.String(), `"worker_revision":"fixture-worker"`)

	// Roll back via a new placement request, not by restoring database contents.
	move.Target, move.RequestID, move.ExpectedVersion = "control", "fixture-return", 1
	reverted := request(control, http.MethodPost, "/admin/cluster/placement", ownerJWT, move)
	require.Equal(t, http.StatusOK, reverted.Code, reverted.Body.String())
	require.Contains(t, reverted.Body.String(), `"owner":"control"`)
	require.Contains(t, reverted.Body.String(), `"placement_version":2`)
	require.Equal(t, channel.StatusArchived, client.Channel.GetX(ctx, archived.ID).Status)
	require.Equal(t, channel.StatusDisabled, client.Channel.GetX(ctx, disabled.ID).Status)
	require.Equal(t, http.StatusConflict, request(control, http.MethodPost, "/admin/cluster/placement/worker-ack", ownerJWT, ack).Code)

	graph := func(query string, vars map[string]any) string {
		t.Helper()
		response := request(control, http.MethodPost, "/admin/graphql", ownerJWT, map[string]any{"query": query, "variables": vars})
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		return response.Body.String()
	}
	created := graph(`mutation New($input: CreateChannelInput!) { createChannel(input: $input) { id status executionMachine } }`, map[string]any{
		"input": map[string]any{"type": "openai", "name": "first-assignment", "baseURL": "http://127.0.0.1:1", "credentials": map[string]any{"apiKey": "synthetic-first-key"}, "supportedModels": []string{"first-model"}, "defaultTestModel": "first-model"},
	})
	require.Contains(t, created, `"status":"disabled","executionMachine":null`)
	first := client.Channel.Query().Where(channel.NameEQ("first-assignment")).OnlyX(ctx)
	id := fmt.Sprintf("gid://axonhub/Channel/%d", first.ID)
	enable := func() string {
		return graph(`mutation Enable($id: ID!) { updateChannelStatus(id: $id, status: enabled) { status executionMachine } }`, map[string]any{"id": id})
	}
	require.Contains(t, enable(), "before enabling it")
	for _, field := range []string{"bulkEnableChannels", "bulkRecoverChannels"} {
		bulk := graph(fmt.Sprintf(`mutation EnableMany($ids: [ID!]!) { %s(ids: $ids) }`, field), map[string]any{"ids": []string{id}})
		require.Contains(t, bulk, "before enabling it")
	}
	require.Equal(t, channel.StatusDisabled, client.Channel.GetX(ctx, first.ID).Status)

	newPreview := request(control, http.MethodGet, fmt.Sprintf("/admin/cluster/placement/%d/preview?target=target", first.ID), ownerJWT, nil)
	require.Equal(t, http.StatusOK, newPreview.Code, newPreview.Body.String())
	require.Contains(t, newPreview.Body.String(), `"from":""`)
	require.NotContains(t, newPreview.Body.String(), "identities")
	firstSave := request(control, http.MethodPost, "/admin/cluster/placement", ownerJWT, biz.PlacementSaveInput{
		ChannelID: first.ID, Target: "target", ExpectedVersion: 2, RequestID: "fixture-first", ChannelIDs: []int{first.ID},
	})
	require.Equal(t, http.StatusOK, firstSave.Code, firstSave.Body.String())
	require.Contains(t, firstSave.Body.String(), `"placement_version":3`)
	require.Contains(t, firstSave.Body.String(), `"ready":true`)
	require.Equal(t, channel.StatusDisabled, client.Channel.GetX(ctx, first.ID).Status)
	firstBinding := targetSvc.NodeConfig().Binding(first.ID)
	require.Equal(t, "target", firstBinding.Owner)
	require.NotEmpty(t, firstBinding.Identity)

	// A legacy enabled unassigned row does not hide valid providers or models.
	legacy := client.Channel.Create().SetName("legacy-unassigned").SetType(channel.TypeOpenai).SetStatus(channel.StatusEnabled).
		SetCredentials(objects.ChannelCredentials{APIKey: "synthetic-legacy-key"}).SetSupportedModels([]string{"legacy-model"}).SetDefaultTestModel("legacy-model").SaveX(ctx)
	require.Contains(t, enable(), `"status":"enabled","executionMachine":"target"`)
	require.NoError(t, controlSvc.ReloadEnabledChannelsCache(ctx))
	_, restarted := makeNode("target")
	require.Equal(t, firstBinding, restarted.NodeConfig().Binding(first.ID))
	require.NotNil(t, restarted.GetEnabledChannel(first.ID))
	require.Empty(t, restarted.GetEnabledChannel(first.ID).RemoteOwner)
	require.Nil(t, restarted.GetEnabledChannel(legacy.ID))
	project := client.Project.Create().SetName("placement manifest").SaveX(ctx)
	client.APIKey.Create().SetName("placement manifest").SetKey("ah-placement-synthetic").SetProjectID(project.ID).
		SetType(apikey.TypeUser).SetScopes([]string{string(scopes.ScopeReadChannels)}).SaveX(ctx)
	manifest = request(control, http.MethodGet, "/admin/cluster/manifest", ownerJWT, nil)
	require.Equal(t, http.StatusOK, manifest.Code, manifest.Body.String())
	var effective biz.ExecutionManifest
	require.NoError(t, json.Unmarshal(manifest.Body.Bytes(), &effective))
	require.EqualValues(t, 3, effective.PlacementVersion)
	require.Contains(t, effective.Models, biz.ModelExecutionRoute{Model: "first-model", Owner: "target", Owners: []string{"target"}, Status: "ready"})
	require.NotContains(t, manifest.Body.String(), "legacy-model")
	require.NotContains(t, manifest.Body.String(), firstBinding.Identity)
	filtered := graph(`{ queryChannels(input: { first: 5, executionMachine: "target", model: "first-model" }) { totalCount edges { node { name status executionMachine } } } }`, nil)
	require.Contains(t, filtered, `"totalCount":1`)
	require.Contains(t, filtered, `"name":"first-assignment","status":"enabled","executionMachine":"target"`)

	imported := graph(`mutation Import($input: BulkImportChannelsInput!) { bulkImportChannels(input: $input) { success channels { status executionMachine } } }`, map[string]any{
		"input": map[string]any{"channels": []map[string]any{{"type": "openai", "name": "imported-shared", "baseURL": "http://127.0.0.1:1", "apiKey": "synthetic-shared-provider-key", "supportedModels": []string{"fixture"}, "defaultTestModel": "fixture"}}},
	})
	require.Contains(t, imported, `"success":true,"channels":[{"status":"disabled","executionMachine":null}]`)
	joining := client.Channel.Query().Where(channel.NameEQ("imported-shared")).OnlyX(ctx)
	joinPath := fmt.Sprintf("/admin/cluster/placement/%d/preview?target=", joining.ID)
	wrongMachine := request(control, http.MethodGet, joinPath+"target", ownerJWT, nil)
	require.Equal(t, http.StatusBadRequest, wrongMachine.Code)
	require.Contains(t, wrongMachine.Body.String(), "before moving the group")
	joinPreview := request(control, http.MethodGet, joinPath+"control", ownerJWT, nil)
	require.Equal(t, http.StatusOK, joinPreview.Code, joinPreview.Body.String())
	require.Contains(t, joinPreview.Body.String(), "fixture-archived")
	join := biz.PlacementSaveInput{ChannelID: joining.ID, Target: "control", ExpectedVersion: 3, RequestID: "fixture-join", ChannelIDs: []int{joining.ID}}
	require.Equal(t, http.StatusConflict, request(control, http.MethodPost, "/admin/cluster/placement", ownerJWT, join).Code)
	join.ChannelIDs = []int{disabled.ID, archived.ID, joining.ID}
	joined := request(control, http.MethodPost, "/admin/cluster/placement", ownerJWT, join)
	require.Equal(t, http.StatusOK, joined.Code, joined.Body.String())
	require.Equal(t, controlSvc.NodeConfig().Binding(disabled.ID), controlSvc.NodeConfig().Binding(joining.ID))
	require.Equal(t, channel.StatusDisabled, client.Channel.GetX(ctx, joining.ID).Status)

	// Once registered, an online-only channel uses the ordinary move operation.
	moveOnline := request(control, http.MethodPost, "/admin/cluster/placement", ownerJWT, biz.PlacementSaveInput{
		ChannelID: first.ID, Target: "control", ExpectedVersion: 4, RequestID: "fixture-online-move", ChannelIDs: []int{first.ID},
	})
	require.Equal(t, http.StatusOK, moveOnline.Code, moveOnline.Body.String())
	require.Contains(t, moveOnline.Body.String(), `"placement_version":5`)
	require.Equal(t, "control", controlSvc.NodeConfig().Owner(first.ID))
	require.Equal(t, firstBinding.Identity, controlSvc.NodeConfig().Binding(first.ID).Identity)
	require.Equal(t, channel.StatusEnabled, client.Channel.GetX(ctx, first.ID).Status)
}
