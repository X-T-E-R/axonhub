package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestManagementChannels_MissingReadScopeDenied(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	project := createManagementTestProject(t, client)
	callCtx := managementAPIKeyContext(client, project.ID, []string{string(scopes.ScopeWriteChannels)})
	handler := &ManagementHandlers{
		ent:                 client,
		channelService:      biz.NewChannelServiceForTest(client),
		permissionValidator: biz.NewPermissionValidator(),
	}

	router := gin.New()
	router.Use(withManagementTestContext(callCtx))
	handler.RegisterOpenAPIRoutes(router.Group(""))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/management/channels", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
}

func TestManagementCapabilities_IncludesTokenAndHealthCheckOperations(t *testing.T) {
	handler := &ManagementHandlers{}

	router := gin.New()
	handler.RegisterOpenAPIRoutes(router.Group(""))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/management/capabilities", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp managementCapabilitiesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, string(scopes.ScopeWriteAPIKeys), resp.Operations["POST /admin/management/tokens"])
	require.Equal(t, string(scopes.ScopeReadAPIKeys), resp.Operations["GET /admin/management/tokens"])
	require.Equal(t, string(scopes.ScopeReadAPIKeys), resp.Operations["GET /admin/management/tokens/:id"])
	require.Equal(t, string(scopes.ScopeWriteAPIKeys), resp.Operations["PATCH /admin/management/tokens/:id"])
	require.Equal(t, string(scopes.ScopeWriteAPIKeys), resp.Operations["POST /admin/management/tokens/:id/revoke"])
	require.Equal(t, string(scopes.ScopeWriteChannels), resp.Operations["POST /openapi/v1/management/channels/:id/keys/:key_id/health-check"])
	require.Equal(t, string(scopes.ScopeWriteChannels), resp.Operations["POST /openapi/v1/management/channels/:id/keys/health-check"])
}

func TestManagementAddChannelKeys_DoesNotEchoRawProviderKey(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	project := createManagementTestProject(t, client)
	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("management-test-channel").
		SetBaseURL("https://api.example.test/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"sk-existing-provider-key"}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetStatus(channel.StatusEnabled).
		Save(setupCtx)
	require.NoError(t, err)

	callCtx := managementAPIKeyContext(client, project.ID, []string{string(scopes.ScopeWriteChannels)})
	handler := &ManagementHandlers{
		ent:                 client,
		channelService:      biz.NewChannelServiceForTest(client),
		permissionValidator: biz.NewPermissionValidator(),
	}

	router := gin.New()
	router.Use(withManagementTestContext(callCtx))
	handler.RegisterOpenAPIRoutes(router.Group(""))

	const rawProviderKey = "sk-new-provider-secret"
	body := strings.NewReader(`{"keys":["` + rawProviderKey + `"]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/management/channels/"+strconvItoa(ch.ID)+"/keys", body)
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), rawProviderKey)
	require.Contains(t, rec.Body.String(), objects.MaskChannelAPIKey(rawProviderKey))

	updated, err := client.Channel.Get(setupCtx, ch.ID)
	require.NoError(t, err)
	require.Contains(t, updated.Credentials.GetAllAPIKeys(), rawProviderKey)
}

func TestManagementSingleKeyHealthCheckRoute_RequiresWriteScope(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	project := createManagementTestProject(t, client)
	callCtx := managementAPIKeyContext(client, project.ID, []string{string(scopes.ScopeReadChannels)})
	handler := &ManagementHandlers{
		ent:                 client,
		channelService:      biz.NewChannelServiceForTest(client),
		permissionValidator: biz.NewPermissionValidator(),
	}

	router := gin.New()
	router.Use(withManagementTestContext(callCtx))
	handler.RegisterOpenAPIRoutes(router.Group(""))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/management/channels/1/keys/key-id/health-check", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestManagementSingleKeyHealthCheckRoute_RejectsUnknownKey(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	project := createManagementTestProject(t, client)
	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("management-test-health-channel").
		SetBaseURL("https://api.example.test/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"sk-existing-provider-key"}}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetStatus(channel.StatusEnabled).
		Save(setupCtx)
	require.NoError(t, err)

	callCtx := managementAPIKeyContext(client, project.ID, []string{string(scopes.ScopeWriteChannels)})
	handler := &ManagementHandlers{
		ent:                 client,
		channelService:      biz.NewChannelServiceForTest(client),
		permissionValidator: biz.NewPermissionValidator(),
	}

	router := gin.New()
	router.Use(withManagementTestContext(callCtx))
	handler.RegisterOpenAPIRoutes(router.Group(""))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/management/channels/"+strconvItoa(ch.ID)+"/keys/not-a-known-key/health-check", nil)
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "sk-existing-provider-key")
}

func TestManagementSelectedKeyHealthCheckRoute_RejectsExplicitEmptyKeyIDs(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	project := createManagementTestProject(t, client)
	callCtx := managementAPIKeyContext(client, project.ID, []string{string(scopes.ScopeWriteChannels)})
	handler := &ManagementHandlers{
		ent:                 client,
		channelService:      biz.NewChannelServiceForTest(client),
		permissionValidator: biz.NewPermissionValidator(),
	}

	router := gin.New()
	router.Use(withManagementTestContext(callCtx))
	handler.RegisterOpenAPIRoutes(router.Group(""))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/management/channels/1/keys/health-check", strings.NewReader(`{"keyIds":["  ",""]}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

func TestManagementChannelKeyIDMatchesFingerprintAndRawKey(t *testing.T) {
	key := "sk-existing-provider-key"
	fingerprint := objects.ChannelAPIKeyFingerprint(key)

	require.True(t, managementChannelKeyIDMatches(fingerprint, fingerprint))
	require.True(t, managementChannelKeyIDMatches(fingerprint, key))
	require.False(t, managementChannelKeyIDMatches(fingerprint, "key_unknown"))
}

func TestManagementTokenCreate_ReturnsRawTokenOnlyOnce(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	project := createManagementTestProject(t, client)
	owner := createManagementTestOwner(t, client)
	callCtx := ent.NewContext(context.Background(), client)
	callCtx = authz.NewUserContext(callCtx, owner.ID)
	callCtx = contexts.WithUser(callCtx, owner)
	callCtx = contexts.WithProjectID(callCtx, project.ID)

	apiKeySvc := biz.NewAPIKeyService(biz.APIKeyServiceParams{
		CacheConfig:    xcache.Config{Mode: xcache.ModeMemory},
		Ent:            client,
		ProjectService: biz.NewProjectService(biz.ProjectServiceParams{CacheConfig: xcache.Config{Mode: xcache.ModeMemory}, Ent: client}),
		KeyPrefix:      "ah",
	})
	t.Cleanup(apiKeySvc.Stop)

	handler := &ManagementHandlers{
		ent:                 client,
		apiKeyService:       apiKeySvc,
		permissionValidator: biz.NewPermissionValidator(),
	}

	router := gin.New()
	router.Use(withManagementTestContext(callCtx))
	handler.RegisterAdminRoutes(router.Group(""))

	createBody := bytes.NewBufferString(`{"name":"ops-bot","projectId":` + strconvItoa(project.ID) + `,"scopes":["read_channels"]}`)
	createRec := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/management/tokens", createBody)
	createReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(createRec, createReq)

	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	var created managementTokenResponse
	require.NoError(t, json.Unmarshal(createRec.Body.Bytes(), &created))
	require.NotEmpty(t, created.Key)
	require.Equal(t, apikey.TypeServiceAccount, created.Type)
	require.Equal(t, []string{string(scopes.ScopeReadChannels)}, created.Scopes)

	listRec := httptest.NewRecorder()
	listReq := httptest.NewRequest(http.MethodGet, "/management/tokens?projectId="+strconvItoa(project.ID), nil)
	router.ServeHTTP(listRec, listReq)

	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	require.NotContains(t, listRec.Body.String(), created.Key)
	require.Contains(t, listRec.Body.String(), created.MaskedKey)
}

func TestManagementTokenCreate_InvalidProjectRejected(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	owner := createManagementTestOwner(t, client)
	callCtx := ent.NewContext(context.Background(), client)
	callCtx = authz.NewUserContext(callCtx, owner.ID)
	callCtx = contexts.WithUser(callCtx, owner)
	callCtx = contexts.WithProjectID(callCtx, 99999)

	apiKeySvc := biz.NewAPIKeyService(biz.APIKeyServiceParams{
		CacheConfig:    xcache.Config{Mode: xcache.ModeMemory},
		Ent:            client,
		ProjectService: biz.NewProjectService(biz.ProjectServiceParams{CacheConfig: xcache.Config{Mode: xcache.ModeMemory}, Ent: client}),
		KeyPrefix:      "ah",
	})
	t.Cleanup(apiKeySvc.Stop)

	handler := &ManagementHandlers{
		ent:                 client,
		apiKeyService:       apiKeySvc,
		permissionValidator: biz.NewPermissionValidator(),
	}

	router := gin.New()
	router.Use(withManagementTestContext(callCtx))
	handler.RegisterAdminRoutes(router.Group(""))

	createBody := bytes.NewBufferString(`{"name":"ops-bot","projectId":99999,"scopes":["read_channels"]}`)
	createRec := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/management/tokens", createBody)
	createReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(createRec, createReq)

	require.Equal(t, http.StatusBadRequest, createRec.Code, createRec.Body.String())

	count, err := client.APIKey.Query().Count(authz.WithTestBypass(ent.NewContext(context.Background(), client)))
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestManagementTokenCreate_ProjectOwnerCannotGrantWildcard(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	project := createManagementTestProject(t, client)
	projectOwner := createManagementTestProjectOwner(t, client, project.ID)
	callCtx := ent.NewContext(context.Background(), client)
	callCtx = authz.NewUserContext(callCtx, projectOwner.ID)
	callCtx = contexts.WithUser(callCtx, projectOwner)
	callCtx = contexts.WithProjectID(callCtx, project.ID)

	apiKeySvc := biz.NewAPIKeyService(biz.APIKeyServiceParams{
		CacheConfig:    xcache.Config{Mode: xcache.ModeMemory},
		Ent:            client,
		ProjectService: biz.NewProjectService(biz.ProjectServiceParams{CacheConfig: xcache.Config{Mode: xcache.ModeMemory}, Ent: client}),
		KeyPrefix:      "ah",
	})
	t.Cleanup(apiKeySvc.Stop)

	handler := &ManagementHandlers{
		ent:                 client,
		apiKeyService:       apiKeySvc,
		permissionValidator: biz.NewPermissionValidator(),
	}

	router := gin.New()
	router.Use(withManagementTestContext(callCtx))
	handler.RegisterAdminRoutes(router.Group(""))

	createBody := bytes.NewBufferString(`{"name":"project-owner-bot","projectId":` + strconvItoa(project.ID) + `,"scopes":["*"]}`)
	createRec := httptest.NewRecorder()
	createReq := httptest.NewRequest(http.MethodPost, "/management/tokens", createBody)
	createReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(createRec, createReq)

	require.Equal(t, http.StatusForbidden, createRec.Code, createRec.Body.String())

	count, err := client.APIKey.Query().Count(authz.WithTestBypass(ent.NewContext(context.Background(), client)))
	require.NoError(t, err)
	require.Zero(t, count)
}

func TestManagementTokenUpdate_InvalidStatusDoesNotPersistOtherChanges(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	project := createManagementTestProject(t, client)
	owner := createManagementTestOwner(t, client)
	callCtx := ent.NewContext(context.Background(), client)
	callCtx = authz.NewUserContext(callCtx, owner.ID)
	callCtx = contexts.WithUser(callCtx, owner)
	callCtx = contexts.WithProjectID(callCtx, project.ID)

	apiKeySvc := biz.NewAPIKeyService(biz.APIKeyServiceParams{
		CacheConfig:    xcache.Config{Mode: xcache.ModeMemory},
		Ent:            client,
		ProjectService: biz.NewProjectService(biz.ProjectServiceParams{CacheConfig: xcache.Config{Mode: xcache.ModeMemory}, Ent: client}),
		KeyPrefix:      "ah",
	})
	t.Cleanup(apiKeySvc.Stop)

	tokenType := apikey.TypeServiceAccount
	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	token, err := apiKeySvc.CreateAPIKey(
		contexts.WithUser(authz.NewUserContext(setupCtx, owner.ID), owner),
		ent.CreateAPIKeyInput{
			Name:      "ops-bot",
			Type:      &tokenType,
			ProjectID: project.ID,
			Scopes:    []string{string(scopes.ScopeReadChannels)},
		},
	)
	require.NoError(t, err)

	handler := &ManagementHandlers{
		ent:                 client,
		apiKeyService:       apiKeySvc,
		permissionValidator: biz.NewPermissionValidator(),
	}

	router := gin.New()
	router.Use(withManagementTestContext(callCtx))
	handler.RegisterAdminRoutes(router.Group(""))

	updateBody := bytes.NewBufferString(`{"name":"should-not-stick","status":"bogus"}`)
	updateRec := httptest.NewRecorder()
	updateReq := httptest.NewRequest(http.MethodPatch, "/management/tokens/"+strconvItoa(token.ID), updateBody)
	updateReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(updateRec, updateReq)

	require.Equal(t, http.StatusBadRequest, updateRec.Code, updateRec.Body.String())

	stored, err := client.APIKey.Get(setupCtx, token.ID)
	require.NoError(t, err)
	require.Equal(t, "ops-bot", stored.Name)
	require.Equal(t, apikey.StatusEnabled, stored.Status)
}

func createManagementTestProject(t *testing.T, client *ent.Client) *ent.Project {
	t.Helper()

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	project, err := client.Project.Create().
		SetName("management-test-project-" + strconvItoa(int(time.Now().UnixNano()))).
		Save(setupCtx)
	require.NoError(t, err)

	return project
}

func createManagementTestOwner(t *testing.T, client *ent.Client) *ent.User {
	t.Helper()

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	hashed, err := biz.HashPassword("test-password")
	require.NoError(t, err)

	owner, err := client.User.Create().
		SetEmail("management-owner-" + strconvItoa(int(time.Now().UnixNano())) + "@example.test").
		SetPassword(hashed).
		SetFirstName("Management").
		SetLastName("Owner").
		SetStatus(user.StatusActivated).
		SetIsOwner(true).
		Save(setupCtx)
	require.NoError(t, err)

	return owner
}

func createManagementTestProjectOwner(t *testing.T, client *ent.Client, projectID int) *ent.User {
	t.Helper()

	setupCtx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	hashed, err := biz.HashPassword("test-password")
	require.NoError(t, err)

	projectOwner, err := client.User.Create().
		SetEmail("management-project-owner-" + strconvItoa(int(time.Now().UnixNano())) + "@example.test").
		SetPassword(hashed).
		SetFirstName("Management").
		SetLastName("ProjectOwner").
		SetStatus(user.StatusActivated).
		SetIsOwner(false).
		SetScopes([]string{string(scopes.ScopeWriteAPIKeys)}).
		Save(setupCtx)
	require.NoError(t, err)

	membership, err := client.UserProject.Create().
		SetUserID(projectOwner.ID).
		SetProjectID(projectID).
		SetIsOwner(true).
		Save(setupCtx)
	require.NoError(t, err)
	projectOwner.Edges.ProjectUsers = []*ent.UserProject{membership}

	return projectOwner
}

func managementAPIKeyContext(client *ent.Client, projectID int, tokenScopes []string) context.Context {
	apiKey := &ent.APIKey{
		ID:        1,
		ProjectID: projectID,
		Type:      apikey.TypeServiceAccount,
		Status:    apikey.StatusEnabled,
		Scopes:    tokenScopes,
	}

	ctx := ent.NewContext(context.Background(), client)
	ctx = authz.NewAPIKeyContext(ctx, apiKey.ID, projectID)
	ctx = contexts.WithAPIKey(ctx, apiKey)
	ctx = contexts.WithProjectID(ctx, projectID)

	return ctx
}

func withManagementTestContext(ctx context.Context) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func strconvItoa(value int) string {
	return strconv.FormatInt(int64(value), 10)
}
