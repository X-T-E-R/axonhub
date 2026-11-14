package biz

import (
	"net/url"
	"strings"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	providerquota "github.com/looplj/axonhub/internal/server/biz/provider_quota"
)

const balanceProbeCreditsCurrency = "CREDITS"

func isTavilyBalanceProbeChannel(ch *ent.Channel) bool {
	if ch == nil {
		return false
	}
	settings := ch.Settings
	if settings != nil && settings.RawHTTP != nil && strings.EqualFold(strings.TrimSpace(settings.RawHTTP.Resource), "tavily-search") {
		return true
	}

	return isTavilyBalanceProbeBaseURL(ch.BaseURL)
}

func parseTavilyUsageBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	payload := decodeBalanceProbeJSON(body)
	snapshot := baseBalanceSnapshot(statusCode)
	key := mapAt(payload, "key")
	account := mapAt(payload, "account")

	keyUsage, keyUsageOK := amountFromAny(valueAt(key, "usage"))
	keyLimit, keyLimitOK := amountFromAny(valueAt(key, "limit"))
	planUsage, planUsageOK := amountFromAny(valueAt(account, "plan_usage"))
	paygoUsage, paygoUsageOK := amountFromAny(valueAt(account, "paygo_usage"))
	planLimit, planLimitOK := amountFromAny(valueAt(account, "plan_limit"))
	paygoLimit, paygoLimitOK := amountFromAny(valueAt(account, "paygo_limit"))

	used, usedOK := keyUsage, keyUsageOK
	if !usedOK && (planUsageOK || paygoUsageOK) {
		used = optionalAmountSum(planUsage, planUsageOK, paygoUsage, paygoUsageOK)
		usedOK = true
	}

	limit, limitOK := keyLimit, keyLimitOK
	if !limitOK && (planLimitOK || paygoLimitOK) {
		limit = optionalAmountSum(planLimit, planLimitOK, paygoLimit, paygoLimitOK)
		limitOK = true
	}

	snapshot.AccountStatus = stringAt(account, "current_plan")
	snapshot.RawSummary = safeSummary(payload,
		"key.usage", "key.limit", "account.plan_usage", "account.plan_limit",
		"account.paygo_usage", "account.paygo_limit", "account.current_plan",
	)
	if limitOK {
		appendProbeBalanceAmount(&snapshot, limit, balanceProbeCreditsCurrency, objects.BalanceAmountKindLimit, "limit", nil)
	}
	if usedOK {
		appendProbeBalanceAmount(&snapshot, used, balanceProbeCreditsCurrency, objects.BalanceAmountKindUsed, "usage", nil)
	}
	if limitOK && usedOK {
		remaining := limit - used
		if remaining < 0 {
			remaining = 0
		}
		appendProbeBalanceAmount(&snapshot, remaining, balanceProbeCreditsCurrency, objects.BalanceAmountKindRemaining, "remaining", nil)
		snapshot.PrimaryBalance = &snapshot.Components[len(snapshot.Components)-1]
		if limit > 0 {
			snapshot.Available = boolPtr(remaining > 0)
		}
	}

	return snapshot
}

func parseKimiCodeUsageBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	snapshot := baseBalanceSnapshot(statusCode)
	windows, err := providerquota.ParseKimiCodeUsageWindows(body)
	if err != nil {
		snapshot.Success = false
		return snapshot
	}

	snapshot.RawSummary = map[string]any{"windows": len(windows)}
	var selected *providerquota.KimiCodeUsageWindow
	selectedIndex := -1
	var earliestReset *time.Time
	available := true
	hasHardWindow := false
	for i := range windows {
		window := windows[i]
		if selected == nil || (selected.Limit <= 0 && window.Limit > 0) {
			selected = &windows[i]
			selectedIndex = i
		}
		if window.ResetAt != nil && (earliestReset == nil || window.ResetAt.Before(*earliestReset)) {
			resetAt := *window.ResetAt
			earliestReset = &resetAt
		}
		if window.Limit > 0 {
			hasHardWindow = true
			if window.Exhausted {
				available = false
			}
		}
		appendProbeBalanceAmount(&snapshot, window.Limit, balanceProbeCreditsCurrency, objects.BalanceAmountKindLimit, window.Label+" limit", window.ResetAt)
		appendProbeBalanceAmount(&snapshot, window.Used, balanceProbeCreditsCurrency, objects.BalanceAmountKindUsed, window.Label+" used", window.ResetAt)
		appendProbeBalanceAmount(&snapshot, window.Remaining, balanceProbeCreditsCurrency, objects.BalanceAmountKindRemaining, window.Label+" remaining", window.ResetAt)
	}
	if selected != nil {
		used := selected.Used
		limit := selected.Limit
		remaining := selected.Remaining
		snapshot.Used = &used
		snapshot.Limit = &limit
		snapshot.Remaining = &remaining
		if limit > 0 {
			ratio := used / limit
			if ratio < 0 {
				ratio = 0
			}
			if ratio > 1 {
				ratio = 1
			}
			snapshot.UsageRatio = &ratio
		}
		remainingIndex := selectedIndex*3 + 2
		if remainingIndex >= 0 && remainingIndex < len(snapshot.Components) {
			snapshot.PrimaryBalance = &snapshot.Components[remainingIndex]
		}
	}
	if hasHardWindow {
		availablePtr := available
		snapshot.Available = &availablePtr
	}
	snapshot.ResetAt = earliestReset

	return snapshot
}

func parseExaTeamCreditsBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	payload := decodeBalanceProbeJSON(body)
	snapshot := baseBalanceSnapshot(statusCode)
	remaining, ok := amountFromAny(valueAt(payload, "credits_usd"))
	if ok {
		if remaining < 0 {
			remaining = 0
		}
		expiresAt := firstExaExpiry(payload)
		appendProbeBalanceAmount(&snapshot, remaining, "USD", objects.BalanceAmountKindRemaining, "credits", expiresAt)
		snapshot.PrimaryBalance = &snapshot.Components[len(snapshot.Components)-1]
		snapshot.ResetAt = expiresAt
		available := remaining > 0
		snapshot.Available = &available
	}
	snapshot.RawSummary = safeSummary(payload, "credits_usd", "expiring_credits")
	return snapshot
}

func parseFirecrawlCreditsBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	payload := decodeBalanceProbeJSON(body)
	snapshot := baseBalanceSnapshot(statusCode)
	data := mapAt(payload, "data")
	remaining, remainingOK := amountFromAny(valueAt(data, "remainingCredits"))
	limit, limitOK := amountFromAny(valueAt(data, "planCredits"))
	if remainingOK {
		if remaining < 0 {
			remaining = 0
		}
		appendProbeBalanceAmount(&snapshot, remaining, balanceProbeCreditsCurrency, objects.BalanceAmountKindRemaining, "remaining credits", nil)
		if snapshot.PrimaryBalance == nil {
			snapshot.PrimaryBalance = &snapshot.Components[len(snapshot.Components)-1]
		}
	}
	if limitOK {
		if limit < 0 {
			limit = 0
		}
		appendProbeBalanceAmount(&snapshot, limit, balanceProbeCreditsCurrency, objects.BalanceAmountKindLimit, "plan credits", nil)
	}
	if remainingOK && limitOK {
		used := limit - remaining
		if used < 0 {
			used = 0
		}
		appendProbeBalanceAmount(&snapshot, used, balanceProbeCreditsCurrency, objects.BalanceAmountKindUsed, "used credits", nil)
		if limit > 0 || remaining > 0 {
			available := remaining > 0
			snapshot.Available = &available
		}
	}
	snapshot.RawSummary = safeSummary(payload, "data.remainingCredits", "data.planCredits")
	return snapshot
}

func appendProbeBalanceAmount(snapshot *objects.ChannelKeyBalanceSnapshot, amount float64, currency string, kind objects.BalanceAmountKind, label string, expiresAt *time.Time) {
	if snapshot == nil {
		return
	}
	snapshot.Components = append(snapshot.Components, objects.BalanceAmount{
		Amount:    amount,
		Currency:  strings.ToUpper(strings.TrimSpace(currency)),
		Kind:      kind,
		Label:     label,
		ExpiresAt: expiresAt,
	})
}

func optionalAmountSum(first float64, firstOK bool, second float64, secondOK bool) float64 {
	var sum float64
	if firstOK {
		sum += first
	}
	if secondOK {
		sum += second
	}
	return sum
}

func firstExaExpiry(payload map[string]any) *time.Time {
	items := arrayAt(payload, "expiring_credits")
	if len(items) == 0 {
		return nil
	}
	item, _ := items[0].(map[string]any)
	return parseBalanceProbeTime(valueAt(item, "expires_at"))
}

func parseBalanceProbeTime(value any) *time.Time {
	text, ok := value.(string)
	if !ok {
		return nil
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		parsed, err := time.Parse(layout, text)
		if err == nil {
			return &parsed
		}
	}
	return nil
}

func boolPtr(value bool) *bool {
	return &value
}

func isTavilyBalanceProbeBaseURL(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	return err == nil && parsed != nil && strings.EqualFold(parsed.Hostname(), "api.tavily.com")
}
