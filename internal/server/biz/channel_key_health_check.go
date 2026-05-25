package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/expr-lang/expr"
	"golang.org/x/sync/errgroup"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
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
	channelKeyHealthCheckMaxHistoryLimit = 100
	channelKeyHealthCheckMinBackoffMin   = 1
	channelKeyHealthCheckMaxBackoffMin   = 10080
	channelKeyHealthCheckKeySpacing      = time.Second
	channelKeyHealthCheckMaxKeyJitter    = 250 * time.Millisecond
)

var errChannelKeyHealthCheckResponseTooLarge = errors.New("response body too large")

var (
	channelKeyHealthCheckDelayForKey = defaultChannelKeyHealthCheckDelayForKey
	channelKeyHealthCheckWait        = waitChannelKeyHealthCheckDelay
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
	Success        bool
	Reason         string
	Balance        any
	Currency       string
	Available      *bool
	Rule           string
	StatusCode     int
	MatchedPolicy  string
	Action         string
	NextCheckAt    *time.Time
	BackoffAttempt int
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
	if channelKeyHealthCheckDueKeys(ch, targetKeys, now) > 0 {
		return true
	}
	if channelKeyHealthCheckAllKeysBackedOff(ch, targetKeys, now) {
		return false
	}

	lastCheckedAt := latestChannelKeyHealthCheckAt(health.KeyMetadata, targetKeys)
	if lastCheckedAt.IsZero() {
		return true
	}

	return !now.Before(lastCheckedAt.Add(time.Duration(health.IntervalMinutesOrDefault()) * time.Minute))
}

func channelKeyHealthCheckDueKeys(ch *ent.Channel, keys []string, now time.Time) int {
	if ch == nil || ch.Settings == nil || ch.Settings.KeyHealthCheck == nil || len(keys) == 0 {
		return 0
	}

	metadataByID := channelKeyMetadataByID(ch.Settings.KeyHealthCheck.KeyMetadata)
	count := 0
	for _, key := range keys {
		meta, ok := metadataByID[objects.ChannelAPIKeyFingerprint(key)]
		if !ok {
			count++
			continue
		}
		if meta.NextCheckAt != nil {
			if !now.Before(*meta.NextCheckAt) {
				count++
			}
			continue
		}
		if meta.LastCheckedAt == nil {
			count++
			continue
		}
		if !now.Before(meta.LastCheckedAt.Add(time.Duration(ch.Settings.KeyHealthCheck.IntervalMinutesOrDefault()) * time.Minute)) {
			count++
		}
	}

	return count
}

func channelKeyHealthCheckAllKeysBackedOff(ch *ent.Channel, keys []string, now time.Time) bool {
	if ch == nil || ch.Settings == nil || ch.Settings.KeyHealthCheck == nil || len(keys) == 0 {
		return false
	}

	metadataByID := channelKeyMetadataByID(ch.Settings.KeyHealthCheck.KeyMetadata)
	for _, key := range keys {
		meta, ok := metadataByID[objects.ChannelAPIKeyFingerprint(key)]
		if !ok || meta.NextCheckAt == nil || !now.Before(*meta.NextCheckAt) {
			return false
		}
	}

	return true
}

func filterChannelKeyHealthCheckDueKeys(ch *ent.Channel, keys []string, now time.Time) []string {
	if ch == nil || ch.Settings == nil || ch.Settings.KeyHealthCheck == nil || len(keys) == 0 {
		return nil
	}

	metadataByID := channelKeyMetadataByID(ch.Settings.KeyHealthCheck.KeyMetadata)
	due := make([]string, 0, len(keys))
	for _, key := range keys {
		meta, ok := metadataByID[objects.ChannelAPIKeyFingerprint(key)]
		if !ok {
			due = append(due, key)
			continue
		}
		if meta.NextCheckAt != nil {
			if !now.Before(*meta.NextCheckAt) {
				due = append(due, key)
			}
			continue
		}
		if meta.LastCheckedAt == nil || !now.Before(meta.LastCheckedAt.Add(time.Duration(ch.Settings.KeyHealthCheck.IntervalMinutesOrDefault())*time.Minute)) {
			due = append(due, key)
		}
	}

	return due
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
	targetKeys = filterChannelKeyHealthCheckDueKeys(ch, targetKeys, now)
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
		if err := waitBeforeChannelKeyHealthCheck(groupCtx, index, apiKey); err != nil {
			if waitErr := group.Wait(); waitErr != nil {
				return channelKeyHealthCheckChannelResult{}, waitErr
			}

			return channelKeyHealthCheckChannelResult{}, err
		}
		group.Go(func() error {
			results[index] = svc.checkChannelAPIKeyHealth(groupCtx, ch, apiKey)

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return channelKeyHealthCheckChannelResult{}, err
	}

	failed := 0
	for i := range targetKeys {
		result := results[i]
		if !result.Success {
			failed++
		}
	}
	allCheckedKeysFailed := len(targetKeys) > 0 && failed == len(targetKeys)
	results = svc.applyFailurePolicyToHealthCheckResults(ctx, ch, settings, targetKeys, results, now, objects.ChannelKeyHealthCheckTriggerScheduled, allCheckedKeysFailed)
	for i, key := range targetKeys {
		settings.KeyHealthCheck.KeyMetadata = upsertChannelKeyHealthCheckMetadata(settings.KeyHealthCheck.KeyMetadata, key, results[i], now, objects.ChannelKeyHealthCheckTriggerScheduled, settings.KeyHealthCheck.HistoryLimitOrDefault())
	}
	mergeChannelKeyOperationalStatus(ch, settings.KeyHealthCheck)

	if _, err := svc.entFromContext(ctx).Channel.UpdateOneID(ch.ID).SetSettings(settings).Save(ctx); err != nil {
		return channelKeyHealthCheckChannelResult{}, fmt.Errorf("failed to save channel key metadata: %w", err)
	}

	if err := svc.applyChannelKeyHealthCheckPolicyActions(ctx, ch.ID, targetKeys, results, allCheckedKeysFailed); err != nil {
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
	targetURL, err := buildChannelKeyHealthCheckHTTPURL(ctx, ch.BaseURL, rule)
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
		if !isValidHTTPHeaderName(name) {
			return ChannelKeyHealthCheckResult{Success: false, Reason: "header name is invalid"}
		}

		headers.Set(name, header.Value)
	}

	injection := rule.KeyInjection
	if injection == nil || injection.Location == "" || injection.Location == objects.ChannelKeyHealthCheckKeyInjectionAuthorizationBearer {
		headers.Set("Authorization", "Bearer "+key)
	} else if injection.Location == objects.ChannelKeyHealthCheckKeyInjectionHeader {
		headerName := strings.TrimSpace(injection.HeaderName)
		if !isValidHTTPHeaderName(headerName) {
			return ChannelKeyHealthCheckResult{Success: false, Reason: "header name is invalid"}
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
		return ChannelKeyHealthCheckResult{Success: false, Reason: "response body too large"}
	}
	if err != nil {
		return ChannelKeyHealthCheckResult{Success: false, Reason: "http request failed: " + err.Error()}
	}

	result := evaluateChannelKeyHealthCheckHTTPResponse(resp, rule.ExpectedStatuses)
	if resp == nil {
		return result
	}
	result.StatusCode = resp.StatusCode
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

func buildChannelKeyHealthCheckHTTPURL(ctx context.Context, baseURL string, rule objects.ChannelKeyHealthCheckHTTPRule) (string, error) {
	if rule.URLMode == objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL {
		if err := validateChannelKeyHealthCheckAbsoluteURLMatchesBaseURL(baseURL, rule.URL); err != nil {
			return "", err
		}
		u, err := validateChannelKeyHealthCheckAbsoluteURL(ctx, rule.URL)
		if err != nil {
			return "", err
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

func validateChannelKeyHealthCheckAbsoluteURL(ctx context.Context, rawURL string) (*url.URL, error) {
	u, err := validateChannelKeyHealthCheckAbsoluteURLStatic(rawURL)
	if err != nil {
		return nil, err
	}

	host := strings.TrimSpace(u.Hostname())
	if ip := net.ParseIP(host); ip != nil {
		return u, nil
	}

	resolveCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	addrs, err := net.DefaultResolver.LookupIPAddr(resolveCtx, host)
	if err != nil {
		return nil, fmt.Errorf("failed to validate absolute url host")
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("failed to validate absolute url host")
	}
	for _, addr := range addrs {
		if isBlockedChannelKeyHealthCheckIP(addr.IP) {
			return nil, fmt.Errorf("absolute url host is not allowed")
		}
	}

	return u, nil
}

func channelKeyHealthCheckNoRedirectClient(hc *httpclient.HttpClient) *httpclient.HttpClient {
	if hc == nil || hc.GetNativeClient() == nil {
		return hc
	}

	native := *hc.GetNativeClient()
	native.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}

	return httpclient.NewHttpClientWithClient(&native)
}

func doChannelKeyHealthCheckHTTPRequest(ctx context.Context, hc *httpclient.HttpClient, request *httpclient.Request) (*httpclient.Response, error) {
	if hc == nil || hc.GetNativeClient() == nil {
		return nil, fmt.Errorf("http client is not configured")
	}

	var body io.Reader
	if len(request.Body) > 0 {
		body = bytes.NewReader(request.Body)
	}
	rawReq, err := http.NewRequestWithContext(ctx, request.Method, request.URL, body)
	if err != nil {
		return nil, err
	}
	rawReq.Header = request.Headers.Clone()
	if rawReq.Header == nil {
		rawReq.Header = make(http.Header)
	}
	if rawReq.Header.Get("User-Agent") == "" {
		rawReq.Header.Set("User-Agent", "axonhub/1.0")
	}
	rawReq.Header.Set("Accept", "application/json")

	rawResp, err := hc.GetNativeClient().Do(rawReq)
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = rawResp.Body.Close()
	}()

	respBody, err := readChannelKeyHealthCheckResponseBody(rawResp.Body)
	if err != nil {
		return nil, err
	}

	return &httpclient.Response{
		StatusCode:  rawResp.StatusCode,
		Headers:     rawResp.Header,
		Body:        respBody,
		Request:     request,
		RawRequest:  rawReq,
		RawResponse: rawResp,
	}, nil
}

func readChannelKeyHealthCheckResponseBody(reader io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	n, err := buf.ReadFrom(io.LimitReader(reader, channelKeyHealthCheckHTTPMaxBodySize+1))
	if err != nil {
		return nil, err
	}
	if n > channelKeyHealthCheckHTTPMaxBodySize {
		return nil, errChannelKeyHealthCheckResponseTooLarge
	}

	return buf.Bytes(), nil
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
	var firstBalance any
	var firstCurrency string
	var fallbackBalance any
	var fallbackCurrency string
	var fallbackNumeric float64
	var hasFallback bool

	infos, _ := payload["balance_infos"].([]any)
	for _, item := range infos {
		info, _ := item.(map[string]any)
		if len(info) == 0 {
			continue
		}

		currency, _ := info["currency"].(string)
		balance := firstNonNil(info["total_balance"], info["granted_balance"], info["topped_up_balance"])
		if balance == nil {
			continue
		}
		if firstBalance == nil {
			firstBalance = balance
			firstCurrency = currency
		}
		if isDeepSeekCNYCurrency(currency) {
			return balance, currency, available
		}
		if numeric, ok := channelKeyHealthCheckNumericBalance(balance); ok {
			if !hasFallback || numeric > fallbackNumeric {
				fallbackBalance = balance
				fallbackCurrency = currency
				fallbackNumeric = numeric
				hasFallback = true
			}
		}
	}
	if hasFallback {
		return fallbackBalance, fallbackCurrency, available
	}

	return firstBalance, firstCurrency, available
}

func isDeepSeekCNYCurrency(currency string) bool {
	currency = strings.TrimSpace(currency)

	return strings.EqualFold(currency, "CNY") || strings.EqualFold(currency, "RMB")
}

func waitBeforeChannelKeyHealthCheck(ctx context.Context, index int, key string) error {
	delay := channelKeyHealthCheckDelayForKey(index, key)
	if delay <= 0 {
		return nil
	}

	return channelKeyHealthCheckWait(ctx, delay)
}

func defaultChannelKeyHealthCheckDelayForKey(index int, key string) time.Duration {
	if index <= 0 {
		return 0
	}

	return channelKeyHealthCheckKeySpacing + deterministicChannelKeyHealthCheckJitter(key)
}

func deterministicChannelKeyHealthCheckJitter(key string) time.Duration {
	if channelKeyHealthCheckMaxKeyJitter <= 0 {
		return 0
	}

	hash := fnv.New32a()
	_, _ = hash.Write([]byte(key))

	return time.Duration(hash.Sum32() % uint32(channelKeyHealthCheckMaxKeyJitter))
}

func waitChannelKeyHealthCheckDelay(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
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
	if next.StatusCode != 0 {
		current.StatusCode = next.StatusCode
	}
	current.Success = next.Success

	return current
}

func upsertChannelKeyHealthCheckMetadata(metadata []objects.ChannelKeyMetadata, key string, result ChannelKeyHealthCheckResult, now time.Time, trigger objects.ChannelKeyHealthCheckTrigger, historyLimit int) []objects.ChannelKeyMetadata {
	id := objects.ChannelAPIKeyFingerprint(key)
	next := slices.Clone(metadata)
	for i := range next {
		if next[i].ID != id {
			continue
		}

		next[i] = updateChannelKeyHealthCheckMetadata(next[i], key, result, now, trigger, historyLimit)

		return next
	}

	return append(next, updateChannelKeyHealthCheckMetadata(objects.ChannelKeyMetadata{ID: id}, key, result, now, trigger, historyLimit))
}

func updateChannelKeyHealthCheckMetadata(meta objects.ChannelKeyMetadata, key string, result ChannelKeyHealthCheckResult, now time.Time, trigger objects.ChannelKeyHealthCheckTrigger, historyLimit int) objects.ChannelKeyMetadata {
	meta.ID = objects.ChannelAPIKeyFingerprint(key)
	meta.MaskedKey = objects.MaskChannelAPIKey(key)
	meta.Status = objects.ChannelKeyStatusActive
	meta.LastCheckedAt = &now
	meta.Success = &result.Success
	meta.Reason = result.Reason
	meta.Balance = result.Balance
	meta.Currency = result.Currency
	meta.Available = result.Available
	meta.StatusCode = result.StatusCode
	meta.MatchedPolicy = result.MatchedPolicy
	meta.Action = result.Action
	if result.Success {
		meta.FailureCount = 0
		meta.NextCheckAt = nil
		meta.BackoffAttempt = 0
		result.NextCheckAt = nil
		result.BackoffAttempt = 0
	} else {
		meta.FailureCount++
		meta.NextCheckAt = result.NextCheckAt
		meta.BackoffAttempt = result.BackoffAttempt
	}
	meta.History = appendChannelKeyHealthCheckHistory(meta.History, meta.ID, result, now, trigger, historyLimit)

	return meta
}

func appendChannelKeyHealthCheckHistory(history []objects.ChannelKeyHealthCheckHistoryEntry, keyID string, result ChannelKeyHealthCheckResult, now time.Time, trigger objects.ChannelKeyHealthCheckTrigger, historyLimit int) []objects.ChannelKeyHealthCheckHistoryEntry {
	entry := objects.ChannelKeyHealthCheckHistoryEntry{
		ID:             fmt.Sprintf("%s:%d:%s", keyID, now.UnixNano(), trigger),
		CheckedAt:      now,
		Success:        result.Success,
		Reason:         result.Reason,
		Balance:        result.Balance,
		Currency:       result.Currency,
		Available:      result.Available,
		Trigger:        trigger,
		Rule:           result.Rule,
		StatusCode:     result.StatusCode,
		MatchedPolicy:  result.MatchedPolicy,
		Action:         result.Action,
		NextCheckAt:    result.NextCheckAt,
		BackoffAttempt: result.BackoffAttempt,
	}

	if historyLimit <= 0 {
		historyLimit = channelKeyHealthCheckHistoryLimit
	}
	historyLimit = min(historyLimit, channelKeyHealthCheckMaxHistoryLimit)
	next := append([]objects.ChannelKeyHealthCheckHistoryEntry{entry}, history...)
	if len(next) > historyLimit {
		next = next[:historyLimit]
	}

	return next
}

func applyChannelKeyHealthCheckPoliciesToResults(
	health *objects.ChannelKeyHealthCheck,
	keys []string,
	results []ChannelKeyHealthCheckResult,
	now time.Time,
	trigger objects.ChannelKeyHealthCheckTrigger,
	allCheckedKeysFailed bool,
) []ChannelKeyHealthCheckResult {
	if health == nil {
		return results
	}
	if len(health.Policies) == 0 {
		return applyLegacyChannelKeyHealthCheckPolicySummary(health, keys, results)
	}

	metadataByID := channelKeyMetadataByID(health.KeyMetadata)
	next := slices.Clone(results)
	for i, key := range keys {
		if i >= len(next) {
			continue
		}

		result := next[i]
		meta := metadataByID[objects.ChannelAPIKeyFingerprint(key)]
		failureCount := meta.FailureCount
		if !result.Success {
			failureCount++
		} else {
			failureCount = 0
		}

		matchedPolicies := matchingChannelKeyHealthCheckPolicies(health.Policies, result, failureCount, trigger, allCheckedKeysFailed)
		if len(matchedPolicies) == 0 {
			next[i] = result
			continue
		}

		result.MatchedPolicy = summarizeChannelKeyHealthCheckPolicies(matchedPolicies)
		result.Action = summarizeChannelKeyHealthCheckMatchedActions(matchedPolicies)
		result.BackoffAttempt = meta.BackoffAttempt
		for _, policy := range matchedPolicies {
			for _, action := range policy.Actions {
				if action.Type != objects.ChannelKeyHealthCheckPolicyActionBackoff || action.Backoff == nil || result.Success {
					continue
				}

				result.BackoffAttempt = meta.BackoffAttempt + 1
				nextCheckAt := now.Add(computeChannelKeyHealthCheckBackoffDuration(*action.Backoff, result.BackoffAttempt))
				result.NextCheckAt = &nextCheckAt
			}
		}
		next[i] = result
	}

	return next
}

func applyLegacyChannelKeyHealthCheckPolicySummary(
	health *objects.ChannelKeyHealthCheck,
	keys []string,
	results []ChannelKeyHealthCheckResult,
) []ChannelKeyHealthCheckResult {
	metadataByID := channelKeyMetadataByID(health.KeyMetadata)
	next := slices.Clone(results)
	for i, key := range keys {
		if i >= len(next) || next[i].Success {
			continue
		}

		meta := metadataByID[objects.ChannelAPIKeyFingerprint(key)]
		if meta.FailureCount+1 < health.FailureThresholdOrDefault() {
			continue
		}

		next[i].MatchedPolicy = "legacy failure threshold"
		next[i].Action = legacyChannelKeyHealthCheckPolicyActions(health.FailureActionOrDefault())
	}

	return next
}

func channelKeyMetadataByID(metadata []objects.ChannelKeyMetadata) map[string]objects.ChannelKeyMetadata {
	metadataByID := make(map[string]objects.ChannelKeyMetadata, len(metadata))
	for _, item := range metadata {
		if item.ID == "" {
			continue
		}

		metadataByID[item.ID] = item
	}

	return metadataByID
}

func matchingChannelKeyHealthCheckPolicies(
	policies []objects.ChannelKeyHealthCheckPolicy,
	result ChannelKeyHealthCheckResult,
	failureCount int,
	trigger objects.ChannelKeyHealthCheckTrigger,
	allCheckedKeysFailed bool,
) []objects.ChannelKeyHealthCheckPolicy {
	matched := make([]objects.ChannelKeyHealthCheckPolicy, 0, len(policies))
	for _, policy := range policies {
		if policy.Enabled != nil && !*policy.Enabled {
			continue
		}
		if len(policy.Actions) == 0 {
			continue
		}
		if !channelKeyHealthCheckPolicyHasCondition(policy.Conditions) {
			continue
		}
		if channelKeyHealthCheckPolicyMatches(policy, result, failureCount, trigger, allCheckedKeysFailed) {
			matched = append(matched, policy)
		}
	}

	return matched
}

func channelKeyHealthCheckPolicyHasCondition(condition objects.ChannelKeyHealthCheckPolicyCondition) bool {
	return condition.MinFailureCount != nil ||
		len(condition.StatusCodes) > 0 ||
		condition.Available != nil ||
		condition.BalanceLTE != nil ||
		strings.TrimSpace(condition.ReasonContains) != "" ||
		condition.AllCheckedKeysFailed != nil ||
		strings.TrimSpace(condition.Expr) != ""
}

func channelKeyHealthCheckPolicyMatches(
	policy objects.ChannelKeyHealthCheckPolicy,
	result ChannelKeyHealthCheckResult,
	failureCount int,
	trigger objects.ChannelKeyHealthCheckTrigger,
	allCheckedKeysFailed bool,
) bool {
	condition := policy.Conditions
	if condition.MinFailureCount != nil && failureCount < *condition.MinFailureCount {
		return false
	}
	if len(condition.StatusCodes) > 0 && !slices.Contains(condition.StatusCodes, result.StatusCode) {
		return false
	}
	if condition.Available != nil {
		if result.Available == nil || *result.Available != *condition.Available {
			return false
		}
	}
	if condition.BalanceLTE != nil {
		balance, ok := channelKeyHealthCheckNumericBalance(result.Balance)
		if !ok || balance > *condition.BalanceLTE {
			return false
		}
	}
	reasonContains := strings.TrimSpace(condition.ReasonContains)
	if reasonContains != "" && !strings.Contains(strings.ToLower(result.Reason), strings.ToLower(reasonContains)) {
		return false
	}
	if condition.AllCheckedKeysFailed != nil && allCheckedKeysFailed != *condition.AllCheckedKeysFailed {
		return false
	}
	if strings.TrimSpace(condition.Expr) != "" {
		matches, reason := evaluateChannelKeyHealthCheckPolicyExpr(condition.Expr, result, failureCount, trigger, allCheckedKeysFailed)
		if !matches {
			_ = reason
			return false
		}
	}

	return true
}

func evaluateChannelKeyHealthCheckPolicyExpr(
	expression string,
	result ChannelKeyHealthCheckResult,
	failureCount int,
	trigger objects.ChannelKeyHealthCheckTrigger,
	allCheckedKeysFailed bool,
) (bool, string) {
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return true, "ok"
	}
	if len(expression) > channelKeyHealthCheckMaxPassWhenLen {
		return false, "policy condition is too long"
	}

	env := buildChannelKeyHealthCheckPolicyEnv(result, failureCount, trigger, allCheckedKeysFailed)
	program, err := expr.Compile(expression, expr.Env(env), expr.AsBool(), expr.DisableAllBuiltins(), expr.MaxNodes(channelKeyHealthCheckMaxPassWhenNode))
	if err != nil {
		return false, "invalid policy condition"
	}

	output, err := expr.Run(program, env)
	if err != nil {
		return false, "policy condition failed"
	}

	matches, ok := output.(bool)
	if !ok || !matches {
		return false, "policy condition was not satisfied"
	}

	return true, "ok"
}

func buildChannelKeyHealthCheckPolicyEnv(
	result ChannelKeyHealthCheckResult,
	failureCount int,
	trigger objects.ChannelKeyHealthCheckTrigger,
	allCheckedKeysFailed bool,
) map[string]any {
	var available any
	if result.Available != nil {
		available = *result.Available
	}

	return map[string]any{
		"success":              result.Success,
		"failureCount":         failureCount,
		"status":               result.StatusCode,
		"reason":               result.Reason,
		"balance":              normalizeChannelKeyHealthCheckBalance(result.Balance),
		"currency":             result.Currency,
		"available":            available,
		"allCheckedKeysFailed": allCheckedKeysFailed,
		"trigger":              string(trigger),
	}
}

func channelKeyHealthCheckNumericBalance(value any) (float64, bool) {
	switch v := normalizeChannelKeyHealthCheckBalance(value).(type) {
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
		number, err := strconv.ParseFloat(v, 64)
		return number, err == nil
	default:
		return 0, false
	}
}

func summarizeChannelKeyHealthCheckPolicy(policy objects.ChannelKeyHealthCheckPolicy) string {
	name := strings.TrimSpace(policy.Name)
	if name != "" {
		return name
	}
	if policy.ID != "" {
		return policy.ID
	}

	return "health policy"
}

func summarizeChannelKeyHealthCheckPolicies(policies []objects.ChannelKeyHealthCheckPolicy) string {
	parts := make([]string, 0, len(policies))
	for _, policy := range policies {
		parts = append(parts, summarizeChannelKeyHealthCheckPolicy(policy))
	}

	return strings.Join(parts, ",")
}

func summarizeChannelKeyHealthCheckMatchedActions(policies []objects.ChannelKeyHealthCheckPolicy) string {
	actions := make([]objects.ChannelKeyHealthCheckPolicyAction, 0, len(policies))
	for _, policy := range policies {
		actions = append(actions, policy.Actions...)
	}

	return summarizeChannelKeyHealthCheckPolicyActions(actions)
}

func summarizeChannelKeyHealthCheckPolicyActions(actions []objects.ChannelKeyHealthCheckPolicyAction) string {
	if len(actions) == 0 {
		return ""
	}

	parts := make([]string, 0, len(actions))
	for _, action := range actions {
		if action.Type == "" {
			continue
		}

		parts = append(parts, string(action.Type))
	}

	return strings.Join(parts, ",")
}

func computeChannelKeyHealthCheckBackoffDuration(backoff objects.ChannelKeyHealthCheckBackoff, attempt int) time.Duration {
	interval := backoff.IntervalMinutes
	if interval <= 0 {
		interval = 5
	}
	interval = clampInt(interval, channelKeyHealthCheckMinBackoffMin, channelKeyHealthCheckMaxBackoffMin)

	maxInterval := backoff.MaxIntervalMinutes
	if maxInterval <= 0 {
		maxInterval = interval
	}
	maxInterval = clampInt(maxInterval, interval, channelKeyHealthCheckMaxBackoffMin)

	if backoff.Mode == objects.ChannelKeyHealthCheckBackoffModeExponential {
		multiplier := backoff.Multiplier
		if multiplier < 1 {
			multiplier = 2
		}
		if attempt > 1 {
			interval = int(math.Round(float64(interval) * math.Pow(multiplier, float64(attempt-1))))
		}
	}
	interval = clampInt(interval, channelKeyHealthCheckMinBackoffMin, maxInterval)

	return time.Duration(interval) * time.Minute
}

func clampInt(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}

	return value
}

func (svc *ChannelService) RunChannelAPIKeyHealthCheck(ctx context.Context, channelID int, keyIDs []string) ([]*ChannelAPIKeyInventoryItem, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeWriteChannels); err != nil {
		return nil, err
	}

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
		if err := waitBeforeChannelKeyHealthCheck(groupCtx, index, apiKey); err != nil {
			if waitErr := group.Wait(); waitErr != nil {
				return nil, waitErr
			}

			return nil, err
		}
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
	for i := range targetKeys {
		result := results[i]
		if !result.Success {
			failed++
		}
	}
	allCheckedKeysFailed := len(targetKeys) > 0 && failed == len(targetKeys)
	results = svc.applyFailurePolicyToHealthCheckResults(ctx, ch, settings, targetKeys, results, now, objects.ChannelKeyHealthCheckTriggerManual, allCheckedKeysFailed)
	for i, key := range targetKeys {
		settings.KeyHealthCheck.KeyMetadata = upsertChannelKeyHealthCheckMetadata(settings.KeyHealthCheck.KeyMetadata, key, results[i], now, objects.ChannelKeyHealthCheckTriggerManual, settings.KeyHealthCheck.HistoryLimitOrDefault())
	}
	mergeChannelKeyOperationalStatus(ch, settings.KeyHealthCheck)

	if _, err := svc.entFromContext(ctx).Channel.UpdateOneID(channelID).SetSettings(settings).Save(ctx); err != nil {
		return nil, fmt.Errorf("failed to save channel key metadata: %w", err)
	}

	if err := svc.applyChannelKeyHealthCheckPolicyActions(ctx, channelID, targetKeys, results, allCheckedKeysFailed); err != nil {
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
	allCheckedKeysFailed bool,
) error {
	if health == nil {
		return nil
	}

	if len(health.Policies) > 0 {
		return svc.applyChannelKeyHealthCheckPolicyActions(ctx, channelID, keys, results, allCheckedKeysFailed)
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

func (svc *ChannelService) applyChannelKeyHealthCheckPolicyActions(
	ctx context.Context,
	channelID int,
	keys []string,
	results []ChannelKeyHealthCheckResult,
	allCheckedKeysFailed bool,
) error {
	channelDisabled := false
	for i, key := range keys {
		if i >= len(results) {
			continue
		}

		result := results[i]
		if result.MatchedPolicy == "" || result.Action == "" {
			continue
		}

		for _, action := range strings.Split(result.Action, ",") {
			action = strings.TrimSpace(action)
			if action == "" {
				continue
			}
			if action == string(objects.ChannelKeyHealthCheckPolicyActionReportOnly) ||
				action == string(objects.ChannelKeyHealthCheckPolicyActionBackoff) ||
				action == string(objects.FailurePolicyActionReportOnly) ||
				action == string(objects.FailurePolicyActionBackoffKey) {
				continue
			}

			reason := result.Reason
			if reason == "" {
				reason = "channel key health check policy matched"
			}
			if !strings.EqualFold(result.MatchedPolicy, "Legacy health-check failure threshold") &&
				!strings.EqualFold(result.MatchedPolicy, "legacy failure threshold") {
				reason = fmt.Sprintf("%s: %s", result.MatchedPolicy, reason)
			}
			switch objects.ChannelKeyHealthCheckPolicyActionType(action) {
			case objects.ChannelKeyHealthCheckPolicyActionDisableKey:
				if err := svc.DisableAPIKey(ctx, channelID, key, channelKeyHealthCheckFailureCode, reason); err != nil {
					return err
				}
			case objects.ChannelKeyHealthCheckPolicyActionArchiveKey:
				err := svc.ArchiveChannelAPIKey(ctx, channelID, key, reason)
				if errors.Is(err, errCannotArchiveLastUsableChannelAPIKey) {
					continue
				}
				if err != nil {
					return err
				}
			case objects.ChannelKeyHealthCheckPolicyActionDeleteKey:
				_, err := svc.DeleteChannelAPIKey(ctx, channelID, key)
				if err != nil {
					return err
				}
			case objects.ChannelKeyHealthCheckPolicyActionDisableChannel:
				if channelDisabled || !allCheckedKeysFailed {
					continue
				}
				if err := svc.disableChannelForKeyHealthPolicy(ctx, channelID, result.MatchedPolicy); err != nil {
					return err
				}
				channelDisabled = true
			default:
				switch objects.FailurePolicyActionType(action) {
				case objects.FailurePolicyActionDisableKey:
					if err := svc.DisableAPIKey(ctx, channelID, key, channelKeyHealthCheckFailureCode, reason); err != nil {
						return err
					}
				case objects.FailurePolicyActionArchiveKey:
					err := svc.ArchiveChannelAPIKey(ctx, channelID, key, reason)
					if errors.Is(err, errCannotArchiveLastUsableChannelAPIKey) {
						continue
					}
					if err != nil {
						return err
					}
				case objects.FailurePolicyActionDeleteKey:
					_, err := svc.DeleteChannelAPIKey(ctx, channelID, key)
					if err != nil {
						return err
					}
				case objects.FailurePolicyActionDisableChannel:
					if channelDisabled || !allCheckedKeysFailed {
						continue
					}
					if err := svc.disableChannelForKeyHealthPolicy(ctx, channelID, result.MatchedPolicy); err != nil {
						return err
					}
					channelDisabled = true
				default:
					return fmt.Errorf("unsupported key health policy action %q", action)
				}
			}
		}
	}

	return nil
}

func legacyChannelKeyHealthCheckPolicyActions(action objects.ChannelKeyHealthCheckFailureAction) string {
	switch action {
	case objects.ChannelKeyHealthCheckFailureActionDisable:
		return string(objects.ChannelKeyHealthCheckPolicyActionDisableKey)
	case objects.ChannelKeyHealthCheckFailureActionArchive:
		return string(objects.ChannelKeyHealthCheckPolicyActionArchiveKey)
	case objects.ChannelKeyHealthCheckFailureActionDelete:
		return string(objects.ChannelKeyHealthCheckPolicyActionDeleteKey)
	default:
		return string(objects.ChannelKeyHealthCheckPolicyActionReportOnly)
	}
}

func (svc *ChannelService) disableChannelForKeyHealthPolicy(ctx context.Context, channelID int, policy string) error {
	reason := strings.TrimSpace(policy)
	if reason == "" {
		reason = "health policy"
	}

	if _, err := svc.entFromContext(ctx).Channel.UpdateOneID(channelID).
		SetStatus(channel.StatusDisabled).
		SetErrorMessage(fmt.Sprintf("Channel key health policy disabled channel: %s", reason)).
		Save(ctx); err != nil {
		return fmt.Errorf("failed to disable channel from key health policy: %w", err)
	}

	svc.asyncReloadChannels()

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
