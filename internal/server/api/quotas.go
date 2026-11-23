package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

// QuotaExposureResponse is the downstream-safe representation of authorized
// upstream credentials. It deliberately has no raw credential, provider URL,
// proxy, or raw probe summary fields.
type QuotaExposureResponse struct {
	Object      string                  `json:"object"`
	Exposed     bool                    `json:"exposed"`
	GeneratedAt time.Time               `json:"generated_at"`
	Summary     QuotaExposureSummary    `json:"summary"`
	Data        []*QuotaExposureChannel `json:"data"`
}

// OpenAIModelQuota is the compact model-level projection used by
// /v1/models?include=quotas and ?include=all.
type OpenAIModelQuota struct {
	TotalKeys      int                     `json:"total_keys"`
	ActiveKeys     int                     `json:"active_keys"`
	TotalRemaining *float64                `json:"total_remaining,omitempty"`
	Currency       string                  `json:"currency,omitempty"`
	Channels       []*QuotaExposureChannel `json:"channels"`
}

type QuotaExposureSummary struct {
	AuthorizedChannels int `json:"authorized_channels"`
	TotalKeys          int `json:"total_keys"`
	ActiveKeys         int `json:"active_keys"`
	AvailableKeys      int `json:"available_keys"`
}

type QuotaExposureChannel struct {
	ChannelID       int                    `json:"channel_id"`
	ChannelName     string                 `json:"channel_name,omitempty"`
	Type            string                 `json:"type"`
	Resources       []string               `json:"resources,omitempty"`
	RoutingStrategy string                 `json:"routing_strategy,omitempty"`
	SharedWallet    bool                   `json:"shared_wallet,omitempty"`
	WalletID        string                 `json:"wallet_id,omitempty"`
	KeyCounts       QuotaExposureKeyCounts `json:"key_counts"`
	QuotaSummary    QuotaSummaryByCurrency `json:"quota_summary"`
	Keys            []*QuotaExposureKey    `json:"keys"`
}

type QuotaExposureKeyCounts struct {
	Total     int `json:"total"`
	Active    int `json:"active"`
	Disabled  int `json:"disabled"`
	Exhausted int `json:"exhausted"`
}

type QuotaSummaryByCurrency struct {
	Currency       string   `json:"currency,omitempty"`
	TotalRemaining *float64 `json:"total_remaining,omitempty"`
	TotalUsed      *float64 `json:"total_used,omitempty"`
	TotalLimit     *float64 `json:"total_limit,omitempty"`
	CheckedKeys    int      `json:"checked_keys"`
}

type QuotaExposureKey struct {
	ID         string                       `json:"id"`
	Name       string                       `json:"name,omitempty"`
	MaskedKey  string                       `json:"masked_key"`
	Status     string                       `json:"status"`
	Available  *bool                        `json:"available,omitempty"`
	Priority   int                          `json:"priority"`
	Weight     int                          `json:"weight"`
	Remaining  *float64                     `json:"remaining,omitempty"`
	Used       *float64                     `json:"used,omitempty"`
	Limit      *float64                     `json:"limit,omitempty"`
	UsageRatio *float64                     `json:"usage_ratio,omitempty"`
	Currency   string                       `json:"currency,omitempty"`
	Unlimited  bool                         `json:"unlimited,omitempty"`
	FreeTier   bool                         `json:"free_tier,omitempty"`
	WalletID   string                       `json:"wallet_id,omitempty"`
	ResetAt    *time.Time                   `json:"reset_at,omitempty"`
	CheckedAt  *time.Time                   `json:"checked_at,omitempty"`
	Windows    []objects.ChannelQuotaWindow `json:"windows,omitempty"`
	Components []QuotaExposureComponent     `json:"components,omitempty"`
}

type QuotaExposureComponent struct {
	Amount    float64    `json:"amount"`
	Currency  string     `json:"currency,omitempty"`
	Kind      string     `json:"kind"`
	Label     string     `json:"label,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

type quotaExposureError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Type    string `json:"type"`
}

var (
	quotaCredentialIDPattern = regexp.MustCompile(`^key_[0-9a-f]{16}$`)
	quotaMaskedKeyPattern    = regexp.MustCompile(`^[A-Za-z0-9._-]{0,8}\*{2,}[A-Za-z0-9._-]{0,8}$`)
)

const (
	quotaExposureDisabledCode    = "key_quota_exposure_disabled"
	quotaExposureDisabledMessage = "Upstream key quota exposure is disabled for this API key."
)

// ListQuotas handles GET /v1/quotas. KeyQuotas is kept as a method alias so
// callers embedding the handler can use the semantic route name directly.
func (handlers *OpenAIHandlers) ListQuotas(c *gin.Context) {
	handlers.writeQuotaExposure(c)
}

func (handlers *OpenAIHandlers) KeyQuotas(c *gin.Context) {
	handlers.writeQuotaExposure(c)
}

func openAIModelQuotasRequested(include string) bool {
	for _, field := range strings.Split(strings.ToLower(include), ",") {
		switch strings.TrimSpace(field) {
		case "all", "quotas":
			return true
		}
	}
	return false
}

func (handlers *OpenAIHandlers) writeQuotaModelInternalError(c *gin.Context, requestID string, err error) {
	log.Error(c.Request.Context(), "failed to build model quota exposure", log.Cause(err))
	handlers.writeOpenAIInternalError(c, requestID, errors.New("failed to retrieve upstream key quotas"))
}

func openAIQuotaExposureEnabled(ctx context.Context) bool {
	key, ok := contexts.GetAPIKey(ctx)
	return ok && key != nil && key.ExposeUpstreamKeyQuota
}

func (handlers *OpenAIHandlers) attachOpenAIModelQuota(ctx context.Context, model *OpenAIModel, modelID string) error {
	if model == nil {
		return nil
	}
	key, ok := contexts.GetAPIKey(ctx)
	if !ok || key == nil || !key.ExposeUpstreamKeyQuota {
		return nil
	}
	channels, err := handlers.ModelService.ModelQuotaChannels(ctx, key, modelID, nil)
	if err != nil {
		return err
	}
	response, err := buildQuotaExposureForChannels(ctx, handlers.ChannelService, channels)
	if err != nil {
		return err
	}
	quota := &OpenAIModelQuota{
		Channels: make([]*QuotaExposureChannel, 0, len(response.Data)),
	}
	currencies := map[string]struct{}{}
	seenWallets := map[string]struct{}{}
	var totalRemaining float64
	var haveRemaining, unclassified bool
	for _, chEntry := range response.Data {
		if chEntry == nil {
			continue
		}
		quota.Channels = append(quota.Channels, chEntry)
		quota.TotalKeys += chEntry.KeyCounts.Total
		quota.ActiveKeys += chEntry.KeyCounts.Active
		if chEntry.SharedWallet && chEntry.WalletID != "" {
			if _, seen := seenWallets[chEntry.WalletID]; seen {
				continue
			}
			seenWallets[chEntry.WalletID] = struct{}{}
			if chEntry.QuotaSummary.Currency == "%" {
				continue
			}
			if chEntry.QuotaSummary.Currency != "" {
				currencies[chEntry.QuotaSummary.Currency] = struct{}{}
			}
			if chEntry.QuotaSummary.TotalRemaining != nil {
				if chEntry.QuotaSummary.Currency == "" {
					unclassified = true
				} else {
					totalRemaining += *chEntry.QuotaSummary.TotalRemaining
					haveRemaining = true
				}
			}
			continue
		}
		for _, k := range chEntry.Keys {
			if k == nil || k.Unlimited || k.FreeTier {
				continue
			}
			if k.WalletID != "" {
				if _, seen := seenWallets[k.WalletID]; seen {
					continue
				}
				seenWallets[k.WalletID] = struct{}{}
			}
			if k.Currency == "%" {
				continue
			}
			if k.Currency != "" {
				currencies[k.Currency] = struct{}{}
			}
			if k.Remaining != nil {
				if k.Currency == "" {
					unclassified = true
				} else {
					totalRemaining += *k.Remaining
					haveRemaining = true
				}
			}
		}
	}
	if len(currencies) == 1 && !unclassified {
		for currency := range currencies {
			quota.Currency = currency
		}
		if haveRemaining {
			quota.TotalRemaining = floatPtr(totalRemaining)
		}
	}
	model.UpstreamQuota = quota
	return nil
}

func (handlers *OpenAIHandlers) writeQuotaExposure(c *gin.Context) {
	ctx := c.Request.Context()
	apiKey, ok := contexts.GetAPIKey(ctx)
	if !ok || apiKey == nil || !apiKey.ExposeUpstreamKeyQuota {
		c.JSON(http.StatusForbidden, gin.H{"error": quotaExposureError{
			Code:    quotaExposureDisabledCode,
			Message: quotaExposureDisabledMessage,
			Type:    "permission_error",
		}})
		return
	}

	channelID, err := quotaChannelIDFilter(c.Query("channel_id"))
	if err != nil {
		JSONError(c, http.StatusBadRequest, err)
		return
	}
	modelID := strings.TrimSpace(c.Query("model"))
	if len(modelID) > 512 {
		JSONError(c, http.StatusBadRequest, errors.New("model is too long"))
		return
	}
	if rawRefresh := strings.TrimSpace(c.Query("refresh")); rawRefresh != "" {
		refresh, err := strconv.ParseBool(rawRefresh)
		if err != nil {
			JSONError(c, http.StatusBadRequest, errors.New("refresh must be a boolean"))
			return
		}
		if refresh {
			JSONError(c, http.StatusBadRequest, errors.New("refresh=true is not supported; quota queries use the latest stored snapshot"))
			return
		}
	}

	response, err := buildQuotaExposureResponse(ctx, handlers.ChannelService, apiKey, biz.ChannelQuotaExposureFilter{
		Model:     modelID,
		ChannelID: channelID,
	})
	if err != nil {
		log.Error(ctx, "failed to build downstream quota exposure", log.Cause(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": quotaExposureError{
			Code:    "internal_server_error",
			Message: "Failed to retrieve upstream key quotas.",
			Type:    "server_error",
		}})
		return
	}

	c.JSON(http.StatusOK, response)
}

func quotaChannelIDFilter(raw string) (*int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	id, err := strconv.Atoi(raw)
	if err != nil || id <= 0 {
		return nil, errors.New("channel_id must be a positive integer")
	}
	return &id, nil
}

func buildQuotaExposureResponse(ctx context.Context, channels *biz.ChannelService, key *ent.APIKey, filter biz.ChannelQuotaExposureFilter) (QuotaExposureResponse, error) {
	// This helper is kept behind the HTTP toggle check. It still requires a key
	// and receives the already-authorized channel view from the biz layer.
	return buildQuotaExposureForChannels(ctx, channels, channels.AuthorizedQuotaChannels(key, filter))
}

func buildQuotaExposureForChannels(ctx context.Context, channelService *biz.ChannelService, authorized []*biz.Channel) (QuotaExposureResponse, error) {
	now := time.Now().UTC()
	// Authorization is complete before this bypass is introduced: authorized
	// channels were selected from the request API key's channel/model profiles.
	// Inventory reads need no additional downstream scope and are still mapped
	// through the redacted DTO below.
	inventoryCtx := authz.WithSystemBypass(ctx, "downstream-key-quota-exposure")
	response := QuotaExposureResponse{
		Object:      "list",
		Exposed:     true,
		GeneratedAt: now,
		Data:        make([]*QuotaExposureChannel, 0, len(authorized)),
	}
	for _, ch := range authorized {
		items, err := channelService.ChannelAPIKeyInventory(inventoryCtx, ch.ID)
		if err != nil {
			return QuotaExposureResponse{}, err
		}
		entry := buildQuotaExposureChannel(ch, items, now)
		response.Data = append(response.Data, entry)
		response.Summary.AuthorizedChannels++
		response.Summary.TotalKeys += entry.KeyCounts.Total
		response.Summary.ActiveKeys += entry.KeyCounts.Active
		for _, item := range entry.Keys {
			if item.Status == "active" {
				response.Summary.AvailableKeys++
			}
		}
	}

	return response, nil
}

func buildQuotaExposureChannel(ch *biz.Channel, items []*biz.ChannelAPIKeyInventoryItem, now time.Time) *QuotaExposureChannel {
	secrets := make([]string, 0, len(items))
	for _, item := range items {
		if item != nil && item.RawKey != "" {
			secrets = append(secrets, item.RawKey)
		}
	}
	channelName := sanitizeQuotaLabel(ch.Name)
	if containsQuotaSecret(channelName, secrets) {
		channelName = ""
	}
	result := &QuotaExposureChannel{
		ChannelID:       ch.ID,
		ChannelName:     channelName,
		Type:            sanitizeQuotaChannelType(ch.Type),
		Resources:       quotaResources(ch, secrets),
		RoutingStrategy: quotaRoutingStrategy(ch),
		Keys:            make([]*QuotaExposureKey, 0, len(items)),
	}

	currencies := map[string]struct{}{}
	seenWallets := map[string]struct{}{}
	var totalRemaining, totalUsed, totalLimit float64
	var haveRemaining, haveUsed, haveLimit, unclassifiedAmount bool
	for _, item := range items {
		key := buildQuotaExposureKey(ch, item, now)
		if key == nil {
			continue
		}
		if item.SharedWallet {
			result.SharedWallet = true
			if result.WalletID == "" && key.WalletID != "" {
				result.WalletID = key.WalletID
			}
		}
		result.Keys = append(result.Keys, key)
		result.KeyCounts.Total++
		switch key.Status {
		case "disabled":
			result.KeyCounts.Disabled++
		default:
			result.KeyCounts.Active++
		}
		if key.Status == "exhausted" {
			result.KeyCounts.Exhausted++
		}
		if key.CheckedAt != nil {
			result.QuotaSummary.CheckedKeys++
		}
		// Never sum unlimited sentinels, free-tier placeholders, or rolling percentages.
		if key.Unlimited || key.FreeTier || key.Currency == "%" {
			continue
		}
		// Skip unobserved members before marking the canonical wallet seen so an
		// unprobed first key never hides a valid observation on a later key.
		if key.Remaining == nil && key.Used == nil && key.Limit == nil {
			continue
		}
		// Deduplicate balance amounts by canonical wallet identity so SharedWallet
		// pools never multiply their balance by N keys.
		if key.WalletID != "" {
			if _, seen := seenWallets[key.WalletID]; seen {
				continue
			}
			seenWallets[key.WalletID] = struct{}{}
		}
		if key.Currency != "" {
			currencies[key.Currency] = struct{}{}
		}
		if key.Remaining != nil {
			if key.Currency == "" {
				unclassifiedAmount = true
			} else {
				totalRemaining += *key.Remaining
				haveRemaining = true
			}
		}
		if key.Used != nil {
			if key.Currency == "" {
				unclassifiedAmount = true
			} else {
				totalUsed += *key.Used
				haveUsed = true
			}
		}
		if key.Limit != nil {
			if key.Currency == "" {
				unclassifiedAmount = true
			} else {
				totalLimit += *key.Limit
				haveLimit = true
			}
		}
	}
	if len(currencies) == 1 && !unclassifiedAmount {
		for currency := range currencies {
			result.QuotaSummary.Currency = currency
		}
		if haveRemaining {
			result.QuotaSummary.TotalRemaining = floatPtr(totalRemaining)
		}
		if haveUsed {
			result.QuotaSummary.TotalUsed = floatPtr(totalUsed)
		}
		if haveLimit {
			result.QuotaSummary.TotalLimit = floatPtr(totalLimit)
		}
	}

	return result
}

func buildQuotaExposureKey(ch *biz.Channel, item *biz.ChannelAPIKeyInventoryItem, now time.Time) *QuotaExposureKey {
	if item == nil || item.Status == objects.ChannelKeyStatusArchived {
		return nil
	}
	id := sanitizeQuotaCredentialID(item.ID)
	if id == "" {
		return nil
	}
	policy := quotaPolicy(ch, id)
	if item.Policy != nil {
		policy = item.Policy.WithDefaults()
	}
	snapshot := item.BalanceSnapshot
	status := quotaStatus(item, snapshot, now)
	available := item.Available
	if snapshot != nil && snapshot.Available != nil {
		available = snapshot.Available
	}
	if status == "exhausted" || status == "cooldown" || status == "disabled" {
		available = boolPtr(false)
	} else if available == nil {
		available = boolPtr(true)
	} else if snapshot != nil && snapshot.ResetAt != nil && !snapshot.ResetAt.After(now) && snapshot.Available != nil && !*snapshot.Available {
		available = boolPtr(true)
	}

	remaining, used, limit, ratio, currency := quotaValues(snapshot, item.Currency)
	if snapshot != nil {
		if worst, ok := snapshot.WorstUsageRatioAt(now); ok {
			ratio = floatPtr(clampQuotaRatio(worst))
		}
	}
	checkedAt := item.LastCheckedAt
	resetAt := (*time.Time)(nil)
	unlimited := false
	freeTier := false
	var windows []objects.ChannelQuotaWindow
	if snapshot != nil {
		if !snapshot.CheckedAt.IsZero() {
			checkedAt = &snapshot.CheckedAt
		}
		resetAt = snapshot.ResetAt
		if snapshot.Unlimited != nil && *snapshot.Unlimited {
			unlimited = true
		}
		if snapshot.FreeTier != nil && *snapshot.FreeTier {
			freeTier = true
		}
		if len(snapshot.Windows) > 0 {
			windows = append([]objects.ChannelQuotaWindow(nil), snapshot.Windows...)
		}
	}
	walletID := ""
	if ch != nil && ch.Channel != nil {
		walletID = biz.QuotaCanonicalWalletID(ch.Channel, id, snapshot)
	}
	return &QuotaExposureKey{
		ID:         id,
		Name:       sanitizeQuotaLabelWithoutSecret(policy.Name, item.RawKey),
		MaskedKey:  sanitizeMaskedQuotaKey(item.MaskedKey, item.RawKey),
		Status:     status,
		Available:  available,
		Priority:   policy.Priority,
		Weight:     policy.Weight,
		Remaining:  remaining,
		Used:       used,
		Limit:      limit,
		UsageRatio: ratio,
		Currency:   currency,
		Unlimited:  unlimited,
		FreeTier:   freeTier,
		WalletID:   walletID,
		ResetAt:    resetAt,
		CheckedAt:  checkedAt,
		Windows:    windows,
		Components: quotaComponents(snapshot),
	}
}

func quotaPolicy(ch *biz.Channel, id string) objects.ChannelCredentialPolicy {
	if ch == nil || ch.Settings == nil {
		return (objects.ChannelCredentialPolicy{ID: id}).WithDefaults()
	}
	return ch.Settings.CredentialPolicy(id)
}

func quotaStatus(item *biz.ChannelAPIKeyInventoryItem, snapshot *objects.ChannelKeyBalanceSnapshot, now time.Time) string {
	if item.Status == objects.ChannelKeyStatusDisabled || item.Status == objects.ChannelKeyStatusArchived {
		return "disabled"
	}
	if (item.NextCheckAt != nil && item.NextCheckAt.After(now)) || (item.CooldownUntil != nil && item.CooldownUntil.After(now)) {
		return "cooldown"
	}
	if snapshot != nil && snapshot.ExhaustedAt(now) {
		return "exhausted"
	}
	if snapshot == nil && item.Available != nil && !*item.Available {
		return "exhausted"
	}
	return "active"
}

func quotaValues(snapshot *objects.ChannelKeyBalanceSnapshot, fallbackCurrency string) (remaining, used, limit, ratio *float64, currency string) {
	if snapshot == nil {
		return nil, nil, nil, nil, sanitizeQuotaCurrency(fallbackCurrency)
	}
	remaining, used, limit = finitePtr(snapshot.Remaining), finitePtr(snapshot.Used), finitePtr(snapshot.Limit)
	ratio = finitePtr(snapshot.UsageRatio)
	if ratio != nil {
		ratio = floatPtr(clampQuotaRatio(*ratio))
	} else if used != nil && limit != nil && *limit > 0 {
		ratio = floatPtr(clampQuotaRatio(*used / *limit))
	}
	currency = sanitizeQuotaCurrency(fallbackCurrency)
	if snapshot.PrimaryBalance != nil {
		if primaryCurrency := sanitizeQuotaCurrency(snapshot.PrimaryBalance.Currency); primaryCurrency != "" {
			currency = primaryCurrency
		}
	}
	for _, component := range snapshot.Components {
		componentCurrency := sanitizeQuotaCurrency(component.Currency)
		if currency == "" && componentCurrency != "" {
			currency = componentCurrency
		}
		if remaining == nil && component.Kind == objects.BalanceAmountKindRemaining && finiteFloat(component.Amount) {
			remaining = floatPtr(component.Amount)
		}
		if used == nil && component.Kind == objects.BalanceAmountKindUsed && finiteFloat(component.Amount) {
			used = floatPtr(component.Amount)
		}
		if limit == nil && component.Kind == objects.BalanceAmountKindLimit && finiteFloat(component.Amount) {
			limit = floatPtr(component.Amount)
		}
	}
	return remaining, used, limit, ratio, currency
}

func quotaComponents(snapshot *objects.ChannelKeyBalanceSnapshot) []QuotaExposureComponent {
	if snapshot == nil || len(snapshot.Components) == 0 {
		return nil
	}
	components := make([]QuotaExposureComponent, 0, len(snapshot.Components))
	for _, component := range snapshot.Components {
		if !finiteFloat(component.Amount) {
			continue
		}
		kind := sanitizeQuotaBalanceKind(component.Kind)
		if kind == "" {
			continue
		}
		components = append(components, QuotaExposureComponent{
			Amount:    component.Amount,
			Currency:  sanitizeQuotaCurrency(component.Currency),
			Kind:      kind,
			Label:     sanitizeQuotaComponentLabel(component.Label, kind),
			ExpiresAt: component.ExpiresAt,
		})
	}
	return components
}

func quotaResources(ch *biz.Channel, secrets []string) []string {
	if ch == nil || ch.Settings == nil || ch.Settings.RawHTTP == nil || !ch.Settings.RawHTTP.Enabled {
		return nil
	}
	resource := sanitizeQuotaLabel(ch.Settings.RawHTTP.Resource)
	if resource == "" || strings.ContainsAny(resource, "@\\") || strings.Contains(resource, "://") || containsQuotaSecret(resource, secrets) {
		return nil
	}
	return []string{resource}
}

func quotaRoutingStrategy(ch *biz.Channel) string {
	if ch == nil || ch.Settings == nil {
		return string(objects.ChannelKeySelectionStrategyTraceSticky)
	}
	strategy := ch.Settings.KeySelection.StrategyOrDefault()
	switch strategy {
	case objects.ChannelKeySelectionStrategyTraceSticky,
		objects.ChannelKeySelectionStrategyCacheAffinity,
		objects.ChannelKeySelectionStrategyRandom,
		objects.ChannelKeySelectionStrategyRoundRobin,
		objects.ChannelKeySelectionStrategySmart,
		objects.ChannelKeySelectionStrategyQuotaResetPriority:
		return string(strategy)
	default:
		return string(objects.ChannelKeySelectionStrategyTraceSticky)
	}
}

func sanitizeQuotaCredentialID(value string) string {
	value = strings.TrimSpace(value)
	if value == objects.OAuthDefaultCredentialID || quotaCredentialIDPattern.MatchString(value) {
		return value
	}
	return ""
}

func sanitizeMaskedQuotaKey(masked, raw string) string {
	if raw != "" {
		if len(raw) <= 8 {
			return "****"
		}
		masked = objects.MaskChannelAPIKey(raw)
		if quotaMaskedKeyPattern.MatchString(masked) && !strings.Contains(masked, raw) {
			return masked
		}
		return "****"
	}
	masked = strings.TrimSpace(masked)
	if quotaMaskedKeyPattern.MatchString(masked) {
		return masked
	}
	return "****"
}

func sanitizeQuotaChannelType(value channel.Type) string {
	value = channel.Type(strings.TrimSpace(string(value)))
	if channel.TypeValidator(value) != nil {
		return "unknown"
	}
	return value.String()
}

func sanitizeQuotaLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 200 || strings.ContainsAny(value, "\r\n") || strings.Contains(value, "://") || strings.ContainsAny(value, "@\\") {
		return ""
	}
	return value
}

func sanitizeQuotaLabelWithoutSecret(value, secret string) string {
	value = sanitizeQuotaLabel(value)
	if secret != "" && strings.Contains(value, secret) {
		return ""
	}
	return value
}

func containsQuotaSecret(value string, secrets []string) bool {
	for _, secret := range secrets {
		if secret != "" && strings.Contains(value, secret) {
			return true
		}
	}
	return false
}

func sanitizeQuotaCurrency(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	switch value {
	case "USD", "CNY", "EUR", "GBP", "JPY", "KRW", "CAD", "AUD", "CHF", "CREDITS", "TOKENS", "REQUESTS", "NANO", "%":
		return value
	default:
		return ""
	}
}

func sanitizeQuotaBalanceKind(value objects.BalanceAmountKind) string {
	switch value {
	case objects.BalanceAmountKindAvailable,
		objects.BalanceAmountKindTotal,
		objects.BalanceAmountKindCash,
		objects.BalanceAmountKindVoucher,
		objects.BalanceAmountKindGranted,
		objects.BalanceAmountKindToppedUp,
		objects.BalanceAmountKindUsed,
		objects.BalanceAmountKindLimit,
		objects.BalanceAmountKindRemaining:
		return string(value)
	default:
		return ""
	}
}

func sanitizeQuotaComponentLabel(value, kind string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	safe := map[string]struct{}{
		"available": {}, "balance": {}, "cash": {}, "charge balance": {}, "credit": {},
		"credits": {}, "granted": {}, "limit": {}, "nano balance": {}, "remaining": {},
		"remaining credits": {}, "total": {}, "total balance": {}, "total credits": {},
		"topped up": {}, "used": {}, "voucher": {},
	}
	if _, ok := safe[value]; ok {
		return value
	}
	if kind != "" {
		return kind
	}
	return ""
}

func finitePtr(value *float64) *float64 {
	if value == nil || !finiteFloat(*value) {
		return nil
	}
	return floatPtr(*value)
}

func finiteFloat(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func clampQuotaRatio(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func floatPtr(value float64) *float64 { return &value }
func boolPtr(value bool) *bool        { return &value }
