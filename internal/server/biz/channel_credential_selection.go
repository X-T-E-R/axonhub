package biz

import (
	"context"
	"math"
	"math/rand/v2"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
)

// RawHTTPCredentialAttempt represents one atomically reserved credential and
// egress lease for a single upstream attempt. Its identity and release callback
// are immutable and independent of later retries on the same request.
type RawHTTPCredentialAttempt struct {
	ChannelID         int
	CredentialOwnerID int
	Key               string
	KeyID             string
	StartedAt         time.Time
	HasRecoveryGen    bool
	RecoveryGen       uint64
	EgressClient      *httpclient.HttpClient
	RawTransport      *http.Transport

	releaseOnce sync.Once
	release     func(status int, outcome CredentialAttemptOutcome)
}

// NewLegacyRawHTTPCredentialAttempt wraps an already-selected context attempt
// (such as test callers using RawHTTPAPIKey before constructing the transport)
// into an immutable attempt handle.
func NewLegacyRawHTTPCredentialAttempt(
	channelID int,
	credentialOwnerID int,
	key string,
	client *httpclient.HttpClient,
	finish func(status int, failed, canceled bool),
) *RawHTTPCredentialAttempt {
	var rawTransport *http.Transport
	if client != nil && client.GetNativeClient() != nil {
		if tr, ok := client.GetNativeClient().Transport.(*http.Transport); ok {
			rawTransport = tr
		}
	}
	var release func(int, CredentialAttemptOutcome)
	if finish != nil {
		release = func(status int, outcome CredentialAttemptOutcome) {
			switch outcome {
			case CredentialAttemptCanceled, CredentialAttemptNeutral:
				finish(status, false, true)
			case CredentialAttemptFailure:
				finish(status, true, false)
			default:
				finish(status, false, false)
			}
		}
	}
	return &RawHTTPCredentialAttempt{
		ChannelID:         channelID,
		CredentialOwnerID: credentialOwnerID,
		Key:               key,
		KeyID:             objects.ChannelAPIKeyFingerprint(key),
		StartedAt:         time.Now(),
		EgressClient:      client,
		RawTransport:      rawTransport,
		release:           release,
	}
}

// Finish releases the attempt's key and proxy leases at most once with the
// specified runtime health outcome.
func (a *RawHTTPCredentialAttempt) Finish(status int, outcome CredentialAttemptOutcome) {
	if a == nil {
		return
	}
	a.releaseOnce.Do(func() {
		if a.release != nil {
			a.release(status, outcome)
		}
	})
}

func (a *RawHTTPCredentialAttempt) legacyFinishFunc() func(int, bool, bool) {
	if a == nil || a.release == nil {
		return nil
	}
	return func(status int, failed, canceled bool) {
		a.Finish(status, outcomeFromLegacyFlags(failed, canceled))
	}
}

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
	return c.credentialCandidatesWithExcluded(ctx, nil)
}

func (c *Channel) credentialCandidatesWithExcluded(ctx context.Context, excluded map[string]struct{}) []credentialCandidate {
	keys := c.cachedEnabledAPIKeys
	if len(keys) == 0 {
		keys = c.Credentials.GetRoutableAPIKeys(c.DisabledAPIKeys, channelArchivedAPIKeys(c.Settings))
	}
	candidates := make([]credentialCandidate, 0, len(keys))
	for _, key := range keys {
		if _, skip := excluded[key]; skip {
			continue
		}
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
	attempt := c.reserveCredentialAttempt(ctx, strategy, sticky, nil, true)
	if attempt == nil {
		contexts.WithChannelAPIKey(ctx, "")
		return ""
	}
	contexts.WithChannelCredentialEgress(ctx, attempt.Key, attempt.EgressClient, attempt.legacyFinishFunc())
	return attempt.Key
}

func (c *Channel) reserveCredentialAttempt(
	ctx context.Context,
	strategy objects.ChannelKeySelectionStrategy,
	sticky func([]string) string,
	excluded map[string]struct{},
	allowSoftFallback bool,
) *RawHTTPCredentialAttempt {
	candidates := c.credentialCandidatesWithExcluded(ctx, excluded)
	runtime := c.runtime()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	now := time.Now()
	candidates = filterCredentialCandidates(candidates, runtime, c.CredentialOwnerID(), now, allowSoftFallback)
	if len(candidates) == 0 {
		return nil
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
	return c.buildCredentialAttempt(ctx, candidates[winner], now)
}

// buildCredentialAttempt runs under the credential runtime lock.
func (c *Channel) buildCredentialAttempt(ctx context.Context, selected credentialCandidate, now time.Time) *RawHTTPCredentialAttempt {
	runtime := c.runtime()
	gen, finishAttempt := runtime.startAttempt(ctx, c.CredentialOwnerID(), selected.id, now)
	var finish func(status int, outcome CredentialAttemptOutcome)
	if finishAttempt != nil {
		if c.credentialEgress != nil && selected.proxyID > 0 {
			if runtime.proxyInFlight == nil {
				runtime.proxyInFlight = make(map[int]int)
			}
			runtime.proxyInFlight[selected.proxyID]++
			finish = func(status int, outcome CredentialAttemptOutcome) {
				applied := finishAttempt(status, outcome)
				runtime.mu.Lock()
				runtime.proxyInFlight[selected.proxyID]--
				runtime.mu.Unlock()
				if applied && outcome == CredentialAttemptFailure && status == 0 {
					c.credentialEgress.blockProxyTemporarily(selected.proxyID)
				}
			}
		} else {
			finish = func(status int, outcome CredentialAttemptOutcome) {
				finishAttempt(status, outcome)
			}
		}
	}
	var rawTransport *http.Transport
	if selected.client != nil && selected.client.GetNativeClient() != nil {
		if tr, ok := selected.client.GetNativeClient().Transport.(*http.Transport); ok {
			rawTransport = tr
		}
	}
	return &RawHTTPCredentialAttempt{
		ChannelID:         c.ID,
		CredentialOwnerID: c.CredentialOwnerID(),
		Key:               selected.key,
		KeyID:             selected.id,
		StartedAt:         now,
		HasRecoveryGen:    true,
		RecoveryGen:       gen,
		EgressClient:      selected.client,
		RawTransport:      rawTransport,
		release:           finish,
	}
}

// reserveCredential runs under the credential runtime lock.
func (c *Channel) reserveCredential(ctx context.Context, selected credentialCandidate, now time.Time) {
	attempt := c.buildCredentialAttempt(ctx, selected, now)
	contexts.WithChannelCredentialEgress(ctx, attempt.Key, attempt.EgressClient, attempt.legacyFinishFunc())
}
