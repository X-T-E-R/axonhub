package biz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
)

type failurePolicyEvent struct {
	Source               objects.FailurePolicyEventSource
	Target               objects.FailurePolicyTarget
	ChannelID            int
	Key                  string
	StatusCode           int
	FailureCount         int
	Available            *bool
	Balance              any
	Currency             string
	Reason               string
	AllCheckedKeysFailed bool
	CheckedAt            time.Time
}

type failurePolicyMatch struct {
	Profile objects.FailurePolicyProfile
	Actions []objects.FailurePolicyAction
}

type effectiveFailurePolicy struct {
	KeyProfiles     []objects.FailurePolicyProfile
	ChannelProfiles []objects.FailurePolicyProfile
}

func resolveEffectiveFailurePolicy(retry *RetryPolicy, settings *objects.ChannelSettings, includeLegacyGlobalChannelProfiles bool) effectiveFailurePolicy {
	var global objects.FailurePolicy
	if retry != nil {
		global = retry.FailurePolicy
		if !failurePolicyHasProfiles(global) {
			global = synthesizeGlobalFailurePolicyFromLegacyAutoDisable(retry.AutoDisableChannel, includeLegacyGlobalChannelProfiles)
		}
	}

	channelPolicy, hasExplicitChannelPolicy := channelFailurePolicyOrLegacy(settings)
	if hasExplicitChannelPolicy {
		switch channelPolicy.Mode {
		case objects.ChannelFailurePolicyModeDisabled:
			return effectiveFailurePolicy{}
		case objects.ChannelFailurePolicyModeOverride:
			return effectiveFailurePolicy{
				KeyProfiles:     slices.Clone(channelPolicy.KeyProfiles),
				ChannelProfiles: slices.Clone(channelPolicy.ChannelProfiles),
			}
		case objects.ChannelFailurePolicyModeMerge:
			return mergeFailurePolicies(channelPolicy.KeyProfiles, global.KeyProfiles, channelPolicy.ChannelProfiles, global.ChannelProfiles)
		case "":
			if channelFailurePolicyHasProfiles(channelPolicy) {
				return mergeFailurePolicies(channelPolicy.KeyProfiles, global.KeyProfiles, channelPolicy.ChannelProfiles, global.ChannelProfiles)
			}

			return effectiveFailurePolicy{
				KeyProfiles:     slices.Clone(global.KeyProfiles),
				ChannelProfiles: slices.Clone(global.ChannelProfiles),
			}
		case objects.ChannelFailurePolicyModeInherit:
			return effectiveFailurePolicy{
				KeyProfiles:     slices.Clone(global.KeyProfiles),
				ChannelProfiles: slices.Clone(global.ChannelProfiles),
			}
		default:
			return effectiveFailurePolicy{
				KeyProfiles:     slices.Clone(global.KeyProfiles),
				ChannelProfiles: slices.Clone(global.ChannelProfiles),
			}
		}
	}

	if failurePolicyHasProfiles(objects.FailurePolicy{
		KeyProfiles:     channelPolicy.KeyProfiles,
		ChannelProfiles: channelPolicy.ChannelProfiles,
	}) {
		return mergeFailurePolicies(channelPolicy.KeyProfiles, global.KeyProfiles, channelPolicy.ChannelProfiles, global.ChannelProfiles)
	}

	return effectiveFailurePolicy{
		KeyProfiles:     slices.Clone(global.KeyProfiles),
		ChannelProfiles: slices.Clone(global.ChannelProfiles),
	}
}

func mergeFailurePolicies(channelKeys, globalKeys, channelProfiles, globalProfiles []objects.FailurePolicyProfile) effectiveFailurePolicy {
	keyProfiles := make([]objects.FailurePolicyProfile, 0, len(channelKeys)+len(globalKeys))
	keyProfiles = append(keyProfiles, channelKeys...)
	keyProfiles = append(keyProfiles, globalKeys...)

	channelPolicyProfiles := make([]objects.FailurePolicyProfile, 0, len(channelProfiles)+len(globalProfiles))
	channelPolicyProfiles = append(channelPolicyProfiles, channelProfiles...)
	channelPolicyProfiles = append(channelPolicyProfiles, globalProfiles...)

	return effectiveFailurePolicy{
		KeyProfiles:     keyProfiles,
		ChannelProfiles: channelPolicyProfiles,
	}
}

func channelFailurePolicyOrLegacy(settings *objects.ChannelSettings) (objects.ChannelFailurePolicy, bool) {
	if settings == nil {
		return objects.ChannelFailurePolicy{}, false
	}
	if settings.FailurePolicy != nil {
		return *settings.FailurePolicy, true
	}

	return synthesizeChannelFailurePolicyFromLegacyHealthCheck(settings.KeyHealthCheck), false
}

func failurePolicyHasProfiles(policy objects.FailurePolicy) bool {
	return len(policy.KeyProfiles) > 0 || len(policy.ChannelProfiles) > 0
}

func channelFailurePolicyHasProfiles(policy objects.ChannelFailurePolicy) bool {
	return len(policy.KeyProfiles) > 0 || len(policy.ChannelProfiles) > 0
}

func synthesizeGlobalFailurePolicyFromLegacyAutoDisable(auto AutoDisableChannel, includeChannelProfiles bool) objects.FailurePolicy {
	if !auto.Enabled || len(auto.Statuses) == 0 {
		return objects.FailurePolicy{}
	}

	keyProfiles := make([]objects.FailurePolicyProfile, 0, len(auto.Statuses))
	channelProfiles := make([]objects.FailurePolicyProfile, 0, len(auto.Statuses))
	for _, status := range auto.Statuses {
		if status.Status < 100 || status.Status > 599 || status.Times <= 0 {
			continue
		}
		name := fmt.Sprintf("Legacy auto-disable %d", status.Status)
		conditions := objects.ChannelKeyHealthCheckPolicyCondition{
			MinFailureCount: &status.Times,
			StatusCodes:     []int{status.Status},
		}
		keyProfiles = append(keyProfiles, objects.FailurePolicyProfile{
			ID:         fmt.Sprintf("legacy-auto-disable-key-%d", status.Status),
			Name:       name,
			Sources:    []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
			Conditions: conditions,
			Actions:    []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDisableKey}},
		})
		if includeChannelProfiles {
			channelProfiles = append(channelProfiles, objects.FailurePolicyProfile{
				ID:         fmt.Sprintf("legacy-auto-disable-channel-%d", status.Status),
				Name:       name,
				Sources:    []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceRequestFailure},
				Conditions: conditions,
				Actions:    []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDisableChannel}},
			})
		}
	}

	return objects.FailurePolicy{KeyProfiles: keyProfiles, ChannelProfiles: channelProfiles}
}

func synthesizeChannelFailurePolicyFromLegacyHealthCheck(health *objects.ChannelKeyHealthCheck) objects.ChannelFailurePolicy {
	if health == nil {
		return objects.ChannelFailurePolicy{}
	}

	legacySources := []objects.FailurePolicyEventSource{
		objects.FailurePolicyEventSourceScheduledHealthCheckFailure,
		objects.FailurePolicyEventSourceManualHealthCheckFailure,
	}

	profiles := make([]objects.FailurePolicyProfile, 0, len(health.Policies)+1)
	for _, policy := range health.Policies {
		if !channelKeyHealthCheckPolicyHasCondition(policy.Conditions) || len(policy.Actions) == 0 {
			continue
		}
		profiles = append(profiles, objects.FailurePolicyProfile{
			ID:         policy.ID,
			Name:       policy.Name,
			Enabled:    policy.Enabled,
			Sources:    legacySources,
			Conditions: policy.Conditions,
			Actions:    failureActionsFromLegacyHealthActions(policy.Actions),
		})
	}

	if len(health.Policies) == 0 {
		threshold := health.FailureThresholdOrDefault()
		profiles = append(profiles, objects.FailurePolicyProfile{
			ID:      "legacy-health-check-threshold",
			Name:    "Legacy health-check failure threshold",
			Sources: legacySources,
			Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
				MinFailureCount: &threshold,
			},
			Actions: []objects.FailurePolicyAction{{Type: failureActionFromLegacyHealthAction(health.FailureActionOrDefault())}},
		})
	}

	return objects.ChannelFailurePolicy{
		Mode:        objects.ChannelFailurePolicyModeMerge,
		KeyProfiles: profiles,
	}
}

func failureActionsFromLegacyHealthActions(actions []objects.ChannelKeyHealthCheckPolicyAction) []objects.FailurePolicyAction {
	next := make([]objects.FailurePolicyAction, 0, len(actions))
	for _, action := range actions {
		next = append(next, objects.FailurePolicyAction{
			Type:    failureActionFromLegacyHealthPolicyAction(action.Type),
			Backoff: action.Backoff,
		})
	}

	return next
}

func failureActionFromLegacyHealthAction(action objects.ChannelKeyHealthCheckFailureAction) objects.FailurePolicyActionType {
	switch action {
	case objects.ChannelKeyHealthCheckFailureActionDisable:
		return objects.FailurePolicyActionDisableKey
	case objects.ChannelKeyHealthCheckFailureActionArchive:
		return objects.FailurePolicyActionArchiveKey
	case objects.ChannelKeyHealthCheckFailureActionDelete:
		return objects.FailurePolicyActionDeleteKey
	default:
		return objects.FailurePolicyActionReportOnly
	}
}

func failureActionFromLegacyHealthPolicyAction(action objects.ChannelKeyHealthCheckPolicyActionType) objects.FailurePolicyActionType {
	switch action {
	case objects.ChannelKeyHealthCheckPolicyActionDisableKey:
		return objects.FailurePolicyActionDisableKey
	case objects.ChannelKeyHealthCheckPolicyActionArchiveKey:
		return objects.FailurePolicyActionArchiveKey
	case objects.ChannelKeyHealthCheckPolicyActionDeleteKey:
		return objects.FailurePolicyActionDeleteKey
	case objects.ChannelKeyHealthCheckPolicyActionDisableChannel:
		return objects.FailurePolicyActionDisableChannel
	case objects.ChannelKeyHealthCheckPolicyActionBackoff:
		return objects.FailurePolicyActionBackoffKey
	default:
		return objects.FailurePolicyActionReportOnly
	}
}

func evaluateFailurePolicyProfiles(profiles []objects.FailurePolicyProfile, event failurePolicyEvent) []failurePolicyMatch {
	matched := make([]failurePolicyMatch, 0, len(profiles))
	for _, profile := range profiles {
		if profile.Enabled != nil && !*profile.Enabled {
			continue
		}
		if len(profile.Actions) == 0 {
			continue
		}
		if len(profile.Sources) == 0 {
			if event.Source == objects.FailurePolicyEventSourceManualHealthCheckFailure {
				continue
			}
		} else if !slices.Contains(profile.Sources, event.Source) {
			continue
		}
		if !failurePolicyProfileMatches(profile, event) {
			continue
		}

		matched = append(matched, failurePolicyMatch{Profile: profile, Actions: profile.Actions})
	}

	return matched
}

func failurePolicyProfileMatches(profile objects.FailurePolicyProfile, event failurePolicyEvent) bool {
	result := ChannelKeyHealthCheckResult{
		Success:    false,
		Reason:     event.Reason,
		Balance:    event.Balance,
		Currency:   event.Currency,
		Available:  event.Available,
		StatusCode: event.StatusCode,
	}
	trigger := objects.ChannelKeyHealthCheckTriggerScheduled
	switch event.Source {
	case objects.FailurePolicyEventSourceRequestFailure:
		trigger = objects.ChannelKeyHealthCheckTriggerRequest
	case objects.FailurePolicyEventSourceManualHealthCheckFailure:
		trigger = objects.ChannelKeyHealthCheckTriggerManual
	}

	return failurePolicyConditionMatches(profile.Conditions, result, event.FailureCount, trigger, event.AllCheckedKeysFailed)
}

func failurePolicyConditionMatches(
	condition objects.ChannelKeyHealthCheckPolicyCondition,
	result ChannelKeyHealthCheckResult,
	failureCount int,
	trigger objects.ChannelKeyHealthCheckTrigger,
	allCheckedKeysFailed bool,
) bool {
	if condition.MinFailureCount != nil && failureCount < *condition.MinFailureCount {
		return false
	}
	if len(condition.StatusCodes) > 0 && !slices.Contains(condition.StatusCodes, result.StatusCode) {
		return false
	}
	if condition.Available != nil {
		if result.Available == nil || *result.Available != *condition.Available {
			return false
		}
	}
	if condition.BalanceLTE != nil {
		balance, ok := channelKeyHealthCheckNumericBalance(result.Balance)
		if !ok || balance > *condition.BalanceLTE {
			return false
		}
	}
	reasonContains := strings.TrimSpace(condition.ReasonContains)
	if reasonContains != "" && !strings.Contains(strings.ToLower(result.Reason), strings.ToLower(reasonContains)) {
		return false
	}
	if condition.AllCheckedKeysFailed != nil && allCheckedKeysFailed != *condition.AllCheckedKeysFailed {
		return false
	}
	if strings.TrimSpace(condition.Expr) != "" {
		matches, _ := evaluateChannelKeyHealthCheckPolicyExpr(condition.Expr, result, failureCount, trigger, allCheckedKeysFailed)
		if !matches {
			return false
		}
	}

	return true
}

func summarizeFailurePolicyMatches(matches []failurePolicyMatch) string {
	parts := make([]string, 0, len(matches))
	for _, match := range matches {
		name := strings.TrimSpace(match.Profile.Name)
		if name == "" {
			name = match.Profile.ID
		}
		if name == "" {
			name = "failure policy"
		}
		parts = append(parts, name)
	}

	return strings.Join(parts, ",")
}

func summarizeFailurePolicyActions(matches []failurePolicyMatch) string {
	parts := make([]string, 0, len(matches))
	for _, match := range matches {
		for _, action := range match.Actions {
			if action.Type == "" {
				continue
			}
			parts = append(parts, string(action.Type))
		}
	}

	return strings.Join(parts, ",")
}

func matchesToFailurePolicyActions(matches []failurePolicyMatch) []objects.FailurePolicyAction {
	actions := make([]objects.FailurePolicyAction, 0, len(matches))
	for _, match := range matches {
		actions = append(actions, match.Actions...)
	}

	return actions
}

func (svc *ChannelService) applyFailurePolicyActions(ctx context.Context, event failurePolicyEvent, matchedPolicy string, actions []objects.FailurePolicyAction, persistBackoff bool) (bool, error) {
	routingChanged := false
	if event.Source == objects.FailurePolicyEventSourceRequestFailure {
		if err := svc.recordRequestFailurePolicyHistory(ctx, event, matchedPolicy, actions); err != nil {
			return routingChanged, err
		}
	}

	for _, action := range actions {
		reason := event.Reason
		if reason == "" {
			reason = "failure policy matched"
		}
		if matchedPolicy != "" {
			reason = fmt.Sprintf("%s: %s", matchedPolicy, reason)
		}

		switch action.Type {
		case "", objects.FailurePolicyActionReportOnly:
			continue
		case objects.FailurePolicyActionBackoffKey:
			if event.Key == "" || action.Backoff == nil {
				continue
			}
			if persistBackoff && event.Source != objects.FailurePolicyEventSourceRequestFailure {
				if err := svc.recordFailurePolicyKeyBackoff(ctx, event, matchedPolicy, actions, *action.Backoff); err != nil {
					return routingChanged, err
				}
			}
			routingChanged = true
		case objects.FailurePolicyActionDisableKey:
			if event.Key == "" {
				continue
			}
			if err := svc.DisableAPIKey(ctx, event.ChannelID, event.Key, event.StatusCode, reason); err != nil {
				return routingChanged, err
			}
			routingChanged = true
		case objects.FailurePolicyActionArchiveKey:
			if event.Key == "" {
				continue
			}
			err := svc.ArchiveChannelAPIKey(ctx, event.ChannelID, event.Key, reason)
			if errors.Is(err, errCannotArchiveLastUsableChannelAPIKey) {
				continue
			}
			if err != nil {
				return routingChanged, err
			}
			routingChanged = true
		case objects.FailurePolicyActionDeleteKey:
			if event.Key == "" {
				continue
			}
			if _, err := svc.DeleteChannelAPIKey(ctx, event.ChannelID, event.Key); err != nil {
				return routingChanged, err
			}
			routingChanged = true
		case objects.FailurePolicyActionDisableChannel:
			if event.Source == objects.FailurePolicyEventSourceRequestFailure {
				svc.markChannelUnavailable(ctx, event.ChannelID, event.StatusCode, event.FailureCount, event.FailureCount)
			} else if event.AllCheckedKeysFailed {
				if err := svc.disableChannelForKeyHealthPolicy(ctx, event.ChannelID, matchedPolicy); err != nil {
					return routingChanged, err
				}
			}
			routingChanged = true
		default:
			return routingChanged, fmt.Errorf("unsupported failure policy action %q", action.Type)
		}
	}

	return routingChanged, nil
}

func (svc *ChannelService) recordRequestFailurePolicyHistory(ctx context.Context, event failurePolicyEvent, matchedPolicy string, actions []objects.FailurePolicyAction) error {
	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, event.ChannelID)
	if err != nil {
		return fmt.Errorf("failed to get channel for request failure policy history: %w", err)
	}

	settings := ensureChannelKeyHealthCheckSettings(ch.Settings)
	now := event.CheckedAt
	if now.IsZero() {
		now = time.Now()
	}
	actionSummary := summarizeFailurePolicyActionList(actions)
	historyLimit := settings.KeyHealthCheck.HistoryLimitOrDefault()
	if event.Target == objects.FailurePolicyTargetKey && event.Key != "" {
		settings.KeyHealthCheck.KeyMetadata = upsertRequestFailurePolicyKeyMetadata(settings.KeyHealthCheck.KeyMetadata, event, matchedPolicy, actionSummary, actions, now, historyLimit)
	}
	settings.KeyHealthCheck.History = appendRequestFailurePolicyChannelHistory(settings.KeyHealthCheck.History, event, matchedPolicy, actionSummary, now, historyLimit)

	if _, err := svc.entFromContext(ctx).Channel.UpdateOneID(event.ChannelID).SetSettings(settings).Save(ctx); err != nil {
		return fmt.Errorf("failed to save request failure policy history: %w", err)
	}

	return nil
}

func upsertRequestFailurePolicyKeyMetadata(metadata []objects.ChannelKeyMetadata, event failurePolicyEvent, matchedPolicy string, actionSummary string, actions []objects.FailurePolicyAction, now time.Time, historyLimit int) []objects.ChannelKeyMetadata {
	id := objects.ChannelAPIKeyFingerprint(event.Key)
	next := slices.Clone(metadata)
	for i := range next {
		if next[i].ID != id {
			continue
		}

		next[i] = updateRequestFailurePolicyKeyMetadata(next[i], event, matchedPolicy, actionSummary, actions, now, historyLimit)

		return next
	}

	return append(next, updateRequestFailurePolicyKeyMetadata(objects.ChannelKeyMetadata{ID: id}, event, matchedPolicy, actionSummary, actions, now, historyLimit))
}

func updateRequestFailurePolicyKeyMetadata(meta objects.ChannelKeyMetadata, event failurePolicyEvent, matchedPolicy string, actionSummary string, actions []objects.FailurePolicyAction, now time.Time, historyLimit int) objects.ChannelKeyMetadata {
	meta.ID = objects.ChannelAPIKeyFingerprint(event.Key)
	meta.MaskedKey = objects.MaskChannelAPIKey(event.Key)
	if meta.Status == "" {
		meta.Status = objects.ChannelKeyStatusActive
	}
	meta.LastCheckedAt = &now
	success := false
	meta.Success = &success
	meta.FailureCount = event.FailureCount
	meta.Reason = event.Reason
	meta.StatusCode = event.StatusCode
	meta.MatchedPolicy = matchedPolicy
	meta.Action = actionSummary

	result := ChannelKeyHealthCheckResult{
		Success:       false,
		Reason:        event.Reason,
		StatusCode:    event.StatusCode,
		MatchedPolicy: matchedPolicy,
		Action:        actionSummary,
	}
	if backoff, ok := firstFailurePolicyBackoff(actions); ok {
		meta.BackoffAttempt++
		if event.FailureCount > meta.BackoffAttempt {
			meta.BackoffAttempt = event.FailureCount
		}
		nextCheckAt := now.Add(computeChannelKeyHealthCheckBackoffDuration(backoff, meta.BackoffAttempt))
		meta.NextCheckAt = &nextCheckAt
		result.NextCheckAt = &nextCheckAt
		result.BackoffAttempt = meta.BackoffAttempt
	}
	meta.History = appendChannelKeyHealthCheckHistory(meta.History, meta.ID, result, now, objects.ChannelKeyHealthCheckTriggerRequest, historyLimit)

	return meta
}

func appendRequestFailurePolicyChannelHistory(history []objects.ChannelKeyHealthCheckHistoryEntry, event failurePolicyEvent, matchedPolicy string, actionSummary string, now time.Time, historyLimit int) []objects.ChannelKeyHealthCheckHistoryEntry {
	result := ChannelKeyHealthCheckResult{
		Success:       false,
		Reason:        event.Reason,
		StatusCode:    event.StatusCode,
		MatchedPolicy: matchedPolicy,
		Action:        actionSummary,
	}
	if historyLimit <= 0 {
		historyLimit = channelKeyHealthCheckHistoryLimit
	}
	historyLimit = min(historyLimit, channelKeyHealthCheckMaxHistoryLimit)
	targetID := fmt.Sprintf("channel:%d", event.ChannelID)
	if event.Target == objects.FailurePolicyTargetKey && event.Key != "" {
		targetID = objects.ChannelAPIKeyFingerprint(event.Key)
	}
	entry := objects.ChannelKeyHealthCheckHistoryEntry{
		ID:            fmt.Sprintf("%s:%d:%s", targetID, now.UnixNano(), objects.ChannelKeyHealthCheckTriggerRequest),
		CheckedAt:     now,
		Success:       result.Success,
		Reason:        result.Reason,
		Trigger:       objects.ChannelKeyHealthCheckTriggerRequest,
		StatusCode:    result.StatusCode,
		MatchedPolicy: result.MatchedPolicy,
		Action:        result.Action,
	}
	next := append([]objects.ChannelKeyHealthCheckHistoryEntry{entry}, history...)
	if len(next) > historyLimit {
		next = next[:historyLimit]
	}

	return next
}

func firstFailurePolicyBackoff(actions []objects.FailurePolicyAction) (objects.ChannelKeyHealthCheckBackoff, bool) {
	for _, action := range actions {
		if action.Type == objects.FailurePolicyActionBackoffKey && action.Backoff != nil {
			return *action.Backoff, true
		}
	}

	return objects.ChannelKeyHealthCheckBackoff{}, false
}

func (svc *ChannelService) recordFailurePolicyKeyBackoff(ctx context.Context, event failurePolicyEvent, matchedPolicy string, actions []objects.FailurePolicyAction, backoff objects.ChannelKeyHealthCheckBackoff) error {
	if event.Key == "" {
		return nil
	}

	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, event.ChannelID)
	if err != nil {
		return fmt.Errorf("failed to get channel for key backoff: %w", err)
	}

	settings := ensureChannelKeyHealthCheckSettings(ch.Settings)
	id := objects.ChannelAPIKeyFingerprint(event.Key)
	now := event.CheckedAt
	if now.IsZero() {
		now = time.Now()
	}

	metadata := settings.KeyHealthCheck.KeyMetadata
	found := false
	for i := range metadata {
		if metadata[i].ID != id {
			continue
		}
		metadata[i] = updateFailurePolicyBackoffMetadata(metadata[i], event, matchedPolicy, actions, backoff, now)
		found = true
		break
	}
	if !found {
		metadata = append(metadata, updateFailurePolicyBackoffMetadata(objects.ChannelKeyMetadata{ID: id}, event, matchedPolicy, actions, backoff, now))
	}
	settings.KeyHealthCheck.KeyMetadata = metadata

	if _, err := svc.entFromContext(ctx).Channel.UpdateOneID(event.ChannelID).SetSettings(settings).Save(ctx); err != nil {
		return fmt.Errorf("failed to save key backoff metadata: %w", err)
	}

	return nil
}

func updateFailurePolicyBackoffMetadata(meta objects.ChannelKeyMetadata, event failurePolicyEvent, matchedPolicy string, actions []objects.FailurePolicyAction, backoff objects.ChannelKeyHealthCheckBackoff, now time.Time) objects.ChannelKeyMetadata {
	meta.ID = objects.ChannelAPIKeyFingerprint(event.Key)
	meta.MaskedKey = objects.MaskChannelAPIKey(event.Key)
	meta.Status = objects.ChannelKeyStatusActive
	meta.LastCheckedAt = &now
	success := false
	meta.Success = &success
	meta.FailureCount = event.FailureCount
	meta.Reason = event.Reason
	meta.StatusCode = event.StatusCode
	meta.MatchedPolicy = matchedPolicy
	meta.Action = summarizeFailurePolicyActionList(actions)
	meta.BackoffAttempt++
	if event.FailureCount > meta.BackoffAttempt {
		meta.BackoffAttempt = event.FailureCount
	}
	nextCheckAt := now.Add(computeChannelKeyHealthCheckBackoffDuration(backoff, meta.BackoffAttempt))
	meta.NextCheckAt = &nextCheckAt

	return meta
}

func summarizeFailurePolicyActionList(actions []objects.FailurePolicyAction) string {
	parts := make([]string, 0, len(actions))
	for _, action := range actions {
		if action.Type == "" {
			continue
		}
		parts = append(parts, string(action.Type))
	}

	return strings.Join(parts, ",")
}

func failurePolicyEventSourceFromHealthTrigger(trigger objects.ChannelKeyHealthCheckTrigger) objects.FailurePolicyEventSource {
	if trigger == objects.ChannelKeyHealthCheckTriggerManual {
		return objects.FailurePolicyEventSourceManualHealthCheckFailure
	}

	return objects.FailurePolicyEventSourceScheduledHealthCheckFailure
}

func (svc *ChannelService) applyFailurePolicyToHealthCheckResults(
	ctx context.Context,
	ch *ent.Channel,
	settings *objects.ChannelSettings,
	keys []string,
	results []ChannelKeyHealthCheckResult,
	now time.Time,
	trigger objects.ChannelKeyHealthCheckTrigger,
	allCheckedKeysFailed bool,
) []ChannelKeyHealthCheckResult {
	if ch == nil || settings == nil {
		return results
	}

	retry := svc.SystemService.RetryPolicyOrDefault(ctx)
	effective := resolveEffectiveFailurePolicy(retry, settings, true)
	if len(effective.KeyProfiles) == 0 && len(effective.ChannelProfiles) == 0 {
		return results
	}

	next := slices.Clone(results)
	source := failurePolicyEventSourceFromHealthTrigger(trigger)
	metadataByID := channelKeyMetadataByID(settings.KeyHealthCheck.KeyMetadata)
	failedIndexes := make([]int, 0, len(keys))
	for i, key := range keys {
		if i >= len(next) || next[i].Success {
			continue
		}
		failedIndexes = append(failedIndexes, i)
		meta := metadataByID[objects.ChannelAPIKeyFingerprint(key)]
		failureCount := meta.FailureCount + 1
		event := failurePolicyEvent{
			Source:               source,
			Target:               objects.FailurePolicyTargetKey,
			ChannelID:            ch.ID,
			Key:                  key,
			StatusCode:           next[i].StatusCode,
			FailureCount:         failureCount,
			Available:            next[i].Available,
			Balance:              next[i].Balance,
			Currency:             next[i].Currency,
			Reason:               next[i].Reason,
			AllCheckedKeysFailed: allCheckedKeysFailed,
			CheckedAt:            now,
		}
		matches := evaluateFailurePolicyProfiles(effective.KeyProfiles, event)
		if len(matches) == 0 {
			continue
		}
		next[i].MatchedPolicy = appendSummary(next[i].MatchedPolicy, summarizeFailurePolicyMatches(matches))
		next[i].Action = appendSummary(next[i].Action, summarizeFailurePolicyActions(matches))
		next[i].BackoffAttempt = meta.BackoffAttempt
		for _, action := range matchesToFailurePolicyActions(matches) {
			if action.Type != objects.FailurePolicyActionBackoffKey || action.Backoff == nil {
				continue
			}
			next[i].BackoffAttempt = meta.BackoffAttempt + 1
			nextCheckAt := now.Add(computeChannelKeyHealthCheckBackoffDuration(*action.Backoff, next[i].BackoffAttempt))
			next[i].NextCheckAt = &nextCheckAt
		}
	}

	if allCheckedKeysFailed && len(failedIndexes) > 0 {
		event := failurePolicyEvent{
			Source:               source,
			Target:               objects.FailurePolicyTargetChannel,
			ChannelID:            ch.ID,
			StatusCode:           next[failedIndexes[0]].StatusCode,
			FailureCount:         len(failedIndexes),
			Reason:               "all checked keys failed",
			AllCheckedKeysFailed: true,
			CheckedAt:            now,
		}
		matches := evaluateFailurePolicyProfiles(effective.ChannelProfiles, event)
		if len(matches) > 0 {
			i := failedIndexes[0]
			next[i].MatchedPolicy = appendSummary(next[i].MatchedPolicy, summarizeFailurePolicyMatches(matches))
			next[i].Action = appendSummary(next[i].Action, summarizeFailurePolicyActions(matches))
		}
	}

	return next
}

func appendSummary(current, extra string) string {
	current = strings.TrimSpace(current)
	extra = strings.TrimSpace(extra)
	if current == "" {
		return extra
	}
	if extra == "" {
		return current
	}

	return current + "," + extra
}
