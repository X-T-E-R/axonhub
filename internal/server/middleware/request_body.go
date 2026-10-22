package middleware

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/llm/httpclient"
)

// WithDecodedRequestBody normalizes encoded requests before body-dependent tracing.
// Dictionary support is opt-in and requires an authenticated request scope.
func WithDecodedRequestBody(transports ...*RequestBodyTransport) gin.HandlerFunc {
	var transport *RequestBodyTransport
	if len(transports) > 0 {
		transport = transports[0]
	}
	return func(c *gin.Context) {
		encoding, encodingErr := httpclient.RequestContentEncoding(c.Request.Header)
		retain := c.GetHeader("X-Dict-Store") == "1"
		stripDictionaryRequestHeaders(c.Request.Header)
		if encodingErr != nil {
			abortRequestDecoding(c, http.StatusUnsupportedMediaType, "request_encoding_unsupported", "Multiple request content encodings are unsupported")
			return
		}
		if encoding == "" || encoding == "identity" {
			c.Next()
			return
		}
		switch encoding {
		case "gzip", "x-gzip", "zstd", "deflate", "dcz":
		default:
			abortRequestDecoding(c, http.StatusUnsupportedMediaType, "request_encoding_unsupported", "Request content encoding is unsupported")
			return
		}

		scope, scoped := dictionaryRequestScope(c)
		eligible := transport != nil && scoped && dictionaryRequestEligible(c.Request)
		var lookup httpclient.RequestDictionaryLookup
		if encoding == "dcz" {
			if !eligible {
				abortRequestDecoding(c, http.StatusUnsupportedMediaType, "request_dictionary_unsupported", "Request dictionaries are unavailable for this endpoint")
				return
			}
			lookup = func(hash [sha256.Size]byte) ([]byte, bool) { return transport.lookup(scope, hash) }
		}

		original := c.Request.Body
		busy := false
		body, err := func() ([]byte, error) {
			if encoding == "dcz" {
				select {
				case transport.decodeSlots <- struct{}{}:
					defer func() { <-transport.decodeSlots }()
				default:
					busy = true
					return nil, errors.New("Request decompression capacity is busy")
				}
			}
			return httpclient.ReadRequestBodyWithDictionary(c.Request, lookup)
		}()
		if busy {
			c.Header("Retry-After", "1")
			AbortWithError(c, http.StatusServiceUnavailable, err)
			return
		}
		if original != nil {
			_ = original.Close()
		}
		if err != nil {
			status, code := http.StatusBadRequest, "request_decode_failed"
			switch {
			case errors.Is(err, httpclient.ErrHTTPRequestBodyTooLarge):
				status, code = http.StatusRequestEntityTooLarge, "request_body_too_large"
			case errors.Is(err, httpclient.ErrRequestDictionaryMissing):
				status, code = http.StatusPreconditionFailed, "request_dictionary_missing"
				c.Header("X-Dict-Miss", "1")
			case errors.Is(err, httpclient.ErrRequestDictionaryUnsupported):
				status, code = http.StatusUnsupportedMediaType, "request_dictionary_unsupported"
			}
			abortRequestDecoding(c, status, code, "Request content decoding was rejected before execution")
			return
		}

		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Request.ContentLength = int64(len(body))
		c.Request.Header.Del("Content-Encoding")
		c.Request.Header.Del("Content-Length")
		if retain && eligible {
			if hash := transport.retain(scope, body); hash != "" {
				c.Header("X-Dict-Stored", hash)
			}
		}
		c.Next()
	}
}

func abortRequestDecoding(c *gin.Context, status int, code, message string) {
	c.Header("X-Portway-Decode-Error", "1")
	if status == http.StatusUnsupportedMediaType {
		c.Header("Accept-Encoding", "zstd, gzip")
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"type": "invalid_request_error", "code": code, "message": message}})
}

func stripDictionaryRequestHeaders(headers http.Header) {
	for _, name := range []string{"X-Dict-Store", "X-Dict-Stored", "X-Dict-Miss", "X-Portway-Decode-Error", "X-Request-Encodings", "X-Request-Dictionary"} {
		headers.Del(name)
	}
}

func dictionaryRequestScope(c *gin.Context) (requestDictionaryScope, bool) {
	key, ok := contexts.GetAPIKey(c.Request.Context())
	if !ok || key.ID <= 0 {
		return requestDictionaryScope{}, false
	}
	project, ok := contexts.GetProjectID(c.Request.Context())
	if !ok || project <= 0 {
		return requestDictionaryScope{}, false
	}
	return requestDictionaryScope{project: project, key: key.ID}, true
}

func dictionaryRequestEligible(request *http.Request) bool {
	if request.Method != http.MethodPost {
		return false
	}
	contentType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return false
	}
	path := request.URL.Path
	return path == "/v1/messages" || path == "/anthropic/v1/messages" || path == "/v1/chat/completions" || path == "/v1/responses" ||
		((strings.HasPrefix(path, "/gemini/") || strings.HasPrefix(path, "/v1beta/")) &&
			(strings.HasSuffix(path, ":generateContent") || strings.HasSuffix(path, ":streamGenerateContent")))
}

// Capabilities advertises transport support without inspecting credentials or generating model output.
func (t *RequestBodyTransport) Capabilities(c *gin.Context) {
	c.Header("X-Request-Encodings", "zstd, gzip")
	c.Header("X-Request-Dictionary", "dcz")
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{
		"request_encodings":        []string{"zstd", "gzip"},
		"request_dictionary":       "dcz",
		"max_body_bytes":           32 << 20,
		"max_dictionary_bytes":     t.maxEntryBytes,
		"dictionary_ttl_seconds":   int(t.ttl.Seconds()),
		"dictionary_storage":       "memory",
		"dictionary_requires_auth": true,
	})
}
