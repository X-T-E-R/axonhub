package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

type SelfServiceHandlersParams struct {
	fx.In

	APIKeyService *biz.APIKeyService
}

type SelfServiceHandlers struct {
	APIKeyService *biz.APIKeyService
}

func NewSelfServiceHandlers(params SelfServiceHandlersParams) *SelfServiceHandlers {
	return &SelfServiceHandlers{APIKeyService: params.APIKeyService}
}

type createMyAPIKeyRequest struct {
	ProjectID     string `json:"projectId"`
	Name          string `json:"name" binding:"required"`
	PresetID      string `json:"presetId"`
	AccessGroupID string `json:"accessGroupId"`
	ProfileID     string `json:"profileId"`
}

type updateMyAPIKeyRequest struct {
	Name *string `json:"name"`
}

type updateMyAPIKeyStatusRequest struct {
	Status string `json:"status" binding:"required"`
}

type addChannelsToAccessGroupRequest struct {
	ChannelIDs []string `json:"channelIds" binding:"required"`
}

func (h *SelfServiceHandlers) ListAPIKeys(c *gin.Context) {
	projectID, ok := h.projectIDFromQuery(c)
	if !ok {
		return
	}

	items, err := h.APIKeyService.ListMyAPIKeys(c.Request.Context(), projectID)
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": items})
}

func (h *SelfServiceHandlers) ListRoutingPresets(c *gin.Context) {
	projectID, ok := h.projectIDFromQuery(c)
	if !ok {
		return
	}

	items, err := h.APIKeyService.ListMyRoutingPresets(c.Request.Context(), projectID)
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": items})
}

func (h *SelfServiceHandlers) ListAccessGroups(c *gin.Context) {
	projectID, ok := h.projectIDFromQuery(c)
	if !ok {
		return
	}

	items, err := h.APIKeyService.ListMyAccessGroups(c.Request.Context(), projectID)
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": items})
}

func (h *SelfServiceHandlers) CreateAPIKey(c *gin.Context) {
	var req createMyAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("Invalid request format"))
		return
	}

	projectID, ok := parseOptionalEntityID(c, req.ProjectID, "Project")
	if !ok {
		return
	}
	presetID, ok := h.accessGroupProfileID(c, req)
	if !ok {
		return
	}

	item, err := h.APIKeyService.CreateMyAPIKey(c.Request.Context(), biz.MyCreateAPIKeyInput{
		ProjectID: projectID,
		Name:      req.Name,
		PresetID:  presetID,
	})
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *SelfServiceHandlers) CreateAPIKeyForAccessGroup(c *gin.Context) {
	var req createMyAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("Invalid request format"))
		return
	}

	projectID, ok := parseOptionalEntityID(c, req.ProjectID, "Project")
	if !ok {
		return
	}
	presetID, ok := parseEntityID(c, c.Param("id"), "APIKeyProfileTemplate")
	if !ok {
		return
	}
	if req.ProfileID != "" {
		parsedProfileID, ok := parseEntityID(c, req.ProfileID, "APIKeyProfileTemplate")
		if !ok {
			return
		}
		if parsedProfileID != presetID {
			JSONError(c, http.StatusBadRequest, errors.New("profileId must belong to the selected access group"))
			return
		}
	}

	item, err := h.APIKeyService.CreateMyAPIKey(c.Request.Context(), biz.MyCreateAPIKeyInput{
		ProjectID: projectID,
		Name:      req.Name,
		PresetID:  presetID,
	})
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *SelfServiceHandlers) UpdateAPIKey(c *gin.Context) {
	id, ok := parseEntityID(c, c.Param("id"), "APIKey")
	if !ok {
		return
	}

	var req updateMyAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("Invalid request format"))
		return
	}

	item, err := h.APIKeyService.UpdateMyAPIKey(c.Request.Context(), id, biz.MyUpdateAPIKeyInput{Name: req.Name})
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *SelfServiceHandlers) UpdateAPIKeyStatus(c *gin.Context) {
	id, ok := parseEntityID(c, c.Param("id"), "APIKey")
	if !ok {
		return
	}

	var req updateMyAPIKeyStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("Invalid request format"))
		return
	}

	status := apikey.Status(req.Status)
	item, err := h.APIKeyService.UpdateMyAPIKeyStatus(c.Request.Context(), id, status)
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *SelfServiceHandlers) RotateAPIKey(c *gin.Context) {
	id, ok := parseEntityID(c, c.Param("id"), "APIKey")
	if !ok {
		return
	}

	item, err := h.APIKeyService.RotateMyAPIKey(c.Request.Context(), id)
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *SelfServiceHandlers) ListModels(c *gin.Context) {
	projectID, ok := h.projectIDFromQuery(c)
	if !ok {
		return
	}

	var presetID *int
	if raw := c.Query("preset_id"); raw != "" {
		id, ok := parseEntityID(c, raw, "APIKeyProfileTemplate")
		if !ok {
			return
		}
		presetID = &id
	}

	items, err := h.APIKeyService.ListMyModels(c.Request.Context(), projectID, presetID)
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": items})
}

func (h *SelfServiceHandlers) ListRequests(c *gin.Context) {
	projectID, ok := h.projectIDFromQuery(c)
	if !ok {
		return
	}

	limit := 50
	if raw := c.Query("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}

	items, err := h.APIKeyService.ListMyRequests(c.Request.Context(), projectID, limit)
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": items})
}

func (h *SelfServiceHandlers) Usage(c *gin.Context) {
	projectID, ok := h.projectIDFromQuery(c)
	if !ok {
		return
	}

	item, err := h.APIKeyService.MyUsage(c.Request.Context(), projectID)
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *SelfServiceHandlers) ListAdminAccessGroups(c *gin.Context) {
	projectID, ok := h.requiredProjectIDFromQuery(c)
	if !ok {
		return
	}
	h.withProjectID(c, projectID)

	items, err := h.APIKeyService.ListAdminAccessGroups(c.Request.Context(), projectID)
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": items})
}

func (h *SelfServiceHandlers) GetAdminAccessGroup(c *gin.Context) {
	id, ok := parseEntityID(c, c.Param("id"), "APIKeyProfileTemplate")
	if !ok {
		return
	}
	if !h.withAccessGroupProjectID(c, id) {
		return
	}

	item, err := h.APIKeyService.GetAdminAccessGroup(c.Request.Context(), id)
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *SelfServiceHandlers) AddChannelsToAccessGroup(c *gin.Context) {
	id, ok := parseEntityID(c, c.Param("id"), "APIKeyProfileTemplate")
	if !ok {
		return
	}
	if !h.withAccessGroupProjectID(c, id) {
		return
	}

	var req addChannelsToAccessGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("Invalid request format"))
		return
	}

	channelIDs := make([]int, 0, len(req.ChannelIDs))
	for _, raw := range req.ChannelIDs {
		channelID, ok := parseEntityID(c, raw, "Channel")
		if !ok {
			return
		}
		channelIDs = append(channelIDs, channelID)
	}

	item, err := h.APIKeyService.AddChannelsToAccessGroup(c.Request.Context(), id, channelIDs)
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *SelfServiceHandlers) projectIDFromQuery(c *gin.Context) (int, bool) {
	raw := c.Query("project_id")
	if raw == "" {
		return 0, true
	}

	return parseEntityID(c, raw, "Project")
}

func (h *SelfServiceHandlers) requiredProjectIDFromQuery(c *gin.Context) (int, bool) {
	raw := c.Query("project_id")
	if raw == "" {
		JSONError(c, http.StatusBadRequest, errors.New("project_id is required"))
		return 0, false
	}

	return parseEntityID(c, raw, "Project")
}

func (h *SelfServiceHandlers) accessGroupProfileID(c *gin.Context, req createMyAPIKeyRequest) (int, bool) {
	raw := req.ProfileID
	if raw == "" {
		raw = req.AccessGroupID
	}
	if raw == "" {
		raw = req.PresetID
	}
	if raw == "" {
		JSONError(c, http.StatusBadRequest, errors.New("accessGroupId or profileId is required"))
		return 0, false
	}

	return parseEntityID(c, raw, "APIKeyProfileTemplate")
}

func parseOptionalEntityID(c *gin.Context, raw string, typ string) (int, bool) {
	if raw == "" {
		return 0, true
	}

	return parseEntityID(c, raw, typ)
}

func parseEntityID(c *gin.Context, raw string, typ string) (int, bool) {
	if raw == "" {
		JSONError(c, http.StatusBadRequest, errors.New("id is required"))
		return 0, false
	}

	if id, err := strconv.Atoi(raw); err == nil {
		return id, true
	}

	guid, err := objects.ParseGUID(raw)
	if err != nil || guid.Type != typ {
		JSONError(c, http.StatusBadRequest, errors.New("invalid id"))
		return 0, false
	}

	return guid.ID, true
}

func selfServiceErrorStatus(err error) int {
	if errors.Is(err, biz.ErrSelfServiceDisabled) {
		return http.StatusForbidden
	}

	return http.StatusForbidden
}

func (h *SelfServiceHandlers) withProjectID(c *gin.Context, projectID int) {
	if projectID <= 0 {
		return
	}

	c.Request = c.Request.WithContext(contexts.WithProjectID(c.Request.Context(), projectID))
}

func (h *SelfServiceHandlers) withAccessGroupProjectID(c *gin.Context, accessGroupID int) bool {
	projectID, err := h.APIKeyService.ResolveAccessGroupProjectID(c.Request.Context(), accessGroupID)
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return false
	}

	h.withProjectID(c, projectID)
	return true
}
