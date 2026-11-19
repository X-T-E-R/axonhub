package httpclient

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

const maxCachedDictDecoders = 16

type dictDecoderEntry struct {
	dec      *zstd.Decoder
	lastUsed uint64
	mu       sync.RWMutex
}

var dictDecoderCache = struct {
	mu      sync.Mutex
	clock   uint64
	entries map[[sha256.Size]byte]*dictDecoderEntry
}{
	entries: make(map[[sha256.Size]byte]*dictDecoderEntry, maxCachedDictDecoders),
}

func getOrCreateDictDecoder(hash [sha256.Size]byte, dictionary []byte) (*dictDecoderEntry, error) {
	dictDecoderCache.mu.Lock()
	if entry, ok := dictDecoderCache.entries[hash]; ok {
		dictDecoderCache.clock++
		entry.lastUsed = dictDecoderCache.clock
		dictDecoderCache.mu.Unlock()
		return entry, nil
	}
	dictDecoderCache.mu.Unlock()

	dec, err := zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(httpRequestBodyReadLimit),
		zstd.WithDecoderMaxWindow(32<<20),
		zstd.WithDecoderDictRaw(0, dictionary),
	)
	if err != nil {
		return nil, err
	}

	var evicted *dictDecoderEntry
	dictDecoderCache.mu.Lock()
	if existing, ok := dictDecoderCache.entries[hash]; ok {
		dictDecoderCache.clock++
		existing.lastUsed = dictDecoderCache.clock
		dictDecoderCache.mu.Unlock()
		dec.Close()
		return existing, nil
	}
	if len(dictDecoderCache.entries) >= maxCachedDictDecoders {
		var oldestKey [sha256.Size]byte
		var oldestUsed uint64
		first := true
		for k, e := range dictDecoderCache.entries {
			if first || e.lastUsed < oldestUsed {
				oldestKey = k
				oldestUsed = e.lastUsed
				first = false
			}
		}
		evicted = dictDecoderCache.entries[oldestKey]
		delete(dictDecoderCache.entries, oldestKey)
	}
	dictDecoderCache.clock++
	entry := &dictDecoderEntry{
		dec:      dec,
		lastUsed: dictDecoderCache.clock,
	}
	dictDecoderCache.entries[hash] = entry
	dictDecoderCache.mu.Unlock()

	if evicted != nil {
		evicted.mu.Lock()
		if evicted.dec != nil {
			evicted.dec.Close()
			evicted.dec = nil
		}
		evicted.mu.Unlock()
	}

	return entry, nil
}

func decodeDictionaryZstd(hash [sha256.Size]byte, dictionary, frame []byte, limit int64) ([]byte, error) {
	if limit <= 0 {
		return nil, ErrHTTPRequestBodyTooLarge
	}
	entry, err := getOrCreateDictDecoder(hash, dictionary)
	if err != nil {
		return nil, err
	}

	entry.mu.RLock()
	dec := entry.dec
	if dec == nil {
		entry.mu.RUnlock()
		return decodeZstdRequestBody(frame, limit, zstd.WithDecoderDictRaw(0, dictionary))
	}
	decoded, err := dec.DecodeAll(frame, nil)
	entry.mu.RUnlock()

	if errors.Is(err, zstd.ErrDecoderSizeExceeded) || int64(len(decoded)) > limit {
		return nil, ErrHTTPRequestBodyTooLarge
	}
	if err != nil {
		return nil, err
	}
	return decoded, nil
}

var (
	ErrRequestDictionaryMissing     = errors.New("request dictionary is unavailable")
	ErrRequestDictionaryUnsupported = errors.New("request dictionary encoding is unavailable")
	ErrInvalidDictionaryRequest     = errors.New("invalid dictionary-compressed request")
)

// RequestDictionaryLookup resolves an immutable dictionary within the caller's authenticated scope.
// The dictionary hash identifies bytes, not permission to access them.
type RequestDictionaryLookup func([sha256.Size]byte) ([]byte, bool)

var requestDictionaryMagic = [...]byte{0x5e, 0x2a, 0x4d, 0x18, 0x20, 0, 0, 0}

func decodeDictionaryRequest(body []byte, lookup RequestDictionaryLookup) ([]byte, error) {
	if lookup == nil {
		return nil, ErrRequestDictionaryUnsupported
	}
	if len(body) <= len(requestDictionaryMagic)+sha256.Size || !bytes.Equal(body[:len(requestDictionaryMagic)], requestDictionaryMagic[:]) {
		return nil, ErrInvalidDictionaryRequest
	}
	var hash [sha256.Size]byte
	copy(hash[:], body[len(requestDictionaryMagic):len(requestDictionaryMagic)+sha256.Size])
	dictionary, ok := lookup(hash)
	if !ok {
		return nil, ErrRequestDictionaryMissing
	}
	if sha256.Sum256(dictionary) != hash {
		return nil, ErrInvalidDictionaryRequest
	}
	frame := body[len(requestDictionaryMagic)+sha256.Size:]
	if err := validateDictionaryFrame(frame); err != nil {
		return nil, err
	}
	decoded, err := decodeDictionaryZstd(hash, dictionary, frame, httpRequestBodyReadLimit)
	if err != nil {
		if errors.Is(err, ErrHTTPRequestBodyTooLarge) {
			return nil, err
		}
		return nil, fmt.Errorf("%w: %v", ErrInvalidDictionaryRequest, err)
	}
	return decoded, nil
}

func validateDictionaryFrame(frame []byte) error {
	var header zstd.Header
	if err := header.Decode(frame); err != nil || header.Skippable || !header.HasCheckSum || header.DictionaryID != 0 {
		return ErrInvalidDictionaryRequest
	}
	if header.WindowSize > httpRequestBodyReadLimit || (header.HasFCS && header.FrameContentSize > httpRequestBodyReadLimit) {
		return ErrHTTPRequestBodyTooLarge
	}
	position := header.HeaderSize
	for {
		if len(frame)-position < 3 {
			return fmt.Errorf("%w: %v", ErrInvalidDictionaryRequest, io.ErrUnexpectedEOF)
		}
		block := uint32(frame[position]) | uint32(frame[position+1])<<8 | uint32(frame[position+2])<<16
		position += 3
		kind := (block >> 1) & 3
		size := int(block >> 3)
		if kind == 3 {
			return ErrInvalidDictionaryRequest
		}
		if kind == 1 {
			size = 1
		}
		if size > len(frame)-position {
			return ErrInvalidDictionaryRequest
		}
		position += size
		if block&1 != 0 {
			break
		}
	}
	if len(frame)-position != 4 {
		return ErrInvalidDictionaryRequest
	}
	return nil
}
