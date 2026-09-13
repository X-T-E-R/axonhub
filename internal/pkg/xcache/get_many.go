package xcache

import (
	"context"
	"maps"
	"time"
)

const redisGetManyBatchSize = 128

type cacheLayers[T any] interface {
	GetCaches() []SetterCache[T]
}

type storeManyGetter[T any] interface {
	GetMany(ctx context.Context, keys []string) (map[string]T, error)
}

type storeFirstWriter[T any] interface {
	SetIfAbsent(ctx context.Context, key string, value T, expiration time.Duration) (bool, error)
}

// SetIfAbsent preserves the first block expiry across Redis-backed instances.
// Memory-only callers provide their own process-local synchronization.
func SetIfAbsent[T any](ctx context.Context, cache Cache[T], key string, value T, expiration time.Duration) (bool, error) {
	if cache == nil || key == "" || cache.GetType() == "noop" {
		return false, nil
	}

	if chain, ok := cache.(cacheLayers[T]); ok {
		layers := chain.GetCaches()
		for index, layer := range layers {
			writer, ok := layer.GetCodec().GetStore().(storeFirstWriter[T])
			if !ok {
				continue
			}
			created, err := writer.SetIfAbsent(ctx, key, value, expiration)
			if err != nil || !created {
				return created, err
			}
			for _, earlier := range layers[:index] {
				if err := earlier.Set(ctx, key, value, WithExpiration(expiration)); err != nil {
					return false, err
				}
			}
			return true, nil
		}
	}

	if layer, ok := cache.(SetterCache[T]); ok {
		if writer, ok := layer.GetCodec().GetStore().(storeFirstWriter[T]); ok {
			return writer.SetIfAbsent(ctx, key, value, expiration)
		}
	}

	if err := cache.Set(ctx, key, value, WithExpiration(expiration)); err != nil {
		return false, err
	}
	return true, nil
}

// GetMany retrieves string keys from memory locally and Redis in bounded MGET batches.
// Missing entries and cache errors are omitted so admission checks fail open.
func GetMany[T any](ctx context.Context, cache Cache[T], keys []string) map[string]T {
	result := make(map[string]T)
	if cache == nil || len(keys) == 0 || cache.GetType() == "noop" {
		return result
	}

	remaining := append([]string(nil), keys...)
	if chain, ok := cache.(cacheLayers[T]); ok {
		for _, layer := range chain.GetCaches() {
			remaining = getManyFromLayer(ctx, layer, remaining, result)
			if len(remaining) == 0 {
				break
			}
		}
		return result
	}

	if layer, ok := cache.(SetterCache[T]); ok {
		getManyFromLayer(ctx, layer, remaining, result)
		return result
	}

	for _, key := range remaining {
		if value, err := cache.Get(ctx, key); err == nil {
			result[key] = value
		}
	}
	return result
}

func getManyFromLayer[T any](ctx context.Context, cache SetterCache[T], keys []string, result map[string]T) []string {
	if len(keys) == 0 {
		return nil
	}

	store := cache.GetCodec().GetStore()
	if getter, ok := store.(storeManyGetter[T]); ok {
		for start := 0; start < len(keys); start += redisGetManyBatchSize {
			end := min(start+redisGetManyBatchSize, len(keys))
			values, err := getter.GetMany(ctx, keys[start:end])
			if err != nil {
				continue
			}
			maps.Copy(result, values)
		}
	} else {
		for _, key := range keys {
			if value, err := cache.Get(ctx, key); err == nil {
				result[key] = value
			}
		}
	}

	remaining := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, ok := result[key]; !ok {
			remaining = append(remaining, key)
		}
	}
	return remaining
}
