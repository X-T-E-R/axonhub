package provider_quota

import (
	"context"
	"time"

	"github.com/looplj/axonhub/internal/ent"
)

// QuotaChecker checks quota status for a provider.
type QuotaChecker interface {
	// CheckQuota makes a minimal API request to get quota information and returns parsed quota data
	CheckQuota(ctx context.Context, channel *ent.Channel) (QuotaData, error)
	// SupportsChannel returns true if this checker supports the channel
	SupportsChannel(channel *ent.Channel) bool
}

// InferenceProbeQuotaChecker is implemented by checkers whose CheckQuota sends
// a real inference request (for example ClaudeCode POST /v1/messages max_tokens:1).
// Read-only quota refresh entrypoints MUST NOT invoke inference-probe checkers.
type InferenceProbeQuotaChecker interface {
	IsInferenceProbe() bool
}

// IsReadOnlyQuotaChecker reports whether checker retrieves quota via a read-only
// endpoint without sending a billable/rate-limited inference request.
func IsReadOnlyQuotaChecker(checker QuotaChecker) bool {
	if checker == nil {
		return false
	}
	if probe, ok := checker.(InferenceProbeQuotaChecker); ok && probe.IsInferenceProbe() {
		return false
	}
	return true
}

type QuotaLimitType string

const (
	QuotaLimitTypeImage             QuotaLimitType = "image"
	QuotaLimitTypeToken             QuotaLimitType = "token"
	QuotaLimitTypeSubscriptionCycle QuotaLimitType = "subscription_cycle"
)

// ApertisDefaultBaseURL is the default base URL for the Apertis API.
const ApertisDefaultBaseURL = "https://api.apertis.ai"

type QuotaLimitStatus struct {
	Type        QuotaLimitType `json:"type"`
	Status      string         `json:"status"`
	UsageRatio  float64        `json:"usage_ratio"`
	Ready       bool           `json:"ready"`
	NextResetAt *time.Time     `json:"next_reset_at"`
}

// QuotaData is the unified quota data structure.
type QuotaData struct {
	Status       string             `json:"status"` // available, warning, exhausted, unknown
	ProviderType string             `json:"provider_type"`
	RawData      map[string]any     `json:"raw_data"`
	NextResetAt  *time.Time         `json:"next_reset_at"` // Next quota reset timestamp
	Ready        bool               `json:"ready"`         // True if status is available or warning
	Limits       []QuotaLimitStatus `json:"limits"`
}

// WarningThresholdRatio is the usage ratio at which a channel transitions to "warning" status.
const WarningThresholdRatio = 0.8

func RequestModality(isImageRequest bool) QuotaLimitType {
	if isImageRequest {
		return QuotaLimitTypeImage
	}
	return QuotaLimitTypeToken
}

func IsReadyStatus(status string) bool {
	return status == "available" || status == "warning"
}

func NewTokenLimitStatus(status string, usageRatio float64, nextResetAt *time.Time) QuotaLimitStatus {
	return QuotaLimitStatus{
		Type:        QuotaLimitTypeToken,
		Status:      status,
		UsageRatio:  usageRatio,
		Ready:       IsReadyStatus(status),
		NextResetAt: nextResetAt,
	}
}
