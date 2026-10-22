package middleware

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

type requestDictionaryScope struct {
	project int
	key     int
}

type requestDictionaryKey struct {
	scope requestDictionaryScope
	hash  [sha256.Size]byte
}

type requestDictionaryEntry struct {
	body      []byte
	expiresAt time.Time
	order     uint64
}

// RequestBodyTransport owns bounded, opt-in, node-local request dictionaries.
// Dictionary acknowledgements describe retained bytes, not model completion.
type RequestBodyTransport struct {
	mu              sync.Mutex
	entries         map[requestDictionaryKey]requestDictionaryEntry
	bytes           int
	sequence        uint64
	ttl             time.Duration
	maxBytes        int
	maxScopeBytes   int
	maxEntryBytes   int
	maxEntries      int
	maxScopeEntries int
	decodeSlots     chan struct{}
	now             func() time.Time
}

func NewRequestBodyTransport() *RequestBodyTransport {
	return &RequestBodyTransport{
		entries:         make(map[requestDictionaryKey]requestDictionaryEntry),
		ttl:             10 * time.Minute,
		maxBytes:        64 << 20,
		maxScopeBytes:   24 << 20,
		maxEntryBytes:   16 << 20,
		maxEntries:      128,
		maxScopeEntries: 4,
		decodeSlots:     make(chan struct{}, 4),
		now:             time.Now,
	}
}

func (t *RequestBodyTransport) lookup(scope requestDictionaryScope, hash [sha256.Size]byte) ([]byte, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := requestDictionaryKey{scope: scope, hash: hash}
	entry, ok := t.entries[key]
	if !ok {
		return nil, false
	}
	if !entry.expiresAt.After(t.now()) {
		t.remove(key)
		return nil, false
	}
	return entry.body, true
}

func (t *RequestBodyTransport) retain(scope requestDictionaryScope, body []byte) string {
	if len(body) == 0 || len(body) > t.maxEntryBytes || len(body) > t.maxScopeBytes || len(body) > t.maxBytes {
		return ""
	}
	hash := sha256.Sum256(body)
	key := requestDictionaryKey{scope: scope, hash: hash}
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	for existing, entry := range t.entries {
		if !entry.expiresAt.After(now) {
			t.remove(existing)
		}
	}
	if entry, ok := t.entries[key]; ok {
		entry.expiresAt = now.Add(t.ttl)
		t.sequence++
		entry.order = t.sequence
		t.entries[key] = entry
		return hex.EncodeToString(hash[:])
	}
	for {
		count, size := t.scopeUsage(scope)
		if count < t.maxScopeEntries && size+len(body) <= t.maxScopeBytes {
			break
		}
		if !t.evict(&scope) {
			return ""
		}
	}
	for len(t.entries) >= t.maxEntries || t.bytes+len(body) > t.maxBytes {
		if !t.evict(nil) {
			return ""
		}
	}
	t.sequence++
	t.entries[key] = requestDictionaryEntry{
		body:      bytes.Clone(body),
		expiresAt: now.Add(t.ttl),
		order:     t.sequence,
	}
	t.bytes += len(body)
	return hex.EncodeToString(hash[:])
}

func (t *RequestBodyTransport) scopeUsage(scope requestDictionaryScope) (count, size int) {
	for key, entry := range t.entries {
		if key.scope == scope {
			count++
			size += len(entry.body)
		}
	}
	return count, size
}

func (t *RequestBodyTransport) remove(key requestDictionaryKey) {
	if entry, ok := t.entries[key]; ok {
		t.bytes -= len(entry.body)
		delete(t.entries, key)
	}
}

func (t *RequestBodyTransport) evict(scope *requestDictionaryScope) bool {
	var oldest requestDictionaryKey
	var order uint64
	found := false
	for key, entry := range t.entries {
		if scope != nil && key.scope != *scope {
			continue
		}
		if !found || entry.order < order {
			oldest, order, found = key, entry.order, true
		}
	}
	if found {
		t.remove(oldest)
	}
	return found
}
