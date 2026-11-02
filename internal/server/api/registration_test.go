package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	entmodel "github.com/looplj/axonhub/internal/ent/model"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/gql"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/portalgql"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
	"github.com/stretchr/testify/require"
)

func TestRegistrationHTTPLoginPortalAndAdminIsolation(t *testing.T) {
	db := enttest.NewEntClient(t, "sqlite3", "file:registration-http-"+uuid.NewString()+"?mode=memory&_fk=1")
	defer db.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), db)
	cache := xcache.Config{Mode: xcache.ModeMemory}
	system := biz.NewSystemService(biz.SystemServiceParams{Ent: db, CacheConfig: cache})
	db.System.Create().SetKey(biz.SystemKeySecretKey).SetValue("synthetic-registration-jwt").SaveX(ctx)
	users := biz.NewUserService(biz.UserServiceParams{Ent: db, CacheConfig: cache})
	projects := biz.NewProjectService(biz.ProjectServiceParams{Ent: db, CacheConfig: cache})
	keys := biz.NewAPIKeyService(biz.APIKeyServiceParams{Ent: db, CacheConfig: cache, ProjectService: projects, KeyPrefix: "ah"})
	defer keys.Stop()
	auth := biz.NewAuthService(biz.AuthServiceParams{Ent: db, SystemService: system, UserService: users, APIKeyService: keys})
	owner := db.User.Create().SetEmail("owner@example.test").SetPassword("unused").SetIsOwner(true).SaveX(ctx)
	ch := db.Channel.Create().SetName("Allowed channel").SetType(channel.TypeOpenaiResponses).SetBaseURL("http://127.0.0.1:1").SetCredentials(objects.ChannelCredentials{APIKey: "synthetic"}).SetSupportedModels([]string{"m"}).SetDefaultTestModel("m").SaveX(ctx)
	outbound, err := responses.NewOutboundTransformer(ch.BaseURL, "synthetic")
	require.NoError(t, err)
	channels := biz.NewChannelServiceForTest(db)
	defer channels.Stop()
	channels.SetEnabledChannelsForTest([]*biz.Channel{{Channel: ch, Outbound: outbound}})
	db.Model.Create().SetModelID("m").SetName("Model").SetType(entmodel.TypeChat).SetStatus(entmodel.StatusEnabled).SetDeveloper("synthetic").SetIcon("OpenAI").SetGroup("synthetic").SetModelCard(&objects.ModelCard{}).SetSettings(&objects.ModelSettings{Associations: []*objects.ModelAssociation{{Type: "channel_model", ChannelModel: &objects.ChannelModelAssociation{ChannelID: ch.ID, ModelID: "m"}}}}).SaveX(ctx)
	ids := []string{"m"}
	g := db.Project.Create().SetName("Registration group").SetProfiles(&objects.ProjectProfiles{ActiveProfile: "p", Profiles: []objects.ProjectProfile{{Name: "p", ModelIDs: &ids, ChannelIDs: []int{ch.ID}}}}).SaveX(ctx)
	other := db.Project.Create().SetName("Other group").SaveX(ctx)
	models := biz.NewModelService(biz.ModelServiceParams{Ent: db, SystemService: system, ChannelService: channels})
	quota := biz.NewQuotaService(db, system)
	portal := biz.NewPortalService(biz.PortalServiceParams{Ent: db, Keys: keys, Models: models, Quota: quota, System: system, Auth: auth, Users: users})
	pgql := portalgql.NewGraphqlHandlers(portal, users)
	agql := gql.NewGraphqlHandlers(gql.Dependencies{Ent: db, AuthService: auth, UserService: users, ProjectService: projects, SystemService: system})
	authHandlers := NewAuthHandlers(AuthHandlersParams{AuthService: auth})
	router := gin.New()
	router.Use(middleware.WithEntClient(db))
	router.GET("/admin/auth/registration", authHandlers.RegistrationStatus)
	router.POST("/admin/auth/signup", authHandlers.SignUp)
	router.POST("/admin/auth/signin", authHandlers.SignIn)
	router.POST("/portal/graphql", middleware.WithJWTAuth(auth), func(c *gin.Context) { pgql.Graphql.ServeHTTP(c.Writer, c.Request) })
	router.POST("/admin/graphql", middleware.WithJWTAuth(auth), middleware.WithLegacyUser(), middleware.WithProjectID(), func(c *gin.Context) { agql.Graphql.ServeHTTP(c.Writer, c.Request) })
	server := httptest.NewServer(router)
	defer server.Close()
	adminToken, err := auth.GenerateJWTToken(ctx, owner)
	require.NoError(t, err)
	post := func(path, token string, body any) (int, []byte) {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		r, err := http.NewRequest("POST", server.URL+path, bytes.NewReader(raw))
		require.NoError(t, err)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Project-ID", fmt.Sprintf("gid://axonhub/Project/%d", g.ID))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(r)
		require.NoError(t, err)
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		return res.StatusCode, data
	}
	adminGraph := func(query string, variables map[string]any) json.RawMessage {
		status, raw := post("/admin/graphql", adminToken, map[string]any{"query": query, "variables": variables})
		require.Equal(t, 200, status, string(raw))
		var body struct {
			Data   json.RawMessage
			Errors json.RawMessage
		}
		require.NoError(t, json.Unmarshal(raw, &body))
		require.Empty(t, body.Errors, string(raw))
		return body.Data
	}
	createQuery := `mutation($p:Int!){createRegistrationInvite(input:{projectID:$p}){code invite{id projectID maxUses usedUses status}}}`
	create := func() struct {
		Code   string
		Invite objects.RegistrationInvite
	} {
		raw := adminGraph(createQuery, map[string]any{"p": g.ID})
		var body struct {
			CreateRegistrationInvite struct {
				Code   string
				Invite objects.RegistrationInvite
			}
		}
		require.NoError(t, json.Unmarshal(raw, &body))
		return body.CreateRegistrationInvite
	}
	invite := create()
	require.Equal(t, 1, invite.Invite.MaxUses)
	body := map[string]any{"email": "registered@example.test", "password": "SyntheticPassword1", "confirmPassword": "SyntheticPassword1", "firstName": "Given", "lastName": "Family", "inviteCode": invite.Code}
	status, raw := post("/admin/auth/signup", "", body)
	require.Equal(t, 403, status, string(raw))
	require.Zero(t, db.RegistrationInvite.GetX(ctx, invite.Invite.ID).UsedUses)
	adminGraph(`mutation{setInvitationRegistration(enabled:true)}`, nil)
	for _, field := range []string{"roleIDs", "roles", "scopes", "isOwner", "accessMode", "access_mode", "projectID"} {
		bad := map[string]any{}
		for key, value := range body {
			bad[key] = value
		}
		bad[field] = other.ID
		status, raw = post("/admin/auth/signup", "", bad)
		require.Equal(t, 400, status, string(raw))
		require.Zero(t, db.RegistrationInvite.GetX(ctx, invite.Invite.ID).UsedUses)
	}
	status, raw = post("/admin/auth/signup", "", body)
	require.Equal(t, 201, status, string(raw))
	require.NotContains(t, string(raw), invite.Code)
	u := db.User.Query().Where(user.EmailEQ("registered@example.test")).WithProjectUsers().WithRoles().OnlyX(ctx)
	require.Equal(t, user.AccessModePortal, u.AccessMode)
	require.False(t, u.IsOwner)
	require.Empty(t, u.Scopes)
	require.Empty(t, u.Edges.Roles)
	require.Len(t, u.Edges.ProjectUsers, 1)
	require.Equal(t, g.ID, u.Edges.ProjectUsers[0].ProjectID)
	status, raw = post("/admin/auth/signin", "", map[string]any{"email": body["email"], "password": body["password"]})
	require.Equal(t, 200, status, string(raw))
	var signed struct {
		Token string
		User  objects.UserInfo
	}
	require.NoError(t, json.Unmarshal(raw, &signed))
	require.Equal(t, "portal", string(signed.User.AccessMode))
	require.Empty(t, signed.User.Scopes)
	require.False(t, signed.User.IsOwner)
	status, raw = post("/portal/graphql", signed.Token, map[string]any{"query": fmt.Sprintf(`{groups{id name} modelChannels(projectID:%d){name models{id}}}`, g.ID)})
	require.Equal(t, 200, status, string(raw))
	require.Contains(t, string(raw), "Allowed channel")
	require.Contains(t, string(raw), `"id":"m"`)
	require.NotContains(t, string(raw), "Other group")
	status, raw = post("/portal/graphql", signed.Token, map[string]any{"query": fmt.Sprintf(`mutation{createKey(projectID:%d,input:{name:"Registered key"}){id projectID}}`, g.ID)})
	require.Equal(t, 200, status, string(raw))
	require.NotContains(t, string(raw), `"errors"`)
	status, raw = post("/portal/graphql", signed.Token, map[string]any{"query": fmt.Sprintf(`mutation{createKey(projectID:%d,input:{name:"Cross group"}){id}}`, other.ID)})
	require.Equal(t, 200, status, string(raw))
	require.Contains(t, string(raw), `"errors"`)
	status, raw = post("/admin/graphql", signed.Token, map[string]any{"query": `{registrationInvites{id}}`})
	require.Equal(t, 403, status, string(raw))
	// A project-level user manager cannot turn a group grant into global admission.
	localAdmin := db.User.Create().SetEmail("local-manager@example.test").SetPassword("unused").SaveX(ctx)
	db.UserProject.Create().SetUserID(localAdmin.ID).SetProjectID(g.ID).SetIsOwner(true).SaveX(ctx)
	localToken, err := auth.GenerateJWTToken(ctx, localAdmin)
	require.NoError(t, err)
	status, raw = post("/admin/graphql", localToken, map[string]any{"query": createQuery, "variables": map[string]any{"p": g.ID}})
	require.Equal(t, 200, status, string(raw))
	require.Contains(t, string(raw), "permission denied")
	list := adminGraph(`{registrationInvites{id status} registrationGroups{id name}}`, nil)
	require.NotContains(t, string(list), invite.Code)
	require.NotContains(t, string(list), "digest")
	require.Contains(t, string(list), `"status":"used"`)
	for _, field := range []string{"code", "digest"} {
		status, raw = post("/admin/graphql", adminToken, map[string]any{"query": fmt.Sprintf(`{registrationInvites{%s}}`, field)})
		require.Contains(t, []int{200, 422}, status)
		require.Contains(t, string(raw), `"errors"`)
	}
	revoked := create()
	adminGraph(fmt.Sprintf(`mutation{revokeRegistrationInvite(id:%d){status}}`, revoked.Invite.ID), nil)
	body["email"], body["inviteCode"] = "revoked@example.test", revoked.Code
	status, raw = post("/admin/auth/signup", "", body)
	require.Equal(t, 400, status, string(raw))
	require.Zero(t, db.RegistrationInvite.GetX(ctx, revoked.Invite.ID).UsedUses)
	if node := os.Getenv("AXON_PORTAL_NODE"); node != "" {
		nodeCtx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		cmd := exec.CommandContext(nodeCtx, node, "../../../integration_test/portal/registration_client.mjs", server.URL)
		cmd.Env = append(os.Environ(), "AXON_REGISTRATION_ADMIN="+adminToken, fmt.Sprintf("AXON_REGISTRATION_GROUP=%d", g.ID))
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, string(output))
		t.Log(string(output))
	}
	adminGraph(`mutation{setInvitationRegistration(enabled:false)}`, nil)
	res, err := http.Get(server.URL + "/admin/auth/registration")
	require.NoError(t, err)
	defer res.Body.Close()
	raw, err = io.ReadAll(res.Body)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"enabled":false`)
	require.Contains(t, res.Header.Get("Cache-Control"), "no-store")
	require.False(t, strings.Contains(string(raw), "synthetic"))
}
