package api

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

func TestResponsesWebSocketLifecycleDrainAndRebase(t *testing.T) {
	var calls, authCalls atomic.Int32
	h := wsTestHandler(func(ctx context.Context, _ *httpclient.Request, observer orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
		n := calls.Add(1)
		observer.OnUpstreamAttemptSelected(orchestrator.StreamLivenessAttempt{})
		if n == 1 {
			return orchestrator.ChatCompletionResult{ChatCompletionStream: streams.SliceStream([]*httpclient.StreamEvent{wsCompletion("warm", `[]`)})}, nil
		}
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.Greater(t, time.Until(deadline), 900*time.Millisecond, "socket age must not shorten the active turn")
		return orchestrator.ChatCompletionResult{ChatCompletionStream: &lifecycleDelayedStream{ctx: ctx, delay: 250 * time.Millisecond, terminal: wsCompletion("finished", `[]`)}}, nil
	})
	h.authenticate = func(c *gin.Context) (context.Context, int, error) {
		authCalls.Add(1)
		return c.Request.Context(), http.StatusOK, nil
	}
	h.lifetime, h.turnTimeout = 200*time.Millisecond, time.Second
	server := wsTestServer(t, h)
	conn := wsTestDial(t, server)
	send := func(lane string) {
		require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": "synthetic", "stream_id": lane}))
	}
	send("warm")
	require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
	time.Sleep(100 * time.Millisecond)
	send("active")
	require.Equal(t, "response.created", wsEventString(wsRead(t, conn), "type"))
	for _, lane := range []string{"queued-1", "", "queued-3"} {
		send(lane)
	}
	// The generation remains active beyond the admission cutoff, while later
	// frames are rejected immediately without waiting for that generation.
	time.Sleep(130 * time.Millisecond)
	send("late")
	late := wsRead(t, conn)
	require.Equal(t, "connection_retiring", wsErrorCode(late))
	require.Equal(t, "late", wsEventString(late, "stream_id"))
	require.Equal(t, "not_executed", wsEventString(late, "execution"))
	completed := wsRead(t, conn)
	require.Equal(t, "response.completed", wsEventString(completed, "type"))
	require.Equal(t, "active", wsEventString(completed, "stream_id"))
	for _, lane := range []string{"queued-1", "", "queued-3"} {
		rejected := wsRead(t, conn)
		require.Equal(t, "connection_retiring", wsErrorCode(rejected))
		require.Equal(t, "not_executed", wsEventString(rejected, "execution"))
		require.Equal(t, lane, wsEventString(rejected, "stream_id"))
		if lane == "" {
			require.NotContains(t, rejected, "stream_id")
		}
	}
	_, _, err := conn.ReadMessage()
	require.True(t, websocket.IsCloseError(err, websocket.CloseNormalClosure), "%v", err)
	require.Equal(t, int32(2), calls.Load())
	require.Equal(t, int32(2), authCalls.Load(), "retired queued frames must not reach authentication or inference")
	require.Eventually(t, func() bool { return len(h.slots) == 0 }, time.Second, time.Millisecond)

	fresh := wsTestDial(t, server)
	require.NoError(t, fresh.WriteJSON(map[string]any{"type": "response.create", "model": "m", "previous_response_id": "finished", "input": "delta"}))
	rejected := wsRead(t, fresh)
	require.Equal(t, "previous_response_not_found", wsErrorCode(rejected))
	require.Equal(t, "not_executed", wsEventString(rejected, "execution"))
	require.Equal(t, int32(2), calls.Load(), "a retired connection does not transfer its baseline")
}

func TestResponsesWebSocketLifecycleTimeoutAndDisconnect(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		name := "original_turn_deadline"
		if disconnect {
			name = "disconnect_while_retiring"
		}
		t.Run(name, func(t *testing.T) {
			canceled := make(chan struct{})
			h := wsTestHandler(func(ctx context.Context, _ *httpclient.Request, observer orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
				observer.OnUpstreamAttemptSelected(orchestrator.StreamLivenessAttempt{})
				return orchestrator.ChatCompletionResult{ChatCompletionStream: &lifecycleDelayedStream{ctx: ctx, delay: time.Second, canceled: canceled, terminal: wsCompletion("never", `[]`)}}, nil
			})
			h.lifetime, h.turnTimeout = 60*time.Millisecond, 180*time.Millisecond
			conn := wsTestDial(t, wsTestServer(t, h))
			require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": "synthetic"}))
			require.Equal(t, "response.created", wsEventString(wsRead(t, conn), "type"))
			time.Sleep(90 * time.Millisecond)
			if disconnect {
				_ = conn.Close()
			} else {
				errorEvent := wsRead(t, conn)
				require.Equal(t, "incomplete_stream", wsErrorCode(errorEvent))
				require.NotContains(t, errorEvent, "execution", "a selected upstream timeout is not safe replay")
				_, _, err := conn.ReadMessage()
				require.True(t, websocket.IsCloseError(err, websocket.CloseNormalClosure), "%v", err)
			}
			select {
			case <-canceled:
			case <-time.After(300 * time.Millisecond):
				t.Fatal("retirement must preserve both cancellation and the original generation timeout")
			}
			require.Eventually(t, func() bool { return len(h.slots) == 0 }, time.Second, time.Millisecond)
		})
	}
}

func TestResponsesWebSocketLifecycleIdleRetirement(t *testing.T) {
	var calls atomic.Int32
	h := wsTestHandler(func(context.Context, *httpclient.Request, orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
		calls.Add(1)
		return orchestrator.ChatCompletionResult{}, errors.New("must not execute")
	})
	h.lifetime = 60 * time.Millisecond
	conn := wsTestDial(t, wsTestServer(t, h))
	_, _, err := conn.ReadMessage()
	require.True(t, websocket.IsCloseError(err, websocket.CloseNormalClosure), "%v", err)
	require.Zero(t, calls.Load())
	require.Eventually(t, func() bool { return len(h.slots) == 0 }, time.Second, time.Millisecond)
}

func TestResponsesWebSocketLifecycleCapabilitiesAndTimeoutDefault(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		active  int64
	}{
		{"zero", 0, 600}, {"negative", -time.Second, 600}, {"unrepresentable_total", time.Duration(math.MaxInt64), 600}, {"fractional_positive", 1501 * time.Millisecond, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := wsTestHandler(nil)
			h.lifetime = 30 * time.Minute
			handlers := &OpenAIHandlers{ResponseWebSocketHandler: h}
			router := gin.New()
			router.GET("/v1/responses", handlers.ResponsesWebSocketWithTimeout(tc.timeout))
			server := httptest.NewServer(router)
			defer server.Close()
			conn, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
			require.NoError(t, err)
			defer conn.Close()
			var capabilities map[string]any
			require.NoError(t, json.Unmarshal([]byte(response.Header.Get(responsesWSCapabilityHeader)), &capabilities))
			require.Equal(t, float64(2), capabilities["version"])
			require.Equal(t, "drain_active", capabilities["lifetime_mode"])
			require.Equal(t, float64(1800), capabilities["admission_lifetime_seconds"])
			require.Equal(t, float64(tc.active), capabilities["active_turn_timeout_seconds"])
			require.Equal(t, float64(1800+tc.active+15), capabilities["connection_lifetime_seconds"])
			require.Equal(t, 5*time.Second, h.turnTimeout, "route registration must not mutate the shared handler")
		})
	}
}

func TestResponsesWebSocketLifecycleHardDeadlineClosesSocket(t *testing.T) {
	// Preserve the real 15s terminal grace; shrink only admission and turn time.
	// A finite synthetic cancellation drain holds the worker until socket close,
	// proving the absolute hard timer does not depend on the worker returning.
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	canceled := make(chan struct{})
	h := wsTestHandler(func(ctx context.Context, _ *httpclient.Request, observer orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
		observer.OnUpstreamAttemptSelected(orchestrator.StreamLivenessAttempt{})
		<-ctx.Done()
		close(canceled)
		timer := time.NewTimer(18 * time.Second)
		defer timer.Stop()
		select {
		case <-release:
		case <-timer.C:
		}
		return orchestrator.ChatCompletionResult{}, ctx.Err()
	})
	h.lifetime, h.turnTimeout = 50*time.Millisecond, 100*time.Millisecond
	conn := wsTestDial(t, wsTestServer(t, h))
	_ = conn.SetReadDeadline(time.Now().Add(20 * time.Second))
	started := time.Now()
	require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": "synthetic"}))
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("the active generation must first honor its own 100ms timeout")
	}
	var event map[string]json.RawMessage
	err := conn.ReadJSON(&event)
	elapsed := time.Since(started)
	releaseOnce.Do(func() { close(release) })
	require.Error(t, err)
	require.Empty(t, event, "a hard socket close must not invent an execution:not_executed event")
	require.GreaterOrEqual(t, elapsed, 16*time.Second)
	require.Less(t, elapsed, 17*time.Second)
	require.Eventually(t, func() bool { return len(h.slots) == 0 }, time.Second, time.Millisecond)
	t.Logf("HARD_BOUND elapsed=%s admission=50ms declared_active=1s terminal_grace=15s", elapsed)
}

func TestResponsesWebSocketLifecycleHardDeadlineBoundsSlowWriter(t *testing.T) {
	finished := make(chan error, 1)
	var canceled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			finished <- err
			return
		}
		defer conn.Close()
		writer := &responsesWSWriter{conn: conn, cancel: func() { canceled.Store(true) }, deadline: time.Now().Add(80 * time.Millisecond)}
		finished <- writer.write(map[string]string{"synthetic": strings.Repeat("x", 8<<20)})
	}))
	defer server.Close()
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err == nil {
			err = conn.(*net.TCPConn).SetReadBuffer(1024)
			if err != nil {
				_ = conn.Close()
			}
		}
		return conn, err
	}}
	conn, _, err := dialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer conn.Close()
	// Deliberately do not read: the tiny TCP receive window blocks a large write.
	select {
	case err := <-finished:
		require.Error(t, err)
		require.True(t, canceled.Load())
	case <-time.After(time.Second):
		t.Fatal("a slow client must not reset or outlive the absolute hard deadline")
	}
}

type lifecycleDelayedStream struct {
	ctx      context.Context
	delay    time.Duration
	terminal *httpclient.StreamEvent
	canceled chan struct{}
	phase    int
}

func (s *lifecycleDelayedStream) Next() bool {
	s.phase++
	if s.phase == 1 {
		return true
	}
	if s.phase != 2 {
		return false
	}
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-s.ctx.Done():
		if s.canceled != nil {
			close(s.canceled)
		}
		return false
	}
}
func (s *lifecycleDelayedStream) Current() *httpclient.StreamEvent {
	if s.phase == 1 {
		return &httpclient.StreamEvent{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"active","status":"in_progress"}}`)}
	}
	return s.terminal
}
func (s *lifecycleDelayedStream) Err() error   { return s.ctx.Err() }
func (s *lifecycleDelayedStream) Close() error { return nil }
