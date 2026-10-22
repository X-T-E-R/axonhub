package middleware

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/looplj/axonhub/llm/httpclient"
)

// WithDecodedRequestBody normalizes encoded requests before body-dependent tracing.
func WithDecodedRequestBody() gin.HandlerFunc {
	return func(c *gin.Context) {
		encoding := strings.TrimSpace(c.GetHeader("Content-Encoding"))
		if encoding == "" || strings.EqualFold(encoding, "identity") {
			c.Next()
			return
		}

		original := c.Request.Body
		body, err := httpclient.ReadRequestBody(c.Request)
		if original != nil {
			_ = original.Close()
		}
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, httpclient.ErrHTTPRequestBodyTooLarge) {
				status = http.StatusRequestEntityTooLarge
			}
			AbortWithError(c, status, err)
			return
		}

		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		c.Request.ContentLength = int64(len(body))
		c.Request.Header.Del("Content-Encoding")
		c.Request.Header.Del("Content-Length")
		c.Next()
	}
}
