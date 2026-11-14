package biz

import (
	"context"
	"hash/fnv"
	"slices"
	"strings"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/objects"
)

const traceStickyLRUSize = 1024

type stickyCacheEntry struct {
	Key       string
	ExpiresAt time.Time
}

type StaticChannelKeyProvider struct{ key string }

func NewStaticChannelKeyProvider(key string) *StaticChannelKeyProvider {
	return &StaticChannelKeyProvider{key: key}
}
func (p *StaticChannelKeyProvider) Get(ctx context.Context) string {
	return recordSelectedChannelAPIKey(ctx, p.key)
}

type TraceStickyKeyProvider struct {
	channel *Channel
	cache   *lru.Cache[string, string]
}

func NewTraceStickyKeyProvider(channel *Channel) *TraceStickyKeyProvider {
	channel.runtime()
	cache, _ := lru.New[string, string](traceStickyLRUSize)
	return &TraceStickyKeyProvider{channel: channel, cache: cache}
}
func (p *TraceStickyKeyProvider) Get(ctx context.Context) string {
	var sticky func([]string) string
	if trace, ok := contexts.GetTrace(ctx); ok && trace != nil && trace.TraceID != "" {
		sticky = func(keys []string) string {
			if old, ok := p.cache.Get(trace.TraceID); ok && !slices.Contains(keys, old) {
				p.channel.logCredentialAffinityEscape(ctx, old, keys)
			}
			return selectStickyKey(p.cache, keys, trace.TraceID)
		}
	}
	return p.channel.selectCredential(ctx, objects.ChannelKeySelectionStrategyTraceSticky, sticky)
}

type CacheAffinityKeyProvider struct {
	channel             *Channel
	cache               *lru.Cache[string, stickyCacheEntry]
	likelyTTL, exactTTL time.Duration
}

func NewCacheAffinityKeyProvider(channel *Channel) *CacheAffinityKeyProvider {
	channel.runtime()
	cache, _ := lru.New[string, stickyCacheEntry](traceStickyLRUSize)
	selection := channelKeySelectionForProvider(channel)
	return &CacheAffinityKeyProvider{channel: channel, cache: cache, likelyTTL: time.Duration(selection.LikelyAffinityTTLMinutesOrDefault()) * time.Minute, exactTTL: time.Duration(selection.ExactAffinityTTLMinutesOrDefault()) * time.Minute}
}
func (p *CacheAffinityKeyProvider) Get(ctx context.Context) string {
	var sticky func([]string) string
	if id, ok := contexts.GetChannelKeyAffinityID(ctx); ok && id != "" {
		sticky = func(keys []string) string {
			if old, ok := p.cache.Get(id); ok && old.ExpiresAt.After(time.Now()) && !slices.Contains(keys, old.Key) {
				p.channel.logCredentialAffinityEscape(ctx, old.Key, keys)
			}
			return selectTTLStickyKey(p.cache, keys, id, p.ttlForAffinityID(id))
		}
	}
	return p.channel.selectCredential(ctx, objects.ChannelKeySelectionStrategyCacheAffinity, sticky)
}
func (p *CacheAffinityKeyProvider) ttlForAffinityID(id string) time.Duration {
	if cacheAffinityTierFromID(id) == "exact" {
		return p.exactTTL
	}
	return p.likelyTTL
}
func channelKeySelectionForProvider(channel *Channel) *objects.ChannelKeySelection {
	if channel == nil {
		return nil
	}
	if channel.Settings == nil || channel.Settings.KeySelection == nil || channel.Settings.KeySelection.Strategy == "" {
		return channel.globalKeySelection
	}
	return channel.Settings.KeySelection
}

type RandomChannelKeyProvider struct{ channel *Channel }

func NewRandomChannelKeyProvider(channel *Channel) *RandomChannelKeyProvider {
	channel.runtime()
	return &RandomChannelKeyProvider{channel}
}
func (p *RandomChannelKeyProvider) Get(ctx context.Context) string {
	return p.channel.selectCredential(ctx, objects.ChannelKeySelectionStrategyRandom, nil)
}

type RoundRobinChannelKeyProvider struct{ channel *Channel }

func NewRoundRobinChannelKeyProvider(channel *Channel) *RoundRobinChannelKeyProvider {
	channel.runtime()
	return &RoundRobinChannelKeyProvider{channel}
}
func (p *RoundRobinChannelKeyProvider) Get(ctx context.Context) string {
	return p.channel.selectCredential(ctx, objects.ChannelKeySelectionStrategyRoundRobin, nil)
}

type capacityChannelKeyProvider struct {
	channel  *Channel
	strategy objects.ChannelKeySelectionStrategy
}

func (p *capacityChannelKeyProvider) Get(ctx context.Context) string {
	return p.channel.selectCredential(ctx, p.strategy, nil)
}

func selectStickyKey(cache *lru.Cache[string, string], enabled []string, seed string) string {
	if cached, ok := cache.Get(seed); ok && slices.Contains(enabled, cached) {
		return cached
	}
	key := rendezvousSelect(enabled, seed)
	cache.Add(seed, key)
	return key
}
func selectTTLStickyKey(cache *lru.Cache[string, stickyCacheEntry], enabled []string, seed string, ttl time.Duration) string {
	return selectTTLStickyKeyAt(cache, enabled, seed, ttl, time.Now())
}
func selectTTLStickyKeyAt(cache *lru.Cache[string, stickyCacheEntry], enabled []string, seed string, ttl time.Duration, now time.Time) string {
	if cached, ok := cache.Get(seed); ok && cached.ExpiresAt.After(now) && slices.Contains(enabled, cached.Key) {
		cached.ExpiresAt = now.Add(ttl)
		cache.Add(seed, cached)
		return cached.Key
	}
	key := rendezvousSelect(enabled, seed)
	cache.Add(seed, stickyCacheEntry{key, now.Add(ttl)})
	return key
}
func recordSelectedChannelAPIKey(ctx context.Context, key string) string {
	contexts.WithChannelAPIKey(ctx, key)
	return key
}
func rendezvousSelect(keys []string, seed string) string {
	best := keys[0]
	score := hashAPIKey(seed + "|" + best)
	for _, key := range keys[1:] {
		if candidate := hashAPIKey(seed + "|" + key); candidate > score {
			score = candidate
			best = key
		}
	}
	return best
}
func hashAPIKey(s string) uint64 { h := fnv.New64a(); _, _ = h.Write([]byte(s)); return h.Sum64() }
func safeAPIKeyPrefix(key string) string {
	if len(key) >= 2 {
		return key[:2] + "..."
	}
	return strings.Repeat("*", len(key))
}
func safeAffinityIDPrefix(id string) string {
	if len(id) >= 18 {
		return id[:18]
	}
	return id
}
func cacheAffinityTierFromID(id string) string {
	if !strings.HasPrefix(id, "cache:") {
		return "unknown"
	}
	rest := strings.TrimPrefix(id, "cache:")
	i := strings.IndexByte(rest, ':')
	if i <= 0 {
		return "unknown"
	}
	return rest[:i]
}
