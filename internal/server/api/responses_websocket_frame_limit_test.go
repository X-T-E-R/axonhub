package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestResponsesWebSocketConfiguredFrameLimit(t *testing.T) {
	var calls atomic.Int32
	h := wsTestHandler(func(context.Context, *httpclient.Request, orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
		calls.Add(1)
		return orchestrator.ChatCompletionResult{}, errors.New("oversized frame executed")
	})
	handlers := &OpenAIHandlers{ResponseWebSocketHandler: h}
	router := gin.New()
	router.GET("/v1/responses", handlers.ResponsesWebSocketWithLimits(time.Second, 1024))
	server := httptest.NewServer(router)
	defer server.Close()
	conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	require.NoError(t, err)
	defer conn.Close()
	var capabilities map[string]any
	require.NoError(t, json.Unmarshal([]byte(response.Header.Get(responsesWSCapabilityHeader)), &capabilities))
	require.Equal(t, float64(1024), capabilities["max_frame_bytes"])
	require.Equal(t, float64(32<<20), capabilities["max_state_bytes"])
	require.EqualValues(t, 983040, h.maxFrameBytes, "route registration must not mutate the shared handler")
	_ = conn.WriteMessage(websocket.TextMessage, []byte(strings.Repeat(" ", 1025)))
	_, _, err = conn.ReadMessage()
	require.True(t, websocket.IsCloseError(err, websocket.CloseMessageTooBig), "%v", err)
	require.Zero(t, calls.Load())
}
