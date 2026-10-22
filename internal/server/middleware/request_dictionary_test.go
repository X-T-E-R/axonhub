package middleware

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestRequestDictionaryRoundTripAndPreInferenceMiss(t *testing.T) {
	transport := NewRequestBodyTransport()
	calls := 0
	var received []byte
	router := gin.New()
	router.Use(func(c *gin.Context) {
		ctx := contexts.WithProjectID(c.Request.Context(), 1)
		ctx = contexts.WithAPIKey(ctx, &ent.APIKey{ID: 2})
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}, WithDecodedRequestBody(transport))
	router.POST("/v1/messages", func(c *gin.Context) {
		calls++
		require.Empty(t, c.GetHeader("X-Dict-Store"))
		req, err := httpclient.ReadHTTPRequest(c.Request)
		require.NoError(t, err)
		received = req.Body
		c.JSON(http.StatusBadRequest, gin.H{"error": "synthetic application error"})
	})
	base := []byte(`{"model":"example","messages":[{"role":"user","content":"hello"}],"opaque":9007199254740993}`)
	var seed bytes.Buffer
	gzipWriter := gzip.NewWriter(&seed)
	_, err := gzipWriter.Write(base)
	require.NoError(t, err)
	require.NoError(t, gzipWriter.Close())
	invoke := func(body []byte, encoding string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", encoding)
		req.Header.Set("X-Dict-Store", "1")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	response := invoke(seed.Bytes(), "gzip")
	require.Equal(t, http.StatusBadRequest, response.Code)
	hash := sha256.Sum256(base)
	require.Equal(t, hex.EncodeToString(hash[:]), response.Header().Get("X-Dict-Stored"))
	require.Empty(t, response.Header().Get("X-Portway-Decode-Error"))
	require.Equal(t, base, received)

	next := []byte(`{"model":"example","messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"thinking","signature":"opaque-synthetic","thinking":"synthetic"}]}],"opaque":9007199254740993}`)
	writer, err := zstd.NewWriter(nil, zstd.WithEncoderDictRaw(0, base), zstd.WithEncoderCRC(true))
	require.NoError(t, err)
	defer func() { require.NoError(t, writer.Close()) }()
	encoded := []byte{0x5e, 0x2a, 0x4d, 0x18, 0x20, 0, 0, 0}
	encoded = append(encoded, hash[:]...)
	encoded = append(encoded, writer.EncodeAll(next, nil)...)
	response = invoke(encoded, "dcz")
	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Equal(t, next, received)
	require.Equal(t, 2, calls)
	require.Empty(t, response.Header().Get("X-Portway-Decode-Error"))
	require.Empty(t, transport.decodeSlots)

	transport.mu.Lock()
	transport.entries = make(map[requestDictionaryKey]requestDictionaryEntry)
	transport.bytes = 0
	transport.mu.Unlock()
	response = invoke(encoded, "dcz")
	require.Equal(t, http.StatusPreconditionFailed, response.Code)
	require.Equal(t, "1", response.Header().Get("X-Dict-Miss"))
	require.Equal(t, "1", response.Header().Get("X-Portway-Decode-Error"))
	require.Empty(t, response.Header().Get("X-Dict-Stored"))
	require.Equal(t, 2, calls)
}

func TestRequestDictionaryRejectsMultipleContentEncodings(t *testing.T) {
	transport := NewRequestBodyTransport()
	base := []byte(`{"model":"synthetic","messages":[]}`)
	hash := sha256.Sum256(base)
	require.NotEmpty(t, transport.retain(requestDictionaryScope{project: 1, key: 2}, base))
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderDictRaw(0, base), zstd.WithEncoderCRC(true))
	require.NoError(t, err)
	defer func() { require.NoError(t, encoder.Close()) }()
	dcz := append([]byte{0x5e, 0x2a, 0x4d, 0x18, 0x20, 0, 0, 0}, hash[:]...)
	dcz = append(dcz, encoder.EncodeAll(base, nil)...)
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		for _, encodings := range [][]string{
			{"dcz", "gzip"}, {"gzip", "zstd"}, {"identity", "gzip"},
			{"identity", "identity"}, {"", "gzip"}, {"gzip, zstd"}, {"dcz, gzip"},
		} {
			t.Run(path+"/"+encodings[0], func(t *testing.T) {
				called := false
				router := gin.New()
				router.Use(func(c *gin.Context) {
					ctx := contexts.WithProjectID(c.Request.Context(), 1)
					c.Request = c.Request.WithContext(contexts.WithAPIKey(ctx, &ent.APIKey{ID: 2}))
					c.Next()
				}, WithDecodedRequestBody(transport))
				router.POST(path, func(c *gin.Context) { called = true; c.Status(http.StatusOK) })
				request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(dcz))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-Dict-Store", "1")
				for _, encoding := range encodings {
					request.Header.Add("Content-Encoding", encoding)
				}
				response := httptest.NewRecorder()
				router.ServeHTTP(response, request)
				require.Equal(t, http.StatusUnsupportedMediaType, response.Code)
				require.Equal(t, "1", response.Header().Get("X-Portway-Decode-Error"))
				require.Empty(t, response.Header().Get("X-Dict-Stored"))
				require.False(t, called)
			})
		}
	}
}

func TestRequestDictionaryScopeExpiryAndBudgets(t *testing.T) {
	transport := NewRequestBodyTransport()
	transport.maxEntryBytes = 8
	transport.maxBytes = 12
	transport.maxScopeBytes = 8
	transport.maxScopeEntries = 2
	now := time.Unix(1000, 0)
	transport.now = func() time.Time { return now }
	scope := requestDictionaryScope{project: 1, key: 1}
	otherKey := requestDictionaryScope{project: 1, key: 2}
	otherProject := requestDictionaryScope{project: 2, key: 1}
	body := []byte("1234")
	hash := sha256.Sum256(body)
	require.NotEmpty(t, transport.retain(scope, body))
	body[0] = 'x'
	stored, ok := transport.lookup(scope, hash)
	require.True(t, ok)
	require.Equal(t, []byte("1234"), stored)
	_, ok = transport.lookup(otherKey, hash)
	require.False(t, ok)
	_, ok = transport.lookup(otherProject, hash)
	require.False(t, ok)
	require.Empty(t, transport.retain(scope, []byte("too-large")))
	require.NotEmpty(t, transport.retain(scope, []byte("5678")))
	require.NotEmpty(t, transport.retain(scope, []byte("abcd")))
	_, ok = transport.lookup(scope, hash)
	require.False(t, ok)
	require.NotEmpty(t, transport.retain(otherKey, []byte("efgh")))
	require.NotEmpty(t, transport.retain(otherProject, []byte("ijkl")))
	require.LessOrEqual(t, transport.bytes, transport.maxBytes)
	now = now.Add(transport.ttl)
	_, ok = transport.lookup(otherProject, sha256.Sum256([]byte("ijkl")))
	require.False(t, ok)
}

func TestRequestDictionaryConcurrentIsolation(t *testing.T) {
	transport := NewRequestBodyTransport()
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("concurrent dictionary panic: %v", recovered)
				}
			}()
			scope := requestDictionaryScope{project: 1, key: i + 1}
			body := []byte{byte(i), 1, 2, 3}
			transport.retain(scope, body)
			_, _ = transport.lookup(scope, sha256.Sum256(body))
		}()
	}
	wg.Wait()
	require.LessOrEqual(t, transport.bytes, transport.maxBytes)
}
