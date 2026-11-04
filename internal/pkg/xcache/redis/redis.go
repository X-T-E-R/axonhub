package redis

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	lib_store "github.com/eko/gocache/lib/v4/store"
	redis "github.com/redis/go-redis/v9"
)

// RedisClientInterface represents the Redis commands needed for normal cache operations.
//
// Scan, SRem, MGet and SetNX are optional capabilities so existing test doubles and
// non-redis clients can still be used for the basic operations. The real go-redis
// clients implement all of them; Clear requires Scan to avoid an unbounded database-wide operation.
type RedisClientInterface interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	TTL(ctx context.Context, key string) *redis.DurationCmd
	Expire(ctx context.Context, key string, expiration time.Duration) *redis.BoolCmd
	Set(ctx context.Context, key string, values any, expiration time.Duration) *redis.StatusCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
	SAdd(ctx context.Context, key string, members ...any) *redis.IntCmd
	SMembers(ctx context.Context, key string) *redis.StringSliceCmd
}

const (
	// RedisType represents the storage type as a string value.
	RedisType = "redis"
	// DefaultNamespace is used when no cache namespace is configured.
	DefaultNamespace = "axonhub"
	// RedisTagPattern is retained for source compatibility. New stores use a
	// namespace/domain-scoped tag key instead of this legacy unscoped pattern.
	RedisTagPattern = "gocache_tag_%s"
	redisValuePart  = "value"
	redisTagPart    = "tag"
	redisScanCount  = 128
	defaultTagsTTL  = 720 * time.Hour
)

// RedisStore wraps RedisStore with a namespace and typed cache domain.
type RedisStore[T any] struct {
	client         RedisClientInterface
	options        *lib_store.Options
	namespace      string
	domain         string
	prefix         string
	legacyPrefixes []string
}

type redisManyClient interface {
	MGet(ctx context.Context, keys ...string) *redis.SliceCmd
}

type redisSetNXClient interface {
	SetNX(ctx context.Context, key string, value any, expiration time.Duration) *redis.BoolCmd
}

type redisScanClient interface {
	Scan(ctx context.Context, cursor uint64, match string, count int64) *redis.ScanCmd
}

type redisSRemClient interface {
	SRem(ctx context.Context, key string, members ...any) *redis.IntCmd
}

// TypedDomain returns a stable, bounded domain identifier for T. The hash keeps
// package/type details out of Redis key syntax while preserving isolation between
// otherwise identical user keys stored by different typed caches.
func TypedDomain[T any]() string {
	typeOf := reflect.TypeOf((*T)(nil)).Elem()
	descriptor := typeOf.PkgPath() + "|" + typeOf.String()
	digest := sha256.Sum256([]byte(descriptor))
	return "type-" + hex.EncodeToString(digest[:8])
}

func normalizeNamespace(namespace string) string {
	if namespace == "" {
		return DefaultNamespace
	}

	return namespace
}

func normalizeDomain[T any](domain string) string {
	if domain == "" {
		return TypedDomain[T]()
	}

	return domain
}

// NewRedisStore creates a namespace/domain-scoped generic store.
func NewRedisStore[T any](client RedisClientInterface, options ...lib_store.Option) *RedisStore[T] {
	return NewRedisStoreWithNamespace[T](client, DefaultNamespace, "", options...)
}

// NewRedisStoreWithNamespace creates a store using the supplied namespace and
// optional explicit domain. An empty domain uses TypedDomain[T]().
func NewRedisStoreWithNamespace[T any](client RedisClientInterface, namespace, domain string, options ...lib_store.Option) *RedisStore[T] {
	namespace = normalizeNamespace(namespace)
	domain = normalizeDomain[T](domain)
	prefix := namespace + ":cache:" + domain + ":"

	return &RedisStore[T]{
		client:    client,
		options:   lib_store.ApplyOptions(options...),
		namespace: namespace,
		domain:    domain,
		prefix:    prefix,
	}
}

func normalizeLegacyPrefixes(prefixes []string) []string {
	result := make([]string, 0, len(prefixes))
	seen := make(map[string]struct{}, len(prefixes))
	for _, prefix := range prefixes {
		if prefix == "" {
			continue
		}
		if _, ok := seen[prefix]; ok {
			continue
		}
		seen[prefix] = struct{}{}
		result = append(result, prefix)
	}

	return result
}

// NewRedisStoreWithLegacyPrefixes is an explicit compatibility mode for
// security state written before namespace/domain prefixes were introduced. It
// reads and writes raw logical keys, but Clear is restricted to the supplied
// prefixes. It never performs an implicit fallback from a scoped store.
func NewRedisStoreWithLegacyPrefixes[T any](client RedisClientInterface, namespace string, legacyPrefixes []string, options ...lib_store.Option) *RedisStore[T] {
	legacyPrefixes = normalizeLegacyPrefixes(legacyPrefixes)
	if len(legacyPrefixes) == 0 {
		return NewRedisStoreWithNamespace[T](client, namespace, "", options...)
	}

	namespace = normalizeNamespace(namespace)
	digest := sha256.Sum256([]byte(strings.Join(legacyPrefixes, "\x00")))
	domain := "legacy-" + hex.EncodeToString(digest[:8])
	prefix := namespace + ":cache:" + domain + ":"

	return &RedisStore[T]{
		client:         client,
		options:        lib_store.ApplyOptions(options...),
		namespace:      namespace,
		domain:         domain,
		prefix:         prefix,
		legacyPrefixes: legacyPrefixes,
	}
}

func (s *RedisStore[T]) legacyMode() bool {
	return len(s.legacyPrefixes) > 0
}

func (s *RedisStore[T]) valueKey(key string) string {
	if s.legacyMode() {
		return key
	}

	return s.prefix + redisValuePart + ":" + key
}

func (s *RedisStore[T]) tagKey(tag string) string {
	return s.prefix + redisTagPart + ":" + tag
}

func (s *RedisStore[T]) valuePatterns() []string {
	if s.legacyMode() {
		patterns := make([]string, 0, len(s.legacyPrefixes))
		for _, prefix := range s.legacyPrefixes {
			patterns = append(patterns, escapeScanPattern(prefix)+"*")
		}
		return patterns
	}

	return []string{escapeScanPattern(s.prefix+redisValuePart+":") + "*"}
}

func (s *RedisStore[T]) tagPattern() string {
	return escapeScanPattern(s.prefix+redisTagPart+":") + "*"
}

func (s *RedisStore[T]) isValueKey(key string) bool {
	if s.legacyMode() {
		for _, prefix := range s.legacyPrefixes {
			if strings.HasPrefix(key, prefix) {
				return true
			}
		}
		return false
	}

	return strings.HasPrefix(key, s.prefix+redisValuePart+":")
}

func (s *RedisStore[T]) validateKey(key string) error {
	if s.legacyMode() && !s.isValueKey(key) {
		return fmt.Errorf("legacy cache key %q is outside configured prefixes", key)
	}

	return nil
}

func cacheKey(key any) (string, error) {
	keyString, ok := key.(string)
	if !ok {
		return "", fmt.Errorf("expected string key, got %T", key)
	}

	return keyString, nil
}

func escapeScanPattern(value string) string {
	var escaped strings.Builder
	escaped.Grow(len(value))

	for _, char := range value {
		switch char {
		case '\\', '*', '?', '[':
			escaped.WriteByte('\\')
		}
		escaped.WriteRune(char)
	}

	return escaped.String()
}

func (s *RedisStore[T]) scanKeys(ctx context.Context, pattern string, visit func([]string) error) error {
	client, ok := s.client.(redisScanClient)
	if !ok {
		return fmt.Errorf("redis client does not support SCAN")
	}

	var cursor uint64
	for {
		keys, next, err := client.Scan(ctx, cursor, pattern, redisScanCount).Result()
		if err != nil {
			return err
		}

		if len(keys) > 0 {
			if err := visit(keys); err != nil {
				return err
			}
		}

		cursor = next
		if cursor == 0 {
			return nil
		}
	}
}

func (s *RedisStore[T]) clearPattern(ctx context.Context, pattern string) error {
	for {
		found := false
		if err := s.scanKeys(ctx, pattern, func(keys []string) error {
			found = true
			return s.deleteKeys(ctx, keys)
		}); err != nil {
			return err
		}
		if !found {
			return nil
		}
	}
}

func (s *RedisStore[T]) deleteKeys(ctx context.Context, keys []string) error {
	for _, key := range keys {
		if err := s.client.Del(ctx, key).Err(); err != nil {
			return err
		}
	}

	return nil
}

// removeTagMembers keeps the tag registry bounded after explicit key deletion.
// A client without SRem/SCAN is still allowed to perform the primary delete;
// go-redis clients support both optional commands.
func (s *RedisStore[T]) removeTagMembers(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}

	client, ok := s.client.(redisSRemClient)
	if !ok {
		return nil
	}
	if _, ok := s.client.(redisScanClient); !ok {
		return nil
	}

	return s.scanKeys(ctx, s.tagPattern(), func(tagKeys []string) error {
		members := make([]any, 0, len(keys))
		for _, key := range keys {
			members = append(members, key)
		}

		for _, tagKey := range tagKeys {
			if err := client.SRem(ctx, tagKey, members...).Err(); err != nil {
				return err
			}
		}

		return nil
	})
}

// Get returns typed data stored from a given key.
func (gs *RedisStore[T]) Get(ctx context.Context, key any) (any, error) {
	var result T

	keyString, err := cacheKey(key)
	if err != nil {
		return result, lib_store.NotFoundWithCause(err)
	}
	if err := gs.validateKey(keyString); err != nil {
		return result, lib_store.NotFoundWithCause(err)
	}

	object, err := gs.client.Get(ctx, gs.valueKey(keyString)).Result()
	if errors.Is(err, redis.Nil) {
		return result, lib_store.NotFoundWithCause(err)
	}

	if err != nil {
		return result, err
	}

	// JSON object or array - unmarshal into the target type
	if err := json.Unmarshal([]byte(object), &result); err != nil {
		var zero T
		// If JSON decoding fails, return error
		return zero, err
	}

	return result, nil
}

// GetMany fetches typed values in one Redis round trip. Missing keys are omitted.
func (gs *RedisStore[T]) GetMany(ctx context.Context, keys []string) (map[string]T, error) {
	client, ok := gs.client.(redisManyClient)
	if !ok {
		return nil, fmt.Errorf("redis client does not support MGET")
	}

	physicalKeys := make([]string, len(keys))
	for index, key := range keys {
		if err := gs.validateKey(key); err != nil {
			return nil, err
		}
		physicalKeys[index] = gs.valueKey(key)
	}

	values, err := client.MGet(ctx, physicalKeys...).Result()
	if err != nil {
		return nil, err
	}

	result := make(map[string]T, len(values))
	for index, raw := range values {
		if raw == nil || index >= len(keys) {
			continue
		}
		encoded, ok := raw.(string)
		if !ok {
			continue
		}
		var value T
		if err := json.Unmarshal([]byte(encoded), &value); err != nil {
			continue
		}
		result[keys[index]] = value
	}

	return result, nil
}

// SetIfAbsent stores a typed value only when the Redis key does not exist.
func (gs *RedisStore[T]) SetIfAbsent(ctx context.Context, key string, value T, expiration time.Duration) (bool, error) {
	if err := gs.validateKey(key); err != nil {
		return false, err
	}

	client, ok := gs.client.(redisSetNXClient)
	if !ok {
		return false, fmt.Errorf("redis client does not support SETNX")
	}

	raw, err := json.Marshal(value)
	if err != nil {
		return false, err
	}

	return client.SetNX(ctx, gs.valueKey(key), string(raw), expiration).Result()
}

// GetWithTTL returns typed data stored from a given key and its corresponding TTL.
func (gs *RedisStore[T]) GetWithTTL(ctx context.Context, key any) (any, time.Duration, error) {
	var result T

	keyString, ok := key.(string)
	if !ok {
		return result, 0, lib_store.NotFoundWithCause(fmt.Errorf("expected string key, got %T", key))
	}
	if err := gs.validateKey(keyString); err != nil {
		return result, 0, lib_store.NotFoundWithCause(err)
	}

	physicalKey := gs.valueKey(keyString)
	object, err := gs.client.Get(ctx, physicalKey).Result()
	if errors.Is(err, redis.Nil) {
		return result, 0, lib_store.NotFoundWithCause(err)
	}

	if err != nil {
		return result, 0, err
	}

	// JSON object or array - unmarshal into the target type
	if err := json.Unmarshal([]byte(object), &result); err != nil {
		var zero T
		// If JSON decoding fails, return error
		return zero, 0, err
	}

	ttl, err := gs.client.TTL(ctx, physicalKey).Result()
	if err != nil {
		var zero T
		return zero, 0, err
	}

	return result, ttl, err
}

// Set defines data in Redis for given key identifier.
func (s *RedisStore[T]) Set(ctx context.Context, key any, value any, options ...lib_store.Option) error {
	opts := lib_store.ApplyOptionsWithDefault(s.options, options...)
	keyString, err := cacheKey(key)
	if err != nil {
		return err
	}
	if err := s.validateKey(keyString); err != nil {
		return err
	}

	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}

	physicalKey := s.valueKey(keyString)
	if err := s.client.Set(ctx, physicalKey, string(raw), opts.Expiration).Err(); err != nil {
		return err
	}

	// A key can be overwritten with a different tag set. Remove old memberships
	// before adding the current set so tagged invalidation remains exact.
	if err := s.removeTagMembers(ctx, []string{physicalKey}); err != nil {
		return err
	}

	if tags := opts.Tags; len(tags) > 0 {
		ttl := opts.TagsTTL
		if ttl == 0 {
			ttl = defaultTagsTTL
		}
		if err := s.setTagsWithTTL(ctx, physicalKey, tags, ttl); err != nil {
			return err
		}
	}

	return nil
}

func (s *RedisStore[T]) setTagsWithTTL(ctx context.Context, physicalKey string, tags []string, ttl time.Duration) error {
	for _, tag := range tags {
		tagKey := s.tagKey(tag)
		if err := s.client.SAdd(ctx, tagKey, physicalKey).Err(); err != nil {
			return err
		}
		if err := s.client.Expire(ctx, tagKey, ttl).Err(); err != nil {
			return err
		}
	}

	return nil
}

// Delete removes data from Redis for given key identifier.
func (gs *RedisStore[T]) Delete(ctx context.Context, key any) error {
	keyString, err := cacheKey(key)
	if err != nil {
		return err
	}
	if err := gs.validateKey(keyString); err != nil {
		return err
	}

	physicalKey := gs.valueKey(keyString)
	if err := gs.client.Del(ctx, physicalKey).Err(); err != nil {
		return err
	}

	return gs.removeTagMembers(ctx, []string{physicalKey})
}

// GetType returns the store type.
func (gs *RedisStore[T]) GetType() string {
	return RedisType
}

// Clear removes only this namespace/domain's value and tag keys, leaving
// unrelated application data untouched.
func (gs *RedisStore[T]) Clear(ctx context.Context) error {
	for _, pattern := range gs.valuePatterns() {
		if err := gs.clearPattern(ctx, pattern); err != nil {
			return err
		}
	}

	return gs.clearPattern(ctx, gs.tagPattern())
}

// Invalidate removes values associated with the supplied tags in this
// namespace/domain only. With no tags it is intentionally a no-op; callers can
// use Clear when they mean the entire typed cache domain.
func (gs *RedisStore[T]) Invalidate(ctx context.Context, options ...lib_store.InvalidateOption) error {
	opts := lib_store.ApplyInvalidateOptions(options...)
	if len(opts.Tags) == 0 {
		return nil
	}

	seenTags := make(map[string]struct{}, len(opts.Tags))
	for _, tag := range opts.Tags {
		if _, seen := seenTags[tag]; seen {
			continue
		}
		seenTags[tag] = struct{}{}

		tagKey := gs.tagKey(tag)
		members, err := gs.client.SMembers(ctx, tagKey).Result()
		if err != nil {
			return err
		}

		physicalKeys := make([]string, 0, len(members))
		for _, member := range members {
			if gs.isValueKey(member) {
				physicalKeys = append(physicalKeys, member)
			}
		}

		if err := gs.deleteKeys(ctx, physicalKeys); err != nil {
			return err
		}
		if err := gs.client.Del(ctx, tagKey).Err(); err != nil {
			return err
		}
		if err := gs.removeTagMembers(ctx, physicalKeys); err != nil {
			return err
		}
	}

	return nil
}
