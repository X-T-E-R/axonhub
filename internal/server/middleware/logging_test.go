package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/looplj/axonhub/internal/tracing"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestWithTracing(t *testing.T) {
	// Set Gin to test mode
	gin.SetMode(gin.TestMode)

	// Create a test request and response recorder
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	// Create a Gin engine and router
	engine := gin.New()
	engine.Use(WithLoggingTracing(tracing.Config{
		TraceHeader: "AH-Trace-Id",
	}))

	// Add a dummy handler to complete the middleware chain
	engine.GET("/", func(c *gin.Context) {
		traceID, ok := tracing.GetTraceID(c.Request.Context())
		assert.True(t, ok)
		assert.NotEmpty(t, traceID)
		assert.Contains(t, traceID, "at-")
		c.Status(http.StatusOK)
	})

	// Perform the request
	engine.ServeHTTP(w, req)

	// Verify the response
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestWithTracingExistingHeader(t *testing.T) {
	// Set Gin to test mode
	gin.SetMode(gin.TestMode)

	// Create a test request with an existing trace ID header
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Ah-Trace-Id", "at-existing-trace-id")

	w := httptest.NewRecorder()

	// Create a Gin engine and router
	engine := gin.New()
	engine.Use(WithLoggingTracing(tracing.Config{
		TraceHeader: "AH-Trace-Id",
	}))

	// Add a dummy handler to complete the middleware chain
	engine.GET("/", func(c *gin.Context) {
		traceID, ok := tracing.GetTraceID(c.Request.Context())
		assert.True(t, ok)
		assert.Equal(t, "at-existing-trace-id", traceID)
		c.Status(http.StatusOK)
	})

	// Perform the request
	engine.ServeHTTP(w, req)

	// Verify the response
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestWithTracingCustomHeader(t *testing.T) {
	// Set Gin to test mode
	gin.SetMode(gin.TestMode)

	// Create a test request with a custom trace ID header
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Custom-Trace-Id", "at-custom-trace-id")

	w := httptest.NewRecorder()

	// Create a Gin engine and router
	engine := gin.New()
	engine.Use(WithLoggingTracing(tracing.Config{
		TraceHeader: "X-Custom-Trace-Id",
	}))

	// Add a dummy handler to complete the middleware chain
	engine.GET("/", func(c *gin.Context) {
		traceID, ok := tracing.GetTraceID(c.Request.Context())
		assert.True(t, ok)
		assert.Equal(t, "at-custom-trace-id", traceID)
		c.Status(http.StatusOK)
	})

	// Perform the request
	engine.ServeHTTP(w, req)

	// Verify the response
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestWithTracingEmptyConfig(t *testing.T) {
	// Set Gin to test mode
	gin.SetMode(gin.TestMode)

	// Create a test request and response recorder
	req, _ := http.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()

	// Create a Gin engine and router
	engine := gin.New()
	engine.Use(WithLoggingTracing(tracing.Config{}))

	// Add a dummy handler to complete the middleware chain
	engine.GET("/", func(c *gin.Context) {
		traceID, ok := tracing.GetTraceID(c.Request.Context())
		assert.True(t, ok)
		assert.NotEmpty(t, traceID)
		assert.Contains(t, traceID, "at-")
		c.Status(http.StatusOK)
	})

	// Perform the request
	engine.ServeHTTP(w, req)

	// Verify the response
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestLoggingClientRequestCorrelation(t *testing.T) {
	for _, value := range []string{"ew-ray:abc._-", "", "bad value", strings.Repeat("x", 129), "日本"} {
		t.Run(value, func(t *testing.T) {
			router := gin.New()
			router.Use(WithLoggingTracing(tracing.Config{}))
			router.GET("/", func(c *gin.Context) {
				id, ok := tracing.GetRequestID(c.Request.Context())
				assert.True(t, ok)
				assert.LessOrEqual(t, len(id), 64)
				assert.Regexp(t, `^ar-[A-Za-z0-9._:-]+$`, id)
				want := value
				if !httpclient.ValidClientRequestID(value) {
					want = id
				}
				assert.Equal(t, id, c.Request.Header.Get("Ah-Request-Id"))
				assert.Equal(t, want, c.Request.Header.Get(httpclient.ClientRequestIDHeader))
				assert.Equal(t, want, c.Writer.Header().Get(httpclient.ClientRequestIDHeader))
				c.Status(http.StatusOK)
			})
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("X-Client-Request-Id", value)
			req.Header.Set("Ah-Request-Id", "caller-cannot-set-this")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			assert.Equal(t, http.StatusOK, w.Code)
			assert.NotEmpty(t, w.Header().Get(httpclient.ClientRequestIDHeader))
		})
	}
}
