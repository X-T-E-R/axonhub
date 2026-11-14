package biz

import (
	"context"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/looplj/axonhub/internal/contexts"
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
			if s.inFlight == 0 && now.Sub(s.lastStartedAt) > 30*time.Minute {
				delete(r.states, key)
			}
		}
	}
	s := &credentialRuntimeState{}
	r.states[key] = s
	return s
}

func (r *credentialRuntime) start(ctx context.Context, channelID int, keyID string, now time.Time) func(int, bool, bool) {
	rk := credentialRuntimeKey{channelID, keyID}
	s := r.state(rk, now)
	s.lastStartedAt = now
	// Discovery and provider-only callers do not own an attempt lifecycle.
	if !contexts.HasRequestContainer(ctx) {
		return nil
	}
	s.inFlight++
	var once sync.Once
	return func(status int, failed, canceled bool) {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			s.inFlight--
			if canceled {
				return
			}
			end := time.Now()
			decay := math.Exp(-end.Sub(s.lastFeedbackAt).Seconds() / 60)
			failure := 0.0
			if failed {
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
			switch status {
			case 429, 402, 432:
				s.cooldownUntil = end.Add(15 * time.Second)
			}
		})
	}
}

func (r *credentialRuntime) snapshot(channelID int, keyID string) credentialRuntimeState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return *r.state(credentialRuntimeKey{channelID, keyID}, time.Now())
}
