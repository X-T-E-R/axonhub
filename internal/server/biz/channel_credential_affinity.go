package biz

import (
	"context"
	"time"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
)

// Called by the selector while it owns the runtime lock and has resolved the
// highest eligible tier. Only non-secret identifiers enter the diagnostic event.
func (c *Channel) logCredentialAffinityEscape(ctx context.Context, key string, eligible []string) {
	id := objects.ChannelAPIKeyFingerprint(key)
	p := c.Settings.CredentialPolicy(id)
	now := time.Now()
	s := c.credentialRuntime.state(credentialRuntimeKey{c.CredentialOwnerID(), id}, now)
	reason := "egress_blocked"
	switch {
	case contexts.ChannelCredentialExcluded(ctx, c.ID, key):
		reason = "request_excluded"
	case p.MaxConcurrent != nil && s.inFlight >= *p.MaxConcurrent:
		reason = "concurrency_saturated"
	case s.cooldownUntil.After(now):
		reason = "cooldown"
	case p.RequestIntervalMs != nil && now.Sub(s.lastStartedAt) < time.Duration(*p.RequestIntervalMs)*time.Millisecond:
		reason = "cooldown"
	default:
		if c.Settings != nil && c.Settings.KeyHealthCheck != nil {
			for _, meta := range c.Settings.KeyHealthCheck.KeyMetadata {
				if meta.ID == id && meta.BalanceSnapshot.ExhaustedAt(now) {
					reason = "quota_exhausted"
					break
				}
			}
		}
		if reason != "quota_exhausted" && len(eligible) > 0 && c.Settings.CredentialPolicy(objects.ChannelAPIKeyFingerprint(eligible[0])).Priority > p.Priority {
			reason = "higher_priority"
		}
	}
	log.Debug(ctx, "Credential affinity escape", log.Int("channel_id", c.ID), log.String("credential_id", id), log.String("reason", reason))
}
