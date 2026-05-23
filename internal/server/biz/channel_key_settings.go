package biz

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/looplj/axonhub/internal/objects"
)

const (
	minChannelKeyHealthCheckIntervalMinutes  = 5
	maxChannelKeyHealthCheckIntervalMinutes  = 10080
	maxChannelKeyHealthCheckFailureThreshold = 20
	maxChannelKeyHealthCheckHTTPTimeoutMs    = 30000
	maxChannelKeyHealthCheckPassWhenLength   = 1024
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
