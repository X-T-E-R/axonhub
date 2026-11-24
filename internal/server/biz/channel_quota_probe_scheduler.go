package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/singleflight"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/cluster"
)

const (
	maxQuotaProbeKeysPerChannelPerTick = 8
	maxInFlightQuotaProbesPerOwner     = 4
	defaultRollingQuotaProbeInterval   = 5 * time.Minute
	defaultWalletQuotaProbeInterval    = 15 * time.Minute
	quotaProbeErrorBaseBackoff         = 5 * time.Minute
	quotaProbeErrorMaxBackoff          = 60 * time.Minute
	quotaProbeUnsupportedTTL           = 6 * time.Hour
	quotaProbeCASMaxRetries            = 5
)

// quotaProbeOriginInterval controls the minimum spacing between probe request
// starts to the same origin per owner (default 1 req/s). Package var so unit
// tests can override for deterministic fast tests while still testing pacing.
var quotaProbeOriginInterval = time.Second

// ChannelUsesPerKeyQuotaAuthority is the single source of truth determining
// whether a channel's quota is owned by ChannelBalanceProbe (per-key / shared
// wallet path) or legacy channel-scoped ProviderQuotaService.
//
// Invariants:
//   - OAuth channels and opencode_go cookie-scraped channels stay on ProviderQuotaService.
//   - Multi-key API-key pools (len(APIKeys) > 1, including Ch 51) ALWAYS belong to
//     the per-key path and MUST NEVER fall back to ProviderQuotaService APIKeys[0].
//   - Any API-key channel with a per-key balance probe preset/domain/config belongs
//     to the per-key path.
//   - Single-credential API-key channels that lack a per-key balance probe adapter
//     (e.g. single-key cline/wafer/synthetic/neuralwatt/apertis/github_copilot)
//     stay on ProviderQuotaService.
func ChannelUsesPerKeyQuotaAuthority(ch *ent.Channel) bool {
	if ch == nil {
		return false
	}
	if ch.Credentials.IsOAuth() {
		return false
	}
	if ch.Type == channel.TypeOpencodeGo || ch.Type == channel.TypeOpencodeGoAnthropic {
		return false
	}
	allKeys := ch.Credentials.GetAllAPIKeys()
	if len(allKeys) > 1 {
		return true
	}
	if ch.Settings != nil && ch.Settings.BalanceProbe != nil {
		return true
	}
	// Single-key API-key channels with persisted manual/balance snapshots (e.g. Ch 73/74/76
	// static $100 ledger) belong on the per-key projection so their ledger is never dropped.
	if ch.Settings != nil && ch.Settings.KeyHealthCheck != nil {
		if ch.Settings.KeyHealthCheck.SharedBalanceSnapshot != nil {
			return true
		}
		for _, meta := range ch.Settings.KeyHealthCheck.KeyMetadata {
			if meta.BalanceSnapshot != nil {
				return true
			}
		}
	}
	if _, _, ok := channelBalanceProbeSpecForChannel(ch); ok {
		return true
	}
	return false
}

// channelOwnedByUnifiedQuotaScheduler reports whether ch's scheduled balance
// probes are owned by runDueQuotaBalanceProbes (bounded per-tick quota scheduler),
// so legacy scheduled health-check/monitoring loops skip channel_balance_probe rules
// and never double-probe the same channel in one tick.
func channelOwnedByUnifiedQuotaScheduler(ch *ent.Channel) bool {
	if ch == nil || !ChannelUsesPerKeyQuotaAuthority(ch) {
		return false
	}
	return ch.Settings != nil && ch.Settings.BalanceProbe != nil && ch.Settings.BalanceProbe.Enabled
}

var errQuotaProbeStaleFence = errors.New("quota probe observation discarded: channel ownership or probe configuration changed")

type quotaProbeCoordinator struct {
	flight        singleflight.Group
	refreshFlight singleflight.Group

	semOnce sync.Once
	sem     chan struct{}

	originMu        sync.Mutex
	originNextStart map[string]time.Time

	unsupportedMu    sync.Mutex
	unsupportedUntil map[string]time.Time
}

func (c *quotaProbeCoordinator) ensureInit() {
	c.semOnce.Do(func() {
		c.sem = make(chan struct{}, maxInFlightQuotaProbesPerOwner)
	})
}

func (c *quotaProbeCoordinator) acquireSlot(ctx context.Context) (func(), error) {
	c.ensureInit()
	select {
	case c.sem <- struct{}{}:
		return func() { <-c.sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *quotaProbeCoordinator) waitOriginRateLimit(ctx context.Context, origin string) error {
	interval := quotaProbeOriginInterval
	if interval <= 0 || origin == "" {
		return nil
	}
	c.originMu.Lock()
	if c.originNextStart == nil {
		c.originNextStart = make(map[string]time.Time)
	}
	now := time.Now()
	nextAllowed := c.originNextStart[origin]
	var wait time.Duration
	if nextAllowed.After(now) {
		wait = nextAllowed.Sub(now)
		c.originNextStart[origin] = nextAllowed.Add(interval)
	} else {
		c.originNextStart[origin] = now.Add(interval)
	}
	c.originMu.Unlock()

	if wait <= 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *quotaProbeCoordinator) isUnsupportedCached(subjectKey string, now time.Time) (time.Time, bool) {
	c.unsupportedMu.Lock()
	defer c.unsupportedMu.Unlock()
	if c.unsupportedUntil == nil {
		return time.Time{}, false
	}
	until, ok := c.unsupportedUntil[subjectKey]
	if !ok || !until.After(now) {
		return time.Time{}, false
	}
	return until, true
}

func (c *quotaProbeCoordinator) markUnsupported(subjectKey string, until time.Time) {
	c.unsupportedMu.Lock()
	defer c.unsupportedMu.Unlock()
	if c.unsupportedUntil == nil {
		c.unsupportedUntil = make(map[string]time.Time)
	}
	c.unsupportedUntil[subjectKey] = until
}

func (c *quotaProbeCoordinator) clearUnsupported(subjectKey string) {
	c.unsupportedMu.Lock()
	defer c.unsupportedMu.Unlock()
	delete(c.unsupportedUntil, subjectKey)
}

func quotaProbeCadenceForChannel(ch *ent.Channel) time.Duration {
	spec, _, ok := channelBalanceProbeSpecForChannel(ch)
	if ok && spec.RollingWindow {
		return defaultRollingQuotaProbeInterval
	}
	return defaultWalletQuotaProbeInterval
}

func nextQuotaProbeTimeForResult(ch *ent.Channel, result ChannelKeyHealthCheckResult, prevFailures int, now time.Time) *time.Time {
	if result.ObservationValid {
		next := now.Add(quotaProbeCadenceForChannel(ch))
		return &next
	}
	if result.ProbeStatus == "unsupported" || result.Unsupported {
		next := now.Add(quotaProbeUnsupportedTTL)
		return &next
	}
	failures := prevFailures + 1
	multiplier := 1 << min(failures-1, 4) // 1, 2, 4, 8, 16
	backoff := min(quotaProbeErrorBaseBackoff*time.Duration(multiplier), quotaProbeErrorMaxBackoff)
	next := now.Add(backoff)
	if result.RetryAfter != nil && result.RetryAfter.After(next) {
		t := *result.RetryAfter
		next = t
	}
	return &next
}

func probeConfigFingerprint(ch *ent.Channel) string {
	if ch == nil {
		return ""
	}
	spec, probe, ok := channelBalanceProbeSpecForChannel(ch)
	if !ok {
		return "none"
	}
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte(p))
			_, _ = h.Write([]byte{0})
		}
	}
	write(
		string(spec.ID),
		string(ch.Type),
		strings.TrimSpace(ch.BaseURL),
		fmt.Sprintf("shared=%v", isChannelSharedWallet(ch)),
		fmt.Sprintf("credSrc=%d", credentialSourceID(ch.Settings)),
	)
	if probe != nil {
		write(
			fmt.Sprintf("enabled=%v", probe.Enabled),
			fmt.Sprintf("exp=%v", probe.Experimental),
			fmt.Sprintf("timeout=%d", probe.TimeoutMs),
			probe.PreferredCurrency,
			string(probe.PrimarySelection),
			probe.ProbeAPIKeyCiphertext,
		)
		if probe.HTTP != nil {
			if b, err := json.Marshal(probe.HTTP); err == nil {
				_, _ = h.Write(b)
			}
		}
		if probe.Script != nil {
			if b, err := json.Marshal(probe.Script); err == nil {
				_, _ = h.Write(b)
			}
		}
	}
	if ch.Settings != nil && len(ch.Settings.CredentialPolicies) > 0 {
		for _, pol := range ch.Settings.CredentialPolicies {
			if pol.ManualQuotaLimit != nil {
				write("pol", pol.ID, fmt.Sprintf("%.6f", *pol.ManualQuotaLimit))
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// runChannelBalanceProbeViaCoordinator routes legacy balance-probe entrypoints
// (channel_monitoring, manual health check modes, and rule-based balance probes)
// through the unified singleflight, concurrency, and origin-pacing coordinator.
func (svc *ChannelService) runChannelBalanceProbeViaCoordinator(
	ctx context.Context,
	ch *ent.Channel,
	rawKey string,
	trigger objects.ChannelKeyHealthCheckTrigger,
	force bool,
) ChannelKeyHealthCheckResult {
	if ch == nil {
		return ChannelKeyHealthCheckResult{Success: false, ProbeStatus: "error", Reason: "channel is nil"}
	}
	subjectKey := fmt.Sprintf("ch:%d:key:%s", ch.ID, objects.ChannelAPIKeyFingerprint(rawKey))
	if isChannelSharedWallet(ch) {
		subjectKey = fmt.Sprintf("ch:%d:wallet", ch.ID)
	}
	res, _, err := svc.runCoalescedQuotaProbe(ctx, ch, subjectKey, rawKey, trigger, force)
	if err != nil {
		return ChannelKeyHealthCheckResult{
			Success:          false,
			ObservationValid: false,
			ProbeStatus:      "error",
			Reason:           err.Error(),
			Source:           balanceProbeEventSource(trigger, false),
		}
	}
	return res
}

func probeOriginForChannel(ch *ent.Channel) string {
	spec, probe, ok := channelBalanceProbeSpecForChannel(ch)
	if !ok {
		return ""
	}
	rule := spec.HTTP
	if spec.ID != objects.ChannelBalanceProbePresetExa && probe != nil && probe.HTTP != nil {
		rule = *probe.HTTP
	}
	rawURL := rule.URL
	if rule.URLMode != objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL {
		rawURL = ch.BaseURL
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed == nil || parsed.Host == "" {
		return ""
	}
	return strings.ToLower(parsed.Scheme + "://" + parsed.Host)
}

func (svc *ChannelService) runCoalescedQuotaProbe(
	ctx context.Context,
	ch *ent.Channel,
	subjectKey string,
	rawKey string,
	trigger objects.ChannelKeyHealthCheckTrigger,
	force bool,
) (ChannelKeyHealthCheckResult, bool, error) {
	now := time.Now().UTC()
	if !force {
		if until, cached := svc.quotaProbeCoord.isUnsupportedCached(subjectKey, now); cached {
			return ChannelKeyHealthCheckResult{
				Success:          false,
				Unsupported:      true,
				ObservationValid: false,
				ProbeStatus:      "unsupported",
				Reason:           "endpoint recently reported unsupported",
				RetryAfter:       &until,
				Source:           balanceProbeEventSource(trigger, false),
			}, false, nil
		}
	}

	executed := false
	resultCh := svc.quotaProbeCoord.flight.DoChan(subjectKey, func() (any, error) {
		executed = true
		// Recheck placement ownership immediately before issuing outbound HTTP.
		if !svc.node.Owns(ch.ID) {
			return nil, svc.ChannelOwnerError(ch.ID)
		}
		release, err := svc.quotaProbeCoord.acquireSlot(ctx)
		if err != nil {
			return nil, err
		}
		defer release()

		// Recheck placement ownership after waiting for concurrency semaphore.
		// Origin pacing is enforced at every actual outbound HTTP start in executeChannelKeyProbeHTTP.
		if !svc.node.Owns(ch.ID) {
			return nil, svc.ChannelOwnerError(ch.ID)
		}

		res := svc.runChannelKeyBalanceProbe(ctx, ch, rawKey, trigger)
		if res.ProbeStatus == "unsupported" || res.Unsupported {
			svc.quotaProbeCoord.markUnsupported(subjectKey, time.Now().UTC().Add(quotaProbeUnsupportedTTL))
		} else if res.ObservationValid {
			svc.quotaProbeCoord.clearUnsupported(subjectKey)
		}
		return res, nil
	})

	select {
	case <-ctx.Done():
		return ChannelKeyHealthCheckResult{}, false, ctx.Err()
	case fr := <-resultCh:
		if fr.Err != nil {
			return ChannelKeyHealthCheckResult{}, executed, fr.Err
		}
		res, ok := fr.Val.(ChannelKeyHealthCheckResult)
		if !ok {
			return ChannelKeyHealthCheckResult{}, executed, errors.New("invalid coalesced quota probe result")
		}
		return res, executed, nil
	}
}

type quotaKeyProbeUpdate struct {
	RawKey string
	KeyID  string
	Result ChannelKeyHealthCheckResult
}

// persistQuotaProbeUpdates writes ONLY quota/probe fields using optimistic
// UpdatedAt CAS. On conflict from an unrelated settings edit, it rereads the
// latest row and merges ONLY the quota fields if ownership, subject, and probe
// config still match—never reissuing HTTP.
func (svc *ChannelService) persistQuotaProbeUpdates(
	ctx context.Context,
	ch *ent.Channel,
	expectedFingerprint string,
	sharedUpdate *ChannelKeyHealthCheckResult,
	keyUpdates []quotaKeyProbeUpdate,
	now time.Time,
) error {
	if ch == nil || (sharedUpdate == nil && len(keyUpdates) == 0) {
		return nil
	}
	// Detach from a potentially expiring cron/request context so freshly fetched
	// HTTP probe observations are still persisted cleanly with a bounded 10s budget,
	// while still enforcing ownership and probe-config fencing on every CAS retry.
	persistCtx, cancel := xcontext.DetachWithTimeout(ctx, 10*time.Second)
	defer cancel()

	db := svc.entFromContext(persistCtx)
	current := ch

	for attempt := range quotaProbeCASMaxRetries {
		if !svc.node.Owns(current.ID) {
			return errQuotaProbeStaleFence
		}
		if credentialSourceID(current.Settings) != 0 {
			return errQuotaProbeStaleFence
		}
		if probeConfigFingerprint(current) != expectedFingerprint {
			return errQuotaProbeStaleFence
		}

		settings := ensureChannelKeyHealthCheckSettings(current.Settings)
		activeKeys := make(map[string]string)
		for _, k := range current.Credentials.GetAllAPIKeys() {
			activeKeys[objects.ChannelAPIKeyFingerprint(k)] = k
		}

		if sharedUpdate != nil {
			prevFailures := settings.KeyHealthCheck.SharedProbeFailureCount
			nextAt := nextQuotaProbeTimeForResult(current, *sharedUpdate, prevFailures, now)
			probeAt := now
			settings.KeyHealthCheck.SharedProbeAt = &probeAt
			settings.KeyHealthCheck.SharedNextProbeAt = nextAt
			if sharedUpdate.ObservationValid && sharedUpdate.BalanceSnapshot != nil && sharedUpdate.BalanceSnapshot.Success {
				settings.KeyHealthCheck.SharedBalanceSnapshot = cloneChannelKeyBalanceSnapshot(sharedUpdate.BalanceSnapshot)
				settings.KeyHealthCheck.SharedProbeStatus = "ok"
				settings.KeyHealthCheck.SharedProbeError = ""
				settings.KeyHealthCheck.SharedProbeFailureCount = 0
			} else {
				status := sharedUpdate.ProbeStatus
				if status == "" {
					status = "error"
				}
				settings.KeyHealthCheck.SharedProbeStatus = status
				settings.KeyHealthCheck.SharedProbeError = sharedUpdate.Reason
				settings.KeyHealthCheck.SharedProbeFailureCount = prevFailures + 1
			}
		}

		appliedKeys := make([]string, 0, len(keyUpdates))
		appliedResults := make([]ChannelKeyHealthCheckResult, 0, len(keyUpdates))
		for _, upd := range keyUpdates {
			if _, exists := activeKeys[upd.KeyID]; !exists {
				continue
			}
			appliedKeys = append(appliedKeys, upd.RawKey)
			appliedResults = append(appliedResults, upd.Result)
		}
		if len(appliedKeys) > 0 {
			trigger := objects.ChannelKeyHealthCheckTriggerScheduled
			if appliedResults[0].Source == objects.FailurePolicyEventSourceManualBalanceProbe ||
				appliedResults[0].Source == objects.FailurePolicyEventSourceManualBalanceProbeFailure {
				trigger = objects.ChannelKeyHealthCheckTriggerManual
			}
			// Only valid observations can match an explicit numeric-balance policy rule.
			appliedResults = svc.applyFailurePolicyToHealthCheckResults(persistCtx, current, settings, appliedKeys, appliedResults, now, trigger, false)
			for i, rawKey := range appliedKeys {
				nextAt := nextQuotaProbeTimeForResult(current, appliedResults[i], existingKeyProbeFailureCount(settings.KeyHealthCheck.KeyMetadata, rawKey), now)
				settings.KeyHealthCheck.KeyMetadata = upsertChannelKeyBalanceProbeMetadata(settings.KeyHealthCheck.KeyMetadata, rawKey, appliedResults[i], now, nextAt)
			}
		}
		mergeChannelKeyOperationalStatus(current, settings.KeyHealthCheck)

		_, err := channelAPIKeyStateUpdate(db, current).SetSettings(settings).Save(persistCtx)
		if err == nil {
			if len(appliedKeys) > 0 {
				if actionErr := svc.applyChannelKeyHealthCheckPolicyActions(persistCtx, current.ID, appliedKeys, appliedResults, false); actionErr != nil {
					log.Warn(persistCtx, "failed to apply explicit balance policy actions", log.Int("channel_id", current.ID), log.Cause(actionErr))
				}
			}
			svc.asyncReloadChannels()
			return nil
		}
		if !ent.IsNotFound(err) || attempt == quotaProbeCASMaxRetries-1 {
			return fmt.Errorf("failed to persist quota probe updates: %w", err)
		}
		// CAS conflict: reread latest channel row and retry quota-only merge without reissuing HTTP.
		latest, getErr := db.Channel.Get(persistCtx, current.ID)
		if getErr != nil {
			if ent.IsNotFound(getErr) {
				return nil
			}
			return fmt.Errorf("failed to reread channel after quota CAS conflict: %w", getErr)
		}
		current = latest
	}
	return nil
}

// runDueQuotaBalanceProbes executes bounded automatic balance probes for enabled
// channels with balanceProbe.enabled == true owned by this node.
func (svc *ChannelService) runDueQuotaBalanceProbes(ctx context.Context, now time.Time) (int, int, error) {
	channels, err := svc.entFromContext(ctx).Channel.Query().
		Where(channel.StatusEQ(channel.StatusEnabled)).
		All(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("query channels for scheduled balance probes: %w", err)
	}

	var updatedTotal, failedTotal int
	var mu sync.Mutex

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(maxInFlightQuotaProbesPerOwner)

	for _, ch := range channels {
		if ch == nil || credentialSourceID(ch.Settings) != 0 || !svc.node.Owns(ch.ID) {
			continue
		}
		if !ChannelUsesPerKeyQuotaAuthority(ch) {
			continue
		}
		// Strictly respect explicit balanceProbe.enabled (frozen enabled:false stays off).
		if ch.Settings == nil || ch.Settings.BalanceProbe == nil || !ch.Settings.BalanceProbe.Enabled {
			continue
		}
		if _, _, ok := channelBalanceProbeSpecForChannel(ch); !ok {
			continue
		}

		chCopy := ch
		group.Go(func() error {
			upd, fail, err := svc.runDueQuotaBalanceProbeForChannel(groupCtx, chCopy, now)
			if err != nil {
				log.Warn(groupCtx, "scheduled balance probe failed", log.Int("channel_id", chCopy.ID), log.Cause(err))
				return nil
			}
			mu.Lock()
			updatedTotal += upd
			failedTotal += fail
			mu.Unlock()
			return nil
		})
	}

	if err := group.Wait(); err != nil {
		return updatedTotal, failedTotal, err
	}
	return updatedTotal, failedTotal, nil
}

func (svc *ChannelService) runDueQuotaBalanceProbeForChannel(ctx context.Context, ch *ent.Channel, now time.Time) (int, int, error) {
	fingerprint := probeConfigFingerprint(ch)

	if isChannelSharedWallet(ch) {
		if ch.Settings != nil && ch.Settings.KeyHealthCheck != nil &&
			ch.Settings.KeyHealthCheck.SharedNextProbeAt != nil &&
			now.Before(*ch.Settings.KeyHealthCheck.SharedNextProbeAt) {
			return 0, 0, nil
		}
		rawKey := ""
		if keys := channelBalanceProbeCandidateKeys(ch); len(keys) > 0 {
			rawKey = keys[0]
		}
		subjectKey := fmt.Sprintf("ch:%d:wallet", ch.ID)
		res, executed, err := svc.runCoalescedQuotaProbe(ctx, ch, subjectKey, rawKey, objects.ChannelKeyHealthCheckTriggerScheduled, false)
		if err != nil {
			return 0, 1, err
		}
		if executed {
			if err := svc.persistQuotaProbeUpdates(ctx, ch, fingerprint, &res, nil, now); err != nil {
				if errors.Is(err, errQuotaProbeStaleFence) {
					return 0, 0, nil
				}
				return 0, 1, err
			}
		}
		if res.ObservationValid {
			return 1, 0, nil
		}
		return 0, 1, nil
	}

	dueKeys := selectDueQuotaProbeKeys(ch, now, maxQuotaProbeKeysPerChannelPerTick)
	if len(dueKeys) == 0 {
		return 0, 0, nil
	}

	updates := make([]quotaKeyProbeUpdate, 0, len(dueKeys))
	var mu sync.Mutex
	var failed int
	eg, egCtx := errgroup.WithContext(ctx)
	eg.SetLimit(min(maxInFlightQuotaProbesPerOwner, len(dueKeys)))

	for _, rawKey := range dueKeys {
		k := rawKey
		keyID := objects.ChannelAPIKeyFingerprint(k)
		subjectKey := fmt.Sprintf("ch:%d:key:%s", ch.ID, keyID)
		eg.Go(func() error {
			res, executed, err := svc.runCoalescedQuotaProbe(egCtx, ch, subjectKey, k, objects.ChannelKeyHealthCheckTriggerScheduled, false)
			if err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil
				}
				mu.Lock()
				failed++
				mu.Unlock()
				return nil
			}
			mu.Lock()
			if executed {
				updates = append(updates, quotaKeyProbeUpdate{RawKey: k, KeyID: keyID, Result: res})
			}
			if !res.ObservationValid {
				failed++
			}
			mu.Unlock()
			return nil
		})
	}
	_ = eg.Wait()

	if len(updates) == 0 {
		return 0, failed, nil
	}
	if err := svc.persistQuotaProbeUpdates(ctx, ch, fingerprint, nil, updates, now); err != nil {
		if errors.Is(err, errQuotaProbeStaleFence) {
			return 0, 0, nil
		}
		return 0, failed, err
	}
	return len(updates) - failed, failed, nil
}

func channelBalanceProbeCandidateKeys(ch *ent.Channel) []string {
	if ch == nil {
		return nil
	}
	includeStatuses := []objects.ChannelKeyStatus{objects.ChannelKeyStatusActive}
	if ch.Settings != nil && ch.Settings.BalanceProbe != nil && len(ch.Settings.BalanceProbe.IncludeStatuses) > 0 {
		includeStatuses = ch.Settings.BalanceProbe.IncludeStatuses
	}
	targets := monitoringRuleTargetKeys(ch, includeStatuses)
	keys := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.RawKey != "" {
			keys = append(keys, t.RawKey)
		}
	}
	return keys
}

// selectDueQuotaProbeKeys returns up to maxKeys due keys ordered oldest-due-first
// so large pools (e.g. 84 OpenRouter keys) make bounded forward progress every tick
// without starving tail keys.
func selectDueQuotaProbeKeys(ch *ent.Channel, now time.Time, maxKeys int) []string {
	allKeys := channelBalanceProbeCandidateKeys(ch)
	if len(allKeys) == 0 {
		return nil
	}
	metaByID := map[string]objects.ChannelKeyMetadata{}
	if ch.Settings != nil && ch.Settings.KeyHealthCheck != nil {
		metaByID = channelKeyMetadataByID(ch.Settings.KeyHealthCheck.KeyMetadata)
	}
	cadence := quotaProbeCadenceForChannel(ch)

	type dueItem struct {
		key   string
		dueAt time.Time
		index int
	}
	due := make([]dueItem, 0, len(allKeys))
	for idx, k := range allKeys {
		id := objects.ChannelAPIKeyFingerprint(k)
		meta, ok := metaByID[id]
		if !ok {
			due = append(due, dueItem{key: k, dueAt: time.Time{}, index: idx})
			continue
		}
		if meta.NextProbeAt != nil {
			if !now.Before(*meta.NextProbeAt) {
				due = append(due, dueItem{key: k, dueAt: *meta.NextProbeAt, index: idx})
			}
			continue
		}
		var lastAt time.Time
		if meta.LastProbeAt != nil {
			lastAt = *meta.LastProbeAt
		} else if meta.BalanceSnapshot != nil && !meta.BalanceSnapshot.CheckedAt.IsZero() && meta.BalanceSnapshot.Source != "manual" {
			lastAt = meta.BalanceSnapshot.CheckedAt
		}
		if lastAt.IsZero() {
			due = append(due, dueItem{key: k, dueAt: time.Time{}, index: idx})
			continue
		}
		nextDue := lastAt.Add(cadence)
		if !now.Before(nextDue) {
			due = append(due, dueItem{key: k, dueAt: nextDue, index: idx})
		}
	}
	sort.Slice(due, func(i, j int) bool {
		if !due[i].dueAt.Equal(due[j].dueAt) {
			return due[i].dueAt.Before(due[j].dueAt)
		}
		return due[i].index < due[j].index
	})
	if maxKeys > 0 && len(due) > maxKeys {
		due = due[:maxKeys]
	}
	out := make([]string, len(due))
	for i, item := range due {
		out[i] = item.key
	}
	return out
}

type RefreshChannelQuotaStatus string

const (
	RefreshChannelQuotaStatusUpdated     RefreshChannelQuotaStatus = "updated"
	RefreshChannelQuotaStatusDeferred    RefreshChannelQuotaStatus = "deferred"
	RefreshChannelQuotaStatusUnsupported RefreshChannelQuotaStatus = "unsupported"
	RefreshChannelQuotaStatusError       RefreshChannelQuotaStatus = "error"
)

type RefreshChannelQuotaPayload struct {
	Status        RefreshChannelQuotaStatus `json:"status"`
	Message       string                    `json:"message,omitempty"`
	UpdatedKeys   int                       `json:"updatedKeys"`
	DeferredKeys  int                       `json:"deferredKeys"`
	RetryAt       *time.Time                `json:"retryAt,omitempty"`
	QuotaOverview *ChannelQuotaOverview     `json:"quotaOverview,omitempty"`
}

type OwnerChannelQuotaRefreshRequest struct {
	ChannelID int      `json:"channel_id"`
	KeyIDs    []string `json:"key_ids,omitempty"`
}

// RefreshChannelQuota performs an owner-routed, bounded, singleflight-coalesced
// quota refresh for a single channel (and optional keyIDs). It strictly enforces
// write_channels permission, never sends inference requests, never triggers
// Codex credit redemption, and never echoes secrets.
func (svc *ChannelService) RefreshChannelQuota(
	ctx context.Context,
	channelID int,
	keyIDs []string,
	providerQuotaSvc *ProviderQuotaService,
) (*RefreshChannelQuotaPayload, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeWriteChannels); err != nil {
		return nil, err
	}
	ctx = authz.WithScopeDecision(ctx, scopes.ScopeWriteChannels)

	db := svc.entFromContext(ctx)
	ch, err := db.Channel.Get(ctx, channelID)
	if err != nil {
		return nil, fmt.Errorf("failed to get channel: %w", err)
	}
	ownerCh, err := resolveCredentialOwner(ctx, db, ch)
	if err != nil {
		return nil, err
	}
	targetID := ownerCh.ID

	if !svc.node.Owns(targetID) {
		var payload RefreshChannelQuotaPayload
		// Never include any secret or ProbeAPIKey in owner-dispatch payload.
		err := cluster.CallOwner(ctx, svc.node, targetID, "/__axon/admin/channel-quota-refresh", OwnerChannelQuotaRefreshRequest{
			ChannelID: channelID,
			KeyIDs:    keyIDs,
		}, &payload)
		if err != nil {
			var statusErr *cluster.OwnerHTTPStatusError
			if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
				return &RefreshChannelQuotaPayload{
					Status:        RefreshChannelQuotaStatusDeferred,
					Message:       "owner node upgrading",
					QuotaOverview: svc.ResolveChannelQuotaOverview(ctx, ch, providerQuotaSvc),
				}, nil
			}
			return nil, err
		}
		return &payload, nil
	}

	now := time.Now().UTC()

	// Route channel-scoped OAuth / cookie / legacy single-key channels to ProviderQuotaService.
	if !ChannelUsesPerKeyQuotaAuthority(ownerCh) {
		if providerQuotaSvc == nil {
			return &RefreshChannelQuotaPayload{
				Status:        RefreshChannelQuotaStatusUnsupported,
				Message:       "provider quota service is not available",
				QuotaOverview: svc.BuildChannelQuotaOverview(ctx, ch, ownerCh, nil, now),
			}, nil
		}
		status, msg, pqs, retryAt := providerQuotaSvc.RefreshSingleChannel(ctx, ownerCh)
		return &RefreshChannelQuotaPayload{
			Status:        status,
			Message:       msg,
			UpdatedKeys:   loTernaryInt(status == RefreshChannelQuotaStatusUpdated, 1, 0),
			RetryAt:       retryAt,
			QuotaOverview: svc.buildChannelQuotaOverviewWithService(ctx, ch, ownerCh, pqs, providerQuotaSvc, now),
		}, nil
	}

	// Per-key / shared-wallet authority path (ChannelBalanceProbe).
	_, _, supported := channelBalanceProbeSpecForChannel(ownerCh)
	if !supported {
		return &RefreshChannelQuotaPayload{
			Status:        RefreshChannelQuotaStatusUnsupported,
			Message:       "channel does not have a supported balance probe preset",
			QuotaOverview: svc.BuildChannelQuotaOverview(ctx, ch, ownerCh, nil, now),
		}, nil
	}

	fingerprint := probeConfigFingerprint(ownerCh)
	sortedKeys := slices.Clone(keyIDs)
	sort.Strings(sortedKeys)
	refreshFlightKey := fmt.Sprintf("refresh:%d:%s:%s", ownerCh.ID, fingerprint, strings.Join(sortedKeys, ","))

	val, err, _ := svc.quotaProbeCoord.refreshFlight.Do(refreshFlightKey, func() (any, error) {
		return svc.executeManualPerKeyRefresh(ctx, db, ch, ownerCh, fingerprint, keyIDs, now)
	})
	if err != nil {
		return nil, err
	}
	payload, ok := val.(*RefreshChannelQuotaPayload)
	if !ok {
		return nil, errors.New("invalid refresh channel quota payload")
	}
	return payload, nil
}

func (svc *ChannelService) executeManualPerKeyRefresh(
	ctx context.Context,
	db *ent.Client,
	ch *ent.Channel,
	ownerCh *ent.Channel,
	fingerprint string,
	keyIDs []string,
	now time.Time,
) (*RefreshChannelQuotaPayload, error) {
	if isChannelSharedWallet(ownerCh) {
		rawKey := ""
		if keys := channelBalanceProbeCandidateKeys(ownerCh); len(keys) > 0 {
			rawKey = keys[0]
		}
		subjectKey := fmt.Sprintf("ch:%d:wallet", ownerCh.ID)
		res, executed, probeErr := svc.runCoalescedQuotaProbe(ctx, ownerCh, subjectKey, rawKey, objects.ChannelKeyHealthCheckTriggerManual, true)
		if probeErr != nil {
			return &RefreshChannelQuotaPayload{
				Status:        RefreshChannelQuotaStatusError,
				Message:       probeErr.Error(),
				QuotaOverview: svc.BuildChannelQuotaOverview(ctx, ch, ownerCh, nil, now),
			}, nil
		}
		if executed {
			if err := svc.persistQuotaProbeUpdates(ctx, ownerCh, fingerprint, &res, nil, now); err != nil {
				if errors.Is(err, errQuotaProbeStaleFence) {
					latest, _ := db.Channel.Get(ctx, ownerCh.ID)
					if latest == nil {
						latest = ownerCh
					}
					return &RefreshChannelQuotaPayload{
						Status:        RefreshChannelQuotaStatusError,
						Message:       err.Error(),
						QuotaOverview: svc.BuildChannelQuotaOverview(ctx, ch, latest, nil, now),
					}, nil
				}
				return nil, err
			}
		}
		refreshedOwner, _ := db.Channel.Get(ctx, ownerCh.ID)
		if refreshedOwner == nil {
			refreshedOwner = ownerCh
		}
		overview := svc.BuildChannelQuotaOverview(ctx, ch, refreshedOwner, nil, now)
		if !res.ObservationValid {
			st := RefreshChannelQuotaStatusError
			if res.ProbeStatus == "unsupported" || res.Unsupported {
				st = RefreshChannelQuotaStatusUnsupported
			}
			var retryAt *time.Time
			if refreshedOwner.Settings != nil && refreshedOwner.Settings.KeyHealthCheck != nil {
				retryAt = refreshedOwner.Settings.KeyHealthCheck.SharedNextProbeAt
			}
			return &RefreshChannelQuotaPayload{
				Status:        st,
				Message:       res.Reason,
				RetryAt:       retryAt,
				QuotaOverview: overview,
			}, nil
		}
		return &RefreshChannelQuotaPayload{
			Status:        RefreshChannelQuotaStatusUpdated,
			UpdatedKeys:   1,
			QuotaOverview: overview,
		}, nil
	}

	var targetKeys []string
	if len(keyIDs) > 0 {
		targetKeys = selectedChannelKeyHealthCheckTargets(ownerCh, keyIDs)
	} else {
		// Bounded manual refresh: pick oldest-due / stale keys first up to maxQuotaProbeKeysPerChannelPerTick.
		allKeys := channelBalanceProbeCandidateKeys(ownerCh)
		targetKeys = orderKeysOldestProbedFirst(ownerCh, allKeys)
	}
	if len(targetKeys) == 0 {
		return &RefreshChannelQuotaPayload{
			Status:        RefreshChannelQuotaStatusUnsupported,
			Message:       "channel has no eligible API keys to probe",
			QuotaOverview: svc.BuildChannelQuotaOverview(ctx, ch, ownerCh, nil, now),
		}, nil
	}

	deferredCount := 0
	if len(targetKeys) > maxQuotaProbeKeysPerChannelPerTick {
		deferredCount = len(targetKeys) - maxQuotaProbeKeysPerChannelPerTick
		targetKeys = targetKeys[:maxQuotaProbeKeysPerChannelPerTick]
	}

	updates := make([]quotaKeyProbeUpdate, 0, len(targetKeys))
	persistUpdates := make([]quotaKeyProbeUpdate, 0, len(targetKeys))
	var mu sync.Mutex
	var firstErrReason string
	var earliestRetry *time.Time
	eg, egCtx := errgroup.WithContext(ctx)
	eg.SetLimit(min(maxInFlightQuotaProbesPerOwner, len(targetKeys)))

	for _, rawKey := range targetKeys {
		k := rawKey
		keyID := objects.ChannelAPIKeyFingerprint(k)
		subjectKey := fmt.Sprintf("ch:%d:key:%s", ownerCh.ID, keyID)
		eg.Go(func() error {
			res, executed, probeErr := svc.runCoalescedQuotaProbe(egCtx, ownerCh, subjectKey, k, objects.ChannelKeyHealthCheckTriggerManual, true)
			if probeErr != nil {
				mu.Lock()
				if firstErrReason == "" {
					firstErrReason = probeErr.Error()
				}
				mu.Unlock()
				return nil
			}
			mu.Lock()
			upd := quotaKeyProbeUpdate{RawKey: k, KeyID: keyID, Result: res}
			updates = append(updates, upd)
			if executed {
				persistUpdates = append(persistUpdates, upd)
			}
			if !res.ObservationValid && firstErrReason == "" {
				firstErrReason = res.Reason
			}
			if res.RetryAfter != nil && (earliestRetry == nil || res.RetryAfter.Before(*earliestRetry)) {
				t := *res.RetryAfter
				earliestRetry = &t
			}
			mu.Unlock()
			return nil
		})
	}
	_ = eg.Wait()

	if len(persistUpdates) > 0 {
		if err := svc.persistQuotaProbeUpdates(ctx, ownerCh, fingerprint, nil, persistUpdates, now); err != nil {
			if errors.Is(err, errQuotaProbeStaleFence) {
				latest, _ := db.Channel.Get(ctx, ownerCh.ID)
				if latest == nil {
					latest = ownerCh
				}
				return &RefreshChannelQuotaPayload{
					Status:        RefreshChannelQuotaStatusError,
					Message:       err.Error(),
					QuotaOverview: svc.BuildChannelQuotaOverview(ctx, ch, latest, nil, now),
				}, nil
			}
			return nil, err
		}
	}

	updatedCount := 0
	unsupportedCount := 0
	for _, u := range updates {
		if u.Result.ObservationValid {
			updatedCount++
		} else if u.Result.ProbeStatus == "unsupported" || u.Result.Unsupported {
			unsupportedCount++
		}
	}

	refreshedOwner, _ := db.Channel.Get(ctx, ownerCh.ID)
	if refreshedOwner == nil {
		refreshedOwner = ownerCh
	}
	overview := svc.BuildChannelQuotaOverview(ctx, ch, refreshedOwner, nil, now)

	var status RefreshChannelQuotaStatus
	var msg string
	switch {
	case updatedCount > 0 && deferredCount > 0:
		status = RefreshChannelQuotaStatusDeferred
		msg = fmt.Sprintf("refreshed %d keys; %d keys deferred to subsequent ticks", updatedCount, deferredCount)
		nextRetry := now.Add(time.Minute)
		earliestRetry = &nextRetry
	case updatedCount > 0:
		status = RefreshChannelQuotaStatusUpdated
	case unsupportedCount == len(updates) && len(updates) > 0:
		status = RefreshChannelQuotaStatusUnsupported
		msg = firstErrReason
	default:
		status = RefreshChannelQuotaStatusError
		msg = firstErrReason
	}

	return &RefreshChannelQuotaPayload{
		Status:        status,
		Message:       msg,
		UpdatedKeys:   updatedCount,
		DeferredKeys:  deferredCount,
		RetryAt:       earliestRetry,
		QuotaOverview: overview,
	}, nil
}

func orderKeysOldestProbedFirst(ch *ent.Channel, keys []string) []string {
	metaByID := map[string]objects.ChannelKeyMetadata{}
	if ch != nil && ch.Settings != nil && ch.Settings.KeyHealthCheck != nil {
		metaByID = channelKeyMetadataByID(ch.Settings.KeyHealthCheck.KeyMetadata)
	}
	type item struct {
		key   string
		ts    time.Time
		index int
	}
	items := make([]item, len(keys))
	for i, k := range keys {
		var ts time.Time
		if meta, ok := metaByID[objects.ChannelAPIKeyFingerprint(k)]; ok {
			if meta.LastProbeAt != nil {
				ts = *meta.LastProbeAt
			} else if meta.BalanceSnapshot != nil {
				ts = meta.BalanceSnapshot.CheckedAt
			}
		}
		items[i] = item{key: k, ts: ts, index: i}
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].ts.Equal(items[j].ts) {
			return items[i].ts.Before(items[j].ts)
		}
		return items[i].index < items[j].index
	})
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.key
	}
	return out
}

func loTernaryInt(cond bool, a, b int) int {
	if cond {
		return a
	}
	return b
}

// suppress unused import warning if slices isn't referenced directly here
var _ = slices.Clone[[]string]
