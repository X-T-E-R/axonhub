package httpclient

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
)

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
	decoder, err := zstd.NewReader(bytes.NewReader(frame),
		zstd.WithDecoderDictRaw(0, dictionary),
		zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(64<<20),
		zstd.WithDecoderMaxWindow(32<<20),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidDictionaryRequest, err)
	}
	defer decoder.Close()
	decoded, err := readLimitedBody(decoder, httpRequestBodyReadLimit)
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
