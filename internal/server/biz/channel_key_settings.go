package biz

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/looplj/axonhub/internal/objects"
)

const (
	minChannelKeyHealthCheckIntervalMinutes   = 5
	maxChannelKeyHealthCheckIntervalMinutes   = 10080
	maxChannelKeyHealthCheckFailureThreshold  = 20
	maxChannelKeyHealthCheckHistoryLimit      = 100
	maxChannelKeyHealthCheckHTTPTimeoutMs     = 30000
	maxChannelKeyHealthCheckPassWhenLength    = 1024
	maxChannelKeyHealthCheckBackoffMinutes    = 10080
	maxChannelKeyHealthCheckBackoffMultiplier = 20
)

func ValidateChannelKeySettings(settings *objects.ChannelSettings) error {
	if settings == nil {
		return nil
	}

	if err := ValidateChannelKeySelection(settings.KeySelection); err != nil {
		return err
	}
	if err := ValidateChannelFailurePolicy(settings.FailurePolicy); err != nil {
		return err
	}
	if err := ValidateChannelBalanceProbe(settings.BalanceProbe); err != nil {
		return err
	}

	return ValidateChannelKeyHealthCheck(settings.KeyHealthCheck)
}

func ValidateChannelKeySelection(selection *objects.ChannelKeySelection) error {
	if selection == nil {
		return nil
	}

	switch selection.StrategyOrDefault() {
	case objects.ChannelKeySelectionStrategyTraceSticky,
		objects.ChannelKeySelectionStrategyCacheAffinity,
		objects.ChannelKeySelectionStrategyRandom,
		objects.ChannelKeySelectionStrategyRoundRobin:
	default:
		return fmt.Errorf("unsupported key selection strategy %q", selection.Strategy)
	}

	if selection.LikelyAffinityTTLMinutes != nil {
		ttl := *selection.LikelyAffinityTTLMinutes
		if ttl < objects.MinChannelKeyAffinityTTLMinutes || ttl > objects.MaxChannelKeyLikelyAffinityTTLMinutes {
			return fmt.Errorf("likely affinity TTL minutes must be between %d and %d", objects.MinChannelKeyAffinityTTLMinutes, objects.MaxChannelKeyLikelyAffinityTTLMinutes)
		}
	}

	if selection.ExactAffinityTTLMinutes != nil {
		ttl := *selection.ExactAffinityTTLMinutes
		if ttl < objects.MinChannelKeyAffinityTTLMinutes || ttl > objects.MaxChannelKeyExactAffinityTTLMinutes {
			return fmt.Errorf("exact affinity TTL minutes must be between %d and %d", objects.MinChannelKeyAffinityTTLMinutes, objects.MaxChannelKeyExactAffinityTTLMinutes)
		}
	}

	return nil
}

func ValidateChannelKeyHealthCheck(health *objects.ChannelKeyHealthCheck) error {
	if health == nil {
		return nil
	}

	interval := health.IntervalMinutesOrDefault()
	if interval < minChannelKeyHealthCheckIntervalMinutes || interval > maxChannelKeyHealthCheckIntervalMinutes {
		return fmt.Errorf("interval minutes must be between %d and %d", minChannelKeyHealthCheckIntervalMinutes, maxChannelKeyHealthCheckIntervalMinutes)
	}

	threshold := health.FailureThresholdOrDefault()
	if threshold < 1 || threshold > maxChannelKeyHealthCheckFailureThreshold {
		return fmt.Errorf("failure threshold must be between 1 and %d", maxChannelKeyHealthCheckFailureThreshold)
	}
	if health.HistoryLimit < 0 || health.HistoryLimit > maxChannelKeyHealthCheckHistoryLimit {
		return fmt.Errorf("history limit must be between 1 and %d", maxChannelKeyHealthCheckHistoryLimit)
	}

	switch health.FailureActionOrDefault() {
	case objects.ChannelKeyHealthCheckFailureActionReportOnly,
		objects.ChannelKeyHealthCheckFailureActionDisable,
		objects.ChannelKeyHealthCheckFailureActionArchive,
		objects.ChannelKeyHealthCheckFailureActionDelete:
	default:
		return fmt.Errorf("unsupported failure action %q", health.FailureAction)
	}

	for i := range health.Rules {
		if err := validateChannelKeyHealthCheckRule(health.Rules[i]); err != nil {
			return fmt.Errorf("invalid rule %d: %w", i, err)
		}
	}
	for i := range health.Policies {
		if err := validateChannelKeyHealthCheckPolicy(health.Policies[i]); err != nil {
			return fmt.Errorf("invalid policy %d: %w", i, err)
		}
	}

	return nil
}

func validateChannelKeyHealthCheckPolicy(policy objects.ChannelKeyHealthCheckPolicy) error {
	if strings.TrimSpace(policy.ID) == "" {
		return fmt.Errorf("id is required")
	}
	if strings.TrimSpace(policy.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if policy.Conditions.MinFailureCount != nil && *policy.Conditions.MinFailureCount < 1 {
		return fmt.Errorf("minFailureCount must be at least 1")
	}
	for _, status := range policy.Conditions.StatusCodes {
		if status < 100 || status > 599 {
			return fmt.Errorf("status code %d is outside HTTP status range", status)
		}
	}
	if len(strings.TrimSpace(policy.Conditions.Expr)) > maxChannelKeyHealthCheckPassWhenLength {
		return fmt.Errorf("expr must be at most %d characters", maxChannelKeyHealthCheckPassWhenLength)
	}
	if len(policy.Actions) == 0 {
		return fmt.Errorf("at least one action is required")
	}
	for i, action := range policy.Actions {
		if err := validateChannelKeyHealthCheckPolicyAction(action); err != nil {
			return fmt.Errorf("invalid action %d: %w", i, err)
		}
	}

	return nil
}

func validateChannelKeyHealthCheckPolicyAction(action objects.ChannelKeyHealthCheckPolicyAction) error {
	switch action.Type {
	case objects.ChannelKeyHealthCheckPolicyActionReportOnly,
		objects.ChannelKeyHealthCheckPolicyActionDisableKey,
		objects.ChannelKeyHealthCheckPolicyActionArchiveKey,
		objects.ChannelKeyHealthCheckPolicyActionDeleteKey,
		objects.ChannelKeyHealthCheckPolicyActionDisableChannel,
		objects.ChannelKeyHealthCheckPolicyActionEnableKey,
		objects.ChannelKeyHealthCheckPolicyActionRestoreKey:
		return nil
	case objects.ChannelKeyHealthCheckPolicyActionBackoff:
		if action.Backoff == nil {
			return fmt.Errorf("backoff is required for backoff action")
		}
		return validateChannelKeyHealthCheckBackoff(*action.Backoff)
	default:
		return fmt.Errorf("unsupported action %q", action.Type)
	}
}

func validateChannelKeyHealthCheckBackoff(backoff objects.ChannelKeyHealthCheckBackoff) error {
	switch backoff.Mode {
	case "", objects.ChannelKeyHealthCheckBackoffModeFixed, objects.ChannelKeyHealthCheckBackoffModeExponential:
	default:
		return fmt.Errorf("unsupported backoff mode %q", backoff.Mode)
	}
	if backoff.IntervalMinutes < 0 || backoff.IntervalMinutes > maxChannelKeyHealthCheckBackoffMinutes {
		return fmt.Errorf("backoff intervalMinutes must be between 1 and %d", maxChannelKeyHealthCheckBackoffMinutes)
	}
	if backoff.MaxIntervalMinutes < 0 || backoff.MaxIntervalMinutes > maxChannelKeyHealthCheckBackoffMinutes {
		return fmt.Errorf("backoff maxIntervalMinutes must be between 1 and %d", maxChannelKeyHealthCheckBackoffMinutes)
	}
	if backoff.Multiplier < 0 || backoff.Multiplier > maxChannelKeyHealthCheckBackoffMultiplier {
		return fmt.Errorf("backoff multiplier must be between 1 and %d", maxChannelKeyHealthCheckBackoffMultiplier)
	}

	return nil
}

func ValidateChannelBalanceProbe(probe *objects.ChannelBalanceProbe) error {
	if probe == nil {
		return nil
	}

	if strings.TrimSpace(probe.PreferredCurrency) != "" && !isValidBalanceCurrencyCode(probe.PreferredCurrency) {
		return fmt.Errorf("preferredCurrency must be a valid currency code")
	}
	switch probe.PrimarySelection {
	case "", objects.ChannelBalancePrimarySelectionAutoHighest, objects.ChannelBalancePrimarySelectionPreferredCurrency:
	default:
		return fmt.Errorf("unsupported primary selection %q", probe.PrimarySelection)
	}
	for _, status := range probe.IncludeStatuses {
		switch status {
		case objects.ChannelKeyStatusActive, objects.ChannelKeyStatusDisabled, objects.ChannelKeyStatusArchived:
		default:
			return fmt.Errorf("unsupported balance probe key status %q", status)
		}
	}
	if probe.TimeoutMs < 0 || probe.TimeoutMs > maxChannelKeyHealthCheckHTTPTimeoutMs {
		return fmt.Errorf("balance probe timeoutMs must be between 0 and %d", maxChannelKeyHealthCheckHTTPTimeoutMs)
	}
	if probe.Preset != "" {
		if probe.Preset != objects.ChannelBalanceProbePresetCustom {
			spec, ok := channelBalanceProbePresetByID(probe.Preset)
			if !ok {
				return fmt.Errorf("unsupported balance probe preset %q", probe.Preset)
			}
			if spec.Experimental && !probe.Experimental {
				return fmt.Errorf("balance probe preset %q is experimental and requires explicit opt-in", probe.Preset)
			}
		}
	}
	if probe.HTTP != nil {
		if err := validateChannelKeyHealthCheckHTTPRule(*probe.HTTP); err != nil {
			return fmt.Errorf("invalid balance probe http override: %w", err)
		}
	}

	return nil
}

func isValidBalanceCurrencyCode(code string) bool {
	code = strings.TrimSpace(code)
	if code == "" || len(code) > 16 {
		return false
	}
	for _, r := range code {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}

		return false
	}

	return true
}

func validateChannelKeyHealthCheckRule(rule objects.ChannelKeyHealthCheckRule) error {
	if strings.TrimSpace(rule.ID) == "" {
		return fmt.Errorf("id is required")
	}
	if strings.TrimSpace(rule.Name) == "" {
		return fmt.Errorf("name is required")
	}

	switch rule.Type {
	case objects.ChannelKeyHealthCheckRuleTypeBuiltinTest:
		if rule.Builtin == nil || strings.TrimSpace(rule.Builtin.Kind) == "" {
			return fmt.Errorf("builtin rule kind is required")
		}
	case objects.ChannelKeyHealthCheckRuleTypeHTTP:
		if rule.HTTP == nil {
			return fmt.Errorf("http rule is required")
		}
		if err := validateChannelKeyHealthCheckHTTPRule(*rule.HTTP); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported rule type %q", rule.Type)
	}

	return nil
}

func validateChannelKeyHealthCheckHTTPRule(rule objects.ChannelKeyHealthCheckHTTPRule) error {
	switch rule.Method {
	case "", objects.ChannelKeyHealthCheckHTTPMethodGet,
		objects.ChannelKeyHealthCheckHTTPMethodPost,
		objects.ChannelKeyHealthCheckHTTPMethodPut,
		objects.ChannelKeyHealthCheckHTTPMethodPatch,
		objects.ChannelKeyHealthCheckHTTPMethodDelete:
	default:
		return fmt.Errorf("unsupported http method %q", rule.Method)
	}

	switch rule.URLMode {
	case "", objects.ChannelKeyHealthCheckHTTPURLModeProviderBaseURL:
		if strings.TrimSpace(rule.Path) == "" {
			return fmt.Errorf("path is required for provider_base_url rules")
		}
		if strings.Contains(rule.Path, "://") {
			return fmt.Errorf("path must not be an absolute URL")
		}
	case objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL:
		if _, err := validateChannelKeyHealthCheckAbsoluteURLStatic(rule.URL); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported url mode %q", rule.URLMode)
	}

	if rule.TimeoutMs < 0 || rule.TimeoutMs > maxChannelKeyHealthCheckHTTPTimeoutMs {
		return fmt.Errorf("timeoutMs must be between 0 and %d", maxChannelKeyHealthCheckHTTPTimeoutMs)
	}

	if rule.KeyInjection != nil {
		switch rule.KeyInjection.Location {
		case "", objects.ChannelKeyHealthCheckKeyInjectionAuthorizationBearer:
		case objects.ChannelKeyHealthCheckKeyInjectionHeader:
			headerName := strings.TrimSpace(rule.KeyInjection.HeaderName)
			if headerName == "" {
				return fmt.Errorf("header name is required for header key injection")
			}
			if !isValidHTTPHeaderName(headerName) {
				return fmt.Errorf("header name is invalid")
			}
		default:
			return fmt.Errorf("unsupported key injection location %q", rule.KeyInjection.Location)
		}
	}

	for _, header := range rule.Headers {
		name := strings.TrimSpace(header.Key)
		if name == "" {
			continue
		}
		if !isValidHTTPHeaderName(name) {
			return fmt.Errorf("header name %q is invalid", name)
		}
	}

	for _, status := range rule.ExpectedStatuses {
		if status < 100 || status > 599 {
			return fmt.Errorf("expected status %d is outside HTTP status range", status)
		}
	}

	if len(strings.TrimSpace(rule.PassWhen)) > maxChannelKeyHealthCheckPassWhenLength {
		return fmt.Errorf("passWhen must be at most %d characters", maxChannelKeyHealthCheckPassWhenLength)
	}

	return nil
}

func ValidateChannelFailurePolicy(policy *objects.ChannelFailurePolicy) error {
	if policy == nil {
		return nil
	}

	switch policy.Mode {
	case "", objects.ChannelFailurePolicyModeInherit,
		objects.ChannelFailurePolicyModeOverride,
		objects.ChannelFailurePolicyModeMerge,
		objects.ChannelFailurePolicyModeDisabled:
	default:
		return fmt.Errorf("unsupported failure policy mode %q", policy.Mode)
	}
	for i, profile := range policy.KeyProfiles {
		if err := validateFailurePolicyProfile(profile); err != nil {
			return fmt.Errorf("invalid key failure profile %d: %w", i, err)
		}
	}
	for i, profile := range policy.ChannelProfiles {
		if err := validateFailurePolicyProfile(profile); err != nil {
			return fmt.Errorf("invalid channel failure profile %d: %w", i, err)
		}
	}

	return nil
}

func validateFailurePolicyProfile(profile objects.FailurePolicyProfile) error {
	if strings.TrimSpace(profile.ID) == "" {
		return fmt.Errorf("id is required")
	}
	if strings.TrimSpace(profile.Name) == "" {
		return fmt.Errorf("name is required")
	}
	if profile.Conditions.MinFailureCount != nil && *profile.Conditions.MinFailureCount < 1 {
		return fmt.Errorf("minFailureCount must be at least 1")
	}
	for _, status := range profile.Conditions.StatusCodes {
		if status < 100 || status > 599 {
			return fmt.Errorf("status code %d is outside HTTP status range", status)
		}
	}
	if len(strings.TrimSpace(profile.Conditions.Expr)) > maxChannelKeyHealthCheckPassWhenLength {
		return fmt.Errorf("expr must be at most %d characters", maxChannelKeyHealthCheckPassWhenLength)
	}
	for _, source := range profile.Sources {
		switch source {
		case objects.FailurePolicyEventSourceRequestFailure,
			objects.FailurePolicyEventSourceScheduledHealthCheckFailure,
			objects.FailurePolicyEventSourceManualHealthCheckFailure,
			objects.FailurePolicyEventSourceScheduledHealthCheck,
			objects.FailurePolicyEventSourceManualHealthCheck,
			objects.FailurePolicyEventSourceScheduledBalanceProbe,
			objects.FailurePolicyEventSourceScheduledBalanceProbeFailure,
			objects.FailurePolicyEventSourceManualBalanceProbe,
			objects.FailurePolicyEventSourceManualBalanceProbeFailure:
		default:
			return fmt.Errorf("unsupported source %q", source)
		}
	}
	if len(profile.Actions) == 0 {
		return fmt.Errorf("at least one action is required")
	}
	for i, action := range profile.Actions {
		if err := validateFailurePolicyAction(action); err != nil {
			return fmt.Errorf("invalid action %d: %w", i, err)
		}
	}

	return nil
}

func validateFailurePolicyAction(action objects.FailurePolicyAction) error {
	switch action.Type {
	case objects.FailurePolicyActionReportOnly,
		objects.FailurePolicyActionDisableKey,
		objects.FailurePolicyActionArchiveKey,
		objects.FailurePolicyActionDeleteKey,
		objects.FailurePolicyActionDisableChannel,
		objects.FailurePolicyActionEnableKey,
		objects.FailurePolicyActionRestoreKey:
		return nil
	case objects.FailurePolicyActionBackoffKey:
		if action.Backoff == nil {
			return fmt.Errorf("backoff is required for backoff_key action")
		}
		return validateChannelKeyHealthCheckBackoff(*action.Backoff)
	default:
		return fmt.Errorf("unsupported action %q", action.Type)
	}
}

func validateChannelKeyHealthCheckURLsForBaseURL(baseURL string, settings *objects.ChannelSettings) error {
	if settings == nil {
		return nil
	}

	if settings.KeyHealthCheck != nil {
		for _, rule := range settings.KeyHealthCheck.Rules {
			if rule.Type != objects.ChannelKeyHealthCheckRuleTypeHTTP || rule.HTTP == nil {
				continue
			}
			if rule.HTTP.URLMode != objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL {
				continue
			}

			if err := validateChannelKeyHealthCheckAbsoluteURLMatchesBaseURL(baseURL, rule.HTTP.URL); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateChannelKeyHealthCheckAbsoluteURLStatic(rawURL string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("valid absolute url is required")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("absolute url scheme must be http or https")
	}
	if u.User != nil {
		return nil, fmt.Errorf("absolute url must not contain user info")
	}

	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return nil, fmt.Errorf("absolute url host is required")
	}
	if isBlockedChannelKeyHealthCheckHostname(host) {
		return nil, fmt.Errorf("absolute url host is not allowed")
	}
	if ip := net.ParseIP(host); ip != nil && isBlockedChannelKeyHealthCheckIP(ip) {
		return nil, fmt.Errorf("absolute url host is not allowed")
	}

	return u, nil
}

func validateChannelKeyHealthCheckAbsoluteURLMatchesBaseURL(baseURL string, rawURL string) error {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return fmt.Errorf("invalid provider base url")
	}

	target, err := validateChannelKeyHealthCheckAbsoluteURLStatic(rawURL)
	if err != nil {
		return err
	}

	if !strings.EqualFold(base.Scheme, target.Scheme) ||
		!strings.EqualFold(base.Hostname(), target.Hostname()) ||
		normalizedURLPort(base) != normalizedURLPort(target) {
		return fmt.Errorf("absolute url must match provider base origin")
	}

	return nil
}

func normalizedURLPort(u *url.URL) string {
	if u == nil {
		return ""
	}
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(strings.TrimSpace(u.Scheme)) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func isBlockedChannelKeyHealthCheckHostname(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	switch host {
	case "", "localhost", "localhost.localdomain",
		"metadata", "metadata.google.internal", "metadata.google.internal.", "instance-data", "169.254.169.254":
		return true
	default:
		return strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local")
	}
}

func isBlockedChannelKeyHealthCheckIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified()
}

func isValidHTTPHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isHTTPTokenChar(name[i]) {
			return false
		}
	}

	return true
}

func isHTTPTokenChar(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z':
		return true
	case c >= 'A' && c <= 'Z':
		return true
	case c >= '0' && c <= '9':
		return true
	default:
		return strings.ContainsRune("!#$%&'*+-.^_`|~", rune(c))
	}
}

func channelArchivedAPIKeys(settings *objects.ChannelSettings) []objects.ChannelArchivedAPIKey {
	if settings == nil || settings.KeyHealthCheck == nil {
		return nil
	}

	return settings.KeyHealthCheck.ArchivedKeys
}

func ChannelArchivedAPIKeys(settings *objects.ChannelSettings) []objects.ChannelArchivedAPIKey {
	return channelArchivedAPIKeys(settings)
}

func routableChannelAPIKeys(credentials objects.ChannelCredentials, disabledKeys []objects.DisabledAPIKey, settings *objects.ChannelSettings, now time.Time) []string {
	keys := credentials.GetRoutableAPIKeys(disabledKeys, channelArchivedAPIKeys(settings))
	if len(keys) == 0 || settings == nil || settings.KeyHealthCheck == nil {
		return keys
	}

	backedOff := channelBackedOffAPIKeyIDs(settings, now)
	if len(backedOff) == 0 {
		return keys
	}

	next := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, ok := backedOff[objects.ChannelAPIKeyFingerprint(key)]; ok {
			continue
		}
		next = append(next, key)
	}

	return next
}

func channelBackedOffAPIKeyIDs(settings *objects.ChannelSettings, now time.Time) map[string]struct{} {
	if settings == nil || settings.KeyHealthCheck == nil {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}

	backedOff := make(map[string]struct{})
	for _, meta := range settings.KeyHealthCheck.KeyMetadata {
		if meta.ID == "" || meta.NextCheckAt == nil {
			continue
		}
		if now.Before(*meta.NextCheckAt) {
			backedOff[meta.ID] = struct{}{}
		}
	}

	return backedOff
}
