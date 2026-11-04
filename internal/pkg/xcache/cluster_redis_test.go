package xcache

import (
	"os"
	"testing"

	"github.com/eko/gocache/lib/v4/store"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestClusterRealRedisDomainBoundaries(t *testing.T) {
	url := os.Getenv("AXONHUB_TEST_REDIS_URL")
	if url == "" {
		t.Skip("requires disposable Redis")
	}
	opts, err := redis.ParseURL(url)
	require.NoError(t, err)
	a, b := redis.NewClient(opts), redis.NewClient(opts)
	defer a.Close()
	defer b.Close()
	ctx := t.Context()
	namespace := "task49-" + uuid.NewString()
	usersA := NewRedisWithDomain[string](a, namespace, "users")
	usersB := NewRedisWithDomain[string](b, namespace, "users")
	system := NewRedisWithDomain[string](a, namespace, "system")
	oidcKey, cyberKey := "oidc_state:"+namespace, "cyber-session-block:"+namespace
	require.NoError(t, a.Set(ctx, oidcKey, "synthetic-oidc", 0).Err())
	require.NoError(t, a.Set(ctx, cyberKey, "synthetic-block", 0).Err())
	defer a.Del(ctx, oidcKey, cyberKey)
	defer usersA.Clear(ctx)
	defer system.Clear(ctx)
	require.NoError(t, usersA.Set(ctx, "tagged", "shared-value", store.WithTags([]string{"shared"})))
	require.NoError(t, usersA.Set(ctx, "untagged", "keep"))
	require.NoError(t, system.Set(ctx, "tagged", "system-value", store.WithTags([]string{"shared"})))
	value, err := usersB.Get(ctx, "tagged")
	require.NoError(t, err)
	require.Equal(t, "shared-value", value)
	require.NoError(t, usersB.Invalidate(ctx, store.WithInvalidateTags([]string{"shared"})))
	_, err = usersA.Get(ctx, "tagged")
	require.Error(t, err)
	value, err = usersA.Get(ctx, "untagged")
	require.NoError(t, err)
	require.Equal(t, "keep", value)
	value, err = system.Get(ctx, "tagged")
	require.NoError(t, err)
	require.Equal(t, "system-value", value)
	require.NoError(t, usersA.Clear(ctx))
	value, err = system.Get(ctx, "tagged")
	require.NoError(t, err)
	require.Equal(t, "system-value", value)
	require.Equal(t, "synthetic-oidc", b.Get(ctx, oidcKey).Val())
	require.Equal(t, "synthetic-block", b.Get(ctx, cyberKey).Val())
}
