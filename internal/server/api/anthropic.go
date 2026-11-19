package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/contexts"
	entprivacy "github.com/looplj/axonhub/internal/ent/privacy"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
)

type AnthropicHandlersParams struct {
	fx.In

	ChannelService              *biz.ChannelService
	ModelService                *biz.ModelService
	DefaultSelector             *orchestrator.DefaultSelector
	RequestService              *biz.RequestService
	SystemService               *biz.SystemService
	UsageLogService             *biz.UsageLogService
	PromptService               *biz.PromptService
	PromptProtectionRuleService *biz.PromptProtectionRuleService
	QuotaService                *biz.QuotaService
	HttpClient                  *httpclient.HttpClient
	LiveStreamRegistry          *biz.LiveStreamRegistry
	ChannelLimiterManager       *orchestrator.ChannelLimiterManager
	ProviderQuotaStatusProvider orchestrator.ProviderQuotaStatusProvider
	SSEKeepAliveConfig          SSEKeepAliveConfig
}

type AnthropicHandlers struct {
	ChannelService         *biz.ChannelService
	ModelService           *biz.ModelService
	SystemService          *biz.SystemService
	ChatCompletionHandlers *ChatCompletionHandlers
	ThreadStore            *AnthropicThreadStore
}

func NewAnthropicHandlers(params AnthropicHandlersParams) *AnthropicHandlers {
	return &AnthropicHandlers{
		ChatCompletionHandlers: &ChatCompletionHandlers{
			ChatCompletionOrchestrator: orchestrator.NewChatCompletionOrchestrator(
				params.ChannelService,
				params.DefaultSelector,
				params.RequestService,
				params.HttpClient,
				anthropic.NewInboundTransformer(),
				params.SystemService,
				params.UsageLogService,
				params.PromptService,
				params.QuotaService,
				params.PromptProtectionRuleService,
				params.LiveStreamRegistry,
				params.ChannelLimiterManager,
				params.ProviderQuotaStatusProvider,
			),
			sseKeepAlive:       params.SSEKeepAliveConfig,
			sseHeartbeatFormat: sseHeartbeatAnthropic,
		},
		ChannelService: params.ChannelService,
		ModelService:   params.ModelService,
		SystemService:  params.SystemService,
		ThreadStore:    NewAnthropicThreadStore(),
	}
}

func (handlers *AnthropicHandlers) CreateMessage(c *gin.Context) {
	ctx := c.Request.Context()

	genericReq, err := httpclient.ReadHTTPRequest(c.Request)
	if err != nil {
		httpErr := handlers.ChatCompletionHandlers.ChatCompletionOrchestrator.Inbound.TransformError(ctx, err)
		c.JSON(httpErr.StatusCode, json.RawMessage(httpErr.Body))
		return
	}

	betaTokens := parseAnthropicBetaTokens(genericReq.Headers)
	if hasAnthropicBetaToken(betaTokens, anthropicMessageThreadsBeta) && len(genericReq.Body) > 0 {
		var topFields map[string]json.RawMessage
		if json.Unmarshal(genericReq.Body, &topFields) == nil && topFields != nil {
			if _, hasThread := topFields["thread"]; hasThread {
				session, materializedReq, threadErr := handlers.prepareAnthropicThreadRequest(ctx, genericReq, topFields, betaTokens)
				if threadErr != nil {
					writeAnthropicThreadError(c, threadErr)
					return
				}
				handlers.ChatCompletionHandlers.chatCompletionWithRequest(c, materializedReq, session)
				return
			}
		}
	}

	handlers.ChatCompletionHandlers.ChatCompletionWithRequest(c, genericReq)
}

// CountTokens rejects requests carrying the message-threads "thread" field per
// the message-threads-2026-08-12 specification.
func (handlers *AnthropicHandlers) CountTokens(c *gin.Context) {
	genericReq, err := httpclient.ReadHTTPRequest(c.Request)
	if err != nil {
		writeAnthropicThreadError(c, newAnthropicThreadInvalidRequestError("Failed to read request body."))
		return
	}
	if len(genericReq.Body) > 0 {
		var topFields map[string]json.RawMessage
		if json.Unmarshal(genericReq.Body, &topFields) == nil && topFields != nil {
			if _, hasThread := topFields["thread"]; hasThread {
				writeAnthropicThreadError(c, newAnthropicThreadInvalidRequestError("The 'thread' parameter is not supported on token counting endpoints."))
				return
			}
		}
	}
	writeAnthropicThreadError(c, &anthropicThreadError{
		status:  http.StatusNotFound,
		errType: "not_found_error",
		message: "Token counting endpoint is not supported.",
	})
}

type AnthropicModel struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created"`
}

// ListModels returns all available models.
// It uses QueryAllChannelModels setting from system config to determine model source.
func (handlers *AnthropicHandlers) ListModels(c *gin.Context) {
	ctx := c.Request.Context()

	models, err := handlers.ModelService.ListEnabledModels(ctx)
	if err != nil {
		requestID, _ := contexts.GetRequestID(ctx)
		status := http.StatusInternalServerError
		errorType := "internal_server_error"
		if errors.Is(err, entprivacy.Deny) {
			status = http.StatusForbidden
			errorType = "permission_error"
		}
		_ = c.Error(err)
		c.JSON(status, anthropic.AnthropicError{
			StatusCode: status,
			Type:       errorType,
			RequestID:  requestID,
			Error: anthropic.ErrorDetail{
				Type:    errorType,
				Message: err.Error(),
			},
		})

		return
	}

	anthropicModels := make([]AnthropicModel, 0, len(models))
	for _, model := range models {
		anthropicModels = append(anthropicModels, AnthropicModel{
			ID:          model.ID,
			Type:        "model",
			DisplayName: model.DisplayName,
			CreatedAt:   model.CreatedAt,
		})
	}

	var firstID string
	if len(anthropicModels) > 0 {
		firstID = anthropicModels[0].ID
	}

	var lastID string
	if len(anthropicModels) > 0 {
		lastID = anthropicModels[len(anthropicModels)-1].ID
	}

	c.JSON(http.StatusOK, gin.H{
		"object":   "list",
		"data":     anthropicModels,
		"has_more": false,
		"first_id": firstID,
		"last_id":  lastID,
	})
}
