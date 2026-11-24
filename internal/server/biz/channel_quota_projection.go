package biz

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/providerquotastatus"
	"github.com/looplj/axonhub/internal/objects"
)

type ChannelQuotaState string

const (
	ChannelQuotaStateAvailable   ChannelQuotaState = "available"
	ChannelQuotaStateWarning     ChannelQuotaState = "warning"
	ChannelQuotaStateExhausted   ChannelQuotaState = "exhausted"
	ChannelQuotaStateFreeTier    ChannelQuotaState = "free_tier"
	ChannelQuotaStateUnlimited   ChannelQuotaState = "unlimited"
	ChannelQuotaStateUnknown     ChannelQuotaState = "unknown"
	ChannelQuotaStateError       ChannelQuotaState = "error"
	ChannelQuotaStateUnsupported ChannelQuotaState = "unsupported"
)

type ChannelQuotaCounts struct {
	Total      int `json:"total"`
	Active     int `json:"active"`
	FreshKnown int `json:"freshKnown"`
	Exhausted  int `json:"exhausted"`
	Unknown    int `json:"unknown"`
}

type ChannelQuotaProbeDiagnostic struct {
	Status      string     `json:"status"`
	Error       string     `json:"error,omitempty"`
	LastProbeAt *time.Time `json:"lastProbeAt,omitempty"`
	NextProbeAt *time.Time `json:"nextProbeAt,omitempty"`
}

type ChannelQuotaOverview struct {
	Counts          ChannelQuotaCounts           `json:"counts"`
	State           ChannelQuotaState            `json:"state"`
	Windows         []objects.ChannelQuotaWindow `json:"windows"`
	WalletBalance   *objects.BalanceAmount       `json:"walletBalance,omitempty"`
	WalletLimit     *float64                     `json:"walletLimit,omitempty"`
	WalletUsed      *float64                     `json:"walletUsed,omitempty"`
	WalletRemaining *float64                     `json:"walletRemaining,omitempty"`
	WalletUnit      string                       `json:"walletUnit,omitempty"`
	PlanName        string                       `json:"planName,omitempty"`
	Unlimited       bool                         `json:"unlimited"`
	FreeTier        bool                         `json:"freeTier"`
	SharedWallet    bool                         `json:"sharedWallet"`
	WalletScope     string                       `json:"walletScope,omitempty"`
	WalletID        string                       `json:"walletId,omitempty"`
	NextResetAt     *time.Time                   `json:"nextResetAt,omitempty"`
	CheckedAt       *time.Time                   `json:"checkedAt,omitempty"`
	Source          string                       `json:"source"`
	CanRefresh      bool                         `json:"canRefresh"`
	ProbeDiagnostic *ChannelQuotaProbeDiagnostic `json:"probeDiagnostic,omitempty"`
}

const quotaOverviewFreshnessWindow = 24 * time.Hour

// ChannelQuotaOverviewBatch caches preloaded owner channels and ProviderQuotaStatus
// rows for a single GraphQL page so Channel.quotaOverview executes with zero N+1
// queries and zero upstream HTTP calls.
type ChannelQuotaOverviewBatch struct {
	mu          sync.RWMutex
	ownersByID  map[int]*ent.Channel
	pqsByChanID map[int]*ent.ProviderQuotaStatus
}

type quotaOverviewBatchCtxKey struct{}

func WithChannelQuotaOverviewBatch(ctx context.Context, batch *ChannelQuotaOverviewBatch) context.Context {
	if batch == nil {
		return ctx
	}
	return context.WithValue(ctx, quotaOverviewBatchCtxKey{}, batch)
}

func channelQuotaOverviewBatchFromContext(ctx context.Context) *ChannelQuotaOverviewBatch {
	batch, _ := ctx.Value(quotaOverviewBatchCtxKey{}).(*ChannelQuotaOverviewBatch)
	return batch
}

func NewChannelQuotaOverviewBatch() *ChannelQuotaOverviewBatch {
	return &ChannelQuotaOverviewBatch{
		ownersByID:  make(map[int]*ent.Channel),
		pqsByChanID: make(map[int]*ent.ProviderQuotaStatus),
	}
}

func (svc *ChannelService) PreloadChannelQuotaOverviewIntoContext(ctx context.Context, channels []*ent.Channel) {
	batch := channelQuotaOverviewBatchFromContext(ctx)
	if batch == nil || len(channels) == 0 {
		return
	}
	loaded := svc.PreloadChannelQuotaOverviewBatch(ctx, channels)
	batch.mu.Lock()
	for id, ch := range loaded.ownersByID {
		batch.ownersByID[id] = ch
	}
	for id, pqs := range loaded.pqsByChanID {
		batch.pqsByChanID[id] = pqs
	}
	batch.mu.Unlock()
}

// PreloadChannelQuotaOverviewBatch loads all credential-source owners and
// ProviderQuotaStatus rows needed by a page of channels in at most 2 bulk queries.
func (svc *ChannelService) PreloadChannelQuotaOverviewBatch(ctx context.Context, channels []*ent.Channel) *ChannelQuotaOverviewBatch {
	batch := &ChannelQuotaOverviewBatch{
		ownersByID:  make(map[int]*ent.Channel, len(channels)),
		pqsByChanID: make(map[int]*ent.ProviderQuotaStatus),
	}
	if len(channels) == 0 {
		return batch
	}

	missingOwnerIDs := make([]int, 0)
	missingOwnerSet := make(map[int]struct{})
	for _, ch := range channels {
		if ch == nil {
			continue
		}
		batch.ownersByID[ch.ID] = ch
		if srcID := credentialSourceID(ch.Settings); srcID != 0 {
			if _, ok := batch.ownersByID[srcID]; !ok {
				if _, seen := missingOwnerSet[srcID]; !seen {
					missingOwnerSet[srcID] = struct{}{}
					missingOwnerIDs = append(missingOwnerIDs, srcID)
				}
			}
		}
	}

	db := svc.entFromContext(ctx)
	if len(missingOwnerIDs) > 0 && db != nil {
		if owners, err := db.Channel.Query().Where(channel.IDIn(missingOwnerIDs...)).All(ctx); err == nil {
			for _, o := range owners {
				batch.ownersByID[o.ID] = o
			}
		}
	}

	// Only query ProviderQuotaStatus for channels NOT owned by per-key authority.
	pqsChannelIDs := make([]int, 0)
	pqsSeen := make(map[int]struct{})
	for _, ch := range channels {
		if ch == nil {
			continue
		}
		owner := ch
		if srcID := credentialSourceID(ch.Settings); srcID != 0 {
			if resolved, ok := batch.ownersByID[srcID]; ok && resolved != nil {
				owner = resolved
			}
		}
		if ChannelUsesPerKeyQuotaAuthority(owner) {
			continue
		}
		if owner.Edges.ProviderQuotaStatus != nil {
			batch.pqsByChanID[owner.ID] = owner.Edges.ProviderQuotaStatus
			continue
		}
		if _, seen := pqsSeen[owner.ID]; !seen {
			pqsSeen[owner.ID] = struct{}{}
			pqsChannelIDs = append(pqsChannelIDs, owner.ID)
		}
	}

	if len(pqsChannelIDs) > 0 && db != nil {
		if rows, err := db.ProviderQuotaStatus.Query().Where(providerquotastatus.ChannelIDIn(pqsChannelIDs...)).All(ctx); err == nil {
			for _, row := range rows {
				batch.pqsByChanID[row.ChannelID] = row
			}
		}
	}

	return batch
}

// ResolveChannelQuotaOverview builds ChannelQuotaOverview for a single GraphQL
// channel node, using any request-scoped preloaded batch to avoid N+1 queries.
func (svc *ChannelService) ResolveChannelQuotaOverview(
	ctx context.Context,
	ch *ent.Channel,
	providerQuotaSvc *ProviderQuotaService,
) *ChannelQuotaOverview {
	if ch == nil {
		return nil
	}
	now := time.Now().UTC()
	batch := channelQuotaOverviewBatchFromContext(ctx)

	owner := ch
	if srcID := credentialSourceID(ch.Settings); srcID != 0 {
		if batch != nil {
			batch.mu.RLock()
			resolved := batch.ownersByID[srcID]
			batch.mu.RUnlock()
			if resolved != nil {
				owner = resolved
			}
		}
		if owner == ch {
			if resolved, err := resolveCredentialOwner(ctx, svc.entFromContext(ctx), ch); err == nil && resolved != nil {
				owner = resolved
			}
		}
	}

	var pqs *ent.ProviderQuotaStatus
	if !ChannelUsesPerKeyQuotaAuthority(owner) {
		if owner.Edges.ProviderQuotaStatus != nil {
			pqs = owner.Edges.ProviderQuotaStatus
		} else if batch != nil {
			batch.mu.RLock()
			pqs = batch.pqsByChanID[owner.ID]
			batch.mu.RUnlock()
		} else if providerQuotaSvc != nil {
			pqs = providerQuotaSvc.GetCachedProviderQuotaStatus(ctx, owner.ID)
		} else if db := svc.entFromContext(ctx); db != nil {
			if row, err := db.ProviderQuotaStatus.Query().Where(providerquotastatus.ChannelIDEQ(owner.ID)).Only(ctx); err == nil {
				pqs = row
			}
		}
	}

	return svc.BuildChannelQuotaOverview(ctx, ch, owner, pqs, now)
}

// BuildChannelQuotaOverview is the single authoritative Go projection normalizing
// both per-key/shared-wallet ChannelBalanceProbe snapshots and channel-scoped
// ProviderQuotaStatus rows into one uniform ChannelQuotaOverview.
func (svc *ChannelService) BuildChannelQuotaOverview(
	_ context.Context,
	ch *ent.Channel,
	owner *ent.Channel,
	pqs *ent.ProviderQuotaStatus,
	now time.Time,
) *ChannelQuotaOverview {
	if owner == nil {
		owner = ch
	}
	if owner == nil {
		return nil
	}

	if !ChannelUsesPerKeyQuotaAuthority(owner) {
		return buildProviderQuotaOverview(owner, pqs, now)
	}

	return buildPerKeyQuotaOverview(owner, now)
}

func buildProviderQuotaOverview(owner *ent.Channel, pqs *ent.ProviderQuotaStatus, now time.Time) *ChannelQuotaOverview {
	overview := &ChannelQuotaOverview{
		Counts: ChannelQuotaCounts{
			Total:  1,
			Active: 1,
		},
		State:       ChannelQuotaStateUnknown,
		Windows:     []objects.ChannelQuotaWindow{},
		WalletScope: "provider_account",
		WalletID:    "provider:ch:" + strconvInt(owner.ID),
		Source:      "none",
		CanRefresh:  hasCredentialsForProvider(owner),
	}
	if pqs == nil {
		overview.Counts.Unknown = 1
		return overview
	}

	overview.Source = "provider_quota"
	checkedAt := pqs.UpdatedAt
	if !checkedAt.IsZero() {
		overview.CheckedAt = &checkedAt
	}
	if pqs.NextResetAt != nil {
		t := *pqs.NextResetAt
		overview.NextResetAt = &t
	}
	if planType := stringAt(pqs.QuotaData, "plan_type"); planType != "" {
		overview.PlanName = planType
	}
	if errMsg, ok := pqs.QuotaData["error"].(string); ok && strings.TrimSpace(errMsg) != "" {
		nextProbe := pqs.NextCheckAt
		overview.ProbeDiagnostic = &ChannelQuotaProbeDiagnostic{
			Status:      "error",
			Error:       strings.TrimSpace(errMsg),
			LastProbeAt: overview.CheckedAt,
			NextProbeAt: &nextProbe,
		}
	}

	// Extract representative windows from provider_quota_status.quota_data.rows when present.
	overview.Windows = extractProviderQuotaWindows(pqs.QuotaData, now)
	if len(overview.Windows) > 2 {
		overview.Windows = selectRepresentativeWindows(overview.Windows, now, 2)
	}

	switch pqs.Status {
	case providerquotastatus.StatusAvailable:
		overview.State = ChannelQuotaStateAvailable
		overview.Counts.FreshKnown = 1
	case providerquotastatus.StatusWarning:
		overview.State = ChannelQuotaStateWarning
		overview.Counts.FreshKnown = 1
	case providerquotastatus.StatusExhausted:
		overview.State = ChannelQuotaStateExhausted
		overview.Counts.FreshKnown = 1
		overview.Counts.Exhausted = 1
	default:
		overview.Counts.Unknown = 1
		if overview.ProbeDiagnostic != nil {
			overview.State = ChannelQuotaStateError
		} else {
			overview.State = ChannelQuotaStateUnknown
		}
	}
	return overview
}

func extractProviderQuotaWindows(data map[string]any, now time.Time) []objects.ChannelQuotaWindow {
	if len(data) == 0 {
		return []objects.ChannelQuotaWindow{}
	}

	var out []objects.ChannelQuotaWindow

	// 1. Row-array format (e.g. kimi_code).
	if rawRows, ok := data["rows"].([]any); ok && len(rawRows) > 0 {
		out = append(out, extractProviderQuotaRowSlice(rawRows, now)...)
	}

	// 2. Codex rate_limit.{primary_window, secondary_window}.
	if rateLimit, ok := data["rate_limit"].(map[string]any); ok && len(rateLimit) > 0 {
		out = append(out, extractCodexRateLimitWindows(rateLimit, now)...)
	}

	// 3. Named or slice windows (claudecode, opencode_go, cline, nanogpt).
	if rawWindows, exists := data["windows"]; exists && rawWindows != nil {
		switch typed := rawWindows.(type) {
		case []any:
			out = append(out, extractProviderQuotaRowSlice(typed, now)...)
		case map[string]any:
			out = append(out, extractProviderQuotaWindowMap(typed, now)...)
		}
	}

	if out == nil {
		return []objects.ChannelQuotaWindow{}
	}
	return out
}

func extractProviderQuotaRowSlice(rawRows []any, now time.Time) []objects.ChannelQuotaWindow {
	out := make([]objects.ChannelQuotaWindow, 0, len(rawRows))
	for i, item := range rawRows {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		label := firstString(stringAt(m, "label"), stringAt(m, "name"), stringAt(m, "key"), "Window")
		w := buildProviderQuotaWindowFromMap(canonicalQuotaWindowKey(label, i), label, m, now)
		if w != nil {
			out = append(out, *w)
		}
	}
	return out
}

func extractCodexRateLimitWindows(rateLimit map[string]any, now time.Time) []objects.ChannelQuotaWindow {
	var out []objects.ChannelQuotaWindow
	for _, spec := range []struct {
		field       string
		fallbackKey string
		fallbackLbl string
	}{
		{field: "primary_window", fallbackKey: "5h", fallbackLbl: "5h"},
		{field: "secondary_window", fallbackKey: "7d", fallbackLbl: "Weekly"},
	} {
		winMap, ok := rateLimit[spec.field].(map[string]any)
		if !ok || len(winMap) == 0 {
			continue
		}
		key, label := codexWindowKeyAndLabel(winMap, spec.fallbackKey, spec.fallbackLbl)
		w := buildProviderQuotaWindowFromMap(key, label, winMap, now)
		if w != nil {
			out = append(out, *w)
		}
	}
	return out
}

func codexWindowKeyAndLabel(win map[string]any, fallbackKey, fallbackLabel string) (string, string) {
	secs, ok := amountFromAny(valueAt(win, "limit_window_seconds"))
	if !ok || secs <= 0 {
		return fallbackKey, fallbackLabel
	}
	switch {
	case secs <= 6*3600:
		hrs := max(1, int(secs/3600+0.5))
		lbl := strconv.Itoa(hrs) + "h"
		return lbl, lbl
	case secs >= 5*86400 && secs <= 9*86400:
		return "7d", "Weekly"
	case secs >= 25*86400:
		return "30d", "Monthly"
	default:
		hrs := max(1, int(secs/3600+0.5))
		lbl := strconv.Itoa(hrs) + "h"
		return lbl, lbl
	}
}

func extractProviderQuotaWindowMap(winMap map[string]any, now time.Time) []objects.ChannelQuotaWindow {
	// Deterministic preferred ordering covering claudecode, opencode_go, cline, nanogpt.
	preferredOrder := []struct {
		rawKey string
		key    string
		label  string
	}{
		{"5h", "5h", "5h"},
		{"five_hour", "5h", "5h"},
		{"rolling", "5h", "5h"},
		{"last5h", "5h", "5h"},
		{"24h", "24h", "24h"},
		{"dailyInputTokens", "24h", "Daily Tokens"},
		{"dailyImages", "24h-img", "Daily Images"},
		{"7d", "7d", "Weekly"},
		{"seven_day", "7d", "Weekly"},
		{"weekly", "7d", "Weekly"},
		{"last7d", "7d", "Weekly"},
		{"weeklyInputTokens", "7d", "Weekly"},
		{"30d", "30d", "Monthly"},
		{"monthly", "30d", "Monthly"},
		{"last30d", "30d", "Monthly"},
	}

	seen := make(map[string]struct{}, len(winMap))
	out := make([]objects.ChannelQuotaWindow, 0, len(winMap))

	for _, spec := range preferredOrder {
		raw, ok := winMap[spec.rawKey]
		if !ok {
			continue
		}
		seen[spec.rawKey] = struct{}{}
		m, ok := raw.(map[string]any)
		if !ok || len(m) == 0 {
			continue
		}
		if w := buildProviderQuotaWindowFromMap(spec.key, spec.label, m, now); w != nil {
			out = append(out, *w)
		}
	}

	// Include any remaining non-overage keys deterministically.
	var extraKeys []string
	for k := range winMap {
		if _, ok := seen[k]; ok || strings.EqualFold(k, "overage") {
			continue
		}
		extraKeys = append(extraKeys, k)
	}
	sort.Strings(extraKeys)
	for i, k := range extraKeys {
		m, ok := winMap[k].(map[string]any)
		if !ok || len(m) == 0 {
			continue
		}
		if w := buildProviderQuotaWindowFromMap(canonicalQuotaWindowKey(k, i), k, m, now); w != nil {
			out = append(out, *w)
		}
	}

	return out
}

func buildProviderQuotaWindowFromMap(key, label string, m map[string]any, now time.Time) *objects.ChannelQuotaWindow {
	resetAt := parseBalanceProbeTime(firstNonNil(
		valueAt(m, "resetAt"),
		valueAt(m, "reset_at"),
		valueAt(m, "reset_time"),
		valueAt(m, "next_reset_at"),
		valueAt(m, "reset"),
	))
	if resetAt == nil {
		if resetAfter, ok := amountFromAny(firstNonNil(valueAt(m, "reset_after_seconds"), valueAt(m, "reset_in_seconds"))); ok && resetAfter > 0 {
			t := now.Add(time.Duration(resetAfter) * time.Second)
			resetAt = &t
		}
	}

	// 1. Explicit used / limit pair (rows, nanogpt used+remaining).
	// Note: Cline cost-unit estimates (`used_cost_units` / `limit_cost_units`) are secondary when
	// official `usage_ratio` / `usage_percent` is present, so only treat explicit `used`/`limit`
	// as primary credits over a percentage ratio.
	explicitUsed, explicitUsedOK := amountFromAny(valueAt(m, "used"))
	explicitLimit, explicitLimitOK := amountFromAny(valueAt(m, "limit"))
	explicitRem, explicitRemOK := amountFromAny(valueAt(m, "remaining"))
	if !explicitLimitOK && explicitUsedOK && explicitRemOK && explicitUsed+explicitRem > 0 {
		explicitLimit = explicitUsed + explicitRem
		explicitLimitOK = true
	}

	// 2. Percentage / utilization fields (codex used_percent, opencode_go usage_percent, claudecode utilization, cline usage_ratio, nanogpt percentUsed).
	var ratioPtr *float64
	if pct, ok := amountFromAny(firstNonNil(valueAt(m, "used_percent"), valueAt(m, "usage_percent"))); ok {
		r := max(0.0, pct/100.0)
		ratioPtr = &r
	} else if rVal, ok := amountFromAny(firstNonNil(valueAt(m, "usage_ratio"), valueAt(m, "utilization"), valueAt(m, "percentUsed"))); ok {
		r := rVal
		if r > 1.0 {
			r = r / 100.0
		}
		r = max(0.0, r)
		ratioPtr = &r
	} else if pctRem, ok := amountFromAny(valueAt(m, "percent_remaining")); ok {
		r := max(0.0, (100.0-pctRem)/100.0)
		ratioPtr = &r
	}

	if explicitLimitOK && explicitLimit > 0 {
		used := explicitUsed
		if !explicitUsedOK && explicitRemOK {
			used = max(0.0, explicitLimit-explicitRem)
		}
		rem := max(0.0, explicitLimit-used)
		if explicitRemOK {
			rem = max(0.0, explicitRem)
		}
		ratio := max(0.0, used/explicitLimit)
		if ratioPtr != nil {
			ratio = *ratioPtr
		}
		w := objects.ChannelQuotaWindow{
			Key:        key,
			Label:      label,
			Used:       lo.ToPtr(used),
			Limit:      lo.ToPtr(explicitLimit),
			Remaining:  lo.ToPtr(rem),
			UsageRatio: lo.ToPtr(ratio),
			Unit:       "CREDITS",
			ResetAt:    resetAt,
		}
		if w.ActiveAt(now) || w.Limit != nil {
			return &w
		}
		return nil
	}

	if ratioPtr != nil {
		// Guard against empty claudecode windows where utilization=0 and status="" and reset=0.
		if *ratioPtr == 0 && resetAt == nil && strings.TrimSpace(stringAt(m, "status")) == "" {
			return nil
		}
		pctUsed := max(0.0, *ratioPtr*100.0)
		pctRem := max(0.0, 100.0-pctUsed)
		w := objects.ChannelQuotaWindow{
			Key:        key,
			Label:      label,
			Used:       lo.ToPtr(pctUsed),
			Limit:      lo.ToPtr(100.0),
			Remaining:  lo.ToPtr(pctRem),
			UsageRatio: lo.ToPtr(*ratioPtr),
			Unit:       "%",
			ResetAt:    resetAt,
		}
		if w.ActiveAt(now) || w.Limit != nil {
			return &w
		}
		return nil
	}

	// Fallback to Cline cost-unit estimates only when no official utilization ratio was supplied.
	costUsed, costUsedOK := amountFromAny(valueAt(m, "used_cost_units"))
	costLimit, costLimitOK := amountFromAny(valueAt(m, "limit_cost_units"))
	if costLimitOK && costLimit > 0 {
		rem := max(0.0, costLimit-costUsed)
		if costRem, ok := amountFromAny(valueAt(m, "remaining_cost_units")); ok {
			rem = max(0.0, costRem)
		}
		if !costUsedOK {
			costUsed = max(0.0, costLimit-rem)
		}
		ratio := max(0.0, costUsed/costLimit)
		w := objects.ChannelQuotaWindow{
			Key:        key,
			Label:      label,
			Used:       lo.ToPtr(costUsed),
			Limit:      lo.ToPtr(costLimit),
			Remaining:  lo.ToPtr(rem),
			UsageRatio: lo.ToPtr(ratio),
			Unit:       "CREDITS",
			ResetAt:    resetAt,
		}
		if w.ActiveAt(now) || w.Limit != nil {
			return &w
		}
	}

	return nil
}

func buildPerKeyQuotaOverview(owner *ent.Channel, now time.Time) *ChannelQuotaOverview {
	sharedWallet := isChannelSharedWallet(owner)
	_, _, canProbe := channelBalanceProbeSpecForChannel(owner)

	disabledSet := make(map[string]struct{}, len(owner.DisabledAPIKeys))
	for _, dk := range owner.DisabledAPIKeys {
		if dk.Key != "" {
			disabledSet[dk.Key] = struct{}{}
		}
	}
	archivedSet := make(map[string]struct{})
	for _, a := range channelArchivedAPIKeys(owner.Settings) {
		if a.ID != "" {
			archivedSet[a.ID] = struct{}{}
		}
	}
	metaByID := map[string]objects.ChannelKeyMetadata{}
	var sharedSnap *objects.ChannelKeyBalanceSnapshot
	var sharedDiag *ChannelQuotaProbeDiagnostic
	if owner.Settings != nil && owner.Settings.KeyHealthCheck != nil {
		hc := owner.Settings.KeyHealthCheck
		metaByID = channelKeyMetadataByID(hc.KeyMetadata)
		sharedSnap = hc.SharedBalanceSnapshot
		if hc.SharedProbeStatus != "" && hc.SharedProbeStatus != "ok" {
			sharedDiag = &ChannelQuotaProbeDiagnostic{
				Status:      hc.SharedProbeStatus,
				Error:       hc.SharedProbeError,
				LastProbeAt: hc.SharedProbeAt,
				NextProbeAt: hc.SharedNextProbeAt,
			}
		}
	}

	overview := &ChannelQuotaOverview{
		Windows:      []objects.ChannelQuotaWindow{},
		SharedWallet: sharedWallet,
		Source:       "none",
		CanRefresh:   canProbe,
	}

	allKeys := owner.Credentials.GetAllAPIKeys()
	if sharedWallet {
		scope, wid := canonicalWalletIdentity(owner, "", true)
		overview.WalletScope = scope
		overview.WalletID = wid
	} else {
		overview.WalletScope = "credential"
	}

	seenWallets := make(map[string]struct{})
	currencies := make(map[string]struct{})
	var sumRemaining, sumUsed, sumLimit float64
	var haveRemaining, haveUsed, haveLimit, mixedOrPercent bool
	var allWindows []objects.ChannelQuotaWindow
	var latestCheckedAt *time.Time
	var earliestReset *time.Time
	var latestDiag *ChannelQuotaProbeDiagnostic
	var anyUnlimited, anyFreeTier, anyWarning, anyValidFact, anyManual bool

	updateCheckedAt := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if latestCheckedAt == nil || t.After(*latestCheckedAt) {
			cp := t
			latestCheckedAt = &cp
		}
	}
	updateEarliestReset := func(t *time.Time) {
		if t == nil || t.IsZero() {
			return
		}
		if earliestReset == nil || t.Before(*earliestReset) {
			cp := *t
			earliestReset = &cp
		}
	}

	for _, rawKey := range allKeys {
		keyID := objects.ChannelAPIKeyFingerprint(rawKey)
		if _, isArchived := archivedSet[keyID]; isArchived {
			continue
		}
		overview.Counts.Total++
		_, isDisabled := disabledSet[rawKey]
		if !isDisabled {
			overview.Counts.Active++
		}

		meta := metaByID[keyID]
		if meta.LastProbeStatus != "" && meta.LastProbeStatus != "ok" {
			if latestDiag == nil || (meta.LastProbeAt != nil && (latestDiag.LastProbeAt == nil || meta.LastProbeAt.After(*latestDiag.LastProbeAt))) {
				latestDiag = &ChannelQuotaProbeDiagnostic{
					Status:      meta.LastProbeStatus,
					Error:       meta.LastProbeError,
					LastProbeAt: meta.LastProbeAt,
					NextProbeAt: meta.NextProbeAt,
				}
			}
		}

		snap := meta.BalanceSnapshot
		if sharedWallet && sharedSnap != nil {
			snap = sharedSnap
		}
		if snap == nil || !snap.Success {
			if !isDisabled {
				overview.Counts.Unknown++
			}
			continue
		}

		anyValidFact = true
		if snap.Source == "manual" || (snap.Source == "" && (owner.Settings == nil || owner.Settings.BalanceProbe == nil)) {
			anyManual = true
		}
		updateCheckedAt(snap.CheckedAt)
		if overview.PlanName == "" && snap.PlanName != "" {
			overview.PlanName = snap.PlanName
		}

		if !isDisabled {
			if snap.CheckedAt.IsZero() || now.Sub(snap.CheckedAt) <= quotaOverviewFreshnessWindow {
				overview.Counts.FreshKnown++
			}
			if snap.ExhaustedAt(now) {
				overview.Counts.Exhausted++
			}
		}

		if snap.Unlimited != nil && *snap.Unlimited {
			anyUnlimited = true
		}
		if snap.FreeTier != nil && *snap.FreeTier {
			anyFreeTier = true
		}
		if worst, ok := snap.WorstUsageRatioAt(now); ok && worst >= 0.8 && !snap.ExhaustedAt(now) {
			anyWarning = true
		}

		if len(snap.Windows) > 0 {
			for _, w := range snap.Windows {
				allWindows = append(allWindows, w)
				updateEarliestReset(w.ResetAt)
			}
		} else {
			updateEarliestReset(snap.ResetAt)
		}

		// Deduplicate wallet amounts by canonical wallet identity so SharedWallet
		// channels NEVER multiply balances by N keys!
		walletKey := QuotaCanonicalWalletID(owner, keyID, snap)
		if _, alreadyCounted := seenWallets[walletKey]; alreadyCounted {
			continue
		}
		seenWallets[walletKey] = struct{}{}

		// Do not sum rolling percentage windows, unlimited sentinels, or free-tier placeholders.
		if (snap.Unlimited != nil && *snap.Unlimited) || (snap.FreeTier != nil && *snap.FreeTier) {
			continue
		}
		unit := snapshotWalletUnit(snap)
		if unit == "" || unit == "%" {
			if snap.Remaining != nil || snap.Limit != nil {
				mixedOrPercent = true
			}
			continue
		}
		currencies[unit] = struct{}{}
		if snap.Remaining != nil && finiteFloatVal(*snap.Remaining) {
			sumRemaining += *snap.Remaining
			haveRemaining = true
		}
		if snap.Used != nil && finiteFloatVal(*snap.Used) {
			sumUsed += *snap.Used
			haveUsed = true
		}
		if snap.Limit != nil && finiteFloatVal(*snap.Limit) {
			sumLimit += *snap.Limit
			haveLimit = true
		}
	}

	if sharedDiag != nil {
		overview.ProbeDiagnostic = sharedDiag
	} else {
		overview.ProbeDiagnostic = latestDiag
	}
	overview.CheckedAt = latestCheckedAt
	overview.NextResetAt = earliestReset
	overview.Unlimited = anyUnlimited
	overview.FreeTier = anyFreeTier && !haveRemaining

	if len(allWindows) > 0 {
		overview.Windows = selectRepresentativeWindows(allWindows, now, 2)
	}

	if len(currencies) == 1 && !mixedOrPercent {
		var unit string
		for u := range currencies {
			unit = u
		}
		overview.WalletUnit = unit
		if haveRemaining {
			r := round2(sumRemaining)
			overview.WalletRemaining = &r
			overview.WalletBalance = &objects.BalanceAmount{
				Amount:   r,
				Currency: unit,
				Kind:     objects.BalanceAmountKindRemaining,
				Label:    "remaining",
			}
		}
		if haveUsed {
			u := round2(sumUsed)
			overview.WalletUsed = &u
		}
		if haveLimit {
			l := round2(sumLimit)
			overview.WalletLimit = &l
		}
	}

	if anyValidFact {
		if anyManual {
			overview.Source = "manual"
		} else {
			overview.Source = "balance_probe"
		}
	}

	switch {
	case overview.Counts.Active > 0 && overview.Counts.Exhausted >= overview.Counts.Active:
		overview.State = ChannelQuotaStateExhausted
	case anyUnlimited:
		overview.State = ChannelQuotaStateUnlimited
	case anyFreeTier && !haveRemaining:
		overview.State = ChannelQuotaStateFreeTier
	case overview.Counts.Exhausted > 0 || anyWarning:
		overview.State = ChannelQuotaStateWarning
	case anyValidFact:
		overview.State = ChannelQuotaStateAvailable
	case overview.ProbeDiagnostic != nil && overview.ProbeDiagnostic.Status == "unsupported":
		overview.State = ChannelQuotaStateUnsupported
	case overview.ProbeDiagnostic != nil:
		overview.State = ChannelQuotaStateError
	case !canProbe:
		overview.State = ChannelQuotaStateUnsupported
	default:
		overview.State = ChannelQuotaStateUnknown
	}

	return overview
}

// QuotaCanonicalWalletID returns the canonical wallet identity for deduplicating
// balance sums across keys in one channel AND across credential-source alias channels.
// Owner shared-wallet scope always takes precedence over any legacy per-key snapshot WalletID.
func QuotaCanonicalWalletID(ch *ent.Channel, keyID string, snapshot *objects.ChannelKeyBalanceSnapshot) string {
	if isChannelSharedWallet(ch) {
		_, wid := canonicalWalletIdentity(ch, keyID, true)
		return wid
	}
	if snapshot != nil && strings.TrimSpace(snapshot.WalletID) != "" {
		return strings.TrimSpace(snapshot.WalletID)
	}
	_, wid := canonicalWalletIdentity(ch, keyID, false)
	return wid
}

func snapshotWalletUnit(snap *objects.ChannelKeyBalanceSnapshot) string {
	if snap == nil {
		return ""
	}
	if snap.PrimaryBalance != nil && strings.TrimSpace(snap.PrimaryBalance.Currency) != "" {
		return strings.ToUpper(strings.TrimSpace(snap.PrimaryBalance.Currency))
	}
	for _, c := range snap.Components {
		if strings.TrimSpace(c.Currency) != "" {
			return strings.ToUpper(strings.TrimSpace(c.Currency))
		}
	}
	return ""
}

// selectRepresentativeWindows groups windows by canonical key (e.g. "5h", "weekly")
// and picks the worst (highest active usageRatio) observation per key, returning
// at most maxCount windows ordered by urgency.
func selectRepresentativeWindows(windows []objects.ChannelQuotaWindow, now time.Time, maxCount int) []objects.ChannelQuotaWindow {
	if len(windows) == 0 {
		return []objects.ChannelQuotaWindow{}
	}
	byKey := make(map[string]objects.ChannelQuotaWindow)
	for _, w := range windows {
		key := w.Key
		if key == "" {
			key = w.Label
		}
		prev, exists := byKey[key]
		if !exists {
			byKey[key] = w
			continue
		}
		if windowMoreUrgent(w, prev, now) {
			byKey[key] = w
		}
	}
	merged := make([]objects.ChannelQuotaWindow, 0, len(byKey))
	for _, w := range byKey {
		merged = append(merged, w)
	}
	sort.Slice(merged, func(i, j int) bool {
		if windowMoreUrgent(merged[i], merged[j], now) {
			return true
		}
		if windowMoreUrgent(merged[j], merged[i], now) {
			return false
		}
		return merged[i].Key < merged[j].Key
	})
	if maxCount > 0 && len(merged) > maxCount {
		merged = merged[:maxCount]
	}
	return merged
}

func windowMoreUrgent(a, b objects.ChannelQuotaWindow, now time.Time) bool {
	aExh, bExh := a.ExhaustedAt(now), b.ExhaustedAt(now)
	if aExh != bExh {
		return aExh
	}
	ar, aOK := a.EffectiveUsageRatio()
	br, bOK := b.EffectiveUsageRatio()
	if aOK && bOK && ar != br {
		return ar > br
	}
	if aOK != bOK {
		return aOK
	}
	if a.ResetAt != nil && b.ResetAt != nil && !a.ResetAt.Equal(*b.ResetAt) {
		return a.ResetAt.Before(*b.ResetAt)
	}
	return false
}

func finiteFloatVal(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

func strconvInt(v int) string {
	return string(appendInt(nil, v))
}

func appendInt(dst []byte, v int) []byte {
	if v == 0 {
		return append(dst, '0')
	}
	if v < 0 {
		dst = append(dst, '-')
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return append(dst, buf[i:]...)
}
