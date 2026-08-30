package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-contrib/sse"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

const (
	errTypeQuotaExhausted = "quota_exhausted"
	errCodeQuotaExhausted = "quota_exhausted"
)

// StreamWriter is a function type for writing stream events to the response.
type StreamWriter func(c *gin.Context, stream streams.Stream[*httpclient.StreamEvent])

// SSEKeepAliveConfig controls downstream heartbeats for SSE-compatible APIs.
type SSEKeepAliveConfig struct {
	Enabled  bool
	Interval time.Duration
}

type sseHeartbeatFormat uint8

const (
	sseHeartbeatNone sseHeartbeatFormat = iota
	sseHeartbeatOpenAI
	sseHeartbeatAnthropic
)

type ChatCompletionHandlers struct {
	ChatCompletionOrchestrator *orchestrator.ChatCompletionOrchestrator
	StreamWriter               StreamWriter
	sseKeepAlive               SSEKeepAliveConfig
	sseHeartbeatFormat         sseHeartbeatFormat
}

func NewChatCompletionHandlers(orchestrator *orchestrator.ChatCompletionOrchestrator) *ChatCompletionHandlers {
	return &ChatCompletionHandlers{
		ChatCompletionOrchestrator: orchestrator,
	}
}

// WithStreamWriter returns a new ChatCompletionHandlers with the specified stream writer.
func (handlers *ChatCompletionHandlers) WithStreamWriter(writer StreamWriter) *ChatCompletionHandlers {
	return &ChatCompletionHandlers{
		ChatCompletionOrchestrator: handlers.ChatCompletionOrchestrator,
		StreamWriter:               writer,
		sseKeepAlive:               handlers.sseKeepAlive,
		sseHeartbeatFormat:         handlers.sseHeartbeatFormat,
	}
}

func (handlers *ChatCompletionHandlers) ChatCompletion(c *gin.Context) {
	ctx := c.Request.Context()

	// Use ReadHTTPRequest to parse the request
	genericReq, err := httpclient.ReadHTTPRequest(c.Request)
	if err != nil {
		httpErr := handlers.ChatCompletionOrchestrator.Inbound.TransformError(ctx, err)
		c.JSON(httpErr.StatusCode, json.RawMessage(httpErr.Body))

		return
	}

	handlers.ChatCompletionWithRequest(c, genericReq)
}

func (handlers *ChatCompletionHandlers) ChatCompletionWithRequest(c *gin.Context, genericReq *httpclient.Request) {
	ctx := c.Request.Context()

	if genericReq == nil || len(genericReq.Body) == 0 {
		JSONError(c, http.StatusBadRequest, errors.New("Request body is empty"))
		return
	}

	// log.Debug(ctx, "Chat completion request", log.Any("request", genericReq))

	var (
		result    orchestrator.ChatCompletionResult
		err       error
		committed bool
		liveness  *sseLivenessSession
	)

	if handlers.sseHeartbeatFormat != sseHeartbeatNone {
		var cancel context.CancelCauseFunc
		ctx, cancel = requestWithSSELivenessContext(c)
		defer cancel(nil)
		liveness = newSSELivenessSession(
			ctx,
			cancel,
			handlers.sseKeepAlive,
			handlers.sseHeartbeatFormat,
			gjson.GetBytes(genericReq.Body, "stream").Bool(),
		)
		if requestedChoices := int(gjson.GetBytes(genericReq.Body, "n").Int()); requestedChoices > 1 {
			liveness.expectedChoices = requestedChoices
		}
		outcome, responseCommitted, aborted := liveness.awaitProcess(
			c,
			handlers.ChatCompletionOrchestrator.ProcessWithStreamLivenessObserver,
			genericReq,
		)
		if aborted {
			return
		}
		result, err, committed = outcome.result, outcome.err, responseCommitted
	} else {
		result, err = handlers.ChatCompletionOrchestrator.Process(ctx, genericReq)
	}
	if err != nil {
		log.Error(ctx, "Error processing chat completion", log.Cause(err))

		httpErr := transformOrchestratorError(ctx, err, handlers.ChatCompletionOrchestrator)
		if committed && liveness != nil {
			_ = liveness.writeError(c, json.RawMessage(httpErr.Body))
			liveness.finish(sseCloseUpstreamError)
		} else {
			c.JSON(httpErr.StatusCode, json.RawMessage(httpErr.Body))
			if liveness != nil {
				liveness.finish(sseCloseUpstreamError)
			}
		}

		return
	}

	if result.ChatCompletion != nil {
		resp := result.ChatCompletion

		contentType := "application/json"
		if ct := resp.Headers.Get("Content-Type"); ct != "" {
			contentType = ct
		}

		c.Data(resp.StatusCode, contentType, resp.Body)
		if liveness != nil {
			liveness.finish(sseCloseStreamCompleted)
		}

		return
	}

	if result.ChatCompletionStream != nil {
		ownedStream := newCloseOnceStream(result.ChatCompletionStream)
		defer func() {
			log.Debug(ctx, "Close chat stream")

			err := ownedStream.Close()
			if err != nil {
				logger.Error(ctx, "Error closing stream", log.Cause(err))
			}
		}()

		c.Header("Access-Control-Allow-Origin", "*")

		stream := newUpstreamErrorStream(ctx, ownedStream, handlers.ChatCompletionOrchestrator.SystemService)
		// Custom writers are reserved for non-SSE protocols (for example binary
		// speech and Gemini JSON). Ordinary OpenAI/Responses/Anthropic SSE routes
		// always use the liveness-aware writer below.
		if handlers.StreamWriter != nil && handlers.sseHeartbeatFormat == sseHeartbeatNone {
			handlers.StreamWriter(c, stream)
			if liveness != nil {
				liveness.finish(sseCloseStreamCompleted)
			}
			return
		}

		keepAlive := handlers.sseKeepAlive
		if liveness != nil {
			keepAlive = liveness.effectiveConfig()
		}
		writeSSEStream(c, stream, FormatStreamError, keepAlive, handlers.sseHeartbeatFormat, liveness)
	}
}

// StreamErrorFormatter formats a stream error into a JSON-serializable object for SSE error events.
type StreamErrorFormatter func(ctx context.Context, err error) any

// maxStreamEventsAfterCancel bounds how many binary events are drained after
// request cancellation. SSE liveness uses a raw decoder interrupt plus reader
// join instead. Pass-through channel buffers hold 64 events, so 256 is generous.
const maxStreamEventsAfterCancel = 256

// WriteSSEStream writes stream events as Server-Sent Events (SSE) with default error formatting.
func WriteSSEStream(c *gin.Context, stream streams.Stream[*httpclient.StreamEvent]) {
	WriteSSEStreamWithErrorFormatter(c, stream, FormatStreamError)
}

// WriteSSEStreamWithErrorFormatter writes stream events as SSE with a custom error formatter.
func WriteSSEStreamWithErrorFormatter(c *gin.Context, stream streams.Stream[*httpclient.StreamEvent], formatErr StreamErrorFormatter) {
	writeSSEStream(c, stream, formatErr, SSEKeepAliveConfig{}, sseHeartbeatNone, nil)
}

func writeSSEStream(
	c *gin.Context,
	stream streams.Stream[*httpclient.StreamEvent],
	formatErr StreamErrorFormatter,
	keepAlive SSEKeepAliveConfig,
	heartbeatFormat sseHeartbeatFormat,
	liveness *sseLivenessSession,
) {
	if heartbeatFormat == sseHeartbeatNone || (liveness == nil && (!keepAlive.Enabled || keepAlive.Interval <= 0)) {
		writeSSEStreamWithoutHeartbeat(c, stream, formatErr, heartbeatFormat, liveness)
		return
	}

	interval := time.Duration(0)
	if keepAlive.Enabled && keepAlive.Interval > 0 {
		interval = keepAlive.Interval
	}
	writeSSEStreamWithHeartbeat(c, stream, formatErr, interval, heartbeatFormat, liveness)
}

func writeSSEStreamWithoutHeartbeat(
	c *gin.Context,
	stream streams.Stream[*httpclient.StreamEvent],
	formatErr StreamErrorFormatter,
	heartbeatFormat sseHeartbeatFormat,
	liveness *sseLivenessSession,
) {
	ctx := c.Request.Context()
	clientDisconnected := false

	if formatErr == nil {
		formatErr = FormatStreamError
	}

	defer func() {
		if clientDisconnected {
			log.Warn(ctx, "Client disconnected")
		}
	}()

	// Set SSE headers
	setSSEHeaders(c)
	if err := flushSSE(c.Writer); err != nil {
		if liveness != nil {
			liveness.failDownstream(err)
		}
		return
	}

	for {
		if !stream.Next() {
			streamErr := stream.Err()
			if reason, canceled := closeReasonFromContext(ctx); canceled {
				clientDisconnected = reason == sseCloseClientDisconnect
				if liveness != nil {
					liveness.finish(reason)
				}
				if streamErr != nil && !errors.Is(streamErr, context.Canceled) && !errors.Is(streamErr, context.DeadlineExceeded) {
					log.Warn(ctx, "Stream error after request cancellation", log.Cause(streamErr))
				}
			} else if streamErr != nil {
				log.Error(ctx, "Error in stream", log.Cause(streamErr))
				if writeErr := writeAndFlushSSEEvent(c.Writer, "error", formatErr(ctx, streamErr)); writeErr != nil {
					if liveness != nil {
						liveness.failDownstream(writeErr)
					}
					return
				}
				if liveness != nil {
					liveness.ledger.recordWrite(false)
					liveness.finish(sseCloseUpstreamError)
				}
			} else if liveness != nil {
				liveness.finish(sseCloseUpstreamEOF)
			}

			return
		}

		if reason, canceled := closeReasonFromContext(ctx); canceled {
			clientDisconnected = reason == sseCloseClientDisconnect
			if liveness != nil {
				liveness.finish(reason)
			}
			return
		}

		cur := stream.Current()
		if err := writeAndFlushSSEEvent(c.Writer, cur.Type, cur.Data); err != nil {
			if liveness != nil {
				liveness.failDownstream(err)
			}
			return
		}
		log.Debug(ctx, "write stream event", log.Any("event", cur))
		if liveness != nil {
			liveness.ledger.recordWrite(false)
		}

		if isTerminalSSEEvent(cur, heartbeatFormat) {
			if liveness != nil {
				if orchestrator.ClassifyStreamSemanticTerminal(cur) == orchestrator.StreamSemanticSucceeded {
					liveness.confirmSemanticSuccess()
				}
				liveness.finish(sseCloseTerminalEvent)
			}
			return
		}
	}
}

func writeSSEStreamWithHeartbeat(
	c *gin.Context,
	stream streams.Stream[*httpclient.StreamEvent],
	formatErr StreamErrorFormatter,
	interval time.Duration,
	heartbeatFormat sseHeartbeatFormat,
	liveness *sseLivenessSession,
) {
	ctx := c.Request.Context()
	clientDisconnected := false

	if formatErr == nil {
		formatErr = FormatStreamError
	}

	defer func() {
		if clientDisconnected {
			log.Warn(ctx, "Client disconnected")
		}
	}()

	setSSEHeaders(c)
	if err := flushSSE(c.Writer); err != nil {
		if liveness != nil {
			liveness.failDownstream(err)
		}
		return
	}

	reader := newSSEStreamReader(ctx, stream)
	// Interrupt only the concurrency-safe raw decoder, then wait for the reader
	// to finish Next/Current before the outer handler closes persistence wrappers.
	var interrupt func() error
	if liveness != nil {
		interrupt = liveness.interruptUpstream
	} else if interruptible, ok := stream.(streams.Interruptible); ok {
		interrupt = interruptible.Interrupt
	}
	defer reader.Stop(interrupt)

	var heartbeatTimer *time.Timer
	var heartbeatC <-chan time.Time
	if interval > 0 {
		heartbeatTimer = time.NewTimer(interval)
		heartbeatC = heartbeatTimer.C
	}
	defer func() {
		if heartbeatTimer != nil {
			heartbeatTimer.Stop()
		}
	}()
	var terminalTimer *time.Timer
	defer func() {
		if terminalTimer != nil {
			terminalTimer.Stop()
		}
	}()

	var terminalC <-chan time.Time
	var semanticC <-chan struct{}
	terminalTracker := newSSESemanticTerminalTracker(heartbeatFormat)
	semanticObserved := false
	if liveness != nil {
		semanticC = liveness.semanticSuccessSignal()
	}
	ctxDone := ctx.Done()
	for {
		select {
		case <-ctxDone:
			reason, _ := closeReasonFromContext(ctx)
			clientDisconnected = reason == sseCloseClientDisconnect
			if heartbeatTimer != nil {
				stopTimer(heartbeatTimer)
			}
			heartbeatC = nil
			if terminalTimer != nil {
				stopTimer(terminalTimer)
				terminalC = nil
			}
			if liveness != nil {
				liveness.finish(reason)
			}
			return

		case result := <-reader.Results():
			if reason, canceled := closeReasonFromContext(ctx); canceled {
				clientDisconnected = reason == sseCloseClientDisconnect
				if liveness != nil {
					liveness.finish(reason)
				}
				return
			}
			if result.done {
				writeSSEStreamEnd(c, ctx, result.err, formatErr, &clientDisconnected, liveness)
				return
			}

			cur := result.event
			if err := writeAndFlushSSEEvent(c.Writer, cur.Type, cur.Data); err != nil {
				if liveness != nil {
					liveness.failDownstream(err)
				}
				return
			}
			log.Debug(ctx, "write stream event", log.Any("event", cur))
			if liveness != nil {
				liveness.ledger.recordWrite(false)
			}
			terminalTracker.observe(cur)

			if isTerminalSSEEvent(cur, heartbeatFormat) {
				if heartbeatTimer != nil {
					stopTimer(heartbeatTimer)
				}
				heartbeatC = nil
				if terminalTimer != nil {
					stopTimer(terminalTimer)
					terminalC = nil
				}
				if liveness != nil {
					if orchestrator.ClassifyStreamSemanticTerminal(cur) == orchestrator.StreamSemanticSucceeded {
						liveness.confirmSemanticSuccess()
					}
					liveness.finish(sseCloseTerminalEvent)
				}
				return
			}

			if semanticObserved {
				resetTimer(terminalTimer, liveness.terminalGrace)
				terminalC = terminalTimer.C
			} else if heartbeatTimer != nil && heartbeatC != nil {
				resetTimer(heartbeatTimer, interval)
			}

		case <-semanticC:
			semanticObserved = true
			semanticC = nil
			if heartbeatTimer != nil {
				stopTimer(heartbeatTimer)
			}
			heartbeatC = nil
			terminalTimer = time.NewTimer(liveness.terminalGrace)
			terminalC = terminalTimer.C

		case <-terminalC:
			terminalC = nil
			if !liveness.confirmSemanticSuccess() {
				continue
			}
			if err := terminalTracker.writeFallback(c.Writer, liveness.semanticCompletionResponse()); err != nil {
				liveness.failDownstream(err)
				return
			}
			liveness.ledger.recordWrite(false)
			liveness.finish(sseCloseSemanticCompletion)
			return

		case <-heartbeatC:
			if err := writeAndFlushSSEHeartbeat(c.Writer, heartbeatFormat); err != nil {
				clientDisconnected = true
				if liveness != nil {
					liveness.failDownstream(err)
				} else {
					log.Warn(ctx, "Failed to write SSE heartbeat", log.Cause(err))
				}
				return
			}

			if liveness != nil {
				liveness.ledger.recordWrite(true)
			}
			if heartbeatTimer != nil {
				heartbeatTimer.Reset(interval)
			}
		}
	}
}

func writeSSEStreamEnd(
	c *gin.Context,
	ctx context.Context,
	streamErr error,
	formatErr StreamErrorFormatter,
	clientDisconnected *bool,
	liveness *sseLivenessSession,
) {
	if reason, canceled := closeReasonFromContext(ctx); canceled {
		*clientDisconnected = reason == sseCloseClientDisconnect
		if liveness != nil {
			liveness.finish(reason)
		}
		if streamErr != nil && !errors.Is(streamErr, context.Canceled) && !errors.Is(streamErr, context.DeadlineExceeded) {
			log.Warn(ctx, "Stream error after request cancellation", log.Cause(streamErr))
		}
		return
	}

	if streamErr != nil {
		log.Error(ctx, "Error in stream", log.Cause(streamErr))
		if err := writeAndFlushSSEEvent(c.Writer, "error", formatErr(ctx, streamErr)); err != nil {
			if liveness != nil {
				liveness.failDownstream(err)
			}
			return
		}
		if liveness != nil {
			liveness.ledger.recordWrite(false)
			liveness.finish(sseCloseUpstreamError)
		}
	} else if liveness != nil {
		liveness.finish(sseCloseUpstreamEOF)
	}
}

func stopTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func resetTimer(timer *time.Timer, interval time.Duration) {
	stopTimer(timer)
	timer.Reset(interval)
}

func setSSEHeaders(c *gin.Context) {
	setSSEResponseHeaders(c.Writer.Header())
}

func setSSEResponseHeaders(header http.Header) {
	header.Set("Content-Type", sse.ContentType)
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("Access-Control-Allow-Origin", "*")
}

type sseErrorTrackingWriter struct {
	writer io.Writer
	err    error
}

func (w *sseErrorTrackingWriter) Write(data []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	n, err := w.writer.Write(data)
	if err != nil {
		w.err = err
	}
	return n, err
}

func (w *sseErrorTrackingWriter) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

func writeAndFlushSSEEvent(writer http.ResponseWriter, event string, data any) error {
	tracked := &sseErrorTrackingWriter{writer: writer}
	if err := sse.Encode(tracked, sse.Event{Event: event, Data: data}); err != nil {
		return err
	}
	if tracked.err != nil {
		return tracked.err
	}
	return flushSSE(writer)
}

func flushSSE(writer http.ResponseWriter) error {
	return http.NewResponseController(writer).Flush()
}

func writeAndFlushSSEHeartbeat(writer http.ResponseWriter, format sseHeartbeatFormat) error {
	if err := writeSSEHeartbeat(writer, format); err != nil {
		return err
	}
	return flushSSE(writer)
}

func writeSSEHeartbeat(writer io.Writer, format sseHeartbeatFormat) error {
	switch format {
	case sseHeartbeatOpenAI:
		_, err := io.WriteString(writer, ": keep-alive\n\n")
		return err
	case sseHeartbeatAnthropic:
		_, err := io.WriteString(writer, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		return err
	default:
		return errors.New("unsupported SSE heartbeat format")
	}
}

func isTerminalSSEEvent(event *httpclient.StreamEvent, format sseHeartbeatFormat) bool {
	if event == nil {
		return false
	}

	typeName := event.Type
	if typeName == "" && len(event.Data) > 0 {
		typeName = gjson.GetBytes(event.Data, "type").String()
	}

	switch format {
	case sseHeartbeatOpenAI:
		if bytes.Equal(bytes.TrimSpace(event.Data), []byte("[DONE]")) {
			return true
		}
		switch typeName {
		case "response.completed", "response.failed", "response.incomplete":
			return true
		}
	case sseHeartbeatAnthropic:
		return typeName == "message_stop"
	}

	return false
}

type sseWireProtocol uint8

const (
	sseWireProtocolOpenAIChat sseWireProtocol = iota
	sseWireProtocolOpenAIResponses
	sseWireProtocolAnthropic
)

type sseSemanticTerminalTracker struct {
	protocol       sseWireProtocol
	response       map[string]any
	completedItems map[int]any
	lastSequence   int64
}

func newSSESemanticTerminalTracker(format sseHeartbeatFormat) *sseSemanticTerminalTracker {
	protocol := sseWireProtocolOpenAIChat
	if format == sseHeartbeatAnthropic {
		protocol = sseWireProtocolAnthropic
	}
	return &sseSemanticTerminalTracker{
		protocol:       protocol,
		completedItems: make(map[int]any),
	}
}

func (t *sseSemanticTerminalTracker) observe(event *httpclient.StreamEvent) {
	if event == nil || t.protocol == sseWireProtocolAnthropic {
		return
	}

	typeName := event.Type
	if typeName == "" {
		typeName = gjson.GetBytes(event.Data, "type").String()
	}
	if !strings.HasPrefix(typeName, "response.") {
		return
	}
	t.protocol = sseWireProtocolOpenAIResponses

	var payload map[string]any
	if err := json.Unmarshal(event.Data, &payload); err != nil {
		return
	}
	if sequence := gjson.GetBytes(event.Data, "sequence_number"); sequence.Exists() && sequence.Int() > t.lastSequence {
		t.lastSequence = sequence.Int()
	}
	if response, ok := payload["response"].(map[string]any); ok && t.response == nil {
		t.response = response
	}
	if typeName != "response.output_item.done" {
		return
	}
	index := int(gjson.GetBytes(event.Data, "output_index").Int())
	item, ok := payload["item"]
	if !ok {
		return
	}
	t.completedItems[index] = item
}

func (t *sseSemanticTerminalTracker) writeFallback(writer http.ResponseWriter, semantic *llm.Response) error {
	switch t.protocol {
	case sseWireProtocolAnthropic:
		return writeAndFlushSSEEvent(writer, "message_stop", json.RawMessage(`{"type":"message_stop"}`))
	case sseWireProtocolOpenAIResponses:
		response := t.completedResponse(semantic)
		payload := map[string]any{
			"type":            "response.completed",
			"sequence_number": t.lastSequence + 1,
			"response":        response,
		}
		return writeAndFlushSSEEvent(writer, "response.completed", payload)
	default:
		return writeAndFlushSSEEvent(writer, "", []byte(`[DONE]`))
	}
}

func (t *sseSemanticTerminalTracker) completedResponse(semantic *llm.Response) map[string]any {
	response := make(map[string]any)
	for key, value := range t.response {
		response[key] = value
	}
	response["object"] = "response"
	response["status"] = "completed"
	response["error"] = nil
	response["incomplete_details"] = nil

	indices := make([]int, 0, len(t.completedItems))
	for index := range t.completedItems {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	output := make([]any, 0, len(indices))
	for _, index := range indices {
		output = append(output, t.completedItems[index])
	}
	response["output"] = output

	if semantic == nil {
		return response
	}
	if _, ok := response["id"]; !ok && semantic.ID != "" {
		response["id"] = semantic.ID
	}
	if _, ok := response["model"]; !ok && semantic.Model != "" {
		response["model"] = semantic.Model
	}
	if _, ok := response["created_at"]; !ok && semantic.Created != 0 {
		response["created_at"] = semantic.Created
	}
	if _, ok := response["usage"]; !ok && semantic.Usage != nil {
		response["usage"] = map[string]any{
			"input_tokens":  semantic.Usage.PromptTokens,
			"output_tokens": semantic.Usage.CompletionTokens,
			"total_tokens":  semantic.Usage.TotalTokens,
		}
	}
	return response
}

// WriteBinaryStream writes raw bytes from stream events directly to the response body.
// The first chunk type is treated as the stream Content-Type when present.
func WriteBinaryStream(c *gin.Context, stream streams.Stream[*httpclient.StreamEvent]) {
	ctx := c.Request.Context()
	clientDisconnected := false
	headersWritten := false
	contentType := "application/octet-stream"

	defer func() {
		if clientDisconnected {
			log.Warn(ctx, "Client disconnected")
		}
	}()

	// Same as WriteSSEStream: do not pre-check ctx.Done() before Next(), so a
	// disconnect right after the terminal chunk does not skip drain / completion.
	// The drain after cancellation is bounded by eventsAfterCancel.
	eventsAfterCancel := 0

	for {
		if !stream.Next() {
			if err := stream.Err(); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
					clientDisconnected = true

					// Keep genuine upstream failures visible even when the client is gone.
					if !errors.Is(err, context.Canceled) {
						log.Warn(ctx, "Binary stream error after client disconnected", log.Cause(err))
					}
				} else {
					log.Error(ctx, "Error in binary stream", log.Cause(err))
					if !headersWritten {
						c.JSON(streamErrorStatus(err), FormatStreamError(ctx, err))
						return
					}
				}
			} else if errors.Is(ctx.Err(), context.Canceled) {
				clientDisconnected = true
			}

			c.Writer.Flush()

			return
		}

		if ctx.Err() != nil {
			eventsAfterCancel++
			if eventsAfterCancel > maxStreamEventsAfterCancel {
				clientDisconnected = true

				log.Warn(ctx, "Binary stream still producing after cancellation, aborting drain",
					log.Int("events_after_cancel", eventsAfterCancel))

				return
			}
		}

		cur := stream.Current()
		if cur != nil && cur.Type == httpclient.BinaryStreamDoneEventType {
			continue
		}

		if cur == nil || len(cur.Data) == 0 {
			continue
		}

		if !headersWritten {
			if ct := strings.TrimSpace(cur.Type); ct != "" {
				contentType = ct
			}

			c.Header("Content-Type", contentType)
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")
			c.Header("Access-Control-Allow-Origin", "*")
			headersWritten = true
		}

		if _, err := c.Writer.Write(cur.Data); err != nil {
			clientDisconnected = true
			log.Warn(ctx, "Failed to write binary stream chunk", log.Cause(err))

			return
		}

		c.Writer.Flush()
	}
}

func streamErrorStatus(err error) int {
	var quotaErr *orchestrator.QuotaExhaustedError
	if errors.As(err, &quotaErr) {
		return http.StatusServiceUnavailable
	}

	var respErr *llm.ResponseError
	if errors.As(err, &respErr) && respErr.StatusCode != 0 {
		return respErr.StatusCode
	}

	var httpErr *httpclient.Error
	if errors.As(err, &httpErr) && httpErr.StatusCode != 0 {
		return httpErr.StatusCode
	}

	return http.StatusInternalServerError
}

// FormatStreamError formats a stream error into an OpenAI-compatible JSON error object.
func FormatStreamError(_ context.Context, err error) any {
	errType := "server_error"
	errCode := ""
	requestID := ""

	var quotaErr *orchestrator.QuotaExhaustedError
	if errors.As(err, &quotaErr) {
		return gin.H{
			"error": gin.H{
				"message": quotaErr.Error(),
				"type":    errTypeQuotaExhausted,
				"code":    errCodeQuotaExhausted,
			},
		}
	}

	var respErr *llm.ResponseError
	if errors.As(err, &respErr) {
		if respErr.Detail.Type != "" {
			errType = respErr.Detail.Type
		}

		errCode = respErr.Detail.Code
		requestID = respErr.Detail.RequestID

		return gin.H{
			"error": gin.H{
				"message": respErr.Detail.Message,
				"type":    errType,
				"code":    errCode,
			},
			"request_id": requestID,
		}
	}

	var httpErr *httpclient.Error
	if errors.As(err, &httpErr) && len(httpErr.Body) > 0 {
		if t := gjson.GetBytes(httpErr.Body, "error.type"); t.Exists() && t.Type == gjson.String && t.String() != "" {
			errType = t.String()
		}

		if c := gjson.GetBytes(httpErr.Body, "error.code"); c.Exists() && c.Type == gjson.String && c.String() != "" {
			errCode = c.String()
		}

		if rid := gjson.GetBytes(httpErr.Body, "request_id"); rid.Exists() && rid.Type == gjson.String && rid.String() != "" {
			requestID = rid.String()
		}
	}

	return gin.H{
		"error": gin.H{
			"message": orchestrator.ExtractErrorMessage(err),
			"type":    errType,
			"code":    errCode,
		},
		"request_id": requestID,
	}
}

func wrapQuotaExhaustedAsResponseError(err error) error {
	if err == nil {
		return nil
	}

	var quotaErr *orchestrator.QuotaExhaustedError
	if errors.As(err, &quotaErr) {
		return &llm.ResponseError{
			StatusCode: http.StatusServiceUnavailable,
			Detail: llm.ErrorDetail{
				Message: quotaErr.Error(),
				Type:    errTypeQuotaExhausted,
				Code:    errCodeQuotaExhausted,
			},
		}
	}

	return err
}
