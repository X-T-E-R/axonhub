package biz

import (
	"context"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
)

func TestChannelService_UpsertChannelCredentialPolicy_RoundtripPreservesKeyHealthState(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer svc.Stop()
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	proxySvc := NewOutboundProxyService(OutboundProxyServiceParams{Ent: client})

	backupProxy, err := proxySvc.Create(ctx, OutboundProxyCreateInput{
		Name:     "policy-backup-proxy",
		Kind:     OutboundProxyKindStatic,
		ProxyURL: "http://proxy-user:proxy-pass@backup.example.com:8080",
		PoolName: "pool-backup",
		Status:   OutboundProxyStatusEnabled,
	})
	require.NoError(t, err)

	primaryProxy, err := proxySvc.Create(ctx, OutboundProxyCreateInput{
		Name:           "policy-primary-proxy",
		Kind:           OutboundProxyKindStatic,
		ProxyURL:       "http://proxy-user:proxy-pass@primary.example.com:8080",
		PoolName:       "pool-alpha",
		Status:         OutboundProxyStatusEnabled,
		MaxCredentials: lo.ToPtr(2),
		BackupProxyID:  lo.ToPtr(backupProxy.ID),
	})
	require.NoError(t, err)

	_, err = client.OutboundProxy.UpdateOneID(primaryProxy.ID).
		SetLastExitIP("203.0.113.42").
		Save(ctx)
	require.NoError(t, err)

	key1 := "sk-policy-roundtrip-1"
	key2 := "sk-policy-roundtrip-2"
	fp1 := objects.ChannelAPIKeyFingerprint(key1)
	fp2 := objects.ChannelAPIKeyFingerprint(key2)

	checkedAt := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	resetAt1 := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	resetAt2 := time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC)

	initialSettings := &objects.ChannelSettings{
		KeyHealthCheck: &objects.ChannelKeyHealthCheck{
			KeyMetadata: []objects.ChannelKeyMetadata{
				{
					ID:            fp1,
					LastCheckedAt: lo.ToPtr(checkedAt),
					Success:       lo.ToPtr(true),
					StatusCode:    200,
					Reason:        "ok",
					Balance:       75.0,
					Currency:      "USD",
					BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
						Used:       lo.ToPtr(25.0),
						Limit:      lo.ToPtr(100.0),
						Remaining:  lo.ToPtr(75.0),
						UsageRatio: lo.ToPtr(0.25),
						ResetAt:    lo.ToPtr(resetAt1),
						CheckedAt:  checkedAt,
						Success:    true,
						Available:  lo.ToPtr(true),
					},
					History: []objects.ChannelKeyHealthCheckHistoryEntry{
						{
							CheckedAt:  checkedAt,
							Success:    true,
							StatusCode: 200,
							Reason:     "initial-probe",
						},
					},
				},
				{
					ID:            fp2,
					LastCheckedAt: lo.ToPtr(checkedAt),
					Success:       lo.ToPtr(true),
					StatusCode:    200,
					Reason:        "ok",
					BalanceSnapshot: &objects.ChannelKeyBalanceSnapshot{
						Remaining: lo.ToPtr(60.0),
						ResetAt:   lo.ToPtr(resetAt2),
						CheckedAt: checkedAt,
						Success:   true,
						Available: lo.ToPtr(true),
					},
				},
			},
			ArchivedKeys: []objects.ChannelArchivedAPIKey{
				{
					ID:        objects.ChannelAPIKeyFingerprint("sk-archived-old"),
					MaskedKey: "sk-ar...old",
					Reason:    "manual-archive",
				},
			},
			History: []objects.ChannelKeyHealthCheckHistoryEntry{
				{
					CheckedAt:  checkedAt,
					Success:    true,
					StatusCode: 200,
					Reason:     "channel-level-check",
				},
			},
		},
	}

	ch, err := svc.CreateChannel(ctx, ent.CreateChannelInput{
		Type:             channel.TypeOpenai,
		Name:             "Policy Roundtrip Channel",
		BaseURL:          lo.ToPtr("https://api.openai.com/v1"),
		Credentials:      objects.ChannelCredentials{APIKeys: []string{key1, key2}},
		SupportedModels:  []string{"gpt-4o"},
		DefaultTestModel: "gpt-4o",
	})
	require.NoError(t, err)

	// Persist runtime key health state (metadata, snapshots, archive, history) on the channel.
	_, err = client.Channel.UpdateOneID(ch.ID).
		SetSettings(initialSettings).
		Save(ctx)
	require.NoError(t, err)

	// Upsert first policy (create)
	item1, err := svc.UpsertChannelCredentialPolicy(ctx, ch.ID, objects.ChannelCredentialPolicy{
		ID:                fp1,
		Name:              "primary-pinned",
		Priority:          20,
		Weight:            3,
		MaxConcurrent:     lo.ToPtr(4),
		RequestIntervalMs: lo.ToPtr(150),
		ManualQuotaLimit:  lo.ToPtr(200.0),
		EgressMode:        objects.CredentialEgressModePinnedProxy,
		OutboundProxyID:   lo.ToPtr(primaryProxy.ID),
		EgressFailPolicy:  objects.CredentialEgressFailPolicyBackupProxy,
	})
	require.NoError(t, err)
	require.NotNil(t, item1)
	require.Equal(t, fp1, item1.ID)
	require.NotNil(t, item1.Policy)
	require.Equal(t, "primary-pinned", item1.Policy.Name)
	require.Equal(t, 20, item1.Policy.Priority)
	require.Equal(t, 3, item1.Policy.Weight)
	require.Equal(t, lo.ToPtr(4), item1.Policy.MaxConcurrent)
	require.Equal(t, lo.ToPtr(150), item1.Policy.RequestIntervalMs)
	require.Equal(t, lo.ToPtr(200.0), item1.Policy.ManualQuotaLimit)
	require.Equal(t, objects.CredentialEgressModePinnedProxy, item1.Policy.EgressMode)
	require.Equal(t, lo.ToPtr(primaryProxy.ID), item1.Policy.OutboundProxyID)
	require.Equal(t, objects.CredentialEgressFailPolicyBackupProxy, item1.Policy.EgressFailPolicy)
	require.Equal(t, "policy-primary-proxy", item1.EffectiveProxyName)
	require.Equal(t, "203.0.113.42", item1.EffectiveExitIP)
	require.NotNil(t, item1.QuotaRemainingRatio)
	require.InDelta(t, 0.75, *item1.QuotaRemainingRatio, 1e-9)
	require.NotNil(t, item1.QuotaResetAt)
	require.True(t, item1.QuotaResetAt.Equal(resetAt1))
	require.NotNil(t, item1.BalanceSnapshot)
	require.InDelta(t, 25.0, *item1.BalanceSnapshot.Used, 1e-9)
	require.InDelta(t, 100.0, *item1.BalanceSnapshot.Limit, 1e-9)
	require.InDelta(t, 75.0, *item1.BalanceSnapshot.Remaining, 1e-9)

	// Upsert fp1 in-place (update) and fp2 (append) with pool egress + manual quota limit fallback.
	item1Updated, err := svc.UpsertChannelCredentialPolicy(ctx, ch.ID, objects.ChannelCredentialPolicy{
		ID:               fp1,
		Name:             "primary-pinned-v2",
		Priority:         35,
		Weight:           5,
		EgressMode:       objects.CredentialEgressModePinnedProxy,
		OutboundProxyID:  lo.ToPtr(primaryProxy.ID),
		EgressFailPolicy: objects.CredentialEgressFailPolicyFailClosed,
	})
	require.NoError(t, err)
	require.NotNil(t, item1Updated.Policy)
	require.Equal(t, "primary-pinned-v2", item1Updated.Policy.Name)
	require.Equal(t, 35, item1Updated.Policy.Priority)
	require.Equal(t, 5, item1Updated.Policy.Weight)
	require.Equal(t, objects.CredentialEgressFailPolicyFailClosed, item1Updated.Policy.EgressFailPolicy)

	item2, err := svc.UpsertChannelCredentialPolicy(ctx, ch.ID, objects.ChannelCredentialPolicy{
		ID:               fp2,
		Name:             "pool-secondary",
		Priority:         10,
		Weight:           2,
		ManualQuotaLimit: lo.ToPtr(120.0),
		EgressMode:       objects.CredentialEgressModeProxyPool,
		ProxyPoolName:    "pool-alpha",
		EgressFailPolicy: objects.CredentialEgressFailPolicyPoolFallback,
	})
	require.NoError(t, err)
	require.NotNil(t, item2.Policy)
	require.Equal(t, objects.CredentialEgressModeProxyPool, item2.Policy.EgressMode)
	require.Equal(t, "pool-alpha", item2.Policy.ProxyPoolName)
	require.Equal(t, "policy-primary-proxy", item2.EffectiveProxyName)
	require.NotNil(t, item2.QuotaRemainingRatio)
	require.InDelta(t, 0.5, *item2.QuotaRemainingRatio, 1e-9)
	require.NotNil(t, item2.QuotaResetAt)
	require.True(t, item2.QuotaResetAt.Equal(resetAt2))

	reloaded, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.NotNil(t, reloaded.Settings)
	require.Len(t, reloaded.Settings.CredentialPolicies, 2)
	require.NotNil(t, reloaded.Settings.KeyHealthCheck)
	require.Len(t, reloaded.Settings.KeyHealthCheck.KeyMetadata, 2)
	require.Len(t, reloaded.Settings.KeyHealthCheck.KeyMetadata[0].History, 1)
	require.Equal(t, "initial-probe", reloaded.Settings.KeyHealthCheck.KeyMetadata[0].History[0].Reason)
	require.NotNil(t, reloaded.Settings.KeyHealthCheck.KeyMetadata[0].BalanceSnapshot)
	require.InDelta(t, 75.0, *reloaded.Settings.KeyHealthCheck.KeyMetadata[0].BalanceSnapshot.Remaining, 1e-9)
	require.Len(t, reloaded.Settings.KeyHealthCheck.ArchivedKeys, 1)
	require.Equal(t, "manual-archive", reloaded.Settings.KeyHealthCheck.ArchivedKeys[0].Reason)
	require.Len(t, reloaded.Settings.KeyHealthCheck.History, 1)
	require.Equal(t, "channel-level-check", reloaded.Settings.KeyHealthCheck.History[0].Reason)
}

func TestChannelService_UpdateChannel_CredentialPoliciesOmittedVsExplicitEmpty(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer svc.Stop()
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	key1 := "sk-preserve-1"
	key2 := "sk-preserve-2"
	fp1 := objects.ChannelAPIKeyFingerprint(key1)
	checkedAt := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	initialSettings := &objects.ChannelSettings{
		CredentialPolicies: []objects.ChannelCredentialPolicy{
			{
				ID:               fp1,
				Name:             "kept-policy",
				Priority:         15,
				Weight:           4,
				EgressMode:       objects.CredentialEgressModeDirect,
				EgressFailPolicy: objects.CredentialEgressFailPolicyFailClosed,
			},
		},
		KeyHealthCheck: &objects.ChannelKeyHealthCheck{
			KeyMetadata: []objects.ChannelKeyMetadata{
				{
					ID:            fp1,
					LastCheckedAt: lo.ToPtr(checkedAt),
					Success:       lo.ToPtr(true),
					Reason:        "healthy",
				},
			},
			History: []objects.ChannelKeyHealthCheckHistoryEntry{
				{
					CheckedAt:  checkedAt,
					Success:    true,
					StatusCode: 200,
					Reason:     "probe-history",
				},
			},
		},
	}

	ch, err := svc.CreateChannel(ctx, ent.CreateChannelInput{
		Type:             channel.TypeOpenai,
		Name:             "Preserve vs Clear Policies",
		BaseURL:          lo.ToPtr("https://api.openai.com/v1"),
		Credentials:      objects.ChannelCredentials{APIKeys: []string{key1, key2}},
		SupportedModels:  []string{"gpt-4o"},
		DefaultTestModel: "gpt-4o",
	})
	require.NoError(t, err)

	_, err = client.Channel.UpdateOneID(ch.ID).
		SetSettings(initialSettings).
		Save(ctx)
	require.NoError(t, err)

	// Omitted (nil) CredentialPolicies on Settings update must preserve existing CredentialPolicies and KeyHealthCheck state.
	updatedOmitted, err := svc.UpdateChannel(ctx, ch.ID, &ent.UpdateChannelInput{
		Settings: &objects.ChannelSettings{
			ExtraModelPrefix: "team/",
		},
	})
	require.NoError(t, err)
	require.NotNil(t, updatedOmitted.Settings)
	require.Equal(t, "team/", updatedOmitted.Settings.ExtraModelPrefix)
	require.Len(t, updatedOmitted.Settings.CredentialPolicies, 1)
	require.Equal(t, "kept-policy", updatedOmitted.Settings.CredentialPolicies[0].Name)
	require.Equal(t, 15, updatedOmitted.Settings.CredentialPolicies[0].Priority)
	require.Equal(t, 4, updatedOmitted.Settings.CredentialPolicies[0].Weight)
	require.NotNil(t, updatedOmitted.Settings.KeyHealthCheck)
	require.Len(t, updatedOmitted.Settings.KeyHealthCheck.KeyMetadata, 1)
	require.Equal(t, "healthy", updatedOmitted.Settings.KeyHealthCheck.KeyMetadata[0].Reason)
	require.Len(t, updatedOmitted.Settings.KeyHealthCheck.History, 1)

	// Explicit empty non-nil slice clears CredentialPolicies while preserving KeyHealthCheck state.
	updatedCleared, err := svc.UpdateChannel(ctx, ch.ID, &ent.UpdateChannelInput{
		Settings: &objects.ChannelSettings{
			ExtraModelPrefix:   "team/",
			CredentialPolicies: []objects.ChannelCredentialPolicy{},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, updatedCleared.Settings)
	require.Empty(t, updatedCleared.Settings.CredentialPolicies)
	require.NotNil(t, updatedCleared.Settings.KeyHealthCheck)
	require.Len(t, updatedCleared.Settings.KeyHealthCheck.KeyMetadata, 1)
	require.Equal(t, "healthy", updatedCleared.Settings.KeyHealthCheck.KeyMetadata[0].Reason)
	require.Len(t, updatedCleared.Settings.KeyHealthCheck.History, 1)

	reloaded, err := client.Channel.Get(ctx, ch.ID)
	require.NoError(t, err)
	require.Empty(t, reloaded.Settings.CredentialPolicies)
	require.Len(t, reloaded.Settings.KeyHealthCheck.KeyMetadata, 1)
}

func TestChannelService_ValidateMaxCredentials_OnBindingAndAcrossChannels(t *testing.T) {
	svc, client := setupTestChannelService(t)
	defer svc.Stop()
	defer client.Close()

	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	proxySvc := NewOutboundProxyService(OutboundProxyServiceParams{Ent: client})

	proxyCap2, err := proxySvc.Create(ctx, OutboundProxyCreateInput{
		Name:           "cap-2-proxy",
		Kind:           OutboundProxyKindStatic,
		ProxyURL:       "http://cap.example.com:8080",
		Status:         OutboundProxyStatusEnabled,
		MaxCredentials: lo.ToPtr(2),
	})
	require.NoError(t, err)

	proxyCap1, err := proxySvc.Create(ctx, OutboundProxyCreateInput{
		Name:           "cap-1-proxy",
		Kind:           OutboundProxyKindStatic,
		ProxyURL:       "http://cap1.example.com:8080",
		Status:         OutboundProxyStatusEnabled,
		MaxCredentials: lo.ToPtr(1),
	})
	require.NoError(t, err)

	// Single-channel create exceeding max_credentials=1 fails.
	_, err = svc.CreateChannel(ctx, ent.CreateChannelInput{
		Type:             channel.TypeOpenai,
		Name:             "Single Channel Exceeds Cap",
		BaseURL:          lo.ToPtr("https://api.openai.com/v1"),
		Credentials:      objects.ChannelCredentials{APIKeys: []string{"sk-single-1", "sk-single-2"}},
		SupportedModels:  []string{"gpt-4o"},
		DefaultTestModel: "gpt-4o",
		Settings: &objects.ChannelSettings{
			CredentialPolicies: []objects.ChannelCredentialPolicy{
				{
					ID:              objects.ChannelAPIKeyFingerprint("sk-single-1"),
					EgressMode:      objects.CredentialEgressModePinnedProxy,
					OutboundProxyID: lo.ToPtr(proxyCap1.ID),
				},
				{
					ID:              objects.ChannelAPIKeyFingerprint("sk-single-2"),
					EgressMode:      objects.CredentialEgressModePinnedProxy,
					OutboundProxyID: lo.ToPtr(proxyCap1.ID),
				},
			},
		},
	})
	require.ErrorContains(t, err, "allows at most 1 credentials")

	// Channel A binds 1st credential to proxyCap2 -> total = 1/2.
	chA, err := svc.CreateChannel(ctx, ent.CreateChannelInput{
		Type:             channel.TypeOpenai,
		Name:             "Cross Channel A",
		BaseURL:          lo.ToPtr("https://api.openai.com/v1"),
		Credentials:      objects.ChannelCredentials{APIKeys: []string{"sk-a1"}},
		SupportedModels:  []string{"gpt-4o"},
		DefaultTestModel: "gpt-4o",
		Settings: &objects.ChannelSettings{
			CredentialPolicies: []objects.ChannelCredentialPolicy{
				{
					ID:              objects.ChannelAPIKeyFingerprint("sk-a1"),
					EgressMode:      objects.CredentialEgressModePinnedProxy,
					OutboundProxyID: lo.ToPtr(proxyCap2.ID),
				},
			},
		},
	})
	require.NoError(t, err)

	// Channel B binds 2nd credential to proxyCap2 -> total across A+B = 2/2.
	chB, err := svc.CreateChannel(ctx, ent.CreateChannelInput{
		Type:             channel.TypeOpenai,
		Name:             "Cross Channel B",
		BaseURL:          lo.ToPtr("https://api.openai.com/v1"),
		Credentials:      objects.ChannelCredentials{APIKeys: []string{"sk-b1", "sk-b2"}},
		SupportedModels:  []string{"gpt-4o"},
		DefaultTestModel: "gpt-4o",
	})
	require.NoError(t, err)

	_, err = svc.UpsertChannelCredentialPolicy(ctx, chB.ID, objects.ChannelCredentialPolicy{
		ID:              objects.ChannelAPIKeyFingerprint("sk-b1"),
		Name:            "b1-pinned",
		EgressMode:      objects.CredentialEgressModePinnedProxy,
		OutboundProxyID: lo.ToPtr(proxyCap2.ID),
	})
	require.NoError(t, err)

	// Re-updating Channel B's existing single binding must not double-count chB against itself.
	itemB1, err := svc.UpsertChannelCredentialPolicy(ctx, chB.ID, objects.ChannelCredentialPolicy{
		ID:              objects.ChannelAPIKeyFingerprint("sk-b1"),
		Name:            "b1-pinned-updated",
		Weight:          2,
		EgressMode:      objects.CredentialEgressModePinnedProxy,
		OutboundProxyID: lo.ToPtr(proxyCap2.ID),
	})
	require.NoError(t, err)
	require.Equal(t, "b1-pinned-updated", itemB1.Policy.Name)

	// Binding a 2nd key on Channel B pushes total to 3/2 -> rejected.
	_, err = svc.UpsertChannelCredentialPolicy(ctx, chB.ID, objects.ChannelCredentialPolicy{
		ID:              objects.ChannelAPIKeyFingerprint("sk-b2"),
		EgressMode:      objects.CredentialEgressModePinnedProxy,
		OutboundProxyID: lo.ToPtr(proxyCap2.ID),
	})
	require.ErrorContains(t, err, "allows at most 2 credentials")

	// Creating Channel C bound to proxyCap2 also sees 2 existing credentials across A+B -> rejected.
	_, err = svc.CreateChannel(ctx, ent.CreateChannelInput{
		Type:             channel.TypeOpenai,
		Name:             "Cross Channel C",
		BaseURL:          lo.ToPtr("https://api.openai.com/v1"),
		Credentials:      objects.ChannelCredentials{APIKeys: []string{"sk-c1"}},
		SupportedModels:  []string{"gpt-4o"},
		DefaultTestModel: "gpt-4o",
		Settings: &objects.ChannelSettings{
			CredentialPolicies: []objects.ChannelCredentialPolicy{
				{
					ID:              objects.ChannelAPIKeyFingerprint("sk-c1"),
					EgressMode:      objects.CredentialEgressModePinnedProxy,
					OutboundProxyID: lo.ToPtr(proxyCap2.ID),
				},
			},
		},
	})
	require.ErrorContains(t, err, "allows at most 2 credentials")

	// Clearing Channel A's binding frees one slot so Channel B can now bind sk-b2.
	_, err = svc.UpdateChannel(ctx, chA.ID, &ent.UpdateChannelInput{
		Settings: &objects.ChannelSettings{
			CredentialPolicies: []objects.ChannelCredentialPolicy{},
		},
	})
	require.NoError(t, err)

	_, err = svc.UpsertChannelCredentialPolicy(ctx, chB.ID, objects.ChannelCredentialPolicy{
		ID:              objects.ChannelAPIKeyFingerprint("sk-b2"),
		EgressMode:      objects.CredentialEgressModePinnedProxy,
		OutboundProxyID: lo.ToPtr(proxyCap2.ID),
	})
	require.NoError(t, err)
}
