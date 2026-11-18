package biz

import (
	"context"
	"math"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
)

type credentialCandidate struct {
	key, id    string
	policy     objects.ChannelCredentialPolicy
	snapshot   *objects.ChannelKeyBalanceSnapshot
	client     *httpclient.HttpClient
	proxyID    int
	proxyLimit *int
	state      *credentialRuntimeState
}

func (c *Channel) RoutableCredentialCount() int {
	if len(c.cachedEnabledAPIKeys) > 0 {
		return len(c.cachedEnabledAPIKeys)
	}
	return len(c.Credentials.GetRoutableAPIKeys(c.DisabledAPIKeys, channelArchivedAPIKeys(c.Settings)))
}

func (c *Channel) credentialCandidates(ctx context.Context) []credentialCandidate {
	keys := c.cachedEnabledAPIKeys
	if len(keys) == 0 {
		keys = c.Credentials.GetRoutableAPIKeys(c.DisabledAPIKeys, channelArchivedAPIKeys(c.Settings))
	}
	candidates := make([]credentialCandidate, 0, len(keys))
	for _, key := range keys {
		if contexts.ChannelCredentialExcluded(ctx, c.ID, key) {
			continue
		}
		id := objects.ChannelAPIKeyFingerprint(key)
		candidate := credentialCandidate{key: key, id: id, policy: c.Settings.CredentialPolicy(id)}
		if c.Settings != nil && c.Settings.KeyHealthCheck != nil {
			for _, meta := range c.Settings.KeyHealthCheck.KeyMetadata {
				if meta.ID == id {
					candidate.snapshot = meta.BalanceSnapshot
					break
				}
			}
		}
		if c.credentialEgress != nil {
			client, proxyID, err := c.credentialEgress.resolveCredentialClient(ctx, c.Channel, id, c.HTTPClient)
			if err != nil {
				log.Debug(ctx, "Credential ineligible", log.Int("channel_id", c.ID), log.String("credential_id", id), log.String("reason", "egress_blocked"))
				continue
			}
			candidate.client, candidate.proxyID = client, proxyID
			if proxyID > 0 {
				candidate.proxyLimit = c.credentialEgress.proxyConcurrentLimit(proxyID)
			}
		} else if candidate.policy.EgressMode == objects.CredentialEgressModePinnedProxy || candidate.policy.EgressMode == objects.CredentialEgressModeProxyPool {
			// A channel without its managed-proxy resolver must never send bound keys directly.
			continue
		}
		candidates = append(candidates, candidate)
	}
	return candidates
}

func filterCredentialCandidates(candidates []credentialCandidate, r *credentialRuntime, channelID int, now time.Time, allowSoftFallback bool) []credentialCandidate {
	hard := make([]credentialCandidate, 0, len(candidates))
	eligible := make([]credentialCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		candidate.state = r.state(credentialRuntimeKey{channelID, candidate.id}, now)
		s, p := candidate.state, candidate.policy
		if p.MaxConcurrent != nil && s.inFlight >= *p.MaxConcurrent {
			continue
		}
		if candidate.proxyLimit != nil && r.proxyInFlight[candidate.proxyID] >= *candidate.proxyLimit {
			continue
		}
		if p.RequestIntervalMs != nil && now.Sub(s.lastStartedAt) < time.Duration(*p.RequestIntervalMs)*time.Millisecond {
			continue
		}
		hard = append(hard, candidate)
		if s.cooldownUntil.After(now) || candidate.snapshot.ExhaustedAt(now) {
			continue
		}
		eligible = append(eligible, candidate)
	}
	if len(eligible) == 0 && allowSoftFallback {
		eligible = hard
	}
	if len(eligible) == 0 {
		return nil
	}
	priority := eligible[0].policy.Priority
	for _, candidate := range eligible {
		priority = max(priority, candidate.policy.Priority)
	}
	return slices.DeleteFunc(eligible, func(candidate credentialCandidate) bool { return candidate.policy.Priority != priority })
}

func (c *Channel) HasEligibleCredential(ctx context.Context) bool {
	candidates := c.credentialCandidates(ctx)
	runtime := c.runtime()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	return len(filterCredentialCandidates(candidates, runtime, c.CredentialOwnerID(), time.Now(), false)) > 0
}

func weightedCredentialRoundRobin(candidates []credentialCandidate) int {
	winner, total := 0, 0
	for i, candidate := range candidates {
		weight := candidate.policy.Weight
		total += weight
		candidate.state.currentWeight += weight
		if candidate.state.currentWeight > candidates[winner].state.currentWeight {
			winner = i
		}
	}
	candidates[winner].state.currentWeight -= total
	return winner
}

func credentialSmartScore(c credentialCandidate, now time.Time) float64 {
	load := 1.0 / (1 + float64(c.state.inFlight))
	if c.policy.MaxConcurrent != nil && *c.policy.MaxConcurrent > 0 {
		load = 1 - float64(c.state.inFlight)/float64(*c.policy.MaxConcurrent)
	}
	quota := 0.5
	if c.snapshot != nil {
		if c.snapshot.UsageRatio != nil {
			quota = 1 - *c.snapshot.UsageRatio
		} else if c.snapshot.Remaining != nil && c.snapshot.Limit != nil && *c.snapshot.Limit > 0 {
			quota = *c.snapshot.Remaining / *c.snapshot.Limit
		} else if c.snapshot.Remaining != nil && c.policy.ManualQuotaLimit != nil && *c.policy.ManualQuotaLimit > 0 {
			quota = *c.snapshot.Remaining / *c.policy.ManualQuotaLimit
		}
	}
	health := 1 - c.state.failureRate*math.Exp(-now.Sub(c.state.lastFeedbackAt).Seconds()/60)
	latency := 1 / (1 + c.state.latencyMs/1000)
	return load + 0.8*max(0, min(1, quota)) + health + 0.5*latency
}

func (c *Channel) selectCredential(ctx context.Context, strategy objects.ChannelKeySelectionStrategy, sticky func([]string) string) string {
	candidates := c.credentialCandidates(ctx)
	runtime := c.credentialRuntime
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	now := time.Now()
	candidates = filterCredentialCandidates(candidates, runtime, c.CredentialOwnerID(), now, true)
	if len(candidates) == 0 {
		contexts.WithChannelAPIKey(ctx, "")
		return ""
	}
	winner := 0
	switch strategy {
	case objects.ChannelKeySelectionStrategyTraceSticky, objects.ChannelKeySelectionStrategyCacheAffinity:
		if sticky != nil {
			keys := make([]string, len(candidates))
			for i, candidate := range candidates {
				keys[i] = candidate.key
			}
			selected := sticky(keys)
			for i, candidate := range candidates {
				if candidate.key == selected {
					winner = i
					break
				}
			}
		} else {
			winner = weightedCredentialRoundRobin(candidates)
		}
	case objects.ChannelKeySelectionStrategyRandom:
		total := 0
		for _, candidate := range candidates {
			total += candidate.policy.Weight
		}
		point := rand.IntN(total)
		for i, candidate := range candidates {
			point -= candidate.policy.Weight
			if point < 0 {
				winner = i
				break
			}
		}
	case objects.ChannelKeySelectionStrategyQuotaResetPriority:
		for i := 1; i < len(candidates); i++ {
			a, b := candidates[i], candidates[winner]
			var ar, br *time.Time
			if a.snapshot != nil {
				ar = a.snapshot.ResetAt
			}
			if b.snapshot != nil {
				br = b.snapshot.ResetAt
			}
			if ar != nil && (br == nil || ar.Before(*br)) {
				winner = i
				continue
			}
			if (ar == nil) != (br == nil) || ar != nil && !ar.Equal(*br) {
				continue
			}
			utilization := func(candidate credentialCandidate) float64 {
				if candidate.policy.MaxConcurrent != nil && *candidate.policy.MaxConcurrent > 0 {
					return float64(candidate.state.inFlight) / float64(*candidate.policy.MaxConcurrent)
				}
				return float64(candidate.state.inFlight)
			}
			au, bu := utilization(a), utilization(b)
			if au < bu || au == bu && a.state.lastStartedAt.Before(b.state.lastStartedAt) {
				winner = i
			}
		}
	case objects.ChannelKeySelectionStrategySmart:
		scores := make([]float64, len(candidates))
		best := math.Inf(-1)
		for i, candidate := range candidates {
			scores[i] = credentialSmartScore(candidate, now)
			best = max(best, scores[i])
		}
		band := make([]int, 0, len(candidates))
		for i, score := range scores {
			if best-score <= 0.05 {
				band = append(band, i)
			}
		}
		winner = band[(runtime.cursor.Add(1)-1)%uint64(len(band))]
	default:
		winner = weightedCredentialRoundRobin(candidates)
	}
	c.reserveCredential(ctx, candidates[winner], now)
	return candidates[winner].key
}

// reserveCredential runs under the credential runtime lock.
func (c *Channel) reserveCredential(ctx context.Context, selected credentialCandidate, now time.Time) {
	runtime := c.credentialRuntime
	finish := runtime.start(ctx, c.CredentialOwnerID(), selected.id, now)
	if finish != nil && c.credentialEgress != nil && selected.proxyID > 0 {
		if runtime.proxyInFlight == nil {
			runtime.proxyInFlight = make(map[int]int)
		}
		runtime.proxyInFlight[selected.proxyID]++
		complete := finish
		finish = func(status int, failed, canceled bool) {
			complete(status, failed, canceled)
			runtime.mu.Lock()
			runtime.proxyInFlight[selected.proxyID]--
			runtime.mu.Unlock()
			if failed && !canceled && status == 0 {
				c.credentialEgress.blockProxyTemporarily(selected.proxyID)
			}
		}
	}
	contexts.WithChannelCredentialEgress(ctx, selected.key, selected.client, finish)
}
