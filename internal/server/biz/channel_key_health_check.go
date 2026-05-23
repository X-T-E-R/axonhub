package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/expr-lang/expr"
	"golang.org/x/sync/errgroup"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
)

const (
	channelKeyHealthCheckTaskTimeout     = 55 * time.Second
	channelKeyHealthCheckHTTPDefaultMs   = 5000
	channelKeyHealthCheckHTTPMaxBodySize = 1 << 20
	channelKeyHealthCheckMaxPassWhenLen  = 1024
	channelKeyHealthCheckMaxPassWhenNode = 64
	channelKeyHealthCheckMaxChannels     = 4
	channelKeyHealthCheckMaxKeys         = 8
	channelKeyHealthCheckFailureCode     = 499
	channelKeyHealthCheckHistoryLimit    = 20
)

// ChannelKeyHealthCheckTester is a narrow seam for reusing the manual
// channel-key test path without making biz import the orchestrator package.
type ChannelKeyHealthCheckTester interface {
	TestSingleChannelAPIKey(ctx context.Context, channelID objects.GUID, key string, modelID *string, proxy *httpclient.ProxyConfig) ChannelKeyHealthCheckBuiltinResult
}

type ChannelKeyHealthCheckBuiltinResult struct {
	Success bool
	Reason  string
	Latency float64
}

type ChannelKeyHealthCheckResult struct {
	Success   bool
	Reason    string
	Balance   any
	Currency  string
	Available *bool
	Rule      string
}

func (svc *ChannelService) SetChannelKeyHealthCheckTester(tester ChannelKeyHealthCheckTester) {
	svc.keyHealthCheckTester = tester
}

func (svc *ChannelService) runChannelKeyHealthChecksScheduled(ctx context.Context) {
	runCtx, cancel := context.WithTimeout(ctx, channelKeyHealthCheckTaskTimeout)
	defer cancel()

	if err := svc.RunDueChannelKeyHealthChecks(runCtx, time.Now()); err != nil {
		log.Warn(ctx, "channel key health check task failed", log.Cause(err))
	}
}

func (svc *ChannelService) RunDueChannelKeyHealthChecks(ctx context.Context, now time.Time) error {
	channels, err := svc.dueChannelKeyHealthCheckChannels(ctx, now)
	if err != nil {
		return err
	}
	if len(channels) == 0 {
		return nil
	}

	var checked atomic.Int32
	var failed atomic.Int32

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(channelKeyHealthCheckMaxChannels)

	for _, ch := range channels {
		channelEntity := ch
		group.Go(func() error {
			result, err := svc.runChannelKeyHealthCheckForChannel(groupCtx, channelEntity, now)
			if err != nil {
				failed.Add(1)
				log.Warn(groupCtx, "channel key health check failed",
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
		return err
	}

	log.Info(ctx, "channel key health check task completed",
		log.Int("channels", len(channels)),
		log.Int32("checked_keys", checked.Load()),
		log.Int32("failed_keys", failed.Load()),
	)

	return nil
}

func (svc *ChannelService) dueChannelKeyHealthCheckChannels(ctx context.Context, now time.Time) ([]*ent.Channel, error) {
	channels, err := svc.entFromContext(ctx).Channel.Query().
		Where(channel.StatusEQ(channel.StatusEnabled)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query channels for key health check: %w", err)
	}

	due := make([]*ent.Channel, 0, len(channels))
	for _, ch := range channels {
		if !channelKeyHealthCheckDue(ch, now) {
			continue
		}

		due = append(due, ch)
	}

	return due, nil
}

func channelKeyHealthCheckDue(ch *ent.Channel, now time.Time) bool {
	if ch == nil || ch.Settings == nil || ch.Settings.KeyHealthCheck == nil {
		return false
	}

	health := ch.Settings.KeyHealthCheck
	if !health.Enabled {
		return false
	}
	if ch.Credentials.IsOAuth() {
		return false
	}

	targetKeys := channelKeyHealthCheckTargetKeys(ch)
	if len(targetKeys) == 0 {
		return false
	}

	lastCheckedAt := latestChannelKeyHealthCheckAt(health.KeyMetadata, targetKeys)
	if lastCheckedAt.IsZero() {
		return true
	}

	return !now.Before(lastCheckedAt.Add(time.Duration(health.IntervalMinutesOrDefault()) * time.Minute))
}

func latestChannelKeyHealthCheckAt(metadata []objects.ChannelKeyMetadata, keys []string) time.Time {
	if len(metadata) == 0 || len(keys) == 0 {
		return time.Time{}
	}

	metadataByID := make(map[string]objects.ChannelKeyMetadata, len(metadata))
	for _, item := range metadata {
		if item.ID == "" || item.LastCheckedAt == nil {
			continue
		}

		metadataByID[item.ID] = item
	}

	for _, key := range keys {
		item, ok := metadataByID[objects.ChannelAPIKeyFingerprint(key)]
		if !ok || item.LastCheckedAt == nil {
			return time.Time{}
		}
	}

	var latest time.Time
	for _, item := range metadataByID {
		if item.LastCheckedAt.After(latest) {
			latest = *item.LastCheckedAt
		}
	}

	return latest
}

func channelKeyHealthCheckTargetKeys(ch *ent.Channel) []string {
	if ch == nil || ch.Settings == nil || ch.Settings.KeyHealthCheck == nil {
		return nil
	}

	health := ch.Settings.KeyHealthCheck
	if health.IncludeDisabled {
		allKeys := ch.Credentials.GetAllAPIKeys()
		if len(allKeys) == 0 {
			return nil
		}

		archived := channelArchivedAPIKeys(ch.Settings)
		if len(archived) == 0 {
			return allKeys
		}

		return ch.Credentials.GetRoutableAPIKeys(nil, archived)
	}

	return ch.Credentials.GetRoutableAPIKeys(ch.DisabledAPIKeys, channelArchivedAPIKeys(ch.Settings))
}

type channelKeyHealthCheckChannelResult struct {
	CheckedKeys int
	FailedKeys  int
}

func (svc *ChannelService) runChannelKeyHealthCheckForChannel(ctx context.Context, ch *ent.Channel, now time.Time) (channelKeyHealthCheckChannelResult, error) {
	if ch.Settings == nil || ch.Settings.KeyHealthCheck == nil {
		return channelKeyHealthCheckChannelResult{}, nil
	}

	targetKeys := channelKeyHealthCheckTargetKeys(ch)
	if len(targetKeys) == 0 {
		return channelKeyHealthCheckChannelResult{}, nil
	}

	settings := ensureChannelKeyHealthCheckSettings(ch.Settings)
	results := make([]ChannelKeyHealthCheckResult, len(targetKeys))

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(min(channelKeyHealthCheckMaxKeys, len(targetKeys)))

	for i, key := range targetKeys {
		index := i
		apiKey := key
		group.Go(func() error {
			results[index] = svc.checkChannelAPIKeyHealth(groupCtx, ch, apiKey)

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return channelKeyHealthCheckChannelResult{}, err
	}

	failed := 0
	for i, key := range targetKeys {
		result := results[i]
		if !result.Success {
			failed++
		}

		metadata := upsertChannelKeyHealthCheckMetadata(settings.KeyHealthCheck.KeyMetadata, key, result, now, objects.ChannelKeyHealthCheckTriggerScheduled)
		settings.KeyHealthCheck.KeyMetadata = metadata
	}
	mergeChannelKeyOperationalStatus(ch, settings.KeyHealthCheck)

	if _, err := svc.entFromContext(ctx).Channel.UpdateOneID(ch.ID).SetSettings(settings).Save(ctx); err != nil {
		return channelKeyHealthCheckChannelResult{}, fmt.Errorf("failed to save channel key metadata: %w", err)
	}

	if err := svc.applyChannelKeyHealthCheckFailureActions(ctx, ch.ID, settings.KeyHealthCheck, targetKeys, results); err != nil {
		return channelKeyHealthCheckChannelResult{}, err
	}

	if failed > 0 {
		svc.asyncReloadChannels()
	}

	return channelKeyHealthCheckChannelResult{CheckedKeys: len(targetKeys), FailedKeys: failed}, nil
}

func (svc *ChannelService) checkChannelAPIKeyHealth(ctx context.Context, ch *ent.Channel, key string) ChannelKeyHealthCheckResult {
	rules := ch.Settings.KeyHealthCheck.Rules
	if len(rules) == 0 {
		rules = defaultChannelKeyHealthCheckRules(ch.Type)
	}

	var merged ChannelKeyHealthCheckResult
	for _, rule := range rules {
		if rule.Enabled != nil && !*rule.Enabled {
			continue
		}

		result := svc.runChannelKeyHealthCheckRule(ctx, ch, key, rule)
		result.Rule = summarizeChannelKeyHealthCheckRule(rule)
		merged = mergeChannelKeyHealthCheckResult(merged, result)
		if !result.Success {
			return merged
		}
	}

	if merged.Reason == "" {
		merged.Reason = "ok"
	}
	merged.Success = true

	return merged
}

func defaultChannelKeyHealthCheckRules(channelType channel.Type) []objects.ChannelKeyHealthCheckRule {
	if channelType == channel.TypeDeepseek {
		return []objects.ChannelKeyHealthCheckRule{{
			ID:   "deepseek-balance",
			Name: "DeepSeek balance",
			Type: objects.ChannelKeyHealthCheckRuleTypeHTTP,
			HTTP: &objects.ChannelKeyHealthCheckHTTPRule{
				Method:           objects.ChannelKeyHealthCheckHTTPMethodGet,
				URLMode:          objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
				URL:              "https://api.deepseek.com/user/balance",
				ExpectedStatuses: []int{http.StatusOK},
				PassWhen:         "json.is_available == true",
			},
		}}
	}

	return []objects.ChannelKeyHealthCheckRule{{
		ID:   "builtin-key-test",
		Name: "Built-in key test",
		Type: objects.ChannelKeyHealthCheckRuleTypeBuiltinTest,
		Builtin: &objects.ChannelKeyHealthCheckBuiltin{
			Kind: "channel_api_key_test",
		},
	}}
}

func (svc *ChannelService) runChannelKeyHealthCheckRule(ctx context.Context, ch *ent.Channel, key string, rule objects.ChannelKeyHealthCheckRule) ChannelKeyHealthCheckResult {
	switch rule.Type {
	case objects.ChannelKeyHealthCheckRuleTypeBuiltinTest:
		return svc.runBuiltinChannelKeyHealthCheck(ctx, ch, key)
	case objects.ChannelKeyHealthCheckRuleTypeHTTP:
		if rule.HTTP == nil {
			return ChannelKeyHealthCheckResult{Success: false, Reason: "http rule is missing"}
		}

		return svc.runHTTPChannelKeyHealthCheck(ctx, ch, key, *rule.HTTP)
	default:
		return ChannelKeyHealthCheckResult{Success: false, Reason: fmt.Sprintf("unsupported rule type %q", rule.Type)}
	}
}

func summarizeChannelKeyHealthCheckRule(rule objects.ChannelKeyHealthCheckRule) string {
	name := strings.TrimSpace(rule.Name)
	if name != "" {
		return name
	}
	if rule.ID != "" {
		return rule.ID
	}
	if rule.Type != "" {
		return string(rule.Type)
	}

	return "health check"
}

func (svc *ChannelService) runBuiltinChannelKeyHealthCheck(ctx context.Context, ch *ent.Channel, key string) ChannelKeyHealthCheckResult {
	if svc.keyHealthCheckTester == nil {
		return ChannelKeyHealthCheckResult{Success: false, Reason: "built-in key tester is not configured"}
	}

	result := svc.keyHealthCheckTester.TestSingleChannelAPIKey(ctx, objects.GUID{ID: ch.ID}, key, nil, getProxyConfig(ch.Settings))
	if result.Reason == "" {
		result.Reason = "ok"
	}

	return ChannelKeyHealthCheckResult{
		Success: result.Success,
		Reason:  result.Reason,
	}
}

func (svc *ChannelService) runHTTPChannelKeyHealthCheck(ctx context.Context, ch *ent.Channel, key string, rule objects.ChannelKeyHealthCheckHTTPRule) ChannelKeyHealthCheckResult {
	targetURL, err := buildChannelKeyHealthCheckHTTPURL(ch.BaseURL, rule)
	if err != nil {
		return ChannelKeyHealthCheckResult{Success: false, Reason: err.Error()}
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

		headers.Set(name, header.Value)
	}

	injection := rule.KeyInjection
	if injection == nil || injection.Location == "" || injection.Location == objects.ChannelKeyHealthCheckKeyInjectionAuthorizationBearer {
		headers.Set("Authorization", "Bearer "+key)
	} else if injection.Location == objects.ChannelKeyHealthCheckKeyInjectionHeader {
		headers.Set(strings.TrimSpace(injection.HeaderName), key)
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

	resp, err := hc.Do(requestCtx, httpReq)
	var httpErr *httpclient.Error
	if err != nil && errors.As(err, &httpErr) {
		resp = &httpclient.Response{
			StatusCode: httpErr.StatusCode,
			Headers:    httpErr.Headers,
			Body:       httpErr.Body,
		}
	} else if err != nil {
		return ChannelKeyHealthCheckResult{Success: false, Reason: "http request failed: " + err.Error()}
	}

	result := evaluateChannelKeyHealthCheckHTTPResponse(resp, rule.ExpectedStatuses)
	if resp == nil {
		return result
	}
	if len(resp.Body) > channelKeyHealthCheckHTTPMaxBodySize {
		result.Success = false
		result.Reason = "response body too large"
		return result
	}

	balance, currency, available := extractDeepSeekBalanceMetadata(resp.Body)
	result.Balance = balance
	result.Currency = currency
	result.Available = available

	if result.Success && strings.TrimSpace(rule.PassWhen) != "" {
		pass, reason := evaluateChannelKeyHealthCheckPassWhen(resp, rule.PassWhen)
		if !pass {
			result.Success = false
			result.Reason = reason
		}
	}
	if available != nil && !*available {
		result.Success = false
		if result.Reason == "" || result.Reason == "ok" {
			result.Reason = "provider reports key unavailable"
		}
	}

	return result
}

func buildChannelKeyHealthCheckHTTPURL(baseURL string, rule objects.ChannelKeyHealthCheckHTTPRule) (string, error) {
	if rule.URLMode == objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL {
		u, err := url.Parse(strings.TrimSpace(rule.URL))
		if err != nil || u.Scheme == "" || u.Host == "" {
			return "", fmt.Errorf("invalid absolute url")
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return "", fmt.Errorf("absolute url scheme must be http or https")
		}

		return u.String(), nil
	}

	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("invalid provider base url")
	}

	path := strings.TrimSpace(rule.Path)
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	if strings.Contains(path, "://") {
		return "", fmt.Errorf("path must not be an absolute URL")
	}

	base.Path = strings.TrimRight(base.Path, "/") + "/" + strings.TrimLeft(path, "/")

	return base.String(), nil
}

func evaluateChannelKeyHealthCheckHTTPResponse(resp *httpclient.Response, expectedStatuses []int) ChannelKeyHealthCheckResult {
	if resp == nil {
		return ChannelKeyHealthCheckResult{Success: false, Reason: "empty http response"}
	}

	expected := expectedStatuses
	if len(expected) == 0 {
		expected = []int{http.StatusOK}
	}

	if !slices.Contains(expected, resp.StatusCode) {
		return ChannelKeyHealthCheckResult{
			Success: false,
			Reason:  fmt.Sprintf("unexpected status %d", resp.StatusCode),
		}
	}

	return ChannelKeyHealthCheckResult{Success: true, Reason: "ok"}
}

func evaluateChannelKeyHealthCheckPassWhen(resp *httpclient.Response, passWhen string) (bool, string) {
	passWhen = strings.TrimSpace(passWhen)
	if passWhen == "" {
		return true, "ok"
	}
	if len(passWhen) > channelKeyHealthCheckMaxPassWhenLen {
		return false, "pass condition is too long"
	}

	env := buildChannelKeyHealthCheckPassWhenEnv(resp)
	program, err := expr.Compile(passWhen, expr.Env(env), expr.AsBool(), expr.DisableAllBuiltins(), expr.MaxNodes(channelKeyHealthCheckMaxPassWhenNode))
	if err != nil {
		return false, "invalid pass condition"
	}

	output, err := expr.Run(program, env)
	if err != nil {
		return false, "pass condition failed"
	}

	pass, ok := output.(bool)
	if !ok || !pass {
		return false, "pass condition was not satisfied"
	}

	return true, "ok"
}

func buildChannelKeyHealthCheckPassWhenEnv(resp *httpclient.Response) map[string]any {
	env := map[string]any{
		"status":    0,
		"ok":        false,
		"headers":   map[string]any{},
		"json":      map[string]any{},
		"balance":   nil,
		"currency":  "",
		"available": nil,
	}
	if resp == nil {
		return env
	}

	env["status"] = resp.StatusCode
	env["ok"] = resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
	env["headers"] = normalizeChannelKeyHealthCheckHeaders(resp.Headers)

	if len(bytes.TrimSpace(resp.Body)) == 0 || len(resp.Body) > channelKeyHealthCheckHTTPMaxBodySize {
		return env
	}

	var payload any
	decoder := json.NewDecoder(bytes.NewReader(resp.Body))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return env
	}

	env["json"] = normalizeChannelKeyHealthCheckJSONNumbers(payload)
	balance, currency, available := extractDeepSeekBalanceMetadata(resp.Body)
	env["balance"] = normalizeChannelKeyHealthCheckBalance(balance)
	env["currency"] = currency
	env["available"] = available

	return env
}

func normalizeChannelKeyHealthCheckHeaders(headers http.Header) map[string]any {
	normalized := make(map[string]any, len(headers)*2)
	for key, values := range headers {
		if len(values) == 0 {
			continue
		}

		normalized[key] = values[0]
		normalized[strings.ToLower(key)] = values[0]
	}

	return normalized
}

func normalizeChannelKeyHealthCheckJSONNumbers(value any) any {
	switch v := value.(type) {
	case json.Number:
		if intValue, err := v.Int64(); err == nil {
			return intValue
		}
		if floatValue, err := v.Float64(); err == nil {
			return floatValue
		}

		return string(v)
	case map[string]any:
		for key, child := range v {
			v[key] = normalizeChannelKeyHealthCheckJSONNumbers(child)
		}

		return v
	case []any:
		for i, child := range v {
			v[i] = normalizeChannelKeyHealthCheckJSONNumbers(child)
		}

		return v
	default:
		return value
	}
}

func normalizeChannelKeyHealthCheckBalance(value any) any {
	switch v := value.(type) {
	case string:
		if floatValue, err := strconv.ParseFloat(v, 64); err == nil {
			return floatValue
		}

		return v
	default:
		return normalizeChannelKeyHealthCheckJSONNumbers(v)
	}
}

func extractDeepSeekBalanceMetadata(body []byte) (any, string, *bool) {
	if len(bytes.TrimSpace(body)) == 0 || len(body) > channelKeyHealthCheckHTTPMaxBodySize {
		return nil, "", nil
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, "", nil
	}

	available := boolFromAny(payload["is_available"])
	var balance any
	var currency string

	infos, _ := payload["balance_infos"].([]any)
	for _, item := range infos {
		info, _ := item.(map[string]any)
		if len(info) == 0 {
			continue
		}

		if currency == "" {
			currency, _ = info["currency"].(string)
		}
		if balance == nil {
			balance = firstNonNil(info["total_balance"], info["granted_balance"], info["topped_up_balance"])
		}
		if balance != nil {
			break
		}
	}

	return balance, currency, available
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}

	return nil
}

func boolFromAny(value any) *bool {
	switch v := value.(type) {
	case bool:
		return &v
	case string:
		parsed, err := strconv.ParseBool(v)
		if err != nil {
			return nil
		}

		return &parsed
	default:
		return nil
	}
}

func mergeChannelKeyHealthCheckResult(current, next ChannelKeyHealthCheckResult) ChannelKeyHealthCheckResult {
	if next.Reason != "" {
		current.Reason = next.Reason
	}
	if next.Balance != nil {
		current.Balance = next.Balance
	}
	if next.Currency != "" {
		current.Currency = next.Currency
	}
	if next.Available != nil {
		current.Available = next.Available
	}
	if next.Rule != "" {
		current.Rule = next.Rule
	}
	current.Success = next.Success

	return current
}

func upsertChannelKeyHealthCheckMetadata(metadata []objects.ChannelKeyMetadata, key string, result ChannelKeyHealthCheckResult, now time.Time, trigger objects.ChannelKeyHealthCheckTrigger) []objects.ChannelKeyMetadata {
	id := objects.ChannelAPIKeyFingerprint(key)
	next := slices.Clone(metadata)
	for i := range next {
		if next[i].ID != id {
			continue
		}

		next[i] = updateChannelKeyHealthCheckMetadata(next[i], key, result, now, trigger)

		return next
	}

	return append(next, updateChannelKeyHealthCheckMetadata(objects.ChannelKeyMetadata{ID: id}, key, result, now, trigger))
}

func updateChannelKeyHealthCheckMetadata(meta objects.ChannelKeyMetadata, key string, result ChannelKeyHealthCheckResult, now time.Time, trigger objects.ChannelKeyHealthCheckTrigger) objects.ChannelKeyMetadata {
	meta.ID = objects.ChannelAPIKeyFingerprint(key)
	meta.MaskedKey = objects.MaskChannelAPIKey(key)
	meta.Status = objects.ChannelKeyStatusActive
	meta.LastCheckedAt = &now
	meta.Success = &result.Success
	meta.Reason = result.Reason
	meta.Balance = result.Balance
	meta.Currency = result.Currency
	meta.Available = result.Available
	if result.Success {
		meta.FailureCount = 0
	} else {
		meta.FailureCount++
	}
	meta.History = appendChannelKeyHealthCheckHistory(meta.History, meta.ID, result, now, trigger)

	return meta
}

func appendChannelKeyHealthCheckHistory(history []objects.ChannelKeyHealthCheckHistoryEntry, keyID string, result ChannelKeyHealthCheckResult, now time.Time, trigger objects.ChannelKeyHealthCheckTrigger) []objects.ChannelKeyHealthCheckHistoryEntry {
	entry := objects.ChannelKeyHealthCheckHistoryEntry{
		ID:        fmt.Sprintf("%s:%d:%s", keyID, now.UnixNano(), trigger),
		CheckedAt: now,
		Success:   result.Success,
		Reason:    result.Reason,
		Balance:   result.Balance,
		Currency:  result.Currency,
		Available: result.Available,
		Trigger:   trigger,
		Rule:      result.Rule,
	}

	next := append([]objects.ChannelKeyHealthCheckHistoryEntry{entry}, history...)
	if len(next) > channelKeyHealthCheckHistoryLimit {
		next = next[:channelKeyHealthCheckHistoryLimit]
	}

	return next
}

func (svc *ChannelService) RunChannelAPIKeyHealthCheck(ctx context.Context, channelID int, keyIDs []string) ([]*ChannelAPIKeyInventoryItem, error) {
	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, channelID)
	if err != nil {
		return nil, fmt.Errorf("failed to get channel: %w", err)
	}
	if ch.Credentials.IsOAuth() {
		return nil, fmt.Errorf("cannot run API key health checks for OAuth channels")
	}

	targetKeys := selectedChannelKeyHealthCheckTargets(ch, keyIDs)
	if len(targetKeys) == 0 {
		return svc.ChannelAPIKeyInventory(ctx, channelID)
	}

	settings := ensureChannelKeyHealthCheckSettings(ch.Settings)
	chForCheck := *ch
	chForCheck.Settings = settings

	results := make([]ChannelKeyHealthCheckResult, len(targetKeys))
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(min(channelKeyHealthCheckMaxKeys, len(targetKeys)))

	for i, key := range targetKeys {
		index := i
		apiKey := key
		group.Go(func() error {
			results[index] = svc.checkChannelAPIKeyHealth(groupCtx, &chForCheck, apiKey)

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return nil, err
	}

	now := time.Now()
	failed := 0
	for i, key := range targetKeys {
		result := results[i]
		if !result.Success {
			failed++
		}

		settings.KeyHealthCheck.KeyMetadata = upsertChannelKeyHealthCheckMetadata(settings.KeyHealthCheck.KeyMetadata, key, result, now, objects.ChannelKeyHealthCheckTriggerManual)
	}
	mergeChannelKeyOperationalStatus(ch, settings.KeyHealthCheck)

	if _, err := svc.entFromContext(ctx).Channel.UpdateOneID(channelID).SetSettings(settings).Save(ctx); err != nil {
		return nil, fmt.Errorf("failed to save channel key metadata: %w", err)
	}

	if err := svc.applyChannelKeyHealthCheckFailureActions(ctx, channelID, settings.KeyHealthCheck, targetKeys, results); err != nil {
		return nil, err
	}
	if failed > 0 {
		svc.asyncReloadChannels()
	}

	return svc.ChannelAPIKeyInventory(ctx, channelID)
}

func mergeChannelKeyOperationalStatus(ch *ent.Channel, health *objects.ChannelKeyHealthCheck) {
	if ch == nil || health == nil || len(health.KeyMetadata) == 0 {
		return
	}

	disabledByID := make(map[string]struct{}, len(ch.DisabledAPIKeys))
	for _, disabled := range ch.DisabledAPIKeys {
		if disabled.Key == "" {
			continue
		}

		disabledByID[objects.ChannelAPIKeyFingerprint(disabled.Key)] = struct{}{}
	}

	archivedByID := make(map[string]struct{}, len(channelArchivedAPIKeys(ch.Settings)))
	for _, archived := range channelArchivedAPIKeys(ch.Settings) {
		if archived.ID == "" {
			continue
		}

		archivedByID[archived.ID] = struct{}{}
	}

	for i := range health.KeyMetadata {
		id := health.KeyMetadata[i].ID
		if _, ok := archivedByID[id]; ok {
			health.KeyMetadata[i].Status = objects.ChannelKeyStatusArchived
			continue
		}
		if _, ok := disabledByID[id]; ok {
			health.KeyMetadata[i].Status = objects.ChannelKeyStatusDisabled
		}
	}
}

func selectedChannelKeyHealthCheckTargets(ch *ent.Channel, keyIDs []string) []string {
	if ch == nil {
		return nil
	}
	if len(keyIDs) == 0 {
		return ch.Credentials.GetRoutableAPIKeys(ch.DisabledAPIKeys, channelArchivedAPIKeys(ch.Settings))
	}

	archived := channelArchivedAPIKeys(ch.Settings)
	archivedIDs := make(map[string]struct{}, len(archived))
	for _, item := range archived {
		if item.ID != "" {
			archivedIDs[item.ID] = struct{}{}
		}
	}

	targets := make([]string, 0, len(keyIDs))
	seen := make(map[string]struct{}, len(keyIDs))
	for _, keyID := range keyIDs {
		key, ok := resolveChannelAPIKey(ch.Credentials, keyID)
		if !ok {
			continue
		}

		id := objects.ChannelAPIKeyFingerprint(key)
		if _, ok := archivedIDs[id]; ok {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}

		seen[id] = struct{}{}
		targets = append(targets, key)
	}

	return targets
}

func (svc *ChannelService) applyChannelKeyHealthCheckFailureActions(
	ctx context.Context,
	channelID int,
	health *objects.ChannelKeyHealthCheck,
	keys []string,
	results []ChannelKeyHealthCheckResult,
) error {
	if health == nil {
		return nil
	}

	action := health.FailureActionOrDefault()
	if action == objects.ChannelKeyHealthCheckFailureActionReportOnly {
		return nil
	}

	metadataByID := make(map[string]objects.ChannelKeyMetadata, len(health.KeyMetadata))
	for _, item := range health.KeyMetadata {
		metadataByID[item.ID] = item
	}

	for i, key := range keys {
		if i >= len(results) || results[i].Success {
			continue
		}

		id := objects.ChannelAPIKeyFingerprint(key)
		meta := metadataByID[id]
		if meta.FailureCount < health.FailureThresholdOrDefault() {
			continue
		}

		reason := results[i].Reason
		if reason == "" {
			reason = "channel key health check failed"
		}

		if err := svc.applyChannelKeyHealthCheckFailureAction(ctx, channelID, key, action, reason); err != nil {
			return err
		}
	}

	return nil
}

func (svc *ChannelService) applyChannelKeyHealthCheckFailureAction(
	ctx context.Context,
	channelID int,
	key string,
	action objects.ChannelKeyHealthCheckFailureAction,
	reason string,
) error {
	switch action {
	case objects.ChannelKeyHealthCheckFailureActionDisable:
		return svc.DisableAPIKey(ctx, channelID, key, channelKeyHealthCheckFailureCode, reason)
	case objects.ChannelKeyHealthCheckFailureActionArchive:
		err := svc.ArchiveChannelAPIKey(ctx, channelID, key, reason)
		if errors.Is(err, errCannotArchiveLastUsableChannelAPIKey) {
			return nil
		}

		return err
	case objects.ChannelKeyHealthCheckFailureActionDelete:
		_, err := svc.DeleteChannelAPIKey(ctx, channelID, key)
		return err
	case objects.ChannelKeyHealthCheckFailureActionReportOnly:
		return nil
	default:
		return fmt.Errorf("unsupported failure action %q", action)
	}
}
