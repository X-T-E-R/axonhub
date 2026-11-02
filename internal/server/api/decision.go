package api

import (
	"github.com/gin-gonic/gin"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/transformer/decision"
)

func newNativeDecisionHandlers(params OpenAIHandlersParams, format llm.APIFormat) *ChatCompletionHandlers {
	return NewChatCompletionHandlers(orchestrator.NewChatCompletionOrchestrator(
		params.ChannelService, params.DefaultSelector, params.RequestService, params.HttpClient,
		decision.NewInbound(format), params.SystemService, params.UsageLogService,
		params.PromptService, params.QuotaService, params.PromptProtectionRuleService,
		params.LiveStreamRegistry, params.ChannelLimiterManager, params.ProviderQuotaStatusProvider,
	))
}

func (h *OpenAIHandlers) CreateSystemOne(c *gin.Context) { h.SystemOneHandlers.ChatCompletion(c) }
func (h *OpenAIHandlers) CreateDecision(c *gin.Context)  { h.DecisionHandlers.ChatCompletion(c) }
