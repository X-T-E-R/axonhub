package biz

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/outboundproxy"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/auth"
)

func setupCredentialSelectionChannel(
	t *testing.T,
	keys []string,
	settings *objects.ChannelSettings,
) (*ChannelService, *ent.Client, *Channel, auth.APIKeyProvider) {
	t.Helper()

	svc, client := setupTestChannelService(t)
	t.Cleanup(func() {
		svc.Stop()
		_ = client.Close()
	})

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	entCh, err := client.Channel.Create().
		SetType(channel.TypeOpenai).
		SetName(fmt.Sprintf("cred-sel-%d", time.Now().UnixNano())).
		SetBaseURL("https://api.openai.com/v1").
		SetCredentials(objects.ChannelCredentials{APIKeys: keys}).
		SetSupportedModels([]string{"gpt-4"}).
		SetDefaultTestModel("gpt-4").
		SetStatus(channel.StatusEnabled).
		SetSettings(settings).
		Save(ctx)
	require.NoError(t, err)

	built, err := svc.buildChannelWithOutbounds(entCh)
	require.NoError(t, err)

	return svc, client, built, getAPIKeyProvider(built)
}

func TestCredentialSelection_WeightedRoundRobin_1To3(t *testing.T) {
	keys := []string{"key-1", "key-2"}
	settings := &objects.ChannelSettings{
		KeySelection: &objects.ChannelKeySelection{
			Strategy: objects.ChannelKeySelectionStrategyRoundRobin,
		},
		CredentialPolicies: []objects.ChannelCredentialPolicy{
			{
				ID:     objects.ChannelAPIKeyFingerprint("key-1"),
				Weight: 1,
			},
			{
				ID:     objects.ChannelAPIKeyFingerprint("key-2"),
				Weight: 3,
			},
		},
	}

	_, _, _, provider := setupCredentialSelectionChannel(t, keys, settings)

	counts := map[string]int{}
	sequence := make([]string, 0, 12)
	for range 12 {
		ctx := contexts.EnsureContainer(context.Background())
		selected := provider.Get(ctx)
		require.NotEmpty(t, selected)
		counts[selected]++
		sequence = append(sequence, selected)
		contexts.FinishChannelCredentialAttempt(ctx, http.StatusOK, false, false)
	}

	require.Equal(t, 3, counts["key-1"])
	require.Equal(t, 9, counts["key-2"])
	require.Equal(t, []string{
		"key-2", "key-1", "key-2", "key-2",
		"key-2", "key-1", "key-2", "key-2",
		"key-2", "key-1", "key-2", "key-2",
	}, sequence)
}

func TestCredentialSelection_PriorityAndExhaustionEscape(t *testing.T) {
	now := time.Now()
	future := now.Add(time.Hour)
	past := now.Add(-time.Minute)

	t.Run("higher priority wins and exhausted key escapes to lower priority", func(t *testing.T) {
		keys := []string{"key-high", "key-low"}
		settings := &objects.ChannelSettings{
			KeySelection: &objects.ChannelKeySelection{
				Strategy: objects.ChannelKeySelectionStrategyRoundRobin,
			},
			CredentialPolicies: []objects.ChannelCredentialPolicy{
				{ID: objects.ChannelAPIKeyFingerprint("key-high"), Priority: 10},
				{ID: objects.ChannelAPIKeyFingerprint("key-low"), Priority: 5},
			},
		}

		_, _, ch, provider := setupCredentialSelectionChannel(t, keys, settings)

		for range 3 {
			ctx := contexts.EnsureContainer(context.Background())
			require.Equal(t, "key-high", provider.Get(ctx))
			contexts.FinishChannelCredentialAttempt(ctx, http.StatusOK, false, false)
		}

		ch.Settings.KeyHealthCheck = &objects.ChannelKeyHealthCheck{
			KeyMetadata: []objects.ChannelKeyMetadata{
				{
					ID: objects.ChannelAPIKeyFingerprint("key-high"),
					BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
						CheckedAt: now,
						Success:   true,
						Limit:     lo.ToPtr(100.0),
						Remaining: lo.ToPtr(0.0),
						ResetAt:   &future,
					},
				},
			},
		}

		for range 3 {
			ctx := contexts.EnsureContainer(context.Background())
			require.Equal(t, "key-low", provider.Get(ctx))
			contexts.FinishChannelCredentialAttempt(ctx, http.StatusOK, false, false)
		}

		// Once reset time has elapsed, the high-priority key becomes eligible again.
		ch.Settings.KeyHealthCheck.KeyMetadata[0].BalanceSnapshot.ResetAt = &past
		ctx := contexts.EnsureContainer(context.Background())
		require.Equal(t, "key-high", provider.Get(ctx))
		contexts.FinishChannelCredentialAttempt(ctx, http.StatusOK, false, false)
	})

	t.Run("all keys exhausted reports ineligible for HasEligibleCredential but soft-falls back on Get", func(t *testing.T) {
		keys := []string{"key-1", "key-2"}
		settings := &objects.ChannelSettings{
			KeySelection: &objects.ChannelKeySelection{
				Strategy: objects.ChannelKeySelectionStrategyRoundRobin,
			},
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				KeyMetadata: []objects.ChannelKeyMetadata{
					{
						ID: objects.ChannelAPIKeyFingerprint("key-1"),
						BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
							CheckedAt: now,
							Success:   true,
							Available: lo.ToPtr(false),
						},
					},
					{
						ID: objects.ChannelAPIKeyFingerprint("key-2"),
						BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
							CheckedAt: now,
							Success:   true,
							Available: lo.ToPtr(false),
						},
					},
				},
			},
		}

		_, _, ch, provider := setupCredentialSelectionChannel(t, keys, settings)
		ctx := contexts.EnsureContainer(context.Background())

		require.False(t, ch.HasEligibleCredential(ctx))
		require.Contains(t, keys, provider.Get(ctx))
	})
}

func TestCredentialSelection_AffinityEscape(t *testing.T) {
	t.Run("trace_sticky escapes when bound key reaches maxConcurrent or is exhausted", func(t *testing.T) {
		keys := []string{"key-1", "key-2"}
		settings := &objects.ChannelSettings{
			KeySelection: &objects.ChannelKeySelection{
				Strategy: objects.ChannelKeySelectionStrategyTraceSticky,
			},
			CredentialPolicies: []objects.ChannelCredentialPolicy{
				{ID: objects.ChannelAPIKeyFingerprint("key-1"), MaxConcurrent: lo.ToPtr(1)},
				{ID: objects.ChannelAPIKeyFingerprint("key-2"), MaxConcurrent: lo.ToPtr(1)},
			},
		}

		_, _, ch, provider := setupCredentialSelectionChannel(t, keys, settings)
		trace := &ent.Trace{TraceID: "trace-affinity-escape"}

		ctx1 := contexts.WithTrace(contexts.EnsureContainer(context.Background()), trace)
		first := provider.Get(ctx1)
		require.Contains(t, keys, first)
		other := lo.Ternary(first == "key-1", "key-2", "key-1")

		// While first is in-flight at maxConcurrent=1, the same trace escapes to the other key.
		ctx2 := contexts.WithTrace(contexts.EnsureContainer(context.Background()), trace)
		escaped := provider.Get(ctx2)
		require.Equal(t, other, escaped)
		contexts.FinishChannelCredentialAttempt(ctx1, http.StatusOK, false, false)
		contexts.FinishChannelCredentialAttempt(ctx2, http.StatusOK, false, false)

		// Exhausting the current sticky key forces trace_sticky to escape to the remaining healthy key.
		ch.Settings.KeyHealthCheck = &objects.ChannelKeyHealthCheck{
			KeyMetadata: []objects.ChannelKeyMetadata{
				{
					ID: objects.ChannelAPIKeyFingerprint(escaped),
					BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
						CheckedAt: time.Now(),
						Success:   true,
						Available: lo.ToPtr(false),
					},
				},
			},
		}

		ctx3 := contexts.WithTrace(contexts.EnsureContainer(context.Background()), trace)
		require.Equal(t, first, provider.Get(ctx3))
		contexts.FinishChannelCredentialAttempt(ctx3, http.StatusOK, false, false)
	})

	t.Run("cache_affinity escapes on cooldown and higher priority tier", func(t *testing.T) {
		keys := []string{"key-1", "key-2"}
		settings := &objects.ChannelSettings{
			KeySelection: &objects.ChannelKeySelection{
				Strategy: objects.ChannelKeySelectionStrategyCacheAffinity,
			},
			CredentialPolicies: []objects.ChannelCredentialPolicy{
				{ID: objects.ChannelAPIKeyFingerprint("key-1"), Priority: 5},
				{ID: objects.ChannelAPIKeyFingerprint("key-2"), Priority: 5},
			},
		}

		_, _, ch, provider := setupCredentialSelectionChannel(t, keys, settings)
		affinityID := "cache:exact:prompt-escape"

		ctx1 := contexts.WithChannelKeyAffinityID(contexts.EnsureContainer(context.Background()), affinityID)
		first := provider.Get(ctx1)
		require.Contains(t, keys, first)
		other := lo.Ternary(first == "key-1", "key-2", "key-1")

		// 429 cooldown pushes cache_affinity off the cooled-down key.
		contexts.FinishChannelCredentialAttempt(ctx1, http.StatusTooManyRequests, true, false)

		ctx2 := contexts.WithChannelKeyAffinityID(contexts.EnsureContainer(context.Background()), affinityID)
		require.Equal(t, other, provider.Get(ctx2))
		contexts.FinishChannelCredentialAttempt(ctx2, http.StatusOK, false, false)

		// Once the first key's cooldown expires at equal priority, cache_affinity remains sticky on other.
		rt := ch.runtime()
		rt.mu.Lock()
		rt.state(credentialRuntimeKey{channelID: ch.ID, keyID: objects.ChannelAPIKeyFingerprint(first)}, time.Now()).cooldownUntil = time.Now().Add(-time.Second)
		rt.mu.Unlock()

		ctxStillSticky := contexts.WithChannelKeyAffinityID(contexts.EnsureContainer(context.Background()), affinityID)
		require.Equal(t, other, provider.Get(ctxStillSticky))
		contexts.FinishChannelCredentialAttempt(ctxStillSticky, http.StatusOK, false, false)

		// Raising priority of the first key above the currently sticky key forces a priority escape.
		for i := range ch.Settings.CredentialPolicies {
			if ch.Settings.CredentialPolicies[i].ID == objects.ChannelAPIKeyFingerprint(first) {
				ch.Settings.CredentialPolicies[i].Priority = 20
			}
		}

		ctx3 := contexts.WithChannelKeyAffinityID(contexts.EnsureContainer(context.Background()), affinityID)
		require.Equal(t, first, provider.Get(ctx3))
		contexts.FinishChannelCredentialAttempt(ctx3, http.StatusOK, false, false)
	})
}

func TestCredentialSelection_MaxConcurrent_EnsureContainerAndFinish(t *testing.T) {
	keys := []string{"key-1", "key-2"}
	settings := &objects.ChannelSettings{
		KeySelection: &objects.ChannelKeySelection{
			Strategy: objects.ChannelKeySelectionStrategyRoundRobin,
		},
		CredentialPolicies: []objects.ChannelCredentialPolicy{
			{
				ID:            objects.ChannelAPIKeyFingerprint("key-1"),
				Priority:      10,
				MaxConcurrent: lo.ToPtr(1),
			},
			{
				ID:            objects.ChannelAPIKeyFingerprint("key-2"),
				Priority:      5,
				MaxConcurrent: lo.ToPtr(1),
			},
		},
	}

	_, _, ch, provider := setupCredentialSelectionChannel(t, keys, settings)

	// Discovery calls without EnsureContainer must not acquire an inFlight slot.
	require.Equal(t, "key-1", provider.Get(context.Background()))

	ctx1 := contexts.EnsureContainer(context.Background())
	require.Equal(t, "key-1", provider.Get(ctx1))

	ctx2 := contexts.EnsureContainer(context.Background())
	require.Equal(t, "key-2", provider.Get(ctx2))

	// Both keys are at maxConcurrent=1: hard limit leaves no eligible keys even on soft fallback.
	ctx3 := contexts.EnsureContainer(context.Background())
	require.False(t, ch.HasEligibleCredential(ctx3))
	require.Empty(t, provider.Get(ctx3))
	recorded, ok := contexts.GetChannelAPIKey(ctx3)
	require.True(t, ok)
	require.Empty(t, recorded)

	// Finishing ctx1 releases key-1's concurrency slot immediately.
	contexts.FinishChannelCredentialAttempt(ctx1, http.StatusOK, false, false)

	ctx4 := contexts.EnsureContainer(context.Background())
	require.True(t, ch.HasEligibleCredential(ctx4))
	require.Equal(t, "key-1", provider.Get(ctx4))

	contexts.FinishChannelCredentialAttempt(ctx2, http.StatusOK, false, false)
	contexts.FinishChannelCredentialAttempt(ctx4, http.StatusOK, false, false)
}

func TestCredentialSelection_RequestIntervalAndCooldown429(t *testing.T) {
	t.Run("request interval hard-gates rapid reuse and recovers after interval", func(t *testing.T) {
		keys := []string{"key-fast-limit", "key-backup"}
		settings := &objects.ChannelSettings{
			KeySelection: &objects.ChannelKeySelection{
				Strategy: objects.ChannelKeySelectionStrategyRoundRobin,
			},
			CredentialPolicies: []objects.ChannelCredentialPolicy{
				{
					ID:                objects.ChannelAPIKeyFingerprint("key-fast-limit"),
					Priority:          10,
					RequestIntervalMs: lo.ToPtr(120),
				},
				{
					ID:                objects.ChannelAPIKeyFingerprint("key-backup"),
					Priority:          5,
					RequestIntervalMs: lo.ToPtr(120),
				},
			},
		}

		_, _, _, provider := setupCredentialSelectionChannel(t, keys, settings)

		ctx1 := contexts.EnsureContainer(context.Background())
		require.Equal(t, "key-fast-limit", provider.Get(ctx1))
		contexts.FinishChannelCredentialAttempt(ctx1, http.StatusOK, false, false)

		// Immediate second request falls within key-fast-limit's interval, so it uses key-backup.
		ctx2 := contexts.EnsureContainer(context.Background())
		require.Equal(t, "key-backup", provider.Get(ctx2))
		contexts.FinishChannelCredentialAttempt(ctx2, http.StatusOK, false, false)

		// Immediate third request hits hard interval limits on both keys.
		ctx3 := contexts.EnsureContainer(context.Background())
		require.Empty(t, provider.Get(ctx3))

		time.Sleep(150 * time.Millisecond)

		ctx4 := contexts.EnsureContainer(context.Background())
		require.Equal(t, "key-fast-limit", provider.Get(ctx4))
		contexts.FinishChannelCredentialAttempt(ctx4, http.StatusOK, false, false)
	})

	t.Run("429 puts key into cooldown while canceled 429 does not", func(t *testing.T) {
		keys := []string{"key-primary", "key-secondary"}
		settings := &objects.ChannelSettings{
			KeySelection: &objects.ChannelKeySelection{
				Strategy: objects.ChannelKeySelectionStrategyRoundRobin,
			},
			CredentialPolicies: []objects.ChannelCredentialPolicy{
				{ID: objects.ChannelAPIKeyFingerprint("key-primary"), Priority: 10},
				{ID: objects.ChannelAPIKeyFingerprint("key-secondary"), Priority: 5},
			},
		}

		_, _, ch, provider := setupCredentialSelectionChannel(t, keys, settings)

		// Canceled attempt must not place key-primary in cooldown.
		ctxCanceled := contexts.EnsureContainer(context.Background())
		require.Equal(t, "key-primary", provider.Get(ctxCanceled))
		contexts.FinishChannelCredentialAttempt(ctxCanceled, http.StatusTooManyRequests, true, true)

		ctxAfterCancel := contexts.EnsureContainer(context.Background())
		require.Equal(t, "key-primary", provider.Get(ctxAfterCancel))

		// Uncanceled 429 places key-primary in cooldown so key-secondary takes over.
		contexts.FinishChannelCredentialAttempt(ctxAfterCancel, http.StatusTooManyRequests, true, false)

		ctxNext := contexts.EnsureContainer(context.Background())
		require.Equal(t, "key-secondary", provider.Get(ctxNext))
		contexts.FinishChannelCredentialAttempt(ctxNext, http.StatusOK, false, false)

		// Put key-secondary on 429 cooldown as well: HasEligibleCredential becomes false.
		ctxSecond429 := contexts.EnsureContainer(context.Background())
		require.Equal(t, "key-secondary", provider.Get(ctxSecond429))
		contexts.FinishChannelCredentialAttempt(ctxSecond429, http.StatusTooManyRequests, true, false)

		require.False(t, ch.HasEligibleCredential(context.Background()))
	})
}

func TestCredentialSelection_QuotaResetPriority(t *testing.T) {
	now := time.Now()
	earliest := now.Add(30 * time.Minute)
	later := now.Add(2 * time.Hour)

	keys := []string{"key-later", "key-earliest", "key-no-reset"}
	settings := &objects.ChannelSettings{
		KeySelection: &objects.ChannelKeySelection{
			Strategy: objects.ChannelKeySelectionStrategyQuotaResetPriority,
		},
		KeyHealthCheck: &objects.ChannelKeyHealthCheck{
			KeyMetadata: []objects.ChannelKeyMetadata{
				{
					ID: objects.ChannelAPIKeyFingerprint("key-later"),
					BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
						CheckedAt: now,
						Success:   true,
						Remaining: lo.ToPtr(50.0),
						Limit:     lo.ToPtr(100.0),
						ResetAt:   &later,
					},
				},
				{
					ID: objects.ChannelAPIKeyFingerprint("key-earliest"),
					BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
						CheckedAt: now,
						Success:   true,
						Remaining: lo.ToPtr(20.0),
						Limit:     lo.ToPtr(100.0),
						ResetAt:   &earliest,
					},
				},
			},
		},
	}

	_, _, ch, provider := setupCredentialSelectionChannel(t, keys, settings)

	ctx1 := contexts.EnsureContainer(context.Background())
	require.Equal(t, "key-earliest", provider.Get(ctx1))
	contexts.FinishChannelCredentialAttempt(ctx1, http.StatusOK, false, false)

	// Once key-earliest is exhausted, key-later (non-nil ResetAt) still beats key-no-reset (nil ResetAt).
	ch.Settings.KeyHealthCheck.KeyMetadata[1].BalanceSnapshot.Remaining = lo.ToPtr(0.0)
	ctx2 := contexts.EnsureContainer(context.Background())
	require.Equal(t, "key-later", provider.Get(ctx2))
	contexts.FinishChannelCredentialAttempt(ctx2, http.StatusOK, false, false)

	// With identical ResetAt, lower utilization breaks the tie while the busier key stays in-flight.
	ch.Settings.KeyHealthCheck.KeyMetadata[1].BalanceSnapshot.Remaining = lo.ToPtr(20.0)
	ch.Settings.KeyHealthCheck.KeyMetadata[1].BalanceSnapshot.ResetAt = &later

	ctxHold := contexts.EnsureContainer(context.Background())
	first := provider.Get(ctxHold)
	require.Contains(t, []string{"key-later", "key-earliest"}, first)

	ctxTie := contexts.EnsureContainer(context.Background())
	second := provider.Get(ctxTie)
	require.Contains(t, []string{"key-later", "key-earliest"}, second)
	require.NotEqual(t, first, second)

	contexts.FinishChannelCredentialAttempt(ctxHold, http.StatusOK, false, false)
	contexts.FinishChannelCredentialAttempt(ctxTie, http.StatusOK, false, false)
}

func TestCredentialSelection_Smart(t *testing.T) {
	t.Run("prefers higher remaining quota via usageRatio and manualQuotaLimit, shifting on load and failures", func(t *testing.T) {
		now := time.Now()
		keys := []string{"key-high-quota", "key-low-quota"}
		settings := &objects.ChannelSettings{
			KeySelection: &objects.ChannelKeySelection{
				Strategy: objects.ChannelKeySelectionStrategySmart,
			},
			CredentialPolicies: []objects.ChannelCredentialPolicy{
				{
					ID:               objects.ChannelAPIKeyFingerprint("key-high-quota"),
					ManualQuotaLimit: lo.ToPtr(100.0),
				},
				{
					ID: objects.ChannelAPIKeyFingerprint("key-low-quota"),
				},
			},
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				KeyMetadata: []objects.ChannelKeyMetadata{
					{
						// UsageRatio omitted: falls back to Remaining / ManualQuotaLimit = 0.9.
						ID: objects.ChannelAPIKeyFingerprint("key-high-quota"),
						BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
							CheckedAt: now,
							Success:   true,
							Remaining: lo.ToPtr(90.0),
						},
					},
					{
						ID: objects.ChannelAPIKeyFingerprint("key-low-quota"),
						BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
							CheckedAt:  now,
							Success:    true,
							UsageRatio: lo.ToPtr(0.5),
						},
					},
				},
			},
		}

		_, _, _, provider := setupCredentialSelectionChannel(t, keys, settings)

		for range 4 {
			ctx := contexts.EnsureContainer(context.Background())
			require.Equal(t, "key-high-quota", provider.Get(ctx))
			contexts.FinishChannelCredentialAttempt(ctx, http.StatusOK, false, false)
		}

		// Holding 1 concurrent request on key-high-quota drops its load score from 1.0 to 0.5 (-0.5),
		// outweighing the 0.32 quota advantage so the next concurrent request routes to key-low-quota.
		hold1 := contexts.EnsureContainer(context.Background())
		require.Equal(t, "key-high-quota", provider.Get(hold1))

		ctxShifted := contexts.EnsureContainer(context.Background())
		require.Equal(t, "key-low-quota", provider.Get(ctxShifted))

		// Fail key-high-quota while key-low-quota is held busy so repeated high-key attempts accumulate failureRate.
		contexts.FinishChannelCredentialAttempt(hold1, http.StatusInternalServerError, true, false)
		fail2 := contexts.EnsureContainer(context.Background())
		require.Equal(t, "key-high-quota", provider.Get(fail2))
		contexts.FinishChannelCredentialAttempt(fail2, http.StatusInternalServerError, true, false)
		contexts.FinishChannelCredentialAttempt(ctxShifted, http.StatusOK, false, false)

		// Equalize quota so health degradation on key-high-quota decides directly.
		settings.KeyHealthCheck.KeyMetadata[1].BalanceSnapshot.UsageRatio = lo.ToPtr(0.1)
		ctxHealth := contexts.EnsureContainer(context.Background())
		require.Equal(t, "key-low-quota", provider.Get(ctxHealth))
		contexts.FinishChannelCredentialAttempt(ctxHealth, http.StatusOK, false, false)
	})

	t.Run("rotates inside 0.05 tolerance band", func(t *testing.T) {
		now := time.Now()
		keys := []string{"key-a", "key-b"}
		settings := &objects.ChannelSettings{
			KeySelection: &objects.ChannelKeySelection{
				Strategy: objects.ChannelKeySelectionStrategySmart,
			},
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				KeyMetadata: []objects.ChannelKeyMetadata{
					{
						ID: objects.ChannelAPIKeyFingerprint("key-a"),
						BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
							CheckedAt:  now,
							Success:    true,
							UsageRatio: lo.ToPtr(0.20),
						},
					},
					{
						ID: objects.ChannelAPIKeyFingerprint("key-b"),
						BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
							CheckedAt:  now,
							Success:    true,
							UsageRatio: lo.ToPtr(0.24),
						},
					},
				},
			},
		}

		_, _, _, provider := setupCredentialSelectionChannel(t, keys, settings)

		seq := make([]string, 0, 4)
		for range 4 {
			ctx := contexts.EnsureContainer(context.Background())
			seq = append(seq, provider.Get(ctx))
			contexts.FinishChannelCredentialAttempt(ctx, http.StatusOK, false, false)
		}

		require.Equal(t, []string{"key-a", "key-b", "key-a", "key-b"}, seq)
	})
}

func TestCredentialSelection_ExcludedKeysHard(t *testing.T) {
	keys := []string{"key-high", "key-low"}
	settings := &objects.ChannelSettings{
		KeySelection: &objects.ChannelKeySelection{
			Strategy: objects.ChannelKeySelectionStrategyRoundRobin,
		},
		CredentialPolicies: []objects.ChannelCredentialPolicy{
			{ID: objects.ChannelAPIKeyFingerprint("key-high"), Priority: 10},
			{ID: objects.ChannelAPIKeyFingerprint("key-low"), Priority: 5},
		},
	}

	_, _, ch, provider := setupCredentialSelectionChannel(t, keys, settings)

	// Excluding a different channel ID does not exclude key-high on this channel.
	otherChanCtx := contexts.EnsureContainer(context.Background())
	contexts.ExcludeChannelCredential(otherChanCtx, ch.ID+999, "key-high")
	require.Equal(t, "key-high", provider.Get(otherChanCtx))
	contexts.FinishChannelCredentialAttempt(otherChanCtx, http.StatusOK, false, false)

	// Excluding key-high on this channel skips it even though it has higher priority.
	excludedHighCtx := contexts.EnsureContainer(context.Background())
	contexts.ExcludeChannelCredential(excludedHighCtx, ch.ID, "key-high")
	require.Equal(t, "key-low", provider.Get(excludedHighCtx))

	// Excluding all keys is hard (no soft fallback).
	contexts.ExcludeChannelCredential(excludedHighCtx, ch.ID, "key-low")
	require.False(t, ch.HasEligibleCredential(excludedHighCtx))
	require.Empty(t, provider.Get(excludedHighCtx))
}

func TestCredentialSelection_PinnedAndPoolFailClosed_WithRealEntProxies(t *testing.T) {
	createProxy := func(
		t *testing.T,
		client *ent.Client,
		name, proxyURL, poolName string,
		status outboundproxy.Status,
		mut func(*ent.OutboundProxyCreate),
	) *ent.OutboundProxy {
		t.Helper()
		ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
		b := client.OutboundProxy.Create().
			SetName(name).
			SetKind(outboundproxy.KindStatic).
			SetProxyURL(proxyURL).
			SetPoolName(poolName).
			SetStatus(status)
		if mut != nil {
			mut(b)
		}
		row, err := b.Save(ctx)
		require.NoError(t, err)
		return row
	}

	t.Run("pinned honors enabled and expiry state but ignores stale manual failure", func(t *testing.T) {
		svc, client := setupTestChannelService(t)
		defer func() {
			svc.Stop()
			_ = client.Close()
		}()

		enabledProxy := createProxy(t, client, "pinned-ok", "http://127.0.0.1:19101", "", outboundproxy.StatusEnabled, nil)
		disabledProxy := createProxy(t, client, "pinned-disabled", "http://127.0.0.1:19102", "", outboundproxy.StatusDisabled, nil)
		expiredProxy := createProxy(t, client, "pinned-expired", "http://127.0.0.1:19103", "", outboundproxy.StatusEnabled, func(b *ent.OutboundProxyCreate) {
			b.SetExpiresAt(time.Now().Add(-time.Minute))
		})
		failedProbeProxy := createProxy(t, client, "pinned-probe-fail", "http://127.0.0.1:19104", "", outboundproxy.StatusEnabled, func(b *ent.OutboundProxyCreate) {
			b.SetLastTestSuccess(false)
		})

		ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
		keys := []string{"key-disabled-proxy", "key-expired-proxy", "key-failed-probe", "key-ok-proxy"}
		entCh, err := client.Channel.Create().
			SetType(channel.TypeOpenai).
			SetName("pinned-fail-closed-channel").
			SetBaseURL("https://api.openai.com/v1").
			SetCredentials(objects.ChannelCredentials{APIKeys: keys}).
			SetSupportedModels([]string{"gpt-4"}).
			SetDefaultTestModel("gpt-4").
			SetStatus(channel.StatusEnabled).
			SetSettings(&objects.ChannelSettings{
				KeySelection: &objects.ChannelKeySelection{Strategy: objects.ChannelKeySelectionStrategyRoundRobin},
				CredentialPolicies: []objects.ChannelCredentialPolicy{
					{
						ID:               objects.ChannelAPIKeyFingerprint("key-disabled-proxy"),
						Priority:         30,
						EgressMode:       objects.CredentialEgressModePinnedProxy,
						OutboundProxyID:  lo.ToPtr(disabledProxy.ID),
						EgressFailPolicy: objects.CredentialEgressFailPolicyFailClosed,
					},
					{
						ID:               objects.ChannelAPIKeyFingerprint("key-expired-proxy"),
						Priority:         25,
						EgressMode:       objects.CredentialEgressModePinnedProxy,
						OutboundProxyID:  lo.ToPtr(expiredProxy.ID),
						EgressFailPolicy: objects.CredentialEgressFailPolicyFailClosed,
					},
					{
						ID:               objects.ChannelAPIKeyFingerprint("key-failed-probe"),
						Priority:         20,
						EgressMode:       objects.CredentialEgressModePinnedProxy,
						OutboundProxyID:  lo.ToPtr(failedProbeProxy.ID),
						EgressFailPolicy: objects.CredentialEgressFailPolicyFailClosed,
					},
					{
						ID:               objects.ChannelAPIKeyFingerprint("key-ok-proxy"),
						Priority:         5,
						EgressMode:       objects.CredentialEgressModePinnedProxy,
						OutboundProxyID:  lo.ToPtr(enabledProxy.ID),
						EgressFailPolicy: objects.CredentialEgressFailPolicyFailClosed,
					},
				},
			}).
			Save(ctx)
		require.NoError(t, err)

		built, err := svc.buildChannelWithOutbounds(entCh)
		require.NoError(t, err)
		provider := getAPIKeyProvider(built)

		reqCtx := contexts.EnsureContainer(context.Background())
		require.Equal(t, "key-failed-probe", provider.Get(reqCtx))

		egressClient, ok := contexts.GetChannelCredentialEgress(reqCtx)
		require.True(t, ok)
		require.NotNil(t, egressClient)
		targetReq, err := http.NewRequest(http.MethodGet, "https://api.openai.com/v1/models", nil)
		require.NoError(t, err)
		proxyURL, err := egressClient.ProxyFunc()(targetReq)
		require.NoError(t, err)
		require.NotNil(t, proxyURL)
		require.Equal(t, failedProbeProxy.ProxyURL, proxyURL.String())

		// Dial/transport failure (status=0, failed=true, canceled=false) temporarily blocks the proxy.
		contexts.FinishChannelCredentialAttempt(reqCtx, 0, true, false)

		blockedCtx := contexts.EnsureContainer(context.Background())
		require.True(t, built.HasEligibleCredential(blockedCtx))
		require.Equal(t, "key-ok-proxy", provider.Get(blockedCtx))
		svc.blockProxyTemporarily(enabledProxy.ID)
		require.Empty(t, provider.Get(contexts.EnsureContainer(context.Background())))
	})

	t.Run("pinned disabled proxy with backup_proxy policy routes to healthy backup", func(t *testing.T) {
		svc, client := setupTestChannelService(t)
		defer func() {
			svc.Stop()
			_ = client.Close()
		}()

		backup := createProxy(t, client, "backup-ok", "http://127.0.0.1:19105", "", outboundproxy.StatusEnabled, nil)
		primary := createProxy(t, client, "primary-down", "http://127.0.0.1:19106", "", outboundproxy.StatusDisabled, func(b *ent.OutboundProxyCreate) {
			b.SetBackupProxyID(backup.ID)
		})

		ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
		entCh, err := client.Channel.Create().
			SetType(channel.TypeOpenai).
			SetName("pinned-backup-channel").
			SetBaseURL("https://api.openai.com/v1").
			SetCredentials(objects.ChannelCredentials{APIKeys: []string{"key-with-backup"}}).
			SetSupportedModels([]string{"gpt-4"}).
			SetDefaultTestModel("gpt-4").
			SetStatus(channel.StatusEnabled).
			SetSettings(&objects.ChannelSettings{
				CredentialPolicies: []objects.ChannelCredentialPolicy{
					{
						ID:               objects.ChannelAPIKeyFingerprint("key-with-backup"),
						EgressMode:       objects.CredentialEgressModePinnedProxy,
						OutboundProxyID:  lo.ToPtr(primary.ID),
						EgressFailPolicy: objects.CredentialEgressFailPolicyBackupProxy,
					},
				},
			}).
			Save(ctx)
		require.NoError(t, err)

		built, err := svc.buildChannelWithOutbounds(entCh)
		require.NoError(t, err)
		provider := getAPIKeyProvider(built)

		reqCtx := contexts.EnsureContainer(context.Background())
		require.Equal(t, "key-with-backup", provider.Get(reqCtx))
		egressClient, ok := contexts.GetChannelCredentialEgress(reqCtx)
		require.True(t, ok)
		targetReq, err := http.NewRequest(http.MethodGet, "https://api.openai.com/v1/models", nil)
		require.NoError(t, err)
		proxyURL, err := egressClient.ProxyFunc()(targetReq)
		require.NoError(t, err)
		require.NotNil(t, proxyURL)
		require.Equal(t, backup.ProxyURL, proxyURL.String())
		contexts.FinishChannelCredentialAttempt(reqCtx, http.StatusOK, false, false)
	})

	t.Run("proxy_pool skips disabled and expired members", func(t *testing.T) {
		svc, client := setupTestChannelService(t)
		defer func() {
			svc.Stop()
			_ = client.Close()
		}()

		createProxy(t, client, "pool-down-1", "http://127.0.0.1:19110", "pool-dead", outboundproxy.StatusDisabled, nil)
		createProxy(t, client, "pool-down-2", "http://127.0.0.1:19111", "pool-dead", outboundproxy.StatusEnabled, func(b *ent.OutboundProxyCreate) {
			b.SetExpiresAt(time.Now().Add(-time.Hour))
		})
		createProxy(t, client, "mixed-down", "http://127.0.0.1:19112", "pool-mixed", outboundproxy.StatusDisabled, func(b *ent.OutboundProxyCreate) {
			b.SetLastExitIP("198.51.100.10")
		})
		mixedHealthySameIP := createProxy(t, client, "mixed-same-ip", "http://127.0.0.1:19113", "pool-mixed", outboundproxy.StatusEnabled, func(b *ent.OutboundProxyCreate) {
			b.SetLastExitIP("198.51.100.10")
		})
		mixedOther := createProxy(t, client, "mixed-other-ip", "http://127.0.0.1:19114", "pool-mixed", outboundproxy.StatusEnabled, func(b *ent.OutboundProxyCreate) {
			b.SetLastExitIP("203.0.113.20")
		})

		ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

		// 1. All proxies in pool-dead are unhealthy: fail_closed blocks key-pool-dead and falls back to key-direct.
		deadPoolCh, err := client.Channel.Create().
			SetType(channel.TypeOpenai).
			SetName("pool-dead-channel").
			SetBaseURL("https://api.openai.com/v1").
			SetCredentials(objects.ChannelCredentials{APIKeys: []string{"key-pool-dead", "key-direct"}}).
			SetSupportedModels([]string{"gpt-4"}).
			SetDefaultTestModel("gpt-4").
			SetStatus(channel.StatusEnabled).
			SetSettings(&objects.ChannelSettings{
				KeySelection: &objects.ChannelKeySelection{Strategy: objects.ChannelKeySelectionStrategyRoundRobin},
				CredentialPolicies: []objects.ChannelCredentialPolicy{
					{
						ID:               objects.ChannelAPIKeyFingerprint("key-pool-dead"),
						Priority:         20,
						EgressMode:       objects.CredentialEgressModeProxyPool,
						ProxyPoolName:    "pool-dead",
						EgressFailPolicy: objects.CredentialEgressFailPolicyFailClosed,
					},
					{
						ID:         objects.ChannelAPIKeyFingerprint("key-direct"),
						Priority:   5,
						EgressMode: objects.CredentialEgressModeDirect,
					},
				},
			}).
			Save(ctx)
		require.NoError(t, err)

		builtDead, err := svc.buildChannelWithOutbounds(deadPoolCh)
		require.NoError(t, err)
		require.Equal(t, "key-direct", getAPIKeyProvider(builtDead).Get(contexts.EnsureContainer(context.Background())))

		// A pool lease always selects a currently enabled, unexpired member.
		for i := range 32 {
			policy := objects.ChannelCredentialPolicy{
				ID: strconv.Itoa(i), EgressMode: objects.CredentialEgressModeProxyPool,
				ProxyPoolName: "pool-mixed", EgressFailPolicy: objects.CredentialEgressFailPolicyFailClosed,
			}
			row, err := svc.managedCredentialProxy(ctx, 77, policy)
			require.NoError(t, err)
			require.Contains(t, []int{mixedHealthySameIP.ID, mixedOther.ID}, row.ID)
		}
	})

	t.Run("channel without credentialEgress blocks pinned and pool credentials instead of sending direct", func(t *testing.T) {
		ch := &Channel{
			Channel: &ent.Channel{
				ID: 77,
				Credentials: objects.ChannelCredentials{
					APIKeys: []string{"key-pinned", "key-pool"},
				},
				Settings: &objects.ChannelSettings{
					KeySelection: &objects.ChannelKeySelection{Strategy: objects.ChannelKeySelectionStrategyRoundRobin},
					CredentialPolicies: []objects.ChannelCredentialPolicy{
						{
							ID:              objects.ChannelAPIKeyFingerprint("key-pinned"),
							EgressMode:      objects.CredentialEgressModePinnedProxy,
							OutboundProxyID: lo.ToPtr(1),
						},
						{
							ID:            objects.ChannelAPIKeyFingerprint("key-pool"),
							EgressMode:    objects.CredentialEgressModeProxyPool,
							ProxyPoolName: "any-pool",
						},
					},
				},
			},
			cachedEnabledAPIKeys: []string{"key-pinned", "key-pool"},
		}

		provider := getAPIKeyProvider(ch)
		require.False(t, ch.HasEligibleCredential(context.Background()))
		require.Empty(t, provider.Get(contexts.EnsureContainer(context.Background())))
	})
}
