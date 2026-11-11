package biz

import (
	"context"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/request"
)

// ExecutedAccountingError is not an upstream failure: the provider has already
// returned usage, and only accounting publication/recovery may be retried.
type ExecutedAccountingError struct{ Cause error }

func (e *ExecutedAccountingError) Error() string {
	return "accounting failed after upstream execution: " + e.Cause.Error()
}
func (e *ExecutedAccountingError) Unwrap() error { return e.Cause }

func ForwardingAccountingEnabled(ctx context.Context) bool {
	scope := observationFromContext(ctx)
	return scope != nil && scope.writer.node.Enabled
}

// APIKeyHasHardQuota reports whether any configured profile on key defines a
// hard request, token, or cost quota that requires authoritative real-time checks.
func APIKeyHasHardQuota(key *ent.APIKey) bool {
	if key == nil || key.Profiles == nil {
		return false
	}
	for i := range key.Profiles.Profiles {
		q := key.Profiles.Profiles[i].Quota
		if q != nil && (q.Requests != nil || q.TotalTokens != nil || q.Cost != nil) {
			return true
		}
	}
	return false
}

// CompactAccountingEligibleContext reports whether ctx belongs to a normal API
// request for a non-quota API key that can use the local compact SQLite outbox.
func CompactAccountingEligibleContext(ctx context.Context) bool {
	src, hasSrc := contexts.GetSource(ctx)
	if hasSrc && src != request.SourceAPI {
		return false
	}
	apiKey, ok := contexts.GetAPIKey(ctx)
	if !ok || apiKey == nil || APIKeyHasHardQuota(apiKey) {
		return false
	}
	return true
}
