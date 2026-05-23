package biz

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
)

func testChannelWithKeySelection(keys []string, selection *objects.ChannelKeySelection) *Channel {
	return &Channel{
		Channel: &ent.Channel{
			Name: "test-channel",
			Credentials: objects.ChannelCredentials{
				APIKeys: keys,
			},
			Settings: &objects.ChannelSettings{
				KeySelection: selection,
			},
		},
		cachedEnabledAPIKeys: keys,
	}
}

func TestGetAPIKeyProvider_KeySelectionStrategies(t *testing.T) {
	keys := []string{"key-1", "key-2", "key-3"}

	tests := []struct {
		name      string
		settings  *objects.ChannelSettings
		wantType  any
		wantFirst string
	}{
		{
			name:     "nil settings defaults to trace sticky",
			settings: nil,
			wantType: &TraceStickyKeyProvider{},
		},
		{
			name: "nil key selection defaults to trace sticky",
			settings: &objects.ChannelSettings{
				KeySelection: nil,
			},
			wantType: &TraceStickyKeyProvider{},
		},
		{
			name: "explicit trace sticky",
			settings: &objects.ChannelSettings{
				KeySelection: &objects.ChannelKeySelection{Strategy: objects.ChannelKeySelectionStrategyTraceSticky},
			},
			wantType: &TraceStickyKeyProvider{},
		},
		{
			name: "explicit cache affinity",
			settings: &objects.ChannelSettings{
				KeySelection: &objects.ChannelKeySelection{Strategy: objects.ChannelKeySelectionStrategyCacheAffinity},
			},
			wantType: &CacheAffinityKeyProvider{},
		},
		{
			name: "explicit random",
			settings: &objects.ChannelSettings{
				KeySelection: &objects.ChannelKeySelection{Strategy: objects.ChannelKeySelectionStrategyRandom},
			},
			wantType: &RandomChannelKeyProvider{},
		},
		{
			name: "explicit round robin",
			settings: &objects.ChannelSettings{
				KeySelection: &objects.ChannelKeySelection{Strategy: objects.ChannelKeySelectionStrategyRoundRobin},
			},
			wantType:  &RoundRobinChannelKeyProvider{},
			wantFirst: "key-1",
		},
		{
			name: "unknown strategy falls back to trace sticky",
			settings: &objects.ChannelSettings{
				KeySelection: &objects.ChannelKeySelection{Strategy: objects.ChannelKeySelectionStrategy("unknown")},
			},
			wantType: &TraceStickyKeyProvider{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := &Channel{
				Channel: &ent.Channel{
					Name:        "test-channel",
					Credentials: objects.ChannelCredentials{APIKeys: keys},
					Settings:    tt.settings,
				},
				cachedEnabledAPIKeys: keys,
			}

			provider := getAPIKeyProvider(ch)
			require.IsType(t, tt.wantType, provider)
			if tt.wantFirst != "" {
				require.Equal(t, tt.wantFirst, provider.Get(context.Background()))
			}
		})
	}
}

func TestCacheAffinityKeyProvider_StableForAffinityID(t *testing.T) {
	keys := []string{"key-1", "key-2", "key-3"}
	provider := NewCacheAffinityKeyProvider(testChannelWithKeySelection(keys, nil))

	ctx := contexts.WithChannelKeyAffinityID(context.Background(), "cache:stable-affinity")

	first := provider.Get(ctx)
	require.Contains(t, keys, first)
	for range 20 {
		require.Equal(t, first, provider.Get(ctx))
	}
}

func TestCacheAffinityKeyProvider_UsesEnabledKeySet(t *testing.T) {
	keys := []string{"key-1", "key-2", "key-3"}
	ch := testChannelWithKeySelection(keys, nil)
	provider := NewCacheAffinityKeyProvider(ch)

	ctx := contexts.WithChannelKeyAffinityID(context.Background(), "cache:enabled-filter")
	first := provider.Get(ctx)
	require.Contains(t, keys, first)

	ch.cachedEnabledAPIKeys = []string{"key-1"}

	require.Equal(t, "key-1", provider.Get(ctx))
}

func TestRoundRobinChannelKeyProvider_Sequence(t *testing.T) {
	keys := []string{"key-1", "key-2", "key-3"}
	provider := NewRoundRobinChannelKeyProvider(testChannelWithKeySelection(keys, nil))

	require.Equal(t, "key-1", provider.Get(context.Background()))
	require.Equal(t, "key-2", provider.Get(context.Background()))
	require.Equal(t, "key-3", provider.Get(context.Background()))
	require.Equal(t, "key-1", provider.Get(context.Background()))
}

func TestRoundRobinChannelKeyProvider_ConcurrentSelectionStaysInEnabledSet(t *testing.T) {
	keys := []string{"key-1", "key-2", "key-3"}
	provider := NewRoundRobinChannelKeyProvider(testChannelWithKeySelection(keys, nil))

	const calls = 120
	results := make(chan string, calls)
	var wg sync.WaitGroup
	wg.Add(calls)
	for range calls {
		go func() {
			defer wg.Done()
			results <- provider.Get(context.Background())
		}()
	}
	wg.Wait()
	close(results)

	counts := map[string]int{}
	for key := range results {
		require.Contains(t, keys, key)
		counts[key]++
	}
	require.Len(t, counts, len(keys))
	for _, key := range keys {
		require.Equal(t, calls/len(keys), counts[key])
	}
}
