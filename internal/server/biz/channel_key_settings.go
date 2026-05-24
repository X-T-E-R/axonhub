package biz

import (
	"fmt"
	"net/url"
	"strings"

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

	if settings.KeySelection != nil {
		switch settings.KeySelection.StrategyOrDefault() {
		case objects.ChannelKeySelectionStrategyTraceSticky,
			objects.ChannelKeySelectionStrategyCacheAffinity,
			objects.ChannelKeySelectionStrategyRandom,
			objects.ChannelKeySelectionStrategyRoundRobin:
		default:
			return fmt.Errorf("unsupported key selection strategy %q", settings.KeySelection.Strategy)
		}
	}

	return ValidateChannelKeyHealthCheck(settings.KeyHealthCheck)
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
		objects.ChannelKeyHealthCheckPolicyActionDisableChannel:
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
		u, err := url.Parse(strings.TrimSpace(rule.URL))
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("valid absolute url is required")
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("absolute url scheme must be http or https")
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
			if strings.TrimSpace(rule.KeyInjection.HeaderName) == "" {
				return fmt.Errorf("header name is required for header key injection")
			}
		default:
			return fmt.Errorf("unsupported key injection location %q", rule.KeyInjection.Location)
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

func channelArchivedAPIKeys(settings *objects.ChannelSettings) []objects.ChannelArchivedAPIKey {
	if settings == nil || settings.KeyHealthCheck == nil {
		return nil
	}

	return settings.KeyHealthCheck.ArchivedKeys
}

func ChannelArchivedAPIKeys(settings *objects.ChannelSettings) []objects.ChannelArchivedAPIKey {
	return channelArchivedAPIKeys(settings)
}
