package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/fx"

	entprivacy "github.com/looplj/axonhub/internal/ent/privacy"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/gemini"
)

type GeminiHandlersParams struct {
	fx.In

	ChannelService  *biz.ChannelService
	ModelService    *biz.ModelService
	DefaultSelector *orchestrator.DefaultSelector
	RequestService  *biz.RequestService
	SystemService   *biz.SystemService
	UsageLogService *biz.UsageLogService
	PromptService   *biz.PromptService
	PromptProtectionRuleService *biz.PromptProtectionRuleService
	QuotaService    *biz.QuotaService
	HttpClient      *httpclient.HttpClient
	LiveStreamRegistry *biz.LiveStreamRegistry
	ChannelLimiterManager       *orchestrator.ChannelLimiterManager
	ProviderQuotaStatusProvider orchestrator.ProviderQuotaStatusProvider
}

type GeminiHandlers struct {
	ChannelService         *biz.ChannelService
	ModelService           *biz.ModelService
	ChatCompletionHandlers *ChatCompletionHandlers
}

func NewGeminiHandlers(params GeminiHandlersParams) *GeminiHandlers {
	return &GeminiHandlers{
		ChatCompletionHandlers: NewChatCompletionHandlers(
			orchestrator.NewChatCompletionOrchestrator(
				params.ChannelService,
				params.DefaultSelector,
				params.RequestService,
				params.HttpClient,
				gemini.NewInboundTransformer(),
				params.SystemService,
				params.UsageLogService,
				params.PromptService,
				params.QuotaService,
				params.PromptProtectionRuleService,
				params.LiveStreamRegistry,
				params.ChannelLimiterManager,
				params.ProviderQuotaStatusProvider,
			),
		),
		ChannelService: params.ChannelService,
		ModelService:   params.ModelService,
	}
}

func (handlers *GeminiHandlers) GenerateContent(c *gin.Context) {
	alt := c.Query("alt")
	switch alt {
	case "sse":
		handlers.ChatCompletionHandlers.WithStreamWriter(func(c *gin.Context, stream streams.Stream[*httpclient.StreamEvent]) {
			WriteSSEStreamWithErrorFormatter(c, stream, formatStreamErrorForInbound(handlers.ChatCompletionHandlers.ChatCompletionOrchestrator.Inbound))
		}).ChatCompletion(c)
	default:
		handlers.ChatCompletionHandlers.WithStreamWriter(func(c *gin.Context, stream streams.Stream[*httpclient.StreamEvent]) {
			WriteGeminiStreamWithErrorFormatter(c, stream, formatStreamErrorForInbound(handlers.ChatCompletionHandlers.ChatCompletionOrchestrator.Inbound))
		}).ChatCompletion(c)
	}
}

func WriteGeminiStream(c *gin.Context, stream streams.Stream[*httpclient.StreamEvent]) {
	WriteGeminiStreamWithErrorFormatter(c, stream, formatStreamErrorForInbound(gemini.NewInboundTransformer()))
}

func WriteGeminiStreamWithErrorFormatter(c *gin.Context, stream streams.Stream[*httpclient.StreamEvent], formatErr StreamErrorFormatter) {
	ctx := c.Request.Context()
	clientDisconnected := false

	if formatErr == nil {
		formatErr = formatStreamErrorForInbound(gemini.NewInboundTransformer())
	}

	defer func() {
		if clientDisconnected {
			log.Warn(ctx, "Client disconnected")
		}
	}()

	c.Header("Content-Type", "application/json; charset=UTF-8")

	_, _ = c.Writer.Write([]byte("["))

	first := true

	for {
		select {
		case <-ctx.Done():
			clientDisconnected = true

			log.Warn(ctx, "Client disconnected, stop streaming")

			return
		default:
			if stream.Next() {
				cur := stream.Current()

				if !first {
					_, _ = c.Writer.Write([]byte(","))
				}

				_, _ = c.Writer.Write(cur.Data)
				first = false

				log.Debug(ctx, "write stream event", log.Any("event", cur))
				c.Writer.Flush()
			} else {
				if err := stream.Err(); err != nil {
					log.Error(ctx, "Error in stream", log.Cause(err))
					if errPayload, marshalErr := marshalStreamErrorPayload(formatErr(ctx, err)); marshalErr == nil && len(errPayload) > 0 {
						if !first {
							_, _ = c.Writer.Write([]byte(","))
						}
						_, _ = c.Writer.Write(errPayload)
						first = false
					}
				}

				_, _ = c.Writer.Write([]byte("]"))
				c.Writer.Flush()

				return
			}
		}
	}
}

func marshalStreamErrorPayload(formatted any) ([]byte, error) {
	switch v := formatted.(type) {
	case nil:
		return nil, nil
	case json.RawMessage:
		return v, nil
	case []byte:
		return v, nil
	default:
		return json.Marshal(v)
	}
}

// GeminiModel represents a model in the list models response.
type GeminiModel struct {
	Name                       string   `json:"name"`
	BaseModelID                string   `json:"baseModelId"`
	Version                    string   `json:"version"`
	DisplayName                string   `json:"displayName"`
	Description                string   `json:"description"`
	SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
}

// ListModels returns all available Gemini models.
// This endpoint is compatible with Gemini's /v1/models API.
// It uses QueryAllChannelModels setting from system config to determine model source.
func (handlers *GeminiHandlers) ListModels(c *gin.Context) {
	ctx := c.Request.Context()

	models, err := handlers.ModelService.ListEnabledModels(ctx)
	if err != nil {
		status := http.StatusInternalServerError
		errorStatus := "internal_server_error"
		if errors.Is(err, entprivacy.Deny) {
			status = http.StatusForbidden
			errorStatus = "permission_error"
		}
		_ = c.Error(err)
		c.JSON(status, gemini.GeminiError{
			Error: gemini.ErrorDetail{
				Message: err.Error(),
				Code:    status,
				Status:  errorStatus,
			},
		})

		return
	}

	geminiModels := make([]GeminiModel, 0, len(models))
	for i, model := range models {
		geminiModels = append(geminiModels, GeminiModel{
			Name:                       "models/" + model.ID,
			BaseModelID:                model.ID,
			Version:                    fmt.Sprintf("%s-%d", model.ID, i),
			DisplayName:                model.DisplayName,
			Description:                model.DisplayName,
			SupportedGenerationMethods: []string{"generateContent", "streamGenerateContent"},
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"models": geminiModels,
	})
}
