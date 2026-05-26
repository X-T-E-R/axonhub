package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelkeymonitoringevent"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
)

type monitoringTargetKey struct {
	RawKey    string
	ID        string
	MaskedKey string
	Status    objects.ChannelKeyStatus
}

func (svc *ChannelService) runDueMonitoringRules(ctx context.Context, now time.Time) (int, int, error) {
	settings := svc.SystemService.MonitoringSettingsOrDefault(ctx)
	if settings == nil || !settings.Enabled || len(settings.Rules) == 0 {
		return 0, 0, nil
	}
	if err := svc.pruneMonitoringEvents(ctx, settings.HistoryRetentionDays, now); err != nil {
		return 0, 0, err
	}

	dueRules := make([]MonitoringRule, 0, len(settings.Rules))
	for _, rule := range settings.Rules {
		if rule.Enabled != nil && !*rule.Enabled {
			continue
		}
		due, err := svc.monitoringRuleDue(ctx, rule, now)
		if err != nil {
			return 0, 0, err
		}
		if due {
			dueRules = append(dueRules, rule)
		}
	}
	if len(dueRules) == 0 {
		return 0, 0, nil
	}

	var checked atomic.Int32
	var failed atomic.Int32
	for _, rule := range dueRules {
		result, err := svc.runMonitoringRule(ctx, rule, now)
		if err != nil {
			failed.Add(1)
			log.Warn(ctx, "monitoring rule failed",
				log.String("rule_id", rule.ID),
				log.String("rule_name", rule.Name),
				log.Cause(err),
			)
			continue
		}

		checked.Add(int32(result.CheckedKeys))
		failed.Add(int32(result.FailedKeys))
	}

	return int(checked.Load()), int(failed.Load()), nil
}

func (svc *ChannelService) pruneMonitoringEvents(ctx context.Context, retentionDays int, now time.Time) error {
	retentionDays = clampInt(retentionDays, 1, 3650)
	cutoff := now.Add(-time.Duration(retentionDays) * 24 * time.Hour)
	if _, err := svc.entFromContext(ctx).ChannelKeyMonitoringEvent.Delete().
		Where(channelkeymonitoringevent.CheckedAtLT(cutoff)).
		Exec(ctx); err != nil {
		return fmt.Errorf("failed to prune monitoring events: %w", err)
	}

	return nil
}

func (svc *ChannelService) monitoringRuleDue(ctx context.Context, rule MonitoringRule, now time.Time) (bool, error) {
	latest, err := svc.entFromContext(ctx).ChannelKeyMonitoringEvent.Query().
		Where(channelkeymonitoringevent.RuleIDEQ(rule.ID)).
		Order(ent.Desc(channelkeymonitoringevent.FieldCheckedAt)).
		First(ctx)
	if ent.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to query monitoring rule history: %w", err)
	}

	return !now.Before(latest.CheckedAt.Add(time.Duration(rule.Schedule.IntervalMinutes) * time.Minute)), nil
}

func (svc *ChannelService) runMonitoringRule(ctx context.Context, rule MonitoringRule, now time.Time) (channelKeyHealthCheckChannelResult, error) {
	channels, err := svc.monitoringRuleTargetChannels(ctx, rule)
	if err != nil {
		return channelKeyHealthCheckChannelResult{}, err
	}
	if len(channels) == 0 {
		return channelKeyHealthCheckChannelResult{}, nil
	}

	var checked atomic.Int32
	var failed atomic.Int32
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(rule.Schedule.MaxChannels)

	for _, ch := range channels {
		channelEntity := ch
		group.Go(func() error {
			result, err := svc.runMonitoringRuleForChannel(groupCtx, rule, channelEntity, now)
			if err != nil {
				failed.Add(1)
				log.Warn(groupCtx, "monitoring rule channel failed",
					log.String("rule_id", rule.ID),
					log.Int("channel_id", channelEntity.ID),
					log.Cause(err),
				)

				return nil
			}

			checked.Add(int32(result.CheckedKeys))
			failed.Add(int32(result.FailedKeys))

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return channelKeyHealthCheckChannelResult{}, err
	}

	return channelKeyHealthCheckChannelResult{CheckedKeys: int(checked.Load()), FailedKeys: int(failed.Load())}, nil
}

func (svc *ChannelService) monitoringRuleTargetChannels(ctx context.Context, rule MonitoringRule) ([]*ent.Channel, error) {
	query := svc.entFromContext(ctx).Channel.Query()
	if len(rule.Targets.ChannelIDs) > 0 {
		query.Where(channel.IDIn(rule.Targets.ChannelIDs...))
	}
	if len(rule.Targets.ChannelStatuses) > 0 {
		statuses := make([]channel.Status, 0, len(rule.Targets.ChannelStatuses))
		for _, status := range rule.Targets.ChannelStatuses {
			statuses = append(statuses, channel.Status(status))
		}
		query.Where(channel.StatusIn(statuses...))
	}

	channels, err := query.All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query monitoring target channels: %w", err)
	}

	return channels, nil
}

func (svc *ChannelService) runMonitoringRuleForChannel(ctx context.Context, rule MonitoringRule, ch *ent.Channel, now time.Time) (channelKeyHealthCheckChannelResult, error) {
	if ch == nil {
		return channelKeyHealthCheckChannelResult{}, nil
	}
	if ch.Credentials.IsOAuth() {
		return svc.recordMonitoringSkippedChannel(ctx, rule, ch, "OAuth channels do not expose raw API keys for monitoring", now)
	}

	targets := monitoringRuleTargetKeys(ch, rule.Targets.KeyStatuses)
	if len(targets) == 0 {
		return channelKeyHealthCheckChannelResult{}, nil
	}
	if !rule.Targets.IncludeBackoff {
		targets = filterMonitoringTargetsDue(ch, targets, now)
	}
	if len(targets) > rule.Schedule.MaxKeysPerChannel {
		targets = targets[:rule.Schedule.MaxKeysPerChannel]
	}
	if len(targets) == 0 {
		return channelKeyHealthCheckChannelResult{}, nil
	}

	settings := ensureChannelKeyHealthCheckSettings(ch.Settings)
	chForCheck := *ch
	chForCheck.Settings = cloneChannelSettings(settings)
	chForCheck.Settings.KeyHealthCheck.Rules = slices.Clone(rule.Probes)
	chForCheck.Settings.KeyHealthCheck.HistoryLimit = rule.Schedule.HistoryLimit

	results := make([]ChannelKeyHealthCheckResult, len(targets))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(min(rule.Schedule.MaxKeysPerChannel, len(targets)))
	for i, target := range targets {
		index := i
		targetKey := target
		if targetKey.RawKey == "" {
			results[index] = ChannelKeyHealthCheckResult{Success: false, Reason: "raw provider key is not available"}
			continue
		}
		if err := waitBeforeChannelKeyHealthCheck(groupCtx, index, targetKey.RawKey); err != nil {
			if waitErr := group.Wait(); waitErr != nil {
				return channelKeyHealthCheckChannelResult{}, waitErr
			}

			return channelKeyHealthCheckChannelResult{}, err
		}
		group.Go(func() error {
			results[index] = svc.checkChannelAPIKeyHealth(groupCtx, &chForCheck, targetKey.RawKey)

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return channelKeyHealthCheckChannelResult{}, err
	}

	failed := 0
	for i := range targets {
		if !results[i].Success {
			failed++
		}
	}
	allCheckedKeysFailed := len(targets) > 0 && failed == len(targets)
	results = svc.applyMonitoringRuleProfiles(ctx, rule, ch, settings, targets, results, now, allCheckedKeysFailed)
	for i, target := range targets {
		if target.RawKey == "" {
			if err := svc.appendMonitoringEvent(ctx, monitoringEventInput{
				Rule:      rule,
				Channel:   ch,
				Target:    target,
				Result:    results[i],
				Trigger:   objects.ChannelKeyHealthCheckTriggerScheduled,
				Skipped:   true,
				CheckedAt: now,
			}); err != nil {
				return channelKeyHealthCheckChannelResult{}, err
			}
			continue
		}
		settings.KeyHealthCheck.KeyMetadata = upsertChannelKeyHealthCheckMetadata(settings.KeyHealthCheck.KeyMetadata, target.RawKey, results[i], now, objects.ChannelKeyHealthCheckTriggerScheduled, rule.Schedule.HistoryLimit)
		if err := svc.appendMonitoringEvent(ctx, monitoringEventInput{
			Rule:      rule,
			Channel:   ch,
			Target:    target,
			Result:    results[i],
			Trigger:   objects.ChannelKeyHealthCheckTriggerScheduled,
			CheckedAt: now,
		}); err != nil {
			return channelKeyHealthCheckChannelResult{}, err
		}
	}
	mergeChannelKeyOperationalStatus(ch, settings.KeyHealthCheck)

	if _, err := svc.entFromContext(ctx).Channel.UpdateOneID(ch.ID).SetSettings(settings).Save(ctx); err != nil {
		return channelKeyHealthCheckChannelResult{}, fmt.Errorf("failed to save monitoring key metadata: %w", err)
	}

	rawKeys := make([]string, 0, len(targets))
	for _, target := range targets {
		rawKeys = append(rawKeys, target.RawKey)
	}
	if err := svc.applyChannelKeyHealthCheckPolicyActions(ctx, ch.ID, rawKeys, results, allCheckedKeysFailed); err != nil {
		return channelKeyHealthCheckChannelResult{}, err
	}

	if failed > 0 || monitoringResultsHaveRoutingAction(results) {
		svc.asyncReloadChannels()
	}

	return channelKeyHealthCheckChannelResult{CheckedKeys: len(targets), FailedKeys: failed}, nil
}

func monitoringResultsHaveRoutingAction(results []ChannelKeyHealthCheckResult) bool {
	for _, result := range results {
		for _, action := range strings.Split(result.Action, ",") {
			switch strings.TrimSpace(action) {
			case string(objects.FailurePolicyActionDisableKey),
				string(objects.FailurePolicyActionArchiveKey),
				string(objects.FailurePolicyActionDeleteKey),
				string(objects.FailurePolicyActionDisableChannel),
				string(objects.FailurePolicyActionEnableKey),
				string(objects.FailurePolicyActionRestoreKey):
				return true
			}
		}
	}

	return false
}

func (svc *ChannelService) applyMonitoringRuleProfiles(
	ctx context.Context,
	rule MonitoringRule,
	ch *ent.Channel,
	settings *objects.ChannelSettings,
	targets []monitoringTargetKey,
	results []ChannelKeyHealthCheckResult,
	now time.Time,
	allCheckedKeysFailed bool,
) []ChannelKeyHealthCheckResult {
	if len(rule.KeyProfiles) == 0 && len(rule.ChannelProfiles) == 0 {
		return results
	}

	next := slices.Clone(results)
	metadataByID := channelKeyMetadataByID(settings.KeyHealthCheck.KeyMetadata)
	for i, target := range targets {
		if i >= len(next) || target.RawKey == "" {
			continue
		}
		meta := metadataByID[target.ID]
		failureCount := meta.FailureCount
		if !next[i].Success {
			failureCount++
		}
		source := objects.FailurePolicyEventSourceScheduledHealthCheck
		if !next[i].Success {
			source = objects.FailurePolicyEventSourceScheduledHealthCheckFailure
		}
		event := failurePolicyEvent{
			Source:               source,
			Target:               objects.FailurePolicyTargetKey,
			ChannelID:            ch.ID,
			Key:                  target.RawKey,
			StatusCode:           next[i].StatusCode,
			FailureCount:         failureCount,
			Success:              next[i].Success,
			KeyStatus:            target.Status,
			Available:            next[i].Available,
			Balance:              next[i].Balance,
			Currency:             next[i].Currency,
			Reason:               next[i].Reason,
			AllCheckedKeysFailed: allCheckedKeysFailed,
			CheckedAt:            now,
		}
		matches := evaluateFailurePolicyProfiles(rule.KeyProfiles, event)
		if len(matches) == 0 {
			continue
		}
		matchedPolicy := summarizeFailurePolicyMatches(matches)
		actions := matchesToFailurePolicyActions(matches)
		next[i].MatchedPolicy = appendSummary(next[i].MatchedPolicy, matchedPolicy)
		next[i].Action = appendSummary(next[i].Action, summarizeFailurePolicyActionList(actions))
		next[i].BackoffAttempt = meta.BackoffAttempt
		for _, action := range actions {
			if action.Type != objects.FailurePolicyActionBackoffKey || action.Backoff == nil {
				continue
			}
			next[i].BackoffAttempt = meta.BackoffAttempt + 1
			nextCheckAt := now.Add(computeChannelKeyHealthCheckBackoffDuration(*action.Backoff, next[i].BackoffAttempt))
			next[i].NextCheckAt = &nextCheckAt
		}
	}

	if allCheckedKeysFailed && len(rule.ChannelProfiles) > 0 {
		event := failurePolicyEvent{
			Source:               objects.FailurePolicyEventSourceScheduledHealthCheckFailure,
			Target:               objects.FailurePolicyTargetChannel,
			ChannelID:            ch.ID,
			FailureCount:         len(targets),
			Success:              false,
			Reason:               "all checked keys failed",
			AllCheckedKeysFailed: true,
			CheckedAt:            now,
		}
		matches := evaluateFailurePolicyProfiles(rule.ChannelProfiles, event)
		if len(matches) > 0 {
			i := firstFailedMonitoringResult(next)
			if i >= 0 {
				matchedPolicy := summarizeFailurePolicyMatches(matches)
				actions := matchesToFailurePolicyActions(matches)
				next[i].MatchedPolicy = appendSummary(next[i].MatchedPolicy, matchedPolicy)
				next[i].Action = appendSummary(next[i].Action, summarizeFailurePolicyActionList(actions))
			}
		}
	}

	return next
}

func firstFailedMonitoringResult(results []ChannelKeyHealthCheckResult) int {
	for i, result := range results {
		if !result.Success {
			return i
		}
	}

	return -1
}

func monitoringRuleTargetKeys(ch *ent.Channel, statuses []objects.ChannelKeyStatus) []monitoringTargetKey {
	if ch == nil {
		return nil
	}
	if len(statuses) == 0 {
		statuses = []objects.ChannelKeyStatus{objects.ChannelKeyStatusActive}
	}
	include := make(map[objects.ChannelKeyStatus]struct{}, len(statuses))
	for _, status := range statuses {
		include[status] = struct{}{}
	}

	disabled := make(map[string]struct{}, len(ch.DisabledAPIKeys))
	for _, item := range ch.DisabledAPIKeys {
		if item.Key != "" {
			disabled[item.Key] = struct{}{}
		}
	}
	archived := make(map[string]objects.ChannelArchivedAPIKey)
	for _, item := range channelArchivedAPIKeys(ch.Settings) {
		if item.ID != "" {
			archived[item.ID] = item
		}
	}

	targets := make([]monitoringTargetKey, 0, len(ch.Credentials.GetAllAPIKeys())+len(archived))
	seen := make(map[string]struct{})
	for _, key := range ch.Credentials.GetAllAPIKeys() {
		id := objects.ChannelAPIKeyFingerprint(key)
		status := objects.ChannelKeyStatusActive
		if _, ok := disabled[key]; ok {
			status = objects.ChannelKeyStatusDisabled
		}
		if _, ok := archived[id]; ok {
			status = objects.ChannelKeyStatusArchived
		}
		if _, ok := include[status]; !ok {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		targets = append(targets, monitoringTargetKey{
			RawKey:    key,
			ID:        id,
			MaskedKey: objects.MaskChannelAPIKey(key),
			Status:    status,
		})
	}

	if _, ok := include[objects.ChannelKeyStatusArchived]; ok {
		for _, item := range archived {
			if _, ok := seen[item.ID]; ok {
				continue
			}
			targets = append(targets, monitoringTargetKey{
				ID:        item.ID,
				MaskedKey: item.MaskedKey,
				Status:    objects.ChannelKeyStatusArchived,
			})
		}
	}

	return targets
}

func filterMonitoringTargetsDue(ch *ent.Channel, targets []monitoringTargetKey, now time.Time) []monitoringTargetKey {
	if ch == nil || ch.Settings == nil || ch.Settings.KeyHealthCheck == nil {
		return targets
	}

	metadataByID := channelKeyMetadataByID(ch.Settings.KeyHealthCheck.KeyMetadata)
	due := make([]monitoringTargetKey, 0, len(targets))
	for _, target := range targets {
		meta, ok := metadataByID[target.ID]
		if !ok || meta.NextCheckAt == nil || !now.Before(*meta.NextCheckAt) {
			due = append(due, target)
		}
	}

	return due
}

func (svc *ChannelService) recordMonitoringSkippedChannel(ctx context.Context, rule MonitoringRule, ch *ent.Channel, reason string, now time.Time) (channelKeyHealthCheckChannelResult, error) {
	err := svc.appendMonitoringEvent(ctx, monitoringEventInput{
		Rule:    rule,
		Channel: ch,
		Result: ChannelKeyHealthCheckResult{
			Success: false,
			Reason:  reason,
		},
		Trigger:   objects.ChannelKeyHealthCheckTriggerScheduled,
		Skipped:   true,
		CheckedAt: now,
	})
	if err != nil {
		return channelKeyHealthCheckChannelResult{}, err
	}

	return channelKeyHealthCheckChannelResult{}, nil
}

type monitoringEventInput struct {
	Rule      MonitoringRule
	Channel   *ent.Channel
	Target    monitoringTargetKey
	Result    ChannelKeyHealthCheckResult
	Trigger   objects.ChannelKeyHealthCheckTrigger
	Skipped   bool
	CheckedAt time.Time
}

func (svc *ChannelService) appendMonitoringEvent(ctx context.Context, input monitoringEventInput) error {
	if input.Channel == nil {
		return nil
	}
	checkedAt := input.CheckedAt
	if checkedAt.IsZero() {
		checkedAt = time.Now()
	}

	create := svc.entFromContext(ctx).ChannelKeyMonitoringEvent.Create().
		SetChannelID(input.Channel.ID).
		SetChannelName(input.Channel.Name).
		SetTrigger(string(input.Trigger)).
		SetSource(string(input.Trigger)).
		SetSuccess(input.Result.Success).
		SetSkipped(input.Skipped).
		SetReason(sanitizeMonitoringEventText(input.Result.Reason, input.Target)).
		SetProbe(input.Result.Rule).
		SetMatchedPolicy(input.Result.MatchedPolicy).
		SetAction(input.Result.Action).
		SetBackoffAttempt(input.Result.BackoffAttempt).
		SetCheckedAt(checkedAt)

	if strings.TrimSpace(input.Rule.ID) != "" {
		create.SetRuleID(input.Rule.ID)
	}
	if strings.TrimSpace(input.Rule.Name) != "" {
		create.SetRuleName(input.Rule.Name)
	}
	if input.Target.ID != "" {
		create.SetKeyID(input.Target.ID)
	}
	if input.Target.MaskedKey != "" {
		create.SetMaskedKey(input.Target.MaskedKey)
	}
	if input.Result.StatusCode != 0 {
		create.SetStatusCode(input.Result.StatusCode)
	}
	if input.Result.Balance != nil {
		balance, err := monitoringEventBalance(input.Result.Balance)
		if err == nil && len(balance) > 0 {
			create.SetBalance(balance)
		}
	}
	if strings.TrimSpace(input.Result.Currency) != "" {
		create.SetCurrency(input.Result.Currency)
	}
	if input.Result.Available != nil {
		create.SetAvailable(*input.Result.Available)
	}
	if input.Result.NextCheckAt != nil {
		create.SetNextCheckAt(*input.Result.NextCheckAt)
	}

	if _, err := create.Save(ctx); err != nil {
		return fmt.Errorf("failed to append monitoring event: %w", err)
	}

	return nil
}

func sanitizeMonitoringEventText(text string, target monitoringTargetKey) string {
	if text == "" || target.RawKey == "" {
		return text
	}
	replacement := target.MaskedKey
	if replacement == "" {
		replacement = "[redacted-key]"
	}

	return strings.ReplaceAll(text, target.RawKey, replacement)
}

func monitoringEventBalance(value any) (objects.JSONRawMessage, error) {
	if raw, ok := value.(objects.JSONRawMessage); ok {
		return raw, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}

	return objects.JSONRawMessage(data), nil
}
