package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestSelfServiceHandlers_ListAPIKeysReturnsForbiddenWhenSelfServiceDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	apiKeyService := biz.NewAPIKeyService(biz.APIKeyServiceParams{
		CacheConfig:  xcache.Config{Mode: xcache.ModeMemory},
		Ent:          client,
		Registration: biz.RegistrationConfig{},
		KeyPrefix:    "ah",
	})
	t.Cleanup(apiKeyService.Stop)

	setupCtx := ent.NewContext(context.Background(), client)
	setupCtx = authz.WithTestBypass(setupCtx)

	testUser, err := client.User.Create().
		SetEmail("self-service-disabled@example.com").
		SetPassword("password").
		SetStatus(user.StatusActivated).
		Save(setupCtx)
	require.NoError(t, err)

	testProject, err := client.Project.Create().
		SetName("self-service-disabled-project").
		SetDescription("self-service-disabled-project").
		SetStatus(project.StatusActive).
		Save(setupCtx)
	require.NoError(t, err)

	_, err = client.UserProject.Create().
		SetUserID(testUser.ID).
		SetProjectID(testProject.ID).
		SetIsOwner(false).
		Save(setupCtx)
	require.NoError(t, err)

	handlers := NewSelfServiceHandlers(SelfServiceHandlersParams{APIKeyService: apiKeyService})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		requestCtx := ent.NewContext(c.Request.Context(), client)
		requestCtx = authz.NewUserContext(requestCtx, testUser.ID)
		requestCtx = contexts.WithUser(requestCtx, testUser)
		c.Request = c.Request.WithContext(requestCtx)
		c.Next()
	})
	router.GET("/admin/self/api-keys", handlers.ListAPIKeys)

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/admin/self/api-keys?project_id=%d", testProject.ID), nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusForbidden, recorder.Code)

	var body map[string]map[string]string
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Equal(t, "self-service is disabled", body["error"]["message"])
}

func TestSelfServiceHandlers_ListAdminAccessGroupsUsesQueryProjectIDForProjectScopedAdmin(t *testing.T) {
	gin.SetMode(gin.TestMode)

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	apiKeyService := biz.NewAPIKeyService(biz.APIKeyServiceParams{
		CacheConfig:  xcache.Config{Mode: xcache.ModeMemory},
		Ent:          client,
		Registration: biz.RegistrationConfig{SelfServiceEnabled: true, SelfServicePresetNames: []string{"fast"}},
		KeyPrefix:    "ah",
	})
	t.Cleanup(apiKeyService.Stop)

	setupCtx := ent.NewContext(context.Background(), client)
	setupCtx = authz.WithTestBypass(setupCtx)

	testProject, err := client.Project.Create().
		SetName("admin-access-groups-project").
		SetDescription("admin-access-groups-project").
		SetStatus(project.StatusActive).
		Save(setupCtx)
	require.NoError(t, err)

	adminUser, err := client.User.Create().
		SetEmail("project-admin-access-groups@example.com").
		SetPassword("password").
		SetStatus(user.StatusActivated).
		Save(setupCtx)
	require.NoError(t, err)

	_, err = client.UserProject.Create().
		SetUserID(adminUser.ID).
		SetProjectID(testProject.ID).
		SetIsOwner(false).
		SetScopes([]string{string(scopes.ScopeReadAPIKeys)}).
		Save(setupCtx)
	require.NoError(t, err)

	_, err = client.APIKeyProfileTemplate.Create().
		SetName("fast").
		SetDescription("Fast preset").
		SetProject(testProject).
		SetProfile(&objects.APIKeyProfile{Name: "fast"}).
		Save(setupCtx)
	require.NoError(t, err)

	adminUserWithEdges, err := client.User.Query().
		Where(user.IDEQ(adminUser.ID)).
		WithProjectUsers().
		WithRoles().
		Only(setupCtx)
	require.NoError(t, err)

	handlers := NewSelfServiceHandlers(SelfServiceHandlersParams{APIKeyService: apiKeyService})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		requestCtx := ent.NewContext(c.Request.Context(), client)
		requestCtx = authz.NewUserContext(requestCtx, adminUserWithEdges.ID)
		requestCtx = contexts.WithUser(requestCtx, adminUserWithEdges)
		c.Request = c.Request.WithContext(requestCtx)
		c.Next()
	})
	router.GET("/admin/access-groups", handlers.ListAdminAccessGroups)

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/admin/access-groups?project_id=%d", testProject.ID), nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)

	var body struct {
		Data []struct {
			ID        int `json:"id"`
			ProjectID int `json:"projectId"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Len(t, body.Data, 1)
	require.Equal(t, testProject.ID, body.Data[0].ProjectID)
}

func TestSelfServiceHandlers_ListAdminAccessGroupsRejectsNormalUser(t *testing.T) {
	gin.SetMode(gin.TestMode)

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	apiKeyService := biz.NewAPIKeyService(biz.APIKeyServiceParams{
		CacheConfig:  xcache.Config{Mode: xcache.ModeMemory},
		Ent:          client,
		Registration: biz.RegistrationConfig{SelfServiceEnabled: true, SelfServicePresetNames: []string{"fast"}},
		KeyPrefix:    "ah",
	})
	t.Cleanup(apiKeyService.Stop)

	setupCtx := ent.NewContext(context.Background(), client)
	setupCtx = authz.WithTestBypass(setupCtx)

	testProject, err := client.Project.Create().
		SetName("normal-user-access-groups-project").
		SetDescription("normal-user-access-groups-project").
		SetStatus(project.StatusActive).
		Save(setupCtx)
	require.NoError(t, err)

	normalUser, err := client.User.Create().
		SetEmail("normal-user-access-groups@example.com").
		SetPassword("password").
		SetStatus(user.StatusActivated).
		Save(setupCtx)
	require.NoError(t, err)

	_, err = client.UserProject.Create().
		SetUserID(normalUser.ID).
		SetProjectID(testProject.ID).
		SetIsOwner(false).
		Save(setupCtx)
	require.NoError(t, err)

	_, err = client.APIKeyProfileTemplate.Create().
		SetName("fast").
		SetDescription("Fast preset").
		SetProject(testProject).
		SetProfile(&objects.APIKeyProfile{Name: "fast"}).
		Save(setupCtx)
	require.NoError(t, err)

	normalUserWithEdges, err := client.User.Query().
		Where(user.IDEQ(normalUser.ID)).
		WithProjectUsers().
		WithRoles().
		Only(setupCtx)
	require.NoError(t, err)

	handlers := NewSelfServiceHandlers(SelfServiceHandlersParams{APIKeyService: apiKeyService})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		requestCtx := ent.NewContext(c.Request.Context(), client)
		requestCtx = authz.NewUserContext(requestCtx, normalUserWithEdges.ID)
		requestCtx = contexts.WithUser(requestCtx, normalUserWithEdges)
		c.Request = c.Request.WithContext(requestCtx)
		c.Next()
	})
	router.GET("/admin/access-groups", handlers.ListAdminAccessGroups)

	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/admin/access-groups?project_id=%d", testProject.ID), nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusForbidden, recorder.Code)
}

func TestSelfServiceHandlers_AddChannelsToAccessGroupUsesAccessGroupProjectContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	t.Cleanup(func() { _ = client.Close() })

	apiKeyService := biz.NewAPIKeyService(biz.APIKeyServiceParams{
		CacheConfig:  xcache.Config{Mode: xcache.ModeMemory},
		Ent:          client,
		Registration: biz.RegistrationConfig{SelfServiceEnabled: true, SelfServicePresetNames: []string{"fast"}},
		KeyPrefix:    "ah",
	})
	t.Cleanup(apiKeyService.Stop)

	setupCtx := ent.NewContext(context.Background(), client)
	setupCtx = authz.WithTestBypass(setupCtx)

	testProject, err := client.Project.Create().
		SetName("assign-access-group-project").
		SetDescription("assign-access-group-project").
		SetStatus(project.StatusActive).
		Save(setupCtx)
	require.NoError(t, err)

	adminUser, err := client.User.Create().
		SetEmail("project-admin-assign@example.com").
		SetPassword("password").
		SetStatus(user.StatusActivated).
		SetScopes([]string{string(scopes.ScopeReadChannels), string(scopes.ScopeWriteChannels)}).
		Save(setupCtx)
	require.NoError(t, err)

	_, err = client.UserProject.Create().
		SetUserID(adminUser.ID).
		SetProjectID(testProject.ID).
		SetIsOwner(false).
		SetScopes([]string{string(scopes.ScopeReadAPIKeys), string(scopes.ScopeWriteAPIKeys)}).
		Save(setupCtx)
	require.NoError(t, err)

	template, err := client.APIKeyProfileTemplate.Create().
		SetName("fast").
		SetDescription("Fast preset").
		SetProject(testProject).
		SetProfile(&objects.APIKeyProfile{Name: "fast"}).
		Save(setupCtx)
	require.NoError(t, err)

	ch, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName("channel-for-assignment").
		SetBaseURL("https://example.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKey: "secret"}).
		SetSupportedModels([]string{"gpt-4o-mini"}).
		SetManualModels([]string{}).
		SetDefaultTestModel("gpt-4o-mini").
		Save(setupCtx)
	require.NoError(t, err)

	adminUserWithEdges, err := client.User.Query().
		Where(user.IDEQ(adminUser.ID)).
		WithProjectUsers().
		WithRoles().
		Only(setupCtx)
	require.NoError(t, err)

	handlers := NewSelfServiceHandlers(SelfServiceHandlersParams{APIKeyService: apiKeyService})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		requestCtx := ent.NewContext(c.Request.Context(), client)
		requestCtx = authz.NewUserContext(requestCtx, adminUserWithEdges.ID)
		requestCtx = contexts.WithUser(requestCtx, adminUserWithEdges)
		c.Request = c.Request.WithContext(requestCtx)
		c.Next()
	})
	router.PATCH("/admin/access-groups/:id/channels", handlers.AddChannelsToAccessGroup)

	reqBody := []byte(fmt.Sprintf(`{"channelIds":["%d"]}`, ch.ID))
	req := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/admin/access-groups/%d/channels", template.ID), bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)

	updatedChannel, err := client.Channel.Get(setupCtx, ch.ID)
	require.NoError(t, err)
	require.Contains(t, updatedChannel.Tags, fmt.Sprintf("access-group:%d", template.ID))
}
