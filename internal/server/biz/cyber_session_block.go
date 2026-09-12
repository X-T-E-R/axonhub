package biz

import (
	"context"
	"time"

	"github.com/looplj/axonhub/internal/pkg/xcache"
)

// CyberSessionBlockEntry is the minimal cache record for a blocked conversation.
// ExpiresAt remains authoritative if a lower cache tier repopulates an upper tier.
type CyberSessionBlockEntry struct {
	ExpiresAt time.Time `json:"expires_at"`
}

// SetCyberSessionBlock records a refusal without retaining request or response bodies.
func (s *SystemService) SetCyberSessionBlock(ctx context.Context, key string, ttl time.Duration) error {
	if s == nil || s.CyberSessionBlockCache == nil || key == "" || ttl <= 0 {
		return nil
	}

	s.cyberSessionBlockMu.Lock()
	defer s.cyberSessionBlockMu.Unlock()
	if _, blocked := s.CyberSessionBlock(ctx, key); blocked {
		return nil
	}

	entry := CyberSessionBlockEntry{ExpiresAt: time.Now().Add(ttl)}
	_, err := xcache.SetIfAbsent(ctx, s.CyberSessionBlockCache, key, entry, ttl)
	return err
}

// CyberSessionBlock returns an active block. Cache misses and unavailable caches fail open.
func (s *SystemService) CyberSessionBlock(ctx context.Context, key string) (CyberSessionBlockEntry, bool) {
	if s == nil || s.CyberSessionBlockCache == nil || key == "" {
		return CyberSessionBlockEntry{}, false
	}

	entry, err := s.CyberSessionBlockCache.Get(ctx, key)
	if err != nil {
		return CyberSessionBlockEntry{}, false
	}
	if entry.ExpiresAt.IsZero() || !time.Now().Before(entry.ExpiresAt) {
		_ = s.CyberSessionBlockCache.Delete(ctx, key)
		return CyberSessionBlockEntry{}, false
	}

	return entry, true
}

// CyberSessionBlocks returns the first active block while using bounded batch reads where supported.
func (s *SystemService) CyberSessionBlocks(ctx context.Context, keys []string) (CyberSessionBlockEntry, bool) {
	if s == nil || s.CyberSessionBlockCache == nil || len(keys) == 0 {
		return CyberSessionBlockEntry{}, false
	}

	entries := xcache.GetMany(ctx, s.CyberSessionBlockCache, keys)
	for _, key := range keys {
		entry, ok := entries[key]
		if !ok {
			continue
		}
		if !entry.ExpiresAt.IsZero() && time.Now().Before(entry.ExpiresAt) {
			return entry, true
		}
		_ = s.CyberSessionBlockCache.Delete(ctx, key)
	}

	return CyberSessionBlockEntry{}, false
}
