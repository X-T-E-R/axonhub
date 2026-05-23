package biz

import (
	"context"
	"hash/fnv"
	"math/rand/v2"
	"slices"
	"sync/atomic"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/log"
)

// traceStickyLRUSize is the default LRU cache size for trace-to-key mappings.
const traceStickyLRUSize = 1024
const cacheAffinityTTL = 30 * time.Minute

type stickyCacheEntry struct {
	Key       string
	ExpiresAt time.Time
}

// TraceStickyKeyProvider selects an API key deterministically per traceID (if present),
// using cached enabled keys from the channel snapshot.
//
// An LRU cache remembers previous traceID→key selections so that, as long as
// the previously chosen key is still enabled, the same key is returned even when
// the enabled-key set changes (e.g. a new key is added). This improves sticky
// stability compared to pure rendezvous hashing alone.
//
//nolint:revive // exported for use in transformers via interface.
type TraceStickyKeyProvider struct {
	channel *Channel
	cache   *lru.Cache[string, string]
}

func NewTraceStickyKeyProvider(channel *Channel) *TraceStickyKeyProvider {
	cache, _ := lru.New[string, string](traceStickyLRUSize)

	return &TraceStickyKeyProvider{
		channel: channel,
		cache:   cache,
	}
}

func (p *TraceStickyKeyProvider) Get(ctx context.Context) string {
	enabled := p.channel.cachedEnabledAPIKeys
	if len(enabled) == 0 {
		return recordSelectedChannelAPIKey(ctx, p.channel.Credentials.APIKeys[0])
	}

	if len(enabled) == 1 {
		return recordSelectedChannelAPIKey(ctx, enabled[0])
	}

	var selectedKey string

	if trace, ok := contexts.GetTrace(ctx); ok && trace != nil {
		selectedKey = selectStickyKey(p.cache, enabled, trace.TraceID)

		if log.DebugEnabled(ctx) {
			log.Debug(ctx, "Trace sticky key selected",
				log.String("trace_id", trace.TraceID),
				log.String("key_prefix", safeAPIKeyPrefix(selectedKey)),
			)
		}
	} else {
		//nolint:gosec // not a security issue, just a random selection.
		selectedKey = enabled[rand.IntN(len(enabled))]
		if log.DebugEnabled(ctx) {
			log.Debug(ctx, "Random key selected",
				log.String("key_prefix", safeAPIKeyPrefix(selectedKey)),
			)
		}
	}

	return recordSelectedChannelAPIKey(ctx, selectedKey)
}

// CacheAffinityKeyProvider selects an API key by a request-content-derived,
// non-secret affinity ID. It falls back to trace-sticky/random behavior when no
// affinity has been derived for the current request.
//
//nolint:revive // exported for use in tests and provider factory.
type CacheAffinityKeyProvider struct {
	channel *Channel
	cache   *lru.Cache[string, stickyCacheEntry]
}

func NewCacheAffinityKeyProvider(channel *Channel) *CacheAffinityKeyProvider {
	cache, _ := lru.New[string, stickyCacheEntry](traceStickyLRUSize)

	return &CacheAffinityKeyProvider{
		channel: channel,
		cache:   cache,
	}
}

func (p *CacheAffinityKeyProvider) Get(ctx context.Context) string {
	enabled := p.channel.cachedEnabledAPIKeys
	if len(enabled) == 0 {
		return recordSelectedChannelAPIKey(ctx, p.channel.Credentials.APIKeys[0])
	}

	if len(enabled) == 1 {
		return recordSelectedChannelAPIKey(ctx, enabled[0])
	}

	if affinityID, ok := contexts.GetChannelKeyAffinityID(ctx); ok && affinityID != "" {
		selectedKey := selectTTLStickyKey(p.cache, enabled, affinityID, cacheAffinityTTL)

		if log.DebugEnabled(ctx) {
			log.Debug(ctx, "Cache affinity key selected",
				log.String("affinity_id_prefix", safeAffinityIDPrefix(affinityID)),
				log.String("key_prefix", safeAPIKeyPrefix(selectedKey)),
			)
		}

		return recordSelectedChannelAPIKey(ctx, selectedKey)
	}

	if trace, ok := contexts.GetTrace(ctx); ok && trace != nil {
		selectedKey := selectTTLStickyKey(p.cache, enabled, "trace:"+trace.TraceID, cacheAffinityTTL)

		if log.DebugEnabled(ctx) {
			log.Debug(ctx, "Cache affinity key selected by trace fallback",
				log.String("trace_id", trace.TraceID),
				log.String("key_prefix", safeAPIKeyPrefix(selectedKey)),
			)
		}

		return recordSelectedChannelAPIKey(ctx, selectedKey)
	}

	//nolint:gosec // not a security issue, just a random selection.
	selectedKey := enabled[rand.IntN(len(enabled))]
	if log.DebugEnabled(ctx) {
		log.Debug(ctx, "Cache affinity random fallback key selected",
			log.String("key_prefix", safeAPIKeyPrefix(selectedKey)),
		)
	}

	return recordSelectedChannelAPIKey(ctx, selectedKey)
}

// RandomChannelKeyProvider explicitly selects a random enabled key while still
// recording the selected channel key for request metrics.
type RandomChannelKeyProvider struct {
	channel *Channel
}

func NewRandomChannelKeyProvider(channel *Channel) *RandomChannelKeyProvider {
	return &RandomChannelKeyProvider{channel: channel}
}

func (p *RandomChannelKeyProvider) Get(ctx context.Context) string {
	enabled := p.channel.cachedEnabledAPIKeys
	if len(enabled) == 0 {
		return recordSelectedChannelAPIKey(ctx, p.channel.Credentials.APIKeys[0])
	}

	if len(enabled) == 1 {
		return recordSelectedChannelAPIKey(ctx, enabled[0])
	}

	//nolint:gosec // not a security issue, just a random selection.
	return recordSelectedChannelAPIKey(ctx, enabled[rand.IntN(len(enabled))])
}

// RoundRobinChannelKeyProvider rotates enabled channel keys with an in-memory,
// concurrency-safe counter.
type RoundRobinChannelKeyProvider struct {
	channel *Channel
	next    atomic.Uint64
}

func NewRoundRobinChannelKeyProvider(channel *Channel) *RoundRobinChannelKeyProvider {
	return &RoundRobinChannelKeyProvider{channel: channel}
}

func (p *RoundRobinChannelKeyProvider) Get(ctx context.Context) string {
	enabled := p.channel.cachedEnabledAPIKeys
	if len(enabled) == 0 {
		return recordSelectedChannelAPIKey(ctx, p.channel.Credentials.APIKeys[0])
	}

	if len(enabled) == 1 {
		return recordSelectedChannelAPIKey(ctx, enabled[0])
	}

	idx := p.next.Add(1) - 1

	return recordSelectedChannelAPIKey(ctx, enabled[int(idx%uint64(len(enabled)))])
}

func selectStickyKey(cache *lru.Cache[string, string], enabled []string, seed string) string {
	if cached, ok := cache.Get(seed); ok && slices.Contains(enabled, cached) {
		return cached
	}

	selectedKey := rendezvousSelect(enabled, seed)
	cache.Add(seed, selectedKey)

	return selectedKey
}

func selectTTLStickyKey(cache *lru.Cache[string, stickyCacheEntry], enabled []string, seed string, ttl time.Duration) string {
	now := time.Now()
	if cached, ok := cache.Get(seed); ok && cached.ExpiresAt.After(now) && slices.Contains(enabled, cached.Key) {
		return cached.Key
	}

	selectedKey := rendezvousSelect(enabled, seed)
	cache.Add(seed, stickyCacheEntry{
		Key:       selectedKey,
		ExpiresAt: now.Add(ttl),
	})

	return selectedKey
}

func recordSelectedChannelAPIKey(ctx context.Context, key string) string {
	ctx = contexts.WithChannelAPIKey(ctx, key)

	return key
}

// rendezvousSelect picks a key using Highest Random Weight (Rendezvous) hashing.
// This is stable when the key set changes (minimal remapping compared to modulo).
func rendezvousSelect(keys []string, seed string) string {
	bestKey := keys[0]
	bestScore := hashAPIKey(seed + "|" + bestKey)

	for i := 1; i < len(keys); i++ {
		k := keys[i]

		s := hashAPIKey(seed + "|" + k)
		if s > bestScore {
			bestScore = s
			bestKey = k
		}
	}

	return bestKey
}

func hashAPIKey(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))

	return h.Sum64()
}

func safeAPIKeyPrefix(key string) string {
	if len(key) >= 2 {
		return key[:2]
	}

	return key
}

func safeAffinityIDPrefix(id string) string {
	if len(id) >= 18 {
		return id[:18]
	}

	return id
}
