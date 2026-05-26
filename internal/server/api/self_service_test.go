package api

import (
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
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/pkg/xcache"
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
