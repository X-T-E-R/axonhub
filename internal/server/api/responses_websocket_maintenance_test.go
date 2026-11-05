package api

import (
	"context"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/maintenance"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/shared"
)

func TestResponsesWebSocketMaintenancePauseDrainsAndJoins(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(map[bool]string{false: "finish_active_reject_queued", true: "cancel_active_join"}[disconnect], func(t *testing.T) {
			gate := maintenance.NewGate()
			var calls atomic.Int32
			canceled := make(chan struct{})
			h := wsTestHandler(func(ctx context.Context, _ *httpclient.Request, o orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
				calls.Add(1)
				o.OnUpstreamAttemptSelected(orchestrator.StreamLivenessAttempt{})
				return orchestrator.ChatCompletionResult{ChatCompletionStream: &lifecycleDelayedStream{ctx: ctx, delay: 200 * time.Millisecond, canceled: canceled, terminal: wsCompletion("finished", `[]`)}}, nil
			})
			h.maintenance = gate
			router := gin.New()
			router.Use(gate.Middleware(), func(c *gin.Context) {
				c.Request = c.Request.WithContext(shared.WithSessionScope(c.Request.Context(), "test-scope"))
			})
			router.GET("/v1/responses", h.Serve)
			s := httptest.NewServer(router)
			defer s.Close()
			conn := wsTestDial(t, s)
			send := func(id string) {
				require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": "synthetic", "stream_id": id}))
			}
			send("active")
			require.Equal(t, "response.created", wsEventString(wsRead(t, conn), "type"))
			send("queued")
			gate.Pause()
			require.Equal(t, 1, gate.Snapshot().ActiveTurns)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			require.Error(t, gate.Wait(ctx))
			cancel()
			if disconnect {
				_ = conn.Close()
				select {
				case <-canceled:
				case <-time.After(time.Second):
					t.Fatal("active provider reader was not canceled")
				}
			} else {
				send("late")
				completed, rejected := 0, 0
				for {
					var event map[string]any
					err := conn.ReadJSON(&event)
					if err != nil {
						require.True(t, websocket.IsCloseError(err, websocket.CloseNormalClosure), "%v", err)
						break
					}
					if event["type"] == "response.completed" {
						completed++
					}
					if event["type"] == "error" {
						rejected++
						require.Equal(t, "not_executed", event["execution"])
					}
				}
				require.Equal(t, 1, completed)
				require.Equal(t, 2, rejected)
			}
			require.Eventually(t, func() bool { x := gate.Snapshot(); return x.ActiveHTTP == 0 && x.ActiveTurns == 0 && len(h.slots) == 0 }, time.Second, time.Millisecond)
			require.Equal(t, int32(1), calls.Load())
			require.NoError(t, gate.Wait(context.Background()))
		})
	}
}
