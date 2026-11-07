package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
)

func placementID(c *gin.Context) (int, bool) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		JSONError(c, http.StatusBadRequest, errors.New("invalid channel ID"))
		return 0, false
	}
	return id, true
}

func bindPlacement(c *gin.Context, input any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	if err := c.ShouldBindJSON(input); err != nil {
		JSONError(c, http.StatusBadRequest, err)
		return false
	}
	return true
}

func placementScope(c *gin.Context, scope scopes.ScopeSlug) bool {
	c.Header("Cache-Control", "no-store")
	if err := authz.RequireScope(c.Request.Context(), scope); err != nil {
		JSONError(c, http.StatusForbidden, err)
		return false
	}
	return true
}

func (h *ClusterHandlers) ObserveChannelPlacement(c *gin.Context) {
	if !placementScope(c, scopes.ScopeReadChannels) {
		return
	}
	id, ok := placementID(c)
	if !ok {
		return
	}
	value, err := h.channels.ObservePlacement(c.Request.Context(), id)
	if err != nil {
		JSONError(c, http.StatusServiceUnavailable, err)
		return
	}
	c.JSON(http.StatusOK, value)
}

func (h *ClusterHandlers) PreviewChannelPlacement(c *gin.Context) {
	if !placementScope(c, scopes.ScopeWriteChannels) {
		return
	}
	id, ok := placementID(c)
	if !ok {
		return
	}
	value, err := h.channels.PreviewPlacement(c.Request.Context(), id, c.Query("target"))
	if err != nil {
		JSONError(c, http.StatusBadRequest, err)
		return
	}
	// Only the target preparation RPC needs stable internal identity labels.
	value.Identities = nil
	c.JSON(http.StatusOK, value)
}

func (h *ClusterHandlers) SaveChannelPlacement(c *gin.Context) {
	if !placementScope(c, scopes.ScopeWriteChannels) {
		return
	}
	var input biz.PlacementSaveInput
	if !bindPlacement(c, &input) {
		return
	}
	value, err := h.channels.SavePlacement(c.Request.Context(), input)
	if err != nil {
		JSONError(c, http.StatusConflict, err)
		return
	}
	c.JSON(http.StatusOK, value)
}

func (h *ClusterHandlers) PrepareChannelPlacement(c *gin.Context) {
	if !placementScope(c, scopes.ScopeWriteChannels) {
		return
	}
	var input biz.PlacementPlan
	if !bindPlacement(c, &input) {
		return
	}
	value, err := h.channels.PreparePlacement(c.Request.Context(), input)
	if err != nil {
		JSONError(c, http.StatusConflict, err)
		return
	}
	c.JSON(http.StatusOK, value)
}

func (h *ClusterHandlers) ReadChannelPlacementOnNode(c *gin.Context) {
	if !placementScope(c, scopes.ScopeReadChannels) {
		return
	}
	var input biz.PlacementReadbackInput
	if !bindPlacement(c, &input) {
		return
	}
	if input.Refresh && !placementScope(c, scopes.ScopeWriteChannels) {
		return
	}
	value, err := h.channels.ReadPlacementOnNode(c.Request.Context(), input)
	if err != nil {
		JSONError(c, http.StatusServiceUnavailable, err)
		return
	}
	c.JSON(http.StatusOK, value)
}

func (h *ClusterHandlers) AcknowledgePlacementWorker(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	user, ok := contexts.GetUser(c.Request.Context())
	if !ok || !user.IsOwner {
		JSONError(c, http.StatusForbidden, errors.New("owner access required"))
		return
	}
	var input biz.PlacementWorkerAck
	if !bindPlacement(c, &input) {
		return
	}
	if err := h.channels.AcknowledgePlacementWorker(c.Request.Context(), input); err != nil {
		JSONError(c, http.StatusConflict, err)
		return
	}
	c.JSON(http.StatusOK, true)
}
