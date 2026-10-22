package httpclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func TestNodeRawZstdDictionaryInterop(t *testing.T) {
	dictionary := []byte("synthetic shared dictionary: " + strings.Repeat("tool_schema_and_history_", 64))
	compressed, err := base64.StdEncoding.DecodeString("KLUv/WR3BZ0CAJLEEBmAtxoDXNgoHaDNIPnjh8SkJMn67WigaBJG8YKuy0WmOegYROaAjMlHUAN2QeKn3VK3WHK56b7TGmBteKRXAWpXBcMDEAqQvPZjgK7hkf8TaoseBg==")
	require.NoError(t, err)
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderDictRaw(0, dictionary), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(64<<20), zstd.WithDecoderMaxWindow(32<<20))
	require.NoError(t, err)
	defer decoder.Close()
	decoded, err := decoder.DecodeAll(compressed, nil)
	require.NoError(t, err)
	expected := []byte(fmt.Sprintf(`{"messages":[{"role":"user","content":%q}],"opaque":"9007199254740993","next":"synthetic"}`, dictionary))
	require.Equal(t, expected, decoded)
}

func TestReadRequestBodyWithDictionary(t *testing.T) {
	dictionary := []byte(strings.Repeat("synthetic dictionary ", 64))
	body := append(bytes.Clone(dictionary), []byte(`{"opaque":9007199254740993}`)...)
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderDictRaw(0, dictionary), zstd.WithEncoderCRC(true))
	require.NoError(t, err)
	defer func() { require.NoError(t, encoder.Close()) }()
	frame := encoder.EncodeAll(body, nil)
	hash := sha256.Sum256(dictionary)
	encoded := append(bytes.Clone(requestDictionaryMagic[:]), hash[:]...)
	encoded = append(encoded, frame...)
	lookup := func(key [sha256.Size]byte) ([]byte, bool) {
		return dictionary, key == hash
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(encoded))
	request.Header.Set("Content-Encoding", "dcz")
	decoded, err := ReadRequestBodyWithDictionary(request, lookup)
	require.NoError(t, err)
	require.Equal(t, body, decoded)
	require.Empty(t, request.Header.Get("Content-Encoding"))
}

func TestRequestContentEncodingRejectsMultipleFields(t *testing.T) {
	for _, encodings := range [][]string{
		{"dcz", "gzip"}, {"gzip", "zstd"}, {"identity", "gzip"},
		{"identity", "identity"}, {"", "gzip"}, {"gzip, zstd"}, {"dcz, gzip"},
	} {
		t.Run(strings.Join(encodings, "/"), func(t *testing.T) {
			body := bytes.NewReader([]byte("unread synthetic body"))
			request := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
			for _, encoding := range encodings {
				request.Header.Add("Content-Encoding", encoding)
			}
			lookups := 0
			_, err := ReadRequestBodyWithDictionary(request, func([sha256.Size]byte) ([]byte, bool) {
				lookups++
				return nil, false
			})
			require.Error(t, err)
			require.Equal(t, int(request.ContentLength), body.Len())
			require.Zero(t, lookups)
			_, err = decodeRequestBody([]byte("{}"), request.Header)
			require.Error(t, err)
		})
	}
	for _, encoding := range []string{"", "identity", " GZip ", "zstd", "dcz"} {
		header := make(http.Header)
		header.Set("Content-Encoding", encoding)
		actual, err := RequestContentEncoding(header)
		require.NoError(t, err)
		require.Equal(t, strings.ToLower(strings.TrimSpace(encoding)), actual)
	}
}

func TestDictionaryRequestRejectsMalformedFrames(t *testing.T) {
	dictionary := []byte(strings.Repeat("synthetic dictionary ", 64))
	hash := sha256.Sum256(dictionary)
	lookup := func(key [sha256.Size]byte) ([]byte, bool) { return dictionary, key == hash }
	wrap := func(frame []byte) []byte {
		body := append(bytes.Clone(requestDictionaryMagic[:]), hash[:]...)
		return append(body, frame...)
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderDictRaw(0, dictionary), zstd.WithEncoderCRC(true))
	require.NoError(t, err)
	defer func() { require.NoError(t, encoder.Close()) }()
	frame := encoder.EncodeAll(append(bytes.Clone(dictionary), 'x'), nil)
	noCRC, err := zstd.NewWriter(nil, zstd.WithEncoderDictRaw(0, dictionary), zstd.WithEncoderCRC(false))
	require.NoError(t, err)
	defer func() { require.NoError(t, noCRC.Close()) }()
	corrupt := bytes.Clone(frame)
	corrupt[len(corrupt)-1] ^= 1
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"truncated magic", []byte("dcz")},
		{"truncated frame", wrap(frame[:len(frame)-1])},
		{"checksum mismatch", wrap(corrupt)},
		{"trailing frame", wrap(append(bytes.Clone(frame), frame...))},
		{"trailing byte", wrap(append(bytes.Clone(frame), 0))},
		{"missing checksum", wrap(noCRC.EncodeAll(dictionary, nil))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeDictionaryRequest(tc.body, lookup)
			require.ErrorIs(t, err, ErrInvalidDictionaryRequest)
		})
	}
	_, err = decodeDictionaryRequest(wrap(frame), nil)
	require.ErrorIs(t, err, ErrRequestDictionaryUnsupported)
	_, err = decodeDictionaryRequest(wrap(frame), func([sha256.Size]byte) ([]byte, bool) { return nil, false })
	require.ErrorIs(t, err, ErrRequestDictionaryMissing)
	_, err = decodeDictionaryRequest(wrap(frame), func([sha256.Size]byte) ([]byte, bool) { return []byte("wrong dictionary"), true })
	require.ErrorIs(t, err, ErrInvalidDictionaryRequest)
}
