package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestWithAPIKeyConfig_RejectsNoAuthKeyWhenDisabled(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(WithAPIKeyConfig(&biz.AuthService{}, nil))
	router.GET("/test", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+biz.NoAuthAPIKeyValue)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected status %d, got %d", http.StatusUnauthorized, recorder.Code)
	}
}

func TestWithAPIKeyConfig_AllowsMissingAuthorizationWhenNoAuthAllowed(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		key, err := ExtractAPIKeyFromRequest(c.Request, &APIKeyConfig{
			Headers:       []string{"Authorization"},
			RequireBearer: true,
		})
		if errors.Is(err, ErrAPIKeyRequired) {
			c.Status(http.StatusNoContent)
			c.Abort()

			return
		}

		if err != nil || key != "" {
			c.Status(http.StatusTeapot)
			c.Abort()

			return
		}
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d", http.StatusNoContent, recorder.Code)
	}
}

func TestAPIKeyAllowedIPs_AppliesToOrdinaryOpenAPIAndGeminiAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client := enttest.NewEntClient(t, "sqlite3", "file:auth-allowed-ips?mode=memory&_fk=1")
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	owner := client.User.Create().
		SetEmail(fmt.Sprintf("auth-%s@example.com", uuid.NewString())).
		SetPassword("password").
		SetStatus(user.StatusActivated).
		SaveX(ctx)
	proj := client.Project.Create().SetName(uuid.NewString()).SetStatus(project.StatusActive).SaveX(ctx)
	key := client.APIKey.Create().
		SetKey("ah-route-allowed-ips").
		SetName("route allowed ips").
		SetType(apikey.TypeServiceAccount).
		SetStatus(apikey.StatusEnabled).
		SetAllowedIps([]string{"203.0.113.0/24"}).
		SetUserID(owner.ID).
		SetProjectID(proj.ID).
		SaveX(ctx)

	cacheConfig := xcache.Config{Mode: xcache.ModeMemory}
	projectService := biz.NewProjectService(biz.ProjectServiceParams{CacheConfig: cacheConfig, Ent: client})
	apiKeyService := biz.NewAPIKeyService(biz.APIKeyServiceParams{
		CacheConfig: cacheConfig, Ent: client, ProjectService: projectService, KeyPrefix: "ah",
	})
	defer apiKeyService.Stop()
	auth := &biz.AuthService{APIKeyService: apiKeyService}

	tests := []struct {
		name       string
		middleware gin.HandlerFunc
		path       string
		authorize  func(*http.Request)
	}{
		{
			name: "ordinary API",
			middleware: WithAPIKeyConfig(auth, &APIKeyConfig{
				Headers: []string{"Authorization"}, RequireBearer: true,
			}),
			path: "/ordinary",
			authorize: func(req *http.Request) {
				req.Header.Set("Authorization", "Bearer "+key.Key)
			},
		},
		{
			name:       "OpenAPI",
			middleware: WithOpenAPIAuth(auth),
			path:       "/openapi",
			authorize: func(req *http.Request) {
				req.Header.Set("Authorization", "Bearer "+key.Key)
			},
		},
		{
			name:       "Gemini",
			middleware: WithGeminiKeyAuth(auth),
			path:       "/gemini?key=" + key.Key,
			authorize:  func(*http.Request) {},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router := gin.New()
			router.Use(tt.middleware)
			router.GET("/ordinary", func(c *gin.Context) { c.Status(http.StatusNoContent) })
			router.GET("/openapi", func(c *gin.Context) { c.Status(http.StatusNoContent) })
			router.GET("/gemini", func(c *gin.Context) { c.Status(http.StatusNoContent) })

			allowed := httptest.NewRequest(http.MethodGet, tt.path, nil)
			allowed.RemoteAddr = "203.0.113.8:1234"
			tt.authorize(allowed)
			allowedRecorder := httptest.NewRecorder()
			router.ServeHTTP(allowedRecorder, allowed)
			require.Equal(t, http.StatusNoContent, allowedRecorder.Code)

			denied := httptest.NewRequest(http.MethodGet, tt.path, nil)
			denied.RemoteAddr = "198.51.100.8:1234"
			denied.Header.Set("X-Forwarded-For", "203.0.113.8")
			tt.authorize(denied)
			deniedRecorder := httptest.NewRecorder()
			router.ServeHTTP(deniedRecorder, denied)
			require.Equal(t, http.StatusForbidden, deniedRecorder.Code)
		})
	}
}
