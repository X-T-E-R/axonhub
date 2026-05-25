package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
)

type ManagementHandlersParams struct {
	fx.In

	Ent                 *ent.Client
	APIKeyService       *biz.APIKeyService
	ChannelService      *biz.ChannelService
	PermissionValidator *biz.PermissionValidator `optional:"true"`
}

type ManagementHandlers struct {
	ent                 *ent.Client
	apiKeyService       *biz.APIKeyService
	channelService      *biz.ChannelService
	permissionValidator *biz.PermissionValidator
}

func NewManagementHandlers(params ManagementHandlersParams) *ManagementHandlers {
	permissionValidator := params.PermissionValidator
	if permissionValidator == nil {
		permissionValidator = biz.NewPermissionValidator()
	}

	return &ManagementHandlers{
		ent:                 params.Ent,
		apiKeyService:       params.APIKeyService,
		channelService:      params.ChannelService,
		permissionValidator: permissionValidator,
	}
}

func (h *ManagementHandlers) RegisterAdminRoutes(group *gin.RouterGroup) {
	tokens := group.Group("/management/tokens")
	tokens.POST("", h.CreateManagementToken)
	tokens.GET("", h.ListManagementTokens)
	tokens.GET("/:id", h.GetManagementToken)
	tokens.PATCH("/:id", h.UpdateManagementToken)
	tokens.POST("/:id/revoke", h.RevokeManagementToken)
}

func (h *ManagementHandlers) RegisterOpenAPIRoutes(group *gin.RouterGroup) {
	management := group.Group("/v1/management")
	management.GET("/capabilities", h.GetManagementCapabilities)
	management.GET("/channels", h.ListManagementChannels)
	management.GET("/channels/:id", h.GetManagementChannel)
	management.PATCH("/channels/:id/status", h.UpdateManagementChannelStatus)
	management.GET("/channels/:id/keys", h.ListManagementChannelKeys)
	management.POST("/channels/:id/keys", h.AddManagementChannelKeys)
	management.DELETE("/channels/:id/keys/:key_id", h.DeleteManagementChannelKey)
	management.POST("/channels/:id/keys/:key_id/disable", h.DisableManagementChannelKey)
	management.POST("/channels/:id/keys/:key_id/enable", h.EnableManagementChannelKey)
	management.POST("/channels/:id/keys/:key_id/archive", h.ArchiveManagementChannelKey)
	management.POST("/channels/:id/keys/:key_id/restore", h.RestoreManagementChannelKey)
	management.POST("/channels/:id/keys/health-check", h.RunManagementChannelKeyHealthCheck)
}

type managementTokenResponse struct {
	ID        int           `json:"id"`
	Name      string        `json:"name"`
	Type      apikey.Type   `json:"type"`
	Status    apikey.Status `json:"status"`
	ProjectID int           `json:"projectId"`
	Scopes    []string      `json:"scopes"`
	MaskedKey string        `json:"maskedKey"`
	Key       string        `json:"key,omitempty"`
	CreatedAt time.Time     `json:"createdAt"`
	UpdatedAt time.Time     `json:"updatedAt"`
}

type createManagementTokenRequest struct {
	Name      string   `json:"name"`
	ProjectID int      `json:"projectId"`
	Scopes    []string `json:"scopes"`
}

type updateManagementTokenRequest struct {
	Name   *string  `json:"name"`
	Scopes []string `json:"scopes"`
	Status *string  `json:"status"`
}

type managementScopeResponse struct {
	Slug        string   `json:"slug"`
	Description string   `json:"description"`
	Levels      []string `json:"levels"`
}

type managementCapabilitiesResponse struct {
	Scopes     []managementScopeResponse `json:"scopes"`
	Operations map[string]string         `json:"operations"`
}

type managementChannelKeyCounts struct {
	Total    int `json:"total"`
	Active   int `json:"active"`
	Disabled int `json:"disabled"`
	Archived int `json:"archived"`
}

type managementChannelResponse struct {
	ID        int                        `json:"id"`
	Name      string                     `json:"name"`
	Type      channel.Type               `json:"type"`
	Status    channel.Status             `json:"status"`
	BaseURL   string                     `json:"baseUrl"`
	Models    []string                   `json:"models"`
	Tags      []string                   `json:"tags"`
	KeyCounts managementChannelKeyCounts `json:"keyCounts"`
	UpdatedAt time.Time                  `json:"updatedAt"`
}

type managementChannelKeyInventoryItem struct {
	ID             string                                      `json:"id"`
	MaskedKey      string                                      `json:"maskedKey"`
	Status         objects.ChannelKeyStatus                    `json:"status"`
	LastCheckedAt  *time.Time                                  `json:"lastCheckedAt,omitempty"`
	Success        *bool                                       `json:"success,omitempty"`
	FailureCount   int                                         `json:"failureCount"`
	Reason         string                                      `json:"reason,omitempty"`
	Balance        any                                         `json:"balance,omitempty"`
	Currency       string                                      `json:"currency,omitempty"`
	Available      *bool                                       `json:"available,omitempty"`
	StatusCode     int                                         `json:"statusCode,omitempty"`
	MatchedPolicy  string                                      `json:"matchedPolicy,omitempty"`
	Action         string                                      `json:"action,omitempty"`
	NextCheckAt    *time.Time                                  `json:"nextCheckAt,omitempty"`
	BackoffAttempt int                                         `json:"backoffAttempt,omitempty"`
	History        []objects.ChannelKeyHealthCheckHistoryEntry `json:"history,omitempty"`
}

type managementChannelKeyInventoryResponse struct {
	Items []*managementChannelKeyInventoryItem `json:"items"`
}

type addManagementChannelKeysRequest struct {
	Keys           []string `json:"keys"`
	RunHealthCheck bool     `json:"runHealthCheck"`
}

type reasonRequest struct {
	Reason string `json:"reason"`
}

type updateChannelStatusRequest struct {
	Status string `json:"status"`
}

type healthCheckRequest struct {
	KeyIDs []string `json:"keyIds"`
}

type deleteKeyResponse struct {
	Success bool                                 `json:"success"`
	Message string                               `json:"message,omitempty"`
	Items   []*managementChannelKeyInventoryItem `json:"items"`
}

func (h *ManagementHandlers) CreateManagementToken(c *gin.Context) {
	var req createManagementTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("invalid request format"))
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		JSONError(c, http.StatusBadRequest, errors.New("token name cannot be empty"))
		return
	}
	if req.ProjectID <= 0 {
		JSONError(c, http.StatusBadRequest, errors.New("projectId must be positive"))
		return
	}

	ctx, ok := h.requireScope(c, scopes.ScopeWriteAPIKeys, req.ProjectID)
	if !ok {
		return
	}
	projectExists, err := h.projectExists(ctx, req.ProjectID)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	if !projectExists {
		JSONError(c, http.StatusBadRequest, errors.New("invalid projectId"))
		return
	}

	tokenScopes, err := normalizeManagementScopes(req.Scopes)
	if err != nil {
		JSONError(c, http.StatusBadRequest, err)
		return
	}
	if err := h.validateGrantAuthority(ctx, tokenScopes, req.ProjectID); err != nil {
		JSONError(c, http.StatusForbidden, err)
		return
	}

	keyType := apikey.TypeServiceAccount
	token, err := h.apiKeyService.CreateAPIKey(ctx, ent.CreateAPIKeyInput{
		Name:      req.Name,
		Type:      &keyType,
		Scopes:    tokenScopes,
		ProjectID: req.ProjectID,
	})
	if err != nil {
		h.writeServiceError(c, err)
		return
	}

	c.JSON(http.StatusCreated, tokenResponse(token, true))
}

func (h *ManagementHandlers) ListManagementTokens(c *gin.Context) {
	projectID, ok := h.projectIDFromRequest(c, "projectId")
	if !ok {
		JSONError(c, http.StatusBadRequest, errors.New("projectId or X-Project-ID is required"))
		return
	}

	ctx, ok := h.requireScope(c, scopes.ScopeReadAPIKeys, projectID)
	if !ok {
		return
	}

	q := h.ent.APIKey.Query().
		Where(apikey.TypeEQ(apikey.TypeServiceAccount), apikey.ProjectIDEQ(projectID)).
		Order(ent.Desc(apikey.FieldCreatedAt))
	if status := strings.TrimSpace(c.Query("status")); status != "" {
		apiKeyStatus := apikey.Status(status)
		if err := apikey.StatusValidator(apiKeyStatus); err != nil {
			JSONError(c, http.StatusBadRequest, fmt.Errorf("invalid status: %s", status))
			return
		}
		q = q.Where(apikey.StatusEQ(apiKeyStatus))
	}

	tokens, err := q.All(ctx)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}

	out := make([]managementTokenResponse, 0, len(tokens))
	for _, token := range tokens {
		out = append(out, tokenResponse(token, false))
	}

	c.JSON(http.StatusOK, gin.H{"items": out})
}

func (h *ManagementHandlers) GetManagementToken(c *gin.Context) {
	id, ok := parsePositiveParam(c, "id")
	if !ok {
		return
	}

	projectID, hasProject := h.projectIDFromRequest(c, "projectId")
	if !hasProject && !currentUserIsOwner(c) {
		JSONError(c, http.StatusBadRequest, errors.New("projectId or X-Project-ID is required"))
		return
	}

	ctx, ok := h.requireScope(c, scopes.ScopeReadAPIKeys, projectID)
	if !ok {
		return
	}

	token, err := h.serviceAccountToken(ctx, id, projectID)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}

	c.JSON(http.StatusOK, tokenResponse(token, false))
}

func (h *ManagementHandlers) UpdateManagementToken(c *gin.Context) {
	id, ok := parsePositiveParam(c, "id")
	if !ok {
		return
	}

	projectID, hasProject := h.projectIDFromRequest(c, "projectId")
	if !hasProject && !currentUserIsOwner(c) {
		JSONError(c, http.StatusBadRequest, errors.New("projectId or X-Project-ID is required"))
		return
	}

	var req updateManagementTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("invalid request format"))
		return
	}

	ctx, ok := h.requireScope(c, scopes.ScopeWriteAPIKeys, projectID)
	if !ok {
		return
	}

	token, err := h.serviceAccountToken(ctx, id, projectID)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}

	if req.Name != nil {
		trimmed := strings.TrimSpace(*req.Name)
		if trimmed == "" {
			JSONError(c, http.StatusBadRequest, errors.New("token name cannot be empty"))
			return
		}
		req.Name = &trimmed
	}
	var status *apikey.Status
	if req.Status != nil {
		parsed := apikey.Status(strings.TrimSpace(*req.Status))
		if err := apikey.StatusValidator(parsed); err != nil {
			JSONError(c, http.StatusBadRequest, fmt.Errorf("invalid status: %s", *req.Status))
			return
		}
		status = &parsed
	}

	updateInput := ent.UpdateAPIKeyInput{Name: req.Name}
	if req.Scopes != nil {
		tokenScopes, err := normalizeManagementScopes(req.Scopes)
		if err != nil {
			JSONError(c, http.StatusBadRequest, err)
			return
		}
		if err := h.validateGrantAuthority(ctx, tokenScopes, token.ProjectID); err != nil {
			JSONError(c, http.StatusForbidden, err)
			return
		}
		updateInput.Scopes = tokenScopes
	}

	updated := token
	if req.Name != nil || req.Scopes != nil {
		updated, err = h.apiKeyService.UpdateAPIKey(ctx, id, updateInput)
		if err != nil {
			h.writeServiceError(c, err)
			return
		}
	}

	if status != nil {
		updated, err = h.apiKeyService.UpdateAPIKeyStatus(ctx, id, *status)
		if err != nil {
			h.writeServiceError(c, err)
			return
		}
	}

	c.JSON(http.StatusOK, tokenResponse(updated, false))
}

func (h *ManagementHandlers) RevokeManagementToken(c *gin.Context) {
	id, ok := parsePositiveParam(c, "id")
	if !ok {
		return
	}

	projectID, hasProject := h.projectIDFromRequest(c, "projectId")
	if !hasProject && !currentUserIsOwner(c) {
		JSONError(c, http.StatusBadRequest, errors.New("projectId or X-Project-ID is required"))
		return
	}

	ctx, ok := h.requireScope(c, scopes.ScopeWriteAPIKeys, projectID)
	if !ok {
		return
	}

	if _, err := h.serviceAccountToken(ctx, id, projectID); err != nil {
		h.writeServiceError(c, err)
		return
	}

	token, err := h.apiKeyService.UpdateAPIKeyStatus(ctx, id, apikey.StatusArchived)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}

	c.JSON(http.StatusOK, tokenResponse(token, false))
}

func (h *ManagementHandlers) GetManagementCapabilities(c *gin.Context) {
	scopeCatalog := scopes.AllScopes(nil)
	scopeResponses := make([]managementScopeResponse, 0, len(scopeCatalog)+1)
	for _, scope := range scopeCatalog {
		levels := make([]string, 0, len(scope.Levels))
		for _, level := range scope.Levels {
			levels = append(levels, string(level))
		}
		scopeResponses = append(scopeResponses, managementScopeResponse{
			Slug:        string(scope.Slug),
			Description: scope.Description,
			Levels:      levels,
		})
	}
	scopeResponses = append(scopeResponses, managementScopeResponse{
		Slug:        scopes.ScopeWildcard,
		Description: "All scopes",
		Levels:      []string{string(scopes.ScopeLevelSystem), string(scopes.ScopeLevelProject)},
	})

	c.JSON(http.StatusOK, managementCapabilitiesResponse{
		Scopes: scopeResponses,
		Operations: map[string]string{
			"GET /openapi/v1/management/capabilities":                       "authenticated",
			"GET /openapi/v1/management/channels":                           string(scopes.ScopeReadChannels),
			"GET /openapi/v1/management/channels/:id":                       string(scopes.ScopeReadChannels),
			"PATCH /openapi/v1/management/channels/:id/status":              string(scopes.ScopeWriteChannels),
			"GET /openapi/v1/management/channels/:id/keys":                  string(scopes.ScopeReadChannels),
			"POST /openapi/v1/management/channels/:id/keys":                 string(scopes.ScopeWriteChannels),
			"DELETE /openapi/v1/management/channels/:id/keys/:key_id":       string(scopes.ScopeWriteChannels),
			"POST /openapi/v1/management/channels/:id/keys/:key_id/disable": string(scopes.ScopeWriteChannels),
			"POST /openapi/v1/management/channels/:id/keys/:key_id/enable":  string(scopes.ScopeWriteChannels),
			"POST /openapi/v1/management/channels/:id/keys/:key_id/archive": string(scopes.ScopeWriteChannels),
			"POST /openapi/v1/management/channels/:id/keys/:key_id/restore": string(scopes.ScopeWriteChannels),
			"POST /openapi/v1/management/channels/:id/keys/health-check":    string(scopes.ScopeWriteChannels),
		},
	})
}

func (h *ManagementHandlers) ListManagementChannels(c *gin.Context) {
	ctx, ok := h.requireScope(c, scopes.ScopeReadChannels, 0)
	if !ok {
		return
	}

	channels, err := h.ent.Channel.Query().
		Order(ent.Desc(channel.FieldOrderingWeight), ent.Asc(channel.FieldID)).
		All(ctx)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}

	out := make([]managementChannelResponse, 0, len(channels))
	for _, ch := range channels {
		resp, err := h.channelResponse(ctx, ch)
		if err != nil {
			h.writeServiceError(c, err)
			return
		}
		out = append(out, resp)
	}

	c.JSON(http.StatusOK, gin.H{"items": out})
}

func (h *ManagementHandlers) GetManagementChannel(c *gin.Context) {
	id, ok := parsePositiveParam(c, "id")
	if !ok {
		return
	}
	ctx, ok := h.requireScope(c, scopes.ScopeReadChannels, 0)
	if !ok {
		return
	}

	ch, err := h.ent.Channel.Get(ctx, id)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}

	resp, err := h.channelResponse(ctx, ch)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *ManagementHandlers) UpdateManagementChannelStatus(c *gin.Context) {
	id, ok := parsePositiveParam(c, "id")
	if !ok {
		return
	}
	var req updateChannelStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("invalid request format"))
		return
	}
	status := channel.Status(strings.TrimSpace(req.Status))
	if err := channel.StatusValidator(status); err != nil {
		JSONError(c, http.StatusBadRequest, fmt.Errorf("invalid status: %s", req.Status))
		return
	}

	ctx, ok := h.requireScope(c, scopes.ScopeWriteChannels, 0)
	if !ok {
		return
	}

	ch, err := h.channelService.UpdateChannelStatus(ctx, id, status)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}

	resp, err := h.channelResponse(ctx, ch)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}
	c.JSON(http.StatusOK, resp)
}

func (h *ManagementHandlers) ListManagementChannelKeys(c *gin.Context) {
	id, ok := parsePositiveParam(c, "id")
	if !ok {
		return
	}
	ctx, ok := h.requireScope(c, scopes.ScopeReadChannels, 0)
	if !ok {
		return
	}

	h.writeInventory(c, ctx, id)
}

func (h *ManagementHandlers) AddManagementChannelKeys(c *gin.Context) {
	id, ok := parsePositiveParam(c, "id")
	if !ok {
		return
	}
	var req addManagementChannelKeysRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("invalid request format"))
		return
	}

	keys := normalizeProviderKeys(req.Keys)
	if len(keys) == 0 {
		JSONError(c, http.StatusBadRequest, errors.New("keys cannot be empty"))
		return
	}

	ctx, ok := h.requireScope(c, scopes.ScopeWriteChannels, 0)
	if !ok {
		return
	}

	for _, key := range keys {
		if err := h.channelService.AddChannelAPIKey(ctx, id, key); err != nil {
			h.writeServiceError(c, err)
			return
		}
	}

	if req.RunHealthCheck {
		items, err := h.channelService.RunChannelAPIKeyHealthCheck(ctx, id, nil)
		if err != nil {
			h.writeServiceError(c, err)
			return
		}
		c.JSON(http.StatusOK, managementChannelKeyInventoryResponse{Items: inventoryItemsResponse(items)})
		return
	}

	h.writeInventory(c, ctx, id)
}

func (h *ManagementHandlers) DeleteManagementChannelKey(c *gin.Context) {
	id, keyID, ok := parseChannelKeyParams(c)
	if !ok {
		return
	}
	ctx, ok := h.requireScope(c, scopes.ScopeWriteChannels, 0)
	if !ok {
		return
	}

	result, err := h.channelService.DeleteChannelAPIKey(ctx, id, keyID)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}

	items, err := h.channelService.ChannelAPIKeyInventory(ctx, id)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}

	c.JSON(http.StatusOK, deleteKeyResponse{
		Success: result == nil || result.Success,
		Message: resultMessage(result),
		Items:   inventoryItemsResponse(items),
	})
}

func (h *ManagementHandlers) DisableManagementChannelKey(c *gin.Context) {
	id, keyID, ok := parseChannelKeyParams(c)
	if !ok {
		return
	}
	ctx, ok := h.requireScope(c, scopes.ScopeWriteChannels, 0)
	if !ok {
		return
	}

	reason, ok := readOptionalReason(c)
	if !ok {
		return
	}
	if err := h.channelService.DisableAPIKey(ctx, id, keyID, 0, reason); err != nil {
		h.writeServiceError(c, err)
		return
	}

	h.writeInventory(c, ctx, id)
}

func (h *ManagementHandlers) EnableManagementChannelKey(c *gin.Context) {
	id, keyID, ok := parseChannelKeyParams(c)
	if !ok {
		return
	}
	ctx, ok := h.requireScope(c, scopes.ScopeWriteChannels, 0)
	if !ok {
		return
	}

	if err := h.channelService.EnableAPIKey(ctx, id, keyID); err != nil {
		h.writeServiceError(c, err)
		return
	}

	h.writeInventory(c, ctx, id)
}

func (h *ManagementHandlers) ArchiveManagementChannelKey(c *gin.Context) {
	id, keyID, ok := parseChannelKeyParams(c)
	if !ok {
		return
	}
	ctx, ok := h.requireScope(c, scopes.ScopeWriteChannels, 0)
	if !ok {
		return
	}

	reason, ok := readOptionalReason(c)
	if !ok {
		return
	}
	if err := h.channelService.ArchiveChannelAPIKey(ctx, id, keyID, reason); err != nil {
		h.writeServiceError(c, err)
		return
	}

	h.writeInventory(c, ctx, id)
}

func (h *ManagementHandlers) RestoreManagementChannelKey(c *gin.Context) {
	id, keyID, ok := parseChannelKeyParams(c)
	if !ok {
		return
	}
	ctx, ok := h.requireScope(c, scopes.ScopeWriteChannels, 0)
	if !ok {
		return
	}

	if err := h.channelService.RestoreChannelAPIKey(ctx, id, keyID); err != nil {
		h.writeServiceError(c, err)
		return
	}

	h.writeInventory(c, ctx, id)
}

func (h *ManagementHandlers) RunManagementChannelKeyHealthCheck(c *gin.Context) {
	id, ok := parsePositiveParam(c, "id")
	if !ok {
		return
	}
	ctx, ok := h.requireScope(c, scopes.ScopeWriteChannels, 0)
	if !ok {
		return
	}

	var req healthCheckRequest
	if c.Request.Body != nil && c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			JSONError(c, http.StatusBadRequest, errors.New("invalid request format"))
			return
		}
	}

	items, err := h.channelService.RunChannelAPIKeyHealthCheck(ctx, id, req.KeyIDs)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}

	c.JSON(http.StatusOK, managementChannelKeyInventoryResponse{Items: inventoryItemsResponse(items)})
}

func (h *ManagementHandlers) requireScope(c *gin.Context, requiredScope scopes.ScopeSlug, projectID int) (context.Context, bool) {
	ctx := c.Request.Context()
	if projectID > 0 {
		ctx = contexts.WithProjectID(ctx, projectID)
	}
	if err := authz.RequireScope(ctx, requiredScope); err != nil {
		JSONError(c, http.StatusForbidden, err)
		return nil, false
	}

	return authz.WithScopeDecision(ctx, requiredScope), true
}

func (h *ManagementHandlers) validateGrantAuthority(ctx context.Context, requestedScopes []string, projectID int) error {
	if slices.Contains(requestedScopes, scopes.ScopeWildcard) && !userIsGlobalOwner(ctx) {
		return fmt.Errorf("only global owners can grant wildcard scope")
	}

	return h.permissionValidator.CanGrantScopes(ctx, requestedScopes, &projectID)
}

func (h *ManagementHandlers) projectIDFromRequest(c *gin.Context, queryName string) (int, bool) {
	if raw := strings.TrimSpace(c.Query(queryName)); raw != "" {
		id, err := strconv.Atoi(raw)
		if err != nil || id <= 0 {
			return 0, false
		}
		return id, true
	}

	id, ok := contexts.GetProjectID(c.Request.Context())
	if !ok || id <= 0 {
		return 0, false
	}

	return id, true
}

func (h *ManagementHandlers) projectExists(ctx context.Context, projectID int) (bool, error) {
	return h.ent.Project.Query().Where(project.IDEQ(projectID)).Exist(ctx)
}

func (h *ManagementHandlers) serviceAccountToken(ctx context.Context, id int, projectID int) (*ent.APIKey, error) {
	q := h.ent.APIKey.Query().Where(apikey.IDEQ(id), apikey.TypeEQ(apikey.TypeServiceAccount))
	if projectID > 0 {
		q = q.Where(apikey.ProjectIDEQ(projectID))
	}

	token, err := q.Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("management token not found: %w", err)
		}
		return nil, err
	}

	return token, nil
}

func (h *ManagementHandlers) channelResponse(ctx context.Context, ch *ent.Channel) (managementChannelResponse, error) {
	items, err := h.channelService.ChannelAPIKeyInventory(ctx, ch.ID)
	if err != nil {
		return managementChannelResponse{}, err
	}

	return managementChannelResponse{
		ID:        ch.ID,
		Name:      ch.Name,
		Type:      ch.Type,
		Status:    ch.Status,
		BaseURL:   ch.BaseURL,
		Models:    ch.SupportedModels,
		Tags:      ch.Tags,
		KeyCounts: keyCounts(items),
		UpdatedAt: ch.UpdatedAt,
	}, nil
}

func (h *ManagementHandlers) writeInventory(c *gin.Context, ctx context.Context, channelID int) {
	items, err := h.channelService.ChannelAPIKeyInventory(ctx, channelID)
	if err != nil {
		h.writeServiceError(c, err)
		return
	}

	c.JSON(http.StatusOK, managementChannelKeyInventoryResponse{Items: inventoryItemsResponse(items)})
}

func (h *ManagementHandlers) writeServiceError(c *gin.Context, err error) {
	switch {
	case ent.IsNotFound(err):
		JSONError(c, http.StatusNotFound, err)
	case strings.Contains(err.Error(), "not found"):
		JSONError(c, http.StatusNotFound, err)
	case strings.Contains(err.Error(), "required scope"), strings.Contains(err.Error(), "insufficient permissions"):
		JSONError(c, http.StatusForbidden, err)
	case strings.Contains(err.Error(), "invalid"), strings.Contains(err.Error(), "cannot "), strings.Contains(err.Error(), "must "):
		JSONError(c, http.StatusBadRequest, err)
	default:
		JSONError(c, http.StatusInternalServerError, err)
	}
}

func tokenResponse(token *ent.APIKey, includeRaw bool) managementTokenResponse {
	resp := managementTokenResponse{
		ID:        token.ID,
		Name:      token.Name,
		Type:      token.Type,
		Status:    token.Status,
		ProjectID: token.ProjectID,
		Scopes:    slices.Clone(token.Scopes),
		MaskedKey: objects.MaskChannelAPIKey(token.Key),
		CreatedAt: token.CreatedAt,
		UpdatedAt: token.UpdatedAt,
	}
	if includeRaw {
		resp.Key = token.Key
	}

	return resp
}

func normalizeManagementScopes(input []string) ([]string, error) {
	seen := make(map[string]struct{}, len(input))
	out := make([]string, 0, len(input))
	for _, scope := range input {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		if !scopes.IsValidScopeOrWildcard(scope) {
			return nil, fmt.Errorf("invalid scope: %s", scope)
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		out = append(out, scope)
	}

	return out, nil
}

func normalizeProviderKeys(input []string) []string {
	seen := make(map[string]struct{}, len(input))
	out := make([]string, 0, len(input))
	for _, key := range input {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}

	return out
}

func keyCounts(items []*biz.ChannelAPIKeyInventoryItem) managementChannelKeyCounts {
	counts := managementChannelKeyCounts{Total: len(items)}
	for _, item := range items {
		switch item.Status {
		case objects.ChannelKeyStatusDisabled:
			counts.Disabled++
		case objects.ChannelKeyStatusArchived:
			counts.Archived++
		default:
			counts.Active++
		}
	}

	return counts
}

func inventoryItemsResponse(items []*biz.ChannelAPIKeyInventoryItem) []*managementChannelKeyInventoryItem {
	out := make([]*managementChannelKeyInventoryItem, 0, len(items))
	for _, item := range items {
		out = append(out, &managementChannelKeyInventoryItem{
			ID:             item.ID,
			MaskedKey:      item.MaskedKey,
			Status:         item.Status,
			LastCheckedAt:  item.LastCheckedAt,
			Success:        item.Success,
			FailureCount:   item.FailureCount,
			Reason:         item.Reason,
			Balance:        item.Balance,
			Currency:       item.Currency,
			Available:      item.Available,
			StatusCode:     item.StatusCode,
			MatchedPolicy:  item.MatchedPolicy,
			Action:         item.Action,
			NextCheckAt:    item.NextCheckAt,
			BackoffAttempt: item.BackoffAttempt,
			History:        slices.Clone(item.History),
		})
	}

	return out
}

func parsePositiveParam(c *gin.Context, name string) (int, bool) {
	value, err := strconv.Atoi(c.Param(name))
	if err != nil || value <= 0 {
		JSONError(c, http.StatusBadRequest, fmt.Errorf("invalid %s", name))
		return 0, false
	}

	return value, true
}

func parseChannelKeyParams(c *gin.Context) (int, string, bool) {
	id, ok := parsePositiveParam(c, "id")
	if !ok {
		return 0, "", false
	}
	keyID := strings.TrimSpace(c.Param("key_id"))
	if keyID == "" {
		JSONError(c, http.StatusBadRequest, errors.New("key_id is required"))
		return 0, "", false
	}

	return id, keyID, true
}

func readOptionalReason(c *gin.Context) (string, bool) {
	if c.Request.Body == nil || c.Request.ContentLength == 0 {
		return "", true
	}

	var req reasonRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		JSONError(c, http.StatusBadRequest, errors.New("invalid request format"))
		return "", false
	}

	return strings.TrimSpace(req.Reason), true
}

func resultMessage(result *biz.DeleteDisabledAPIKeysResult) string {
	if result == nil {
		return ""
	}

	return result.Message
}

func currentUserIsOwner(c *gin.Context) bool {
	user, ok := contexts.GetUser(c.Request.Context())
	return ok && user != nil && user.IsOwner
}

func userIsGlobalOwner(ctx context.Context) bool {
	user, ok := contexts.GetUser(ctx)
	return ok && user != nil && user.IsOwner
}
