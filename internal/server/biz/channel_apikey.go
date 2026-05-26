package biz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
)

var errCannotArchiveLastUsableChannelAPIKey = errors.New("cannot archive the last usable channel api key")

type ChannelAPIKeyInventoryItem struct {
	ID              string
	MaskedKey       string
	Status          objects.ChannelKeyStatus
	LastCheckedAt   *time.Time
	Success         *bool
	FailureCount    int
	Reason          string
	Balance         any
	Currency        string
	BalanceSnapshot *objects.ChannelKeyBalanceSnapshot
	Available       *bool
	StatusCode      int
	MatchedPolicy   string
	Action          string
	NextCheckAt     *time.Time
	BackoffAttempt  int
	History         []objects.ChannelKeyHealthCheckHistoryEntry
}

func (svc *ChannelService) ChannelAPIKeyInventory(ctx context.Context, channelID int) ([]*ChannelAPIKeyInventoryItem, error) {
	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, channelID)
	if err != nil {
		return nil, fmt.Errorf("failed to get channel: %w", err)
	}

	disabled := make(map[string]objects.DisabledAPIKey, len(ch.DisabledAPIKeys))
	for _, dk := range ch.DisabledAPIKeys {
		if dk.Key != "" {
			disabled[dk.Key] = dk
		}
	}

	metadata := make(map[string]objects.ChannelKeyMetadata)
	if ch.Settings != nil && ch.Settings.KeyHealthCheck != nil {
		for _, item := range ch.Settings.KeyHealthCheck.KeyMetadata {
			if item.ID != "" {
				metadata[item.ID] = item
			}
		}
	}

	archived := make(map[string]objects.ChannelArchivedAPIKey)
	for _, item := range channelArchivedAPIKeys(ch.Settings) {
		if item.ID != "" {
			archived[item.ID] = item
		}
	}

	allKeys := ch.Credentials.GetAllAPIKeys()
	items := make([]*ChannelAPIKeyInventoryItem, 0, len(allKeys)+len(archived))
	for _, key := range allKeys {
		id := objects.ChannelAPIKeyFingerprint(key)
		status := objects.ChannelKeyStatusActive
		if _, ok := disabled[key]; ok {
			status = objects.ChannelKeyStatusDisabled
		}
		if _, ok := archived[id]; ok {
			status = objects.ChannelKeyStatusArchived
		}

		item := &ChannelAPIKeyInventoryItem{
			ID:        id,
			MaskedKey: objects.MaskChannelAPIKey(key),
			Status:    status,
		}
		if meta, ok := metadata[id]; ok {
			mergeInventoryMetadata(item, meta)
		}
		if dk, ok := disabled[key]; ok && item.Reason == "" {
			item.LastCheckedAt = &dk.DisabledAt
			item.Reason = dk.Reason
		}
		if archive, ok := archived[id]; ok {
			mergeInventoryArchive(item, archive)
		}

		items = append(items, item)
	}

	known := make(map[string]struct{}, len(items))
	for _, item := range items {
		known[item.ID] = struct{}{}
	}
	for _, archive := range archived {
		if _, ok := known[archive.ID]; ok {
			continue
		}

		item := &ChannelAPIKeyInventoryItem{
			ID:        archive.ID,
			MaskedKey: archive.MaskedKey,
			Status:    objects.ChannelKeyStatusArchived,
		}
		mergeInventoryArchive(item, archive)
		items = append(items, item)
	}

	return items, nil
}

func (svc *ChannelService) AddChannelAPIKey(ctx context.Context, channelID int, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("api key cannot be empty")
	}

	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, channelID)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}
	if ch.Credentials.IsOAuth() {
		return fmt.Errorf("cannot add API keys for OAuth channels")
	}
	if slices.Contains(ch.Credentials.GetAllAPIKeys(), key) {
		return nil
	}

	credentials := ch.Credentials
	credentials.APIKeys = append(credentials.APIKeys, key)

	if _, err := svc.entFromContext(ctx).Channel.UpdateOneID(channelID).SetCredentials(credentials).Save(ctx); err != nil {
		return fmt.Errorf("failed to add channel api key: %w", err)
	}

	svc.asyncReloadChannels()

	return nil
}

func (svc *ChannelService) DeleteChannelAPIKey(ctx context.Context, channelID int, keyID string) (*DeleteDisabledAPIKeysResult, error) {
	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, channelID)
	if err != nil {
		return nil, fmt.Errorf("failed to get channel: %w", err)
	}
	if ch.Credentials.IsOAuth() {
		return nil, fmt.Errorf("cannot delete API keys for OAuth channels")
	}

	key, ok := resolveChannelAPIKey(ch.Credentials, keyID)
	if !ok {
		if channelKeySettingsHasKeyID(ch.Settings, normalizeChannelKeyID(keyID)) {
			settings := removeChannelKeySettingsMetadata(ch.Settings, normalizeChannelKeyID(keyID))
			if _, err := svc.entFromContext(ctx).Channel.UpdateOneID(channelID).SetSettings(settings).Save(ctx); err != nil {
				return nil, fmt.Errorf("failed to delete archived channel api key: %w", err)
			}

			svc.asyncReloadChannels()
		}

		return &DeleteDisabledAPIKeysResult{Success: true}, nil
	}

	credentials := removeChannelCredentialKey(ch.Credentials, key)
	disabled := removeDisabledAPIKey(ch.DisabledAPIKeys, key)
	settings := removeChannelKeySettingsMetadata(ch.Settings, objects.ChannelAPIKeyFingerprint(key))
	remaining := credentials.GetRoutableAPIKeys(disabled, channelArchivedAPIKeys(settings))
	result := &DeleteDisabledAPIKeysResult{Success: true}
	if len(remaining) == 0 {
		result.Message = "ONE_KEY_PRESERVED"
		return result, nil
	}

	if _, err := svc.entFromContext(ctx).Channel.UpdateOneID(channelID).
		SetCredentials(credentials).
		SetDisabledAPIKeys(disabled).
		SetSettings(settings).
		Save(ctx); err != nil {
		return nil, fmt.Errorf("failed to delete channel api key: %w", err)
	}

	svc.asyncReloadChannels()

	return result, nil
}

func (svc *ChannelService) ArchiveChannelAPIKey(ctx context.Context, channelID int, keyID string, reason string) error {
	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, channelID)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	key, ok := resolveChannelAPIKey(ch.Credentials, keyID)
	if !ok {
		return nil
	}

	settings := ensureChannelKeyHealthCheckSettings(ch.Settings)
	id := objects.ChannelAPIKeyFingerprint(key)
	for _, archived := range settings.KeyHealthCheck.ArchivedKeys {
		if archived.ID == id {
			return nil
		}
	}

	now := time.Now()
	archivedKey := objects.ChannelArchivedAPIKey{
		ID:         id,
		MaskedKey:  objects.MaskChannelAPIKey(key),
		ArchivedAt: &now,
		Reason:     reason,
	}
	for _, meta := range settings.KeyHealthCheck.KeyMetadata {
		if meta.ID != id {
			continue
		}

		archivedKey.LastCheckedAt = meta.LastCheckedAt
		archivedKey.FailureCount = meta.FailureCount
		archivedKey.Balance = meta.Balance
		archivedKey.Currency = meta.Currency
		archivedKey.BalanceSnapshot = cloneChannelKeyBalanceSnapshot(meta.BalanceSnapshot)
		archivedKey.Available = meta.Available
		if archivedKey.Reason == "" {
			archivedKey.Reason = meta.Reason
		}
		break
	}
	nextArchived := append(settings.KeyHealthCheck.ArchivedKeys, archivedKey)
	settings.KeyHealthCheck.ArchivedKeys = nextArchived

	if len(ch.Credentials.GetRoutableAPIKeys(ch.DisabledAPIKeys, nextArchived)) == 0 {
		return errCannotArchiveLastUsableChannelAPIKey
	}

	if _, err := svc.entFromContext(ctx).Channel.UpdateOneID(channelID).SetSettings(settings).Save(ctx); err != nil {
		return fmt.Errorf("failed to archive channel api key: %w", err)
	}

	svc.asyncReloadChannels()

	return nil
}

func (svc *ChannelService) RestoreChannelAPIKey(ctx context.Context, channelID int, keyID string) error {
	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, channelID)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}
	if ch.Settings == nil || ch.Settings.KeyHealthCheck == nil || len(ch.Settings.KeyHealthCheck.ArchivedKeys) == 0 {
		return nil
	}

	id := normalizeChannelKeyID(keyID)
	settings := cloneChannelSettings(ch.Settings)
	next := make([]objects.ChannelArchivedAPIKey, 0, len(settings.KeyHealthCheck.ArchivedKeys))
	changed := false
	for _, archived := range settings.KeyHealthCheck.ArchivedKeys {
		if archived.ID == id {
			changed = true
			continue
		}
		next = append(next, archived)
	}
	if !changed {
		return nil
	}
	settings.KeyHealthCheck.ArchivedKeys = next

	if _, err := svc.entFromContext(ctx).Channel.UpdateOneID(channelID).SetSettings(settings).Save(ctx); err != nil {
		return fmt.Errorf("failed to restore channel api key: %w", err)
	}

	svc.asyncReloadChannels()

	return nil
}

// DisableAPIKey 禁用指定 key；若所有 key 都不可用则禁用 channel.
func (svc *ChannelService) DisableAPIKey(ctx context.Context, channelID int, key string, errorCode int, reason string) error {
	if key == "" {
		return fmt.Errorf("api key cannot be empty")
	}

	// 读取 channel
	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, channelID)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	resolvedKey, found := resolveChannelAPIKey(ch.Credentials, key)
	if !found {
		// key 不在 credentials 中，忽略
		return nil
	}
	key = resolvedKey

	disabled := lo.ContainsBy(ch.DisabledAPIKeys, func(dk objects.DisabledAPIKey) bool {
		return dk.Key == key
	})

	if disabled {
		// 已禁用，忽略
		return nil
	}

	// 追加到 disabled_api_keys
	disabledKey := objects.DisabledAPIKey{
		Key:        key,
		DisabledAt: time.Now(),
		ErrorCode:  errorCode,
		Reason:     reason,
	}

	newDisabledKeys := append(ch.DisabledAPIKeys, disabledKey)

	// 计算 enabled keys
	enabledKeys := ch.Credentials.GetRoutableAPIKeys(newDisabledKeys, channelArchivedAPIKeys(ch.Settings))

	// 更新 channel
	update := svc.entFromContext(ctx).Channel.UpdateOneID(channelID).
		SetDisabledAPIKeys(newDisabledKeys)

	// 如果没有可用 key 了，禁用整个 channel
	channelDisabled := len(enabledKeys) == 0
	if channelDisabled {
		update.SetStatus(channel.StatusDisabled)
		update.SetErrorMessage(fmt.Sprintf("All API keys disabled (last error: %d)", errorCode))
		log.Warn(ctx, "Channel disabled because all API keys are disabled",
			log.Int("channel_id", channelID),
			log.String("channel_name", ch.Name),
		)
	}

	if _, err := update.Save(ctx); err != nil {
		return fmt.Errorf("failed to disable api key: %w", err)
	}

	log.Info(ctx, "API key disabled",
		log.Int("channel_id", channelID),
		log.Int("error_code", errorCode),
	)

	if channelDisabled {
		// Synchronously reload the local cache to immediately stop selecting this channel.
		// This matches the behavior of markChannelUnavailable.
		reloadCtx, cancel := xcontext.DetachWithTimeout(ctx, 10*time.Second)
		defer cancel()

		if err := svc.enabledChannelsCache.Load(reloadCtx, true); err != nil {
			log.Warn(ctx, "Failed to synchronously reload channels after API key exhaustion",
				log.Int("channel_id", channelID),
				log.Cause(err),
			)
		}
	}

	// Also notify other instances via the watcher for cross-instance cache invalidation.
	svc.asyncReloadChannels()

	return nil
}

// EnableAPIKey 重新启用指定 key（从 disabled_api_keys 中移除）.
func (svc *ChannelService) EnableAPIKey(ctx context.Context, channelID int, key string) error {
	// 读取 channel
	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, channelID)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}
	if resolvedKey, ok := resolveChannelAPIKey(ch.Credentials, key); ok {
		key = resolvedKey
	}

	if len(ch.DisabledAPIKeys) == 0 {
		// 没有禁用的 key，忽略
		return nil
	}

	// 从 disabled_api_keys 中移除指定 key
	newDisabledKeys := make([]objects.DisabledAPIKey, 0, len(ch.DisabledAPIKeys))
	found := false

	for _, dk := range ch.DisabledAPIKeys {
		if dk.Key == key {
			found = true
			continue
		}

		newDisabledKeys = append(newDisabledKeys, dk)
	}

	if !found {
		// key 不在禁用列表中，忽略
		return nil
	}

	// 更新 channel
	update := svc.entFromContext(ctx).Channel.UpdateOneID(channelID).
		SetDisabledAPIKeys(newDisabledKeys)

	if _, err := update.Save(ctx); err != nil {
		return fmt.Errorf("failed to enable api key: %w", err)
	}

	svc.asyncReloadChannels()

	return nil
}

// EnableAllAPIKeys 清空 disabled_api_keys.
func (svc *ChannelService) EnableAllAPIKeys(ctx context.Context, channelID int) error {
	// 读取 channel
	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, channelID)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if len(ch.DisabledAPIKeys) == 0 {
		// 没有禁用的 key，忽略
		return nil
	}

	// 更新 channel，清空 disabled_api_keys
	update := svc.entFromContext(ctx).Channel.UpdateOneID(channelID).
		SetDisabledAPIKeys([]objects.DisabledAPIKey{})

	if _, err := update.Save(ctx); err != nil {
		return fmt.Errorf("failed to enable all api keys: %w", err)
	}

	log.Info(ctx, "All API keys enabled",
		log.Int("channel_id", channelID),
	)

	svc.asyncReloadChannels()

	return nil
}

// EnableSelectedAPIKeys re-enables multiple specific keys from disabled_api_keys.
func (svc *ChannelService) EnableSelectedAPIKeys(ctx context.Context, channelID int, keys []string) error {
	if len(keys) == 0 {
		return nil
	}

	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, channelID)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if len(ch.DisabledAPIKeys) == 0 {
		return nil
	}

	keysToEnable := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		keysToEnable[k] = struct{}{}
	}

	newDisabledKeys := make([]objects.DisabledAPIKey, 0, len(ch.DisabledAPIKeys))
	for _, dk := range ch.DisabledAPIKeys {
		if _, found := keysToEnable[dk.Key]; !found {
			newDisabledKeys = append(newDisabledKeys, dk)
		}
	}

	if len(newDisabledKeys) == len(ch.DisabledAPIKeys) {
		return nil
	}

	update := svc.entFromContext(ctx).Channel.UpdateOneID(channelID).
		SetDisabledAPIKeys(newDisabledKeys)

	if _, err := update.Save(ctx); err != nil {
		return fmt.Errorf("failed to enable selected api keys: %w", err)
	}

	log.Info(ctx, "Selected API keys enabled",
		log.Int("channel_id", channelID),
		log.Int("count", len(keys)),
	)

	svc.asyncReloadChannels()

	return nil
}

// DeleteDisabledAPIKeysResult is the result of deleting disabled API keys.
type DeleteDisabledAPIKeysResult struct {
	Success bool
	Message string
}

// DeleteDisabledAPIKeys removes disabled API keys from both disabled_api_keys list and credentials.
// It ensures at least one API key remains and prevents deletion for OAuth channels.
func (svc *ChannelService) DeleteDisabledAPIKeys(ctx context.Context, channelID int, keys []string) (*DeleteDisabledAPIKeysResult, error) {
	if len(keys) == 0 {
		return &DeleteDisabledAPIKeysResult{Success: true}, nil
	}

	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, channelID)
	if err != nil {
		return nil, fmt.Errorf("failed to get channel: %w", err)
	}

	// Check if channel uses OAuth - cannot delete keys for OAuth channels
	if ch.Credentials.IsOAuth() {
		return nil, fmt.Errorf("cannot delete API keys for OAuth channels")
	}

	keysToDelete := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		keysToDelete[k] = struct{}{}
	}

	// Remove from disabled_api_keys
	newDisabledKeys := make([]objects.DisabledAPIKey, 0, len(ch.DisabledAPIKeys))
	for _, dk := range ch.DisabledAPIKeys {
		if _, found := keysToDelete[dk.Key]; !found {
			newDisabledKeys = append(newDisabledKeys, dk)
		}
	}

	// Remove from credentials
	newCredentials := ch.Credentials
	if len(newCredentials.APIKeys) > 0 {
		filteredKeys := make([]string, 0, len(newCredentials.APIKeys))
		for _, k := range newCredentials.APIKeys {
			if _, found := keysToDelete[k]; !found {
				filteredKeys = append(filteredKeys, k)
			}
		}

		newCredentials.APIKeys = filteredKeys
	}

	if newCredentials.APIKey != "" {
		if _, found := keysToDelete[newCredentials.APIKey]; found {
			newCredentials.APIKey = ""
		}
	}

	// Ensure at least one API key remains
	routableKeys := newCredentials.GetRoutableAPIKeys(newDisabledKeys, channelArchivedAPIKeys(ch.Settings))
	if len(routableKeys) == 0 {
		// Restore at least one key from the keys being deleted
		// Prefer the first key that was supposed to be deleted
		restoredKey := keys[0]
		newCredentials.APIKeys = []string{restoredKey}
	}

	update := svc.entFromContext(ctx).Channel.UpdateOneID(channelID).
		SetDisabledAPIKeys(newDisabledKeys).
		SetCredentials(newCredentials)

	if _, err := update.Save(ctx); err != nil {
		return nil, fmt.Errorf("failed to delete disabled api keys: %w", err)
	}

	log.Info(ctx, "Disabled API keys deleted",
		log.Int("channel_id", channelID),
		log.Int("count", len(keys)),
	)

	// Check if we had to preserve a key
	result := &DeleteDisabledAPIKeysResult{Success: true}
	if len(routableKeys) == 0 {
		result.Message = "ONE_KEY_PRESERVED"
	}

	svc.asyncReloadChannels()

	return result, nil
}

func mergeInventoryMetadata(item *ChannelAPIKeyInventoryItem, meta objects.ChannelKeyMetadata) {
	if meta.MaskedKey != "" {
		item.MaskedKey = meta.MaskedKey
	}
	if meta.Status != "" {
		item.Status = meta.Status
	}
	item.LastCheckedAt = meta.LastCheckedAt
	item.Success = meta.Success
	item.FailureCount = meta.FailureCount
	item.Reason = meta.Reason
	item.Balance = meta.Balance
	item.Currency = meta.Currency
	item.BalanceSnapshot = cloneChannelKeyBalanceSnapshot(meta.BalanceSnapshot)
	item.Available = meta.Available
	item.StatusCode = meta.StatusCode
	item.MatchedPolicy = meta.MatchedPolicy
	item.Action = meta.Action
	item.NextCheckAt = meta.NextCheckAt
	item.BackoffAttempt = meta.BackoffAttempt
	item.History = slices.Clone(meta.History)
}

func mergeInventoryArchive(item *ChannelAPIKeyInventoryItem, archive objects.ChannelArchivedAPIKey) {
	if archive.MaskedKey != "" {
		item.MaskedKey = archive.MaskedKey
	}
	item.Status = objects.ChannelKeyStatusArchived
	if archive.LastCheckedAt != nil {
		item.LastCheckedAt = archive.LastCheckedAt
	} else {
		item.LastCheckedAt = archive.ArchivedAt
	}
	item.FailureCount = archive.FailureCount
	item.Reason = archive.Reason
	item.Balance = archive.Balance
	item.Currency = archive.Currency
	item.BalanceSnapshot = cloneChannelKeyBalanceSnapshot(archive.BalanceSnapshot)
	item.Available = archive.Available
}

func resolveChannelAPIKey(credentials objects.ChannelCredentials, keyID string) (string, bool) {
	keyID = strings.TrimSpace(keyID)
	for _, key := range credentials.GetAllAPIKeys() {
		if key == keyID || objects.ChannelAPIKeyFingerprint(key) == normalizeChannelKeyID(keyID) {
			return key, true
		}
	}

	return "", false
}

func normalizeChannelKeyID(keyID string) string {
	if strings.HasPrefix(keyID, "key_") {
		return keyID
	}

	return objects.ChannelAPIKeyFingerprint(keyID)
}

func removeChannelCredentialKey(credentials objects.ChannelCredentials, key string) objects.ChannelCredentials {
	if credentials.APIKey == key {
		credentials.APIKey = ""
	}
	credentials.APIKeys = slices.DeleteFunc(credentials.APIKeys, func(item string) bool {
		return item == key
	})

	return credentials
}

func mergeArchivedChannelCredentials(current, next objects.ChannelCredentials, archivedKeys []objects.ChannelArchivedAPIKey) objects.ChannelCredentials {
	if len(archivedKeys) == 0 || next.IsOAuth() {
		return next
	}

	archivedSet := make(map[string]struct{}, len(archivedKeys))
	for _, archived := range archivedKeys {
		if archived.ID == "" {
			continue
		}

		archivedSet[archived.ID] = struct{}{}
	}
	if len(archivedSet) == 0 {
		return next
	}

	existing := make(map[string]struct{}, len(next.GetAllAPIKeys()))
	for _, key := range next.GetAllAPIKeys() {
		existing[key] = struct{}{}
	}

	restoredArchived := make([]string, 0, len(archivedSet))
	for _, key := range current.GetAllAPIKeys() {
		if _, ok := archivedSet[objects.ChannelAPIKeyFingerprint(key)]; !ok {
			continue
		}
		if _, ok := existing[key]; ok {
			continue
		}

		existing[key] = struct{}{}
		restoredArchived = append(restoredArchived, key)
	}
	if len(restoredArchived) == 0 {
		return next
	}

	if next.APIKey == "" && len(next.APIKeys) == 0 {
		next.APIKeys = restoredArchived
		return next
	}

	next.APIKeys = append(next.APIKeys, restoredArchived...)

	return next
}

func removeDisabledAPIKey(disabled []objects.DisabledAPIKey, key string) []objects.DisabledAPIKey {
	return slices.DeleteFunc(slices.Clone(disabled), func(item objects.DisabledAPIKey) bool {
		return item.Key == key
	})
}

func removeChannelKeySettingsMetadata(settings *objects.ChannelSettings, id string) *objects.ChannelSettings {
	if settings == nil || settings.KeyHealthCheck == nil {
		return settings
	}

	next := cloneChannelSettings(settings)
	next.KeyHealthCheck.KeyMetadata = slices.DeleteFunc(next.KeyHealthCheck.KeyMetadata, func(item objects.ChannelKeyMetadata) bool {
		return item.ID == id
	})
	next.KeyHealthCheck.ArchivedKeys = slices.DeleteFunc(next.KeyHealthCheck.ArchivedKeys, func(item objects.ChannelArchivedAPIKey) bool {
		return item.ID == id
	})

	return next
}

func channelKeySettingsHasKeyID(settings *objects.ChannelSettings, id string) bool {
	if settings == nil || settings.KeyHealthCheck == nil || id == "" {
		return false
	}

	for _, item := range settings.KeyHealthCheck.KeyMetadata {
		if item.ID == id {
			return true
		}
	}
	for _, item := range settings.KeyHealthCheck.ArchivedKeys {
		if item.ID == id {
			return true
		}
	}

	return false
}

func ensureChannelKeyHealthCheckSettings(settings *objects.ChannelSettings) *objects.ChannelSettings {
	next := cloneChannelSettings(settings)
	if next == nil {
		next = &objects.ChannelSettings{}
	}
	if next.KeyHealthCheck == nil {
		next.KeyHealthCheck = &objects.ChannelKeyHealthCheck{}
	}

	return next
}

func cloneChannelSettings(settings *objects.ChannelSettings) *objects.ChannelSettings {
	if settings == nil {
		return nil
	}

	next := *settings
	if settings.KeySelection != nil {
		keySelection := *settings.KeySelection
		next.KeySelection = &keySelection
	}
	if settings.BalanceProbe != nil {
		balanceProbe := *settings.BalanceProbe
		balanceProbe.IncludeStatuses = slices.Clone(settings.BalanceProbe.IncludeStatuses)
		if settings.BalanceProbe.HTTP != nil {
			httpRule := *settings.BalanceProbe.HTTP
			httpRule.Headers = slices.Clone(settings.BalanceProbe.HTTP.Headers)
			httpRule.ExpectedStatuses = slices.Clone(settings.BalanceProbe.HTTP.ExpectedStatuses)
			if settings.BalanceProbe.HTTP.KeyInjection != nil {
				keyInjection := *settings.BalanceProbe.HTTP.KeyInjection
				httpRule.KeyInjection = &keyInjection
			}
			balanceProbe.HTTP = &httpRule
		}
		next.BalanceProbe = &balanceProbe
	}
	if settings.KeyHealthCheck != nil {
		health := *settings.KeyHealthCheck
		health.Rules = slices.Clone(settings.KeyHealthCheck.Rules)
		health.Policies = slices.Clone(settings.KeyHealthCheck.Policies)
		for i := range health.Policies {
			health.Policies[i].Actions = slices.Clone(settings.KeyHealthCheck.Policies[i].Actions)
		}
		health.KeyMetadata = slices.Clone(settings.KeyHealthCheck.KeyMetadata)
		for i := range health.KeyMetadata {
			health.KeyMetadata[i].History = slices.Clone(settings.KeyHealthCheck.KeyMetadata[i].History)
			health.KeyMetadata[i].BalanceSnapshot = cloneChannelKeyBalanceSnapshot(settings.KeyHealthCheck.KeyMetadata[i].BalanceSnapshot)
		}
		health.ArchivedKeys = slices.Clone(settings.KeyHealthCheck.ArchivedKeys)
		for i := range health.ArchivedKeys {
			health.ArchivedKeys[i].BalanceSnapshot = cloneChannelKeyBalanceSnapshot(settings.KeyHealthCheck.ArchivedKeys[i].BalanceSnapshot)
		}
		health.History = slices.Clone(settings.KeyHealthCheck.History)
		for i := range health.History {
			health.History[i].BalanceSnapshot = cloneChannelKeyBalanceSnapshot(settings.KeyHealthCheck.History[i].BalanceSnapshot)
		}
		next.KeyHealthCheck = &health
	}

	return &next
}

func mergeChannelSettingsForUpdate(current, input *objects.ChannelSettings) *objects.ChannelSettings {
	next := cloneChannelSettings(input)
	if next == nil {
		return nil
	}

	currentRuntime := cloneChannelSettings(current)
	var keyMetadata []objects.ChannelKeyMetadata
	var archivedKeys []objects.ChannelArchivedAPIKey
	var history []objects.ChannelKeyHealthCheckHistoryEntry
	if currentRuntime != nil && currentRuntime.KeyHealthCheck != nil {
		keyMetadata = currentRuntime.KeyHealthCheck.KeyMetadata
		archivedKeys = currentRuntime.KeyHealthCheck.ArchivedKeys
		history = currentRuntime.KeyHealthCheck.History
	}

	if next.KeyHealthCheck == nil {
		if len(keyMetadata) == 0 && len(archivedKeys) == 0 && len(history) == 0 {
			return next
		}

		next.KeyHealthCheck = &objects.ChannelKeyHealthCheck{}
	}

	next.KeyHealthCheck.KeyMetadata = keyMetadata
	next.KeyHealthCheck.ArchivedKeys = archivedKeys
	next.KeyHealthCheck.History = history

	return next
}
