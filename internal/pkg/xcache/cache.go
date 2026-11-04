package xcache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/eko/gocache/lib/v4/store"

	cachelib "github.com/eko/gocache/lib/v4/cache"
	gocache_store "github.com/eko/gocache/store/go_cache/v4"
	gocache "github.com/patrickmn/go-cache"
	redis "github.com/redis/go-redis/v9"

	"github.com/looplj/axonhub/internal/log"
	redis_store "github.com/looplj/axonhub/internal/pkg/xcache/redis"
	"github.com/looplj/axonhub/internal/pkg/xredis"
)

// DefaultNamespace is the shared Redis key namespace used when none is configured.
const DefaultNamespace = redis_store.DefaultNamespace

// Cache is an alias to the gocache CacheInterface for convenience.
// It allows you to depend on xcache while still exposing the common methods:
//   - Get(ctx, key) (T, error)
//   - Set(ctx, key, value, options ...Option) error
//   - Delete(ctx, key) error
//   - Invalidate(ctx, options ...store.InvalidateOption) error
//   - Clear(ctx) error
//   - GetType() string
// For setter caches (memory/redis/chain), GetWithTTL and GetCodec are also available on the returned value.
// See: github.com/eko/gocache/lib/v4/cache
//
// Usage example:
//   mem := xcache.NewMemory[string](gocache.New(5*time.Minute, 10*time.Minute))
//   rdb := redis.NewUniversalClient(&redis.UniversalOptions{Addrs: []string{"127.0.0.1:6379"}})
//   rds := xcache.NewRedis[string](rdb, store.WithExpiration(15*time.Second))
//   l2 := xcache.NewTwoLevel[string](mem, rds)
//
//   _ = mem.Set(ctx, "foo", "bar")
//   val, _ := l2.Get(ctx, "foo")

type Cache[T any] = cachelib.CacheInterface[T]

type SetterCache[T any] = cachelib.SetterCacheInterface[T]

// NewMemory creates a pure in-memory cache using patrickmn/go-cache as the backend.
// Pass an existing *gocache.Cache so you control default expiration & cleanup interval.
// Optionally pass store options (e.g., store.WithExpiration) when setting values.
func NewMemory[T any](client *gocache.Cache, options ...Option) SetterCache[T] {
	store := gocache_store.NewGoCache(client, options...)
	return cachelib.New[T](store)
}

// NewMemoryWithOptions is a convenience constructor that builds the patrickmn/go-cache client
// for you using the provided default expiration and cleanup interval.
func NewMemoryWithOptions[T any](defaultExpiration, cleanupInterval time.Duration, options ...Option) SetterCache[T] {
	client := gocache.New(defaultExpiration, cleanupInterval)
	return NewMemory[T](client, options...)
}

// NewFromConfig builds a typed cache from the given Config.
// Modes:
//   - memory: in-memory only
//   - redis: redis only
//   - two-level: memory + redis chain
//
// Memory and Redis expiration can be configured separately.
// If mode is not set or invalid, returns a noop cache that does nothing.
// Redis keys use cfg.Namespace (defaulting to axonhub) and a stable typed domain.
func NewFromConfig[T any](cfg Config) Cache[T] {
	return newFromConfig[T](cfg, "", nil)
}

// NewFromConfigWithDomain is the explicit-domain variant for callers whose
// semantic cache identity should remain stable even if its Go type moves.
func NewFromConfigWithDomain[T any](cfg Config, domain string) Cache[T] {
	return newFromConfig[T](cfg, domain, nil)
}

// NewFromConfigWithLegacyPrefixes explicitly keeps pre-namespace security
// state on its raw physical keys. It is not a fallback: callers choosing this
// constructor use only the named legacy key prefixes.
func NewFromConfigWithLegacyPrefixes[T any](cfg Config, prefixes ...string) Cache[T] {
	return newFromConfig[T](cfg, "", prefixes)
}

func newFromConfig[T any](cfg Config, domain string, legacyPrefixes []string) Cache[T] {
	// If mode is not set or empty, return noop cache
	if cfg.Mode == "" {
		return NewNoop[T]()
	}

	// Build memory setter cache with separate expiration settings
	memExpiration := defaultIfZero(cfg.Memory.Expiration, 5*time.Minute)
	memCleanupInterval := defaultIfZero(cfg.Memory.CleanupInterval, 10*time.Minute)

	memClient := gocache.New(memExpiration, memCleanupInterval)
	memStore := gocache_store.NewGoCache(memClient, store.WithExpiration(memExpiration))
	mem := cachelib.New[T](memStore)

	// Build redis setter cache if configured
	var rds SetterCache[T]

	if (cfg.Redis.Addr != "" || len(cfg.Redis.Addrs) != 0 || cfg.Redis.URL != "") && cfg.Mode != ModeMemory {
		client, err := xredis.NewClient(cfg.Redis)
		if err != nil {
			panic(fmt.Errorf("invalid redis config: %w", err))
		}

		redisExpiration := defaultIfZero(cfg.Redis.Expiration, 30*time.Minute) // Default longer for Redis
		var rdsStore *redis_store.RedisStore[T]
		if len(legacyPrefixes) > 0 {
			rdsStore = redis_store.NewRedisStoreWithLegacyPrefixes[T](client, cfg.Namespace, legacyPrefixes, store.WithExpiration(redisExpiration))
		} else {
			rdsStore = redis_store.NewRedisStoreWithNamespace[T](client, cfg.Namespace, domain, store.WithExpiration(redisExpiration))
		}
		rds = cachelib.New[T](rdsStore)
	}

	switch cfg.Mode {
	case ModeTwoLevel:
		if rds != nil {
			log.Info(context.Background(), "Using two-level cache")
			return cachelib.NewChain[T](mem, rds)
		}

		return mem
	case ModeRedis:
		if rds == nil {
			panic(errors.New("redis cache config is invalid"))
		}

		log.Info(context.Background(), "Using redis cache")

		return rds
	case ModeMemory:
		log.Info(context.Background(), "Using memory cache")
		return mem
	default:
		log.Info(context.Background(), "Disable cache")
		// Return noop cache for invalid modes
		return NewNoop[T]()
	}
}

func defaultIfZero(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}

	return d
}

// NewRedis creates a pure Redis cache using github.com/redis/go-redis/v9 as the client.
// You can pass store options like store.WithExpiration, store.WithTags, etc.
func NewRedis[T any](client redis.UniversalClient, options ...Option) SetterCache[T] {
	return NewRedisWithNamespace[T](client, redis_store.DefaultNamespace, options...)
}

// NewRedisWithNamespace creates a Redis cache with an explicit shared namespace
// and the stable typed domain for T.
func NewRedisWithNamespace[T any](client redis.UniversalClient, namespace string, options ...Option) SetterCache[T] {
	return NewRedisWithDomain[T](client, namespace, "", options...)
}

// NewRedisWithDomain creates a Redis cache with explicit namespace and domain.
func NewRedisWithDomain[T any](client redis.UniversalClient, namespace, domain string, options ...Option) SetterCache[T] {
	store := redis_store.NewRedisStoreWithNamespace[T](client, namespace, domain, options...)
	return cachelib.New[T](store)
}

// NewRedisWithLegacyPrefixes explicitly preserves raw physical keys for named
// security-state prefixes. Clear remains limited to those prefixes.
func NewRedisWithLegacyPrefixes[T any](client redis.UniversalClient, namespace string, prefixes []string, options ...Option) SetterCache[T] {
	store := redis_store.NewRedisStoreWithLegacyPrefixes[T](client, namespace, prefixes, options...)
	return cachelib.New[T](store)
}

// NewRedisWithOptions builds a redis.UniversalClient for you and returns the cache.
func NewRedisWithOptions[T any](opts *redis.UniversalOptions, options ...Option) SetterCache[T] {
	client := redis.NewUniversalClient(opts)
	return NewRedis[T](client, options...)
}

// NewTwoLevel constructs a 2-level cache: memory first, then Redis.
// It takes already constructed setter caches so you can mix any memory/redis implementations.
// Typical usage: NewTwoLevel(NewMemory(...), NewRedis(...)).
func NewTwoLevel[T any](memory SetterCache[T], redis SetterCache[T]) Cache[T] {
	return cachelib.NewChain[T](memory, redis)
}

// NewTwoLevelWithClients is a convenience to create a 2-level cache from raw clients directly.
func NewTwoLevelWithClients[T any](memClient *gocache.Cache, redisClient redis.UniversalClient, memOptions []Option, redisOptions []Option) Cache[T] {
	mem := NewMemory[T](memClient, memOptions...)
	rds := NewRedis[T](redisClient, redisOptions...)

	return NewTwoLevel[T](mem, rds)
}
