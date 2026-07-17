package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/server/diagnostics"
	"github.com/looplj/axonhub/internal/server/middleware"
)

func TestDiagnosticsPullRejectsOversizedBodyWithContractError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/admin/diagnostics/v1/pull", strings.NewReader(strings.Repeat(" ", (64<<10)+1)))
	ctx.Request.Header.Set("Content-Type", "application/json")

	(&DiagnosticsHandlers{}).Pull(ctx)

	require.Equal(t, http.StatusRequestEntityTooLarge, recorder.Code)
	require.Equal(t, diagnosticsMediaType, recorder.Header().Get("Content-Type"))
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	require.Equal(t, "nosniff", recorder.Header().Get("X-Content-Type-Options"))
	require.Contains(t, recorder.Body.String(), `"code":"REQUEST_TOO_LARGE"`)
}

func TestDiagnosticsAuthMiddlewareUsesContractErrorEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &DiagnosticsHandlers{}
	for _, authMiddleware := range []gin.HandlerFunc{
		middleware.WithJWTAuthResponder(nil, handler.MiddlewareError),
		middleware.WithOpenAPIAuthResponder(nil, handler.MiddlewareError),
	} {
		router := gin.New()
		router.POST("/diagnostics", authMiddleware, func(c *gin.Context) { c.Status(http.StatusNoContent) })
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/diagnostics", nil)
		router.ServeHTTP(recorder, request)

		require.Equal(t, http.StatusUnauthorized, recorder.Code)
		require.Equal(t, diagnosticsMediaType, recorder.Header().Get("Content-Type"))
		require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
		require.Contains(t, recorder.Body.String(), `"code":"UNAUTHENTICATED"`)
		require.Contains(t, recorder.Body.String(), `"correlationId":`)
	}
}

func TestDiagnosticsInvalidProjectMiddlewareUsesContractErrorEnvelope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &DiagnosticsHandlers{}
	router := gin.New()
	router.POST("/diagnostics", middleware.WithProjectIDResponder(handler.MiddlewareError), func(c *gin.Context) { c.Status(http.StatusNoContent) })
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/diagnostics", nil)
	request.Header.Set("X-Project-ID", "not-a-project-guid")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Equal(t, diagnosticsMediaType, recorder.Header().Get("Content-Type"))
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	require.Contains(t, recorder.Body.String(), `"code":"INVALID_PROJECT"`)
}

func TestDiagnosticsPullPreservesLargeRequestIDThroughHTTPBoundary(t *testing.T) {
	gin.SetMode(gin.TestMode)
	client := enttest.NewEntClient(t, "sqlite3", "file:diagnostics-http-large-id?mode=memory&_fk=1")
	defer client.Close()
	setupCtx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	projectRow := client.Project.Create().SetName("diagnostics-project").SetStatus(project.StatusActive).SaveX(setupCtx)
	owner := client.User.Create().SetEmail("diagnostics-owner@example.com").SetPassword("x").SetStatus(user.StatusActivated).SetIsOwner(true).SaveX(setupCtx)
	handler := &DiagnosticsHandlers{service: diagnostics.NewService(diagnostics.Params{Ent: client})}
	requestCtx := authz.NewUserContext(context.Background(), owner.ID)
	requestCtx = contexts.WithUser(requestCtx, owner)
	requestCtx = contexts.WithProjectID(requestCtx, projectRow.ID)

	pull := func(id string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"contract":{"name":"axonhub.remote-diagnostics","major":1,"minMinor":0,"maxMinor":0},"scope":{"projectId":%d},"selector":{"kind":"requestIds","ids":[%s]},"include":{"sections":["requests"]}}`, projectRow.ID, id)
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/admin/diagnostics/v1/pull", strings.NewReader(body)).WithContext(requestCtx)
		ctx.Request.Header.Set("Content-Type", "application/json")
		handler.Pull(ctx)
		return recorder
	}

	exact := pull("9007199254740993")
	require.Equal(t, http.StatusOK, exact.Code, exact.Body.String())
	require.Contains(t, exact.Body.String(), `"kind":"requestId"`)
	require.Contains(t, exact.Body.String(), `"value":"9007199254740993"`)
	require.Contains(t, exact.Body.String(), `"ids":[9007199254740993]`)
	require.NotContains(t, exact.Body.String(), "9007199254740992")

	overflow := pull("9223372036854775808")
	require.Equal(t, http.StatusBadRequest, overflow.Code, overflow.Body.String())
	require.Contains(t, overflow.Body.String(), `"code":"VALIDATION_FAILED"`)

	fractional := pull("9007199254740993.5")
	require.Equal(t, http.StatusBadRequest, fractional.Code, fractional.Body.String())
	require.Contains(t, fractional.Body.String(), `"code":"VALIDATION_FAILED"`)
}
