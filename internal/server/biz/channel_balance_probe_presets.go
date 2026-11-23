package biz

import (
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	providerquota "github.com/looplj/axonhub/internal/server/biz/provider_quota"
)

const (
	balanceProbeCreditsCurrency = "CREDITS"
	newAPIQuotaPerUSD           = 500000.0
	openAIBillingUnlimitedSent  = 10000000.0
	commandCodeUserAgent        = "commandcode-cli/1.54.2"
)

// exaTeamCreditsEndpointURL is locked to the official HTTPS Admin endpoint.
// It is a package variable solely so unit tests in package biz can redirect
// against a local httptest server without allowing user/config HTTP overrides.
var exaTeamCreditsEndpointURL = "https://admin-api.exa.ai/team-management/credits"

type commandCodePlanInfo struct {
	name string
	cap  float64
}

var commandCodePlanLimits = map[string]commandCodePlanInfo{
	"individual-go":       {name: "Go", cap: 10.0},
	"individual-provider": {name: "Provider", cap: 15.0},
	"individual-pro":      {name: "Pro", cap: 30.0},
	"teams-pro":           {name: "Teams Pro", cap: 40.0},
	"individual-goat":     {name: "GOAT", cap: 70.0},
	"individual-max":      {name: "Max", cap: 150.0},
	"individual-ultra":    {name: "Ultra", cap: 300.0},
}

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

func channelBalanceProbePresetForBaseURL(ch *ent.Channel) (objects.ChannelBalanceProbePreset, bool) {
	if ch == nil {
		return "", false
	}
	if isTavilyBalanceProbeChannel(ch) {
		return objects.ChannelBalanceProbePresetTavily, true
	}
	parsed, err := url.Parse(strings.TrimSpace(ch.BaseURL))
	if err != nil || parsed == nil {
		return "", false
	}
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(parsed.Hostname()), "."))
	switch {
	case host == "":
		return "", false
	case host == "api.commandcode.ai" || strings.HasSuffix(host, ".commandcode.ai"):
		return objects.ChannelBalanceProbePresetCommandCode, true
	case host == "api.stepfun.com" || strings.HasSuffix(host, ".stepfun.com"):
		return objects.ChannelBalanceProbePresetStepPlan, true
	case host == "openrouter.ai" || strings.HasSuffix(host, ".openrouter.ai") || host == "api.experientiallabs.ai":
		return objects.ChannelBalanceProbePresetOpenRouter, true
	case host == "api.kimi.com":
		return objects.ChannelBalanceProbePresetKimiCode, true
	case host == "api.exa.ai" || host == "admin-api.exa.ai":
		return objects.ChannelBalanceProbePresetExa, true
	case host == "api.firecrawl.dev":
		return objects.ChannelBalanceProbePresetFirecrawl, true
	case host == "api.openai-next.com":
		return objects.ChannelBalanceProbePresetOpenAIBilling, true
	case host == "api.n.icu" || host == "ioll.pp.ua" || host == "codingplan.alayanew.com":
		return objects.ChannelBalanceProbePresetNewAPI, true
	default:
		return "", false
	}
}

func originFromBaseURL(baseURL string, fallbackOrigin string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed == nil || parsed.Scheme == "" || parsed.Host == "" {
		return strings.TrimRight(fallbackOrigin, "/")
	}
	return parsed.Scheme + "://" + parsed.Host
}

func parseTavilyUsageBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	payload := decodeBalanceProbeJSON(body)
	snapshot := baseBalanceSnapshot(statusCode)
	if len(payload) == 0 {
		snapshot.Success = false
		return snapshot
	}
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

	if !usedOK && !limitOK {
		snapshot.Success = false
		return snapshot
	}

	snapshot.AccountStatus = stringAt(account, "current_plan")
	snapshot.PlanName = snapshot.AccountStatus
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

func canonicalQuotaWindowKey(label string, index int) string {
	lower := strings.ToLower(strings.TrimSpace(label))
	switch {
	case strings.Contains(lower, "5h") || strings.Contains(lower, "5 hour") || strings.Contains(lower, "five hour") || strings.Contains(lower, "five_hour"):
		return "5h"
	case strings.Contains(lower, "week") || strings.Contains(lower, "7d") || strings.Contains(lower, "7 day"):
		return "weekly"
	case strings.Contains(lower, "day") || strings.Contains(lower, "daily") || strings.Contains(lower, "24h"):
		return "daily"
	case strings.Contains(lower, "month") || strings.Contains(lower, "30d"):
		return "monthly"
	default:
		if lower != "" {
			return strings.ReplaceAll(lower, " ", "_")
		}
		return "window_" + strconv.Itoa(index+1)
	}
}

func parseKimiCodeUsageBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	snapshot := baseBalanceSnapshot(statusCode)
	windows, err := providerquota.ParseKimiCodeUsageWindows(body)
	if err != nil {
		snapshot.Success = false
		return snapshot
	}

	snapshot.RawSummary = map[string]any{"windows": len(windows)}
	snapshot.Windows = make([]objects.ChannelQuotaWindow, 0, len(windows))
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
		var ratioPtr *float64
		if window.Limit > 0 {
			hasHardWindow = true
			if window.Exhausted {
				available = false
			}
			ratio := max(0.0, min(1.0, window.Used/window.Limit))
			ratioPtr = &ratio
		}
		var winReset *time.Time
		if window.ResetAt != nil {
			t := *window.ResetAt
			winReset = &t
		}
		snapshot.Windows = append(snapshot.Windows, objects.ChannelQuotaWindow{
			Key:        canonicalQuotaWindowKey(window.Label, i),
			Label:      window.Label,
			Used:       lo.ToPtr(window.Used),
			Limit:      lo.ToPtr(window.Limit),
			Remaining:  lo.ToPtr(window.Remaining),
			UsageRatio: ratioPtr,
			Unit:       balanceProbeCreditsCurrency,
			ResetAt:    winReset,
		})
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
			ratio := max(0.0, min(1.0, used/limit))
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
	// ResetAt is preserved strictly as a display fact ("next reset"); multi-window
	// exhaustion evaluates each window in snapshot.Windows independently.
	snapshot.ResetAt = earliestReset

	return snapshot
}

func parseExaTeamCreditsBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	payload := decodeBalanceProbeJSON(body)
	snapshot := baseBalanceSnapshot(statusCode)
	if len(payload) == 0 {
		snapshot.Success = false
		return snapshot
	}
	remaining, ok := amountFromAny(valueAt(payload, "credits_usd"))
	if !ok {
		snapshot.Success = false
		return snapshot
	}
	if remaining < 0 {
		remaining = 0
	}
	expiresAt := firstExaExpiry(payload)
	appendProbeBalanceAmount(&snapshot, remaining, "USD", objects.BalanceAmountKindRemaining, "credits", expiresAt)
	snapshot.PrimaryBalance = &snapshot.Components[len(snapshot.Components)-1]
	snapshot.ResetAt = expiresAt
	available := remaining > 0
	snapshot.Available = &available
	snapshot.RawSummary = safeSummary(payload, "credits_usd", "expiring_credits")
	return snapshot
}

func parseFirecrawlCreditsBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	payload := decodeBalanceProbeJSON(body)
	snapshot := baseBalanceSnapshot(statusCode)
	if len(payload) == 0 {
		snapshot.Success = false
		return snapshot
	}
	data := mapAt(payload, "data")
	remaining, remainingOK := amountFromAny(valueAt(data, "remainingCredits"))
	limit, limitOK := amountFromAny(valueAt(data, "planCredits"))
	if !remainingOK && !limitOK {
		snapshot.Success = false
		return snapshot
	}
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

func parseCommandCodeBillingSnapshot(creditsBody []byte, subsBody []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	creditsDoc := decodeBalanceProbeJSON(creditsBody)
	snapshot := baseBalanceSnapshot(statusCode)
	if len(creditsDoc) == 0 {
		snapshot.Success = false
		return snapshot
	}
	credits := mapAt(creditsDoc, "credits")
	wl := mapAt(creditsDoc, "windowLimits")
	if len(credits) == 0 && len(wl) == 0 {
		snapshot.Success = false
		return snapshot
	}

	monthlyRem, _ := amountFromAny(valueAt(credits, "monthlyCredits"))
	purchasedRem, _ := amountFromAny(valueAt(credits, "purchasedCredits"))
	freeRem, _ := amountFromAny(valueAt(credits, "freeCredits"))
	totalRem := round2(max(0.0, monthlyRem+purchasedRem+freeRem))

	subsDoc := decodeBalanceProbeJSON(subsBody)
	subData := mapAt(subsDoc, "data")
	if len(subData) == 0 {
		subData = subsDoc
	}
	planID := strings.ToLower(stringAt(subData, "planId"))
	planInfo, ok := commandCodePlanLimits[planID]
	if !ok {
		if monthlyRem > 30 {
			planInfo = commandCodePlanInfo{name: "GOAT", cap: 70.0}
		} else {
			planInfo = commandCodePlanInfo{name: "Plan", cap: 70.0}
		}
	}
	totalLimit := round2(max(planInfo.cap, monthlyRem) + purchasedRem + freeRem)
	usedMonthly := round2(max(0.0, totalLimit-totalRem))

	snapshot.PlanName = planInfo.name
	snapshot.AccountStatus = firstString(strings.ToLower(stringAt(subData, "status")), "active")
	periodReset := parseBalanceProbeTime(valueAt(subData, "currentPeriodEnd"))

	var earliestReset *time.Time
	updateEarliest := func(t *time.Time) {
		if t == nil {
			return
		}
		if earliestReset == nil || t.Before(*earliestReset) {
			cp := *t
			earliestReset = &cp
		}
	}
	updateEarliest(periodReset)

	parseWindow := func(raw map[string]any, key, label string) {
		if len(raw) == 0 {
			return
		}
		capVal, capOK := amountFromAny(valueAt(raw, "cap"))
		usedVal, usedOK := amountFromAny(valueAt(raw, "used"))
		if !capOK && !usedOK {
			return
		}
		resetAt := parseBalanceProbeTime(valueAt(raw, "resetAt"))
		updateEarliest(resetAt)
		w := objects.ChannelQuotaWindow{
			Key:     key,
			Label:   label,
			Unit:    "USD",
			ResetAt: resetAt,
		}
		if usedOK {
			u := round2(max(0.0, usedVal))
			w.Used = &u
		}
		if capOK && capVal > 0 {
			c := round2(capVal)
			rem := round2(max(0.0, capVal-usedVal))
			ratio := max(0.0, min(1.0, usedVal/capVal))
			w.Limit = &c
			w.Remaining = &rem
			w.UsageRatio = &ratio
		}
		snapshot.Windows = append(snapshot.Windows, w)
	}

	parseWindow(mapAt(wl, "fiveHour"), "5h", "5h limit")
	parseWindow(mapAt(wl, "weekly"), "weekly", "Weekly limit")
	if totalLimit > 0 {
		ratio := max(0.0, min(1.0, usedMonthly/totalLimit))
		snapshot.Windows = append(snapshot.Windows, objects.ChannelQuotaWindow{
			Key:        "monthly",
			Label:      "Monthly credits",
			Used:       lo.ToPtr(usedMonthly),
			Limit:      lo.ToPtr(totalLimit),
			Remaining:  lo.ToPtr(totalRem),
			UsageRatio: lo.ToPtr(ratio),
			Unit:       "USD",
			ResetAt:    periodReset,
		})
	}

	appendProbeBalanceAmount(&snapshot, totalRem, "USD", objects.BalanceAmountKindRemaining, "remaining credits", periodReset)
	snapshot.PrimaryBalance = &snapshot.Components[len(snapshot.Components)-1]
	appendProbeBalanceAmount(&snapshot, totalLimit, "USD", objects.BalanceAmountKindLimit, "monthly limit", periodReset)
	appendProbeBalanceAmount(&snapshot, usedMonthly, "USD", objects.BalanceAmountKindUsed, "monthly used", periodReset)
	snapshot.Remaining = lo.ToPtr(totalRem)
	snapshot.Limit = lo.ToPtr(totalLimit)
	snapshot.Used = lo.ToPtr(usedMonthly)
	if worst, ok := snapshot.WorstUsageRatioAt(time.Now()); ok {
		snapshot.UsageRatio = lo.ToPtr(worst)
	}
	snapshot.ResetAt = earliestReset
	snapshot.Available = lo.ToPtr(!snapshot.ExhaustedAt(time.Now()))
	snapshot.RawSummary = map[string]any{
		"planId":           planID,
		"planName":         planInfo.name,
		"monthlyCredits":   monthlyRem,
		"purchasedCredits": purchasedRem,
		"freeCredits":      freeRem,
	}
	return snapshot
}

func parseStepPlanUsageSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	payload := decodeBalanceProbeJSON(body)
	snapshot := baseBalanceSnapshot(statusCode)
	if len(payload) == 0 {
		snapshot.Success = false
		return snapshot
	}
	q := mapAt(payload, "quota")
	if len(q) == 0 {
		snapshot.Success = false
		return snapshot
	}
	plan := mapAt(payload, "plan")
	allowed := boolFromAny(valueAt(q, "allowed"))
	reason := stringAt(q, "reason")
	snapshot.PlanName = firstString(stringAt(plan, "name"), stringAt(plan, "family"), "Step Plan")
	snapshot.AccountStatus = reason
	snapshot.RawSummary = safeSummary(payload, "plan.name", "plan.family", "quota.allowed", "quota.reason")

	if allowed != nil && !*allowed {
		snapshot.Available = lo.ToPtr(false)
		ratio := 1.0
		snapshot.UsageRatio = &ratio
		return snapshot
	}

	rawWindows := arrayAt(q, "windows")
	var earliestReset *time.Time
	for i, item := range rawWindows {
		wm, ok := item.(map[string]any)
		if !ok || len(wm) == 0 {
			continue
		}
		label := firstString(stringAt(wm, "label"), stringAt(wm, "name"), stringAt(wm, "window"), "Window "+strconv.Itoa(i+1))
		var usedRatio *float64
		if remRatio, ok := amountFromAny(valueAt(wm, "remaining_ratio")); ok {
			r := max(0.0, min(1.0, 1.0-remRatio))
			usedRatio = &r
		} else if ur, ok := amountFromAny(valueAt(wm, "used_ratio")); ok {
			r := max(0.0, min(1.0, ur))
			usedRatio = &r
		}
		resetAt := parseBalanceProbeTime(firstNonNil(valueAt(wm, "reset_at"), valueAt(wm, "resetAt")))
		if resetAt != nil && (earliestReset == nil || resetAt.Before(*earliestReset)) {
			cp := *resetAt
			earliestReset = &cp
		}
		w := objects.ChannelQuotaWindow{
			Key:        canonicalQuotaWindowKey(label, i),
			Label:      label,
			UsageRatio: usedRatio,
			Unit:       "%",
			ResetAt:    resetAt,
		}
		if usedRatio != nil {
			usedPct := round1(*usedRatio * 100.0)
			remPct := round1((1.0 - *usedRatio) * 100.0)
			limitPct := 100.0
			w.Used = &usedPct
			w.Remaining = &remPct
			w.Limit = &limitPct
		}
		snapshot.Windows = append(snapshot.Windows, w)
	}
	if worst, ok := snapshot.WorstUsageRatioAt(time.Now()); ok {
		snapshot.UsageRatio = lo.ToPtr(worst)
	}
	snapshot.ResetAt = earliestReset
	if allowed != nil {
		snapshot.Available = lo.ToPtr(*allowed && !snapshot.ExhaustedAt(time.Now()))
	} else {
		snapshot.Available = lo.ToPtr(!snapshot.ExhaustedAt(time.Now()))
	}
	return snapshot
}

func parseNewAPITokenUsageSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	payload := decodeBalanceProbeJSON(body)
	snapshot := baseBalanceSnapshot(statusCode)
	data := mapAt(payload, "data")
	if len(data) == 0 {
		snapshot.Success = false
		return snapshot
	}
	if stringAt(data, "object") != "token_usage" && valueAt(data, "total_available") == nil && valueAt(data, "unlimited_quota") == nil {
		snapshot.Success = false
		return snapshot
	}

	unlimited := false
	if u := boolFromAny(valueAt(data, "unlimited_quota")); u != nil {
		unlimited = *u
	}
	grantedQ, grantedOK := amountFromAny(valueAt(data, "total_granted"))
	usedQ, usedOK := amountFromAny(valueAt(data, "total_used"))
	availQ, availOK := amountFromAny(valueAt(data, "total_available"))

	if !unlimited && !availOK && !(grantedOK && usedOK) {
		snapshot.Success = false
		return snapshot
	}

	usedUSD := round2(max(0.0, usedQ) / newAPIQuotaPerUSD)
	if usedOK || unlimited {
		snapshot.Used = lo.ToPtr(usedUSD)
	}
	snapshot.RawSummary = safeSummary(data, "object", "unlimited_quota", "total_granted", "total_used", "total_available")

	if unlimited {
		snapshot.Unlimited = lo.ToPtr(true)
		snapshot.Available = lo.ToPtr(true)
		if usedOK {
			appendProbeBalanceAmount(&snapshot, usedUSD, "USD", objects.BalanceAmountKindUsed, "used", nil)
		}
		return snapshot
	}

	if !availOK && grantedOK && usedOK {
		availQ = max(0.0, grantedQ-usedQ)
	}
	remUSD := round2(max(0.0, availQ) / newAPIQuotaPerUSD)
	appendProbeBalanceAmount(&snapshot, remUSD, "USD", objects.BalanceAmountKindRemaining, "remaining", nil)
	snapshot.PrimaryBalance = &snapshot.Components[len(snapshot.Components)-1]
	snapshot.Remaining = lo.ToPtr(remUSD)
	if grantedOK && grantedQ > 0 {
		grantedUSD := round2(grantedQ / newAPIQuotaPerUSD)
		appendProbeBalanceAmount(&snapshot, grantedUSD, "USD", objects.BalanceAmountKindLimit, "granted", nil)
		snapshot.Limit = lo.ToPtr(grantedUSD)
		if usedOK {
			ratio := max(0.0, min(1.0, usedUSD/grantedUSD))
			snapshot.UsageRatio = lo.ToPtr(ratio)
		}
	}
	if usedOK {
		appendProbeBalanceAmount(&snapshot, usedUSD, "USD", objects.BalanceAmountKindUsed, "used", nil)
	}
	snapshot.Available = lo.ToPtr(remUSD > 0)
	return snapshot
}

func parseOpenAIBillingSnapshot(subBody []byte, usageBody []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	subDoc := decodeBalanceProbeJSON(subBody)
	snapshot := baseBalanceSnapshot(statusCode)
	if len(subDoc) == 0 {
		snapshot.Success = false
		return snapshot
	}
	hardLimit, ok := amountFromAny(firstNonNil(valueAt(subDoc, "hard_limit_usd"), valueAt(subDoc, "system_hard_limit_usd")))
	if !ok {
		snapshot.Success = false
		return snapshot
	}

	usageDoc := decodeBalanceProbeJSON(usageBody)
	usedCents, usedOK := amountFromAny(valueAt(usageDoc, "total_usage"))

	if hardLimit >= openAIBillingUnlimitedSent || hardLimit <= 0 {
		usedUSD := round2(max(0.0, usedCents) / 100.0)
		snapshot.Unlimited = lo.ToPtr(true)
		snapshot.Available = lo.ToPtr(true)
		if usedOK {
			snapshot.Used = lo.ToPtr(usedUSD)
			appendProbeBalanceAmount(&snapshot, usedUSD, "USD", objects.BalanceAmountKindUsed, "used", nil)
		}
		snapshot.RawSummary = map[string]any{
			"hard_limit_usd": hardLimit,
			"total_usage":    usedCents,
		}
		return snapshot
	}

	if !usedOK {
		// Never treat a 200 /usage response with missing or non-numeric total_usage
		// as $0 usage and overwrite the last valid balance.
		snapshot.Success = false
		return snapshot
	}

	usedUSD := round2(max(0.0, usedCents) / 100.0)
	snapshot.Used = lo.ToPtr(usedUSD)
	snapshot.RawSummary = map[string]any{
		"hard_limit_usd": hardLimit,
		"total_usage":    usedCents,
	}

	limitUSD := round2(hardLimit)
	remUSD := round2(max(0.0, hardLimit-usedUSD))
	ratio := max(0.0, min(1.0, usedUSD/limitUSD))

	appendProbeBalanceAmount(&snapshot, remUSD, "USD", objects.BalanceAmountKindRemaining, "remaining", nil)
	snapshot.PrimaryBalance = &snapshot.Components[len(snapshot.Components)-1]
	appendProbeBalanceAmount(&snapshot, limitUSD, "USD", objects.BalanceAmountKindLimit, "hard limit", nil)
	appendProbeBalanceAmount(&snapshot, usedUSD, "USD", objects.BalanceAmountKindUsed, "used", nil)
	snapshot.Limit = lo.ToPtr(limitUSD)
	snapshot.Remaining = lo.ToPtr(remUSD)
	snapshot.UsageRatio = lo.ToPtr(ratio)
	snapshot.Available = lo.ToPtr(remUSD > 0)
	return snapshot
}

func parseSub2UsageSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	payload := decodeBalanceProbeJSON(body)
	snapshot := baseBalanceSnapshot(statusCode)
	if len(payload) == 0 {
		snapshot.Success = false
		return snapshot
	}
	data := mapAt(payload, "data")
	if len(data) == 0 {
		data = payload
	}

	quotaObj := mapAt(data, "quota")
	subObj := mapAt(data, "subscription")
	rawRateLimits := arrayAt(data, "rate_limits")

	currency := firstString(
		stringAt(quotaObj, "unit"),
		stringAt(data, "unit"),
		stringAt(data, "currency"),
		stringAt(payload, "currency"),
		"USD",
	)
	unlimited := false
	if u := boolFromAny(firstNonNil(valueAt(data, "unlimited"), valueAt(data, "unlimited_quota"))); u != nil {
		unlimited = *u
	}

	remaining, remOK := amountFromAny(firstNonNil(
		valueAt(quotaObj, "remaining"),
		valueAt(data, "remaining"),
		valueAt(data, "balance"),
		valueAt(data, "quota_remaining"),
		valueAt(data, "available"),
	))
	used, usedOK := amountFromAny(firstNonNil(
		valueAt(quotaObj, "used"),
		valueAt(data, "used"),
		valueAt(data, "total_usage"),
	))
	limit, limitOK := amountFromAny(firstNonNil(
		valueAt(quotaObj, "limit"),
		valueAt(data, "limit"),
		valueAt(data, "total_quota"),
		valueAt(data, "hard_limit_usd"),
	))
	// Avoid reading nested object "quota" as a numeric limit when quota is a map.
	if !limitOK && len(quotaObj) == 0 {
		limit, limitOK = amountFromAny(valueAt(data, "quota"))
	}

	var earliestReset *time.Time
	recordReset := func(t *time.Time) {
		if t == nil {
			return
		}
		if earliestReset == nil || t.Before(*earliestReset) {
			cp := *t
			earliestReset = &cp
		}
	}

	// 1. Parse Sub2 quota_limited rate_limits[] (5h / 1d / 7d windows with independent resets).
	for i, item := range rawRateLimits {
		rm, ok := item.(map[string]any)
		if !ok || len(rm) == 0 {
			continue
		}
		winKey := firstString(stringAt(rm, "window"), stringAt(rm, "label"), "window_"+strconv.Itoa(i+1))
		wLimit, wLimitOK := amountFromAny(valueAt(rm, "limit"))
		wUsed, wUsedOK := amountFromAny(valueAt(rm, "used"))
		wRem, wRemOK := amountFromAny(valueAt(rm, "remaining"))
		if !wLimitOK && !wUsedOK && !wRemOK {
			continue
		}
		if !wRemOK && wLimitOK && wUsedOK {
			wRem = max(0.0, wLimit-wUsed)
			wRemOK = true
		}
		resetAt := parseBalanceProbeTime(firstNonNil(valueAt(rm, "reset_at"), valueAt(rm, "resetAt")))
		recordReset(resetAt)
		w := objects.ChannelQuotaWindow{
			Key:     canonicalQuotaWindowKey(winKey, i),
			Label:   winKey,
			Unit:    currency,
			ResetAt: resetAt,
		}
		if wUsedOK {
			u := round2(max(0.0, wUsed))
			w.Used = &u
		}
		if wLimitOK && wLimit > 0 {
			l := round2(wLimit)
			w.Limit = &l
			if wUsedOK {
				r := max(0.0, min(1.0, wUsed/wLimit))
				w.UsageRatio = &r
			}
		}
		if wRemOK {
			rem := round2(max(0.0, wRem))
			w.Remaining = &rem
		}
		snapshot.Windows = append(snapshot.Windows, w)
	}

	// 2. Parse Sub2 unrestricted subscription limits (daily / weekly / monthly USD windows).
	if len(subObj) > 0 {
		subExpires := parseBalanceProbeTime(firstNonNil(valueAt(subObj, "expires_at"), valueAt(subObj, "expiresAt")))
		recordReset(subExpires)
		addSubWindow := func(key, label, usedField, limitField string) {
			lim, limOK := amountFromAny(valueAt(subObj, limitField))
			if !limOK || lim <= 0 {
				return
			}
			u, _ := amountFromAny(valueAt(subObj, usedField))
			rem := round2(max(0.0, lim-u))
			ratio := max(0.0, min(1.0, u/lim))
			snapshot.Windows = append(snapshot.Windows, objects.ChannelQuotaWindow{
				Key:        key,
				Label:      label,
				Used:       lo.ToPtr(round2(max(0.0, u))),
				Limit:      lo.ToPtr(round2(lim)),
				Remaining:  lo.ToPtr(rem),
				UsageRatio: lo.ToPtr(ratio),
				Unit:       "USD",
				// Keep subscription expiry distinct from rolling window reset time.
				ResetAt: nil,
			})
		}
		addSubWindow("1d", "Daily", "daily_usage_usd", "daily_limit_usd")
		addSubWindow("7d", "Weekly", "weekly_usage_usd", "weekly_limit_usd")
		addSubWindow("30d", "Monthly", "monthly_usage_usd", "monthly_limit_usd")
	}

	if !unlimited && !remOK && !usedOK && !limitOK && len(snapshot.Windows) == 0 {
		snapshot.Success = false
		return snapshot
	}

	snapshot.PlanName = firstString(stringAt(data, "planName"), stringAt(data, "plan_name"), stringAt(data, "plan"), stringAt(data, "group"))
	snapshot.AccountStatus = stringAt(data, "status")
	topReset := parseBalanceProbeTime(firstNonNil(valueAt(data, "reset_at"), valueAt(data, "resetAt"), valueAt(data, "expires_at")))
	recordReset(topReset)
	snapshot.ResetAt = earliestReset
	snapshot.RawSummary = safeSummary(data, "mode", "status", "isValid", "planName", "currency", "unit", "unlimited", "remaining", "used", "limit", "balance")

	isValid := boolFromAny(valueAt(data, "isValid"))

	if unlimited && len(snapshot.Windows) == 0 {
		snapshot.Unlimited = lo.ToPtr(true)
		avail := true
		if isValid != nil && !*isValid {
			avail = false
		}
		snapshot.Available = &avail
		if usedOK {
			u := round2(max(0.0, used))
			snapshot.Used = &u
			appendProbeBalanceAmount(&snapshot, u, currency, objects.BalanceAmountKindUsed, "used", snapshot.ResetAt)
		}
		return snapshot
	}

	if !remOK && limitOK && usedOK {
		remaining = max(0.0, limit-used)
		remOK = true
	}
	if remOK {
		rem := round2(max(0.0, remaining))
		appendProbeBalanceAmount(&snapshot, rem, currency, objects.BalanceAmountKindRemaining, "remaining", snapshot.ResetAt)
		snapshot.PrimaryBalance = &snapshot.Components[len(snapshot.Components)-1]
		snapshot.Remaining = &rem
	}
	if limitOK && limit > 0 {
		lim := round2(limit)
		appendProbeBalanceAmount(&snapshot, lim, currency, objects.BalanceAmountKindLimit, "limit", snapshot.ResetAt)
		snapshot.Limit = &lim
		if usedOK {
			ratio := max(0.0, min(1.0, used/limit))
			snapshot.UsageRatio = &ratio
		}
	}
	if usedOK {
		u := round2(max(0.0, used))
		appendProbeBalanceAmount(&snapshot, u, currency, objects.BalanceAmountKindUsed, "used", snapshot.ResetAt)
		snapshot.Used = &u
	}
	if snapshot.UsageRatio == nil {
		if worst, ok := snapshot.WorstUsageRatioAt(time.Now()); ok {
			snapshot.UsageRatio = &worst
		}
	}

	now := time.Now()
	exhausted := snapshot.ExhaustedAt(now)
	if isValid != nil && !*isValid {
		exhausted = true
	}
	if remOK && remaining <= 0 && !unlimited {
		exhausted = true
	}
	snapshot.Available = lo.ToPtr(!exhausted)
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
	switch v := value.(type) {
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return nil
		}
		if num, err := strconv.ParseFloat(text, 64); err == nil && num > 0 {
			return epochToTimePtr(num)
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
			parsed, err := time.Parse(layout, text)
			if err == nil {
				return &parsed
			}
		}
		return nil
	case int:
		return epochToTimePtr(float64(v))
	case int64:
		return epochToTimePtr(float64(v))
	case float64:
		return epochToTimePtr(v)
	default:
		if num, ok := amountFromAny(value); ok {
			return epochToTimePtr(num)
		}
		return nil
	}
}

func epochToTimePtr(num float64) *time.Time {
	if math.IsNaN(num) || math.IsInf(num, 0) || num <= 0 {
		return nil
	}
	if num > 1e11 {
		num /= 1000.0
	}
	sec := int64(num)
	nsec := int64((num - float64(sec)) * 1e9)
	t := time.Unix(sec, nsec).UTC()
	return &t
}

func round1(v float64) float64 {
	return math.Round(v*10) / 10
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func boolPtr(value bool) *bool {
	return &value
}

func isTavilyBalanceProbeBaseURL(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	return err == nil && parsed != nil && strings.EqualFold(parsed.Hostname(), "api.tavily.com")
}
