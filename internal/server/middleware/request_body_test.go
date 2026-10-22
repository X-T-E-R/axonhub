package middleware

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/tracing"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestCompressedRequestBodyBeforeTrace(t *testing.T) {
	payload := []byte(`{"metadata":{"trace":"compressed-trace"},"messages":[{"role":"user","content":"hello"}],"opaque":9007199254740993}`)
	for _, encoding := range []string{"gzip", "zstd"} {
		t.Run(encoding, func(t *testing.T) {
			var compressed []byte
			if encoding == "gzip" {
				var buf bytes.Buffer
				writer := gzip.NewWriter(&buf)
				_, err := writer.Write(payload)
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				compressed = buf.Bytes()
			} else {
				writer, err := zstd.NewWriter(nil)
				require.NoError(t, err)
				compressed = writer.EncodeAll(payload, nil)
				require.NoError(t, writer.Close())
			}
			router := gin.New()
			router.Use(WithDecodedRequestBody(), WithTrace(tracing.Config{ExtraTraceBodyFields: []string{"metadata.trace"}}, nil))
			var traceID string
			var captured []byte
			router.POST("/v1/messages", func(c *gin.Context) {
				require.Empty(t, c.GetHeader("Content-Encoding"))
				require.Empty(t, c.GetHeader("Content-Length"))
				require.Equal(t, int64(len(payload)), c.Request.ContentLength)
				traceID, _ = tracing.GetTraceID(c.Request.Context())
				req, err := httpclient.ReadHTTPRequest(c.Request)
				require.NoError(t, err)
				captured = req.Body
				c.Status(http.StatusOK)
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(compressed))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Content-Encoding", encoding)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, req)
			require.Equal(t, http.StatusOK, response.Code)
			require.Equal(t, payload, captured)
			require.Equal(t, "compressed-trace", traceID)
		})
	}
}

func TestDecodedRequestBodyRejectsBeforeHandler(t *testing.T) {
	var bomb bytes.Buffer
	writer := gzip.NewWriter(&bomb)
	_, err := writer.Write(bytes.Repeat([]byte("a"), (32<<20)+1))
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	for _, tc := range []struct {
		name     string
		encoding string
		body     []byte
		status   int
	}{
		{"invalid gzip", "gzip", []byte("not gzip"), http.StatusBadRequest},
		{"invalid zstd", "zstd", []byte("not zstd"), http.StatusBadRequest},
		{"unknown encoding", "unsupported", []byte("{}"), http.StatusBadRequest},
		{"decoded limit", "gzip", bomb.Bytes(), http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.Use(WithDecodedRequestBody())
			called := false
			router.POST("/v1/messages", func(c *gin.Context) {
				called = true
				c.Status(http.StatusOK)
			})
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(tc.body))
			req.Header.Set("Content-Encoding", tc.encoding)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			require.Equal(t, tc.status, recorder.Code)
			require.False(t, called)
		})
	}
}

func TestDecodedRequestBodyPreservesUnencodedBody(t *testing.T) {
	for _, encoding := range []string{"", "identity"} {
		t.Run(encoding, func(t *testing.T) {
			payload := []byte("\x00binary and multipart bytes\xff")
			req := httptest.NewRequest(http.MethodPost, "/binary", bytes.NewReader(payload))
			req.Header.Set("Content-Encoding", encoding)
			original := req.Body
			router := gin.New()
			router.Use(WithDecodedRequestBody())
			router.POST("/binary", func(c *gin.Context) {
				require.True(t, original == c.Request.Body)
				captured, err := httpclient.ReadRequestBody(c.Request)
				require.NoError(t, err)
				require.Equal(t, payload, captured)
				c.Status(http.StatusOK)
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, req)
			require.Equal(t, http.StatusOK, recorder.Code)
		})
	}
}
