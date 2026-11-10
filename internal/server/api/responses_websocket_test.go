package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/shared"
)

func wsTestHandler(process responsesWSProcessor) *ResponsesWebSocketHandler {
	return &ResponsesWebSocketHandler{process: process, authenticate: func(c *gin.Context) (context.Context, int, error) { return c.Request.Context(), 200, nil }, slots: make(chan struct{}, 4), turnTimeout: time.Second * 5, lifetime: time.Minute, stateTTL: time.Minute}
}
func wsTestServer(t *testing.T, h *ResponsesWebSocketHandler) *httptest.Server {
	t.Helper()
	router := gin.New()
	router.Use(func(c *gin.Context) {
		ctx := shared.WithSessionScope(c.Request.Context(), "test-scope")
		c.Request = c.Request.WithContext(ctx)
	})
	router.GET("/v1/responses", h.Serve)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}
func wsTestDial(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	require.NoError(t, err)
	var capabilities struct {
		Version   int    `json:"version"`
		Lifetime  int64  `json:"connection_lifetime_seconds"`
		Admission int64  `json:"admission_lifetime_seconds"`
		Active    int64  `json:"active_turn_timeout_seconds"`
		Mode      string `json:"lifetime_mode"`
		Frame     int    `json:"max_frame_bytes"`
		Queue     int    `json:"max_queued_bytes"`
		State     int    `json:"max_state_bytes"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Header.Get(http.CanonicalHeaderKey(responsesWSCapabilityHeader))), &capabilities))
	require.Equal(t, 2, capabilities.Version)
	require.Equal(t, "drain_active", capabilities.Mode)
	require.Positive(t, capabilities.Active)
	require.Equal(t, capabilities.Admission+capabilities.Active+15, capabilities.Lifetime)
	require.Equal(t, responsesWSMaxBytes, capabilities.Frame)
	require.Equal(t, responsesWSMaxBytes, capabilities.Queue)
	require.Equal(t, responsesWSMaxBytes, capabilities.State)
	require.JSONEq(t, fmt.Sprintf(responsesWSCapabilities, responsesWSMaxBytes, responsesWSMaxBytes, responsesWSMaxBytes,
		capabilities.Lifetime, capabilities.Admission, capabilities.Active), resp.Header.Get(http.CanonicalHeaderKey(responsesWSCapabilityHeader)))
	t.Cleanup(func() { conn.Close() })
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	return conn
}
func wsRead(t *testing.T, conn *websocket.Conn) map[string]json.RawMessage {
	t.Helper()
	var event map[string]json.RawMessage
	require.NoError(t, conn.ReadJSON(&event))
	return event
}
func wsEventString(event map[string]json.RawMessage, key string) string {
	var s string
	_ = json.Unmarshal(event[key], &s)
	return s
}
func wsErrorCode(event map[string]json.RawMessage) string {
	var e struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(event["error"], &e)
	return e.Code
}
func wsCompletion(id string, output string) *httpclient.StreamEvent {
	return &httpclient.StreamEvent{Type: "response.completed", Data: []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","output":%s}}`, id, output))}
}

func TestResponsesWebSocketLargeFrameAndContinuation(t *testing.T) {
	const oldLimit = 8 << 20
	large := strings.Repeat("x", oldLimit+1024)
	var calls atomic.Int32
	h := wsTestHandler(func(_ context.Context, req *httpclient.Request, _ orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
		require.Greater(t, len(req.Body), oldLimit)
		require.Less(t, len(req.Body), responsesWSMaxBytes)
		var body struct {
			Input []struct {
				Content string `json:"content"`
			} `json:"input"`
		}
		require.NoError(t, json.Unmarshal(req.Body, &body))
		require.Equal(t, large, body.Input[0].Content)
		n := calls.Add(1)
		if n == 2 {
			require.Len(t, body.Input, 2)
			require.Equal(t, "suffix", body.Input[1].Content)
		}
		return orchestrator.ChatCompletionResult{ChatCompletionStream: streams.SliceStream([]*httpclient.StreamEvent{wsCompletion(fmt.Sprintf("large_%d", n), `[]`)})}, nil
	})
	conn := wsTestDial(t, wsTestServer(t, h))
	require.Equal(t, 32<<20, responsesWSMaxBytes)
	require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": large}))
	require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
	require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "previous_response_id": "large_1", "input": "suffix"}))
	require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
	require.Equal(t, int32(2), calls.Load())
}

func TestResponsesWebSocketOversizedFrameNeverExecutes(t *testing.T) {
	var calls atomic.Int32
	h := wsTestHandler(func(context.Context, *httpclient.Request, orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
		calls.Add(1)
		return orchestrator.ChatCompletionResult{}, errors.New("oversized frame executed")
	})
	conn := wsTestDial(t, wsTestServer(t, h))
	// The server can close while the client is still writing the oversized frame.
	_ = conn.WriteMessage(websocket.TextMessage, []byte(strings.Repeat(" ", responsesWSMaxBytes+1)))
	_, _, err := conn.ReadMessage()
	require.True(t, websocket.IsCloseError(err, websocket.CloseMessageTooBig), "expected close 1009, got %v", err)
	require.Zero(t, calls.Load())
}

func TestResponsesWebSocketQueueByteBudget(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	h := wsTestHandler(func(ctx context.Context, _ *httpclient.Request, _ orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return orchestrator.ChatCompletionResult{ChatCompletionStream: streams.SliceStream([]*httpclient.StreamEvent{wsCompletion("queued", `[]`)})}, nil
	})
	conn := wsTestDial(t, wsTestServer(t, h))
	defer close(release)
	require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": "active"}))
	<-started
	queued := map[string]any{"type": "response.create", "stream_id": "large", "model": "m", "input": strings.Repeat("x", responsesWSMaxBytes/2+1024)}
	require.NoError(t, conn.WriteJSON(queued))
	require.NoError(t, conn.WriteJSON(queued))
	overflow := wsRead(t, conn)
	require.Equal(t, "queue_capacity_exceeded", wsErrorCode(overflow))
	require.Equal(t, "large", wsEventString(overflow, "stream_id"))
	require.Equal(t, "not_executed", wsEventString(overflow, "execution"))
	require.Equal(t, int32(1), calls.Load())
}

func TestResponsesWebSocketFullDeltaForkAndReconnect(t *testing.T) {
	var calls atomic.Int32
	bodies := make(chan []byte, 5)
	ids := make(chan string, 5)
	h := wsTestHandler(func(ctx context.Context, req *httpclient.Request, _ orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
		require.True(t, shared.NativeResponsesWebSocket(ctx))
		id, _ := contexts.GetRequestID(ctx)
		ids <- id
		require.Equal(t, http.MethodPost, req.Method)
		require.Empty(t, req.Headers.Get("Upgrade"))
		bodies <- append([]byte(nil), req.Body...)
		n := calls.Add(1)
		return orchestrator.ChatCompletionResult{ChatCompletionStream: streams.SliceStream([]*httpclient.StreamEvent{wsCompletion(fmt.Sprintf("resp_%d", n), `[{"id":"out","type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"done","opaque":{"signature":"sig","n":9007199254740993}}],"future":"retained"},{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}]`)})}, nil
	})
	server := wsTestServer(t, h)
	conn := wsTestDial(t, server)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"m","store":false,"instructions":"first","input":[{"role":"user","content":"start","unknown":{"n":9007199254740993}}]}`)))
	require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
	first := <-bodies
	require.Contains(t, string(first), "9007199254740993")
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","stream_id":"fork","model":"m","previous_response_id":"resp_1","instructions":"current","input":[{"type":"function_call_output","call_id":"call_1","output":"result","opaque":"keep"}]}`)))
	event := wsRead(t, conn)
	require.Equal(t, "fork", wsEventString(event, "stream_id"))
	var expanded map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(<-bodies, &expanded))
	require.NotContains(t, expanded, "previous_response_id")
	require.Equal(t, `"current"`, string(expanded["instructions"]))
	var input []json.RawMessage
	require.NoError(t, json.Unmarshal(expanded["input"], &input))
	require.Len(t, input, 4)
	require.Contains(t, string(input[1]), `"phase":"commentary"`)
	require.Contains(t, string(input[1]), `9007199254740993`)
	require.Contains(t, string(input[1]), `"future":"retained"`)
	require.Contains(t, string(input[3]), `"call_id":"call_1"`)
	// stream_id alone selects a lane, not history; it must start a new input.
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","stream_id":"fork","model":"m","input":"new chain"}`)))
	require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
	var reset map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(<-bodies, &reset))
	require.NoError(t, json.Unmarshal(reset["input"], &input))
	require.Len(t, input, 1)
	require.NotEqual(t, <-ids, <-ids)
	conn.Close()
	conn = wsTestDial(t, server)
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"m","previous_response_id":"resp_1","input":"delta"}`)))
	miss := wsRead(t, conn)
	require.Equal(t, "previous_response_not_found", wsErrorCode(miss))
	require.Equal(t, "not_executed", wsEventString(miss, "execution"))
	require.Equal(t, int32(3), calls.Load())
}

func TestResponsesWebSocketRejectsControlsBeforeInference(t *testing.T) {
	var calls atomic.Int32
	h := wsTestHandler(func(context.Context, *httpclient.Request, orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
		calls.Add(1)
		return orchestrator.ChatCompletionResult{}, errors.New("should not execute")
	})
	conn := wsTestDial(t, wsTestServer(t, h))
	for _, body := range []string{
		`{"type":"response.create","model":"m","stream":true,"generate":false}`, `{"type":"response.create","model":"m","stream":true,"background":true}`, `{"type":"response.create","model":"m","stream":false}`, `{"type":"response.create","model":"m","stream":true,"steering":{}}`, `{"type":"response.cancel"}`, `{"type":"response.create","model":"m","previous_response_id":"missing"}`, `{"type":"response.create","model":"m","stream_id":2}`, `null`,
	} {
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(body)))
		event := wsRead(t, conn)
		require.Equal(t, "error", wsEventString(event, "type"))
		require.Equal(t, "not_executed", wsEventString(event, "execution"))
	}
	require.Zero(t, calls.Load())
}

func TestResponsesWebSocketDoesNotCommitPartialOrFailedStreams(t *testing.T) {
	var calls atomic.Int32
	h := wsTestHandler(func(context.Context, *httpclient.Request, orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
		calls.Add(1)
		return orchestrator.ChatCompletionResult{ChatCompletionStream: &errorAfterStream{items: []*httpclient.StreamEvent{wsCompletion("ambiguous", `[]`)}, err: errors.New("stream interrupted")}}, nil
	})
	conn := wsTestDial(t, wsTestServer(t, h))
	require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": "full"}))
	require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
	event := wsRead(t, conn)
	require.Equal(t, "incomplete_stream", wsErrorCode(event))
	require.NotContains(t, event, "execution")
	require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "previous_response_id": "ambiguous", "input": "delta"}))
	require.Equal(t, "previous_response_not_found", wsErrorCode(wsRead(t, conn)))
	require.Equal(t, int32(1), calls.Load())
}

func TestResponsesWebSocketDisconnectCancelsGeneration(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	h := wsTestHandler(func(ctx context.Context, _ *httpclient.Request, _ orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return orchestrator.ChatCompletionResult{}, ctx.Err()
	})
	conn := wsTestDial(t, wsTestServer(t, h))
	require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": "full"}))
	<-started
	conn.Close()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("generation did not cancel")
	}
}

func TestResponsesWebSocketQueueAndFreshAuthentication(t *testing.T) {
	block, started := make(chan struct{}), make(chan struct{})
	var calls, authCalls atomic.Int32
	h := wsTestHandler(func(ctx context.Context, req *httpclient.Request, _ orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-block:
			case <-ctx.Done():
			}
		}
		return orchestrator.ChatCompletionResult{ChatCompletionStream: streams.SliceStream([]*httpclient.StreamEvent{wsCompletion("r", `[]`)})}, nil
	})
	h.authenticate = func(c *gin.Context) (context.Context, int, error) {
		n := authCalls.Add(1)
		if n > 1 {
			return nil, 401, errors.New("revoked")
		}
		return c.Request.Context(), 200, nil
	}
	conn := wsTestDial(t, wsTestServer(t, h))
	send := func() {
		require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": "full", "stream_id": "queued-lane"}))
	}
	send()
	<-started
	for range 9 {
		send()
	}
	overflow := wsRead(t, conn)
	require.Equal(t, "queue_capacity_exceeded", wsErrorCode(overflow))
	require.Equal(t, "queued-lane", wsEventString(overflow, "stream_id"))
	require.Equal(t, "not_executed", wsEventString(overflow, "execution"))
	close(block)
	require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
	for range 8 {
		event := wsRead(t, conn)
		require.Equal(t, "authentication_failed", wsErrorCode(event))
		require.Equal(t, "not_executed", wsEventString(event, "execution"))
	}
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, int32(9), authCalls.Load())
}

func TestResponsesWebSocketStateTTLModelAndBounds(t *testing.T) {
	now := time.Now()
	state := newResponsesWSState(time.Second)
	full, err := parseResponsesWSCreate([]byte(`{"type":"response.create","model":"m","input":"full"}`))
	require.Nil(t, err)
	_, input, err := state.prepare(full, now)
	require.Nil(t, err)
	require.True(t, state.commit(full, input, []byte(`{"id":"r","status":"completed","output":[]}`), now))
	delta, _ := parseResponsesWSCreate([]byte(`{"type":"response.create","model":"other","previous_response_id":"r","input":"delta"}`))
	_, _, err = state.prepare(delta, now)
	require.Equal(t, "previous_response_not_found", err.code)
	delta.model = "m"
	_, _, err = state.prepare(delta, now.Add(time.Second))
	require.Equal(t, "previous_response_not_found", err.code)
	require.Zero(t, state.bytes)
	for i := 0; i < 8; i++ {
		full.streamID = fmt.Sprint(i)
		_, _, err = state.prepare(full, now)
		require.Nil(t, err)
	}
	full.streamID = "overflow"
	_, _, err = state.prepare(full, now)
	require.Equal(t, "stream_capacity_exceeded", err.code)
}

func TestResponsesWebSocketPerGenerationTimeoutKeepsSocketUsable(t *testing.T) {
	h := wsTestHandler(func(ctx context.Context, _ *httpclient.Request, o orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
		o.OnUpstreamAttemptSelected(orchestrator.StreamLivenessAttempt{})
		<-ctx.Done()
		return orchestrator.ChatCompletionResult{}, ctx.Err()
	})
	handlers := &OpenAIHandlers{ResponseWebSocketHandler: h}
	router := gin.New()
	router.GET("/v1/responses", handlers.ResponsesWebSocketWithTimeout(20*time.Millisecond))
	server := httptest.NewServer(router)
	defer server.Close()
	conn := wsTestDial(t, server)
	require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": "full"}))
	timeout := wsRead(t, conn)
	require.Equal(t, "generation_failed", wsErrorCode(timeout))
	require.NotContains(t, timeout, "execution")
	require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.cancel"}))
	require.Equal(t, "unsupported_event", wsErrorCode(wsRead(t, conn)))
	require.Equal(t, 5*time.Second, h.turnTimeout, "factory must not mutate the shared handler")
}

func TestResponsesWebSocketStateEvictsAndNeverCommitsFakeTerminal(t *testing.T) {
	now := time.Now()
	state := newResponsesWSState(time.Minute)
	frame, _ := parseResponsesWSCreate([]byte(`{"type":"response.create","model":"m","input":[]}`))
	require.False(t, state.commit(frame, nil, []byte(`{"id":"partial","status":"incomplete","output":[]}`), now))
	require.False(t, state.commit(frame, nil, []byte(`{"id":"partial","status":"completed"}`), now))
	large, _ := json.Marshal(map[string]any{"role": "user", "content": strings.Repeat("x", responsesWSMaxBytes/2)})
	require.True(t, state.commit(frame, []json.RawMessage{large}, []byte(`{"id":"first","status":"completed","output":[]}`), now))
	frame.streamID = "named"
	require.True(t, state.commit(frame, []json.RawMessage{large}, []byte(`{"id":"second","status":"completed","output":[]}`), now.Add(time.Millisecond)))
	require.LessOrEqual(t, state.bytes, responsesWSMaxBytes)
	require.NotContains(t, state.lanes, "")
	require.Contains(t, state.lanes, "named")
}

// Cancellation races with the second read, but concrete stream/Close errors
// must remain visible even when a completion event was delivered first.
type wsCompletedErrorStream struct {
	*errorAfterStream
	cancel   context.CancelFunc
	closeErr error
	closed   *atomic.Int32
}

func (s *wsCompletedErrorStream) Next() bool {
	if s.idx > 0 {
		s.cancel()
	}
	return s.errorAfterStream.Next()
}

func (s *wsCompletedErrorStream) Close() error {
	s.closed.Add(1)
	return s.closeErr
}

func TestResponsesWebSocketCompletedCancellationKeepsConcreteErrors(t *testing.T) {
	providerErr := errors.New("provider stream failed")
	transformErr := errors.New("transform drain failed")
	for _, tc := range []struct {
		name                string
		streamErr, closeErr error
		invalidCompletion   bool
	}{
		{name: "provider error", streamErr: providerErr},
		{name: "transform join error", streamErr: context.Canceled, closeErr: transformErr},
		{name: "provider error joined with cancellation", streamErr: errors.Join(context.Canceled, providerErr)},
		{name: "transform error joined with cancellation", closeErr: errors.Join(context.Canceled, transformErr)},
		{name: "different cancellation cause", streamErr: context.DeadlineExceeded},
		{name: "invalid completed snapshot", streamErr: context.Canceled, invalidCompletion: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls, closed atomic.Int32
			var cancelGeneration context.CancelFunc
			h := wsTestHandler(func(context.Context, *httpclient.Request, orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error) {
				calls.Add(1)
				output := `[]`
				if tc.invalidCompletion {
					output = `null`
				}
				stream := &wsCompletedErrorStream{
					errorAfterStream: &errorAfterStream{items: []*httpclient.StreamEvent{wsCompletion("r", output)}, err: tc.streamErr},
					cancel:           cancelGeneration, closeErr: tc.closeErr, closed: &closed,
				}
				return orchestrator.ChatCompletionResult{ChatCompletionStream: stream}, nil
			})
			h.authenticate = func(c *gin.Context) (context.Context, int, error) {
				ctx, cancel := context.WithCancel(c.Request.Context())
				cancelGeneration = cancel
				return ctx, http.StatusOK, nil
			}
			conn := wsTestDial(t, wsTestServer(t, h))
			require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "input": "full"}))
			require.Equal(t, "response.completed", wsEventString(wsRead(t, conn), "type"))
			require.Equal(t, "incomplete_stream", wsErrorCode(wsRead(t, conn)))
			wsObservationBarrier(t, conn)
			require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.create", "model": "m", "previous_response_id": "r", "input": "suffix"}))
			require.Equal(t, "previous_response_not_found", wsErrorCode(wsRead(t, conn)))
			require.Equal(t, int32(1), calls.Load())
			require.Equal(t, int32(1), closed.Load())
		})
	}
}
