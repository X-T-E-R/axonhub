package biz

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/objects"
)

// CredentialAttemptOutcome classifies how a finished credential attempt updates
// the runtime health state. Neutral and Canceled release key and proxy leases
// without modifying EWMA failure rate, latency, or cooldowns.
type CredentialAttemptOutcome int

const (
	CredentialAttemptSuccess CredentialAttemptOutcome = iota
	CredentialAttemptFailure
	CredentialAttemptCanceled
	CredentialAttemptNeutral
)

type credentialRuntimeKey struct {
	channelID int
	keyID     string
}
type credentialRuntimeState struct {
	inFlight                                     int
	lastStartedAt, lastFeedbackAt, cooldownUntil time.Time
	failureRate, latencyMs                       float64
	currentWeight                                int
	recoveryGen                                  uint64
	recoveredAt                                  time.Time
}

type credentialRuntime struct {
	mu            sync.Mutex
	states        map[credentialRuntimeKey]*credentialRuntimeState
	proxyInFlight map[int]int
	cursor        atomic.Uint64
}

func (c *Channel) runtime() *credentialRuntime {
	if c.credentialRuntime == nil {
		c.credentialRuntime = &credentialRuntime{}
	}
	return c.credentialRuntime
}

func (r *credentialRuntime) state(key credentialRuntimeKey, now time.Time) *credentialRuntimeState {
	if r.states == nil {
		r.states = make(map[credentialRuntimeKey]*credentialRuntimeState)
	}
	if s, ok := r.states[key]; ok {
		return s
	}
	if len(r.states) >= 8192 {
		for key, s := range r.states {
			if s.inFlight == 0 && s.recoveryGen == 0 && now.Sub(s.lastStartedAt) > 30*time.Minute {
				delete(r.states, key)
			}
		}
	}
	s := &credentialRuntimeState{}
	r.states[key] = s
	return s
}

func (r *credentialRuntime) start(ctx context.Context, channelID int, keyID string, now time.Time) func(int, bool, bool) {
	_, finish := r.startAttempt(ctx, channelID, keyID, now)
	if finish == nil {
		return nil
	}
	return func(status int, failed, canceled bool) {
		finish(status, outcomeFromLegacyFlags(failed, canceled))
	}
}

func outcomeFromLegacyFlags(failed, canceled bool) CredentialAttemptOutcome {
	if canceled {
		return CredentialAttemptCanceled
	}
	if failed {
		return CredentialAttemptFailure
	}
	return CredentialAttemptSuccess
}

// startAttempt runs under r.mu.Lock() and returns the key's recovery generation
// at start along with an idempotent release callback that reports whether
// health feedback was applied (false for canceled, neutral, or stale pre-recovery
// completions).
func (r *credentialRuntime) startAttempt(ctx context.Context, channelID int, keyID string, now time.Time) (uint64, func(int, CredentialAttemptOutcome) bool) {
	rk := credentialRuntimeKey{channelID, keyID}
	s := r.state(rk, now)
	s.lastStartedAt = now
	startGen := s.recoveryGen
	// Discovery and provider-only callers do not own an attempt lifecycle.
	if !contexts.HasRequestContainer(ctx) {
		return startGen, nil
	}
	s.inFlight++
	var once sync.Once
	return startGen, func(status int, outcome CredentialAttemptOutcome) bool {
		applied := false
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			s.inFlight--
			if outcome == CredentialAttemptCanceled || outcome == CredentialAttemptNeutral {
				return
			}
			// Ignore late feedback from attempts started before an administrative
			// key recovery so pre-recovery failures cannot re-poison the key.
			if s.recoveryGen != startGen {
				return
			}
			applied = true
			end := time.Now()
			decay := math.Exp(-end.Sub(s.lastFeedbackAt).Seconds() / 60)
			failure := 0.0
			if outcome == CredentialAttemptFailure {
				failure = 1
			}
			s.failureRate = s.failureRate*decay*0.8 + failure*0.2
			latency := float64(end.Sub(now).Milliseconds())
			if s.latencyMs == 0 {
				s.latencyMs = latency
			} else {
				s.latencyMs = s.latencyMs*0.8 + latency*0.2
			}
			s.lastFeedbackAt = end
			if outcome == CredentialAttemptFailure {
				switch status {
				case 429, 402, 432:
					s.cooldownUntil = end.Add(15 * time.Second)
				}
			}
		})
		return applied
	}
}

// recover clears runtime penalties and cooldowns for the recovered keys without
// disturbing active in-flight leases, advancing each key's recovery generation so
// pre-recovery in-flight completions are ignored.
func (r *credentialRuntime) recover(channelID int, keyIDs []string, now time.Time) {
	if len(keyIDs) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, keyID := range keyIDs {
		if keyID == "" {
			continue
		}
		s := r.state(credentialRuntimeKey{channelID, keyID}, now)
		s.cooldownUntil = time.Time{}
		s.failureRate = 0
		s.lastFeedbackAt = time.Time{}
		s.recoveryGen++
		s.recoveredAt = now
	}
}

func (r *credentialRuntime) isStaleAfterRecovery(channelID int, keyID string, startedAt time.Time, hasGen bool, gen uint64) bool {
	if keyID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.states[credentialRuntimeKey{channelID, keyID}]
	if s == nil || s.recoveryGen == 0 {
		return false
	}
	if hasGen {
		return s.recoveryGen != gen
	}
	return !startedAt.IsZero() && !s.recoveredAt.IsZero() && !startedAt.After(s.recoveredAt)
}

func (r *credentialRuntime) snapshot(channelID int, keyID string) credentialRuntimeState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return *r.state(credentialRuntimeKey{channelID, keyID}, time.Now())
}

// CredentialRuntimeMetrics exposes read-only runtime telemetry for a credential
// key (used by diagnostics and integration verification).
func (svc *ChannelService) CredentialRuntimeMetrics(ownerID int, key string) (inFlight int, failureRate float64, latencyMs float64, cooldownUntil time.Time) {
	s := svc.credentialRuntime.snapshot(ownerID, objects.ChannelAPIKeyFingerprint(key))
	return s.inFlight, s.failureRate, s.latencyMs, s.cooldownUntil
}
