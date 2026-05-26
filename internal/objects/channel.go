package objects

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/oauth"
)

// ChannelEndpoint represents an outbound API endpoint configuration within a Channel.
// Each endpoint specifies the upstream API format and an optional custom path override.
// Within a single channel, api_format must be unique.
type ChannelEndpoint struct {
	APIFormat string `json:"api_format"`
	Path      string `json:"path,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
	Transport string `json:"transport,omitempty"`
}

const (
	ChannelEndpointTransportHTTP      = "http"
	ChannelEndpointTransportWebSocket = "websocket"
)

type (
	ProxyType   = httpclient.ProxyType
	ProxyConfig = httpclient.ProxyConfig
)

type ModelMapping struct {
	// From is the model name in the request.
	From string `json:"from"`

	// To is the model name in the provider.
	To string `json:"to"`
}

type HeaderEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Override operation types.
const (
	OverrideOpSet          = "set"
	OverrideOpSetIfAbsent  = "set_if_absent"
	OverrideOpDelete       = "delete"
	OverrideOpRename       = "rename"
	OverrideOpCopy         = "copy"
	OverrideOpArrayAppend  = "array_append"
	OverrideOpArrayPrepend = "array_prepend"
	OverrideOpArrayInsert  = "array_insert"
	OverrideOpArrayRemove  = "array_remove"
)

// OverrideMatch defines a simple equality matcher for array_remove operations.
type OverrideMatch struct {
	// Path is resolved relative to each array item.
	Path string `json:"path"`
	// Eq is the value that removes the item when it matches.
	Eq string `json:"eq"`
}

// OverrideOperation defines a structured override operation for request body/header manipulation.
type OverrideOperation struct {
	Op        string `json:"op"`
	Path      string `json:"path,omitempty"`
	From      string `json:"from,omitempty"`
	To        string `json:"to,omitempty"`
	Value     string `json:"value,omitempty"`
	Condition string `json:"condition,omitempty"`
	// Match identifies array items removed by array_remove.
	Match *OverrideMatch `json:"match,omitempty"`
	// Index is the target position for array_insert. Only used by array_insert.
	// Negative values count from the end (-1 = before last). Out-of-range values are clamped to [0, len].
	Index *int `json:"index,omitempty"`
	// Splat controls whether a JSON-array value is spread into the target array
	// (true: each element inserted individually) or inserted as a single nested element (false).
	// Only meaningful for array_append, array_prepend, and array_insert. Defaults to true.
	Splat *bool `json:"splat,omitempty"`
}

func HeaderEntriesToOverrideOperations(headers []HeaderEntry) []OverrideOperation {
	if len(headers) == 0 {
		return nil
	}

	ops := make([]OverrideOperation, 0, len(headers))
	for _, header := range headers {
		if header.Value == "__AXONHUB_CLEAR__" {
			ops = append(ops, OverrideOperation{Op: OverrideOpDelete, Path: header.Key})
			continue
		}

		ops = append(ops, OverrideOperation{Op: OverrideOpSet, Path: header.Key, Value: header.Value})
	}

	return ops
}

type TransformOptions struct {
	// ForceArrayInstructions forces the channel to accept array format for instructions.
	ForceArrayInstructions bool `json:"forceArrayInstructions"`

	// ForceArrayInputs forces the channel to accept array format for inputs.
	ForceArrayInputs bool `json:"forceArrayInputs"`

	// ReplaceDeveloperRoleWithSystem replaces developer role with system in messages for Bailian compatibility.
	ReplaceDeveloperRoleWithSystem bool `json:"replaceDeveloperRoleWithSystem"`

	// ReasoningEffortMapping maps inbound reasoning_effort values to outbound ones for
	// non-standard OpenAI-compatible providers. The first entry whose From matches the
	// effort value wins; values not in the list pass through unchanged.
	// e.g. [{"from":"xhigh","to":"max"}] converts Anthropic's internal "xhigh" (mapped
	// from "max") back to "max" for providers that only recognize "max".
	// Consumed by the OpenAI-shared outbound transformer. Other transformers ignore it
	// for now. Strong-typed to mirror ModelMapping; see llm.ReasoningEffortMapping.
	ReasoningEffortMapping []llm.ReasoningEffortMapping `json:"reasoningEffortMapping,omitempty"`
}

type ChannelSettings struct {
	// ExtraModelPrefix sets the channel accept the model with the extra prefix.
	// e.g. a channel
	// supported_modles is ["deepseek-chat", "deepseek-reasoner"]
	// extraModelPrefix is "deepseek"
	// then the model "deepseek-chat", "deepseek-reasoner", "deepseek/deepseek-chat", "deepseek/deepseek-reasoner"  will be accepted.
	// And if other channel support "deepseek/deepseek-chat", "deepseek/deepseek-reasoner" modles, the two channels can accept the request both.
	ExtraModelPrefix string `json:"extraModelPrefix"`

	// AutoTrimedModelPrefixes configures prefixes to automatically trim the model name when added to supported models.
	// e.g. a channel
	// supported_modles is ["deepseek-ai/deepseek-chat", "openai/gpt-4"]
	// autoTrimedModelPrefixes is ["openai", "deepseek"]
	// then the model "openai/gpt-4", "deepseek/deepseek-chat", "deepseek-chat", "gpt-4" will be accepted.
	AutoTrimedModelPrefixes []string `json:"autoTrimedModelPrefixes"`

	// ModelMappings add model alias for the model in the channels.
	// e.g. {"from": "deepseek-chat", "to": "deepseek/deepseek-chat"} will add a alias "deepseek-chat" for "deepseek/deepseek-chat".
	ModelMappings []ModelMapping `json:"modelMappings"`

	// HideOriginalModels hides the original models from the model list when model mappings are configured.
	// When enabled, only the mapped model names (from field) will be exposed, not the actual model names (to field).
	HideOriginalModels bool `json:"hideOriginalModels"`

	// HideMappedModels hides the mapped models from the model list when model mappings are configured.
	// When enabled, only the original model names (from field) will be exposed, not the mapped model names (to field).
	HideMappedModels bool `json:"hideMappedModels"`

	// LowercaseModelID converts model name matching keys to lowercase.
	// When enabled, only RequestModel (used for matching) is lowercased; ActualModel
	// (sent to provider) preserves original casing. This enables cross-channel load
	// balancing where providers use different casing for the same model.
	LowercaseModelID bool `json:"lowercaseModelId"`

	// OverrideParameters sets the channel override the request body.
	// A json string.
	// e.g. {"max_tokens": 100}, {"temperature": 0.7}
	// Deprecated Use bodyOverrideOperations instead.
	OverrideParameters string `json:"overrideParameters"`

	// BodyOverrideOperations sets the channel override operations for the request body.
	// When present (including an empty array), it takes precedence over OverrideParameters.
	BodyOverrideOperations []OverrideOperation `json:"bodyOverrideOperations,omitempty"`

	// OverrideHeaders sets the channel override the request headers.
	// e.g. [{"key": "User-Agent", "value": "AxonHub"}]
	// Supported ops: set (default), delete, rename, copy.
	// Deprecated Use headerOverrideOperations instead.
	OverrideHeaders []HeaderEntry `json:"overrideHeaders"`

	// HeaderOverrideOperations sets the channel override operations for request headers.
	// When present (including an empty array), it takes precedence over OverrideHeaders.
	HeaderOverrideOperations []OverrideOperation `json:"headerOverrideOperations,omitempty"`

	// Proxy configuration for the channel. If not set, defaults to environment proxy type.
	Proxy *httpclient.ProxyConfig `json:"proxy,omitempty"`

	// TransformOptions configures the transform options for the channel.
	TransformOptions TransformOptions `json:"transformOptions"`

	// PassThroughUserAgent controls whether to pass through the original User-Agent header to upstream AI providers.
	// When set to nil, it inherits from the global system setting.
	// When set to true/false, it overrides the global setting.
	PassThroughUserAgent *bool `json:"passThroughUserAgent,omitempty"`

	// PassThroughBody controls whether to forward the original request body directly
	// to the upstream provider and the raw provider response/stream directly to the client
	// without re-serialization through the transform pipelines.
	// Only effective when the inbound and outbound API formats are identical.
	// When set to nil, it inherits from the global system setting.
	// When set to true/false, it overrides the global setting.
	PassThroughBody *bool `json:"passThroughBody,omitempty"`

	// DisableRetries disables both same-channel retries and fallback to the next
	// channel for failures on this channel. Upstream is responsible for retrying.
	DisableRetries bool `json:"disableRetries,omitempty"`

	// FullPassThrough preserves the inbound AxonHub request path/query/method/body
	// and raw response for trusted AxonHub-to-AxonHub upstream boundaries.
	FullPassThrough bool `json:"fullPassThrough,omitempty"`

	// RateLimit configures the upstream rate limit for the channel.
	// When configured, the load balancer will skip channels that have exceeded their rate limits.
	RateLimit *ChannelRateLimit `json:"rateLimit,omitempty"`

	// RetryableStatusCodes configures additional HTTP status codes that should
	// trigger retry for this channel. Default retryable codes (429 and 5xx) are
	// always handled by the retry policy even when this list is empty.
	RetryableStatusCodes []int `json:"retryableStatusCodes,omitempty"`

	// RetryableErrorPatterns configures additional error text patterns that should
	// trigger retry for this channel. When Regex is false, Pattern is matched as a
	// case-sensitive substring of the error text.
	RetryableErrorPatterns []RetryableErrorPattern `json:"retryableErrorPatterns,omitempty"`

	// ProviderQuota stores provider-specific credentials used only for quota
	// polling. Keep upstream request credentials in ChannelCredentials.
	ProviderQuota *ChannelProviderQuotaSettings `json:"providerQuota,omitempty"`

	// KeySelection configures how multiple credential API keys are selected.
	// Defaults to trace_sticky for backward compatibility.
	KeySelection *ChannelKeySelection `json:"keySelection,omitempty"`

	// KeyHealthCheck configures per-channel credential key health checks and
	// stores secret-safe key metadata for the channel keys panel.
	KeyHealthCheck *ChannelKeyHealthCheck `json:"keyHealthCheck,omitempty"`

	// FailurePolicy configures request-time and health-check failure reactions.
	FailurePolicy *ChannelFailurePolicy `json:"failurePolicy,omitempty"`
}

type RetryableErrorPattern struct {
	Pattern string `json:"pattern"`
	Regex   bool   `json:"regex,omitempty"`
}

type ChannelProviderQuotaSettings struct {
	OpencodeGo *OpenCodeGoQuotaSettings `json:"opencodeGo,omitempty"`
}

type OpenCodeGoQuotaSettings struct {
	WorkspaceID string `json:"workspaceId,omitempty"`
	AuthCookie  string `json:"authCookie,omitempty"`
}

type ChannelKeySelectionStrategy string

const (
	ChannelKeySelectionStrategyTraceSticky   ChannelKeySelectionStrategy = "trace_sticky"
	ChannelKeySelectionStrategyCacheAffinity ChannelKeySelectionStrategy = "cache_affinity"
	ChannelKeySelectionStrategyRandom        ChannelKeySelectionStrategy = "random"
	ChannelKeySelectionStrategyRoundRobin    ChannelKeySelectionStrategy = "round_robin"
)

type ChannelKeySelection struct {
	Strategy                 ChannelKeySelectionStrategy `json:"strategy,omitempty"`
	LikelyAffinityTTLMinutes *int                        `json:"likelyAffinityTTLMinutes,omitempty"`
	ExactAffinityTTLMinutes  *int                        `json:"exactAffinityTTLMinutes,omitempty"`
}

func (s *ChannelKeySelection) StrategyOrDefault() ChannelKeySelectionStrategy {
	if s == nil || s.Strategy == "" {
		return ChannelKeySelectionStrategyTraceSticky
	}

	return s.Strategy
}

const (
	DefaultChannelKeyLikelyAffinityTTLMinutes = 30
	DefaultChannelKeyExactAffinityTTLMinutes  = 1440
	MinChannelKeyAffinityTTLMinutes           = 1
	MaxChannelKeyLikelyAffinityTTLMinutes     = 1440
	MaxChannelKeyExactAffinityTTLMinutes      = 10080
)

func (s *ChannelKeySelection) LikelyAffinityTTLMinutesOrDefault() int {
	if s == nil || s.LikelyAffinityTTLMinutes == nil {
		return DefaultChannelKeyLikelyAffinityTTLMinutes
	}

	return *s.LikelyAffinityTTLMinutes
}

func (s *ChannelKeySelection) ExactAffinityTTLMinutesOrDefault() int {
	if s == nil || s.ExactAffinityTTLMinutes == nil {
		return DefaultChannelKeyExactAffinityTTLMinutes
	}

	return *s.ExactAffinityTTLMinutes
}

type ChannelKeyHealthCheckFailureAction string

const (
	ChannelKeyHealthCheckFailureActionReportOnly ChannelKeyHealthCheckFailureAction = "report_only"
	ChannelKeyHealthCheckFailureActionDisable    ChannelKeyHealthCheckFailureAction = "disable"
	ChannelKeyHealthCheckFailureActionArchive    ChannelKeyHealthCheckFailureAction = "archive"
	ChannelKeyHealthCheckFailureActionDelete     ChannelKeyHealthCheckFailureAction = "delete"
)

type ChannelKeyHealthCheckPolicyActionType string

const (
	ChannelKeyHealthCheckPolicyActionReportOnly     ChannelKeyHealthCheckPolicyActionType = "report_only"
	ChannelKeyHealthCheckPolicyActionDisableKey     ChannelKeyHealthCheckPolicyActionType = "disable_key"
	ChannelKeyHealthCheckPolicyActionArchiveKey     ChannelKeyHealthCheckPolicyActionType = "archive_key"
	ChannelKeyHealthCheckPolicyActionDeleteKey      ChannelKeyHealthCheckPolicyActionType = "delete_key"
	ChannelKeyHealthCheckPolicyActionDisableChannel ChannelKeyHealthCheckPolicyActionType = "disable_channel"
	ChannelKeyHealthCheckPolicyActionBackoff        ChannelKeyHealthCheckPolicyActionType = "backoff"
	ChannelKeyHealthCheckPolicyActionEnableKey      ChannelKeyHealthCheckPolicyActionType = "enable_key"
	ChannelKeyHealthCheckPolicyActionRestoreKey     ChannelKeyHealthCheckPolicyActionType = "restore_key"
)

type ChannelKeyHealthCheckBackoffMode string

const (
	ChannelKeyHealthCheckBackoffModeFixed       ChannelKeyHealthCheckBackoffMode = "fixed"
	ChannelKeyHealthCheckBackoffModeExponential ChannelKeyHealthCheckBackoffMode = "exponential"
)

type ChannelKeyHealthCheckRuleType string

const (
	ChannelKeyHealthCheckRuleTypeBuiltinTest ChannelKeyHealthCheckRuleType = "builtin_test"
	ChannelKeyHealthCheckRuleTypeHTTP        ChannelKeyHealthCheckRuleType = "http"
)

type ChannelKeyHealthCheckHTTPMethod string

const (
	ChannelKeyHealthCheckHTTPMethodGet    ChannelKeyHealthCheckHTTPMethod = "GET"
	ChannelKeyHealthCheckHTTPMethodPost   ChannelKeyHealthCheckHTTPMethod = "POST"
	ChannelKeyHealthCheckHTTPMethodPut    ChannelKeyHealthCheckHTTPMethod = "PUT"
	ChannelKeyHealthCheckHTTPMethodPatch  ChannelKeyHealthCheckHTTPMethod = "PATCH"
	ChannelKeyHealthCheckHTTPMethodDelete ChannelKeyHealthCheckHTTPMethod = "DELETE"
)

type ChannelKeyHealthCheckHTTPURLMode string

const (
	ChannelKeyHealthCheckHTTPURLModeProviderBaseURL ChannelKeyHealthCheckHTTPURLMode = "provider_base_url"
	ChannelKeyHealthCheckHTTPURLModeAbsoluteURL     ChannelKeyHealthCheckHTTPURLMode = "absolute_url"
)

type ChannelKeyHealthCheckKeyInjectionLocation string

const (
	ChannelKeyHealthCheckKeyInjectionAuthorizationBearer ChannelKeyHealthCheckKeyInjectionLocation = "authorization_bearer"
	ChannelKeyHealthCheckKeyInjectionHeader              ChannelKeyHealthCheckKeyInjectionLocation = "header"
)

type ChannelKeyStatus string

const (
	ChannelKeyStatusActive   ChannelKeyStatus = "active"
	ChannelKeyStatusDisabled ChannelKeyStatus = "disabled"
	ChannelKeyStatusArchived ChannelKeyStatus = "archived"
)

type ChannelKeyHealthCheckTrigger string

const (
	ChannelKeyHealthCheckTriggerManual    ChannelKeyHealthCheckTrigger = "manual"
	ChannelKeyHealthCheckTriggerScheduled ChannelKeyHealthCheckTrigger = "scheduled"
	ChannelKeyHealthCheckTriggerRequest   ChannelKeyHealthCheckTrigger = "request"
)

type ChannelKeyHealthCheck struct {
	Enabled          bool                                `json:"enabled"`
	IntervalMinutes  int                                 `json:"intervalMinutes,omitempty"`
	HistoryLimit     int                                 `json:"historyLimit,omitempty"`
	FailureThreshold int                                 `json:"failureThreshold,omitempty"`
	FailureAction    ChannelKeyHealthCheckFailureAction  `json:"failureAction,omitempty"`
	IncludeDisabled  bool                                `json:"includeDisabled,omitempty"`
	Rules            []ChannelKeyHealthCheckRule         `json:"rules,omitempty"`
	Policies         []ChannelKeyHealthCheckPolicy       `json:"policies,omitempty"`
	KeyMetadata      []ChannelKeyMetadata                `json:"keyMetadata,omitempty"`
	ArchivedKeys     []ChannelArchivedAPIKey             `json:"archivedKeys,omitempty"`
	History          []ChannelKeyHealthCheckHistoryEntry `json:"history,omitempty"`
}

func (h *ChannelKeyHealthCheck) IntervalMinutesOrDefault() int {
	if h == nil || h.IntervalMinutes <= 0 {
		return 60
	}

	return h.IntervalMinutes
}

func (h *ChannelKeyHealthCheck) HistoryLimitOrDefault() int {
	if h == nil || h.HistoryLimit <= 0 {
		return 20
	}
	if h.HistoryLimit > 100 {
		return 100
	}

	return h.HistoryLimit
}

func (h *ChannelKeyHealthCheck) FailureThresholdOrDefault() int {
	if h == nil || h.FailureThreshold <= 0 {
		return 3
	}

	return h.FailureThreshold
}

func (h *ChannelKeyHealthCheck) FailureActionOrDefault() ChannelKeyHealthCheckFailureAction {
	if h == nil || h.FailureAction == "" {
		return ChannelKeyHealthCheckFailureActionReportOnly
	}

	return h.FailureAction
}

type ChannelKeyHealthCheckPolicy struct {
	ID         string                               `json:"id"`
	Name       string                               `json:"name"`
	Enabled    *bool                                `json:"enabled,omitempty"`
	Conditions ChannelKeyHealthCheckPolicyCondition `json:"conditions,omitempty"`
	Actions    []ChannelKeyHealthCheckPolicyAction  `json:"actions,omitempty"`
}

type ChannelKeyHealthCheckPolicyCondition struct {
	MinFailureCount      *int               `json:"minFailureCount,omitempty"`
	Success              *bool              `json:"success,omitempty"`
	StatusCodes          []int              `json:"statusCodes,omitempty"`
	Available            *bool              `json:"available,omitempty"`
	BalanceLTE           *float64           `json:"balanceLTE,omitempty"`
	BalanceGTE           *float64           `json:"balanceGTE,omitempty"`
	ReasonContains       string             `json:"reasonContains,omitempty"`
	AllCheckedKeysFailed *bool              `json:"allCheckedKeysFailed,omitempty"`
	KeyStatuses          []ChannelKeyStatus `json:"keyStatuses,omitempty"`
	Expr                 string             `json:"expr,omitempty"`
}

type ChannelKeyHealthCheckPolicyAction struct {
	Type    ChannelKeyHealthCheckPolicyActionType `json:"type"`
	Backoff *ChannelKeyHealthCheckBackoff         `json:"backoff,omitempty"`
}

type ChannelKeyHealthCheckBackoff struct {
	Mode               ChannelKeyHealthCheckBackoffMode `json:"mode,omitempty"`
	IntervalMinutes    int                              `json:"intervalMinutes,omitempty"`
	MaxIntervalMinutes int                              `json:"maxIntervalMinutes,omitempty"`
	Multiplier         float64                          `json:"multiplier,omitempty"`
}

type ChannelFailurePolicyMode string

const (
	ChannelFailurePolicyModeInherit  ChannelFailurePolicyMode = "inherit"
	ChannelFailurePolicyModeOverride ChannelFailurePolicyMode = "override"
	ChannelFailurePolicyModeMerge    ChannelFailurePolicyMode = "merge"
	ChannelFailurePolicyModeDisabled ChannelFailurePolicyMode = "disabled"
)

type FailurePolicyEventSource string

const (
	FailurePolicyEventSourceRequestFailure              FailurePolicyEventSource = "request_failure"
	FailurePolicyEventSourceScheduledHealthCheckFailure FailurePolicyEventSource = "scheduled_health_check_failure"
	FailurePolicyEventSourceManualHealthCheckFailure    FailurePolicyEventSource = "manual_health_check_failure"
	FailurePolicyEventSourceScheduledHealthCheck        FailurePolicyEventSource = "scheduled_health_check"
	FailurePolicyEventSourceManualHealthCheck           FailurePolicyEventSource = "manual_health_check"
)

type FailurePolicyTarget string

const (
	FailurePolicyTargetKey     FailurePolicyTarget = "key"
	FailurePolicyTargetChannel FailurePolicyTarget = "channel"
)

type FailurePolicyActionType string

const (
	FailurePolicyActionReportOnly     FailurePolicyActionType = "report_only"
	FailurePolicyActionBackoffKey     FailurePolicyActionType = "backoff_key"
	FailurePolicyActionDisableKey     FailurePolicyActionType = "disable_key"
	FailurePolicyActionArchiveKey     FailurePolicyActionType = "archive_key"
	FailurePolicyActionDeleteKey      FailurePolicyActionType = "delete_key"
	FailurePolicyActionDisableChannel FailurePolicyActionType = "disable_channel"
	FailurePolicyActionEnableKey      FailurePolicyActionType = "enable_key"
	FailurePolicyActionRestoreKey     FailurePolicyActionType = "restore_key"
)

type FailurePolicy struct {
	KeyProfiles     []FailurePolicyProfile `json:"keyProfiles,omitempty"`
	ChannelProfiles []FailurePolicyProfile `json:"channelProfiles,omitempty"`
}

type ChannelFailurePolicy struct {
	Mode            ChannelFailurePolicyMode `json:"mode,omitempty"`
	KeyProfiles     []FailurePolicyProfile   `json:"keyProfiles,omitempty"`
	ChannelProfiles []FailurePolicyProfile   `json:"channelProfiles,omitempty"`
}

type FailurePolicyProfile struct {
	ID         string                               `json:"id"`
	Name       string                               `json:"name"`
	Enabled    *bool                                `json:"enabled,omitempty"`
	Sources    []FailurePolicyEventSource           `json:"sources,omitempty"`
	Conditions ChannelKeyHealthCheckPolicyCondition `json:"conditions,omitempty"`
	Actions    []FailurePolicyAction                `json:"actions,omitempty"`
}

type FailurePolicyAction struct {
	Type    FailurePolicyActionType       `json:"type"`
	Backoff *ChannelKeyHealthCheckBackoff `json:"backoff,omitempty"`
}

type ChannelKeyHealthCheckRule struct {
	ID      string                         `json:"id"`
	Name    string                         `json:"name"`
	Type    ChannelKeyHealthCheckRuleType  `json:"type"`
	Enabled *bool                          `json:"enabled,omitempty"`
	Builtin *ChannelKeyHealthCheckBuiltin  `json:"builtin,omitempty"`
	HTTP    *ChannelKeyHealthCheckHTTPRule `json:"http,omitempty"`
}

type ChannelKeyHealthCheckBuiltin struct {
	Kind string `json:"kind"`
}

type ChannelKeyHealthCheckHTTPRule struct {
	Method           ChannelKeyHealthCheckHTTPMethod    `json:"method,omitempty"`
	URLMode          ChannelKeyHealthCheckHTTPURLMode   `json:"urlMode,omitempty"`
	Path             string                             `json:"path,omitempty"`
	URL              string                             `json:"url,omitempty"`
	TimeoutMs        int                                `json:"timeoutMs,omitempty"`
	Headers          []HeaderEntry                      `json:"headers,omitempty"`
	KeyInjection     *ChannelKeyHealthCheckKeyInjection `json:"keyInjection,omitempty"`
	ExpectedStatuses []int                              `json:"expectedStatuses,omitempty"`
	PassWhen         string                             `json:"passWhen,omitempty"`
}

type ChannelKeyHealthCheckKeyInjection struct {
	Location   ChannelKeyHealthCheckKeyInjectionLocation `json:"location,omitempty"`
	HeaderName string                                    `json:"headerName,omitempty"`
}

type ChannelKeyMetadata struct {
	ID             string                              `json:"id,omitempty"`
	MaskedKey      string                              `json:"maskedKey,omitempty"`
	Status         ChannelKeyStatus                    `json:"status,omitempty"`
	LastCheckedAt  *time.Time                          `json:"lastCheckedAt,omitempty"`
	Success        *bool                               `json:"success,omitempty"`
	FailureCount   int                                 `json:"failureCount,omitempty"`
	Reason         string                              `json:"reason,omitempty"`
	Balance        any                                 `json:"balance,omitempty"`
	Currency       string                              `json:"currency,omitempty"`
	Available      *bool                               `json:"available,omitempty"`
	StatusCode     int                                 `json:"statusCode,omitempty"`
	MatchedPolicy  string                              `json:"matchedPolicy,omitempty"`
	Action         string                              `json:"action,omitempty"`
	NextCheckAt    *time.Time                          `json:"nextCheckAt,omitempty"`
	BackoffAttempt int                                 `json:"backoffAttempt,omitempty"`
	History        []ChannelKeyHealthCheckHistoryEntry `json:"history,omitempty"`
}

type ChannelKeyHealthCheckHistoryEntry struct {
	ID             string                       `json:"id,omitempty"`
	CheckedAt      time.Time                    `json:"checkedAt"`
	Success        bool                         `json:"success"`
	Reason         string                       `json:"reason,omitempty"`
	Balance        any                          `json:"balance,omitempty"`
	Currency       string                       `json:"currency,omitempty"`
	Available      *bool                        `json:"available,omitempty"`
	Trigger        ChannelKeyHealthCheckTrigger `json:"trigger,omitempty"`
	Rule           string                       `json:"rule,omitempty"`
	StatusCode     int                          `json:"statusCode,omitempty"`
	MatchedPolicy  string                       `json:"matchedPolicy,omitempty"`
	Action         string                       `json:"action,omitempty"`
	NextCheckAt    *time.Time                   `json:"nextCheckAt,omitempty"`
	BackoffAttempt int                          `json:"backoffAttempt,omitempty"`
}

type ChannelArchivedAPIKey struct {
	ID            string     `json:"id,omitempty"`
	MaskedKey     string     `json:"maskedKey,omitempty"`
	ArchivedAt    *time.Time `json:"archivedAt,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	LastCheckedAt *time.Time `json:"lastCheckedAt,omitempty"`
	FailureCount  int        `json:"failureCount,omitempty"`
	Balance       any        `json:"balance,omitempty"`
	Currency      string     `json:"currency,omitempty"`
	Available     *bool      `json:"available,omitempty"`
}

func ChannelAPIKeyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))

	return "key_" + hex.EncodeToString(sum[:8])
}

func MaskChannelAPIKey(key string) string {
	key = strings.TrimSpace(key)
	if len(key) == 0 {
		return "****"
	}
	if len(key) <= 8 {
		return "****" + key[max(0, len(key)-4):]
	}

	return key[:4] + "****" + key[len(key)-4:]
}

type ChannelRateLimit struct {
	RPM           *int64 `json:"rpm,omitempty"`           // Requests Per Minute, nil = unlimited
	TPM           *int64 `json:"tpm,omitempty"`           // Tokens Per Minute, nil = unlimited
	MaxConcurrent *int64 `json:"maxConcurrent,omitempty"` // Maximum concurrent requests, nil = unlimited

	// QueueSize controls the limiter mode when MaxConcurrent is set:
	//   nil / 0 = soft mode (count only, no blocking, no rejection — preserves PR #1322 scoring behaviour)
	//   > 0     = hard mode (FIFO wait queue with bounded capacity; excess requests rejected)
	// Has no effect when MaxConcurrent is unset or <= 0.
	QueueSize *int64 `json:"queueSize,omitempty"`

	// QueueTimeoutMs is the per-channel queue wait timeout in milliseconds.
	//   nil / 0 = no per-channel timeout (only the request context bounds the wait)
	//   > 0     = waiters that exceed this duration receive ErrChannelQueueTimeout
	// Only meaningful in hard mode (QueueSize > 0).
	QueueTimeoutMs *int64 `json:"queueTimeoutMs,omitempty"`
}

// DisabledAPIKey 记录被禁用的 API key 信息（敏感，按 credentials 同级保护）
// 注意：禁用判断以 Key 明文为主键。
type DisabledAPIKey struct {
	Key        string    `json:"key"`
	DisabledAt time.Time `json:"disabledAt"`
	ErrorCode  int       `json:"errorCode"`
	Reason     string    `json:"reason,omitempty"`
}

type ChannelCredentials struct {
	// APIKey is the API key for the channel, for the single key channel, e.g. Codex, Claude code, Antigravity.
	// It is kept for backward compatibility with existing data, recommend to use OAuth instead.
	APIKey string `json:"apiKey,omitempty"`

	// OAuth is the OAuth credentials for the channel, for the OAuth channel, e.g. Codex, Claude code, Antigravity.
	OAuth *OAuthCredentials `json:"oauth,omitempty"`

	// APIKeys is a list of API keys for the channel.
	// Multiple-key routing is controlled by ChannelSettings.KeySelection and
	// defaults to trace-sticky behavior for backward compatibility.
	APIKeys []string `json:"apiKeys,omitempty"`

	// Azure configuration for the channel.
	Azure *AzureCredential `json:"azure,omitempty"`

	// GCP is the GCP credentials for the channel.
	GCP *GCPCredential `json:"gcp,omitempty"`
}

// GetAllAPIKeys returns all API keys for the channel, combining APIKey and APIKeys fields.
// This ensures backward compatibility with old data that only has APIKey set.
func (c *ChannelCredentials) GetAllAPIKeys() []string {
	if c == nil {
		return nil
	}

	var keys []string

	// Add legacy APIKey if present (only if not OAuth credential)
	if c.APIKey != "" && !c.IsOAuth() {
		keys = append(keys, c.APIKey)
	}

	// Add new APIKeys
	keys = append(keys, c.APIKeys...)

	return keys
}

// GetEnabledAPIKeys returns API keys that are not disabled.
func (c *ChannelCredentials) GetEnabledAPIKeys(disabledKeys []DisabledAPIKey) []string {
	return c.GetRoutableAPIKeys(disabledKeys, nil)
}

// GetRoutableAPIKeys returns API keys that are neither disabled nor archived.
func (c *ChannelCredentials) GetRoutableAPIKeys(disabledKeys []DisabledAPIKey, archivedKeys []ChannelArchivedAPIKey) []string {
	allKeys := c.GetAllAPIKeys()
	if len(disabledKeys) == 0 && len(archivedKeys) == 0 {
		return allKeys
	}

	disabledSet := make(map[string]struct{}, len(disabledKeys))
	for _, dk := range disabledKeys {
		if dk.Key == "" {
			continue
		}

		disabledSet[dk.Key] = struct{}{}
	}

	archivedSet := make(map[string]struct{}, len(archivedKeys))
	for _, archived := range archivedKeys {
		if archived.ID == "" {
			continue
		}

		archivedSet[archived.ID] = struct{}{}
	}

	enabled := make([]string, 0, len(allKeys))
	for _, key := range allKeys {
		if _, ok := disabledSet[key]; ok {
			continue
		}
		if _, ok := archivedSet[ChannelAPIKeyFingerprint(key)]; ok {
			continue
		}

		enabled = append(enabled, key)
	}

	return enabled
}

// IsOAuth returns true if OAuth credentials are configured and valid.
// It checks both the new OAuth field and legacy APIKey field for backward compatibility.
func (c *ChannelCredentials) IsOAuth() bool {
	if c == nil {
		return false
	}

	// Check new OAuth field first
	if c.OAuth != nil && c.OAuth.AccessToken != "" {
		return true
	}

	// Backward compatibility: check if APIKey contains OAuth JSON
	return isOAuthJSON(c.APIKey)
}

// isOAuthJSON checks if a string is an OAuth JSON credential.
func isOAuthJSON(s string) bool {
	s = strings.TrimSpace(s)
	return strings.HasPrefix(s, "{") && strings.Contains(s, "access_token")
}

type OAuthCredentials = oauth.OAuthCredentials

type AzureCredential struct {
	// APIVersion is a optional version for the channel.
	APIVersion string `json:"apiVersion"`
}

type GCPCredential struct {
	Region    string `json:"region"`
	ProjectID string `json:"projectID"`
	JSONData  string `json:"jsonData"`
}

type GCPCredentialsJSON struct {
	Type                    string `json:"type" validate:"required"`
	ProjectID               string `json:"projectID" validate:"required"`
	PrivateKeyID            string `json:"privateKeyID" validate:"required"`
	PrivateKey              string `json:"privateKey" validate:"required"`
	ClientEmail             string `json:"clientEmail" validate:"required"`
	ClientID                string `json:"clientID" validate:"required"`
	AuthURI                 string `json:"authURI" validate:"required"`
	TokenURI                string `json:"tokenURI" validate:"required"`
	AuthProviderX509CertURL string `json:"authProviderX509CertURL" validate:"required"`
	ClientX509CertURL       string `json:"clientX509CertURL" validate:"required"`
	UniverseDomain          string `json:"universeDomain" validate:"required"`
}

type CapabilityPolicy string

const (
	CapabilityPolicyUnlimited CapabilityPolicy = "unlimited"
	CapabilityPolicyRequire   CapabilityPolicy = "require"
	CapabilityPolicyForbid    CapabilityPolicy = "forbid"
)

type ChannelPolicies struct {
	Stream CapabilityPolicy `json:"stream,omitempty"`
}

// ParseOverrideOperations parses the override parameters string.
// Supports both legacy map format (JSON object) and new operation array format (JSON array).
// Legacy format is automatically converted to OverrideOperation slice.
func ParseOverrideOperations(raw string) ([]OverrideOperation, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" || raw == "[]" {
		return nil, nil
	}

	if raw[0] == '[' {
		var ops []OverrideOperation
		if err := json.Unmarshal([]byte(raw), &ops); err != nil {
			return nil, fmt.Errorf("invalid override operations: %w", err)
		}

		return ops, nil
	}

	var legacy map[string]any
	if err := json.Unmarshal([]byte(raw), &legacy); err != nil {
		return nil, fmt.Errorf("invalid override parameters: %w", err)
	}

	ops := make([]OverrideOperation, 0, len(legacy))
	for key, value := range legacy {
		if strVal, ok := value.(string); ok && strVal == "__AXONHUB_CLEAR__" {
			ops = append(ops, OverrideOperation{Op: OverrideOpDelete, Path: key})
		} else {
			// Convert value to string
			var strValue string

			switch v := value.(type) {
			case string:
				strValue = v
			default:
				strValue = fmt.Sprintf("%v", value)
			}

			ops = append(ops, OverrideOperation{Op: OverrideOpSet, Path: key, Value: strValue})
		}
	}

	return ops, nil
}

// SerializeOverrideOperations converts override operations to a JSON string for storage.
func SerializeOverrideOperations(ops []OverrideOperation) (string, error) {
	if len(ops) == 0 {
		return "[]", nil
	}

	data, err := json.Marshal(ops)
	if err != nil {
		return "", fmt.Errorf("failed to serialize override operations: %w", err)
	}

	return string(data), nil
}
