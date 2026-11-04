package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
)

func (h *ClusterHandlers) ChannelModels(c *gin.Context) {
	if err := authz.RequireScope(c.Request.Context(), scopes.ScopeWriteChannels); err != nil {
		JSONError(c, 403, err)
		return
	}
	var input biz.FetchModelsInput
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	if err := c.ShouldBindJSON(&input); err != nil {
		JSONError(c, 400, err)
		return
	}
	result, err := h.channels.FetchModelsOnOwner(c.Request.Context(), input)
	if err != nil {
		JSONError(c, 400, err)
		return
	}
	c.JSON(200, result)
}

func (h *ClusterHandlers) ChannelSync(c *gin.Context) {
	if err := authz.RequireScope(c.Request.Context(), scopes.ScopeWriteChannels); err != nil {
		JSONError(c, 403, err)
		return
	}
	var input biz.OwnerChannelSyncRequest
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	if err := c.ShouldBindJSON(&input); err != nil {
		JSONError(c, 400, err)
		return
	}
	if !h.channels.OwnsChannel(input.ChannelID) {
		JSONError(c, 409, h.channels.ChannelOwnerError(input.ChannelID))
		return
	}
	if _, err := h.channels.SyncChannelModels(c.Request.Context(), input.ChannelID, input.Pattern); err != nil {
		JSONError(c, 400, err)
		return
	}
	c.JSON(200, gin.H{"success": true})
}
