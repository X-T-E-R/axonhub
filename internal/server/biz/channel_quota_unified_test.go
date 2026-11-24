package biz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/providerquotastatus"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/llm/httpclient"
)

func newQuotaTestService(t *testing.T, rt http.RoundTripper) (*ent.Client, *ChannelService, *SystemService, context.Context) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })

	sysSvc := NewSystemService(SystemServiceParams{
		CacheConfig: xcache.Config{Mode: xcache.ModeMemory},
		Ent:         client,
	})
	hc := httpclient.NewHttpClient()
	if rt != nil {
		hc = httpclient.NewHttpClientWithClient(&http.Client{Transport: rt})
	}
	chSvc := NewChannelService(ChannelServiceParams{
		Ent:           client,
		CacheConfig:   xcache.Config{Mode: xcache.ModeMemory},
		SystemService: sysSvc,
		HttpClient:    hc,
	})
	t.Cleanup(func() { chSvc.Stop() })

	ctx := ent.NewContext(context.Background(), client)
	ctx = authz.WithTestBypass(ctx)
	return client, chSvc, sysSvc, ctx
}

func TestUnifiedQuota_KimiCh51_MultiWindowAndSingleWriterAuthority(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	pastReset := now.Add(-10 * time.Minute)
	futureReset := now.Add(48 * time.Hour)

	// Parse a realistic Kimi payload with both 5h and 7d windows.
	body := []byte(`{
		"usage": {"limit":"100","used":"100","remaining":"0","resetTime":"` + futureReset.Format(time.RFC3339) + `"},
		"limits": [
			{"window":{"duration":5,"timeUnit":"TIME_UNIT_HOUR"},"detail":{"limit":"20","used":"20","remaining":"0","resetTime":"` + pastReset.Format(time.RFC3339) + `"}}
		]
	}`)
	snap := parseKimiCodeUsageBalanceSnapshot(body, http.StatusOK)
	require.True(t, snap.Success)
	require.Len(t, snap.Windows, 2)
	// Legacy scalar fields are preserved alongside normalized Windows[].
	require.NotNil(t, snap.PrimaryBalance)
	require.NotNil(t, snap.ResetAt)

	// CRITICAL INVARIANT: 5h window resetAt is in the past, but 7d weekly window is STILL exhausted!
	// Short window reset MUST NOT prematurely unlock the key.
	require.True(t, snap.ExhaustedAt(now), "expired 5h resetAt must not unlock exhausted 7d window")
	worstRatio, hasRatio := snap.WorstUsageRatioAt(now)
	require.True(t, hasRatio)
	require.InDelta(t, 1.0, worstRatio, 1e-6)

	// Once the 7d window also passes its resetAt, the key is no longer exhausted.
	require.False(t, snap.ExhaustedAt(futureReset.Add(time.Minute)))

	// Single-writer authority: Kimi 3-key channel uses per-key authority; stale provider_quota_status
	// row MUST NOT pollute ChannelQuotaOverview or ProviderQuotaService polling.
	client, chSvc, sysSvc, ctx := newQuotaTestService(t, nil)
	k1, k2, k3 := "sk-kimi-1", "sk-kimi-2", "sk-kimi-3"
	fp1 := objects.ChannelAPIKeyFingerprint(k1)
	fp2 := objects.ChannelAPIKeyFingerprint(k2)
	fp3 := objects.ChannelAPIKeyFingerprint(k3)

	healthySnap := objects.ChannelKeyBalanceSnapshot{
		Success:   true,
		CheckedAt: now,
		Available: lo.ToPtr(true),
		Windows: []objects.ChannelQuotaWindow{
			{Key: "5h", Label: "5h", Used: lo.ToPtr(4.0), Limit: lo.ToPtr(20.0), Remaining: lo.ToPtr(16.0), UsageRatio: lo.ToPtr(0.2), Unit: "%", ResetAt: lo.ToPtr(now.Add(2 * time.Hour))},
			{Key: "7d", Label: "7d", Used: lo.ToPtr(30.0), Limit: lo.ToPtr(100.0), Remaining: lo.ToPtr(70.0), UsageRatio: lo.ToPtr(0.3), Unit: "%", ResetAt: lo.ToPtr(futureReset)},
		},
	}

	ch, err := client.Channel.Create().
		SetName("kimi-ch51").
		SetType(channel.TypeMoonshotCoding).
		SetBaseURL("https://api.kimi.com/coding/v1").
		SetSupportedModels([]string{"kimi-for-coding"}).
		SetDefaultTestModel("kimi-for-coding").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{k1, k2, k3}}).
		SetSettings(&objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled: true,
				Preset:  objects.ChannelBalanceProbePresetKimiCode,
			},
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				KeyMetadata: []objects.ChannelKeyMetadata{
					{ID: fp1, LastProbeAt: &now, LastProbeStatus: "ok", BalanceSnapshot: &snap},        // exhausted on 7d
					{ID: fp2, LastProbeAt: &now, LastProbeStatus: "ok", BalanceSnapshot: &healthySnap}, // healthy
					{ID: fp3, LastProbeAt: &now, LastProbeStatus: "ok", BalanceSnapshot: &healthySnap}, // healthy
				},
			},
		}).
		Save(ctx)
	require.NoError(t, err)
	require.True(t, ChannelUsesPerKeyQuotaAuthority(ch))

	// Seed a stale legacy provider_quota_status row claiming "exhausted" for the entire channel.
	_, err = client.ProviderQuotaStatus.Create().
		SetChannelID(ch.ID).
		SetProviderType(providerquotastatus.ProviderType("kimi_code")).
		SetStatus(providerquotastatus.StatusExhausted).
		SetNextCheckAt(now.Add(time.Hour)).
		SetQuotaData(map[string]any{"stale": true}).
		Save(ctx)
	require.NoError(t, err)

	pqSvc := NewProviderQuotaService(ProviderQuotaServiceParams{
		Ent:           client,
		SystemService: sysSvc,
		HttpClient:    httpclient.NewHttpClient(),
	})

	overview := chSvc.ResolveChannelQuotaOverview(ctx, ch, pqSvc)
	require.NotNil(t, overview)
	require.Equal(t, 3, overview.Counts.Total)
	require.Equal(t, 3, overview.Counts.FreshKnown)
	require.Equal(t, 1, overview.Counts.Exhausted)
	require.Equal(t, 0, overview.Counts.Unknown)
	require.NotEqual(t, ChannelQuotaStateExhausted, overview.State, "2 of 3 keys are healthy; stale provider_quota_status must be ignored")
	require.GreaterOrEqual(t, len(overview.Windows), 1)
	require.LessOrEqual(t, len(overview.Windows), 2)
}

func TestUnifiedQuota_OpenRouterCh45_FreeTierAndBounded84KeyScheduler(t *testing.T) {
	// 1. OpenRouter free-tier fix: total_credits=0, total_usage=0.3738 must NOT pick 0.3738 as balance
	// and must NOT mark the key exhausted.
	freeBody := []byte(`{"data":{"total_credits":0,"total_usage":0.3738}}`)
	snap := parseOpenRouterBalanceSnapshot(freeBody, http.StatusOK)
	require.True(t, snap.Success)
	require.NotNil(t, snap.FreeTier)
	require.True(t, *snap.FreeTier)
	require.NotNil(t, snap.Available)
	require.True(t, *snap.Available, "free-tier OpenRouter key must stay available")
	require.NotNil(t, snap.PrimaryBalance)
	require.InDelta(t, 0.0, snap.PrimaryBalance.Amount, 1e-6)
	require.False(t, snap.ExhaustedAt(time.Now().UTC()), "free-tier key without exhausted hard window must not be exhausted")

	// Paid exhausted OpenRouter key: total_credits=5.0, total_usage=5.0 -> exhausted.
	paidEmpty := parseOpenRouterBalanceSnapshot([]byte(`{"data":{"total_credits":5.0,"total_usage":5.0}}`), http.StatusOK)
	require.True(t, paidEmpty.Success)
	require.True(t, paidEmpty.FreeTier == nil || !*paidEmpty.FreeTier)
	require.NotNil(t, paidEmpty.Available)
	require.False(t, *paidEmpty.Available)
	require.True(t, paidEmpty.ExhaustedAt(time.Now().UTC()))

	// 2. Bounded scheduler on 84-key pool:
	// First verify balanceProbe.enabled=false (Ch 45 frozen state) runs 0 probes.
	var probeCalls atomic.Int32
	rt := channelKeyHealthCheckRoundTripper(func(r *http.Request) (*http.Response, error) {
		probeCalls.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(freeBody)),
			Request:    r,
		}, nil
	})
	client, chSvc, _, ctx := newQuotaTestService(t, rt)

	keys := make([]string, 84)
	for i := range keys {
		keys[i] = fmt.Sprintf("sk-or-v1-%03d", i)
	}
	chFrozen, err := client.Channel.Create().
		SetName("or-ch45-frozen").
		SetType(channel.TypeOpenrouter).
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://openrouter.ai/api/v1").
		SetSupportedModels([]string{"openai/gpt-4o-mini"}).
		SetDefaultTestModel("openai/gpt-4o-mini").
		SetCredentials(objects.ChannelCredentials{APIKeys: keys}).
		SetSettings(&objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled: false,
				Preset:  objects.ChannelBalanceProbePresetOpenRouter,
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	chSvc.runDueQuotaBalanceProbes(ctx, time.Now().UTC())
	require.Equal(t, int32(0), probeCalls.Load(), "disabled balanceProbe must never auto-probe")

	// Now enable balanceProbe on the 84-key channel and verify 1 tick probes AT MOST 8 oldest-due keys
	// and persists immediately.
	_, err = client.Channel.UpdateOneID(chFrozen.ID).
		SetSettings(&objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled: true,
				Preset:  objects.ChannelBalanceProbePresetOpenRouter,
			},
		}).
		Save(ctx)
	require.NoError(t, err)

	origInterval := quotaProbeOriginInterval
	quotaProbeOriginInterval = 0
	t.Cleanup(func() { quotaProbeOriginInterval = origInterval })
	chSvc.runDueQuotaBalanceProbes(ctx, time.Now().UTC())
	require.Equal(t, int32(maxQuotaProbeKeysPerChannelPerTick), probeCalls.Load(), "single tick must probe at most 8 keys")

	reloaded, err := client.Channel.Get(ctx, chFrozen.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.Settings)
	require.NotNil(t, reloaded.Settings.KeyHealthCheck)
	require.Len(t, reloaded.Settings.KeyHealthCheck.KeyMetadata, 8, "8 probed keys must be persisted immediately")
}

func TestUnifiedQuota_ExaCh68_SharedWalletAdminKeySecurityAndCrypto(t *testing.T) {
	t.Setenv("AXONHUB_PROBE_SECRET_KEY", "test-probe-master-secret-key-2026")

	var hits atomic.Int32
	var gotAuth atomic.Value
	var gotURL atomic.Value
	rt := channelKeyHealthCheckRoundTripper(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		gotAuth.Store(r.Header.Get("x-api-key") + "|" + r.Header.Get("Authorization"))
		gotURL.Store(r.URL.String())
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"credits_usd":50.0}`)),
			Request:    r,
		}, nil
	})

	client, chSvc, _, _ := newQuotaTestService(t, rt)
	adminUser := &ent.User{
		ID:      1,
		IsOwner: true,
		Scopes:  []string{string(scopes.ScopeWriteChannels), string(scopes.ScopeReadChannels)},
	}
	ctx := contexts.WithUser(ent.NewContext(context.Background(), client), adminUser)
	ctx, err := authz.WithPrincipal(ctx, authz.Principal{Type: authz.PrincipalTypeUser})
	require.NoError(t, err)

	// 1. Validation MUST reject custom HTTP override on exa_team_credits.
	_, err = chSvc.CreateChannel(ctx, ent.CreateChannelInput{
		Name:             "exa-bad-http",
		Type:             channel.TypeOpenai,
		BaseURL:          lo.ToPtr("https://api.exa.ai"),
		SupportedModels:  []string{"exa"},
		DefaultTestModel: "exa",
		Credentials:      objects.ChannelCredentials{APIKeys: []string{"exa-inf-1"}},
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled: true,
				Preset:  objects.ChannelBalanceProbePresetExa,
				HTTP: &objects.ChannelKeyHealthCheckHTTPRule{
					Method:  objects.ChannelKeyHealthCheckHTTPMethodGet,
					URLMode: objects.ChannelKeyHealthCheckHTTPURLModeAbsoluteURL,
					URL:     "https://attacker.example.com/steal",
				},
			},
		},
	})
	require.Error(t, err, "exa_team_credits must reject custom HTTP override")

	// 2. Validation MUST reject blank or masked ProbeAPIKey placeholders on SET.
	for _, bad := range []string{"", "   ", "****", "••••••", "sk-***123", "sk-...999"} {
		_, err := chSvc.CreateChannel(ctx, ent.CreateChannelInput{
			Name:             "exa-bad-secret-" + bad,
			Type:             channel.TypeOpenai,
			BaseURL:          lo.ToPtr("https://api.exa.ai"),
			SupportedModels:  []string{"exa"},
			DefaultTestModel: "exa",
			Credentials:      objects.ChannelCredentials{APIKeys: []string{"exa-inf-1"}},
			Settings: &objects.ChannelSettings{
				BalanceProbe: &objects.ChannelBalanceProbe{
					Enabled:       false,
					Preset:        objects.ChannelBalanceProbePresetExa,
					ProbeAPIKeyOp: objects.ChannelSecretWriteOpSet,
					ProbeAPIKey:   bad,
				},
			},
		})
		require.Error(t, err, "must reject placeholder/blank probeAPIKey: %q", bad)
	}

	// 3. Create 8-key Exa channel WITHOUT ProbeAPIKey first: probe must fail with missing_probe_key
	// and NEVER fall back to inference API keys!
	exaKeys := []string{"exa-inf-1", "exa-inf-2", "exa-inf-3", "exa-inf-4", "exa-inf-5", "exa-inf-6", "exa-inf-7", "exa-inf-8"}
	ch, err := chSvc.CreateChannel(ctx, ent.CreateChannelInput{
		Name:             "exa-ch68",
		Type:             channel.TypeOpenai,
		BaseURL:          lo.ToPtr("https://custom-proxy.example.com/v1"),
		SupportedModels:  []string{"exa"},
		DefaultTestModel: "exa",
		Credentials:      objects.ChannelCredentials{APIKeys: exaKeys},
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled:      false,
				Preset:       objects.ChannelBalanceProbePresetExa,
				SharedWallet: true,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, ch.Settings.BalanceProbe.HasProbeAPIKey())

	missingRes := chSvc.runChannelKeyBalanceProbe(ctx, ch, exaKeys[0], objects.ChannelKeyHealthCheckTriggerManual)
	require.False(t, missingRes.Success)
	require.Equal(t, "missing_probe_key", missingRes.ProbeStatus)
	require.Equal(t, int32(0), hits.Load(), "missing Exa admin key must NOT call upstream or fall back to inference key")

	// 4. Set write-only ProbeAPIKey -> encrypted at rest with enc:v1:, transient fields cleared,
	// and subsequent generic UpdateChannel preserves the ciphertext!
	const adminSecret = "exa-admin-secret-live-token"
	updated, err := chSvc.UpdateChannel(ctx, ch.ID, &ent.UpdateChannelInput{
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled:       true,
				Preset:        objects.ChannelBalanceProbePresetExa,
				SharedWallet:  true,
				ProbeAPIKeyOp: objects.ChannelSecretWriteOpSet,
				ProbeAPIKey:   adminSecret,
			},
		},
	})
	require.NoError(t, err)
	require.True(t, updated.Settings.BalanceProbe.HasProbeAPIKey())
	require.True(t, strings.HasPrefix(updated.Settings.BalanceProbe.ProbeAPIKeyCiphertext, probeAPIKeyCipherPrefix))
	require.Empty(t, updated.Settings.BalanceProbe.ProbeAPIKey, "plaintext probeAPIKey must never stay on persisted struct")
	rawJSON, err := json.Marshal(updated.Settings.BalanceProbe)
	require.NoError(t, err)
	require.NotContains(t, string(rawJSON), adminSecret, "serialized BalanceProbe must NEVER leak plaintext ProbeAPIKey")

	// Generic UpdateChannel omitting ProbeAPIKey preserves stored ciphertext!
	savedCipher := updated.Settings.BalanceProbe.ProbeAPIKeyCiphertext
	updated2, err := chSvc.UpdateChannel(ctx, ch.ID, &ent.UpdateChannelInput{
		Name: lo.ToPtr("exa-ch68-renamed"),
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled:      true,
				Preset:       objects.ChannelBalanceProbePresetExa,
				SharedWallet: true,
			},
		},
	})
	require.NoError(t, err)
	require.Equal(t, savedCipher, updated2.Settings.BalanceProbe.ProbeAPIKeyCiphertext, "generic updateChannel must preserve encrypted ProbeAPIKey")

	// 5. RefreshChannelQuota on 8-key SharedWallet Exa channel:
	// MUST send ONLY 1 upstream request to the locked official HTTPS Admin URL using adminSecret,
	// and overview MUST report 50 (NOT 50*8 = 400)!
	origInterval := quotaProbeOriginInterval
	quotaProbeOriginInterval = 0
	t.Cleanup(func() { quotaProbeOriginInterval = origInterval })
	payload, err := chSvc.RefreshChannelQuota(ctx, ch.ID, nil, nil)
	require.NoError(t, err)
	require.Equal(t, RefreshChannelQuotaStatusUpdated, payload.Status)
	require.Equal(t, int32(1), hits.Load(), "SharedWallet 8-key Exa channel must probe upstream ONCE, not 8 times")
	require.Equal(t, exaTeamCreditsEndpointURL, gotURL.Load(), "Exa must hit locked official HTTPS Admin URL, ignoring custom BaseURL")
	require.Contains(t, gotAuth.Load().(string), adminSecret)
	require.NotContains(t, gotAuth.Load().(string), "exa-inf-1")

	require.NotNil(t, payload.QuotaOverview)
	require.True(t, payload.QuotaOverview.SharedWallet)
	require.Equal(t, "shared_wallet", payload.QuotaOverview.WalletScope)
	require.NotNil(t, payload.QuotaOverview.WalletRemaining)
	require.InDelta(t, 50.0, *payload.QuotaOverview.WalletRemaining, 1e-6, "SharedWallet must NOT multiply balance by 8")

	// 6. Preset-bound AAD: Exa Admin ciphertext MUST NOT decrypt under another preset,
	// and switching preset on UpdateChannel without SET MUST clear the stored ciphertext!
	_, decErr := DecryptProbeAPIKey(ctx, nil, objects.ChannelBalanceProbePresetCustom, savedCipher)
	require.Error(t, decErr, "Exa Admin ciphertext must fail AAD decryption under any other preset")

	switchedPreset, err := chSvc.UpdateChannel(ctx, ch.ID, &ent.UpdateChannelInput{
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled: false,
				Preset:  objects.ChannelBalanceProbePresetDeepSeek,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, switchedPreset.Settings.BalanceProbe.HasProbeAPIKey(), "switching preset must clear previous admin ProbeAPIKey")

	// 7. Script + Exa/ProbeAPIKey MUST be rejected by validation.
	_, err = chSvc.UpdateChannel(ctx, ch.ID, &ent.UpdateChannelInput{
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled:       false,
				Preset:        objects.ChannelBalanceProbePresetExa,
				ProbeAPIKeyOp: objects.ChannelSecretWriteOpSet,
				ProbeAPIKey:   adminSecret,
				Script: &objects.ChannelBalanceProbeScript{
					Mode:    objects.ChannelBalanceProbeScriptModeCommand,
					Command: []string{"/bin/sh", "-c", "env"},
				},
			},
		},
	})
	require.Error(t, err, "probe scripts must be rejected when combined with Exa or admin ProbeAPIKey")

	// 8. Explicit CLEAR removes stored ciphertext.
	_, err = chSvc.UpdateChannel(ctx, ch.ID, &ent.UpdateChannelInput{
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled:       false,
				Preset:        objects.ChannelBalanceProbePresetExa,
				SharedWallet:  true,
				ProbeAPIKeyOp: objects.ChannelSecretWriteOpSet,
				ProbeAPIKey:   adminSecret,
			},
		},
	})
	require.NoError(t, err)
	cleared, err := chSvc.UpdateChannel(ctx, ch.ID, &ent.UpdateChannelInput{
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled:       false,
				Preset:        objects.ChannelBalanceProbePresetExa,
				SharedWallet:  true,
				ProbeAPIKeyOp: objects.ChannelSecretWriteOpClear,
			},
		},
	})
	require.NoError(t, err)
	require.False(t, cleared.Settings.BalanceProbe.HasProbeAPIKey())
	require.Empty(t, cleared.Settings.BalanceProbe.ProbeAPIKeyCiphertext)

	// 7. Redirect MUST NOT be followed on Exa probe.
	var redirectTargets atomic.Int32
	redirRT := channelKeyHealthCheckRoundTripper(func(r *http.Request) (*http.Response, error) {
		redirectTargets.Add(1)
		return &http.Response{
			StatusCode: http.StatusFound,
			Status:     "302 Found",
			Header:     http.Header{"Location": []string{"https://evil.example.com/steal"}},
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    r,
		}, nil
	})
	chSvc.httpClient = httpclient.NewHttpClientWithClient(&http.Client{Transport: redirRT})
	updated2.Settings.BalanceProbe.ProbeAPIKeyCiphertext = savedCipher
	redirRes := chSvc.runChannelKeyBalanceProbe(ctx, updated2, exaKeys[0], objects.ChannelKeyHealthCheckTriggerManual)
	require.False(t, redirRes.Success)
	require.Equal(t, int32(1), redirectTargets.Load(), "Exa admin probe must NOT follow redirects")
}

func TestUnifiedQuota_TavilyCh69_ManualLedgerCh73_AndAllNewPresets(t *testing.T) {
	rt := channelKeyHealthCheckRoundTripper(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Host, "tavily.com"):
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"key":{"usage":1001,"limit":1000},"account":{"plan_usage":1001,"plan_limit":1000}}`)),
				Request:    r,
			}, nil
		case strings.Contains(r.URL.Path, "/alpha/billing/credits"):
			require.Equal(t, commandCodeUserAgent, r.Header.Get("User-Agent"))
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"credits":{"monthlyCredits":45,"purchasedCredits":10,"freeCredits":0},"windowLimits":{"fiveHour":{"used":25,"limit":100,"resetsAt":"2026-11-24T15:00:00Z"},"weekly":{"used":120,"limit":500,"resetsAt":"2026-11-30T00:00:00Z"}}}`)),
				Request:    r,
			}, nil
		case strings.Contains(r.URL.Path, "/subscriptions"):
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"data":{"planId":"goat"}}`)),
				Request:    r,
			}, nil
		case strings.Contains(r.URL.Path, "/step_plan/v1/usage"):
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"plan":{"name":"Step Plan Pro"},"quota":{"allowed":true,"windows":[{"label":"5h","used_ratio":0.4,"reset_at":"2026-11-24T18:00:00Z"}]}}`)),
				Request:    r,
			}, nil
		case strings.Contains(r.URL.Path, "/api/usage/token"):
			if strings.Contains(r.Header.Get("Authorization"), "unlimited") {
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":true,"data":{"unlimited_quota":true,"total_used":1250000}}`)),
					Request:    r,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":true,"data":{"total_granted":5000000,"total_used":1500000,"total_available":3500000,"unlimited_quota":false}}`)),
				Request:    r,
			}, nil
		case strings.Contains(r.URL.Path, "/v1/dashboard/billing/subscription"):
			if strings.Contains(r.Header.Get("Authorization"), "sentinel") {
				return &http.Response{
					StatusCode: http.StatusOK,
					Status:     "200 OK",
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"hard_limit_usd":100000000}`)),
					Request:    r,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"hard_limit_usd":120.0,"plan":{"title":"Tier 3"}}`)),
				Request:    r,
			}, nil
		case strings.Contains(r.URL.Path, "/v1/dashboard/billing/usage"):
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"total_usage":2550}`)), // cents = $25.50
				Request:    r,
			}, nil
		case strings.Contains(r.URL.Path, "/v1/usage"):
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"balance":42.5,"used":7.5,"limit":50.0,"currency":"USD"}`)),
				Request:    r,
			}, nil
		default:
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Status:     "404 Not Found",
				Body:       io.NopCloser(strings.NewReader(`{"error":"not found"}`)),
				Request:    r,
			}, nil
		}
	})
	_, chSvc, _, ctx := newQuotaTestService(t, rt)

	// Tavily 1001/1000
	tavCh := &ent.Channel{
		Type:    channel.TypeOpenai,
		BaseURL: "https://api.tavily.com",
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{Enabled: true, Preset: objects.ChannelBalanceProbePresetTavily},
		},
	}
	tavRes := chSvc.runChannelKeyBalanceProbe(ctx, tavCh, "tvly-key-1", objects.ChannelKeyHealthCheckTriggerManual)
	require.True(t, tavRes.Success)
	require.True(t, tavRes.ObservationValid)
	require.Equal(t, "ok", tavRes.ProbeStatus)
	require.NotNil(t, tavRes.Available)
	require.False(t, *tavRes.Available)
	require.True(t, tavRes.BalanceSnapshot.ExhaustedAt(time.Now().UTC()))

	// CommandCode billing (/alpha/billing/credits + /subscriptions + User-Agent)
	ccCh := &ent.Channel{
		Type:    channel.TypeOpenai,
		BaseURL: "https://api.commandcode.ai/v1",
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{Enabled: true, Preset: objects.ChannelBalanceProbePresetCommandCode},
		},
	}
	ccRes := chSvc.runChannelKeyBalanceProbe(ctx, ccCh, "cc-key-1", objects.ChannelKeyHealthCheckTriggerManual)
	require.True(t, ccRes.ObservationValid)
	require.Equal(t, "GOAT", ccRes.BalanceSnapshot.PlanName)
	require.Len(t, ccRes.BalanceSnapshot.Windows, 3)

	// StepFun plan usage
	stepCh := &ent.Channel{
		Type:    channel.TypeOpenai,
		BaseURL: "https://api.stepfun.com/v1",
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{Enabled: true, Preset: objects.ChannelBalanceProbePresetStepPlan},
		},
	}
	stepRes := chSvc.runChannelKeyBalanceProbe(ctx, stepCh, "step-key-1", objects.ChannelKeyHealthCheckTriggerManual)
	require.True(t, stepRes.ObservationValid)
	require.NotEmpty(t, stepRes.BalanceSnapshot.Windows)

	// NewAPI token usage (÷500000 USD + unlimited_quota)
	newAPICh := &ent.Channel{
		Type:    channel.TypeOpenai,
		BaseURL: "https://api.n.icu/v1",
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{Enabled: true, Preset: objects.ChannelBalanceProbePresetNewAPI},
		},
	}
	newRes := chSvc.runChannelKeyBalanceProbe(ctx, newAPICh, "sk-newapi-1", objects.ChannelKeyHealthCheckTriggerManual)
	require.True(t, newRes.ObservationValid)
	require.NotNil(t, newRes.BalanceSnapshot.Remaining)
	require.InDelta(t, 7.0, *newRes.BalanceSnapshot.Remaining, 1e-6)
	require.InDelta(t, 10.0, *newRes.BalanceSnapshot.Limit, 1e-6)
	newUnlim := chSvc.runChannelKeyBalanceProbe(ctx, newAPICh, "sk-newapi-unlimited", objects.ChannelKeyHealthCheckTriggerManual)
	require.True(t, newUnlim.ObservationValid)
	require.NotNil(t, newUnlim.BalanceSnapshot.Unlimited)
	require.True(t, *newUnlim.BalanceSnapshot.Unlimited)
	require.False(t, newUnlim.BalanceSnapshot.ExhaustedAt(time.Now().UTC()))

	// OpenAI billing usage (normal + >=1e7 infinite sentinel)
	oaiCh := &ent.Channel{
		Type:    channel.TypeOpenai,
		BaseURL: "https://api.openai-next.com/v1",
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{Enabled: true, Preset: objects.ChannelBalanceProbePresetOpenAIBilling},
		},
	}
	oaiRes := chSvc.runChannelKeyBalanceProbe(ctx, oaiCh, "sk-oai-1", objects.ChannelKeyHealthCheckTriggerManual)
	require.True(t, oaiRes.ObservationValid)
	require.NotNil(t, oaiRes.BalanceSnapshot.Remaining)
	require.InDelta(t, 94.5, *oaiRes.BalanceSnapshot.Remaining, 1e-6)
	oaiSentinel := chSvc.runChannelKeyBalanceProbe(ctx, oaiCh, "sk-oai-sentinel", objects.ChannelKeyHealthCheckTriggerManual)
	require.True(t, oaiSentinel.ObservationValid)
	require.NotNil(t, oaiSentinel.BalanceSnapshot.Unlimited)
	require.True(t, *oaiSentinel.BalanceSnapshot.Unlimited)

	// Sub2API usage
	sub2Ch := &ent.Channel{
		Type:    channel.TypeOpenai,
		BaseURL: "https://sub2.example.com/v1",
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{Enabled: true, Preset: objects.ChannelBalanceProbePresetSub2},
		},
	}
	sub2Res := chSvc.runChannelKeyBalanceProbe(ctx, sub2Ch, "sk-sub2-1", objects.ChannelKeyHealthCheckTriggerManual)
	require.True(t, sub2Res.ObservationValid)
	require.NotNil(t, sub2Res.BalanceSnapshot.Remaining)
	require.InDelta(t, 42.5, *sub2Res.BalanceSnapshot.Remaining, 1e-6)

	// Explicit preset priority over domain default (e.g. legacy moonshot_balance honored on api.kimi.com)
	explicitCh := &ent.Channel{
		Type:    channel.TypeMoonshotCoding,
		BaseURL: "https://api.kimi.com/coding/v1",
		Settings: &objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{Preset: objects.ChannelBalanceProbePresetMoonshot},
		},
	}
	spec, _, ok := channelBalanceProbeSpecForChannel(explicitCh)
	require.True(t, ok)
	require.Equal(t, objects.ChannelBalanceProbePresetMoonshot, spec.ID)

	// Ch 73: Manual static $100 ledger preserved across probe failure.
	checkedAt := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	manualSnap := &objects.ChannelKeyBalanceSnapshot{
		Success:        true,
		Source:         "manual",
		CheckedAt:      checkedAt,
		Available:      lo.ToPtr(true),
		Limit:          lo.ToPtr(100.0),
		Remaining:      lo.ToPtr(100.0),
		PrimaryBalance: &objects.BalanceAmount{Currency: "USD", Amount: 100, Kind: objects.BalanceAmountKindRemaining},
	}
	rawKey73 := "sk-openai-ch73-raw"
	fp73 := objects.ChannelAPIKeyFingerprint(rawKey73)
	meta := []objects.ChannelKeyMetadata{{
		ID:              fp73,
		LastCheckedAt:   &checkedAt,
		Success:         lo.ToPtr(true),
		FailureCount:    0,
		BalanceSnapshot: manualSnap,
	}}
	nextProbe := checkedAt.Add(30 * time.Minute)
	updatedMeta := upsertChannelKeyBalanceProbeMetadata(meta, rawKey73, ChannelKeyHealthCheckResult{
		Success:          false,
		ObservationValid: false,
		ProbeStatus:      "error",
		StatusCode:       404,
		Reason:           "404 not found",
	}, checkedAt.Add(time.Minute), &nextProbe)
	require.Len(t, updatedMeta, 1)
	require.NotNil(t, updatedMeta[0].BalanceSnapshot)
	require.InDelta(t, 100.0, updatedMeta[0].BalanceSnapshot.PrimaryBalance.Amount, 1e-6, "failed probe must NEVER wipe manual $100 snapshot")
	require.Equal(t, "error", updatedMeta[0].LastProbeStatus)
	require.Equal(t, 1, updatedMeta[0].ProbeFailureCount)
	// Sub2API real /v1/usage contract: nested quota + rate_limits[] (wallet remaining > 0
	// but 7d weekly rate limit exhausted MUST mark key unavailable / exhausted!).
	sub2Real := parseSub2UsageSnapshot([]byte(`{
		"mode":"quota_limited",
		"isValid":true,
		"status":"active",
		"quota":{"limit":100,"used":20,"remaining":80,"unit":"USD"},
		"remaining":80,
		"unit":"USD",
		"rate_limits":[
			{"window":"5h","limit":10,"used":10,"remaining":0,"reset_at":"2026-11-23T00:00:00Z"},
			{"window":"7d","limit":50,"used":50,"remaining":0,"reset_at":"2026-11-30T00:00:00Z"}
		]
	}`), http.StatusOK)
	require.True(t, sub2Real.Success)
	require.Len(t, sub2Real.Windows, 2)
	require.True(t, sub2Real.ExhaustedAt(time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)), "exhausted 7d rate_limit must mark Sub2 key exhausted even when wallet remaining is $80")

	// Sub2API rate-limit-only response without quota object must STILL be accepted as valid.
	sub2RLOnly := parseSub2UsageSnapshot([]byte(`{
		"mode":"quota_limited",
		"isValid":true,
		"rate_limits":[{"window":"5h","limit":10,"used":3,"remaining":7,"reset_at":"2026-11-25T00:00:00Z"}]
	}`), http.StatusOK)
	require.True(t, sub2RLOnly.Success)
	require.Len(t, sub2RLOnly.Windows, 1)

	// NewAPI missing numeric fields when !unlimited must NOT be accepted as $0 exhausted.
	newAPIMalformed := parseNewAPITokenUsageSnapshot([]byte(`{"code":true,"data":{"object":"token_usage","unlimited_quota":false}}`), http.StatusOK)
	require.False(t, newAPIMalformed.Success)

	// Ch 73 single-key OpenAI channel with manual $100 static ledger MUST classify onto per-key
	// authority and surface $100 in ResolveChannelQuotaOverview!
	ch73 := &ent.Channel{
		ID:          73,
		Name:        "openai-official-ch73",
		Type:        channel.TypeOpenai,
		BaseURL:     "https://api.openai.com/v1",
		Credentials: objects.ChannelCredentials{APIKey: rawKey73},
		Settings: &objects.ChannelSettings{
			KeyHealthCheck: &objects.ChannelKeyHealthCheck{
				KeyMetadata: updatedMeta,
			},
		},
	}
	require.True(t, ChannelUsesPerKeyQuotaAuthority(ch73))
	ov73 := chSvc.ResolveChannelQuotaOverview(ctx, ch73, nil)
	require.NotNil(t, ov73)
	require.NotNil(t, ov73.WalletRemaining)
	require.InDelta(t, 100.0, *ov73.WalletRemaining, 1e-6, "Ch 73 manual $100 ledger must survive failed probe and appear in ChannelQuotaOverview")
}

func TestUnifiedQuota_ProbeFailureIsolation_NeverPollutesHealthOrFailurePolicies(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	infChecked := now.Add(-5 * time.Minute)
	infNextCheck := now.Add(15 * time.Minute)

	// 1. Existing inference backoff/FailureCount must NEVER be touched by failed OR successful probes.
	rawIsoKey := "sk-isolation-key-1"
	fpIso := objects.ChannelAPIKeyFingerprint(rawIsoKey)
	meta := []objects.ChannelKeyMetadata{{
		ID:             fpIso,
		LastCheckedAt:  &infChecked,
		Success:        lo.ToPtr(false),
		FailureCount:   3,
		BackoffAttempt: 2,
		NextCheckAt:    &infNextCheck,
		BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
			Success:        true,
			CheckedAt:      infChecked,
			Remaining:      lo.ToPtr(25.0),
			PrimaryBalance: &objects.BalanceAmount{Currency: "USD", Amount: 25, Kind: objects.BalanceAmountKindRemaining},
		},
	}}

	for _, status := range []int{401, 404, 429, 500} {
		meta = upsertChannelKeyBalanceProbeMetadata(meta, rawIsoKey, ChannelKeyHealthCheckResult{
			Success:          false,
			ObservationValid: false,
			ProbeStatus:      "error",
			StatusCode:       status,
			Reason:           fmt.Sprintf("http %d", status),
		}, now, lo.ToPtr(now.Add(10*time.Minute)))
		require.Equal(t, 3, meta[0].FailureCount)
		require.Equal(t, 2, meta[0].BackoffAttempt)
		require.Equal(t, &infNextCheck, meta[0].NextCheckAt)
		require.InDelta(t, 25.0, meta[0].BalanceSnapshot.PrimaryBalance.Amount, 1e-6)
	}

	// Successful balance probe MUST update snapshot & clear ProbeFailureCount,
	// but MUST NOT clear inference FailureCount/BackoffAttempt!
	okMeta := upsertChannelKeyBalanceProbeMetadata(meta, rawIsoKey, ChannelKeyHealthCheckResult{
		Success:          true,
		ObservationValid: true,
		ProbeStatus:      "ok",
		BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
			Success:        true,
			CheckedAt:      now.Add(time.Minute),
			Remaining:      lo.ToPtr(19.5),
			PrimaryBalance: &objects.BalanceAmount{Currency: "USD", Amount: 19.5, Kind: objects.BalanceAmountKindRemaining},
		},
	}, now.Add(time.Minute), lo.ToPtr(now.Add(16*time.Minute)))
	require.Equal(t, 0, okMeta[0].ProbeFailureCount)
	require.InDelta(t, 19.5, okMeta[0].BalanceSnapshot.PrimaryBalance.Amount, 1e-6)
	require.Equal(t, 3, okMeta[0].FailureCount, "successful probe must NOT clear inference FailureCount")
	require.Equal(t, 2, okMeta[0].BackoffAttempt, "successful probe must NOT clear inference BackoffAttempt")

	// 2. FailurePolicy isolation matrix:
	widePolicies := []objects.FailurePolicyProfile{
		{
			ID:         "wildcard-401-404-429",
			Name:       "Disable on 401/404/429",
			Sources:    nil, // empty sources = wildcard
			Conditions: objects.ChannelKeyHealthCheckPolicyCondition{StatusCodes: []int{401, 404, 429}},
			Actions:    []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDisableKey}},
		},
		{
			ID:         "explicit-failure-source-no-balance",
			Name:       "Disable on balance probe failure",
			Sources:    []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceScheduledBalanceProbeFailure, objects.FailurePolicyEventSourceManualBalanceProbeFailure},
			Conditions: objects.ChannelKeyHealthCheckPolicyCondition{Success: lo.ToPtr(false)},
			Actions:    []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDisableKey}},
		},
		{
			ID:                "or-bypass-balance-predicate",
			Name:              "OR bypass",
			Sources:           []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceScheduledBalanceProbe},
			ConditionCombiner: objects.FailurePolicyConditionCombinerOr,
			Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
				BalanceLTE: lo.ToPtr(0.0),
				Success:    lo.ToPtr(true),
			},
			Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDisableKey}},
		},
		{
			ID:         "all-checked-keys-failed",
			Name:       "Disable channel when all failed",
			Sources:    []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceScheduledBalanceProbe},
			Conditions: objects.ChannelKeyHealthCheckPolicyCondition{AllCheckedKeysFailed: lo.ToPtr(true)},
			Actions:    []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDisableChannel}},
		},
	}

	// Probe failure events NEVER match ANY policy!
	for _, src := range []objects.FailurePolicyEventSource{
		objects.FailurePolicyEventSourceScheduledBalanceProbeFailure,
		objects.FailurePolicyEventSourceManualBalanceProbeFailure,
	} {
		matches := evaluateFailurePolicyProfiles(widePolicies, failurePolicyEvent{
			Source:               src,
			Target:               objects.FailurePolicyTargetKey,
			StatusCode:           401,
			Success:              false,
			ObservationValid:     false,
			AllCheckedKeysFailed: true,
			CheckedAt:            now,
		})
		require.Empty(t, matches, "balance probe failure must NEVER trigger any failure policy")
	}

	// Valid observation with positive balance $10 must NOT match OR-bypass or allCheckedKeysFailed!
	matches := evaluateFailurePolicyProfiles(widePolicies, failurePolicyEvent{
		Source:               objects.FailurePolicyEventSourceScheduledBalanceProbe,
		Target:               objects.FailurePolicyTargetKey,
		Success:              true,
		ObservationValid:     true,
		Balance:              10.0,
		BalanceSnapshot:      &objects.ChannelKeyBalanceSnapshot{Success: true, Remaining: lo.ToPtr(10.0)},
		AllCheckedKeysFailed: true,
		CheckedAt:            now,
	})
	require.Empty(t, matches, "OR combiner must NOT bypass balanceLTE, and allCheckedKeysFailed without balance predicate must NOT match")

	// Explicit AND balance predicate on valid observation DOES match when balance <= 0!
	validLowBalancePolicy := []objects.FailurePolicyProfile{{
		ID:                "disable-zero-balance",
		Name:              "Disable zero balance",
		Sources:           []objects.FailurePolicyEventSource{objects.FailurePolicyEventSourceScheduledBalanceProbe},
		ConditionCombiner: objects.FailurePolicyConditionCombinerAnd,
		Conditions: objects.ChannelKeyHealthCheckPolicyCondition{
			BalanceLTE: lo.ToPtr(0.0),
		},
		Actions: []objects.FailurePolicyAction{{Type: objects.FailurePolicyActionDisableKey}},
	}}
	validMatches := evaluateFailurePolicyProfiles(validLowBalancePolicy, failurePolicyEvent{
		Source:           objects.FailurePolicyEventSourceScheduledBalanceProbe,
		Target:           objects.FailurePolicyTargetKey,
		Success:          true,
		ObservationValid: true,
		Balance:          0.0,
		BalanceSnapshot:  &objects.ChannelKeyBalanceSnapshot{Success: true, Remaining: lo.ToPtr(0.0)},
		CheckedAt:        now,
	})
	require.Len(t, validMatches, 1)
}

func TestUnifiedQuota_SingleflightConcurrencyCASMergeAndPermissions(t *testing.T) {
	var upstreamCalls atomic.Int32
	var inFlight atomic.Int32
	var maxInFlight atomic.Int32
	blockCh := make(chan struct{})
	started := make(chan struct{}, 16)

	rt := channelKeyHealthCheckRoundTripper(func(r *http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		cur := inFlight.Add(1)
		for {
			prev := maxInFlight.Load()
			if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
				break
			}
		}
		defer inFlight.Add(-1)

		select {
		case started <- struct{}{}:
		default:
		}
		<-blockCh
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"88.00"}]}`)),
			Request:    r,
		}, nil
	})

	client, chSvc, _, baseCtx := newQuotaTestService(t, rt)
	origInterval := quotaProbeOriginInterval
	quotaProbeOriginInterval = 0
	t.Cleanup(func() { quotaProbeOriginInterval = origInterval })

	ch, err := client.Channel.Create().
		SetName("deepseek-cas").
		SetType(channel.TypeDeepseek).
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://api.deepseek.com/v1").
		SetSupportedModels([]string{"deepseek-chat"}).
		SetDefaultTestModel("deepseek-chat").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"sk-ds-1"}}).
		SetSettings(&objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{
				Enabled: true,
				Preset:  objects.ChannelBalanceProbePresetDeepSeek,
			},
		}).
		Save(baseCtx)
	require.NoError(t, err)

	// 1. Permission gate: read-only user MUST be rejected by RefreshChannelQuota!
	readOnlyUser := &ent.User{ID: 2, IsOwner: false, Scopes: []string{string(scopes.ScopeReadChannels)}}
	readOnlyCtx := contexts.WithUser(ent.NewContext(context.Background(), client), readOnlyUser)
	readOnlyCtx, err = authz.WithPrincipal(readOnlyCtx, authz.Principal{Type: authz.PrincipalTypeUser})
	require.NoError(t, err)
	_, err = chSvc.RefreshChannelQuota(readOnlyCtx, ch.ID, nil, nil)
	require.Error(t, err)
	require.Equal(t, int32(0), upstreamCalls.Load())

	// 2. Singleflight + CAS conflict merge:
	writerUser := &ent.User{ID: 1, IsOwner: true, Scopes: []string{string(scopes.ScopeWriteChannels)}}
	writerCtx := contexts.WithUser(ent.NewContext(context.Background(), client), writerUser)
	writerCtx, err = authz.WithPrincipal(writerCtx, authz.Principal{Type: authz.PrincipalTypeUser})
	require.NoError(t, err)

	var wg sync.WaitGroup
	errs := make([]error, 4)
	payloads := make([]*RefreshChannelQuotaPayload, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			payloads[idx], errs[idx] = chSvc.RefreshChannelQuota(writerCtx, ch.ID, nil, nil)
		}(i)
	}

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream probe to start")
	}
	time.Sleep(15 * time.Millisecond)

	// Concurrently update a non-quota field on the channel to bump UpdatedAt.
	_, err = client.Channel.UpdateOneID(ch.ID).
		SetName("deepseek-cas-concurrent-edit").
		SetTags([]string{"concurrent-tag"}).
		SetUpdatedAt(time.Now().UTC().Add(5 * time.Second)).
		Save(baseCtx)
	require.NoError(t, err)

	close(blockCh)
	wg.Wait()

	for i := 0; i < 4; i++ {
		require.NoError(t, errs[i])
		require.NotNil(t, payloads[i])
		require.Equal(t, RefreshChannelQuotaStatusUpdated, payloads[i].Status)
	}
	require.Equal(t, int32(1), upstreamCalls.Load(), "singleflight + CAS merge must call upstream EXACTLY ONCE")

	finalCh, err := client.Channel.Get(baseCtx, ch.ID)
	require.NoError(t, err)
	require.Equal(t, "deepseek-cas-concurrent-edit", finalCh.Name, "CAS merge must preserve concurrent non-quota edits")
	require.Equal(t, []string{"concurrent-tag"}, finalCh.Tags)
	require.NotNil(t, finalCh.Settings.KeyHealthCheck)
	require.Len(t, finalCh.Settings.KeyHealthCheck.KeyMetadata, 1)
	require.NotNil(t, finalCh.Settings.KeyHealthCheck.KeyMetadata[0].BalanceSnapshot)
	require.InDelta(t, 88.0, finalCh.Settings.KeyHealthCheck.KeyMetadata[0].BalanceSnapshot.PrimaryBalance.Amount, 1e-6)

	// 3. Global concurrency cap <= 4 across 8 distinct keys:
	upstreamCalls.Store(0)
	maxInFlight.Store(0)
	blockCh = make(chan struct{})
	eightKeys := make([]string, 8)
	for i := range eightKeys {
		eightKeys[i] = "sk-ds-cap-" + strconv.Itoa(i)
	}
	ch8, err := client.Channel.Create().
		SetName("deepseek-cap8").
		SetType(channel.TypeDeepseek).
		SetStatus(channel.StatusEnabled).
		SetBaseURL("https://api.deepseek.com/v1").
		SetSupportedModels([]string{"deepseek-chat"}).
		SetDefaultTestModel("deepseek-chat").
		SetCredentials(objects.ChannelCredentials{APIKeys: eightKeys}).
		SetSettings(&objects.ChannelSettings{
			BalanceProbe: &objects.ChannelBalanceProbe{Enabled: true, Preset: objects.ChannelBalanceProbePresetDeepSeek},
		}).
		Save(baseCtx)
	require.NoError(t, err)

	doneCap := make(chan error, 1)
	go func() {
		_, callErr := chSvc.RefreshChannelQuota(writerCtx, ch8.ID, nil, nil)
		doneCap <- callErr
	}()
	for i := 0; i < 4; i++ {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for concurrent probe %d/4 to start", i+1)
		}
	}
	time.Sleep(25 * time.Millisecond)
	require.LessOrEqual(t, maxInFlight.Load(), int32(maxInFlightQuotaProbesPerOwner), "global in-flight probes must stay <= 4")
	close(blockCh)
	require.NoError(t, <-doneCap)
	require.Equal(t, int32(8), upstreamCalls.Load())
	require.LessOrEqual(t, maxInFlight.Load(), int32(maxInFlightQuotaProbesPerOwner))
}

func TestUnifiedQuota_ZeroExternalCallsOnRead_AndUnsupportedCache(t *testing.T) {
	var hits atomic.Int32
	rt := channelKeyHealthCheckRoundTripper(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return nil, errors.New("network should not be called on read")
	})
	client, chSvc, _, ctx := newQuotaTestService(t, rt)

	ch, err := client.Channel.Create().
		SetName("read-only-check").
		SetType(channel.TypeOpenai).
		SetBaseURL("https://api.openai.com/v1").
		SetSupportedModels([]string{"gpt-4o"}).
		SetDefaultTestModel("gpt-4o").
		SetCredentials(objects.ChannelCredentials{APIKeys: []string{"sk-1", "sk-2"}}).
		Save(ctx)
	require.NoError(t, err)

	reqCtx := WithChannelQuotaOverviewBatch(ctx, NewChannelQuotaOverviewBatch())
	chSvc.PreloadChannelQuotaOverviewIntoContext(reqCtx, []*ent.Channel{ch})
	ov := chSvc.ResolveChannelQuotaOverview(reqCtx, ch, nil)
	require.NotNil(t, ov)
	require.Equal(t, 2, ov.Counts.Total)
	require.Equal(t, 2, ov.Counts.Unknown)
	require.Equal(t, int32(0), hits.Load(), "QuotaOverview read must NEVER make external HTTP calls")

	// Unsupported channel returns unsupported cleanly without making upstream HTTP calls.
	writerCtx := contexts.WithUser(ent.NewContext(context.Background(), client), &ent.User{ID: 1, IsOwner: true, Scopes: []string{string(scopes.ScopeWriteChannels)}})
	writerCtx, err = authz.WithPrincipal(writerCtx, authz.Principal{Type: authz.PrincipalTypeUser})
	require.NoError(t, err)
	payload, err := chSvc.RefreshChannelQuota(writerCtx, ch.ID, nil, nil)
	require.NoError(t, err)
	require.Equal(t, RefreshChannelQuotaStatusUnsupported, payload.Status)
	require.Equal(t, int32(0), hits.Load())
}

func TestUnifiedQuota_ProviderQuotaWindowsNormalization(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	reset5h := now.Add(3 * time.Hour)
	reset7d := now.Add(5 * 24 * time.Hour)
	reset30d := now.Add(20 * 24 * time.Hour)

	// 1. Codex rate_limit.{primary_window, secondary_window}
	codexData := map[string]any{
		"plan_type": "plus",
		"rate_limit": map[string]any{
			"primary_window": map[string]any{
				"used_percent":         35.0,
				"reset_at":             reset5h.Unix(),
				"limit_window_seconds": 18000,
			},
			"secondary_window": map[string]any{
				"used_percent":         82.0,
				"reset_at":             reset7d.Unix(),
				"limit_window_seconds": 604800,
			},
		},
	}
	codexWins := extractProviderQuotaWindows(codexData, now)
	require.Len(t, codexWins, 2)
	require.Equal(t, "5h", codexWins[0].Key)
	require.InDelta(t, 0.35, lo.FromPtr(codexWins[0].UsageRatio), 0.001)
	require.Equal(t, "%", codexWins[0].Unit)
	require.Equal(t, "7d", codexWins[1].Key)
	require.InDelta(t, 0.82, lo.FromPtr(codexWins[1].UsageRatio), 0.001)

	// 2. Claude Code windows map ("5h", "7d", "overage")
	claudeData := map[string]any{
		"windows": map[string]any{
			"5h":      map[string]any{"status": "allowed", "utilization": 0.42, "reset": reset5h.Unix()},
			"7d":      map[string]any{"status": "allowed_warning", "utilization": 0.88, "reset": reset7d.Unix()},
			"overage": map[string]any{"status": "", "utilization": 0.0, "reset": int64(0)},
		},
	}
	claudeWins := extractProviderQuotaWindows(claudeData, now)
	require.Len(t, claudeWins, 2)
	require.Equal(t, "5h", claudeWins[0].Key)
	require.InDelta(t, 0.42, lo.FromPtr(claudeWins[0].UsageRatio), 0.001)
	require.Equal(t, "7d", claudeWins[1].Key)
	require.InDelta(t, 0.88, lo.FromPtr(claudeWins[1].UsageRatio), 0.001)

	// 3. OpenCode Go windows map ("rolling", "weekly", "monthly")
	opencodeData := map[string]any{
		"plan_type": "go",
		"windows": map[string]any{
			"rolling": map[string]any{"usage_percent": 60.0, "reset_time": reset5h.Format(time.RFC3339), "status": "available"},
			"weekly":  map[string]any{"usage_percent": 90.0, "reset_time": reset7d.Format(time.RFC3339), "status": "warning"},
			"monthly": map[string]any{"usage_percent": 25.0, "reset_time": reset30d.Format(time.RFC3339), "status": "available"},
		},
	}
	opencodeWins := extractProviderQuotaWindows(opencodeData, now)
	require.Len(t, opencodeWins, 3)
	require.Equal(t, "5h", opencodeWins[0].Key)
	require.Equal(t, "7d", opencodeWins[1].Key)
	require.Equal(t, "30d", opencodeWins[2].Key)

	// 4. Cline windows map ("last5h", "last7d") — official usage_ratio (90%) MUST take precedence over 50/100 cost_units estimate.
	clineData := map[string]any{
		"windows": map[string]any{
			"last5h": map[string]any{
				"usage_ratio":      0.9,
				"usage_percent":    90.0,
				"used_cost_units":  50.0,
				"limit_cost_units": 100.0,
				"next_reset_at":    reset5h.Format(time.RFC3339),
			},
			"last7d": map[string]any{"usage_ratio": 0.95, "usage_percent": 95.0, "next_reset_at": reset7d.Format(time.RFC3339)},
		},
	}
	clineWins := extractProviderQuotaWindows(clineData, now)
	require.Len(t, clineWins, 2)
	require.Equal(t, "5h", clineWins[0].Key)
	require.Equal(t, "%", clineWins[0].Unit)
	require.InDelta(t, 90.0, lo.FromPtr(clineWins[0].Used), 0.001)
	require.InDelta(t, 0.9, lo.FromPtr(clineWins[0].UsageRatio), 0.001)
	require.Equal(t, "7d", clineWins[1].Key)
	require.InDelta(t, 0.95, lo.FromPtr(clineWins[1].UsageRatio), 0.001)

	// 5. NanoGPT windows map ("dailyInputTokens", "weeklyInputTokens") with unix ms resetAt
	nanoData := map[string]any{
		"windows": map[string]any{
			"dailyInputTokens":  map[string]any{"used": 400.0, "remaining": 600.0, "percentUsed": 0.4, "resetAt": reset5h.UnixMilli()},
			"weeklyInputTokens": map[string]any{"used": 8000.0, "remaining": 2000.0, "percentUsed": 0.8, "resetAt": reset7d.UnixMilli()},
		},
	}
	nanoWins := extractProviderQuotaWindows(nanoData, now)
	require.Len(t, nanoWins, 2)
	require.Equal(t, "24h", nanoWins[0].Key)
	require.InDelta(t, 0.4, lo.FromPtr(nanoWins[0].UsageRatio), 0.001)
	require.Equal(t, "7d", nanoWins[1].Key)
	require.InDelta(t, 0.8, lo.FromPtr(nanoWins[1].UsageRatio), 0.001)

	// 6. End-to-end buildProviderQuotaOverview projects Codex plan_type + 2 windows cleanly.
	pqs := &ent.ProviderQuotaStatus{
		Status:      providerquotastatus.StatusWarning,
		QuotaData:   codexData,
		NextResetAt: &reset5h,
		UpdatedAt:   now,
	}
	ov := buildProviderQuotaOverview(&ent.Channel{Type: channel.TypeCodex}, pqs, now)
	require.Equal(t, ChannelQuotaStateWarning, ov.State)
	require.Equal(t, "plus", ov.PlanName)
	require.Len(t, ov.Windows, 2)
	require.Equal(t, "5h", ov.Windows[0].Key)
	require.Equal(t, "7d", ov.Windows[1].Key)
}
