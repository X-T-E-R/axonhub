package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
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

type classifyLegacyAPIKeyRequest struct {
	Mode          string `json:"mode" binding:"required"`
	AccessGroupID string `json:"accessGroupId"`
}

type addChannelsToAccessGroupRequest struct {
	ChannelIDs []string `json:"channelIds"`
}

type upsertAdminAccessGroupRequest struct {
	ProjectID            string    `json:"projectId"`
	Name                 *string   `json:"name"`
	Description          *string   `json:"description"`
	SelfServiceVisible   *bool     `json:"selfServiceVisible"`
	ModelIDs             *[]string `json:"modelIds"`
	ChannelIDs           *[]string `json:"channelIds"`
	ChannelTags          *[]string `json:"channelTags"`
	ChannelTagsMatchMode *string   `json:"channelTagsMatchMode"`
	LoadBalanceStrategy  *string   `json:"loadBalanceStrategy"`
	ClearLoadBalance     bool      `json:"clearLoadBalance"`
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

func (h *SelfServiceHandlers) RevealAPIKey(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Header("Pragma", "no-cache")
	id, ok := parseEntityID(c, c.Param("id"), "APIKey")
	if !ok {
		return
	}
	item, err := h.APIKeyService.RevealMyAPIKey(c.Request.Context(), id)
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}
	c.JSON(http.StatusOK, item)
}

func (h *SelfServiceHandlers) ClassifyLegacyAPIKey(c *gin.Context) {
	h.classifyLegacyAPIKey(c, false)
}

func (h *SelfServiceHandlers) ClassifyMyLegacyAPIKey(c *gin.Context) {
	h.classifyLegacyAPIKey(c, true)
}

func (h *SelfServiceHandlers) classifyLegacyAPIKey(c *gin.Context, selfOnly bool) {
	id, ok := parseEntityID(c, c.Param("id"), "APIKey")
	if !ok {
		return
	}
	var req classifyLegacyAPIKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("Invalid request format"))
		return
	}
	var groupID *int
	if req.AccessGroupID != "" {
		parsed, ok := parseEntityID(c, req.AccessGroupID, "APIKeyProfileTemplate")
		if !ok {
			return
		}
		groupID = &parsed
	}
	var item biz.MyAPIKeySummary
	var err error
	if selfOnly {
		item, err = h.APIKeyService.ClassifyMyLegacyAPIKey(c.Request.Context(), id, biz.LegacyAPIKeyClassificationInput{Mode: req.Mode, AccessGroupID: groupID})
	} else {
		item, err = h.APIKeyService.ClassifyLegacyAPIKey(c.Request.Context(), id, biz.LegacyAPIKeyClassificationInput{Mode: req.Mode, AccessGroupID: groupID})
	}
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *SelfServiceHandlers) DetachAPIKeyAccessGroup(c *gin.Context) {
	id, ok := parseEntityID(c, c.Param("id"), "APIKey")
	if !ok {
		return
	}
	item, err := h.APIKeyService.DetachAPIKeyAccessGroup(c.Request.Context(), id)
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": item})
}

func (h *SelfServiceHandlers) GetRequest(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	id, ok := parseEntityID(c, c.Param("id"), "Request")
	if !ok {
		return
	}
	item, err := h.APIKeyService.GetMyRequestDetail(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, biz.ErrSelfServiceRequestDetailsDisabled) {
			_ = c.Error(err)
			c.JSON(http.StatusForbidden, objects.ErrorResponse{Error: objects.Error{Type: "SELF_SERVICE_REQUEST_DETAILS_DISABLED", Message: err.Error()}})
			return
		}
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

func (h *SelfServiceHandlers) CreateAdminAccessGroup(c *gin.Context) {
	var req upsertAdminAccessGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("Invalid request format"))
		return
	}

	projectID, ok := h.projectIDFromRequest(c, req.ProjectID)
	if !ok {
		return
	}
	h.withProjectID(c, projectID)

	input, ok := h.adminAccessGroupInput(c, req)
	if !ok {
		return
	}
	input.ProjectID = projectID

	item, err := h.APIKeyService.CreateAdminAccessGroup(c.Request.Context(), input)
	if err != nil {
		JSONError(c, selfServiceErrorStatus(err), err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": item})
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

func (h *SelfServiceHandlers) UpdateAdminAccessGroup(c *gin.Context) {
	id, ok := parseEntityID(c, c.Param("id"), "APIKeyProfileTemplate")
	if !ok {
		return
	}
	if !h.withAccessGroupProjectID(c, id) {
		return
	}

	var req upsertAdminAccessGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("Invalid request format"))
		return
	}

	input, ok := h.adminAccessGroupInput(c, req)
	if !ok {
		return
	}

	item, err := h.APIKeyService.UpdateAdminAccessGroup(c.Request.Context(), id, input)
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
	if req.ChannelIDs == nil {
		JSONError(c, http.StatusBadRequest, errors.New("channelIds is required"))
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

func (h *SelfServiceHandlers) projectIDFromRequest(c *gin.Context, rawProjectID string) (int, bool) {
	if rawProjectID != "" {
		return parseEntityID(c, rawProjectID, "Project")
	}

	return h.requiredProjectIDFromQuery(c)
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

func (h *SelfServiceHandlers) adminAccessGroupInput(c *gin.Context, req upsertAdminAccessGroupRequest) (biz.AdminAccessGroupInput, bool) {
	channelIDs, ok := parseOptionalEntityIDs(c, req.ChannelIDs, "Channel")
	if !ok {
		return biz.AdminAccessGroupInput{}, false
	}

	input := biz.AdminAccessGroupInput{
		Name:                 req.Name,
		Description:          req.Description,
		SelfServiceVisible:   req.SelfServiceVisible,
		ModelIDs:             req.ModelIDs,
		ChannelIDs:           channelIDs,
		ChannelTags:          req.ChannelTags,
		ChannelTagsMatchMode: req.ChannelTagsMatchMode,
		LoadBalanceStrategy:  req.LoadBalanceStrategy,
		ClearLoadBalance:     req.ClearLoadBalance,
	}

	return input, true
}

func parseOptionalEntityIDs(c *gin.Context, rawIDs *[]string, typ string) (*[]int, bool) {
	if rawIDs == nil {
		return nil, true
	}

	ids := make([]int, 0, len(*rawIDs))
	for _, raw := range *rawIDs {
		id, ok := parseEntityID(c, raw, typ)
		if !ok {
			return nil, false
		}
		ids = append(ids, id)
	}

	return &ids, true
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
	if errors.Is(err, biz.ErrAPIKeyArchived) {
		return http.StatusGone
	}
	if errors.Is(err, biz.ErrNotFoundOrNotAuthorized) || ent.IsNotFound(err) {
		return http.StatusNotFound
	}
	if errors.Is(err, biz.ErrSelfServiceDisabled) {
		return http.StatusForbidden
	}
	if errors.Is(err, biz.ErrSelfServiceRequestDetailsDisabled) {
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
