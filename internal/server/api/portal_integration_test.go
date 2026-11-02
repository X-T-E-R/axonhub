package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/ent/userproject"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/gql"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/internal/server/portalgql"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
	"github.com/stretchr/testify/require"
)

// Uses the production JWT/API-key middleware, restricted and admin schemas,
// pipeline, HTTP handler and WS generation handler. Every upstream is loopback.
func TestPortalJWTGraphQLPipelineIsolationAndRevocation(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:portal-"+uuid.NewString()+"?mode=memory&_fk=1")
	defer client.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
	cache := xcache.Config{Mode: xcache.ModeMemory}
	system := biz.NewSystemService(biz.SystemServiceParams{Ent: client, CacheConfig: cache})
	client.System.Create().SetKey(biz.SystemKeySecretKey).SetValue("synthetic-portal-jwt-key-not-a-secret").SaveX(ctx)
	projects := biz.NewProjectService(biz.ProjectServiceParams{Ent: client, CacheConfig: cache})
	users := biz.NewUserService(biz.UserServiceParams{Ent: client, CacheConfig: cache})
	keys := biz.NewAPIKeyService(biz.APIKeyServiceParams{Ent: client, CacheConfig: cache, ProjectService: projects, KeyPrefix: "ah"})
	defer keys.Stop()
	auth := biz.NewAuthService(biz.AuthServiceParams{Ent: client, SystemService: system, APIKeyService: keys, UserService: users})
	makeUser := func(email string, mode user.AccessMode, owner bool) *ent.User {
		return client.User.Create().SetEmail(email).SetPassword("test").SetAccessMode(mode).SetIsOwner(owner).SaveX(ctx)
	}
	a, b, none, admin := makeUser("a@example.test", user.AccessModePortal, false), makeUser("b@example.test", user.AccessModePortal, false), makeUser("none@example.test", user.AccessModePortal, false), makeUser("admin@example.test", user.AccessModeLegacy, true)
	g := client.Project.Create().SetName("Group A").SaveX(ctx)
	g2 := client.Project.Create().SetName("Group B").SaveX(ctx)
	for _, pair := range [][2]int{{a.ID, g.ID}, {a.ID, g2.ID}, {b.ID, g.ID}} {
		client.UserProject.Create().SetUserID(pair[0]).SetProjectID(pair[1]).SaveX(ctx)
	}
	client.DataStorage.Create().SetName("primary").SetDescription("synthetic primary").SetType(datastorage.TypeDatabase).SetPrimary(true).SetSettings(&objects.DataStorageSettings{}).SaveX(ctx)
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Contains(t, string(raw), `"model":"provider-model"`)
		var body map[string]any
		require.NoError(t, json.Unmarshal(raw, &body))
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_portal\",\"object\":\"response\",\"model\":\"provider-model\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"resp_http","object":"response","model":"provider-model","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`)
	}))
	defer upstream.Close()
	ch := client.Channel.Create().SetName("internal-route").SetType(channel.TypeOpenaiResponses).SetBaseURL(upstream.URL).SetCredentials(objects.ChannelCredentials{APIKey: "synthetic-upstream"}).SetSupportedModels([]string{"provider-model", "other"}).SetDefaultTestModel("m").SetSettings(&objects.ChannelSettings{ModelMappings: []objects.ModelMapping{{From: "m", To: "provider-model"}}}).SaveX(ctx)
	outbound, err := responses.NewOutboundTransformer(upstream.URL, "synthetic-upstream")
	require.NoError(t, err)
	channels := biz.NewChannelServiceForTest(client)
	defer channels.Stop()
	channels.SetEnabledChannelsForTest([]*biz.Channel{{Channel: ch, Outbound: outbound, Outbounds: map[string]transformer.Outbound{}}})
	profile := func(ids []string) *objects.ProjectProfiles {
		return &objects.ProjectProfiles{ActiveProfile: "active", Profiles: []objects.ProjectProfile{{Name: "active", ModelIDs: &ids, ChannelIDs: []int{ch.ID}}}}
	}
	client.Project.UpdateOne(g).SetProfiles(profile([]string{"m"})).SaveX(ctx)
	client.Project.UpdateOne(g2).SetProfiles(profile([]string{"other"})).SaveX(ctx)
	models := biz.NewModelService(biz.ModelServiceParams{Ent: client, SystemService: system, ChannelService: channels})
	quota := biz.NewQuotaService(client, system)
	portal := biz.NewPortalService(biz.PortalServiceParams{Ent: client, Keys: keys, Models: models, Quota: quota, System: system, Auth: auth, Users: users})
	usage := biz.NewUsageLogService(client, system, channels)
	storage := &biz.DataStorageService{AbstractService: &biz.AbstractService{}, SystemService: system, Cache: xcache.NewFromConfig[ent.DataStorage](cache)}
	requests := biz.NewRequestService(client, system, usage, storage, biz.NewLiveStreamRegistry())
	selector := orchestrator.NewDefaultSelector(channels, models, system)
	processor := orchestrator.NewChatCompletionOrchestrator(channels, selector, requests, httpclient.NewHttpClient(), responses.NewInboundTransformer(), system, usage, nil, quota, nil, nil, orchestrator.NewChannelLimiterManager(), nil)
	processor.PromptProtecter = nil
	processor.PromptProvider = wsPromptProvider{}
	h := &ChatCompletionHandlers{ChatCompletionOrchestrator: processor}
	pgql := portalgql.NewGraphqlHandlers(portal, users)
	agql := gql.NewGraphqlHandlers(gql.Dependencies{Ent: client, AuthService: auth, APIKeyService: keys, UserService: users, SystemService: system, ProjectService: projects, ModelService: models, ChannelService: channels})
	router := gin.New()
	router.Use(middleware.WithEntClient(client))
	router.POST("/portal/graphql", middleware.WithJWTAuth(auth), func(c *gin.Context) { pgql.Graphql.ServeHTTP(c.Writer, c.Request) })
	adminGroup := router.Group("/admin", middleware.WithJWTAuth(auth), middleware.WithLegacyUser(), middleware.WithProjectID())
	adminGroup.POST("/graphql", func(c *gin.Context) { agql.Graphql.ServeHTTP(c.Writer, c.Request) })
	adminGroup.GET("/playground", func(c *gin.Context) { agql.Playground.ServeHTTP(c.Writer, c.Request) })
	for _, p := range []string{"/requests/:request_id/content", "/diagnostics/v1/pull", "/management/tokens"} {
		adminGroup.POST(p, func(c *gin.Context) { c.Status(204) })
	}
	router.POST("/v1/responses", middleware.WithAPIKeyConfig(auth, nil), h.ChatCompletion)
	router.GET("/v1/responses", middleware.WithAPIKeyConfig(auth, nil), newResponsesWebSocketHandler(h, auth).Serve)
	router.POST("/v1beta/models/:model", middleware.WithGeminiKeyAuth(auth), func(c *gin.Context) { c.Status(204) })
	videos := biz.NewVideoService(channels, requests)
	router.POST("/v1/videos/:id", middleware.WithAPIKeyConfig(auth, nil), func(c *gin.Context) {
		err := videos.DeleteTaskByExternalID(c.Request.Context(), c.Param("id"))
		if err != nil {
			c.Status(http.StatusForbidden)
			return
		}
		c.Status(204)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	token := func(u *ent.User) string { v, e := auth.GenerateJWTToken(ctx, u); require.NoError(t, e); return v }
	ta, tb, tn, tadmin := token(a), token(b), token(none), token(admin)
	post := func(path, credential string, body any) (int, []byte) {
		raw, e := json.Marshal(body)
		require.NoError(t, e)
		r, e := http.NewRequest("POST", server.URL+path, bytes.NewReader(raw))
		require.NoError(t, e)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+credential)
		res, e := http.DefaultClient.Do(r)
		require.NoError(t, e)
		defer res.Body.Close()
		out, e := io.ReadAll(res.Body)
		require.NoError(t, e)
		return res.StatusCode, out
	}
	graph := func(tok, q string, vars map[string]any) map[string]json.RawMessage {
		status, out := post("/portal/graphql", tok, map[string]any{"query": q, "variables": vars})
		require.Contains(t, []int{http.StatusOK, http.StatusUnprocessableEntity}, status, string(out))
		var b map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(out, &b))
		return b
	}
	okGraph := func(tok, q string, vars map[string]any) json.RawMessage {
		body := graph(tok, q, vars)
		require.NotContains(t, body, "errors", string(body["errors"]))
		return body["data"]
	}
	badGraph := func(tok, q string, vars map[string]any) {
		body := graph(tok, q, vars)
		require.Contains(t, body, "errors", string(body["data"]))
	}
	identity := okGraph(ta, `{ me { id accessMode } groups { id callable } currencyCode }`, nil)
	require.Contains(t, string(identity), `"portal"`)
	require.Contains(t, string(okGraph(tn, `{ groups { id } }`, nil)), `"groups":[]`)
	badGraph(ta, `{ node(id:"User:1") { id } }`, nil)
	badGraph(ta, `{ keys(first:101) { totalCount } }`, nil)
	badGraph(ta, `{ keys(after:-1) { totalCount } }`, nil)
	// Safe account self-service is bound to the JWT subject, including OIDC IDs.
	okGraph(ta, `mutation { updateProfile(firstName:"Alice",preferLanguage:"en") { id firstName } }`, nil)
	require.Equal(t, "Alice", client.User.GetX(ctx, a.ID).FirstName)
	require.Empty(t, client.User.GetX(ctx, b.ID).FirstName)
	aIdentity := client.OIDCIdentity.Create().SetUserID(a.ID).SetIssuer("https://idp.example.test").SetSubject("a").SaveX(ctx)
	bIdentity := client.OIDCIdentity.Create().SetUserID(b.ID).SetIssuer("https://idp.example.test").SetSubject("b").SaveX(ctx)
	unlink := `mutation($id:Int!) { unlinkOIDCIdentity(id:$id) }`
	badGraph(ta, unlink, map[string]any{"id": bIdentity.ID})
	require.Equal(t, b.ID, client.OIDCIdentity.GetX(ctx, bIdentity.ID).UserID)
	okGraph(ta, unlink, map[string]any{"id": aIdentity.ID})
	_, err = client.OIDCIdentity.Get(ctx, aIdentity.ID)
	require.True(t, ent.IsNotFound(err))
	client.User.UpdateOne(b).SetPassword(biz.OIDC_ONLY_PLACEHOLDER).SaveX(ctx)
	badGraph(tb, unlink, map[string]any{"id": bIdentity.ID})
	// An OIDC-only subject may set its first local password, then safely unlink.
	okGraph(tb, `mutation { updatePassword(newPassword:"synthetic-local-password") }`, nil)
	okGraph(tb, unlink, map[string]any{"id": bIdentity.ID})
	create := func(tok string, pid int, name string) int {
		data := okGraph(tok, `mutation($p:Int!,$input:PortalKeyInput!){createKey(projectID:$p,input:$input){id name modelIDs}}`, map[string]any{"p": pid, "input": map[string]any{"name": name, "modelIDs": []string{}, "allowedIPs": []string{}}})
		var d struct{ CreateKey struct{ ID int } }
		require.NoError(t, json.Unmarshal(data, &d))
		return d.CreateKey.ID
	}
	ka, kb := create(ta, g.ID, "A key"), create(tb, g.ID, "B key")
	kc := create(ta, g2.ID, "A second group")
	badGraph(ta, `query($p:Int!,$k:Int!){models(projectID:$p,keyID:$k){id}}`, map[string]any{"p": g.ID, "k": kc})
	// Execute the actual query strings used by all four portal pages.
	frontend, err := os.ReadFile("../../../frontend/src/features/portal/data.ts")
	require.NoError(t, err)
	callerQuery := func(name string) string {
		re := regexp.MustCompile("(?s)export const " + name + " = `([^`]+)`")
		match := re.FindSubmatch(frontend)
		require.Len(t, match, 2, name)
		query := string(match[1])
		for _, field := range []string{"KEY_FIELDS", "MODEL_FIELDS", "DOCUMENT_FIELDS"} {
			m := regexp.MustCompile("export const " + field + " = `([^`]+)`").FindSubmatch(frontend)
			require.Len(t, m, 2)
			query = strings.ReplaceAll(query, "${"+field+"}", string(m[1]))
		}
		return query
	}
	for _, name := range []string{"IDENTITY_QUERY", "GROUPS_QUERY", "KEYS_QUERY", "MODELS_QUERY", "CONTENT_QUERY", "DASHBOARD_QUERY"} {
		vars := map[string]any{"projectID": g.ID, "first": 50, "after": nil, "period": "all"}
		okGraph(ta, callerQuery(name), vars)
	}
	client.Request.Create().SetAPIKeyID(kb).SetProjectID(g.ID).SetModelID("m").SetChannelID(ch.ID).SetExternalID("b-video").SetRequestBody(objects.JSONRawMessage(`{}`)).SetStatus(request.StatusCompleted).SaveX(ctx)

	badGraph(tn, `mutation($p:Int!){createKey(projectID:$p,input:{name:"no"}){id}}`, map[string]any{"p": g.ID})
	badGraph(ta, `mutation($id:Int!){revealKey(id:$id)}`, map[string]any{"id": kb})
	badGraph(ta, `mutation($id:Int!){setKeyStatus(id:$id,status:"disabled"){id}}`, map[string]any{"id": kb})
	badGraph(ta, `query($p:Int!,$k:Int!){models(projectID:$p,keyID:$k){id}}`, map[string]any{"p": g.ID, "k": kb})
	badGraph(ta, `mutation($k:Int!){updateKey(id:$k,input:{name:"A key",modelIDs:["other"]}){id}}`, map[string]any{"k": ka})
	listed := okGraph(ta, `query($p:Int!){models(projectID:$p){id apiFormats} keys(projectID:$p){items{id name maskedKey} totalCount}}`, map[string]any{"p": g.ID})
	require.Contains(t, string(listed), `"id":"m"`)
	require.NotContains(t, string(listed), `"id":"other"`)
	require.NotContains(t, string(listed), "B key")
	raw := okGraph(ta, `mutation($id:Int!){revealKey(id:$id)}`, map[string]any{"id": ka})
	var secretData struct{ RevealKey string }
	require.NoError(t, json.Unmarshal(raw, &secretData))
	secret := secretData.RevealKey
	for _, path := range []string{"/admin/graphql", "/admin/requests/1/content", "/admin/diagnostics/v1/pull", "/admin/management/tokens"} {
		status, _ := post(path, ta, map[string]any{"query": `{users{totalCount}}`})
		require.Equal(t, 403, status, path)
	}
	req, _ := http.NewRequest("GET", server.URL+"/admin/playground", nil)
	req.Header.Set("Authorization", "Bearer "+ta)
	res, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	res.Body.Close()
	require.Equal(t, 403, res.StatusCode)
	status, body := post("/admin/graphql", tadmin, map[string]any{"query": `{me{email accessMode}}`})
	require.Equal(t, 200, status, string(body))
	require.Contains(t, string(body), admin.Email)
	adminMutation := func(q string, vars map[string]any) []byte {
		s, b := post("/admin/graphql", tadmin, map[string]any{"query": q, "variables": vars})
		require.Equal(t, 200, s, string(b))
		return b
	}
	groupMutation := `mutation($id:ID!,$input:UpdateProjectProfilesInput!){updateProjectProfiles(id:$id,input:$input){profiles{activeProfile profiles{name modelIDs channelIDs}}}}`
	for _, models := range []any{nil, []string{}, []string{"m"}} {
		raw := adminMutation(groupMutation, map[string]any{"id": fmt.Sprintf("gid://axonhub/Project/%d", g.ID), "input": map[string]any{"activeProfile": "active", "profiles": []any{map[string]any{"name": "active", "modelIDs": models, "channelIDs": []int{ch.ID}}}}})
		require.NotContains(t, string(raw), `"errors"`)
		stored := client.Project.GetX(ctx, g.ID).GetActiveProfile()
		require.NotNil(t, stored)
		if models == nil {
			require.Nil(t, stored.ModelIDs)
			require.Contains(t, string(raw), `"modelIDs":null`)
		} else {
			require.NotNil(t, stored.ModelIDs)
			require.Equal(t, models, *stored.ModelIDs)
		}
		if models == nil || len(models.([]string)) == 0 {
			s, _ := post("/v1/responses", secret, map[string]any{"model": "m", "input": "unconfigured or empty"})
			require.Equal(t, 401, s)
			require.Equal(t, int32(0), calls.Load())
		}
	}
	modeUser := makeUser("mode@example.test", user.AccessModeLegacy, false)
	legacyKey := client.APIKey.Create().SetName("mode legacy").SetKey("ah-mode-legacy").SetProjectID(g.ID).SetUserID(modeUser.ID).SetType(apikey.TypeUser).SaveX(ctx)
	modeMutation := `mutation($id:ID!,$disable:Boolean!){setUserAccessMode(id:$id,mode:portal,disableLegacyKeys:$disable){id accessMode}}`
	modeVars := map[string]any{"id": fmt.Sprintf("gid://axonhub/User/%d", modeUser.ID), "disable": false}
	require.Contains(t, string(adminMutation(modeMutation, modeVars)), `"errors"`)
	modeVars["disable"] = true
	require.NotContains(t, string(adminMutation(modeMutation, modeVars)), `"errors"`)
	require.Equal(t, apikey.StatusDisabled, client.APIKey.GetX(ctx, legacyKey.ID).Status)
	modeVars["id"] = fmt.Sprintf("gid://axonhub/User/%d", admin.ID)
	require.Contains(t, string(adminMutation(modeMutation, modeVars)), `"errors"`)
	status, _ = post("/v1/videos/b-video", secret, map[string]any{})
	require.Equal(t, 403, status)
	require.Equal(t, int32(0), calls.Load())
	// Save and read content using the real administrator schema, then published-only portal projection.
	status, body = post("/admin/graphql", tadmin, map[string]any{"query": `mutation{updatePortalSettings(input:{announcements:[],guides:[{slug:"live",title:"Live",markdown:"# Hello",published:true,pinned:false,order:0},{slug:"draft",title:"Draft",markdown:"secret",published:false,pinned:false,order:1}]}){guides{slug published}}}`})
	require.Equal(t, 200, status, string(body))
	require.NotContains(t, string(body), `"errors"`)
	content := okGraph(ta, `{content{guides{slug markdown}}}`, nil)
	require.Contains(t, string(content), "live")
	require.NotContains(t, string(content), "draft")
	updated := okGraph(ta, `mutation($id:Int!){updateKey(id:$id,input:{name:"A renamed",modelIDs:["m"],allowedIPs:["127.0.0.1/32"]}){id name modelIDs allowedIPs}}`, map[string]any{"id": ka})
	require.Contains(t, string(updated), "A renamed")
	okGraph(ta, `mutation($id:Int!){setKeyStatus(id:$id,status:"disabled"){id}}`, map[string]any{"id": ka})
	status, _ = post("/v1/responses", secret, map[string]any{"model": "m", "input": "disabled"})
	require.Equal(t, 401, status)
	require.Equal(t, int32(0), calls.Load())
	okGraph(ta, `mutation($id:Int!){setKeyStatus(id:$id,status:"enabled"){id}}`, map[string]any{"id": ka})
	auth.AllowNoAuth = true
	modeVars["id"] = fmt.Sprintf("gid://axonhub/User/%d", modeUser.ID)
	require.Contains(t, string(adminMutation(modeMutation, modeVars)), `"errors"`)
	badGraph(ta, `mutation($p:Int!){createKey(projectID:$p,input:{name:"anonymous-conflict"}){id}}`, map[string]any{"p": g.ID})
	auth.AllowNoAuth = false
	status, body = post("/v1/responses", secret, map[string]any{"model": "m", "input": "hello"})
	require.Equal(t, 200, status, string(body))
	require.Equal(t, int32(1), calls.Load())
	status, _ = post("/v1/responses", secret, map[string]any{"model": "other", "input": "blocked"})
	require.NotEqual(t, 200, status)
	require.Equal(t, int32(1), calls.Load())
	// A second user's legitimate traffic is excluded from A's personal aggregate.
	bk := client.APIKey.GetX(ctx, kb)
	status, body = post("/v1/responses", bk.Key, map[string]any{"model": "m", "input": "B"})
	require.Equal(t, 200, status, string(body))
	stats := okGraph(ta, `query($p:Int!){dashboard(projectID:$p,period:"all"){requests usageCount totalTokens pricedCount cost models{modelID} }}`, map[string]any{"p": g.ID})
	var d struct{ Dashboard objects.PortalDashboard }
	require.NoError(t, json.Unmarshal(stats, &d))
	require.Equal(t, 1, d.Dashboard.Requests)
	require.Equal(t, int64(5), d.Dashboard.TotalTokens)
	require.Nil(t, d.Dashboard.Cost)
	// Rotate preserves identity, history, and limits. The former secret immediately fails.
	okGraph(ta, `mutation($id:Int!){rotateKey(id:$id){id}}`, map[string]any{"id": ka})
	rotated := client.APIKey.GetX(ctx, ka)
	require.NotEqual(t, secret, rotated.Key)
	status, _ = post("/v1/responses", secret, map[string]any{"model": "m", "input": "old"})
	require.Equal(t, 401, status)
	require.Equal(t, int32(2), calls.Load())
	secret = rotated.Key
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer " + secret}})
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": "WS"}))
	require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
	require.Equal(t, int32(3), calls.Load())
	// A direct DB mutation intentionally leaves all old auth/key/project caches hot.
	client.Project.UpdateOne(g).SetProfiles(profile([]string{})).SaveX(ctx)
	require.Contains(t, string(okGraph(ta, `query($p:Int!){models(projectID:$p){id}}`, map[string]any{"p": g.ID})), `"models":[]`)
	status, _ = post("/v1/responses", secret, map[string]any{"model": "m", "input": "empty"})
	require.Equal(t, 401, status)
	require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": "revoked"}))
	require.Equal(t, "authentication_failed", wsErrorCode(wsRead(t, conn)))
	require.Equal(t, int32(3), calls.Load())
	client.Project.UpdateOne(g).SetProfiles(profile([]string{"m"})).SaveX(ctx)
	client.UserProject.Delete().Where(userproject.UserIDEQ(a.ID), userproject.ProjectIDEQ(g.ID)).ExecX(ctx)
	status, _ = post("/v1/responses", secret, map[string]any{"model": "m", "input": "removed"})
	require.Equal(t, 401, status)
	status, _ = post("/v1beta/models/m?key="+secret, "", map[string]any{})
	require.Equal(t, 401, status)
	badGraph(ta, `query($p:Int!){models(projectID:$p){id}}`, map[string]any{"p": g.ID})
	okGraph(ta, `mutation($id:Int!){setKeyStatus(id:$id,status:"archived"){id}}`, map[string]any{"id": ka})
	require.Contains(t, string(okGraph(ta, `query($p:Int!){dashboard(projectID:$p,period:"all"){usageCount}}`, map[string]any{"p": g.ID})), `"usageCount":2`)
	auth.AllowNoAuth = true
	status, _ = post("/v1/responses", "invalid", map[string]any{"model": "m"})
	require.Equal(t, 401, status)
	require.Equal(t, int32(3), calls.Load())
	// JWT mode changes are authoritative too; a formerly permitted admin token cannot escape.
	client.User.UpdateOne(b).SetAccessMode(user.AccessModeLegacy).SaveX(ctx)
	status, _ = post("/admin/graphql", tb, map[string]any{"query": `{me{email}}`})
	require.Equal(t, 200, status)
	client.User.UpdateOne(b).SetAccessMode(user.AccessModePortal).SaveX(ctx)
	status, _ = post("/admin/graphql", tb, map[string]any{"query": `{me{email}}`})
	require.Equal(t, 403, status)
	// Authorization failures did not enqueue provider calls.
	require.Equal(t, int32(3), calls.Load())
	// A legacy shared key keeps its original no-membership/no-explicit-profile contract.
	auth.AllowNoAuth = false
	oldGroup := client.Project.Create().SetName("Legacy group").SaveX(ctx)
	shared := client.APIKey.Create().SetName("Legacy shared").SetKey("ah-legacy-shared").SetUserID(admin.ID).SetProjectID(oldGroup.ID).SetType(apikey.TypeUser).SetScopes([]string{"read_channels", "write_requests"}).SaveX(ctx)
	status, body = post("/v1/responses", shared.Key, map[string]any{"model": "m", "input": "legacy control"})
	require.Equal(t, 200, status, string(body))
	require.Equal(t, int32(4), calls.Load())
	// Rehearse the downgrade prerequisite on this disposable database: the old
	// binary understands status, but does not understand the portal access marker.
	client.User.Update().Where(user.AccessModeEQ(user.AccessModePortal)).SetStatus(user.StatusDeactivated).ExecX(ctx)
	client.APIKey.Update().Where(apikey.PortalManagedEQ(true)).SetStatus(apikey.StatusDisabled).ExecX(ctx)
	status, _ = post("/portal/graphql", ta, map[string]any{"query": `{me{id}}`})
	require.Equal(t, 401, status)
	status, _ = post("/v1/responses", bk.Key, map[string]any{"model": "m", "input": "prepared downgrade"})
	require.Equal(t, 401, status)
	status, _ = post("/admin/graphql", tadmin, map[string]any{"query": `{me{email}}`})
	require.Equal(t, 200, status)
	require.Equal(t, int32(4), calls.Load())
}
