package biz

import (
	"bytes"
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
)

// StructuredJSONError holds exact machine-readable error identifiers extracted
// from a bounded non-2xx JSON object body. Free-form prose fields (such as
// "message" or string "error") are intentionally never extracted.
type StructuredJSONError struct {
	Tag  string
	Code string
	Type string
}

// HasIdentifier reports whether at least one structured machine error
// identifier was extracted from the payload.
func (e StructuredJSONError) HasIdentifier() bool {
	return e.Tag != "" || e.Code != "" || e.Type != ""
}

// ExtractStructuredJSONError extracts exact top-level string "tag", scalar
// "error.code"/"code", and string "error.type" from a bounded JSON object
// payload using gjson without full-body deserialization or whitespace trimming.
func ExtractStructuredJSONError(body []byte, contentEncoding string) StructuredJSONError {
	if enc := strings.TrimSpace(strings.ToLower(contentEncoding)); enc != "" && enc != "identity" {
		return StructuredJSONError{}
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return StructuredJSONError{}
	}
	if !gjson.ValidBytes(trimmed) {
		return StructuredJSONError{}
	}

	var out StructuredJSONError
	if res := gjson.GetBytes(trimmed, "tag"); res.Exists() && res.Type == gjson.String {
		out.Tag = res.String()
	}
	if res := gjson.GetBytes(trimmed, "error.code"); res.Exists() && (res.Type == gjson.String || res.Type == gjson.Number) {
		out.Code = res.String()
	} else if res := gjson.GetBytes(trimmed, "code"); res.Exists() && (res.Type == gjson.String || res.Type == gjson.Number) {
		out.Code = res.String()
	}
	if res := gjson.GetBytes(trimmed, "error.type"); res.Exists() && res.Type == gjson.String {
		out.Type = res.String()
	}
	return out
}

// NormalizeRawHTTPPath ensures a relative Raw HTTP path starts with "/"
// without trimming trailing whitespace so distinct forwarded paths such as
// "/search " never alias an exact "/search" rule.
func NormalizeRawHTTPPath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

// exprReferencesStructuredErrorField reports whether a policy expression
// explicitly inspects at least one structured error identifier (errorTag,
// errorCode, or errorType) outside string literals.
func exprReferencesStructuredErrorField(expression string) bool {
	stripped := stripExprStringLiterals(expression)
	for _, ident := range []string{"errorTag", "errorCode", "errorType"} {
		if containsIdentifierToken(stripped, ident) {
			return true
		}
	}
	return false
}

func stripExprStringLiterals(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	var quote byte
	escaped := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if escaped {
				escaped = false
				continue
			}
			if c == '\\' && quote != '`' {
				escaped = true
				continue
			}
			if c == quote {
				quote = 0
				b.WriteByte(' ')
			}
			continue
		}
		if c == '"' || c == '\'' || c == '`' {
			quote = c
			b.WriteByte(' ')
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func containsIdentifierToken(s, ident string) bool {
	for offset := 0; offset < len(s); {
		idx := strings.Index(s[offset:], ident)
		if idx < 0 {
			return false
		}
		start := offset + idx
		end := start + len(ident)
		leftOK := start == 0 || !isExprIdentChar(s[start-1])
		rightOK := end == len(s) || !isExprIdentChar(s[end])
		if leftOK && rightOK {
			return true
		}
		offset = start + 1
	}
	return false
}

func isExprIdentChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

// IsSafeRawHTTPReplayOperation reports whether repeating the given HTTP method
// and normalized relative Raw path with an alternate credential is permitted by
// HTTP safe-method semantics (GET/HEAD/OPTIONS) or explicit channel allowlist.
func IsSafeRawHTTPReplayOperation(method, rawPath string, cfg *objects.ChannelRawHTTP) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	if cfg == nil || len(cfg.SafeReplayPaths) == 0 {
		return false
	}
	normalized := NormalizeRawHTTPPath(rawPath)
	for _, allowed := range cfg.SafeReplayPaths {
		if NormalizeRawHTTPPath(allowed) == normalized {
			return true
		}
	}
	return false
}

// RawHTTPKeyFailureClassification describes whether a Raw HTTP 403 response
// matched an explicit Expr-based key failure profile and whether that match
// authorizes a single alternate credential replay.
type RawHTTPKeyFailureClassification struct {
	Classified    bool
	AllowReplay   bool
	MatchedPolicy string
}

// HasRawHTTP403KeyPolicy reports whether the channel has any enabled
// Expr-based request_failure key profile referencing structured error fields
// that can classify a 403 response.
func (svc *ChannelService) HasRawHTTP403KeyPolicy(ctx context.Context, ch *Channel) bool {
	if svc == nil || ch == nil {
		return false
	}
	effective := svc.effectiveRawHTTPFailurePolicy(ctx, ch)
	for _, profile := range effective.KeyProfiles {
		if profile.Enabled != nil && !*profile.Enabled {
			continue
		}
		if len(profile.Actions) == 0 {
			continue
		}
		if len(profile.Sources) > 0 && !slices.Contains(profile.Sources, objects.FailurePolicyEventSourceRequestFailure) {
			continue
		}
		exprText := strings.TrimSpace(profile.Conditions.Expr)
		if exprText == "" || !exprReferencesStructuredErrorField(exprText) {
			continue
		}
		if len(profile.Conditions.StatusCodes) > 0 && !slices.Contains(profile.Conditions.StatusCodes, http.StatusForbidden) {
			continue
		}
		return true
	}
	return false
}

// ClassifyRawHTTPKeyFailure evaluates enabled Expr-based key failure profiles
// against structured error identifiers without mutating counters or DB state.
func (svc *ChannelService) ClassifyRawHTTPKeyFailure(
	ctx context.Context,
	ch *Channel,
	key string,
	method string,
	rawPath string,
	status int,
	errFields StructuredJSONError,
) RawHTTPKeyFailureClassification {
	if svc == nil || ch == nil || status == 0 {
		return RawHTTPKeyFailureClassification{}
	}
	if status == http.StatusForbidden && !errFields.HasIdentifier() {
		return RawHTTPKeyFailureClassification{}
	}
	effective := svc.effectiveRawHTTPFailurePolicy(ctx, ch)
	if len(effective.KeyProfiles) == 0 {
		return RawHTTPKeyFailureClassification{}
	}

	nextCount := svc.peekAPIKeyFailureCount(ch.CredentialOwnerID(), key, status) + 1
	event := failurePolicyEvent{
		Source:        objects.FailurePolicyEventSourceRequestFailure,
		Target:        objects.FailurePolicyTargetKey,
		ChannelID:     ch.CredentialOwnerID(),
		Key:           key,
		StatusCode:    status,
		RequestMethod: strings.ToUpper(strings.TrimSpace(method)),
		RequestPath:   NormalizeRawHTTPPath(rawPath),
		ErrorTag:      errFields.Tag,
		ErrorCode:     errFields.Code,
		ErrorType:     errFields.Type,
		FailureCount:  nextCount,
		Reason:        deriveErrorMessage(status),
		CheckedAt:     time.Now(),
	}
	matches := evaluateFailurePolicyProfilesWithOptions(effective.KeyProfiles, event, true, true)
	if len(matches) == 0 {
		return RawHTTPKeyFailureClassification{}
	}

	explicitDeny := false
	explicitAllow := false
	for _, match := range matches {
		if match.Profile.AllowReplay != nil {
			if *match.Profile.AllowReplay {
				explicitAllow = true
			} else {
				explicitDeny = true
			}
		}
	}

	var rawCfg *objects.ChannelRawHTTP
	if ch.Settings != nil {
		rawCfg = ch.Settings.RawHTTP
	}
	allowReplay := !explicitDeny && (explicitAllow || IsSafeRawHTTPReplayOperation(method, rawPath, rawCfg))
	return RawHTTPKeyFailureClassification{
		Classified:    true,
		AllowReplay:   allowReplay,
		MatchedPolicy: summarizeFailurePolicyMatches(matches),
	}
}

func (svc *ChannelService) effectiveRawHTTPFailurePolicy(ctx context.Context, ch *Channel) effectiveFailurePolicy {
	policy := &RetryPolicy{}
	if svc.SystemService != nil {
		policyCtx, cancel := xcontext.DetachWithTimeout(ctx, 5*time.Second)
		defer cancel()
		policy = svc.SystemService.RetryPolicyOrDefault(authz.WithSystemBypass(policyCtx, "raw-http-failure-classification"))
	}
	return resolveEffectiveFailurePolicy(policy, ch.Settings, false)
}

func (svc *ChannelService) peekAPIKeyFailureCount(channelID int, apiKey string, status int) int {
	svc.apiKeyErrorCountsLock.Lock()
	defer svc.apiKeyErrorCountsLock.Unlock()
	return svc.apiKeyErrorCounts[channelID][apiKey][status]
}
