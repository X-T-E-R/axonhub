package xcache

import (
	"fmt"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/eko/gocache/lib/v4/store"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type cyberCacheState struct {
	Blocked bool `json:"blocked"`
}

type userCacheState struct {
	ID int `json:"id"`
}

func TestRedisCacheDomainIsolationAcrossClients(t *testing.T) {
	server := miniredis.RunT(t)
	defer server.Close()

	clientOne := redis.NewClient(&redis.Options{Addr: server.Addr()})
	clientTwo := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer clientOne.Close()
	defer clientTwo.Close()

	const namespace = "task49"
	ctx := t.Context()

	sentinelCache := NewRedisWithDomain[string](clientOne, namespace, "sentinel")
	oidcCache := NewRedisWithDomain[[]byte](clientOne, namespace, "oidc")
	cyberCache := NewRedisWithDomain[cyberCacheState](clientOne, namespace, "cyber")
	userCacheOne := NewRedisWithDomain[userCacheState](clientOne, namespace, "user")
	userCacheTwo := NewRedisWithDomain[userCacheState](clientTwo, namespace, "user")

	require.NoError(t, clientOne.Set(ctx, "legacy-oidc", `"legacy"`, 0).Err())
	require.NoError(t, clientOne.Set(ctx, "unscoped-sentinel", "unscoped", 0).Err())
	require.NoError(t, sentinelCache.Set(ctx, "sentinel", "ready", WithExpiration(0), store.WithTags([]string{"shared"})))
	require.NoError(t, oidcCache.Set(ctx, "oidc", []byte("token"), store.WithTags([]string{"shared"})))
	require.NoError(t, cyberCache.Set(ctx, "cyber", cyberCacheState{Blocked: true}, store.WithTags([]string{"shared"})))
	require.NoError(t, userCacheOne.Set(ctx, "tagged", userCacheState{ID: 1}, store.WithTags([]string{"shared"})))
	require.NoError(t, userCacheOne.Set(ctx, "untagged", userCacheState{ID: 2}))

	// Equal namespaces and domains let independent Redis clients share values.
	shared, err := userCacheTwo.Get(ctx, "untagged")
	require.NoError(t, err)
	require.Equal(t, userCacheState{ID: 2}, shared)

	// The explicit domain prevents reads from the legacy unscoped key.
	_, err = oidcCache.Get(ctx, "legacy-oidc")
	require.Error(t, err)

	require.NoError(t, sentinelCache.Clear(ctx))
	_, err = sentinelCache.Get(ctx, "sentinel")
	require.Error(t, err)
	require.Equal(t, "unscoped", mustGetRawRedisValue(t, clientOne, "unscoped-sentinel"))

	// Clear is limited to the sentinel domain.
	oidcValue, err := oidcCache.Get(ctx, "oidc")
	require.NoError(t, err)
	require.Equal(t, []byte("token"), oidcValue)
	cyberValue, err := cyberCache.Get(ctx, "cyber")
	require.NoError(t, err)
	require.Equal(t, cyberCacheState{Blocked: true}, cyberValue)
	userValue, err := userCacheTwo.Get(ctx, "untagged")
	require.NoError(t, err)
	require.Equal(t, userCacheState{ID: 2}, userValue)

	// Invalidate removes only values carrying the requested tag in this domain.
	require.NoError(t, userCacheTwo.Invalidate(ctx, store.WithInvalidateTags([]string{"shared"})))
	_, err = userCacheOne.Get(ctx, "tagged")
	require.Error(t, err)
	userValue, err = userCacheOne.Get(ctx, "untagged")
	require.NoError(t, err)
	require.Equal(t, userCacheState{ID: 2}, userValue)

	// The same tag in other typed domains remains intact.
	oidcValue, err = oidcCache.Get(ctx, "oidc")
	require.NoError(t, err)
	require.Equal(t, []byte("token"), oidcValue)
	cyberValue, err = cyberCache.Get(ctx, "cyber")
	require.NoError(t, err)
	require.Equal(t, cyberCacheState{Blocked: true}, cyberValue)
}

func mustGetRawRedisValue(t *testing.T, client *redis.Client, key string) string {
	t.Helper()
	value, err := client.Get(t.Context(), key).Result()
	require.NoError(t, err)
	return value
}

func TestRedisCacheClearScansOnlyCurrentDomain(t *testing.T) {
	server := miniredis.RunT(t)
	defer server.Close()

	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()

	ctx := t.Context()
	cache := NewRedisWithDomain[string](client, "task49-many", "user")
	otherDomain := NewRedisWithDomain[string](client, "task49-many", "oidc")

	for index := range 300 {
		require.NoError(t, cache.Set(ctx, fmt.Sprintf("key-%03d", index), "value"))
	}
	require.NoError(t, otherDomain.Set(ctx, "keep", "value"))

	require.NoError(t, cache.Clear(ctx))
	for index := range 300 {
		_, err := cache.Get(ctx, fmt.Sprintf("key-%03d", index))
		require.Error(t, err)
	}
	value, err := otherDomain.Get(ctx, "keep")
	require.NoError(t, err)
	require.Equal(t, "value", value)
}

func TestExplicitLegacySecurityCachePreservesRawKeys(t *testing.T) {
	server := miniredis.RunT(t)
	defer server.Close()

	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()

	ctx := t.Context()
	legacyOIDC := NewRedisWithLegacyPrefixes[string](client, "task49-security", []string{"oidc_"})
	scopedOIDC := NewRedisWithDomain[string](client, "task49-security", "oidc")

	require.NoError(t, client.Set(ctx, "oidc_state:old", `"old"`, 0).Err())
	require.NoError(t, client.Set(ctx, "cyber-session-block:keep", `"keep"`, 0).Err())
	require.NoError(t, client.Set(ctx, "unrelated:keep", `"keep"`, 0).Err())

	value, err := legacyOIDC.Get(ctx, "oidc_state:old")
	require.NoError(t, err)
	require.Equal(t, "old", value)
	_, err = scopedOIDC.Get(ctx, "oidc_state:old")
	require.Error(t, err)

	require.NoError(t, legacyOIDC.Set(ctx, "oidc_pkce:new", "fresh"))
	raw, err := client.Get(ctx, "oidc_pkce:new").Result()
	require.NoError(t, err)
	require.Equal(t, `"fresh"`, raw)

	require.NoError(t, legacyOIDC.Clear(ctx))
	_, err = client.Get(ctx, "oidc_state:old").Result()
	require.Error(t, err)
	_, err = client.Get(ctx, "oidc_pkce:new").Result()
	require.Error(t, err)
	require.Equal(t, `"keep"`, mustGetRawRedisValue(t, client, "cyber-session-block:keep"))
	require.Equal(t, `"keep"`, mustGetRawRedisValue(t, client, "unrelated:keep"))
}
