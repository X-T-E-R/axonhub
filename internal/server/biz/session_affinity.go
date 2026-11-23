package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/objects"
)

const (
	sessionAffinityLRUSize = 16384
	responseOwnerLRUSize   = 32768
)

type sessionAffinitySlot struct {
	keyID     string
	rev       uint64
	expiresAt time.Time
}

type responseOwnerSlot struct {
	keyID     string
	expiresAt time.Time
}

// SessionAffinityStore manages tenant-isolated sticky credential bindings with
// CPR-style admission claim and observed-rev CAS rebinding, plus short-term
// response_id -> credentialKeyID ownership lookup for stateful continuations.
type SessionAffinityStore struct {
	mu             sync.Mutex
	nextRev        uint64
	slots          *lru.Cache[string, sessionAffinitySlot]
	responseOwners *lru.Cache[string, responseOwnerSlot]
}

func NewSessionAffinityStore() *SessionAffinityStore {
	slots, _ := lru.New[string, sessionAffinitySlot](sessionAffinityLRUSize)
	owners, _ := lru.New[string, responseOwnerSlot](responseOwnerLRUSize)
	return &SessionAffinityStore{
		slots:          slots,
		responseOwners: owners,
	}
}

func (s *SessionAffinityStore) GetSlot(key string, now time.Time) (string, uint64, bool) {
	if s == nil || key == "" {
		return "", 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	slot, ok := s.slots.Get(key)
	if !ok || !slot.expiresAt.After(now) {
		return "", 0, false
	}
	return slot.keyID, slot.rev, true
}

// ClaimIfEmpty atomically claims an empty/expired affinity slot so concurrent
// first packets for the same session coalesce onto one credential.
func (s *SessionAffinityStore) ClaimIfEmpty(key string, proposedKeyID string, ttl time.Duration, now time.Time) (string, uint64, bool) {
	if s == nil || key == "" || proposedKeyID == "" {
		return proposedKeyID, 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot, ok := s.slots.Get(key); ok && slot.expiresAt.After(now) {
		return slot.keyID, slot.rev, false
	}
	s.nextRev++
	slot := sessionAffinitySlot{
		keyID:     proposedKeyID,
		rev:       s.nextRev,
		expiresAt: now.Add(ttl),
	}
	s.slots.Add(key, slot)
	return slot.keyID, slot.rev, true
}

// CompareAndSwap rebinds an affinity slot ONLY if its revision still equals
// observedRev (or the slot has expired/been evicted), preventing stale failover
// races from overwriting a newer binding.
func (s *SessionAffinityStore) CompareAndSwap(key string, observedRev uint64, newKeyID string, ttl time.Duration, now time.Time) (string, uint64, bool) {
	if s == nil || key == "" || newKeyID == "" {
		return newKeyID, 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if slot, ok := s.slots.Get(key); ok && slot.expiresAt.After(now) {
		if slot.rev != observedRev {
			return slot.keyID, slot.rev, false
		}
		s.nextRev++
		next := sessionAffinitySlot{
			keyID:     newKeyID,
			rev:       s.nextRev,
			expiresAt: now.Add(ttl),
		}
		s.slots.Add(key, next)
		return next.keyID, next.rev, true
	}
	s.nextRev++
	next := sessionAffinitySlot{
		keyID:     newKeyID,
		rev:       s.nextRev,
		expiresAt: now.Add(ttl),
	}
	s.slots.Add(key, next)
	return next.keyID, next.rev, true
}

// Touch extends TTL when the expected bound key succeeded.
func (s *SessionAffinityStore) Touch(key string, expectedKeyID string, ttl time.Duration, now time.Time) {
	if s == nil || key == "" || expectedKeyID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	slot, ok := s.slots.Get(key)
	if !ok || slot.keyID != expectedKeyID {
		return
	}
	slot.expiresAt = now.Add(ttl)
	s.slots.Add(key, slot)
}

func responseOwnerCacheKey(projectID, ownerChannelID int, responseID string) string {
	return fmt.Sprintf("v1|%d|%d|%s", projectID, ownerChannelID, responseID)
}

func (s *SessionAffinityStore) GetResponseOwner(projectID, ownerChannelID int, responseID string, now time.Time) (string, bool) {
	if s == nil || strings.TrimSpace(responseID) == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	slot, ok := s.responseOwners.Get(responseOwnerCacheKey(projectID, ownerChannelID, strings.TrimSpace(responseID)))
	if !ok || !slot.expiresAt.After(now) {
		return "", false
	}
	return slot.keyID, true
}

func (s *SessionAffinityStore) SetResponseOwner(projectID, ownerChannelID int, responseID, keyID string, ttl time.Duration, now time.Time) {
	if s == nil || strings.TrimSpace(responseID) == "" || keyID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.responseOwners.Add(responseOwnerCacheKey(projectID, ownerChannelID, strings.TrimSpace(responseID)), responseOwnerSlot{
		keyID:     keyID,
		expiresAt: now.Add(ttl),
	})
}

// ExtractSessionAnchor extracts the first non-empty anchor value in configured
// order from the original ingress request headers or literal JSON body paths,
// falling back to the bounded prompt prefix hint when configured anchors miss.
func ExtractSessionAnchor(compiled *CompiledSessionAffinity, req *contexts.SessionAffinityRequest, fallbackHint string) (source string, value string) {
	if compiled != nil && req != nil {
		for _, anchor := range compiled.Anchors {
			switch anchor.Kind {
			case CompiledAnchorHeader:
				if req.RawHeaders != nil {
					if v := strings.TrimSpace(req.RawHeaders.Get(anchor.HeaderKey)); v != "" {
						return anchor.Raw, v
					}
				}
			case CompiledAnchorJSON:
				if len(req.RawBody) > 0 {
					res := gjson.GetBytes(req.RawBody, anchor.EscapedPath)
					if res.Exists() && res.Type == gjson.String {
						if v := strings.TrimSpace(res.String()); v != "" {
							return anchor.Raw, v
						}
					}
				}
			}
		}
	}
	if req != nil && strings.TrimSpace(req.WeakAnchorHint) != "" {
		return "hint:prefix", strings.TrimSpace(req.WeakAnchorHint)
	}
	if strings.TrimSpace(fallbackHint) != "" {
		return "hint:prefix", strings.TrimSpace(fallbackHint)
	}
	return "", ""
}

// BuildTenantSessionAffinityKey builds the tenant-isolated sticky cache key:
// v1|{projectID}|{channelOwnerID}|{routeKind}|{anchorHash}.
func BuildTenantSessionAffinityKey(projectID, channelOwnerID int, routeKind, anchorSource, anchorValue string) string {
	if anchorValue == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(anchorSource + "\x00" + anchorValue))
	return fmt.Sprintf("v1|%d|%d|%s|%s", projectID, channelOwnerID, routeKind, hex.EncodeToString(sum[:16]))
}

// SessionAffinityKeyProvider implements CPR-style admission claim, busy-overflow
// borrowing without rebinding, success-only CAS failover rebind, and response
// owner lookup while delegating candidate ranking to the channel's keySelection.
type SessionAffinityKeyProvider struct {
	channel  *Channel
	compiled *CompiledSessionAffinity
	store    *SessionAffinityStore
}

func NewSessionAffinityKeyProvider(ch *Channel, compiled *CompiledSessionAffinity) *SessionAffinityKeyProvider {
	ch.runtime()
	store := ch.sessionAffinityStore
	if store == nil {
		store = NewSessionAffinityStore()
		ch.sessionAffinityStore = store
	}
	return &SessionAffinityKeyProvider{
		channel:  ch,
		compiled: compiled,
		store:    store,
	}
}

func (p *SessionAffinityKeyProvider) Get(ctx context.Context) string {
	attempt, dec := p.selectAttempt(ctx, nil)
	if attempt == nil {
		contexts.WithChannelAPIKey(ctx, "")
		return ""
	}
	if dec != nil {
		contexts.WithSessionAffinityDecision(ctx, dec)
	}
	contexts.WithChannelCredentialEgress(ctx, attempt.Key, attempt.EgressClient, attempt.legacyFinishFunc())
	return attempt.Key
}

func (p *SessionAffinityKeyProvider) selectAttempt(ctx context.Context, excluded map[string]struct{}) (*RawHTTPCredentialAttempt, *contexts.SessionAffinityDecision) {
	ch := p.channel
	ownerID := ch.CredentialOwnerID()
	now := time.Now()
	ttl := time.Duration(p.compiled.TTLSeconds) * time.Second

	req, _ := contexts.GetSessionAffinityRequest(ctx)
	projectID := 0
	routeKind := "default"
	prevRespID := ""
	if req != nil {
		projectID = req.ProjectID
		if req.RouteKind != "" {
			routeKind = req.RouteKind
		}
		prevRespID = strings.TrimSpace(req.PreviousResponseID)
	} else if pid, ok := contexts.GetProjectID(ctx); ok {
		projectID = pid
	}
	fallbackHint, _ := contexts.GetChannelKeyAffinityID(ctx)
	if fallbackHint == "" {
		if trace, ok := contexts.GetTrace(ctx); ok && trace != nil && trace.TraceID != "" {
			fallbackHint = "trace:" + trace.TraceID
		}
	}

	anchorSource, anchorValue := ExtractSessionAnchor(p.compiled, req, fallbackHint)
	affinityKey := BuildTenantSessionAffinityKey(projectID, ownerID, routeKind, anchorSource, anchorValue)

	var prevOwnerKeyID string
	if prevRespID != "" {
		prevOwnerKeyID, _ = p.store.GetResponseOwner(projectID, ownerID, prevRespID, now)
	}

	allCandidates := ch.credentialCandidatesWithExcluded(ctx, excluded)
	runtime := ch.runtime()
	runtime.mu.Lock()
	defer runtime.mu.Unlock()

	// Classify each candidate into hard-admitted vs busy-only vs soft-healthy.
	healthy := make([]credentialCandidate, 0, len(allCandidates))
	hardAdmitted := make([]credentialCandidate, 0, len(allCandidates))
	busyHealthyByID := make(map[string]credentialCandidate)
	healthyByID := make(map[string]credentialCandidate)

	for _, cand := range allCandidates {
		cand.state = runtime.state(credentialRuntimeKey{ownerID, cand.id}, now)
		s, pol := cand.state, cand.policy
		isBusy := (pol.MaxConcurrent != nil && s.inFlight >= *pol.MaxConcurrent) ||
			(cand.proxyLimit != nil && runtime.proxyInFlight[cand.proxyID] >= *cand.proxyLimit) ||
			(pol.RequestIntervalMs != nil && now.Sub(s.lastStartedAt) < time.Duration(*pol.RequestIntervalMs)*time.Millisecond)
		isUnhealthy := s.cooldownUntil.After(now) || cand.snapshot.ExhaustedAt(now)
		if isBusy {
			if !isUnhealthy {
				busyHealthyByID[cand.id] = cand
			}
			continue
		}
		hardAdmitted = append(hardAdmitted, cand)
		if !isUnhealthy {
			healthy = append(healthy, cand)
			healthyByID[cand.id] = cand
		}
	}

	if len(hardAdmitted) == 0 {
		return nil, nil
	}

	// Highest admin priority among healthy candidates.
	topHealthy := filterHighestPriorityCandidates(healthy)
	topHealthyByID := make(map[string]credentialCandidate, len(topHealthy))
	for _, c := range topHealthy {
		topHealthyByID[c.id] = c
	}

	strategy := channelKeySelectionForProvider(ch).StrategyOrDefault()
	pickFrom := func(pool []credentialCandidate, excludeKeyID string) (credentialCandidate, bool) {
		filtered := pool
		if excludeKeyID != "" {
			filtered = make([]credentialCandidate, 0, len(pool))
			for _, c := range pool {
				if c.id != excludeKeyID {
					filtered = append(filtered, c)
				}
			}
		}
		if len(filtered) == 0 {
			return credentialCandidate{}, false
		}
		idx := rankCredentialCandidate(filtered, strategy, runtime, now)
		return filtered[idx], true
	}

	var (
		selected           credentialCandidate
		selectedOK         bool
		previousBoundKeyID string
		observedRev        uint64
		switched           bool
		busyOverflow       bool
		rebindOnSuccess    bool
	)

	// 1. Continuation owner preference: if previous_response_id maps to an
	// eligible key in the channel, pin THIS continuation request to that owner
	// without overwriting the stable session binding.
	if prevOwnerKeyID != "" {
		if cand, ok := healthyByID[prevOwnerKeyID]; ok {
			selected = cand
			selectedOK = true
			if boundID, rev, hasSlot := p.store.GetSlot(affinityKey, now); hasSlot {
				previousBoundKeyID = boundID
				observedRev = rev
				if boundID != prevOwnerKeyID {
					switched = true
				}
			}
		}
	}

	strictFailoverRejected := false
	// 2. Affinity slot resolution (CPR claim / busy-overflow / strict failover).
	if !selectedOK && affinityKey != "" {
		boundID, rev, hasSlot := p.store.GetSlot(affinityKey, now)
		if !hasSlot {
			// Empty/expired slot: rank healthy candidates (fallback to hard-admitted)
			// and atomically claim the initial binding.
			pool := topHealthy
			if len(pool) == 0 {
				pool = filterHighestPriorityCandidates(hardAdmitted)
			}
			proposed, ok := pickFrom(pool, "")
			if ok {
				effectiveID, claimedRev, _ := p.store.ClaimIfEmpty(affinityKey, proposed.id, ttl, now)
				observedRev = claimedRev
				previousBoundKeyID = effectiveID
				if effectiveID != proposed.id {
					if raced, racedOK := topHealthyByID[effectiveID]; racedOK {
						selected = raced
						selectedOK = true
					} else if raced, racedOK := healthyByID[effectiveID]; racedOK {
						selected = raced
						selectedOK = true
					}
				}
				if !selectedOK {
					selected = proposed
					selectedOK = true
				}
			}
		} else {
			previousBoundKeyID = boundID
			observedRev = rev
			if boundCand, ok := topHealthyByID[boundID]; ok {
				// Bound key is healthy and in top priority tier: stick to it.
				selected = boundCand
				selectedOK = true
			} else if _, isBusyOnly := busyHealthyByID[boundID]; isBusyOnly {
				// Bound key is healthy and only experiencing transient busy overflow:
				// borrow a healthy alternate WITHOUT rebinding the affinity slot.
				if alt, ok := pickFrom(topHealthy, boundID); ok {
					selected = alt
					selectedOK = true
					busyOverflow = true
					rebindOnSuccess = false
					switched = true
				} else if alt, ok := pickFrom(filterHighestPriorityCandidates(hardAdmitted), boundID); ok {
					selected = alt
					selectedOK = true
					busyOverflow = true
					rebindOnSuccess = false
					switched = true
				}
			} else {
				// Bound key is in cooldown / exhausted / disabled / excluded:
				// pick strict healthy replacement (allowSoftFallback=false)
				// and defer CAS rebind until upstream confirms success.
				if alt, ok := pickFrom(topHealthy, boundID); ok {
					selected = alt
					selectedOK = true
					switched = true
					rebindOnSuccess = true
				} else {
					strictFailoverRejected = true
				}
			}
		}
	}

	// 3. Fallback when no affinity key exists (never override strict failover rejection
	// with cooldown/exhausted keys).
	if !selectedOK && !strictFailoverRejected {
		pool := topHealthy
		if len(pool) == 0 {
			pool = filterHighestPriorityCandidates(hardAdmitted)
		}
		if alt, ok := pickFrom(pool, ""); ok {
			selected = alt
			selectedOK = true
			if previousBoundKeyID != "" && previousBoundKeyID != selected.id {
				switched = true
				if !busyOverflow {
					rebindOnSuccess = true
				}
			}
		}
	}

	if !selectedOK {
		return nil, nil
	}

	ownership := contexts.SessionStateOwnershipUnknown
	if prevOwnerKeyID != "" {
		if prevOwnerKeyID == selected.id {
			ownership = contexts.SessionStateOwnershipSame
		} else {
			ownership = contexts.SessionStateOwnershipDifferent
		}
	}

	dec := &contexts.SessionAffinityDecision{
		AffinityKey:        affinityKey,
		AnchorSource:       anchorSource,
		AnchorValue:        anchorValue,
		SelectedKeyID:      selected.id,
		PreviousBoundKeyID: previousBoundKeyID,
		PreviousOwnerKeyID: prevOwnerKeyID,
		StateOwnership:     ownership,
		ObservedRev:        observedRev,
		Switched:           switched,
		BusyOverflow:       busyOverflow,
		RebindOnSuccess:    rebindOnSuccess,
	}

	store := p.store
	dec.SetOnResponseOwner(func(responseID string) {
		if responseID != "" {
			store.SetResponseOwner(projectID, ownerID, responseID, selected.id, ttl, time.Now())
		}
	})
	dec.SetOnSuccess(func(responseID string) {
		confirmNow := time.Now()
		if responseID != "" {
			store.SetResponseOwner(projectID, ownerID, responseID, selected.id, ttl, confirmNow)
		}
		if affinityKey == "" || busyOverflow {
			return
		}
		if rebindOnSuccess {
			store.CompareAndSwap(affinityKey, observedRev, selected.id, ttl, confirmNow)
		} else {
			store.Touch(affinityKey, selected.id, ttl, confirmNow)
		}
	})

	return ch.buildCredentialAttempt(ctx, selected, now), dec
}

func filterHighestPriorityCandidates(candidates []credentialCandidate) []credentialCandidate {
	if len(candidates) == 0 {
		return nil
	}
	priority := candidates[0].policy.Priority
	for _, c := range candidates[1:] {
		priority = max(priority, c.policy.Priority)
	}
	out := make([]credentialCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c.policy.Priority == priority {
			out = append(out, c)
		}
	}
	return out
}

func rankCredentialCandidate(candidates []credentialCandidate, strategy objects.ChannelKeySelectionStrategy, runtime *credentialRuntime, now time.Time) int {
	if len(candidates) <= 1 {
		return 0
	}
	switch strategy {
	case objects.ChannelKeySelectionStrategyRandom:
		total := 0
		for _, candidate := range candidates {
			total += candidate.policy.Weight
		}
		if total <= 0 {
			return rand.IntN(len(candidates))
		}
		point := rand.IntN(total)
		for i, candidate := range candidates {
			point -= candidate.policy.Weight
			if point < 0 {
				return i
			}
		}
		return 0
	case objects.ChannelKeySelectionStrategyQuotaResetPriority:
		winner := 0
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
			au, bu := candidateUtilization(a), candidateUtilization(b)
			if au < bu || au == bu && a.state.lastStartedAt.Before(b.state.lastStartedAt) {
				winner = i
			}
		}
		return winner
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
		return band[(runtime.cursor.Add(1)-1)%uint64(len(band))]
	default:
		return weightedCredentialRoundRobin(candidates)
	}
}

func candidateUtilization(candidate credentialCandidate) float64 {
	if candidate.policy.MaxConcurrent != nil && *candidate.policy.MaxConcurrent > 0 {
		return float64(candidate.state.inFlight) / float64(*candidate.policy.MaxConcurrent)
	}
	return float64(candidate.state.inFlight)
}

var _ = slices.Contains[[]string, string]
