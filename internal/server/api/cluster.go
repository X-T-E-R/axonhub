package api

import (
	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"net/http"
	"strings"
)

type ClusterHandlers struct {
	models   *biz.ModelService
	channels *biz.ChannelService
	tester   *orchestrator.TestChannelOrchestrator
	videos   *biz.VideoService
	quota    *biz.ProviderQuotaService
}

func NewClusterHandlers(models *biz.ModelService, channels *biz.ChannelService, tester *orchestrator.TestChannelOrchestrator, videos *biz.VideoService, quota *biz.ProviderQuotaService) *ClusterHandlers {
	return &ClusterHandlers{models: models, channels: channels, tester: tester, videos: videos, quota: quota}
}

func (h *ClusterHandlers) ChannelQuotaReset(c *gin.Context) {
	if err := authz.RequireScope(c.Request.Context(), scopes.ScopeWriteChannels); err != nil {
		JSONError(c, http.StatusForbidden, err)
		return
	}
	var input biz.OwnerQuotaResetRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	if err := c.ShouldBindJSON(&input); err != nil {
		JSONError(c, http.StatusBadRequest, err)
		return
	}
	if !h.channels.OwnsChannel(input.ChannelID) {
		JSONError(c, http.StatusConflict, h.channels.ChannelOwnerError(input.ChannelID))
		return
	}
	if err := h.quota.ResetChannelQuotaNow(c.Request.Context(), input.ChannelID); err != nil {
		JSONError(c, http.StatusBadRequest, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, true)
}

func WithClusterCallerAuthorization() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request = c.Request.WithContext(cluster.WithCallerAuthorization(c.Request.Context(), c.GetHeader("Authorization")))
		c.Next()
	}
}

func (h *ClusterHandlers) Manifest(c *gin.Context) {
	if err := authz.RequireScope(c.Request.Context(), scopes.ScopeReadChannels); err != nil {
		JSONError(c, http.StatusForbidden, err)
		return
	}
	value, err := h.models.BuildExecutionManifest(c.Request.Context())
	if err != nil {
		JSONError(c, 503, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, value)
}
func (h *ClusterHandlers) Resolve(c *gin.Context) {
	key, _ := contexts.GetAPIKey(c.Request.Context())
	route, err := h.models.ExecutionRouteForKey(c.Request.Context(), key, c.Query("model"))
	if err != nil {
		JSONError(c, 503, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	status := http.StatusOK
	if route.Status == "unavailable" {
		status = 404
	}
	if route.Status == "ambiguous" {
		status = 409
	}
	c.JSON(status, route)
}
func (h *ClusterHandlers) ChannelTest(c *gin.Context) {
	if err := authz.RequireScope(c.Request.Context(), scopes.ScopeReadChannels); err != nil {
		JSONError(c, http.StatusForbidden, err)
		return
	}
	var input orchestrator.OwnerTestRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	if err := c.ShouldBindJSON(&input); err != nil {
		JSONError(c, 400, err)
		return
	}
	ctx := contexts.WithSource(c.Request.Context(), request.SourceTest)
	value, err := h.tester.ExecuteOwnerTest(ctx, input)
	if err != nil {
		JSONError(c, 400, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, value)
}

func (h *ClusterHandlers) ChannelHealth(c *gin.Context) {
	var input biz.OwnerChannelHealthRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	if err := c.ShouldBindJSON(&input); err != nil {
		JSONError(c, 400, err)
		return
	}
	if !h.channels.OwnsChannel(input.ChannelID) {
		JSONError(c, 409, h.channels.ChannelOwnerError(input.ChannelID))
		return
	}
	value, err := h.channels.RunChannelAPIKeyHealthCheck(c.Request.Context(), input.ChannelID, input.KeyIDs, input.Modes...)
	if err != nil {
		JSONError(c, 400, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(200, value)
}

// DataNodeRoutes confines control-plane entry points to the designated node.
// Internal execution endpoints authenticate the original caller with unchanged
// scopes. Only management surfaces accept explicit channel selection.
func DataNodeRoutes(node cluster.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if strings.HasPrefix(path, "/v1/axon/") || strings.HasPrefix(path, "/__axon/") || path == "/admin/cluster/manifest" {
			c.Header("Cache-Control", "no-store")
		}
		if node.IsControl() {
			c.Next()
			return
		}
		if path == "/health" || path == "/__portway/capabilities" || path == "/__axon/admin/channel-quota-reset" || path == "/__axon/openapi/channel-quota-reset" || path == "/__axon/admin/channel-test" || path == "/__axon/admin/channel-health" || path == "/__axon/openapi/channel-test" || path == "/__axon/openapi/channel-health" || path == "/__axon/admin/playground/chat" || path == "/__axon/admin/channel-models" || path == "/__axon/admin/channel-sync" || path == "/__axon/openapi/channel-models" || path == "/__axon/openapi/channel-sync" || strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/v1beta/") || strings.HasPrefix(path, "/jina/") || strings.HasPrefix(path, "/doubao/") || strings.HasPrefix(path, "/gemini/") || strings.HasPrefix(path, "/anthropic/") {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(404, gin.H{"error": gin.H{"code": "control_node_required", "message": "Use the control-node endpoint."}, "execution": "not_executed"})
	}
}
