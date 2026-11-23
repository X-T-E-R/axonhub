package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	providerquota "github.com/looplj/axonhub/internal/server/biz/provider_quota"
	"github.com/looplj/axonhub/llm/httpclient"
)

type balanceProbePresetSpec struct {
	ID            objects.ChannelBalanceProbePreset
	Provider      channel.Type
	Name          string
	Experimental  bool
	RollingWindow bool
	RequiresAdmin bool
	SharedDefault bool
	HTTP          objects.ChannelKeyHealthCheckHTTPRule
	Parse         func([]byte, int) objects.ChannelKeyBalanceSnapshot
	Execute       func(ctx context.Context, svc *ChannelService, ch *ent.Channel, authKey string, rule objects.ChannelKeyHealthCheckHTTPRule) (*httpclient.Response, ChannelKeyHealthCheckResult, objects.ChannelKeyBalanceSnapshot, bool)
}

var channelBalanceProbePresets = []balanceProbePresetSpec{
	{
		ID:       objects.ChannelBalanceProbePresetDeepSeek,
		Provider: channel.TypeDeepseek,
		Name:     "DeepSeek balance",
		HTTP: objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
			URL:              "https://api.deepseek.com/user/balance",
			ExpectedStatuses: []int{http.StatusOK},
		},
		Parse: parseDeepSeekBalanceSnapshot,
	},
	{
		ID:       objects.ChannelBalanceProbePresetSiliconFlow,
		Provider: channel.TypeSiliconflow,
		Name:     "SiliconFlow user info",
		HTTP: objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
			Path:             "/user/info",
			ExpectedStatuses: []int{http.StatusOK},
		},
		Parse: parseSiliconFlowBalanceSnapshot,
	},
	{
		ID:       objects.ChannelBalanceProbePresetMoonshot,
		Provider: channel.TypeMoonshot,
		Name:     "Moonshot/Kimi balance",
		HTTP: objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
			Path:             "/users/me/balance",
			ExpectedStatuses: []int{http.StatusOK},
		},
		Parse: parseMoonshotBalanceSnapshot,
	},
	{
		ID:       objects.ChannelBalanceProbePresetOpenRouter,
		Provider: channel.TypeOpenrouter,
		Name:     "OpenRouter credits",
		HTTP: objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL,
			Path:             "/credits",
			ExpectedStatuses: []int{http.StatusOK},
		},
		Parse: parseOpenRouterBalanceSnapshot,
	},
	{
		ID:       objects.ChannelBalanceProbePresetNanoGPT,
		Provider: channel.TypeNanogpt,
		Name:     "NanoGPT check balance",
		HTTP: objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodPost,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
			URL:              "https://nano-gpt.com/api/check-balance",
			ExpectedStatuses: []int{http.StatusOK},
			KeyInjection: &objects.ChannelKeyHealthCheckKeyInjection{
				Location:   objects.ChannelKeyHealthCheckKeyInjectionHeader,
				HeaderName: "x-api-key",
			},
		},
		Parse: parseNanoGPTBalanceSnapshot,
	},
	{
		ID:   objects.ChannelBalanceProbePresetTavily,
		Name: "Tavily usage",
		HTTP: objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
			URL:              "https://api.tavily.com/usage",
			ExpectedStatuses: []int{http.StatusOK},
		},
		Parse: parseTavilyUsageBalanceSnapshot,
	},
	{
		ID:            objects.ChannelBalanceProbePresetKimiCode,
		Provider:      channel.TypeMoonshotCoding,
		Name:          "Kimi Code usages",
		RollingWindow: true,
		HTTP: objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
			URL:              providerquota.BuildKimiCodeUsageURL(""),
			ExpectedStatuses: []int{http.StatusOK},
		},
		Parse: parseKimiCodeUsageBalanceSnapshot,
	},
	{
		ID:            objects.ChannelBalanceProbePresetExa,
		Name:          "Exa team credits",
		RequiresAdmin: true,
		SharedDefault: true,
		HTTP: objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
			URL:              exaTeamCreditsEndpointURL,
			ExpectedStatuses: []int{http.StatusOK},
			KeyInjection: &objects.ChannelKeyHealthCheckKeyInjection{
				Location:   objects.ChannelKeyHealthCheckKeyInjectionHeader,
				HeaderName: "x-api-key",
			},
		},
		Parse: parseExaTeamCreditsBalanceSnapshot,
	},
	{
		ID:   objects.ChannelBalanceProbePresetFirecrawl,
		Name: "Firecrawl credits",
		HTTP: objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
			URL:              "https://api.firecrawl.dev/v2/team/credit-usage",
			ExpectedStatuses: []int{http.StatusOK},
		},
		Parse: parseFirecrawlCreditsBalanceSnapshot,
	},
	{
		ID:            objects.ChannelBalanceProbePresetCommandCode,
		Name:          "CommandCode billing",
		RollingWindow: true,
		HTTP: objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
			URL:              "https://api.commandcode.ai/alpha/billing/credits",
			Headers:          []objects.HeaderEntry{{Key: "User-Agent", Value: commandCodeUserAgent}},
			ExpectedStatuses: []int{http.StatusOK},
		},
		Parse: func(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
			return parseCommandCodeBillingSnapshot(body, nil, statusCode)
		},
	},
	{
		ID:            objects.ChannelBalanceProbePresetStepPlan,
		Name:          "StepFun plan usage",
		RollingWindow: true,
		HTTP: objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
			URL:              "https://api.stepfun.com/step_plan/v1/usage",
			ExpectedStatuses: []int{http.StatusOK},
		},
		Parse: parseStepPlanUsageSnapshot,
	},
	{
		ID:   objects.ChannelBalanceProbePresetNewAPI,
		Name: "NewAPI token usage",
		HTTP: objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
			URL:              "https://api.n.icu/api/usage/token/",
			ExpectedStatuses: []int{http.StatusOK},
		},
		Parse: parseNewAPITokenUsageSnapshot,
	},
	{
		ID:   objects.ChannelBalanceProbePresetOpenAIBilling,
		Name: "OpenAI billing usage",
		HTTP: objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
			URL:              "https://api.openai-next.com/v1/dashboard/billing/subscription",
			ExpectedStatuses: []int{http.StatusOK},
		},
		Parse: func(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
			return parseOpenAIBillingSnapshot(body, nil, statusCode)
		},
	},
	{
		ID:   objects.ChannelBalanceProbePresetSub2,
		Name: "Sub2API usage",
		HTTP: objects.ChannelKeyHealthCheckHTTPRule{
			Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
			URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
			URL:              "https://example.invalid/v1/usage",
			ExpectedStatuses: []int{http.StatusOK},
		},
		Parse: parseSub2UsageSnapshot,
	},
}

func init() {
	for i := range channelBalanceProbePresets {
		switch channelBalanceProbePresets[i].ID {
		case objects.ChannelBalanceProbePresetCommandCode:
			channelBalanceProbePresets[i].Execute = executeCommandCodeBillingProbe
		case objects.ChannelBalanceProbePresetOpenAIBilling:
			channelBalanceProbePresets[i].Execute = executeOpenAIBillingProbe
		}
	}
}

func channelBalanceProbePresetByID(id objects.ChannelBalanceProbePreset) (balanceProbePresetSpec, bool) {
	for _, spec := range channelBalanceProbePresets {
		if spec.ID == id {
			if spec.ID == objects.ChannelBalanceProbePresetExa {
				spec.HTTP.URL = exaTeamCreditsEndpointURL
			}
			return spec, true
		}
	}

	return balanceProbePresetSpec{}, false
}

func channelBalanceProbePresetForChannelType(channelType channel.Type) (balanceProbePresetSpec, bool) {
	switch channelType {
	case channel.TypeDeepseek, channel.TypeDeepseekAnthropic:
		return channelBalanceProbePresetByID(objects.ChannelBalanceProbePresetDeepSeek)
	case channel.TypeSiliconflow:
		return channelBalanceProbePresetByID(objects.ChannelBalanceProbePresetSiliconFlow)
	case channel.TypeMoonshot, channel.TypeMoonshotAnthropic:
		return channelBalanceProbePresetByID(objects.ChannelBalanceProbePresetMoonshot)
	case channel.TypeMoonshotCoding:
		return channelBalanceProbePresetByID(objects.ChannelBalanceProbePresetKimiCode)
	case channel.TypeOpenrouter:
		return channelBalanceProbePresetByID(objects.ChannelBalanceProbePresetOpenRouter)
	case channel.TypeNanogpt, channel.TypeNanogptResponses:
		return channelBalanceProbePresetByID(objects.ChannelBalanceProbePresetNanoGPT)
	default:
		return balanceProbePresetSpec{}, false
	}
}

func finalizeBalanceProbePresetURL(spec balanceProbePresetSpec, ch *ent.Channel) balanceProbePresetSpec {
	if ch == nil {
		return spec
	}
	switch spec.ID {
	case objects.ChannelBalanceProbePresetKimiCode:
		spec.HTTP.URL = providerquota.BuildKimiCodeUsageURL(ch.BaseURL)
	case objects.ChannelBalanceProbePresetExa:
		// Exa Admin Probe URL is strictly locked to the official HTTPS endpoint.
		spec.HTTP.URLMode = objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL
		spec.HTTP.URL = exaTeamCreditsEndpointURL
	case objects.ChannelBalanceProbePresetCommandCode:
		spec.HTTP.URL = originFromBaseURL(ch.BaseURL, "https://api.commandcode.ai") + "/alpha/billing/credits"
	case objects.ChannelBalanceProbePresetStepPlan:
		spec.HTTP.URL = originFromBaseURL(ch.BaseURL, "https://api.stepfun.com") + "/step_plan/v1/usage"
	case objects.ChannelBalanceProbePresetNewAPI:
		spec.HTTP.URL = originFromBaseURL(ch.BaseURL, "https://api.n.icu") + "/api/usage/token/"
	case objects.ChannelBalanceProbePresetOpenAIBilling:
		spec.HTTP.URL = originFromBaseURL(ch.BaseURL, "https://api.openai-next.com") + "/v1/dashboard/billing/subscription"
	case objects.ChannelBalanceProbePresetSub2:
		if origin := originFromBaseURL(ch.BaseURL, ""); origin != "" {
			spec.HTTP.URL = origin + "/v1/usage"
		}
	case objects.ChannelBalanceProbePresetOpenRouter:
		if parsed, err := url.Parse(strings.TrimSpace(ch.BaseURL)); err == nil && parsed != nil && parsed.Host != "" {
			path := strings.TrimRight(parsed.Path, "/")
			if !strings.HasSuffix(path, "/v1") && !strings.HasSuffix(path, "/api/v1") {
				spec.HTTP.URLMode = objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL
				spec.HTTP.URL = originFromBaseURL(ch.BaseURL, "https://openrouter.ai") + "/api/v1/credits"
			}
		}
	}
	return spec
}

func channelBalanceProbeSpecForChannel(ch *ent.Channel) (balanceProbePresetSpec, *objects.ChannelBalanceProbe, bool) {
	if ch == nil {
		return balanceProbePresetSpec{}, nil, false
	}

	var probe *objects.ChannelBalanceProbe
	if ch.Settings != nil {
		probe = ch.Settings.BalanceProbe
	}
	customSpec := func() (balanceProbePresetSpec, *objects.ChannelBalanceProbe, bool) {
		if probe == nil || probe.HTTP == nil {
			return balanceProbePresetSpec{}, probe, false
		}

		return balanceProbePresetSpec{
			ID:       objects.ChannelBalanceProbePresetCustom,
			Provider: ch.Type,
			Name:     "Custom balance probe",
			HTTP:     *probe.HTTP,
			Parse:    parseGenericBalanceSnapshot,
		}, probe, true
	}
	// Honor explicit preset first so operator choices (e.g. legacy moonshot_balance)
	// are respected until explicitly changed.
	if probe != nil && probe.Preset != "" {
		if probe.Preset == objects.ChannelBalanceProbePresetCustom {
			return customSpec()
		}
		spec, ok := channelBalanceProbePresetByID(probe.Preset)
		if !ok || (spec.Experimental && !probe.Experimental) {
			return balanceProbePresetSpec{}, probe, false
		}
		return finalizeBalanceProbePresetURL(spec, ch), probe, true
	}

	if presetID, ok := channelBalanceProbePresetForBaseURL(ch); ok {
		if spec, found := channelBalanceProbePresetByID(presetID); found {
			return finalizeBalanceProbePresetURL(spec, ch), probe, true
		}
	}

	spec, ok := channelBalanceProbePresetForChannelType(ch.Type)
	if ok {
		return finalizeBalanceProbePresetURL(spec, ch), probe, true
	}

	return customSpec()
}

func isChannelSharedWallet(ch *ent.Channel) bool {
	if ch == nil {
		return false
	}
	if ch.Settings != nil && ch.Settings.BalanceProbe != nil && ch.Settings.BalanceProbe.SharedWallet {
		return true
	}
	spec, _, ok := channelBalanceProbeSpecForChannel(ch)
	return ok && spec.SharedDefault
}

func canonicalWalletIdentity(ch *ent.Channel, keyID string, sharedWallet bool) (scope string, walletID string) {
	ownerID := 0
	if ch != nil {
		ownerID = credentialOwnerID(ch)
	}
	if sharedWallet {
		return "shared_wallet", "wallet:ch:" + strconv.Itoa(ownerID)
	}
	return "credential", "cred:ch:" + strconv.Itoa(ownerID) + ":" + keyID
}

func executeCommandCodeBillingProbe(
	ctx context.Context,
	svc *ChannelService,
	ch *ent.Channel,
	authKey string,
	rule objects.ChannelKeyHealthCheckHTTPRule,
) (*httpclient.Response, ChannelKeyHealthCheckResult, objects.ChannelKeyBalanceSnapshot, bool) {
	resp, result := svc.executeChannelKeyProbeHTTP(ctx, ch, authKey, rule, false)
	if resp == nil || !result.Success {
		return resp, result, objects.ChannelKeyBalanceSnapshot{}, false
	}
	subRule := rule
	subRule.URL = strings.Replace(rule.URL, "/alpha/billing/credits", "/alpha/billing/subscriptions", 1)
	var subsBody []byte
	if subResp, subRes := svc.executeChannelKeyProbeHTTP(ctx, ch, authKey, subRule, false); subResp != nil && subRes.Success {
		subsBody = subResp.Body
	}
	snapshot := parseCommandCodeBillingSnapshot(resp.Body, subsBody, resp.StatusCode)
	return resp, result, snapshot, true
}

func executeOpenAIBillingProbe(
	ctx context.Context,
	svc *ChannelService,
	ch *ent.Channel,
	authKey string,
	rule objects.ChannelKeyHealthCheckHTTPRule,
) (*httpclient.Response, ChannelKeyHealthCheckResult, objects.ChannelKeyBalanceSnapshot, bool) {
	resp, result := svc.executeChannelKeyProbeHTTP(ctx, ch, authKey, rule, false)
	if resp == nil || !result.Success {
		return resp, result, objects.ChannelKeyBalanceSnapshot{}, false
	}
	// If subscription already reports the infinite-quota sentinel (>=1e7), skip /usage.
	prelim := parseOpenAIBillingSnapshot(resp.Body, nil, resp.StatusCode)
	if prelim.Success && prelim.Unlimited != nil && *prelim.Unlimited {
		return resp, result, prelim, true
	}
	usageRule := rule
	now := time.Now().UTC()
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
	end := now.AddDate(0, 0, 1).Format("2006-01-02")
	usageRule.URL = strings.Replace(rule.URL, "/v1/dashboard/billing/subscription", "/v1/dashboard/billing/usage?start_date="+start+"&end_date="+end, 1)
	usageResp, usageRes := svc.executeChannelKeyProbeHTTP(ctx, ch, authKey, usageRule, false)
	if usageResp == nil || !usageRes.Success {
		// Never treat a failed /usage request as $0 usage and overwrite a valid snapshot.
		usageRes.Success = false
		usageRes.ObservationValid = false
		if usageRes.Reason == "" {
			usageRes.Reason = "openai billing usage request failed"
		}
		return usageResp, usageRes, objects.ChannelKeyBalanceSnapshot{}, false
	}
	snapshot := parseOpenAIBillingSnapshot(resp.Body, usageResp.Body, resp.StatusCode)
	return resp, result, snapshot, true
}

func (svc *ChannelService) runChannelKeyBalanceProbe(ctx context.Context, ch *ent.Channel, key string, trigger objects.ChannelKeyHealthCheckTrigger) ChannelKeyHealthCheckResult {
	spec, probe, ok := channelBalanceProbeSpecForChannel(ch)
	if !ok {
		return ChannelKeyHealthCheckResult{
			Success:          false,
			Unsupported:      true,
			ObservationValid: false,
			ProbeStatus:      "unsupported",
			Reason:           "channel does not have a supported balance probe",
			Source:           balanceProbeEventSource(trigger, false),
		}
	}

	rule := spec.HTTP
	usingProbeSecret := false
	authKey := key

	if spec.ID == objects.ChannelBalanceProbePresetExa {
		// Exa security boundary: locked official HTTPS admin endpoint, no HTTP override,
		// no redirects, and strictly requires write-only ProbeAPIKey (no inference key fallback).
		rule = spec.HTTP
		rule.URLMode = objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL
		rule.URL = exaTeamCreditsEndpointURL
		if probe != nil && probe.TimeoutMs > 0 {
			rule.TimeoutMs = probe.TimeoutMs
		}
		if probe == nil || !probe.HasProbeAPIKey() {
			return ChannelKeyHealthCheckResult{
				Success:          false,
				ObservationValid: false,
				ProbeStatus:      "missing_probe_key",
				Rule:             spec.Name,
				Reason:           "exa team credits probe requires a configured admin probeAPIKey",
				Source:           balanceProbeEventSource(trigger, false),
			}
		}
		decrypted, err := DecryptProbeAPIKey(ctx, svc.SystemService, objects.ChannelBalanceProbePresetExa, probe.ProbeAPIKeyCiphertext)
		if err != nil || strings.TrimSpace(decrypted) == "" {
			return ChannelKeyHealthCheckResult{
				Success:          false,
				ObservationValid: false,
				ProbeStatus:      "error",
				Rule:             spec.Name,
				Reason:           "failed to decrypt admin probeAPIKey",
				Source:           balanceProbeEventSource(trigger, false),
			}
		}
		authKey = decrypted
		usingProbeSecret = true
	} else if probe != nil {
		if probe.HTTP != nil {
			rule = *probe.HTTP
		}
		if probe.TimeoutMs > 0 {
			rule.TimeoutMs = probe.TimeoutMs
		}
		if probe.HasProbeAPIKey() {
			decrypted, err := DecryptProbeAPIKey(ctx, svc.SystemService, probe.Preset, probe.ProbeAPIKeyCiphertext)
			if err != nil || strings.TrimSpace(decrypted) == "" {
				return ChannelKeyHealthCheckResult{
					Success:          false,
					ObservationValid: false,
					ProbeStatus:      "error",
					Rule:             spec.Name,
					Reason:           "failed to decrypt probeAPIKey",
					Source:           balanceProbeEventSource(trigger, false),
				}
			}
			authKey = decrypted
			usingProbeSecret = true
		}
	}

	var (
		resp          *httpclient.Response
		result        ChannelKeyHealthCheckResult
		snapshot      objects.ChannelKeyBalanceSnapshot
		parsedInHook  bool
		scriptEnabled = !usingProbeSecret && spec.ID != objects.ChannelBalanceProbePresetExa && probe != nil && probe.Script != nil && (probe.Script.Mode == objects.ChannelBalanceProbeScriptModeExpr || probe.Script.Mode == objects.ChannelBalanceProbeScriptModeCommand)
	)
	if spec.Execute != nil && !scriptEnabled {
		resp, result, snapshot, parsedInHook = spec.Execute(ctx, svc, ch, authKey, rule)
	} else {
		resp, result = svc.executeChannelKeyProbeHTTP(ctx, ch, authKey, rule, usingProbeSecret)
	}
	result.Rule = spec.Name
	result.Source = balanceProbeEventSource(trigger, false)
	if resp == nil || !result.Success {
		result.ObservationValid = false
		result.BalanceSnapshot = nil
		if result.ProbeStatus == "" {
			if result.StatusCode == http.StatusNotFound || result.StatusCode == http.StatusNotImplemented {
				result.ProbeStatus = "unsupported"
			} else {
				result.ProbeStatus = "error"
			}
		}
		return result
	}

	if scriptEnabled {
		parsed, err := runChannelBalanceProbeScript(ctx, probe, resp, ch.ID, authKey)
		if err != nil {
			result.Success = false
			result.ObservationValid = false
			result.ProbeStatus = "error"
			result.StatusCode = resp.StatusCode
			result.Reason = "balance probe script failed"
			result.BalanceSnapshot = nil
			return result
		}
		snapshot = parsed
	} else if !parsedInHook {
		snapshot = spec.Parse(resp.Body, resp.StatusCode)
	}

	if !snapshot.Success {
		result.Success = false
		result.ObservationValid = false
		result.ProbeStatus = "error"
		result.StatusCode = resp.StatusCode
		result.BalanceSnapshot = nil
		if result.Reason == "" || result.Reason == "ok" {
			result.Reason = "balance probe response schema invalid"
		}
		return result
	}

	sharedWallet := isChannelSharedWallet(ch)
	keyID := objects.ChannelAPIKeyFingerprint(key)
	walletScope, walletID := canonicalWalletIdentity(ch, keyID, sharedWallet)

	snapshot.Provider = string(spec.ID)
	snapshot.Source = "balance_probe"
	snapshot.CheckedAt = time.Now().UTC()
	snapshot.StatusCode = resp.StatusCode
	snapshot.WalletScope = walletScope
	snapshot.WalletID = walletID

	preferredCurrency := preferredChannelBalanceCurrency(probe)
	if snapshot.PrimaryBalance == nil || preferredCurrency != "" {
		selectChannelBalancePrimary(&snapshot, preferredCurrency)
	}
	var manualLimit *float64
	if ch.Settings != nil && !sharedWallet {
		manualLimit = ch.Settings.CredentialPolicy(keyID).ManualQuotaLimit
	}
	normalizeChannelBalanceCapacity(&snapshot, manualLimit)

	result.StatusCode = resp.StatusCode
	result.BalanceSnapshot = &snapshot
	result.Available = snapshot.Available
	if snapshot.PrimaryBalance != nil {
		result.Balance = snapshot.PrimaryBalance.Amount
		result.Currency = snapshot.PrimaryBalance.Currency
	}
	// Observation validity is strictly separate from remaining capacity: a valid
	// response reporting zero balance / available=false (e.g. Tavily 1001/1000) is
	// a valid observation, NOT a probe failure!
	result.ObservationValid = true
	result.ProbeStatus = "ok"
	result.Success = true
	result.Reason = "ok"
	if snapshot.Available != nil && !*snapshot.Available {
		result.Reason = "quota exhausted"
	}
	result.Source = balanceProbeEventSource(trigger, true)

	return result
}

func (svc *ChannelService) executeChannelKeyProbeHTTP(
	ctx context.Context,
	ch *ent.Channel,
	key string,
	rule objects.ChannelKeyHealthCheckHTTPRule,
	requireHTTPS bool,
) (*httpclient.Response, ChannelKeyHealthCheckResult) {
	targetURL, err := buildBalanceProbeHTTPURL(ctx, ch.BaseURL, rule)
	if err != nil {
		return nil, ChannelKeyHealthCheckResult{Success: false, ProbeStatus: "error", Reason: err.Error()}
	}
	if requireHTTPS {
		parsed, parseErr := url.Parse(targetURL)
		if parseErr != nil || parsed == nil {
			return nil, ChannelKeyHealthCheckResult{Success: false, ProbeStatus: "error", Reason: "invalid probe url"}
		}
		if !strings.EqualFold(parsed.Scheme, "https") {
			return nil, ChannelKeyHealthCheckResult{Success: false, ProbeStatus: "error", Reason: "probeAPIKey requires an HTTPS endpoint"}
		}
	}
	// Enforce per-origin request rate budget at every actual outbound HTTP request
	// (including multi-endpoint presets like CommandCode and OpenAI Billing), then
	// recheck placement ownership before sending bytes on the wire.
	if parsedURL, parseErr := url.Parse(targetURL); parseErr == nil && parsedURL != nil && parsedURL.Host != "" {
		origin := strings.ToLower(parsedURL.Scheme + "://" + parsedURL.Host)
		if err := svc.quotaProbeCoord.waitOriginRateLimit(ctx, origin); err != nil {
			return nil, ChannelKeyHealthCheckResult{Success: false, ProbeStatus: "error", Reason: err.Error()}
		}
	}
	if ch != nil && ch.ID > 0 && !svc.node.Owns(ch.ID) {
		return nil, ChannelKeyHealthCheckResult{Success: false, ProbeStatus: "error", Reason: svc.ChannelOwnerError(ch.ID).Error()}
	}

	timeoutMs := rule.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = channelKeyHealthCheckHTTPDefaultMs
	}
	if timeoutMs > maxChannelKeyHealthCheckHTTPTimeoutMs {
		timeoutMs = maxChannelKeyHealthCheckHTTPTimeoutMs
	}

	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()

	headers := make(http.Header)
	for _, header := range rule.Headers {
		name := strings.TrimSpace(header.Key)
		if name == "" {
			continue
		}
		if strings.EqualFold(name, "Authorization") {
			continue
		}
		if !isValidHTTPHeaderName(name) {
			return nil, ChannelKeyHealthCheckResult{Success: false, ProbeStatus: "error", Reason: "header name is invalid"}
		}

		headers.Set(name, header.Value)
	}

	injection := rule.KeyInjection
	if injection == nil || injection.Location == "" || injection.Location == objects.ChannelKeyHealthCheckKeyInjectionAuthorizationBearer {
		headers.Set("Authorization", "Bearer "+key)
	} else if injection.Location == objects.ChannelKeyHealthCheckKeyInjectionHeader {
		headerName := strings.TrimSpace(injection.HeaderName)
		if !isValidHTTPHeaderName(headerName) {
			return nil, ChannelKeyHealthCheckResult{Success: false, ProbeStatus: "error", Reason: "header name is invalid"}
		}
		headers.Set(headerName, key)
	}

	method := string(rule.Method)
	if method == "" {
		method = http.MethodGet
	}

	httpReq := &httpclient.Request{
		Method:  method,
		URL:     targetURL,
		Headers: headers,
	}

	hc := svc.getHttpClient(ch.Settings)
	credentialID := objects.ChannelAPIKeyFingerprint(key)
	if ch.Credentials.IsOAuth() {
		credentialID = objects.OAuthDefaultCredentialID
	}
	if !requireHTTPS && !isChannelSharedWallet(ch) {
		isolated, _, egressErr := svc.resolveCredentialClient(requestCtx, ch, credentialID, hc)
		if egressErr != nil {
			return nil, ChannelKeyHealthCheckResult{Success: false, ProbeStatus: "error", Reason: "credential egress is blocked"}
		}
		if isolated != nil {
			hc = isolated
		}
	}
	hc = channelKeyHealthCheckNoRedirectClient(hc)

	resp, err := doChannelKeyHealthCheckHTTPRequest(requestCtx, hc, httpReq)
	if errors.Is(err, errChannelKeyHealthCheckResponseTooLarge) {
		return nil, ChannelKeyHealthCheckResult{Success: false, ProbeStatus: "error", Reason: "response body too large"}
	}
	if err != nil {
		return nil, ChannelKeyHealthCheckResult{Success: false, ProbeStatus: "error", Reason: "http request failed: " + err.Error()}
	}

	result := evaluateChannelKeyHealthCheckHTTPResponse(resp, rule.ExpectedStatuses)
	if resp != nil {
		result.StatusCode = resp.StatusCode
		if retryAfter := parseRetryAfterHeader(resp.Headers); retryAfter != nil {
			result.RetryAfter = retryAfter
		}
	}

	return resp, result
}

func parseRetryAfterHeader(headers http.Header) *time.Time {
	if headers == nil {
		return nil
	}
	raw := strings.TrimSpace(headers.Get("Retry-After"))
	if raw == "" {
		return nil
	}
	if secs, err := strconv.Atoi(raw); err == nil && secs > 0 {
		t := time.Now().UTC().Add(time.Duration(secs) * time.Second)
		return &t
	}
	if parsed, err := http.ParseTime(raw); err == nil {
		t := parsed.UTC()
		return &t
	}
	return nil
}

func balanceProbeEventSource(trigger objects.ChannelKeyHealthCheckTrigger, success bool) objects.FailurePolicyEventSource {
	if trigger == objects.ChannelKeyHealthCheckTriggerManual {
		if success {
			return objects.FailurePolicyEventSourceManualBalanceProbe
		}

		return objects.FailurePolicyEventSourceManualBalanceProbeFailure
	}
	if success {
		return objects.FailurePolicyEventSourceScheduledBalanceProbe
	}

	return objects.FailurePolicyEventSourceScheduledBalanceProbeFailure
}

func isBalanceProbeEventSource(source objects.FailurePolicyEventSource) bool {
	switch source {
	case objects.FailurePolicyEventSourceScheduledBalanceProbe,
		objects.FailurePolicyEventSourceScheduledBalanceProbeFailure,
		objects.FailurePolicyEventSourceManualBalanceProbe,
		objects.FailurePolicyEventSourceManualBalanceProbeFailure:
		return true
	default:
		return false
	}
}

func preferredChannelBalanceCurrency(probe *objects.ChannelBalanceProbe) string {
	if probe == nil {
		return ""
	}
	if probe.PrimarySelection == objects.ChannelBalancePrimarySelectionAutoHighest {
		return ""
	}

	return strings.TrimSpace(probe.PreferredCurrency)
}

func parseDeepSeekBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	payload := decodeBalanceProbeJSON(body)
	snapshot := baseBalanceSnapshot(statusCode)
	if len(payload) == 0 {
		snapshot.Success = false
		return snapshot
	}
	snapshot.Available = boolFromAny(valueAt(payload, "is_available"))
	snapshot.RawSummary = safeSummary(payload, "is_available")

	for _, item := range arrayAt(payload, "balance_infos") {
		info, _ := item.(map[string]any)
		if len(info) == 0 {
			continue
		}
		currency := stringAt(info, "currency")
		addBalanceAmount(&snapshot.Components, info, "total_balance", currency, objects.BalanceAmountKindTotal, "total")
		addBalanceAmount(&snapshot.Components, info, "granted_balance", currency, objects.BalanceAmountKindGranted, "granted")
		addBalanceAmount(&snapshot.Components, info, "topped_up_balance", currency, objects.BalanceAmountKindToppedUp, "topped up")
	}
	if snapshot.Available == nil && len(snapshot.Components) == 0 {
		snapshot.Success = false
	}

	return snapshot
}

func parseSiliconFlowBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	payload := decodeBalanceProbeJSON(body)
	if len(payload) == 0 {
		s := baseBalanceSnapshot(statusCode)
		s.Success = false
		return s
	}
	data := mapAt(payload, "data")
	if len(data) == 0 {
		data = payload
	}

	snapshot := baseBalanceSnapshot(statusCode)
	snapshot.Available = boolFromAny(firstNonNil(valueAt(payload, "status"), valueAt(data, "status")))
	if status := stringAt(data, "status"); status != "" {
		snapshot.AccountStatus = status
	}
	snapshot.AccountID = stringAt(data, "id")
	snapshot.RawSummary = safeSummary(data, "id", "status", "role")
	addBalanceAmount(&snapshot.Components, data, "balance", "CNY", objects.BalanceAmountKindAvailable, "balance")
	addBalanceAmount(&snapshot.Components, data, "chargeBalance", "CNY", objects.BalanceAmountKindCash, "charge balance")
	addBalanceAmount(&snapshot.Components, data, "totalBalance", "CNY", objects.BalanceAmountKindTotal, "total balance")
	if snapshot.Available == nil && len(snapshot.Components) == 0 {
		snapshot.Success = false
	}

	return snapshot
}

func parseMoonshotBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	payload := decodeBalanceProbeJSON(body)
	if len(payload) == 0 {
		s := baseBalanceSnapshot(statusCode)
		s.Success = false
		return s
	}
	data := mapAt(payload, "data")
	if len(data) == 0 {
		data = payload
	}
	currency := firstString(stringAt(data, "currency"), "CNY")

	snapshot := baseBalanceSnapshot(statusCode)
	snapshot.AccountID = firstString(stringAt(data, "id"), stringAt(data, "account_id"))
	snapshot.AccountStatus = firstString(stringAt(data, "status"), stringAt(data, "account_status"))
	snapshot.RawSummary = safeSummary(data, "id", "account_id", "status", "account_status", "currency")
	addBalanceAmount(&snapshot.Components, data, "available_balance", currency, objects.BalanceAmountKindAvailable, "available")
	addBalanceAmount(&snapshot.Components, data, "available", currency, objects.BalanceAmountKindAvailable, "available")
	addBalanceAmount(&snapshot.Components, data, "balance", currency, objects.BalanceAmountKindAvailable, "balance")
	addBalanceAmount(&snapshot.Components, data, "voucher_balance", currency, objects.BalanceAmountKindVoucher, "voucher")
	addBalanceAmount(&snapshot.Components, data, "voucher", currency, objects.BalanceAmountKindVoucher, "voucher")
	addBalanceAmount(&snapshot.Components, data, "cash_balance", currency, objects.BalanceAmountKindCash, "cash")
	addBalanceAmount(&snapshot.Components, data, "cash", currency, objects.BalanceAmountKindCash, "cash")
	if len(snapshot.Components) == 0 {
		snapshot.Success = false
	}

	return snapshot
}

func parseOpenRouterBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
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

	snapshot.RawSummary = safeSummary(data, "total_credits", "total_usage")
	total, totalOK := amountFromAny(valueAt(data, "total_credits"))
	used, usedOK := amountFromAny(valueAt(data, "total_usage"))
	if !totalOK && !usedOK {
		snapshot.Success = false
		return snapshot
	}
	if totalOK {
		snapshot.Components = append(snapshot.Components, objects.BalanceAmount{Amount: total, Currency: "USD", Kind: objects.BalanceAmountKindTotal, Label: "total credits"})
	}
	if usedOK {
		snapshot.Components = append(snapshot.Components, objects.BalanceAmount{Amount: max(0.0, used), Currency: "USD", Kind: objects.BalanceAmountKindUsed, Label: "total usage"})
	}
	if totalOK && usedOK {
		usedClean := max(0.0, used)
		if total <= 0 {
			// Free-tier OpenRouter accounts report total_credits=0 and positive total_usage.
			// Never display positive total_usage as remaining balance and never hard-block free tier.
			rem := 0.0
			snapshot.FreeTier = lo.ToPtr(true)
			snapshot.Available = lo.ToPtr(true)
			snapshot.Limit = lo.ToPtr(0.0)
			snapshot.Used = lo.ToPtr(usedClean)
			snapshot.Remaining = lo.ToPtr(rem)
			snapshot.Components = append(snapshot.Components, objects.BalanceAmount{Amount: rem, Currency: "USD", Kind: objects.BalanceAmountKindRemaining, Label: "remaining credits"})
			snapshot.PrimaryBalance = &snapshot.Components[len(snapshot.Components)-1]
		} else {
			rem := max(0.0, total-usedClean)
			snapshot.Limit = lo.ToPtr(total)
			snapshot.Used = lo.ToPtr(usedClean)
			snapshot.Remaining = lo.ToPtr(rem)
			snapshot.Available = lo.ToPtr(rem > 0)
			snapshot.Components = append(snapshot.Components, objects.BalanceAmount{Amount: rem, Currency: "USD", Kind: objects.BalanceAmountKindRemaining, Label: "remaining credits"})
		}
	}

	return snapshot
}

func parseNanoGPTBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	payload := decodeBalanceProbeJSON(body)
	snapshot := baseBalanceSnapshot(statusCode)
	if len(payload) == 0 {
		snapshot.Success = false
		return snapshot
	}
	snapshot.RawSummary = safeSummary(payload, "usd_balance", "nano_balance")
	addBalanceAmount(&snapshot.Components, payload, "usd_balance", "USD", objects.BalanceAmountKindAvailable, "USD balance")
	addBalanceAmount(&snapshot.Components, payload, "nano_balance", "NANO", objects.BalanceAmountKindAvailable, "NANO balance")
	if len(snapshot.Components) == 0 {
		snapshot.Success = false
	}

	return snapshot
}

func parseGenericBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
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
	currency := firstString(
		stringAt(data, "currency"),
		stringAt(data, "balance_currency"),
		stringAt(data, "credit_currency"),
		stringAt(payload, "currency"),
	)

	snapshot.Available = boolFromAny(firstNonNil(
		valueAt(data, "available"),
		valueAt(data, "is_available"),
		valueAt(data, "enabled"),
		valueAt(payload, "available"),
		valueAt(payload, "is_available"),
	))
	snapshot.AccountID = firstString(stringAt(data, "id"), stringAt(data, "account_id"))
	snapshot.AccountStatus = firstString(stringAt(data, "status"), stringAt(data, "account_status"))
	snapshot.RawSummary = safeSummary(data, "id", "account_id", "status", "account_status", "currency")
	addBalanceAmount(&snapshot.Components, data, "balance", currency, objects.BalanceAmountKindAvailable, "balance")
	addBalanceAmount(&snapshot.Components, data, "available_balance", currency, objects.BalanceAmountKindAvailable, "available")
	addBalanceAmount(&snapshot.Components, data, "available", currency, objects.BalanceAmountKindAvailable, "available")
	addBalanceAmount(&snapshot.Components, data, "credits", currency, objects.BalanceAmountKindAvailable, "credits")
	addBalanceAmount(&snapshot.Components, data, "credit", currency, objects.BalanceAmountKindAvailable, "credit")
	addBalanceAmount(&snapshot.Components, data, "total_balance", currency, objects.BalanceAmountKindTotal, "total")
	addBalanceAmount(&snapshot.Components, data, "total", currency, objects.BalanceAmountKindTotal, "total")
	addBalanceAmount(&snapshot.Components, data, "remaining", currency, objects.BalanceAmountKindRemaining, "remaining")
	if snapshot.Available == nil && len(snapshot.Components) == 0 {
		snapshot.Success = false
	}

	return snapshot
}

func baseBalanceSnapshot(statusCode int) objects.ChannelKeyBalanceSnapshot {
	return objects.ChannelKeyBalanceSnapshot{
		Success:    statusCode >= http.StatusOK && statusCode < http.StatusMultipleChoices,
		StatusCode: statusCode,
		Components: []objects.BalanceAmount{},
	}
}

func decodeBalanceProbeJSON(body []byte) map[string]any {
	if len(bytes.TrimSpace(body)) == 0 || len(body) > channelKeyHealthCheckHTTPMaxBodySize {
		return nil
	}

	var payload any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil
	}
	normalized, _ := normalizeChannelKeyHealthCheckJSONNumbers(payload).(map[string]any)

	return normalized
}

func selectChannelBalancePrimary(snapshot *objects.ChannelKeyBalanceSnapshot, preferredCurrency string) {
	if snapshot == nil || len(snapshot.Components) == 0 {
		return
	}

	if preferredCurrency != "" {
		if primary, ok := largestBalanceAmount(snapshot.Components, preferredCurrency, false); ok {
			snapshot.PrimaryBalance = &primary
			return
		}
	}
	if primary, ok := largestBalanceAmount(snapshot.Components, "", true); ok {
		snapshot.PrimaryBalance = &primary
		return
	}
	if primary, ok := largestBalanceAmount(snapshot.Components, "", false); ok {
		snapshot.PrimaryBalance = &primary
	}
}

func largestBalanceAmount(amounts []objects.BalanceAmount, currency string, positiveOnly bool) (objects.BalanceAmount, bool) {
	var selected objects.BalanceAmount
	found := false
	for _, amount := range amounts {
		// Never treat consumed usage or capacity limit as a primary remaining balance.
		if amount.Kind == objects.BalanceAmountKindUsed || amount.Kind == objects.BalanceAmountKindLimit {
			continue
		}
		if currency != "" && !strings.EqualFold(amount.Currency, currency) {
			continue
		}
		if math.IsNaN(amount.Amount) || math.IsInf(amount.Amount, 0) {
			continue
		}
		if positiveOnly && amount.Amount <= 0 {
			continue
		}
		if !found || amount.Amount > selected.Amount {
			selected = amount
			found = true
		}
	}

	return selected, found
}

func addBalanceAmount(amounts *[]objects.BalanceAmount, source map[string]any, key string, currency string, kind objects.BalanceAmountKind, label string) {
	amount, ok := amountFromAny(valueAt(source, key))
	if !ok {
		return
	}

	*amounts = append(*amounts, objects.BalanceAmount{
		Amount:   amount,
		Currency: strings.ToUpper(strings.TrimSpace(currency)),
		Kind:     kind,
		Label:    label,
	})
}

func amountFromAny(value any) (float64, bool) {
	switch v := value.(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case json.Number:
		number, err := v.Float64()
		return number, err == nil
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return number, err == nil
	default:
		return 0, false
	}
}

func mapAt(source map[string]any, key string) map[string]any {
	value, _ := valueAt(source, key).(map[string]any)
	return value
}

func arrayAt(source map[string]any, key string) []any {
	value, _ := valueAt(source, key).([]any)
	return value
}

func stringAt(source map[string]any, key string) string {
	value, _ := valueAt(source, key).(string)
	return strings.TrimSpace(value)
}

func valueAt(source map[string]any, key string) any {
	if source == nil || key == "" {
		return nil
	}
	if value, ok := source[key]; ok {
		return value
	}

	parts := strings.Split(key, ".")
	var current any = source
	for _, part := range parts {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[part]
	}

	return current
}

func safeSummary(source map[string]any, keys ...string) map[string]any {
	if len(source) == 0 || len(keys) == 0 {
		return nil
	}

	summary := make(map[string]any, len(keys))
	for _, key := range keys {
		if value := valueAt(source, key); value != nil {
			summary[key] = value
		}
	}
	if len(summary) == 0 {
		return nil
	}

	return summary
}

func firstString(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}

	return ""
}

func cloneChannelKeyBalanceSnapshot(snapshot *objects.ChannelKeyBalanceSnapshot) *objects.ChannelKeyBalanceSnapshot {
	if snapshot == nil {
		return nil
	}
	next := *snapshot
	if snapshot.Available != nil {
		v := *snapshot.Available
		next.Available = &v
	}
	if snapshot.Unlimited != nil {
		v := *snapshot.Unlimited
		next.Unlimited = &v
	}
	if snapshot.FreeTier != nil {
		v := *snapshot.FreeTier
		next.FreeTier = &v
	}
	if snapshot.Used != nil {
		v := *snapshot.Used
		next.Used = &v
	}
	if snapshot.Limit != nil {
		v := *snapshot.Limit
		next.Limit = &v
	}
	if snapshot.Remaining != nil {
		v := *snapshot.Remaining
		next.Remaining = &v
	}
	if snapshot.UsageRatio != nil {
		v := *snapshot.UsageRatio
		next.UsageRatio = &v
	}
	if snapshot.ResetAt != nil {
		v := *snapshot.ResetAt
		next.ResetAt = &v
	}
	if snapshot.PrimaryBalance != nil {
		primary := *snapshot.PrimaryBalance
		if primary.ExpiresAt != nil {
			t := *primary.ExpiresAt
			primary.ExpiresAt = &t
		}
		next.PrimaryBalance = &primary
	}
	next.Components = slices.Clone(snapshot.Components)
	if len(snapshot.Windows) > 0 {
		next.Windows = make([]objects.ChannelQuotaWindow, len(snapshot.Windows))
		for i, w := range snapshot.Windows {
			nw := w
			if w.Used != nil {
				v := *w.Used
				nw.Used = &v
			}
			if w.Limit != nil {
				v := *w.Limit
				nw.Limit = &v
			}
			if w.Remaining != nil {
				v := *w.Remaining
				nw.Remaining = &v
			}
			if w.UsageRatio != nil {
				v := *w.UsageRatio
				nw.UsageRatio = &v
			}
			if w.ResetAt != nil {
				v := *w.ResetAt
				nw.ResetAt = &v
			}
			next.Windows[i] = nw
		}
	}
	if snapshot.RawSummary != nil {
		next.RawSummary = make(map[string]any, len(snapshot.RawSummary))
		for key, value := range snapshot.RawSummary {
			next.RawSummary[key] = value
		}
	}

	return &next
}
