package httpclient

import (
	"bytes"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

// BenchmarkRequestZstdWholeBuffer compares the production stream mechanism to
// the library's whole-buffer decoder on the same synthetic bytes and options.
// Both plain and dictionary requests already have their complete frame in RAM.
func BenchmarkRequestZstdWholeBuffer(b *testing.B) {
	payload := bytes.Repeat([]byte(`{"role":"tool","content":"synthetic repeated tool history 9007199254740993"}`), 16384)
	dictionary := bytes.Clone(payload[:64<<10])
	for _, useDictionary := range []bool{false, true} {
		name := "plain"
		options := []zstd.EOption{zstd.WithEncoderCRC(true), zstd.WithEncoderConcurrency(1)}
		if useDictionary {
			name = "dictionary"
			options = append(options, zstd.WithEncoderDictRaw(0, dictionary))
		}
		encoder, err := zstd.NewWriter(nil, options...)
		require.NoError(b, err)
		frame := encoder.EncodeAll(payload, nil)
		require.NoError(b, encoder.Close())
		for _, wholeBuffer := range []bool{false, true} {
			mechanism := "stream"
			if wholeBuffer {
				mechanism = "DecodeAll"
			}
			b.Run(name+"/"+mechanism, func(b *testing.B) {
				decode := func() ([]byte, error) {
					options := []zstd.DOption{zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64 << 20), zstd.WithDecoderMaxWindow(32 << 20)}
					if useDictionary {
						options = append(options, zstd.WithDecoderDictRaw(0, dictionary))
					}
					if wholeBuffer {
						if useDictionary {
							return decodeZstdRequestBody(frame, httpRequestBodyReadLimit, zstd.WithDecoderDictRaw(0, dictionary))
						}
						return decodeZstdRequestBody(frame, httpRequestBodyReadLimit)
					}
					decoder, err := zstd.NewReader(bytes.NewReader(frame), options...)
					if err != nil {
						return nil, err
					}
					defer decoder.Close()
					return readLimitedBody(decoder, httpRequestBodyReadLimit)
				}
				actual, err := decode()
				require.NoError(b, err)
				require.Equal(b, payload, actual)
				b.SetBytes(int64(len(payload)))
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					actual, err := decode()
					if err != nil || len(actual) != len(payload) {
						b.Fatal("synthetic whole-buffer decode failed")
					}
				}
			})
		}
	}
}
