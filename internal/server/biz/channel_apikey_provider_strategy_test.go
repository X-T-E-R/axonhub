package biz

import (
	"context"
	"sync"
	"testing"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/samber/lo"
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

	ctx := contexts.WithChannelKeyAffinityID(context.Background(), "cache:exact:stable-affinity")

	first := provider.Get(ctx)
	require.Contains(t, keys, first)
	for range 20 {
		require.Equal(t, first, provider.Get(ctx))
	}
}

func TestCacheAffinityKeyProvider_UsesDefaultAffinityTTLs(t *testing.T) {
	provider := NewCacheAffinityKeyProvider(testChannelWithKeySelection(
		[]string{"key-1", "key-2"},
		&objects.ChannelKeySelection{Strategy: objects.ChannelKeySelectionStrategyCacheAffinity},
	))

	require.Equal(t, time.Duration(objects.DefaultChannelKeyLikelyAffinityTTLMinutes)*time.Minute, provider.likelyTTL)
	require.Equal(t, time.Duration(objects.DefaultChannelKeyExactAffinityTTLMinutes)*time.Minute, provider.exactTTL)
}

func TestValidateChannelKeySelection_AffinityTTLs(t *testing.T) {
	require.NoError(t, ValidateChannelKeySelection(&objects.ChannelKeySelection{
		Strategy: objects.ChannelKeySelectionStrategyCacheAffinity,
	}))
	require.NoError(t, ValidateChannelKeySelection(&objects.ChannelKeySelection{
		Strategy:                 objects.ChannelKeySelectionStrategyCacheAffinity,
		LikelyAffinityTTLMinutes: lo.ToPtr(1),
		ExactAffinityTTLMinutes:  lo.ToPtr(10080),
	}))

	err := ValidateChannelKeySelection(&objects.ChannelKeySelection{
		Strategy:                 objects.ChannelKeySelectionStrategyCacheAffinity,
		LikelyAffinityTTLMinutes: lo.ToPtr(0),
	})
	require.ErrorContains(t, err, "likely affinity TTL minutes")

	err = ValidateChannelKeySelection(&objects.ChannelKeySelection{
		Strategy:                objects.ChannelKeySelectionStrategyCacheAffinity,
		ExactAffinityTTLMinutes: lo.ToPtr(10081),
	})
	require.ErrorContains(t, err, "exact affinity TTL minutes")
}

func TestCacheAffinityKeyProvider_UsesCustomAffinityTTLsByTier(t *testing.T) {
	keys := []string{"key-1", "key-2", "key-3"}
	provider := NewCacheAffinityKeyProvider(testChannelWithKeySelection(keys, &objects.ChannelKeySelection{
		Strategy:                 objects.ChannelKeySelectionStrategyCacheAffinity,
		LikelyAffinityTTLMinutes: lo.ToPtr(7),
		ExactAffinityTTLMinutes:  lo.ToPtr(90),
	}))
	now := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)

	likelyCtx := contexts.WithChannelKeyAffinityID(context.Background(), "cache:likely:shared-prefix")
	exactCtx := contexts.WithChannelKeyAffinityID(context.Background(), "cache:exact:prompt-cache-key")

	require.Contains(t, keys, provider.Get(likelyCtx))
	require.Contains(t, keys, provider.Get(exactCtx))

	likelyEntry, ok := provider.cache.Get("cache:likely:shared-prefix")
	require.True(t, ok)
	exactEntry, ok := provider.cache.Get("cache:exact:prompt-cache-key")
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(7*time.Minute), likelyEntry.ExpiresAt, time.Second)
	require.WithinDuration(t, time.Now().Add(90*time.Minute), exactEntry.ExpiresAt, time.Second)

	cache, _ := lru.New[string, stickyCacheEntry](traceStickyLRUSize)
	first := selectTTLStickyKeyAt(cache, keys, "cache:exact:sliding", 90*time.Minute, now)
	require.Contains(t, keys, first)
	entry, ok := cache.Get("cache:exact:sliding")
	require.True(t, ok)
	require.Equal(t, now.Add(90*time.Minute), entry.ExpiresAt)
}

func TestSelectTTLStickyKeyAt_RefreshesExpirationOnHit(t *testing.T) {
	keys := []string{"key-1", "key-2", "key-3"}
	cache, _ := lru.New[string, stickyCacheEntry](traceStickyLRUSize)
	ttl := 30 * time.Minute
	start := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	seed := "cache:likely:sliding-window"

	first := selectTTLStickyKeyAt(cache, keys, seed, ttl, start)
	firstEntry, ok := cache.Get(seed)
	require.True(t, ok)
	require.Equal(t, start.Add(ttl), firstEntry.ExpiresAt)

	second := selectTTLStickyKeyAt(cache, keys, seed, ttl, start.Add(20*time.Minute))
	secondEntry, ok := cache.Get(seed)
	require.True(t, ok)
	require.Equal(t, first, second)
	require.Equal(t, start.Add(50*time.Minute), secondEntry.ExpiresAt)

	third := selectTTLStickyKeyAt(cache, keys, seed, ttl, start.Add(35*time.Minute))
	require.Equal(t, first, third, "second hit should extend mapping past the original expiry")
}

func TestCacheAffinityKeyProvider_NoAffinityUsesRoundRobin(t *testing.T) {
	keys := []string{"key-1", "key-2", "key-3"}
	provider := NewCacheAffinityKeyProvider(testChannelWithKeySelection(keys, nil))

	require.Equal(t, "key-1", provider.Get(context.Background()))
	require.Equal(t, "key-2", provider.Get(context.Background()))
	require.Equal(t, "key-3", provider.Get(context.Background()))
	require.Equal(t, "key-1", provider.Get(context.Background()))
}

func TestCacheAffinityKeyProvider_NoAffinityDoesNotUseTraceStickyFallback(t *testing.T) {
	keys := []string{"key-1", "key-2", "key-3"}
	provider := NewCacheAffinityKeyProvider(testChannelWithKeySelection(keys, nil))
	ctx := contexts.WithTrace(context.Background(), &ent.Trace{TraceID: "same-trace"})

	require.Equal(t, "key-1", provider.Get(ctx))
	require.Equal(t, "key-2", provider.Get(ctx))
	require.Equal(t, "key-3", provider.Get(ctx))
}

func TestCacheAffinityKeyProvider_UsesEnabledKeySet(t *testing.T) {
	keys := []string{"key-1", "key-2", "key-3"}
	ch := testChannelWithKeySelection(keys, nil)
	provider := NewCacheAffinityKeyProvider(ch)

	ctx := contexts.WithChannelKeyAffinityID(context.Background(), "cache:likely:enabled-filter")
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
