package api

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/internal/server/biz"
)

// TaskRoute is a pre-execution lookup for GET/DELETE only. Provider IDs are opaque;
// placement always comes from the authorized persisted Request.channel_id.
func (h *ClusterHandlers) TaskRoute(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	method, protocol, id := c.Query("method"), c.Query("protocol"), c.Query("task_id")
	if protocol != "openai" && protocol != "doubao" {
		c.JSON(400, gin.H{"error": "invalid task protocol", "execution": "not_executed"})
		return
	}
	if (method != http.MethodGet && method != http.MethodDelete) || id == "" {
		c.JSON(400, gin.H{"error": "invalid task operation", "execution": "not_executed"})
		return
	}
	task, err := h.videos.AuthorizedTask(c.Request.Context(), id)
	if err != nil {
		c.JSON(404, gin.H{"error": "task unavailable", "execution": "not_executed"})
		return
	}
	owner := h.channels.NodeConfig().Owner(task.ChannelID)
	if owner == "" {
		c.JSON(503, gin.H{"error": "task execution owner unavailable", "execution": "not_executed"})
		return
	}
	c.JSON(200, gin.H{"owner": owner, "status": "ready"})
}

// proxyTaskToOwner keeps direct/control-node callers working as well as edge
// callers. A single RoundTrip streams the original response. Unknown delivery is
// terminal; neither DELETE nor GET is replayed on another node.
func proxyTaskToOwner(c *gin.Context, service *biz.VideoService) bool {
	node := service.ChannelService.NodeConfig()
	if !node.Enabled {
		return false
	}
	c.Header("Cache-Control", "no-store")
	task, err := service.AuthorizedTask(c.Request.Context(), c.Param("id"))
	if err != nil {
		c.JSON(404, gin.H{"error": "task unavailable", "execution": "not_executed"})
		return true
	}
	if service.ChannelService.OwnsChannel(task.ChannelID) {
		return false
	}
	proxyExecutionOwner(c, node.Nodes[node.Owner(task.ChannelID)])
	return true
}

func proxyExecutionOwner(c *gin.Context, origin string) {
	target, err := url.Parse(origin)
	if err != nil || target.Host == "" {
		c.JSON(503, gin.H{"error": "execution owner unavailable", "execution": "not_executed"})
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		_, _ = fmt.Fprint(w, `{"error":"execution owner delivery unknown; do not replay"}`)
	}
	proxy.ServeHTTP(c.Writer, c.Request)
}
