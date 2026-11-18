package biz

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
)

var errCannotArchiveLastUsableChannelAPIKey = errors.New("cannot archive the last usable channel api key")

func channelAPIKeyStateUpdate(client *ent.Client, ch *ent.Channel) *ent.ChannelUpdateOne {
	return client.Channel.UpdateOneID(ch.ID).Where(channel.UpdatedAtEQ(ch.UpdatedAt))
}

type ChannelAPIKeyInventoryItem struct {
	ID                  string
	RawKey              string
	MaskedKey           string
	Status              objects.ChannelKeyStatus
	LastCheckedAt       *time.Time
	Success             *bool
	FailureCount        int
	Reason              string
	Balance             any
	Currency            string
	BalanceSnapshot     *objects.ChannelKeyBalanceSnapshot
	Policy              *objects.ChannelCredentialPolicy
	EffectiveProxyName  string
	EffectiveExitIP     string
	InFlight            int
	QuotaRemainingRatio *float64
	QuotaResetAt        *time.Time
	CooldownUntil       *time.Time
	Available           *bool
	StatusCode          int
	MatchedPolicy       string
	Action              string
	NextCheckAt         *time.Time
	BackoffAttempt      int
	History             []objects.ChannelKeyHealthCheckHistoryEntry
}

func (svc *ChannelService) ChannelAPIKeyInventory(ctx context.Context, channelID int) ([]*ChannelAPIKeyInventoryItem, error) {
	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, channelID)
	if err != nil {
		return nil, fmt.Errorf("failed to get channel: %w", err)
	}

	source, err := resolveCredentialOwner(ctx, svc.entFromContext(ctx), ch)
	if err != nil {
		return nil, err
	}
	ch = withEffectiveCredentials(ch, source)

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
			RawKey:    key,
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

	if ch.Credentials.IsOAuth() {
		items = append(items, &ChannelAPIKeyInventoryItem{ID: objects.OAuthDefaultCredentialID, MaskedKey: "****", Status: objects.ChannelKeyStatusActive})
	}
	for _, item := range items {
		svc.populateCredentialInventory(ctx, ch, item)
	}
	return items, nil
}

func (svc *ChannelService) AddChannelAPIKey(ctx context.Context, channelID int, key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return fmt.Errorf("api key cannot be empty")
	}

	ch, err := svc.mutableCredentialChannel(ctx, channelID)
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

	if _, err := channelAPIKeyStateUpdate(svc.entFromContext(ctx), ch).SetCredentials(credentials).Save(ctx); err != nil {
		return fmt.Errorf("failed to add channel api key: %w", err)
	}

	svc.reloadChannelsAfterCommit(ctx)

	return nil
}

func (svc *ChannelService) DeleteChannelAPIKey(ctx context.Context, channelID int, keyID string) (*DeleteDisabledAPIKeysResult, error) {
	ch, err := svc.mutableCredentialChannel(ctx, channelID)
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
			if _, err := channelAPIKeyStateUpdate(svc.entFromContext(ctx), ch).SetSettings(settings).Save(ctx); err != nil {
				return nil, fmt.Errorf("failed to delete archived channel api key: %w", err)
			}

			svc.reloadChannelsAfterCommit(ctx)
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

	if _, err := channelAPIKeyStateUpdate(svc.entFromContext(ctx), ch).
		SetCredentials(credentials).
		SetDisabledAPIKeys(disabled).
		SetSettings(settings).
		Save(ctx); err != nil {
		return nil, fmt.Errorf("failed to delete channel api key: %w", err)
	}

	svc.reloadChannelsAfterCommit(ctx)

	return result, nil
}

func (svc *ChannelService) ArchiveChannelAPIKey(ctx context.Context, channelID int, keyID string, reason string) error {
	ch, err := svc.mutableCredentialChannel(ctx, channelID)
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

	if _, err := channelAPIKeyStateUpdate(svc.entFromContext(ctx), ch).SetSettings(settings).Save(ctx); err != nil {
		return fmt.Errorf("failed to archive channel api key: %w", err)
	}

	svc.reloadChannelsAfterCommit(ctx)

	return nil
}

func (svc *ChannelService) RestoreChannelAPIKey(ctx context.Context, channelID int, keyID string) error {
	ch, err := svc.mutableCredentialChannel(ctx, channelID)
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

	if _, err := channelAPIKeyStateUpdate(svc.entFromContext(ctx), ch).SetSettings(settings).Save(ctx); err != nil {
		return fmt.Errorf("failed to restore channel api key: %w", err)
	}

	svc.reloadChannelsAfterCommit(ctx)

	return nil
}

// DisableChannelAPIKey is the admin mutation; delegated keys are edited at the source.
func (svc *ChannelService) DisableChannelAPIKey(ctx context.Context, channelID int, key, reason string) error {
	ch, err := svc.mutableCredentialChannel(ctx, channelID)
	if err != nil {
		return err
	}
	return svc.disableOwnedAPIKey(ctx, ch, key, 0, reason)
}

// DisableAPIKey applies runtime disablement to the credential owner.
func (svc *ChannelService) DisableAPIKey(ctx context.Context, channelID int, key string, errorCode int, reason string) error {
	db := svc.entFromContext(ctx)
	ch, err := db.Channel.Get(ctx, channelID)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}
	ch, err = resolveCredentialOwner(ctx, db, ch)
	if err != nil {
		return err
	}
	return svc.disableOwnedAPIKey(ctx, ch, key, errorCode, reason)
}

// Late failure feedback cannot follow a new reference after an owner is replaced.
func (svc *ChannelService) currentFailureCredentialOwner(ctx context.Context, id int, key string) (*ent.Channel, error) {
	ch, err := observationPrimaryClient(svc.entFromContext(ctx)).Channel.Get(ctx, id)
	if ent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ch.Status == channel.StatusArchived || credentialSourceID(ch.Settings) != 0 {
		return nil, nil
	}
	if _, ok := resolveChannelAPIKey(ch.Credentials, key); !ok {
		return nil, nil
	}
	return ch, nil
}

func (svc *ChannelService) disableOwnedAPIKey(ctx context.Context, ch *ent.Channel, key string, errorCode int, reason string) error {
	if key == "" {
		return fmt.Errorf("api key cannot be empty")
	}
	channelID := ch.ID
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
	update := channelAPIKeyStateUpdate(svc.entFromContext(ctx), ch).
		SetDisabledAPIKeys(newDisabledKeys)

	// 如果没有可用 key 了，禁用整个 channel
	channelDisabled := len(enabledKeys) == 0 && ch.Status != channel.StatusArchived
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

	if ent.TxFromContext(ctx) == nil {
		// Stop selecting the disabled key on this node before the next attempt.
		reloadCtx, cancel := xcontext.DetachWithTimeout(ctx, 10*time.Second)
		defer cancel()

		if err := svc.enabledChannelsCache.Load(reloadCtx, true); err != nil {
			log.Warn(ctx, "Failed to synchronously reload channels after API key disablement",
				log.Int("channel_id", channelID),
				log.Cause(err),
			)
		}
	}

	// Also notify other instances after the write is visible to cache readers.
	svc.reloadChannelsAfterCommit(ctx)

	return nil
}

// EnableAPIKey 重新启用指定 key（从 disabled_api_keys 中移除）.
func (svc *ChannelService) EnableAPIKey(ctx context.Context, channelID int, key string) error {
	// 读取 channel
	ch, err := svc.mutableCredentialChannel(ctx, channelID)
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
	update := channelAPIKeyStateUpdate(svc.entFromContext(ctx), ch).
		SetDisabledAPIKeys(newDisabledKeys)

	if _, err := update.Save(ctx); err != nil {
		return fmt.Errorf("failed to enable api key: %w", err)
	}

	svc.reloadChannelsAfterCommit(ctx)

	return nil
}

// EnableAllAPIKeys 清空 disabled_api_keys.
func (svc *ChannelService) EnableAllAPIKeys(ctx context.Context, channelID int) error {
	// 读取 channel
	ch, err := svc.mutableCredentialChannel(ctx, channelID)
	if err != nil {
		return fmt.Errorf("failed to get channel: %w", err)
	}

	if len(ch.DisabledAPIKeys) == 0 {
		// 没有禁用的 key，忽略
		return nil
	}

	// 更新 channel，清空 disabled_api_keys
	update := channelAPIKeyStateUpdate(svc.entFromContext(ctx), ch).
		SetDisabledAPIKeys([]objects.DisabledAPIKey{})

	if _, err := update.Save(ctx); err != nil {
		return fmt.Errorf("failed to enable all api keys: %w", err)
	}

	log.Info(ctx, "All API keys enabled",
		log.Int("channel_id", channelID),
	)

	svc.reloadChannelsAfterCommit(ctx)

	return nil
}

// EnableSelectedAPIKeys re-enables multiple specific keys from disabled_api_keys.
func (svc *ChannelService) EnableSelectedAPIKeys(ctx context.Context, channelID int, keys []string) error {
	if len(keys) == 0 {
		return nil
	}

	ch, err := svc.mutableCredentialChannel(ctx, channelID)
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

	update := channelAPIKeyStateUpdate(svc.entFromContext(ctx), ch).
		SetDisabledAPIKeys(newDisabledKeys)

	if _, err := update.Save(ctx); err != nil {
		return fmt.Errorf("failed to enable selected api keys: %w", err)
	}

	log.Info(ctx, "Selected API keys enabled",
		log.Int("channel_id", channelID),
		log.Int("count", len(keys)),
	)

	svc.reloadChannelsAfterCommit(ctx)

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

	ch, err := svc.mutableCredentialChannel(ctx, channelID)
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

	settings := cloneChannelSettings(ch.Settings)
	pruneCredentialPolicies(settings, newCredentials)
	update := channelAPIKeyStateUpdate(svc.entFromContext(ctx), ch).
		SetDisabledAPIKeys(newDisabledKeys).
		SetCredentials(newCredentials).
		SetSettings(settings)

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

	svc.reloadChannelsAfterCommit(ctx)

	return result, nil
}

func mergeInventoryMetadata(item *ChannelAPIKeyInventoryItem, meta objects.ChannelKeyMetadata) {
	if meta.MaskedKey != "" {
		item.MaskedKey = meta.MaskedKey
	}
	if item.Status == "" && meta.Status != "" {
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
	if settings == nil {
		return nil
	}
	next := cloneChannelSettings(settings)
	next.CredentialPolicies = slices.DeleteFunc(next.CredentialPolicies, func(item objects.ChannelCredentialPolicy) bool { return item.ID == id })
	if next.KeyHealthCheck == nil {
		return next
	}
	next.KeyHealthCheck.KeyMetadata = slices.DeleteFunc(next.KeyHealthCheck.KeyMetadata, func(item objects.ChannelKeyMetadata) bool {
		return item.ID == id
	})
	next.KeyHealthCheck.ArchivedKeys = slices.DeleteFunc(next.KeyHealthCheck.ArchivedKeys, func(item objects.ChannelArchivedAPIKey) bool {
		return item.ID == id
	})

	return next
}

func channelKeySettingsHasKeyID(settings *objects.ChannelSettings, id string) bool {
	if settings == nil || id == "" {
		return false
	}
	for _, policy := range settings.CredentialPolicies {
		if policy.ID == id {
			return true
		}
	}
	if settings.KeyHealthCheck == nil {
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
	next.CredentialPolicies = slices.Clone(settings.CredentialPolicies)
	if settings.RawHTTP != nil {
		raw := *settings.RawHTTP
		next.RawHTTP = &raw
	}
	if settings.SSEKeepAlive != nil {
		sseKeepAlive := *settings.SSEKeepAlive
		next.SSEKeepAlive = &sseKeepAlive
	}
	if settings.KeySelection != nil {
		keySelection := *settings.KeySelection
		next.KeySelection = &keySelection
	}
	if settings.BalanceProbe != nil {
		balanceProbe := *settings.BalanceProbe
		if script := settings.BalanceProbe.Script; script != nil {
			copied := *script
			copied.Command = slices.Clone(script.Command)
			balanceProbe.Script = &copied
		}
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
	if settings.FailurePolicy != nil {
		failurePolicy := *settings.FailurePolicy
		failurePolicy.KeyProfiles = slices.Clone(settings.FailurePolicy.KeyProfiles)
		for i := range failurePolicy.KeyProfiles {
			failurePolicy.KeyProfiles[i].Sources = slices.Clone(settings.FailurePolicy.KeyProfiles[i].Sources)
			failurePolicy.KeyProfiles[i].Actions = slices.Clone(settings.FailurePolicy.KeyProfiles[i].Actions)
		}
		failurePolicy.ChannelProfiles = slices.Clone(settings.FailurePolicy.ChannelProfiles)
		for i := range failurePolicy.ChannelProfiles {
			failurePolicy.ChannelProfiles[i].Sources = slices.Clone(settings.FailurePolicy.ChannelProfiles[i].Sources)
			failurePolicy.ChannelProfiles[i].Actions = slices.Clone(settings.FailurePolicy.ChannelProfiles[i].Actions)
		}
		next.FailurePolicy = &failurePolicy
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
	normalizeChannelFailurePolicy(next.FailurePolicy)

	return &next
}

func mergeChannelSettingsForUpdate(current, input *objects.ChannelSettings) *objects.ChannelSettings {
	next := cloneChannelSettings(input)
	if next == nil {
		return nil
	}
	if credentialSourceID(next) == 0 {
		next.CredentialSourceChannelID = nil
	}
	normalizeChannelFailurePolicy(next.FailurePolicy)

	currentRuntime := cloneChannelSettings(current)
	// Existing settings editors may omit the newly introduced raw mode. Require
	// an explicit enabled:false to change that channel's protocol boundary.
	if currentRuntime != nil {
		if next.RawHTTP == nil {
			next.RawHTTP = currentRuntime.RawHTTP
		}
		if next.CredentialPolicies == nil {
			next.CredentialPolicies = currentRuntime.CredentialPolicies
		}
	}
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

func mergeChannelPoliciesForUpdate(current objects.ChannelPolicies, input *objects.ChannelPolicies) *objects.ChannelPolicies {
	if input == nil {
		return nil
	}
	next := *input
	if next.Stream == "" {
		next.Stream = current.Stream
	}
	if next.AllowRequestFormats == nil && current.AllowRequestFormats != nil {
		next.AllowRequestFormats = slices.Clone(current.AllowRequestFormats)
	}
	if next.DenyRequestFormats == nil && current.DenyRequestFormats != nil {
		next.DenyRequestFormats = slices.Clone(current.DenyRequestFormats)
	}
	if next.CodexOfficialClient == nil && current.CodexOfficialClient != nil {
		val := *current.CodexOfficialClient
		next.CodexOfficialClient = &val
	}
	if next.DenyCodexOfficialClient == nil && current.DenyCodexOfficialClient != nil {
		val := *current.DenyCodexOfficialClient
		next.DenyCodexOfficialClient = &val
	}
	return &next
}

// NormalizeChannelPolicies validates client gates and deduplicates request formats.
func NormalizeChannelPolicies(policies *objects.ChannelPolicies) error {
	if policies == nil {
		return nil
	}
	if policies.RequiresCodexOfficialClient() && policies.ForbidsCodexOfficialClient() {
		return fmt.Errorf("codexOfficialClient and denyCodexOfficialClient cannot both be enabled")
	}
	if len(policies.AllowRequestFormats) > 0 {
		normalized, err := normalizePolicyAPIFormats("allowRequestFormats", policies.AllowRequestFormats)
		if err != nil {
			return err
		}
		policies.AllowRequestFormats = normalized
	}
	if len(policies.DenyRequestFormats) > 0 {
		normalized, err := normalizePolicyAPIFormats("denyRequestFormats", policies.DenyRequestFormats)
		if err != nil {
			return err
		}
		policies.DenyRequestFormats = normalized
	}
	return nil
}

func normalizePolicyAPIFormats(field string, formats []string) ([]string, error) {
	out := make([]string, 0, len(formats))
	seen := make(map[string]struct{}, len(formats))
	for _, f := range formats {
		trimmed := strings.TrimSpace(f)
		if trimmed == "" {
			continue
		}
		if _, ok := SupportedAPIFormats[trimmed]; !ok {
			return nil, fmt.Errorf("invalid %s format %q", field, trimmed)
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out, nil
}
