package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
)

type balanceProbePresetSpec struct {
	ID           objects.ChannelBalanceProbePreset
	Provider     channel.Type
	Name         string
	Experimental bool
	HTTP         objects.ChannelKeyHealthCheckHTTPRule
	Parse        func([]byte, int) objects.ChannelKeyBalanceSnapshot
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
}

func channelBalanceProbePresetByID(id objects.ChannelBalanceProbePreset) (balanceProbePresetSpec, bool) {
	for _, spec := range channelBalanceProbePresets {
		if spec.ID == id {
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
	case channel.TypeMoonshot, channel.TypeMoonshotAnthropic, channel.TypeMoonshotCoding:
		return channelBalanceProbePresetByID(objects.ChannelBalanceProbePresetMoonshot)
	case channel.TypeOpenrouter:
		return channelBalanceProbePresetByID(objects.ChannelBalanceProbePresetOpenRouter)
	case channel.TypeNanogpt, channel.TypeNanogptResponses:
		return channelBalanceProbePresetByID(objects.ChannelBalanceProbePresetNanoGPT)
	default:
		return balanceProbePresetSpec{}, false
	}
}

func channelBalanceProbeSpecForChannel(ch *ent.Channel) (balanceProbePresetSpec, *objects.ChannelBalanceProbe, bool) {
	if ch == nil {
		return balanceProbePresetSpec{}, nil, false
	}

	var probe *objects.ChannelBalanceProbe
	if ch.Settings != nil {
		probe = ch.Settings.BalanceProbe
	}
	if probe != nil && probe.Preset != "" {
		spec, ok := channelBalanceProbePresetByID(probe.Preset)
		if !ok || (spec.Experimental && !probe.Experimental) {
			return balanceProbePresetSpec{}, probe, false
		}

		return spec, probe, true
	}

	spec, ok := channelBalanceProbePresetForChannelType(ch.Type)
	return spec, probe, ok
}

func (svc *ChannelService) runChannelKeyBalanceProbe(ctx context.Context, ch *ent.Channel, key string, trigger objects.ChannelKeyHealthCheckTrigger) ChannelKeyHealthCheckResult {
	spec, probe, ok := channelBalanceProbeSpecForChannel(ch)
	if !ok {
		return ChannelKeyHealthCheckResult{Success: false, Reason: "channel does not have a supported balance probe"}
	}

	rule := spec.HTTP
	if probe != nil {
		if probe.HTTP != nil {
			rule = *probe.HTTP
		}
		if probe.TimeoutMs > 0 {
			rule.TimeoutMs = probe.TimeoutMs
		}
	}

	resp, result := svc.executeChannelKeyProbeHTTP(ctx, ch, key, rule)
	result.Rule = spec.Name
	result.Source = balanceProbeEventSource(trigger, result.Success)
	if resp == nil {
		return result
	}

	snapshot := spec.Parse(resp.Body, resp.StatusCode)
	snapshot.Provider = string(spec.ID)
	snapshot.CheckedAt = time.Now()
	snapshot.StatusCode = resp.StatusCode
	selectChannelBalancePrimary(&snapshot, preferredChannelBalanceCurrency(probe))

	result.StatusCode = resp.StatusCode
	result.BalanceSnapshot = &snapshot
	result.Available = snapshot.Available
	if snapshot.PrimaryBalance != nil {
		result.Balance = snapshot.PrimaryBalance.Amount
		result.Currency = snapshot.PrimaryBalance.Currency
	}
	result.Success = result.Success && snapshot.Success
	if snapshot.Available != nil && !*snapshot.Available {
		result.Success = false
		if result.Reason == "" || result.Reason == "ok" {
			result.Reason = "provider reports key unavailable"
		}
	}
	if result.Success {
		result.Reason = "ok"
	} else if result.Reason == "" || result.Reason == "ok" {
		result.Reason = "balance probe failed"
	}
	result.Source = balanceProbeEventSource(trigger, result.Success)

	return result
}

func (svc *ChannelService) executeChannelKeyProbeHTTP(ctx context.Context, ch *ent.Channel, key string, rule objects.ChannelKeyHealthCheckHTTPRule) (*httpclient.Response, ChannelKeyHealthCheckResult) {
	targetURL, err := buildChannelKeyHealthCheckHTTPURL(ctx, ch.BaseURL, rule)
	if err != nil {
		return nil, ChannelKeyHealthCheckResult{Success: false, Reason: err.Error()}
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
			return nil, ChannelKeyHealthCheckResult{Success: false, Reason: "header name is invalid"}
		}

		headers.Set(name, header.Value)
	}

	injection := rule.KeyInjection
	if injection == nil || injection.Location == "" || injection.Location == objects.ChannelKeyHealthCheckKeyInjectionAuthorizationBearer {
		headers.Set("Authorization", "Bearer "+key)
	} else if injection.Location == objects.ChannelKeyHealthCheckKeyInjectionHeader {
		headerName := strings.TrimSpace(injection.HeaderName)
		if !isValidHTTPHeaderName(headerName) {
			return nil, ChannelKeyHealthCheckResult{Success: false, Reason: "header name is invalid"}
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

	hc := svc.httpClient
	if ch.Settings != nil && ch.Settings.Proxy != nil {
		hc = svc.httpClient.WithProxy(ch.Settings.Proxy)
	}
	hc = channelKeyHealthCheckNoRedirectClient(hc)

	resp, err := doChannelKeyHealthCheckHTTPRequest(requestCtx, hc, httpReq)
	if errors.Is(err, errChannelKeyHealthCheckResponseTooLarge) {
		return nil, ChannelKeyHealthCheckResult{Success: false, Reason: "response body too large"}
	}
	if err != nil {
		return nil, ChannelKeyHealthCheckResult{Success: false, Reason: "http request failed: " + err.Error()}
	}

	result := evaluateChannelKeyHealthCheckHTTPResponse(resp, rule.ExpectedStatuses)
	if resp != nil {
		result.StatusCode = resp.StatusCode
	}

	return resp, result
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
	if snapshot.Available != nil && !*snapshot.Available {
		snapshot.Success = false
	}

	return snapshot
}

func parseSiliconFlowBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	payload := decodeBalanceProbeJSON(body)
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
	if snapshot.Available != nil && !*snapshot.Available {
		snapshot.Success = false
	}

	return snapshot
}

func parseMoonshotBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	payload := decodeBalanceProbeJSON(body)
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

	return snapshot
}

func parseOpenRouterBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	payload := decodeBalanceProbeJSON(body)
	data := mapAt(payload, "data")
	if len(data) == 0 {
		data = payload
	}

	snapshot := baseBalanceSnapshot(statusCode)
	snapshot.RawSummary = safeSummary(data, "total_credits", "total_usage")
	total, totalOK := amountFromAny(valueAt(data, "total_credits"))
	used, usedOK := amountFromAny(valueAt(data, "total_usage"))
	if totalOK {
		snapshot.Components = append(snapshot.Components, objects.BalanceAmount{Amount: total, Currency: "USD", Kind: objects.BalanceAmountKindTotal, Label: "total credits"})
	}
	if usedOK {
		snapshot.Components = append(snapshot.Components, objects.BalanceAmount{Amount: used, Currency: "USD", Kind: objects.BalanceAmountKindUsed, Label: "total usage"})
	}
	if totalOK && usedOK {
		snapshot.Components = append(snapshot.Components, objects.BalanceAmount{Amount: total - used, Currency: "USD", Kind: objects.BalanceAmountKindRemaining, Label: "remaining credits"})
	}

	return snapshot
}

func parseNanoGPTBalanceSnapshot(body []byte, statusCode int) objects.ChannelKeyBalanceSnapshot {
	payload := decodeBalanceProbeJSON(body)
	snapshot := baseBalanceSnapshot(statusCode)
	snapshot.RawSummary = safeSummary(payload, "usd_balance", "nano_balance")
	addBalanceAmount(&snapshot.Components, payload, "usd_balance", "USD", objects.BalanceAmountKindAvailable, "USD balance")
	addBalanceAmount(&snapshot.Components, payload, "nano_balance", "NANO", objects.BalanceAmountKindAvailable, "NANO balance")

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
	if snapshot.PrimaryBalance != nil {
		primary := *snapshot.PrimaryBalance
		next.PrimaryBalance = &primary
	}
	next.Components = slices.Clone(snapshot.Components)
	if snapshot.RawSummary != nil {
		next.RawSummary = make(map[string]any, len(snapshot.RawSummary))
		for key, value := range snapshot.RawSummary {
			next.RawSummary[key] = value
		}
	}

	return &next
}
