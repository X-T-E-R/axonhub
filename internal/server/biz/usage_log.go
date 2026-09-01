package biz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
)

// UsageLogService handles usage log operations.
type UsageLogService struct {
	*AbstractService

	SystemService  *SystemService
	ChannelService *ChannelService

	// OnUsageLogCreated is called after a usage log is successfully created.
	// Used to invalidate caches that depend on usage log data.
	OnUsageLogCreated func()

	afterManagedCoreInsertForTest func()
}

var (
	errUsageLogInsertStage = errors.New("usage log insert stage failed")
	errUsageLogMarkStage   = errors.New("usage log managed-group mark stage failed")
)

func (s *UsageLogService) computeUsageCost(ctx context.Context, channelID int, modelID string, usage *llm.Usage) ([]objects.CostItem, *float64, string) {
	if usage == nil {
		return nil, nil, ""
	}

	ch := s.ChannelService.GetEnabledChannel(channelID)
	if ch == nil {
		log.Warn(ctx, "channel not enabled for cost calculation",
			log.Int("channel_id", channelID),
			log.String("model_id", modelID),
		)

		return nil, nil, ""
	}

	if log.DebugEnabled(ctx) {
		log.Debug(ctx, "checking cached model price",
			log.Int("channel_id", channelID),
			log.String("model_id", modelID),
			log.Int("cached_price_count", len(ch.cachedModelPrices)),
		)
	}

	if modelPrice, ok := ch.cachedModelPrices[modelID]; ok {
		items, total := ComputeUsageCost(usage, modelPrice.Price)

		totalCost := total.InexactFloat64()
		if log.DebugEnabled(ctx) {
			log.Debug(ctx, "computed usage cost from cache",
				log.Int("channel_id", channelID),
				log.String("model_id", modelID),
				log.Float64("total_cost", totalCost),
				log.Int64("total_tokens", usage.TotalTokens),
				log.String("price_reference_id", modelPrice.ReferenceID),
			)
		}

		return items, lo.ToPtr(totalCost), modelPrice.ReferenceID
	}

	return nil, nil, ""
}

// NewUsageLogService creates a new UsageLogService.
func NewUsageLogService(ent *ent.Client, systemService *SystemService, channelService *ChannelService) *UsageLogService {
	return &UsageLogService{
		AbstractService: &AbstractService{
			db: ent,
		},
		SystemService:  systemService,
		ChannelService: channelService,
	}
}

// SetAfterManagedCoreInsertHookForTest installs a deterministic barrier after
// the core insert but before the managed-group mark and accounting delta.
func (s *UsageLogService) SetAfterManagedCoreInsertHookForTest(hook func()) {
	s.afterManagedCoreInsertForTest = hook
}

// CreateUsageLogParams represents the parameters for creating a usage log.
type CreateUsageLogParams struct {
	RequestID     int
	ProjectID     int
	ChannelID     int
	ActualModelID string // The channel actual model ID, not the request model ID.
	Usage         *llm.Usage
	Source        usagelog.Source
	Format        string
	APIKeyID      *int
}

// CreateUsageLog creates a new usage log record from LLM response usage data.
func (s *UsageLogService) CreateUsageLog(ctx context.Context, params CreateUsageLogParams) (*ent.UsageLog, error) {
	if params.Usage == nil {
		return nil, nil // No usage data to log
	}

	// Calculate cost if price is configured
	var (
		totalCost        *float64
		costItems        []objects.CostItem
		priceReferenceID string
	)

	costItems, totalCost, priceReferenceID = s.computeUsageCost(ctx, params.ChannelID, params.ActualModelID, params.Usage)

	saveUsageLog := func(saveCtx context.Context) (*ent.UsageLog, error) {
		client := s.entFromContext(saveCtx)
		mut := client.UsageLog.Create().
			SetRequestID(params.RequestID).
			SetProjectID(params.ProjectID).
			SetModelID(params.ActualModelID).
			SetChannelID(params.ChannelID).
			SetPromptTokens(params.Usage.PromptTokens).
			SetCompletionTokens(params.Usage.CompletionTokens).
			SetTotalTokens(params.Usage.TotalTokens).
			SetSource(params.Source).
			SetFormat(params.Format).
			SetNillableTotalCost(totalCost).
			SetCostItems(costItems)

		if params.APIKeyID != nil {
			mut = mut.SetAPIKeyID(*params.APIKeyID)
		} else if ctxAPIKey, ok := contexts.GetAPIKey(saveCtx); ok && ctxAPIKey != nil {
			mut = mut.SetAPIKeyID(ctxAPIKey.ID)
		}
		if params.Usage.PromptTokensDetails != nil {
			mut = mut.
				SetPromptAudioTokens(params.Usage.PromptTokensDetails.AudioTokens).
				SetPromptCachedTokens(params.Usage.PromptTokensDetails.CachedTokens).
				SetPromptWriteCachedTokens(params.Usage.PromptTokensDetails.WriteCachedTokens).
				SetPromptWriteCachedTokens5m(params.Usage.PromptTokensDetails.WriteCached5MinTokens).
				SetPromptWriteCachedTokens1h(params.Usage.PromptTokensDetails.WriteCached1HourTokens)
		}
		if params.Usage.CompletionTokensDetails != nil {
			mut = mut.
				SetCompletionAudioTokens(params.Usage.CompletionTokensDetails.AudioTokens).
				SetCompletionReasoningTokens(params.Usage.CompletionTokensDetails.ReasoningTokens).
				SetCompletionAcceptedPredictionTokens(params.Usage.CompletionTokensDetails.AcceptedPredictionTokens).
				SetCompletionRejectedPredictionTokens(params.Usage.CompletionTokensDetails.RejectedPredictionTokens)
		}
		if priceReferenceID != "" {
			mut = mut.SetCostPriceReferenceID(priceReferenceID)
		}
		return mut.Save(saveCtx)
	}

	// Managed Usage rows, their parent mark and their conservative delta share
	// the reconciliation state lock and one commit. This makes the GC projection
	// the serialization authority while allowing the compact row to soft-overrun.
	costBytes, _ := json.Marshal(costItems)
	var usageLog *ent.UsageLog
	managed, persistErr := s.SystemService.persistManagedObservabilityCore(
		ctx,
		"usage_log",
		managedUsageLogCharge(params.ActualModelID, params.Format, priceReferenceID, len(costBytes)),
		func(txCtx context.Context) error {
			if lockErr := lockRequestGroup(txCtx, s.entFromContext(txCtx), params.RequestID); lockErr != nil {
				return fmt.Errorf("%w: %w", errUsageLogMarkStage, lockErr)
			}
			return nil
		},
		func(txCtx context.Context, managed bool) error {
			var saveErr error
			usageLog, saveErr = saveUsageLog(txCtx)
			if saveErr != nil {
				return fmt.Errorf("%w: %w", errUsageLogInsertStage, saveErr)
			}
			if managed {
				if s.afterManagedCoreInsertForTest != nil {
					s.afterManagedCoreInsertForTest()
				}
				if _, markErr := s.entFromContext(txCtx).Request.UpdateOneID(params.RequestID).SetManagedObservability(true).Save(txCtx); markErr != nil {
					return fmt.Errorf("%w: %w", errUsageLogMarkStage, markErr)
				}
			}
			return nil
		},
	)
	if persistErr == nil && ent.TxFromContext(ctx) == nil {
		usageLog.Unwrap()
	}
	if persistErr != nil {
		if errors.Is(persistErr, errUsageLogInsertStage) {
			return nil, fmt.Errorf("failed to create usage log: %w", persistErr)
		}
		if !errors.Is(persistErr, errManagedCoreAccounting) && !errors.Is(persistErr, errUsageLogMarkStage) {
			return nil, fmt.Errorf("failed to commit usage log: %w", persistErr)
		}
		if ent.TxFromContext(ctx) != nil {
			return nil, fmt.Errorf("managed usage-log stage failed inside caller transaction: %w", persistErr)
		}

		component := "core_accounting:usage_log"
		if errors.Is(persistErr, errUsageLogMarkStage) {
			component = "usage_group_mark"
		}
		s.SystemService.RecordManagedObservabilityFailure(ctx, component, "failed")
		log.Warn(ctx, "Managed usage-log transaction degraded; preserving the core usage row", log.Cause(persistErr))

		var saveErr error
		usageLog, saveErr = saveUsageLog(ctx)
		if saveErr != nil {
			return nil, fmt.Errorf("failed to create usage log after managed accounting degradation: %w", saveErr)
		}
		if managed {
			if _, markErr := s.entFromContext(ctx).Request.UpdateOneID(params.RequestID).SetManagedObservability(true).Save(ctx); markErr != nil {
				s.SystemService.RecordManagedObservabilityFailure(ctx, "usage_group_mark", "failed")
				log.Warn(ctx, "Failed to mark degraded managed usage-log request group; core usage row was preserved", log.Cause(markErr))
			}
		}
	}

	if log.DebugEnabled(ctx) {
		log.Debug(ctx, "Created usage log",
			log.Int("usage_log_id", usageLog.ID),
			log.Int("request_id", params.RequestID),
			log.String("model_id", params.ActualModelID),
			log.Int64("total_tokens", params.Usage.TotalTokens),
		)
	}

	if s.OnUsageLogCreated != nil {
		s.OnUsageLogCreated()
	}

	return usageLog, nil
}

// CreateUsageLogFromRequest creates a usage log from request and response data.
func (s *UsageLogService) CreateUsageLogFromRequest(
	ctx context.Context,
	request *ent.Request,
	requestExec *ent.RequestExecution,
	usage *llm.Usage,
) (*ent.UsageLog, error) {
	if request == nil || usage == nil {
		return nil, nil
	}

	return s.CreateUsageLog(ctx, CreateUsageLogParams{
		RequestID:     request.ID,
		ProjectID:     request.ProjectID,
		ChannelID:     requestExec.ChannelID,
		ActualModelID: requestExec.ModelID,
		Usage:         usage,
		Source:        usagelog.Source(request.Source),
		Format:        request.Format,
		APIKeyID:      lo.ToPtr(request.APIKeyID),
	})
}
