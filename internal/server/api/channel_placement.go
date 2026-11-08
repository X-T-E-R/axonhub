package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/objects"
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
	override := make(map[int]string, len(value.Channels))
	focus := make(map[int]bool, len(value.Channels))
	for _, peer := range value.Channels {
		override[peer.ID] = value.To
		focus[peer.ID] = true
	}
	value.ModelRoutes, err = h.models.ModelMachineNotices(c.Request.Context(), biz.ModelRouteQuery{OwnerOverride: override, Focus: focus})
	if err != nil {
		JSONError(c, http.StatusServiceUnavailable, err)
		return
	}
	if value.ModelRoutes == nil {
		value.ModelRoutes = []biz.ModelMachineNotice{}
	}
	// Only the target preparation RPC needs stable internal identity labels.
	value.Identities = nil
	c.JSON(http.StatusOK, value)
}

func (h *ClusterHandlers) ModelRoutes(c *gin.Context) {
	if !placementScope(c, scopes.ScopeReadChannels) {
		return
	}
	query := biz.ModelRouteQuery{}
	if raw := c.Query("channel_id"); raw != "" {
		id, err := strconv.Atoi(raw)
		if err != nil || id <= 0 {
			JSONError(c, http.StatusBadRequest, errors.New("invalid channel ID"))
			return
		}
		query.Focus = map[int]bool{id: true}
		switch c.Query("status") {
		case "disabled":
			query.ExcludeChannel = id
		case "enabled":
			channel, err := h.channels.GetChannel(c.Request.Context(), id)
			if err != nil {
				JSONError(c, http.StatusBadRequest, err)
				return
			}
			query.Include = channel
		default:
			JSONError(c, http.StatusBadRequest, errors.New("status must be enabled or disabled"))
			return
		}
	}
	routes, err := h.models.ModelMachineNotices(c.Request.Context(), query)
	if err != nil {
		JSONError(c, http.StatusServiceUnavailable, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"model_routes": routes})
}

func (h *ClusterHandlers) PreviewModelRoutes(c *gin.Context) {
	if !placementScope(c, scopes.ScopeReadChannels) {
		return
	}
	var input struct {
		Model        string                       `json:"model"`
		Associations []*objects.ModelAssociation  `json:"associations"`
		Structural   bool                         `json:"structural"`
	}
	if !bindPlacement(c, &input) {
		return
	}
	if len(input.Associations) > 10 {
		JSONError(c, http.StatusBadRequest, errors.New("at most 10 associations"))
		return
	}
	routes, err := h.models.ModelMachineNotices(c.Request.Context(), biz.ModelRouteQuery{
		DraftModel: input.Model, Draft: input.Associations, UseDraft: true, Structural: input.Structural, Focus: map[int]bool{},
	})
	if err != nil {
		JSONError(c, http.StatusServiceUnavailable, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"model_routes": routes})
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
