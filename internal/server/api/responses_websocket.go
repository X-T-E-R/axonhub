package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/middleware"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/shared"
)

const responsesWSCapabilityHeader = "X-Responses-WebSocket-Capabilities"
const responsesWSCapabilities = `{"version":1,"events":["response.create"],"generate":true,"generate_false":false,"background":false,"steering":false,"cancel":false,"scheduling":"connection_fifo","max_in_flight":1,"max_named_streams":8,"max_queued":8,"max_frame_bytes":8388608,"max_queued_bytes":8388608,"max_state_bytes":8388608,"state_ttl_seconds":600,"connection_lifetime_seconds":1800,"state_scope":"connection","previous_response_id":true,"native_responses_upstream_required":true,"state_miss":"previous_response_not_found","state_miss_execution":"not_executed"}`
const responsesWSMaxBytes = 8 << 20

type responsesWSProcessor func(context.Context, *httpclient.Request, orchestrator.StreamLivenessObserver) (orchestrator.ChatCompletionResult, error)
type responsesWSAuthenticator func(*gin.Context) (context.Context, int, error)

// ResponsesWebSocketHandler adapts official response.create events to the existing
// Responses orchestrator. Its state is ephemeral and local to one client socket.
type ResponsesWebSocketHandler struct {
	process      responsesWSProcessor
	authenticate responsesWSAuthenticator
	slots        chan struct{}
	turnTimeout  time.Duration
	lifetime     time.Duration
	stateTTL     time.Duration
}

func newResponsesWebSocketHandler(handler *ChatCompletionHandlers, auth *biz.AuthService) *ResponsesWebSocketHandler {
	return &ResponsesWebSocketHandler{
		process: handler.ChatCompletionOrchestrator.ProcessWithStreamLivenessObserver,
		authenticate: func(c *gin.Context) (context.Context, int, error) {
			return middleware.AuthenticateAPIKeyContext(c, auth, nil)
		},
		slots: make(chan struct{}, 64), turnTimeout: 10 * time.Minute, lifetime: 30 * time.Minute, stateTTL: 10 * time.Minute,
	}
}

func (handlers *OpenAIHandlers) CreateResponseWebSocket(c *gin.Context) {
	handlers.ResponseWebSocketHandler.Serve(c)
}

// ResponsesWebSocketWithTimeout freezes the per-generation timeout at route
// registration. The upgraded connection has its own independent lifetime.
func (handlers *OpenAIHandlers) ResponsesWebSocketWithTimeout(timeout time.Duration) gin.HandlerFunc {
	configured := *handlers.ResponseWebSocketHandler
	configured.turnTimeout = timeout
	return configured.Serve
}

type responsesWSWriter struct {
	conn   *websocket.Conn
	mu     sync.Mutex
	cancel context.CancelFunc
}

func (w *responsesWSWriter) write(value any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	_ = w.conn.SetWriteDeadline(time.Now().Add(15 * time.Second))
	if err := w.conn.WriteJSON(value); err != nil {
		w.cancel()
		_ = w.conn.Close()
		return err
	}
	return nil
}
func (w *responsesWSWriter) failure(streamID string, e *responsesWSError) error {
	event := map[string]any{"type": "error", "status": e.status, "error": map[string]any{"type": e.kind, "code": e.code, "message": e.message, "param": e.param}}
	if streamID != "" {
		event["stream_id"] = streamID
	}
	if e.notExecuted {
		event["execution"] = "not_executed"
	}
	return w.write(event)
}

func (w *responsesWSWriter) queueFailure(data []byte, message string) error {
	var frame struct {
		StreamID string `json:"stream_id"`
	}
	if json.Unmarshal(data, &frame) != nil || len(frame.StreamID) > 64 {
		frame.StreamID = ""
	}
	return w.failure(frame.StreamID, wsRejected("queue_capacity_exceeded", message, ""))
}

type responsesWSFrame struct{ data []byte }

func (h *ResponsesWebSocketHandler) Serve(c *gin.Context) {
	if !websocket.IsWebSocketUpgrade(c.Request) {
		c.JSON(http.StatusUpgradeRequired, gin.H{"error": gin.H{"type": "invalid_request_error", "code": "websocket_upgrade_required", "message": "Use a WebSocket upgrade for this endpoint, or POST /v1/responses."}})
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"code": "websocket_capacity_exceeded", "message": "WebSocket connection capacity reached."}})
		return
	}
	upgrader := websocket.Upgrader{HandshakeTimeout: 10 * time.Second, ReadBufferSize: 4096, WriteBufferSize: 4096}
	conn, err := upgrader.Upgrade(c.Writer, c.Request, http.Header{responsesWSCapabilityHeader: []string{responsesWSCapabilities}})
	if err != nil {
		return
	}
	defer conn.Close()
	conn.SetReadLimit(responsesWSMaxBytes)
	// A finite connection lifetime also unblocks an idle reader without a timer goroutine.
	_ = conn.SetReadDeadline(time.Now().Add(h.lifetime))
	ctx, cancel := context.WithTimeout(c.Request.Context(), h.lifetime)
	defer cancel()
	writer := &responsesWSWriter{conn: conn, cancel: cancel}
	queue := make(chan responsesWSFrame, 8)
	var queuedBytes atomic.Int64
	done := make(chan struct{})
	snapshot := c.Copy()
	go func() {
		defer close(done)
		defer func() {
			if recover() != nil {
				log.Error(ctx, "Responses WebSocket worker panicked", log.String("stack", string(debug.Stack())))
				cancel()
				_ = conn.Close()
			}
		}()
		state := newResponsesWSState(h.stateTTL)
		cleanup := time.NewTicker(min(h.stateTTL, time.Minute))
		defer cleanup.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-cleanup.C:
				state.expire(now)
			case frame := <-queue:
				queuedBytes.Add(-int64(len(frame.data)))
				if ctx.Err() != nil {
					return
				}
				h.generate(ctx, snapshot, frame.data, state, writer)
			}
		}
	}()
	// Cancellation must unblock ReadMessage and provider I/O; Close is safe with
	// the sole reader and the serialized writer. Join before releasing the slot.
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer func() { cancel(); _ = conn.Close(); stopClose(); <-done }()
	for {
		typ, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		if typ != websocket.TextMessage {
			if writer.failure("", wsRejected("unsupported_frame", "Only JSON text frames are supported.", "")) != nil {
				return
			}
			continue
		}
		size := int64(len(data))
		if queuedBytes.Add(size) > responsesWSMaxBytes {
			queuedBytes.Add(-size)
			if writer.queueFailure(data, "The connection queue byte limit was reached; this event was not executed.") != nil {
				return
			}
			continue
		}
		select {
		case queue <- responsesWSFrame{data: data}:
		default:
			queuedBytes.Add(-size)
			if writer.queueFailure(data, "The connection queue is full; this event was not executed.") != nil {
				return
			}
		}
	}
}

func (h *ResponsesWebSocketHandler) generate(parent context.Context, c *gin.Context, data []byte, state *responsesWSState, writer *responsesWSWriter) {
	frame, parseErr := parseResponsesWSCreate(data)
	if parseErr != nil {
		_ = writer.failure(frame.streamID, parseErr)
		return
	}
	ctx, cancel := context.WithTimeout(contexts.NewRequestScope(parent), h.turnTimeout)
	defer cancel()
	ctx = shared.WithNativeResponsesWebSocket(ctx)
	ctx = contexts.WithRequestID(ctx, uuid.NewString())
	authContext := c.Copy()
	authContext.Request = c.Request.Clone(ctx)
	authenticated, status, err := h.authenticate(authContext)
	if err != nil {
		_ = writer.failure(frame.streamID, &responsesWSError{status: status, kind: "authentication_error", code: "authentication_failed", message: err.Error(), notExecuted: true})
		return
	}
	// A response reference is never transferable across a changed key/project.
	scope, _ := shared.GetSessionScope(authenticated)
	originalScope, _ := shared.GetSessionScope(c.Request.Context())
	if scope != originalScope {
		state.clear()
		_ = writer.failure(frame.streamID, wsRejected("authentication_context_changed", "Authentication scope changed; reconnect before generating.", ""))
		return
	}
	body, input, prepErr := state.prepare(frame, time.Now())
	if prepErr != nil {
		_ = writer.failure(frame.streamID, prepErr)
		return
	}
	// Retire this lane's previous baseline before attempting a new generation.
	// Failure/partial output never becomes a continuation checkpoint.
	state.retire(frame.streamID)
	req := &httpclient.Request{Method: http.MethodPost, URL: c.Request.URL.String(), Path: c.Request.URL.Path, Headers: c.Request.Header.Clone(), Body: body, ContentType: "application/json", APIFormat: string(llm.APIFormatOpenAIResponse)}
	req.Headers.Set("Content-Type", "application/json")
	for _, name := range []string{"Connection", "Upgrade", "Sec-WebSocket-Key", "Sec-WebSocket-Version", "Sec-WebSocket-Extensions", "Content-Encoding", "Content-Length"} {
		req.Headers.Del(name)
	}
	observer := &responsesWSObserver{}
	result, err := h.process(authenticated, req, observer)
	if err != nil {
		notExecuted := !observer.selected.Load()
		code := "generation_failed"
		kind := "server_error"
		message := "Generation failed. Delivery may be unknown; do not automatically replay."
		errorStatus := http.StatusInternalServerError
		var llmErr *llm.ResponseError
		if errors.As(err, &llmErr) {
			code = llmErr.Detail.Code
			kind = llmErr.Detail.Type
			errorStatus = llmErr.StatusCode
			if notExecuted {
				message = llmErr.Detail.Message
			}
		}
		_ = writer.failure(frame.streamID, &responsesWSError{status: errorStatus, kind: kind, code: code, message: message, notExecuted: notExecuted})
		return
	}
	if result.ChatCompletionStream == nil {
		_ = writer.failure(frame.streamID, &responsesWSError{status: 502, kind: "server_error", code: "invalid_upstream_stream", message: "The upstream did not return a Responses stream."})
		return
	}
	stream := newCloseOnceStream(result.ChatCompletionStream)
	defer stream.Close()
	var completed json.RawMessage
	terminal := false
	failed := false
	for stream.Next() {
		if authenticated.Err() != nil {
			return
		}
		event := stream.Current()
		if event == nil {
			continue
		}
		if len(event.Data) > responsesWSMaxBytes {
			failed = true
			break
		}
		var raw map[string]json.RawMessage
		if json.Unmarshal(event.Data, &raw) != nil {
			failed = true
			break
		}
		var eventType string
		_ = json.Unmarshal(raw["type"], &eventType)
		if eventType == "" && event.Type != "" {
			eventType = event.Type
			raw["type"], _ = json.Marshal(eventType)
		}
		if eventType == "" || terminal {
			failed = true
			break
		}
		delete(raw, "stream_id")
		if frame.streamID != "" {
			raw["stream_id"], _ = json.Marshal(frame.streamID)
		}
		if writer.write(raw) != nil {
			return
		}
		switch eventType {
		case "response.completed":
			terminal = true
			completed = raw["response"]
		case "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
			terminal = true
		}
	}
	// Join the raw producer and canonical persistence drain before inspecting
	// their terminal errors or committing a continuation baseline.
	closeErr := stream.Close()
	streamErr := stream.Err()
	if failed || streamErr != nil || closeErr != nil || !terminal {
		_ = writer.failure(frame.streamID, &responsesWSError{status: 502, kind: "server_error", code: "incomplete_stream", message: "The response stream ended without a valid terminal. Delivery may be unknown; do not automatically replay."})
		return
	}
	if len(completed) > 0 {
		state.commit(frame, input, completed, time.Now())
	}
}

type responsesWSObserver struct{ selected atomic.Bool }

func (o *responsesWSObserver) OnUpstreamAttemptSelected(orchestrator.StreamLivenessAttempt) {
	o.selected.Store(true)
}
func (*responsesWSObserver) OnUpstreamResponseHeaders(orchestrator.StreamLivenessAttempt) {}
func (*responsesWSObserver) OnRawSSE(*httpclient.StreamEvent)                             {}
func (*responsesWSObserver) OnTransformableEvent(*llm.Response)                           {}
func (*responsesWSObserver) OnUpstreamError(error)                                        {}
