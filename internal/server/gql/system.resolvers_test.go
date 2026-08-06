package gql

import (
	"context"
	"testing"

	"github.com/99designs/gqlgen/graphql"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
)

func setupTestSystemMutationResolver(t *testing.T) (*mutationResolver, context.Context, *ent.Client) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	systemService := &biz.SystemService{
		Cache: xcache.NewFromConfig[ent.System](xcache.Config{Mode: xcache.ModeMemory}),
	}

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	resolver := &mutationResolver{&Resolver{systemService: systemService}}
	return resolver, ctx, client
}

func TestMutationResolver_UpdateStoragePolicy_PreservesOmittedFieldsAndAppliesFalse(t *testing.T) {
	resolver, ctx, client := setupTestSystemMutationResolver(t)
	defer client.Close()

	original := &biz.StoragePolicy{
		StoreChunks:                 true,
		LivePreview:                 true,
		StoreRequestBody:            true,
		StoreExecutionRequestBody:   lo.ToPtr(true),
		StoreResponseBody:           true,
		ManagedObservabilityHardMiB: lo.ToPtr(10),
		ManagedObservabilityLowMiB:  lo.ToPtr(8),
		CleanupOptions: []biz.CleanupOption{{
			ResourceType: "requests", Enabled: true, CleanupDays: 30,
		}},
	}
	require.NoError(t, resolver.systemService.SetStoragePolicy(ctx, original))

	ok, err := resolver.UpdateStoragePolicy(ctx, UpdateStoragePolicyInput{
		StoreRequestBody: graphql.OmittableOf(lo.ToPtr(false)),
	})
	require.NoError(t, err)
	require.True(t, ok)

	updated, err := resolver.systemService.StoragePolicy(ctx)
	require.NoError(t, err)
	require.False(t, updated.StoreRequestBody)
	require.True(t, updated.StoreChunks)
	require.True(t, updated.LivePreview)
	require.True(t, updated.StoreResponseBody)
	require.NotNil(t, updated.StoreExecutionRequestBody)
	require.True(t, *updated.StoreExecutionRequestBody)
	require.Equal(t, 10, *updated.ManagedObservabilityHardMiB)
	require.Equal(t, 8, *updated.ManagedObservabilityLowMiB)
	require.Equal(t, original.CleanupOptions, updated.CleanupOptions)
}

func TestMutationResolver_UpdateStoragePolicy_ExplicitNullClearsNullableFields(t *testing.T) {
	resolver, ctx, client := setupTestSystemMutationResolver(t)
	defer client.Close()

	require.NoError(t, resolver.systemService.SetStoragePolicy(ctx, &biz.StoragePolicy{
		StoreRequestBody:            true,
		StoreExecutionRequestBody:   lo.ToPtr(false),
		StoreResponseBody:           true,
		ManagedObservabilityHardMiB: lo.ToPtr(10),
		ManagedObservabilityLowMiB:  lo.ToPtr(8),
	}))

	ok, err := resolver.UpdateStoragePolicy(ctx, UpdateStoragePolicyInput{
		StoreExecutionRequestBody:   graphql.OmittableOf[*bool](nil),
		ManagedObservabilityHardMiB: graphql.OmittableOf[*int](nil),
	})
	require.NoError(t, err)
	require.True(t, ok)

	updated, err := resolver.systemService.StoragePolicy(ctx)
	require.NoError(t, err)
	require.Nil(t, updated.StoreExecutionRequestBody)
	require.Nil(t, updated.ManagedObservabilityHardMiB)
	require.Nil(t, updated.ManagedObservabilityLowMiB)
	require.True(t, updated.StoreRequestBody)
	require.True(t, updated.StoreResponseBody)
}

func TestMutationResolver_UpdateSystemChannelSettings_MergesAutoSyncWithoutOverwritingProbe(t *testing.T) {
	resolver, ctx, client := setupTestSystemMutationResolver(t)
	defer client.Close()

	err := resolver.systemService.SetChannelSetting(ctx, biz.SystemChannelSettings{
		Probe: biz.ChannelProbeSetting{
			Enabled:   true,
			Frequency: biz.ProbeFrequency5Min,
		},
		AutoSync: biz.ChannelModelAutoSyncSetting{
			Frequency: biz.AutoSyncFrequencyOneHour,
		},
	})
	require.NoError(t, err)

	ok, err := resolver.UpdateSystemChannelSettings(ctx, biz.SystemChannelSettings{
		AutoSync: biz.ChannelModelAutoSyncSetting{
			Frequency: biz.AutoSyncFrequencySixHours,
		},
	})
	require.NoError(t, err)
	require.True(t, ok)

	setting, err := resolver.systemService.ChannelSetting(ctx)
	require.NoError(t, err)
	require.True(t, setting.Probe.Enabled)
	require.Equal(t, biz.ProbeFrequency5Min, setting.Probe.Frequency)
	require.Equal(t, biz.AutoSyncFrequencySixHours, setting.AutoSync.Frequency)
}

func TestMutationResolver_UpdateSystemChannelSettings_MergesProbeWithoutOverwritingAutoSync(t *testing.T) {
	resolver, ctx, client := setupTestSystemMutationResolver(t)
	defer client.Close()

	err := resolver.systemService.SetChannelSetting(ctx, biz.SystemChannelSettings{
		Probe: biz.ChannelProbeSetting{
			Enabled:   true,
			Frequency: biz.ProbeFrequency5Min,
		},
		AutoSync: biz.ChannelModelAutoSyncSetting{
			Frequency: biz.AutoSyncFrequencySixHours,
		},
	})
	require.NoError(t, err)

	ok, err := resolver.UpdateSystemChannelSettings(ctx, biz.SystemChannelSettings{
		Probe: biz.ChannelProbeSetting{
			Enabled:   false,
			Frequency: biz.ProbeFrequency1Hour,
		},
	})
	require.NoError(t, err)
	require.True(t, ok)

	setting, err := resolver.systemService.ChannelSetting(ctx)
	require.NoError(t, err)
	require.False(t, setting.Probe.Enabled)
	require.Equal(t, biz.ProbeFrequency1Hour, setting.Probe.Frequency)
	require.Equal(t, biz.AutoSyncFrequencySixHours, setting.AutoSync.Frequency)
}

func TestMutationResolver_UpdateSystemChannelSettings_MergesActionMenuWithoutOverwritingProbeOrAutoSync(t *testing.T) {
	resolver, ctx, client := setupTestSystemMutationResolver(t)
	defer client.Close()

	err := resolver.systemService.SetChannelSetting(ctx, biz.SystemChannelSettings{
		Probe: biz.ChannelProbeSetting{
			Enabled:   true,
			Frequency: biz.ProbeFrequency5Min,
		},
		AutoSync: biz.ChannelModelAutoSyncSetting{
			Frequency: biz.AutoSyncFrequencySixHours,
		},
		ActionMenu: biz.ChannelActionMenuSetting{
			AdvancedActionsMode: biz.ChannelAdvancedActionMenuModeGrouped,
		},
	})
	require.NoError(t, err)

	ok, err := resolver.UpdateSystemChannelSettings(ctx, biz.SystemChannelSettings{
		ActionMenu: biz.ChannelActionMenuSetting{
			AdvancedActionsMode: biz.ChannelAdvancedActionMenuModeExpanded,
		},
	})
	require.NoError(t, err)
	require.True(t, ok)

	setting, err := resolver.systemService.ChannelSetting(ctx)
	require.NoError(t, err)
	require.True(t, setting.Probe.Enabled)
	require.Equal(t, biz.ProbeFrequency5Min, setting.Probe.Frequency)
	require.Equal(t, biz.AutoSyncFrequencySixHours, setting.AutoSync.Frequency)
	require.Equal(t, biz.ChannelAdvancedActionMenuModeExpanded, setting.ActionMenu.AdvancedActionsMode)
}

func TestMutationResolver_UpdateSystemChannelSettings_MergesRoutingWithoutOverwritingOtherSections(t *testing.T) {
	resolver, ctx, client := setupTestSystemMutationResolver(t)
	defer client.Close()

	err := resolver.systemService.SetChannelSetting(ctx, biz.SystemChannelSettings{
		Probe: biz.ChannelProbeSetting{
			Enabled:   true,
			Frequency: biz.ProbeFrequency5Min,
		},
		AutoSync: biz.ChannelModelAutoSyncSetting{
			Frequency: biz.AutoSyncFrequencySixHours,
		},
		ActionMenu: biz.ChannelActionMenuSetting{
			AdvancedActionsMode: biz.ChannelAdvancedActionMenuModeExpanded,
		},
		Routing: biz.ChannelKeyRoutingSetting{
			Strategy: objects.ChannelKeySelectionStrategyTraceSticky,
		},
	})
	require.NoError(t, err)

	ok, err := resolver.UpdateSystemChannelSettings(ctx, biz.SystemChannelSettings{
		Routing: biz.ChannelKeyRoutingSetting{
			Strategy: objects.ChannelKeySelectionStrategyRoundRobin,
		},
	})
	require.NoError(t, err)
	require.True(t, ok)

	setting, err := resolver.systemService.ChannelSetting(ctx)
	require.NoError(t, err)
	require.True(t, setting.Probe.Enabled)
	require.Equal(t, biz.ProbeFrequency5Min, setting.Probe.Frequency)
	require.Equal(t, biz.AutoSyncFrequencySixHours, setting.AutoSync.Frequency)
	require.Equal(t, biz.ChannelAdvancedActionMenuModeExpanded, setting.ActionMenu.AdvancedActionsMode)
	require.Equal(t, objects.ChannelKeySelectionStrategyRoundRobin, setting.Routing.Strategy)
}
