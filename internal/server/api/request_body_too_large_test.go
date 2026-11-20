package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/internal/tracing"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/gemini"
	"github.com/looplj/axonhub/llm/transformer/jina"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func newMinimalChatHandlers(inbound transformer.Inbound) *ChatCompletionHandlers {
	return &ChatCompletionHandlers{
		ChatCompletionOrchestrator: &orchestrator.ChatCompletionOrchestrator{
			Inbound: inbound,
		},
	}
}

func TestRequestBodyTooLarge_UncompressedEndpointsReturn413(t *testing.T) {
	gin.SetMode(gin.TestMode)

	oversized := bytes.Repeat([]byte("x"), (32<<20)+1)

	anthropicHandlers := &AnthropicHandlers{
		ChatCompletionHandlers: newMinimalChatHandlers(anthropic.NewInboundTransformer()),
		ThreadStore:            NewAnthropicThreadStore(),
	}
	openaiChatHandlers := newMinimalChatHandlers(openai.NewInboundTransformer())
	openaiResponsesHandlers := newMinimalChatHandlers(responses.NewInboundTransformer())
	geminiHandlers := newMinimalChatHandlers(gemini.NewInboundTransformer())
	jinaRerankHandlers := newMinimalChatHandlers(jina.NewRerankInboundTransformer())
	playgroundHandlers := &PlaygroundHandlers{
		ChannelService: &biz.ChannelService{},
	}

	t.Run("POST /v1/messages without WithTrace body inspection returns 413 request_too_large", func(t *testing.T) {
		for _, encoding := range []string{"", "identity"} {
			t.Run("encoding="+encoding, func(t *testing.T) {
				router := gin.New()
				router.Use(middleware.WithDecodedRequestBody(), middleware.WithTrace(tracing.Config{}, nil))
				router.POST("/v1/messages", anthropicHandlers.CreateMessage)

				req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(oversized))
				req.Header.Set("Content-Type", "application/json")
				if encoding != "" {
					req.Header.Set("Content-Encoding", encoding)
				}

				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)

				require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
				require.Equal(t, "error", gjson.GetBytes(w.Body.Bytes(), "type").String())
				require.Equal(t, "request_too_large", gjson.GetBytes(w.Body.Bytes(), "error.type").String())
				require.Contains(t, gjson.GetBytes(w.Body.Bytes(), "error.message").String(), "http request body exceeds size limit")
			})
		}
	})

	t.Run("POST /v1/messages with WithTrace body inspection returns 413", func(t *testing.T) {
		for _, cfg := range []struct {
			name string
			cfg  tracing.Config
		}{
			{
				name: "claude_code_trace",
				cfg:  tracing.Config{ClaudeCodeTraceEnabled: true},
			},
			{
				name: "extra_trace_body_fields",
				cfg:  tracing.Config{ExtraTraceBodyFields: []string{"metadata.trace_id"}},
			},
		} {
			t.Run(cfg.name, func(t *testing.T) {
				router := gin.New()
				router.Use(middleware.WithDecodedRequestBody(), middleware.WithTrace(cfg.cfg, nil))
				router.POST("/v1/messages", anthropicHandlers.CreateMessage)

				req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(oversized))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Content-Encoding", "identity")

				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)

				require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
				require.Contains(t, w.Body.String(), "http request body exceeds size limit")
			})
		}
	})

	t.Run("POST /v1/messages/count_tokens returns 413 request_too_large", func(t *testing.T) {
		router := gin.New()
		router.Use(middleware.WithDecodedRequestBody(), middleware.WithTrace(tracing.Config{}, nil))
		router.POST("/v1/messages/count_tokens", anthropicHandlers.CountTokens)

		req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", bytes.NewReader(oversized))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		require.Equal(t, "error", gjson.GetBytes(w.Body.Bytes(), "type").String())
		require.Equal(t, "request_too_large", gjson.GetBytes(w.Body.Bytes(), "error.type").String())
		require.Contains(t, gjson.GetBytes(w.Body.Bytes(), "error.message").String(), "http request body exceeds size limit")
	})

	t.Run("POST /v1/chat/completions returns 413 request_body_too_large", func(t *testing.T) {
		router := gin.New()
		router.Use(middleware.WithDecodedRequestBody(), middleware.WithTrace(tracing.Config{}, nil))
		router.POST("/v1/chat/completions", openaiChatHandlers.ChatCompletion)

		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(oversized))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Content-Encoding", "identity")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		require.Equal(t, "invalid_request_error", gjson.GetBytes(w.Body.Bytes(), "error.type").String())
		require.Equal(t, "request_body_too_large", gjson.GetBytes(w.Body.Bytes(), "error.code").String())
		require.Contains(t, gjson.GetBytes(w.Body.Bytes(), "error.message").String(), "http request body exceeds size limit")
	})

	t.Run("POST /v1/responses returns 413 request_body_too_large", func(t *testing.T) {
		router := gin.New()
		router.Use(middleware.WithDecodedRequestBody(), middleware.WithTrace(tracing.Config{}, nil))
		router.POST("/v1/responses", openaiResponsesHandlers.ChatCompletion)

		req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(oversized))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		require.Equal(t, "invalid_request_error", gjson.GetBytes(w.Body.Bytes(), "error.type").String())
		require.Equal(t, "request_body_too_large", gjson.GetBytes(w.Body.Bytes(), "error.code").String())
	})

	t.Run("POST /v1beta/models/*action returns 413 INVALID_ARGUMENT", func(t *testing.T) {
		router := gin.New()
		router.Use(middleware.WithDecodedRequestBody(), middleware.WithTrace(tracing.Config{}, nil))
		router.POST("/v1beta/models/*action", geminiHandlers.ChatCompletion)

		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", bytes.NewReader(oversized))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		require.Equal(t, int64(http.StatusRequestEntityTooLarge), gjson.GetBytes(w.Body.Bytes(), "error.code").Int())
		require.Equal(t, "INVALID_ARGUMENT", gjson.GetBytes(w.Body.Bytes(), "error.status").String())
	})

	t.Run("POST /v1/rerank returns 413 request_body_too_large", func(t *testing.T) {
		router := gin.New()
		router.Use(middleware.WithDecodedRequestBody(), middleware.WithTrace(tracing.Config{}, nil))
		router.POST("/v1/rerank", jinaRerankHandlers.ChatCompletion)

		req := httptest.NewRequest(http.MethodPost, "/v1/rerank", bytes.NewReader(oversized))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		require.Equal(t, "invalid_request_error", gjson.GetBytes(w.Body.Bytes(), "error.type").String())
		require.Equal(t, "request_body_too_large", gjson.GetBytes(w.Body.Bytes(), "error.code").String())
	})

	t.Run("POST /admin/playground/chat returns 413", func(t *testing.T) {
		router := gin.New()
		router.POST("/admin/playground/chat", playgroundHandlers.ChatCompletion)

		req := httptest.NewRequest(http.MethodPost, "/admin/playground/chat", bytes.NewReader(oversized))
		req.Header.Set("Content-Type", "application/json")

		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)

		require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
		require.Equal(t, int64(http.StatusRequestEntityTooLarge), gjson.GetBytes(w.Body.Bytes(), "error.code").Int())
		require.Contains(t, gjson.GetBytes(w.Body.Bytes(), "error.message").String(), "http request body exceeds size limit")
	})

	t.Run("MaxBytesReader on CountTokens and Playground returns 413", func(t *testing.T) {
		router := gin.New()
		router.Use(func(c *gin.Context) {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16)
			c.Next()
		})
		router.POST("/v1/messages/count_tokens", anthropicHandlers.CountTokens)
		router.POST("/admin/playground/chat", playgroundHandlers.ChatCompletion)

		for _, tc := range []struct {
			path string
			key  string
			want string
		}{
			{"/v1/messages/count_tokens", "error.type", "request_too_large"},
			{"/admin/playground/chat", "error.code", "413"},
		} {
			req := httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(bytes.Repeat([]byte("z"), 64)))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
			require.Equal(t, tc.want, gjson.GetBytes(w.Body.Bytes(), tc.key).String())
		}
	})
}
