package httpclient

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func encodeStreamingZstd(t *testing.T, body, dictionary []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	options := []zstd.EOption{zstd.WithEncoderConcurrency(1), zstd.WithEncoderCRC(true)}
	if dictionary != nil {
		options = append(options, zstd.WithEncoderDictRaw(0, dictionary))
	}
	encoder, err := zstd.NewWriter(&output, options...)
	require.NoError(t, err)
	_, err = encoder.Write(body)
	require.NoError(t, err)
	require.NoError(t, encoder.Close())
	return output.Bytes()
}

func TestZstdWholeBufferPreservesUnknownSizeAndConcatenatedFrames(t *testing.T) {
	body := bytes.Repeat([]byte("synthetic opaque input \x00 \xff 9007199254740993 "), 8192)
	frame := encodeStreamingZstd(t, body, nil)
	var header zstd.Header
	require.NoError(t, header.Decode(frame))
	require.False(t, header.HasFCS)
	for _, encoded := range [][]byte{frame, append(bytes.Clone(frame), frame...)} {
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(encoded))
		request.Header.Set("Content-Encoding", "zstd")
		actual, err := ReadRequestBody(request)
		require.NoError(t, err)
		expected := body
		if len(encoded) == 2*len(frame) {
			expected = bytes.Repeat(body, 2)
		}
		require.Equal(t, expected, actual)
		require.Empty(t, request.Header.Get("Content-Encoding"))
	}
}

func TestZstdWholeBufferDecodedLimitUnknownSizeAndCumulative(t *testing.T) {
	// Unknown FCS must remain bounded, not just frames announcing their size.
	tooLarge := encodeStreamingZstd(t, bytes.Repeat([]byte("x"), httpRequestBodyReadLimit+1), nil)
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, encoder.Close()) })
	part := encoder.EncodeAll(bytes.Repeat([]byte("y"), 20<<20), nil)
	for name, encoded := range map[string][]byte{
		"unknown_size":      tooLarge,
		"cumulative_frames": append(bytes.Clone(part), part...),
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(encoded))
			request.Header.Set("Content-Encoding", "zstd")
			actual, err := ReadRequestBody(request)
			require.ErrorIs(t, err, ErrHTTPRequestBodyTooLarge)
			require.Nil(t, actual)
			require.Equal(t, "zstd", request.Header.Get("Content-Encoding"))
		})
	}
}

func TestZstdWholeBufferSmallOutputLimitRetainsWindowAllowance(t *testing.T) {
	// A large advertised window is valid when the actual output fits the limit.
	frame := encodeStreamingZstd(t, []byte("small"), nil)
	actual, err := decodeZstdRequestBody(frame, 5)
	require.NoError(t, err)
	require.Equal(t, []byte("small"), actual)
	_, err = decodeZstdRequestBody(frame, 4)
	require.ErrorIs(t, err, ErrHTTPRequestBodyTooLarge)
}

func TestDictionaryWholeBufferUnknownSizeAndIsolation(t *testing.T) {
	for _, dictionary := range [][]byte{[]byte("first synthetic scope"), []byte("second synthetic scope")} {
		body := bytes.Repeat(dictionary, 8192)
		frame := encodeStreamingZstd(t, body, dictionary)
		hash := sha256.Sum256(dictionary)
		encoded := append(bytes.Clone(requestDictionaryMagic[:]), hash[:]...)
		encoded = append(encoded, frame...)
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(encoded))
		request.Header.Set("Content-Encoding", "dcz")
		actual, err := ReadRequestBodyWithDictionary(request, func(key [sha256.Size]byte) ([]byte, bool) {
			return dictionary, key == hash
		})
		require.NoError(t, err)
		require.Equal(t, body, actual)
		_, err = decodeDictionaryRequest(encoded, func([sha256.Size]byte) ([]byte, bool) {
			return nil, false
		})
		require.True(t, errors.Is(err, ErrRequestDictionaryMissing))
	}
}
