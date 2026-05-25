package biz

import (
	"context"
	"fmt"
	"time"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
)

func (svc *ChannelService) markChannelUnavailable(ctx context.Context, channelID int, responseStatusCode int, threshold int, actualCount int) {
	ctx, cancel := xcontext.DetachWithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Only disable channels that are currently enabled to avoid repeated disabling
	// of the same channel under sustained error traffic, which would keep resetting
	// the cache debounce timer and prevent the cache from ever refreshing.
	affected, err := svc.db.Channel.Update().
		Where(
			channel.ID(channelID),
			channel.StatusEQ(channel.StatusEnabled),
		).
		SetStatus(channel.StatusDisabled).
		SetErrorMessage(deriveErrorMessage(responseStatusCode)).
		Save(ctx)
	if err != nil {
		log.Error(ctx, "Failed to disable channel on unrecoverable error",
			log.Int("channel_id", channelID),
			log.Int("error_code", responseStatusCode),
			log.Cause(err),
		)

		return
	}

	if affected == 0 {
		log.Debug(ctx, "Channel already disabled, skipping",
			log.Int("channel_id", channelID),
			log.Int("error_code", responseStatusCode),
		)

		// Another instance may have already disabled the channel in DB while this
		// instance still serves it from a stale in-memory cache. Force a local
		// refresh so candidate selection stops using the channel immediately.
		if err := svc.enabledChannelsCache.Load(ctx, true); err != nil {
			log.Warn(ctx, "Failed to refresh local cache for already-disabled channel",
				log.Int("channel_id", channelID),
				log.Cause(err),
			)
		}

		return
	}

	log.Warn(ctx, "Channel disabled due to unrecoverable error",
		log.Int("channel_id", channelID),
		log.Int("error_code", responseStatusCode),
	)

	// Fetch the updated channel for webhook notification
	updatedChannel, err := svc.db.Channel.Get(ctx, channelID)
	if err != nil {
		log.Error(ctx, "Failed to fetch disabled channel for webhook notification",
			log.Int("channel_id", channelID),
			log.Cause(err),
		)
	} else {
		notifyCtx := context.WithoutCancel(ctx)
		go svc.WebhookNotifier.NotifyChannelAutoDisabled(notifyCtx, ChannelAutoDisabledEvent{
			ChannelID:       updatedChannel.ID,
			ChannelName:     updatedChannel.Name,
			ChannelProvider: updatedChannel.Type.String(),
			ChannelBaseURL:  updatedChannel.BaseURL,
			ChannelStatus:   updatedChannel.Status.String(),
			StatusCode:      responseStatusCode,
			Threshold:       threshold,
			ActualCount:     actualCount,
			Reason:          deriveErrorMessage(responseStatusCode),
			OccurredAt:      time.Now(),
		})
	}

	// Synchronously reload the local cache to immediately stop selecting this channel.
	// This avoids the debounce delay that could keep the disabled channel in the candidate pool.
	if err := svc.enabledChannelsCache.Load(ctx, true); err != nil {
		log.Warn(ctx, "Failed to synchronously reload channels after auto-disable",
			log.Int("channel_id", channelID),
			log.Cause(err),
		)
	}

	// Also notify other instances via the watcher for cross-instance cache invalidation.
	svc.asyncReloadChannels()
}

// checkAndHandleChannelError checks if the channel should be disabled based on the error status code.
func (svc *ChannelService) checkAndHandleChannelError(ctx context.Context, perf *PerformanceRecord, policy *RetryPolicy) bool {
	for _, statusConfig := range policy.AutoDisableChannel.Statuses {
		if statusConfig.Status != perf.ResponseStatusCode {
			continue
		}

		svc.channelErrorCountsLock.Lock()

		if svc.channelErrorCounts[perf.ChannelID] == nil {
			svc.channelErrorCounts[perf.ChannelID] = make(map[int]int)
		}

		svc.channelErrorCounts[perf.ChannelID][perf.ResponseStatusCode]++
		count := svc.channelErrorCounts[perf.ChannelID][perf.ResponseStatusCode]
		svc.channelErrorCountsLock.Unlock()

		if count >= statusConfig.Times {
			svc.markChannelUnavailable(ctx, perf.ChannelID, perf.ResponseStatusCode, statusConfig.Times, count)
			svc.channelErrorCountsLock.Lock()
			delete(svc.channelErrorCounts, perf.ChannelID)
			svc.channelErrorCountsLock.Unlock()

			return true
		}
	}

	return false
}

// checkAndHandleAPIKeyError checks if the API key should be disabled based on the error status code.
// Returns true if the API key was disabled.
func (svc *ChannelService) checkAndHandleAPIKeyError(ctx context.Context, perf *PerformanceRecord, policy *RetryPolicy) bool {
	for _, statusConfig := range policy.AutoDisableChannel.Statuses {
		if statusConfig.Status != perf.ResponseStatusCode {
			continue
		}

		svc.apiKeyErrorCountsLock.Lock()

		if svc.apiKeyErrorCounts[perf.ChannelID] == nil {
			svc.apiKeyErrorCounts[perf.ChannelID] = make(map[string]map[int]int)
		}

		if svc.apiKeyErrorCounts[perf.ChannelID][perf.APIKey] == nil {
			svc.apiKeyErrorCounts[perf.ChannelID][perf.APIKey] = make(map[int]int)
		}

		svc.apiKeyErrorCounts[perf.ChannelID][perf.APIKey][perf.ResponseStatusCode]++
		count := svc.apiKeyErrorCounts[perf.ChannelID][perf.APIKey][perf.ResponseStatusCode]
		svc.apiKeyErrorCountsLock.Unlock()

		if count >= statusConfig.Times {
			reason := fmt.Sprintf("Auto-disabled after %d consecutive errors with status %d", count, perf.ResponseStatusCode)
			if err := svc.DisableAPIKey(ctx, perf.ChannelID, perf.APIKey, perf.ResponseStatusCode, reason); err != nil {
				log.Error(ctx, "Failed to disable API key",
					log.Int("channel_id", perf.ChannelID),
					log.Int("error_code", perf.ResponseStatusCode),
					log.Cause(err),
				)

				return false
			}

			svc.apiKeyErrorCountsLock.Lock()
			delete(svc.apiKeyErrorCounts[perf.ChannelID], perf.APIKey)
			svc.apiKeyErrorCountsLock.Unlock()

			return true
		}
	}

	return false
}

func (svc *ChannelService) handleRequestFailurePolicy(ctx context.Context, perf *PerformanceRecord, policy *RetryPolicy) bool {
	if perf == nil || policy == nil || perf.ResponseStatusCode == 0 {
		return false
	}

	channelCount := svc.incrementChannelFailureCount(perf.ChannelID, perf.ResponseStatusCode)
	keyCount := 0
	if perf.APIKey != "" {
		keyCount = svc.incrementAPIKeyFailureCount(perf.ChannelID, perf.APIKey, perf.ResponseStatusCode)
	}

	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, perf.ChannelID)
	if err != nil {
		log.Warn(ctx, "failed to load channel for failure policy",
			log.Int("channel_id", perf.ChannelID),
			log.Cause(err),
		)

		return false
	}

	effective := resolveEffectiveFailurePolicy(policy, ch.Settings, perf.APIKey == "")
	if len(effective.KeyProfiles) == 0 && len(effective.ChannelProfiles) == 0 {
		return false
	}

	now := time.Now()
	routingChanged := false
	if perf.APIKey != "" {
		keyEvent := failurePolicyEvent{
			Source:       objects.FailurePolicyEventSourceRequestFailure,
			Target:       objects.FailurePolicyTargetKey,
			ChannelID:    perf.ChannelID,
			Key:          perf.APIKey,
			StatusCode:   perf.ResponseStatusCode,
			FailureCount: keyCount,
			Reason:       deriveErrorMessage(perf.ResponseStatusCode),
			CheckedAt:    now,
		}
		matches := evaluateFailurePolicyProfiles(effective.KeyProfiles, keyEvent)
		if len(matches) > 0 {
			changed, err := svc.applyFailurePolicyActions(ctx, keyEvent, summarizeFailurePolicyMatches(matches), matchesToFailurePolicyActions(matches), true)
			if err != nil {
				log.Error(ctx, "failed to apply key failure policy",
					log.Int("channel_id", perf.ChannelID),
					log.Int("error_code", perf.ResponseStatusCode),
					log.Cause(err),
				)
			}
			routingChanged = routingChanged || changed
			if changed {
				svc.apiKeyErrorCountsLock.Lock()
				delete(svc.apiKeyErrorCounts[perf.ChannelID], perf.APIKey)
				svc.apiKeyErrorCountsLock.Unlock()
			}
		}
	}

	channelEvent := failurePolicyEvent{
		Source:       objects.FailurePolicyEventSourceRequestFailure,
		Target:       objects.FailurePolicyTargetChannel,
		ChannelID:    perf.ChannelID,
		StatusCode:   perf.ResponseStatusCode,
		FailureCount: channelCount,
		Reason:       deriveErrorMessage(perf.ResponseStatusCode),
		CheckedAt:    now,
	}
	matches := evaluateFailurePolicyProfiles(effective.ChannelProfiles, channelEvent)
	if len(matches) > 0 {
		changed, err := svc.applyFailurePolicyActions(ctx, channelEvent, summarizeFailurePolicyMatches(matches), matchesToFailurePolicyActions(matches), true)
		if err != nil {
			log.Error(ctx, "failed to apply channel failure policy",
				log.Int("channel_id", perf.ChannelID),
				log.Int("error_code", perf.ResponseStatusCode),
				log.Cause(err),
			)
		}
		routingChanged = routingChanged || changed
		if changed {
			svc.channelErrorCountsLock.Lock()
			delete(svc.channelErrorCounts, perf.ChannelID)
			svc.channelErrorCountsLock.Unlock()
		}
	}

	if routingChanged {
		svc.asyncReloadChannels()
	}

	return routingChanged
}

func (svc *ChannelService) incrementChannelFailureCount(channelID int, status int) int {
	svc.channelErrorCountsLock.Lock()
	defer svc.channelErrorCountsLock.Unlock()

	if svc.channelErrorCounts[channelID] == nil {
		svc.channelErrorCounts[channelID] = make(map[int]int)
	}
	svc.channelErrorCounts[channelID][status]++

	return svc.channelErrorCounts[channelID][status]
}

func (svc *ChannelService) incrementAPIKeyFailureCount(channelID int, apiKey string, status int) int {
	svc.apiKeyErrorCountsLock.Lock()
	defer svc.apiKeyErrorCountsLock.Unlock()

	if svc.apiKeyErrorCounts[channelID] == nil {
		svc.apiKeyErrorCounts[channelID] = make(map[string]map[int]int)
	}
	if svc.apiKeyErrorCounts[channelID][apiKey] == nil {
		svc.apiKeyErrorCounts[channelID][apiKey] = make(map[int]int)
	}
	svc.apiKeyErrorCounts[channelID][apiKey][status]++

	return svc.apiKeyErrorCounts[channelID][apiKey][status]
}
