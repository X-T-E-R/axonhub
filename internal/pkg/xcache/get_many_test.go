package xcache

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

type countingRedisClient struct {
	redis.UniversalClient

	mgetCalls atomic.Int64
}

func (c *countingRedisClient) MGet(ctx context.Context, keys ...string) *redis.SliceCmd {
	c.mgetCalls.Add(1)
	return c.UniversalClient.MGet(ctx, keys...)
}

func TestGetManyUsesBoundedRedisBatches(t *testing.T) {
	server := miniredis.RunT(t)
	client := &countingRedisClient{
		UniversalClient: redis.NewClient(&redis.Options{Addr: server.Addr()}),
	}
	defer client.Close()
	cache := NewRedis[string](client)

	keys := make([]string, 256)
	for index := range keys {
		keys[index] = fmt.Sprintf("key-%03d", index)
	}
	require.NoError(t, cache.Set(t.Context(), keys[0], "first"))
	require.NoError(t, cache.Set(t.Context(), keys[200], "last"))

	values := GetMany[string](t.Context(), cache, keys)
	require.Equal(t, "first", values[keys[0]])
	require.Equal(t, "last", values[keys[200]])
	require.Len(t, values, 2)
	require.EqualValues(t, 2, client.mgetCalls.Load())
}

func TestSetIfAbsentPreservesFirstRedisValue(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	defer client.Close()
	cache := NewRedis[string](client)

	created, err := SetIfAbsent(t.Context(), cache, "first-write", "first", time.Minute)
	require.NoError(t, err)
	require.True(t, created)
	created, err = SetIfAbsent(t.Context(), cache, "first-write", "second", time.Hour)
	require.NoError(t, err)
	require.False(t, created)
	value, err := cache.Get(t.Context(), "first-write")
	require.NoError(t, err)
	require.Equal(t, "first", value)
	require.Equal(t, time.Minute, server.TTL("first-write"))
}
