package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-contrib/sse"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func setupUpstreamErrorPolicyTest(t *testing.T, policy biz.UpstreamErrorPolicy) (context.Context, *biz.SystemService) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	t.Cleanup(func() {
		_ = client.Close()
	})

	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), client)
	systemService := biz.NewSystemService(biz.SystemServiceParams{})
	err := systemService.SetRetryPolicy(ctx, &biz.RetryPolicy{
		Enabled:                 true,
		MaxChannelRetries:       3,
		MaxSingleChannelRetries: 2,
		RetryDelayMs:            1000,
		LoadBalancerStrategy:    biz.LoadBalancerStrategyAdaptive,
		UpstreamErrorPolicy:     policy,
	})
	require.NoError(t, err)

	return ctx, systemService
}

// errorAfterStream emits items then returns an error.
type errorAfterStream struct {
	items []*httpclient.StreamEvent
	idx   int
	err   error
}

func (s *errorAfterStream) Next() bool {
	if s.idx < len(s.items) {
		return true
	}

	return false
}

func (s *errorAfterStream) Current() *httpclient.StreamEvent {
	item := s.items[s.idx]
	s.idx++

	return item
}

func (s *errorAfterStream) Err() error {
	if s.idx >= len(s.items) {
		return s.err
	}

	return nil
}

func (s *errorAfterStream) Close() error { return nil }

type trackingStream struct {
	items   []*httpclient.StreamEvent
	idx     int
	current *httpclient.StreamEvent
}

func (s *trackingStream) Next() bool {
	if s.idx >= len(s.items) {
		return false
	}

	s.current = s.items[s.idx]
	s.idx++

	return true
}

func (s *trackingStream) Current() *httpclient.StreamEvent { return s.current }
func (s *trackingStream) Err() error                       { return nil }
func (s *trackingStream) Close() error                     { return nil }

type delayedStream struct {
	delay   time.Duration
	event   *httpclient.StreamEvent
	current *httpclient.StreamEvent
	done    bool
}

type delayedSequenceStream struct {
	delay   time.Duration
	events  []*httpclient.StreamEvent
	current *httpclient.StreamEvent
	index   int
}

func (s *delayedSequenceStream) Next() bool {
	if s.index >= len(s.events) {
		return false
	}
	time.Sleep(s.delay)
	s.current = s.events[s.index]
	s.index++
	return true
}

func (s *delayedSequenceStream) Current() *httpclient.StreamEvent { return s.current }
func (s *delayedSequenceStream) Err() error                       { return nil }
func (s *delayedSequenceStream) Close() error                     { return nil }

func (s *delayedStream) Next() bool {
	if s.done {
		return false
	}

	time.Sleep(s.delay)
	s.current = s.event
	s.done = true

	return true
}

func (s *delayedStream) Current() *httpclient.StreamEvent { return s.current }
func (s *delayedStream) Err() error                       { return nil }
func (s *delayedStream) Close() error                     { return nil }

type blockingStream struct {
	nextStarted    chan struct{}
	nextReleased   chan struct{}
	nextReturned   chan struct{}
	releaseOnce    sync.Once
	closeCount     atomic.Int32
	interruptCount atomic.Int32
}

func (s *blockingStream) Next() bool {
	close(s.nextStarted)
	<-s.nextReleased
	close(s.nextReturned)

	return false
}

func (s *blockingStream) Current() *httpclient.StreamEvent { return nil }
func (s *blockingStream) Err() error                       { return nil }
func (s *blockingStream) Close() error {
	s.closeCount.Add(1)
	s.releaseOnce.Do(func() { close(s.nextReleased) })
	return nil
}

func (s *blockingStream) Interrupt() error {
	s.interruptCount.Add(1)
	s.releaseOnce.Do(func() { close(s.nextReleased) })
	return nil
}

type blockingEventStream struct {
	event          *httpclient.StreamEvent
	nextStarted    chan struct{}
	nextReleased   chan struct{}
	current        *httpclient.StreamEvent
	done           bool
	releaseOnce    sync.Once
	closeCount     atomic.Int32
	interruptCount atomic.Int32
}

type terminalThenBlockingStream struct {
	event           *httpclient.StreamEvent
	current         *httpclient.StreamEvent
	emitted         bool
	nextStarted     chan struct{}
	nextStartedOnce sync.Once
	released        chan struct{}
	releaseOnce     sync.Once
	closeCount      atomic.Int32
	interruptCount  atomic.Int32
}

func (s *terminalThenBlockingStream) Next() bool {
	if !s.emitted {
		s.current = s.event
		s.emitted = true
		return true
	}
	s.nextStartedOnce.Do(func() { close(s.nextStarted) })
	<-s.released
	return false
}

func (s *terminalThenBlockingStream) Current() *httpclient.StreamEvent { return s.current }
func (s *terminalThenBlockingStream) Err() error                       { return nil }
func (s *terminalThenBlockingStream) Close() error {
	s.closeCount.Add(1)
	s.releaseOnce.Do(func() { close(s.released) })
	return nil
}
func (s *terminalThenBlockingStream) Interrupt() error {
	s.interruptCount.Add(1)
	s.releaseOnce.Do(func() { close(s.released) })
	return nil
}

func (s *blockingEventStream) Next() bool {
	if s.done {
		return false
	}

	close(s.nextStarted)
	<-s.nextReleased
	s.current = s.event
	s.done = true

	return true
}

func (s *blockingEventStream) Current() *httpclient.StreamEvent { return s.current }
func (s *blockingEventStream) Err() error                       { return nil }
func (s *blockingEventStream) Close() error {
	s.closeCount.Add(1)
	s.release()
	return nil
}

func (s *blockingEventStream) Interrupt() error {
	s.interruptCount.Add(1)
	s.release()
	return nil
}

func (s *blockingEventStream) release() {
	s.releaseOnce.Do(func() { close(s.nextReleased) })
}

type failingResponseWriter struct {
	gin.ResponseWriter

	err    error
	writes int
}

func (w *failingResponseWriter) Write(_ []byte) (int, error) {
	w.writes++

	return 0, w.err
}

type heartbeatFailingResponseWriter struct {
	gin.ResponseWriter

	err    error
	failed chan struct{}
}

type heartbeatObservingResponseWriter struct {
	gin.ResponseWriter
	heartbeat chan struct{}
}

type secondFlushFailingResponseWriter struct {
	gin.ResponseWriter

	err     error
	flushes int
}

func (w *secondFlushFailingResponseWriter) FlushError() error {
	w.flushes++
	if w.flushes == 2 {
		return w.err
	}
	w.ResponseWriter.Flush()
	return nil
}

type closeCountingStream struct {
	streams.Stream[*httpclient.StreamEvent]
	closeCount atomic.Int32
}

type closeSignalStream struct {
	streams.Stream[*httpclient.StreamEvent]
	closed     chan struct{}
	closeOnce  sync.Once
	closeCount atomic.Int32
}

type blockingReadCloser struct {
	err         error
	readStarted chan struct{}
	released    chan struct{}
	startOnce   sync.Once
	closeOnce   sync.Once
	closeCount  atomic.Int32
}

func (r *blockingReadCloser) Read([]byte) (int, error) {
	r.startOnce.Do(func() { close(r.readStarted) })
	<-r.released
	return 0, r.err
}

func (r *blockingReadCloser) Close() error {
	r.closeCount.Add(1)
	r.closeOnce.Do(func() { close(r.released) })
	return nil
}

func (s *closeSignalStream) Close() error {
	s.closeCount.Add(1)
	s.closeOnce.Do(func() { close(s.closed) })
	return s.Stream.Close()
}

func (s *closeCountingStream) Close() error {
	s.closeCount.Add(1)
	return s.Stream.Close()
}

func (w *heartbeatFailingResponseWriter) Write(_ []byte) (int, error) {
	select {
	case w.failed <- struct{}{}:
	default:
	}

	return 0, w.err
}

func (w *heartbeatFailingResponseWriter) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

func (w *heartbeatObservingResponseWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if bytes.Contains(data, []byte("keep-alive")) {
		select {
		case w.heartbeat <- struct{}{}:
		default:
		}
	}

	return n, err
}

func (w *heartbeatObservingResponseWriter) WriteString(data string) (int, error) {
	return w.Write([]byte(data))
}

func TestWriteSSEStream_Success(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	events := []*httpclient.StreamEvent{
		{Type: "", Data: []byte(`{"id":"1","choices":[{"delta":{"content":"Hi"}}]}`)},
		{Type: "", Data: []byte(`[DONE]`)},
	}
	stream := streams.SliceStream(events)

	WriteSSEStream(c, stream)

	body := w.Body.String()
	assert.Contains(t, body, `{"id":"1","choices":[{"delta":{"content":"Hi"}}]}`)
	assert.Contains(t, body, `[DONE]`)
}

func TestWriteSSEStream_OpenAIHeartbeat(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	stream := &delayedStream{
		delay: 25 * time.Millisecond,
		event: &httpclient.StreamEvent{Data: []byte(`[DONE]`)},
	}

	writeSSEStream(c, stream, FormatStreamError, SSEKeepAliveConfig{
		Enabled:  true,
		Interval: 5 * time.Millisecond,
	}, sseHeartbeatOpenAI, nil)

	body := w.Body.String()
	require.Contains(t, body, ": keep-alive\n\n")
	require.Contains(t, body, "data: [DONE]\n\n")
	require.Less(t, strings.LastIndex(body, ": keep-alive"), strings.LastIndex(body, "data: [DONE]"))
}

func TestWriteSSEStream_ResponsesCanceledIsTerminal(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	stream := streams.SliceStream([]*httpclient.StreamEvent{
		{Type: "response.canceled", Data: []byte(`{"type":"response.canceled","response":{"status":"canceled"}}`)},
		{Type: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","delta":"must not be written"}`)},
	})

	writeSSEStream(c, stream, FormatStreamError, SSEKeepAliveConfig{
		Enabled: true, Interval: time.Millisecond,
	}, sseHeartbeatOpenAI, nil)
	require.Contains(t, w.Body.String(), "response.canceled")
	require.NotContains(t, w.Body.String(), "must not be written")
}

func TestWriteSSEStream_AnthropicHeartbeat(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	stream := &delayedStream{
		delay: 25 * time.Millisecond,
		event: &httpclient.StreamEvent{Type: "message_stop", Data: []byte(`{"type":"message_stop"}`)},
	}

	writeSSEStream(c, stream, FormatStreamError, SSEKeepAliveConfig{
		Enabled:  true,
		Interval: 5 * time.Millisecond,
	}, sseHeartbeatAnthropic, nil)

	body := w.Body.String()
	require.Contains(t, body, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
	require.Contains(t, body, "event:message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}

func TestWriteSSEStream_SemanticSuccessInterruptsBlockedTailOnce(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	heartbeats := &heartbeatObservingResponseWriter{ResponseWriter: c.Writer, heartbeat: make(chan struct{}, 1)}
	c.Writer = heartbeats
	ctx, cancel := requestWithSSELivenessContext(c)
	defer cancel(nil)

	var confirmed atomic.Int32
	session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{Enabled: true, Interval: 5 * time.Millisecond}, sseHeartbeatOpenAI, true)
	session.terminalGrace = 20 * time.Millisecond
	finalChunk := &httpclient.StreamEvent{
		Data: []byte(`{"id":"chatcmpl-144325","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":9,"total_tokens":19}}`),
	}
	stream := &terminalThenBlockingStream{
		event: finalChunk, nextStarted: make(chan struct{}), released: make(chan struct{}),
	}
	owned := newCloseOnceStream(stream)
	session.OnUpstreamResponseHeaders(orchestrator.StreamLivenessAttempt{
		Interrupt: stream.Interrupt,
		ConfirmSemanticCompletion: func() bool {
			confirmed.Add(1)
			return true
		},
	})

	done := make(chan struct{})
	go func() {
		writeSSEStreamWithHeartbeat(c, owned, FormatStreamError, 5*time.Millisecond, sseHeartbeatOpenAI, session)
		close(done)
	}()

	select {
	case <-stream.nextStarted:
	case <-time.After(time.Second):
		t.Fatal("reader did not block after the semantic final chunk")
	}
	select {
	case <-heartbeats.heartbeat:
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not keep the pre-terminal blocked stream alive")
	}
	session.OnRawSSE(finalChunk)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("semantic terminal grace did not stop the blocked stream")
	}

	body := w.Body.String()
	require.Contains(t, body, `"finish_reason":"tool_calls"`)
	require.Contains(t, body, "data: [DONE]\n\n")
	require.Less(t, strings.LastIndex(body, ": keep-alive"), strings.LastIndex(body, "data: [DONE]"))
	require.Equal(t, int32(1), confirmed.Load())
	require.Equal(t, int32(1), stream.interruptCount.Load())
	require.NoError(t, owned.Close())
	require.Equal(t, int32(1), stream.closeCount.Load())
	require.Equal(t, sseCloseSemanticCompletion, session.ledger.reason())
}

func TestWriteSSEStream_ExplicitSuccessConfirmsBeforeInterruptingBlockedTail(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx, cancel := requestWithSSELivenessContext(c)
	defer cancel(nil)

	var confirmed atomic.Int32
	session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{Enabled: true, Interval: time.Hour}, sseHeartbeatOpenAI, true)
	stream := &terminalThenBlockingStream{
		event: &httpclient.StreamEvent{Data: []byte(`[DONE]`)}, nextStarted: make(chan struct{}), released: make(chan struct{}),
	}
	owned := newCloseOnceStream(stream)
	session.OnUpstreamResponseHeaders(orchestrator.StreamLivenessAttempt{
		Interrupt: stream.Interrupt,
		ConfirmSemanticCompletion: func() bool {
			confirmed.Add(1)
			return true
		},
	})

	writeSSEStreamWithHeartbeat(c, owned, FormatStreamError, time.Hour, sseHeartbeatOpenAI, session)
	require.Contains(t, w.Body.String(), "data: [DONE]\n\n")
	require.Equal(t, int32(1), confirmed.Load())
	require.Equal(t, int32(1), stream.interruptCount.Load())
	require.NoError(t, owned.Close())
	require.Equal(t, int32(1), stream.closeCount.Load())
	require.Equal(t, sseCloseTerminalEvent, session.ledger.reason())
}

func TestWriteSSEStream_GenuineErrorDuringSemanticGraceWins(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx, cancel := requestWithSSELivenessContext(c)
	defer cancel(nil)

	var confirmed atomic.Int32
	session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{}, sseHeartbeatOpenAI, true)
	session.terminalGrace = 100 * time.Millisecond
	session.OnUpstreamResponseHeaders(orchestrator.StreamLivenessAttempt{
		ConfirmSemanticCompletion: func() bool {
			confirmed.Add(1)
			return true
		},
	})
	finalChunk := &httpclient.StreamEvent{Data: []byte(`{"id":"chatcmpl-grace-error","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)}
	session.OnRawSSE(finalChunk)
	streamErr := errors.New("provider failed during terminal grace")
	stream := &errorAfterStream{items: []*httpclient.StreamEvent{finalChunk}, err: streamErr}

	writeSSEStreamWithHeartbeat(c, stream, FormatStreamError, 0, sseHeartbeatOpenAI, session)
	body := w.Body.String()
	require.Contains(t, body, "provider failed during terminal grace")
	require.Contains(t, body, "event:error")
	require.NotContains(t, body, "data: [DONE]\n\n")
	require.Zero(t, confirmed.Load())
	require.Equal(t, sseCloseUpstreamError, session.ledger.reason())
}

func TestWriteSSEStream_SlowPreTerminalStreamIsNotTruncated(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx, cancel := requestWithSSELivenessContext(c)
	defer cancel(nil)
	session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{Enabled: true, Interval: 5 * time.Millisecond}, sseHeartbeatOpenAI, true)
	session.terminalGrace = time.Millisecond

	stream := &delayedSequenceStream{
		delay: 20 * time.Millisecond,
		events: []*httpclient.StreamEvent{
			{Data: []byte(`{"choices":[{"delta":{"content":"still working"},"finish_reason":null}]}`)},
			{Data: []byte(`[DONE]`)},
		},
	}
	writeSSEStreamWithHeartbeat(c, stream, FormatStreamError, 5*time.Millisecond, sseHeartbeatOpenAI, session)

	body := w.Body.String()
	require.Contains(t, body, "still working")
	require.Contains(t, body, ": keep-alive\n\n")
	require.Contains(t, body, "data: [DONE]\n\n")
	require.Equal(t, sseCloseTerminalEvent, session.ledger.reason())
}

func TestSSESemanticTerminalTracker_SynthesizesCompletedResponsesSnapshot(t *testing.T) {
	tracker := newSSESemanticTerminalTracker(sseHeartbeatOpenAI)
	tracker.observe(&httpclient.StreamEvent{
		Type: "response.created",
		Data: []byte(`{"type":"response.created","sequence_number":0,"response":{"id":"resp_144325","object":"response","status":"in_progress","model":"grok-4.1","output":[]}}`),
	})
	for i := range 9 {
		tracker.observe(&httpclient.StreamEvent{
			Type: "response.output_item.done",
			Data: []byte(fmt.Sprintf(`{"type":"response.output_item.done","sequence_number":%d,"output_index":%d,"item":{"id":"fc_%d","type":"function_call","status":"completed","call_id":"call_%d","name":"tool","arguments":"{}"}}`, i+1, i, i, i)),
		})
	}

	w := httptest.NewRecorder()
	require.NoError(t, tracker.writeFallback(w, &llm.Response{
		ID: "resp_144325", Model: "grok-4.1",
		Usage: &llm.Usage{PromptTokens: 100, CompletionTokens: 90, TotalTokens: 190},
	}))

	body := w.Body.String()
	require.Contains(t, body, "event:response.completed")
	dataLine := ""
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data:") {
			dataLine = strings.TrimPrefix(line, "data:")
			break
		}
	}
	require.NotEmpty(t, dataLine)
	require.Equal(t, "completed", gjson.Get(dataLine, "response.status").String())
	require.Len(t, gjson.Get(dataLine, "response.output").Array(), 9)
	require.Equal(t, int64(190), gjson.Get(dataLine, "response.usage.total_tokens").Int())
}

func TestSSELivenessSession_WaitsForEveryRequestedChoice(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{}, sseHeartbeatOpenAI, true)
	session.expectedChoices = 2

	session.OnRawSSE(&httpclient.StreamEvent{Data: []byte(`{"choices":[{"index":0,"finish_reason":"stop"}]}`)})
	select {
	case <-session.semanticSuccessSignal():
		t.Fatal("one completed choice must not terminate an n=2 stream")
	default:
	}

	session.OnRawSSE(&httpclient.StreamEvent{Data: []byte(`{"choices":[{"index":1,"finish_reason":"tool_calls"}]}`)})
	select {
	case <-session.semanticSuccessSignal():
	case <-time.After(time.Second):
		t.Fatal("all requested choices completed without signaling semantic success")
	}
}

func TestSSELivenessSession_CommitsAndHeartbeatsOnlyForConfirmedStream(t *testing.T) {
	for _, tt := range []struct {
		name      string
		streaming bool
		wantSSE   bool
	}{
		{name: "streaming request", streaming: true, wantSSE: true},
		{name: "non-streaming request", streaming: false, wantSSE: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			observingWriter := &heartbeatObservingResponseWriter{ResponseWriter: c.Writer, heartbeat: make(chan struct{}, 1)}
			c.Writer = observingWriter

			ctx, cancel := requestWithSSELivenessContext(c)
			defer cancel(nil)
			session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{Enabled: true, Interval: 5 * time.Millisecond}, sseHeartbeatOpenAI, tt.streaming)

			processStarted := make(chan struct{})
			releaseProcess := make(chan struct{})
			result := make(chan struct{ committed, aborted bool }, 1)
			go func() {
				_, committed, aborted := session.awaitProcess(c, func(
					_ context.Context,
					_ *httpclient.Request,
					observer orchestrator.StreamLivenessObserver,
				) (orchestrator.ChatCompletionResult, error) {
					observer.OnUpstreamAttemptSelected(orchestrator.StreamLivenessAttempt{})
					close(processStarted)
					<-releaseProcess
					return orchestrator.ChatCompletionResult{}, errors.New("upstream unavailable")
				}, &httpclient.Request{})
				result <- struct{ committed, aborted bool }{committed: committed, aborted: aborted}
			}()

			select {
			case <-processStarted:
			case <-time.After(time.Second):
				t.Fatal("process did not start")
			}

			if tt.wantSSE {
				select {
				case <-observingWriter.heartbeat:
				case <-time.After(time.Second):
					t.Fatal("heartbeat was not emitted while process was blocked")
				}
				require.Equal(t, sse.ContentType, recorder.Header().Get("Content-Type"))
				require.Equal(t, "*", recorder.Header().Get("Access-Control-Allow-Origin"))
				require.Contains(t, recorder.Body.String(), ": keep-alive\n\n")
			} else {
				select {
				case <-observingWriter.heartbeat:
					t.Fatal("non-streaming request emitted an SSE heartbeat")
				case <-time.After(25 * time.Millisecond):
				}
				require.Empty(t, recorder.Header().Get("Content-Type"))
				require.Empty(t, recorder.Body.String())
			}

			close(releaseProcess)
			got := <-result
			require.Equal(t, tt.wantSSE, got.committed)
			require.False(t, got.aborted)
		})
	}
}

func TestSSELivenessSession_HonorsChannelOverrideBeforeProviderSetup(t *testing.T) {
	tests := []struct {
		name           string
		globalEnabled  bool
		channelEnabled bool
		wantHeartbeat  bool
	}{
		{
			name:           "channel disabled overrides global enabled",
			globalEnabled:  true,
			channelEnabled: false,
			wantHeartbeat:  false,
		},
		{
			name:           "channel enabled overrides global disabled",
			globalEnabled:  false,
			channelEnabled: true,
			wantHeartbeat:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			observingWriter := &heartbeatObservingResponseWriter{
				ResponseWriter: c.Writer,
				heartbeat:      make(chan struct{}, 1),
			}
			c.Writer = observingWriter

			ctx, cancel := requestWithSSELivenessContext(c)
			defer cancel(nil)
			session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{
				Enabled:  tt.globalEnabled,
				Interval: 5 * time.Millisecond,
			}, sseHeartbeatOpenAI, true)

			selected := make(chan struct{})
			releaseProviderSetup := make(chan struct{})
			result := make(chan bool, 1)
			go func() {
				_, committed, _ := session.awaitProcess(c, func(
					_ context.Context,
					_ *httpclient.Request,
					observer orchestrator.StreamLivenessObserver,
				) (orchestrator.ChatCompletionResult, error) {
					observer.OnUpstreamAttemptSelected(orchestrator.StreamLivenessAttempt{
						KeepAlive: &objects.ChannelSSEKeepAlive{Enabled: &tt.channelEnabled},
					})
					close(selected)
					<-releaseProviderSetup
					return orchestrator.ChatCompletionResult{}, errors.New("provider setup failed")
				}, &httpclient.Request{})
				result <- committed
			}()

			<-selected
			if tt.wantHeartbeat {
				select {
				case <-observingWriter.heartbeat:
				case <-time.After(time.Second):
					t.Fatal("channel-enabled request did not heartbeat during provider setup")
				}
			} else {
				select {
				case <-observingWriter.heartbeat:
					t.Fatal("channel-disabled request emitted a heartbeat during provider setup")
				case <-time.After(50 * time.Millisecond):
				}
			}

			close(releaseProviderSetup)
			require.Equal(t, tt.wantHeartbeat, <-result)
			require.Equal(t, tt.wantHeartbeat, strings.Contains(recorder.Body.String(), ": keep-alive\n\n"))
		})
	}
}

func TestSSELivenessSession_HeartbeatDuringProcessPreRead(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	ctx, cancel := requestWithSSELivenessContext(c)
	session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{
		Enabled:  false,
		Interval: 5 * time.Millisecond,
	}, sseHeartbeatOpenAI, true)

	enabled := true
	process := func(
		_ context.Context,
		_ *httpclient.Request,
		observer orchestrator.StreamLivenessObserver,
	) (orchestrator.ChatCompletionResult, error) {
		observer.OnUpstreamAttemptSelected(orchestrator.StreamLivenessAttempt{
			ChannelID:   42,
			ChannelName: "DeepSeek Pro",
			KeepAlive: &objects.ChannelSSEKeepAlive{
				Enabled: &enabled,
			},
		})
		observer.OnUpstreamResponseHeaders(orchestrator.StreamLivenessAttempt{
			ChannelID:   42,
			ChannelName: "DeepSeek Pro",
			KeepAlive: &objects.ChannelSSEKeepAlive{
				Enabled: &enabled,
			},
		})
		time.Sleep(25 * time.Millisecond)
		observer.OnRawSSE(&httpclient.StreamEvent{Data: []byte(`{"choices":[{"finish_reason":null}]}`)})
		observer.OnTransformableEvent(&llm.Response{Object: "chat.completion.chunk"})

		return orchestrator.ChatCompletionResult{
			ChatCompletionStream: streams.SliceStream([]*httpclient.StreamEvent{{Data: []byte("[DONE]")}}),
		}, nil
	}

	outcome, committed, aborted := session.awaitProcess(c, process, &httpclient.Request{})
	require.False(t, aborted)
	require.True(t, committed)
	require.NoError(t, outcome.err)
	require.NotNil(t, outcome.result.ChatCompletionStream)
	require.Contains(t, w.Body.String(), ": keep-alive\n\n")
	require.True(t, session.effectiveConfig().Enabled, "channel override should enable keep-alive independently")
	session.finish(sseCloseStreamCompleted)
}

func TestSSELivenessSession_PreReadHeartbeatFailureCancelsAndClosesLateStream(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx, cancel := requestWithSSELivenessContext(c)
	session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{
		Enabled:  true,
		Interval: time.Millisecond,
	}, sseHeartbeatOpenAI, true)

	failingWriter := &heartbeatFailingResponseWriter{
		ResponseWriter: c.Writer,
		err:            errors.New("broken pipe"),
		failed:         make(chan struct{}, 1),
	}
	c.Writer = failingWriter

	lateStream := &closeSignalStream{
		Stream: streams.SliceStream([]*httpclient.StreamEvent(nil)),
		closed: make(chan struct{}),
	}
	process := func(
		processCtx context.Context,
		_ *httpclient.Request,
		observer orchestrator.StreamLivenessObserver,
	) (orchestrator.ChatCompletionResult, error) {
		observer.OnUpstreamResponseHeaders(orchestrator.StreamLivenessAttempt{})
		<-processCtx.Done()
		return orchestrator.ChatCompletionResult{ChatCompletionStream: lateStream}, nil
	}

	_, committed, aborted := session.awaitProcess(c, process, &httpclient.Request{})
	require.True(t, committed)
	require.True(t, aborted)
	require.Equal(t, "downstream_write_failed", context.Cause(ctx).Error())

	select {
	case <-lateStream.closed:
	case <-time.After(time.Second):
		t.Fatal("late process result stream was not closed after downstream failure")
	}
	require.Equal(t, int32(1), lateStream.closeCount.Load())
}

func TestResolveSSEKeepAlive_ChannelInheritanceAndOverride(t *testing.T) {
	global := SSEKeepAliveConfig{Enabled: true, Interval: 15 * time.Second}

	require.Equal(t, global, resolveSSEKeepAlive(global, nil))

	disabled := false
	intervalSeconds := 30
	require.Equal(t, SSEKeepAliveConfig{
		Enabled:  false,
		Interval: 30 * time.Second,
	}, resolveSSEKeepAlive(global, &objects.ChannelSSEKeepAlive{
		Enabled:         &disabled,
		IntervalSeconds: &intervalSeconds,
	}))
}

func TestSSELivenessSession_ContextCauseWinsOutcomeRace(t *testing.T) {
	tests := []struct {
		name       string
		cause      error
		wantReason sseCloseCause
	}{
		{name: "client disconnect", cause: context.Canceled, wantReason: sseCloseClientDisconnect},
		{name: "request deadline", cause: context.DeadlineExceeded, wantReason: sseCloseRequestDeadline},
		{name: "request cancellation cause", cause: errors.New("server shutdown"), wantReason: sseCloseRequestContextCanceled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequestWithContext(ctx, http.MethodPost, "/", nil)
			session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{}, sseHeartbeatOpenAI, true)
			processStarted := make(chan struct{})
			releaseProcess := make(chan struct{})
			result := make(chan struct {
				committed bool
				aborted   bool
			}, 1)

			go func() {
				_, committed, aborted := session.awaitProcess(c, func(
					context.Context,
					*httpclient.Request,
					orchestrator.StreamLivenessObserver,
				) (orchestrator.ChatCompletionResult, error) {
					close(processStarted)
					<-releaseProcess
					return orchestrator.ChatCompletionResult{}, errors.New("upstream fallback")
				}, &httpclient.Request{})
				result <- struct {
					committed bool
					aborted   bool
				}{committed: committed, aborted: aborted}
			}()

			<-processStarted
			cancel(tt.cause)
			close(releaseProcess)
			got := <-result
			require.False(t, got.committed)
			require.True(t, got.aborted)
			require.Equal(t, tt.wantReason, session.ledger.reason())
		})
	}
}

func TestSSELivenessSession_ExistingCancellationWinsDownstreamFailure(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{}, sseHeartbeatOpenAI, true)
	requestCause := errors.New("server shutdown")
	cancel(requestCause)

	session.failDownstream(errors.New("broken pipe"))

	require.ErrorIs(t, context.Cause(ctx), requestCause)
	require.Equal(t, sseCloseRequestContextCanceled, session.ledger.reason())
}

func TestSSELivenessSession_DisabledAttemptWinsPendingHeartbeatTick(t *testing.T) {
	w := httptest.NewRecorder()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{
		Enabled:  true,
		Interval: time.Millisecond,
	}, sseHeartbeatOpenAI, true)

	enabled := true
	disabled := false
	session.OnUpstreamResponseHeaders(orchestrator.StreamLivenessAttempt{
		KeepAlive: &objects.ChannelSSEKeepAlive{Enabled: &enabled},
	})
	session.OnUpstreamResponseHeaders(orchestrator.StreamLivenessAttempt{
		KeepAlive: &objects.ChannelSSEKeepAlive{Enabled: &disabled},
	})

	wrote, _, err := session.writeHeartbeatIfEnabled(w)
	require.NoError(t, err)
	require.False(t, wrote)
	require.Empty(t, w.Body.String())
}

func TestWriteSSEStream_BusinessActivityResetsHeartbeatTimer(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	stream := &delayedSequenceStream{
		delay: 120 * time.Millisecond,
		events: []*httpclient.StreamEvent{
			{Data: []byte(`{"choices":[{"delta":{"content":"hello"}}]}`)},
			{Data: []byte(`[DONE]`)},
		},
	}

	writeSSEStream(c, stream, FormatStreamError, SSEKeepAliveConfig{
		Enabled:  true,
		Interval: 200 * time.Millisecond,
	}, sseHeartbeatOpenAI, nil)

	require.NotContains(t, w.Body.String(), ": keep-alive\n\n",
		"the successful business write should move the heartbeat deadline past the terminal event")
}

func TestWriteSSEStream_HeartbeatWriteErrorClosesReader(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	ctx, cancel := requestWithSSELivenessContext(c)
	session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{
		Enabled:  true,
		Interval: time.Millisecond,
	}, sseHeartbeatOpenAI, true)

	failingWriter := &heartbeatFailingResponseWriter{
		ResponseWriter: c.Writer,
		err:            errors.New("broken pipe"),
		failed:         make(chan struct{}, 1),
	}
	c.Writer = failingWriter

	stream := &blockingStream{
		nextStarted:  make(chan struct{}),
		nextReleased: make(chan struct{}),
		nextReturned: make(chan struct{}),
	}
	ownedStream := newCloseOnceStream(stream)
	session.OnUpstreamResponseHeaders(orchestrator.StreamLivenessAttempt{Interrupt: stream.Interrupt})

	done := make(chan struct{})
	go func() {
		writeSSEStreamWithHeartbeat(c, ownedStream, FormatStreamError, time.Millisecond, sseHeartbeatOpenAI, session)
		close(done)
	}()

	select {
	case <-failingWriter.failed:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for heartbeat write failure")
	}

	select {
	case <-stream.nextStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream reader")
	}

	select {
	case <-stream.nextReturned:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream reader to stop")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SSE writer")
	}
	require.ErrorIs(t, context.Cause(ctx), context.Canceled)
	require.Equal(t, "downstream_write_failed", context.Cause(ctx).Error())
	require.NoError(t, ownedStream.Close())
	require.Equal(t, int32(1), stream.closeCount.Load())
	require.Equal(t, int32(1), stream.interruptCount.Load())
}

func TestWriteSSEStream_DownstreamFailureInterruptsDecoderBeforePersistentClose(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	ctx, cancel := requestWithSSELivenessContext(c)
	session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{
		Enabled:  true,
		Interval: time.Millisecond,
	}, sseHeartbeatOpenAI, true)

	failingWriter := &heartbeatFailingResponseWriter{
		ResponseWriter: c.Writer,
		err:            errors.New("broken pipe"),
		failed:         make(chan struct{}, 1),
	}
	c.Writer = failingWriter

	rawReader := &blockingReadCloser{
		err: &net.OpError{
			Op:  "read",
			Net: "tcp",
			Err: net.ErrClosed,
		},
		readStarted: make(chan struct{}),
		released:    make(chan struct{}),
	}
	decoder := httpclient.NewDefaultSSEDecoder(ctx, rawReader)
	interruptible := decoder.(streams.Interruptible)
	state := &orchestrator.PersistenceState{}
	persistent := orchestrator.NewInboundPersistentStream(ctx, decoder, nil, nil, nil, nil, nil, state)
	owned := newCloseOnceStream(persistent)
	session.OnUpstreamResponseHeaders(orchestrator.StreamLivenessAttempt{Interrupt: interruptible.Interrupt})

	writeSSEStreamWithHeartbeat(c, owned, FormatStreamError, time.Millisecond, sseHeartbeatOpenAI, session)
	select {
	case <-rawReader.readStarted:
	default:
		t.Fatal("decoder Next never reached the blocking reader")
	}
	require.NoError(t, decoder.Err(),
		"reader.Stop's local interrupt must not turn downstream teardown into an upstream failure")
	require.Equal(t, sseCloseDownstreamWriteFailed, session.ledger.reason())
	require.NoError(t, owned.Close())
	require.Equal(t, int32(1), rawReader.closeCount.Load())
}

func TestWriteSSEStream_BusinessWriteErrorCancelsAndClosesOnce(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	ctx, cancel := requestWithSSELivenessContext(c)
	session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{
		Enabled:  true,
		Interval: time.Hour,
	}, sseHeartbeatOpenAI, true)

	failingWriter := &heartbeatFailingResponseWriter{
		ResponseWriter: c.Writer,
		err:            errors.New("broken pipe"),
		failed:         make(chan struct{}, 1),
	}
	c.Writer = failingWriter

	underlying := &closeCountingStream{
		Stream: streams.SliceStream([]*httpclient.StreamEvent{{
			Data: []byte(`{"choices":[{"delta":{"content":"hello"}}]}`),
		}}),
	}
	owned := newCloseOnceStream(underlying)
	writeSSEStreamWithHeartbeat(c, owned, FormatStreamError, time.Hour, sseHeartbeatOpenAI, session)

	require.ErrorIs(t, context.Cause(ctx), context.Canceled)
	require.Equal(t, "downstream_write_failed", context.Cause(ctx).Error())
	require.NoError(t, owned.Close())
	require.Equal(t, int32(1), underlying.closeCount.Load())
}

func TestWriteSSEStream_BusinessFlushErrorCancelsUpstream(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	ctx, cancel := requestWithSSELivenessContext(c)
	session := newSSELivenessSession(ctx, cancel, SSEKeepAliveConfig{
		Enabled:  true,
		Interval: time.Hour,
	}, sseHeartbeatOpenAI, true)

	failingWriter := &secondFlushFailingResponseWriter{
		ResponseWriter: c.Writer,
		err:            errors.New("flush failed"),
	}
	c.Writer = failingWriter

	underlying := &closeCountingStream{
		Stream: streams.SliceStream([]*httpclient.StreamEvent{{Data: []byte(`{"choices":[]}`)}}),
	}
	owned := newCloseOnceStream(underlying)
	writeSSEStreamWithHeartbeat(c, owned, FormatStreamError, time.Hour, sseHeartbeatOpenAI, session)

	require.Equal(t, 2, failingWriter.flushes)
	require.Equal(t, "downstream_write_failed", context.Cause(ctx).Error())
	require.NoError(t, owned.Close())
	require.Equal(t, int32(1), underlying.closeCount.Load())
}

func TestWriteSSEStream_CanceledContextClosesHeartbeatReader(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)

	stream := &blockingEventStream{
		event:        &httpclient.StreamEvent{Data: []byte(`[DONE]`)},
		nextStarted:  make(chan struct{}),
		nextReleased: make(chan struct{}),
	}

	done := make(chan struct{})
	go func() {
		writeSSEStreamWithHeartbeat(c, stream, FormatStreamError, time.Hour, sseHeartbeatOpenAI, nil)
		close(done)
	}()

	select {
	case <-stream.nextStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for stream reader")
	}

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for canceled SSE writer")
	}

	assert.NotContains(t, w.Body.String(), "data: [DONE]\n\n")
	assert.NotContains(t, w.Body.String(), "keep-alive")
	require.NoError(t, stream.Close())
	require.Equal(t, int32(1), stream.closeCount.Load())
	require.Equal(t, int32(1), stream.interruptCount.Load())
}

func TestWriteSSEStream_DefaultHasNoHeartbeat(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	stream := &delayedStream{
		delay: 10 * time.Millisecond,
		event: &httpclient.StreamEvent{Data: []byte(`[DONE]`)},
	}

	WriteSSEStream(c, stream)

	require.NotContains(t, w.Body.String(), "keep-alive")
}

func TestWriteSSEStream_CanceledContextDoesNotWriteBufferedEvents(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c.Request = httptest.NewRequestWithContext(ctx, http.MethodGet, "/", nil)

	events := []*httpclient.StreamEvent{
		{Type: "", Data: []byte(`{"id":"1","choices":[{"delta":{"content":"Hi"}}]}`)},
		{Type: "", Data: []byte(`[DONE]`)},
	}
	stream := streams.SliceStream(events)

	WriteSSEStream(c, stream)

	body := w.Body.String()
	assert.Empty(t, body)
	assert.NotContains(t, body, `"error"`)
}

func TestWriteSSEStream_ErrorFormatsAsJSON(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	streamErr := errors.New("upstream connection reset")
	stream := &errorAfterStream{
		items: []*httpclient.StreamEvent{
			{Type: "", Data: []byte(`{"id":"1","choices":[{"delta":{"content":"He"}}]}`)},
		},
		err: streamErr,
	}

	WriteSSEStream(c, stream)

	body := w.Body.String()

	// The error event should be JSON-formatted, not a plain string
	assert.Contains(t, body, "event:error")

	// Extract the data line from the error event
	lines := strings.Split(body, "\n")

	var errorData string

	foundError := false

	for i, line := range lines {
		if strings.HasPrefix(line, "event:error") {
			foundError = true
			// The next line should be the data
			if i+1 < len(lines) {
				errorData = strings.TrimPrefix(lines[i+1], "data:")
			}

			break
		}
	}

	require.True(t, foundError, "should contain an error event")
	require.NotEmpty(t, errorData, "error event should have data")

	// Parse the JSON error
	var errObj map[string]any

	err := json.Unmarshal([]byte(errorData), &errObj)
	require.NoError(t, err, "error data should be valid JSON: %s", errorData)

	// Verify structure
	errorField, ok := errObj["error"].(map[string]any)
	require.True(t, ok, "should have 'error' field")
	assert.Equal(t, "upstream connection reset", errorField["message"])
	assert.Equal(t, "server_error", errorField["type"])
	_, hasCode := errorField["code"]
	assert.True(t, hasCode)
}

func TestWriteSSEStream_HttpClientError(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	httpErr := &httpclient.Error{
		StatusCode: 429,
		Body:       []byte(`{"error":{"message":"Rate limit exceeded","type":"rate_limit_error"}}`),
	}
	stream := &errorAfterStream{err: httpErr}

	WriteSSEStream(c, stream)

	body := w.Body.String()

	// Extract error data
	lines := strings.Split(body, "\n")

	var errorData string

	for i, line := range lines {
		if strings.HasPrefix(line, "event:error") {
			if i+1 < len(lines) {
				errorData = strings.TrimPrefix(lines[i+1], "data:")
			}

			break
		}
	}

	require.NotEmpty(t, errorData)

	var errObj map[string]any

	err := json.Unmarshal([]byte(errorData), &errObj)
	require.NoError(t, err)

	errorField, ok := errObj["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "Rate limit exceeded", errorField["message"])
	assert.Equal(t, "rate_limit_error", errorField["type"])
	assert.Empty(t, errorField["code"])
}

func TestWriteSSEStream_CustomErrorFormatter(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	streamErr := errors.New("custom error")
	stream := &errorAfterStream{err: streamErr}

	customFormatter := func(_ context.Context, err error) any {
		return gin.H{"custom_error": err.Error()}
	}

	WriteSSEStreamWithErrorFormatter(c, stream, customFormatter)

	body := w.Body.String()
	lines := strings.Split(body, "\n")

	var errorData string

	for i, line := range lines {
		if strings.HasPrefix(line, "event:error") {
			if i+1 < len(lines) {
				errorData = strings.TrimPrefix(lines[i+1], "data:")
			}

			break
		}
	}

	require.NotEmpty(t, errorData)

	var errObj map[string]any

	err := json.Unmarshal([]byte(errorData), &errObj)
	require.NoError(t, err)
	assert.Equal(t, "custom error", errObj["custom_error"])
}

func TestWriteSSEStream_NoError(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	stream := streams.SliceStream([]*httpclient.StreamEvent{
		{Type: "", Data: []byte(`[DONE]`)},
	})

	WriteSSEStream(c, stream)

	body := w.Body.String()
	assert.NotContains(t, body, "event:error")
}

func TestWriteBinaryStream(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	stream := streams.SliceStream([]*httpclient.StreamEvent{
		{Type: "audio/mpeg", Data: []byte{0x01, 0x02}},
		{Type: "audio/mpeg", Data: []byte{0x03, 0x04, 0x05}},
	})

	WriteBinaryStream(c, stream)

	require.Equal(t, "audio/mpeg", w.Header().Get("Content-Type"))
	require.Equal(t, []byte{0x01, 0x02, 0x03, 0x04, 0x05}, w.Body.Bytes())
}

func TestWriteBinaryStream_ErrorBeforeFirstChunkReturnsJSON(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	stream := &errorAfterStream{
		err: &httpclient.Error{
			StatusCode: http.StatusTooManyRequests,
			Body:       []byte(`{"error":{"message":"Rate limit exceeded","type":"rate_limit_error","code":"rate_limit"},"request_id":"req_1"}`),
		},
	}

	WriteBinaryStream(c, stream)

	require.Equal(t, http.StatusTooManyRequests, w.Code)
	require.Equal(t, "application/json; charset=utf-8", w.Header().Get("Content-Type"))

	var body map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	errorField, ok := body["error"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "rate_limit_error", errorField["type"])
	require.Equal(t, "rate_limit", errorField["code"])
	require.Equal(t, "req_1", body["request_id"])
}

func TestWriteBinaryStream_WriteErrorStopsConsuming(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)

	failingWriter := &failingResponseWriter{
		ResponseWriter: c.Writer,
		err:            errors.New("broken pipe"),
	}
	c.Writer = failingWriter

	stream := &trackingStream{
		items: []*httpclient.StreamEvent{
			{Type: "audio/mpeg", Data: []byte{0x01}},
			{Type: "audio/mpeg", Data: []byte{0x02}},
		},
	}

	WriteBinaryStream(c, stream)

	require.Equal(t, 1, failingWriter.writes)
	require.Equal(t, 1, stream.idx)
	require.Empty(t, w.Body.Bytes())
}

func TestFormatStreamError_PlainError(t *testing.T) {
	err := errors.New("something went wrong")
	result := FormatStreamError(context.Background(), err)

	data, marshalErr := json.Marshal(result)
	require.NoError(t, marshalErr)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(data, &parsed))

	errorField := parsed["error"].(map[string]any)
	assert.Equal(t, "something went wrong", errorField["message"])
	assert.Equal(t, "server_error", errorField["type"])
	assert.Equal(t, "", errorField["code"])
}

func TestFormatStreamError_HttpClientError(t *testing.T) {
	httpErr := &httpclient.Error{
		StatusCode: 500,
		Body:       []byte(`{"error":{"message":"Internal server error","type":"internal_error"}}`),
	}
	result := FormatStreamError(context.Background(), httpErr)

	data, marshalErr := json.Marshal(result)
	require.NoError(t, marshalErr)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(data, &parsed))

	errorField := parsed["error"].(map[string]any)
	assert.Equal(t, "Internal server error", errorField["message"])
	assert.Equal(t, "internal_error", errorField["type"])
	assert.Equal(t, "", errorField["code"])
}

func TestFormatStreamError_QuotaExhaustedError(t *testing.T) {
	quotaErr := orchestrator.NewQuotaExhaustedError("gpt-4")
	result := FormatStreamError(context.Background(), quotaErr)

	data, marshalErr := json.Marshal(result)
	require.NoError(t, marshalErr)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(data, &parsed))

	errorField := parsed["error"].(map[string]any)
	assert.Equal(t, "all channels quota exhausted for model gpt-4", errorField["message"])
	assert.Equal(t, "quota_exhausted", errorField["type"])
	assert.Equal(t, "quota_exhausted", errorField["code"])
}

func TestWrapQuotaExhaustedAsResponseError_QuotaError(t *testing.T) {
	quotaErr := orchestrator.NewQuotaExhaustedError("gpt-4")
	result := wrapQuotaExhaustedAsResponseError(quotaErr)

	respErr := &llm.ResponseError{}
	ok := errors.As(result, &respErr)
	require.True(t, ok, "should convert to *llm.ResponseError")
	assert.Equal(t, http.StatusServiceUnavailable, respErr.StatusCode)
	assert.Equal(t, "all channels quota exhausted for model gpt-4", respErr.Detail.Message)
	assert.Equal(t, "quota_exhausted", respErr.Detail.Type)
	assert.Equal(t, "quota_exhausted", respErr.Detail.Code)
}

func TestWrapQuotaExhaustedAsResponseError_OtherError(t *testing.T) {
	otherErr := errors.New("something else")
	result := wrapQuotaExhaustedAsResponseError(otherErr)
	assert.Equal(t, otherErr, result, "non-quota errors should pass through unchanged")
}

func TestPlaygroundHandleError_QuotaExhausted_Returns503(t *testing.T) {
	handlers := &PlaygroundHandlers{}

	quotaErr := orchestrator.NewQuotaExhaustedError("gpt-4")
	errResp := handlers.HandleError(quotaErr)

	assert.Equal(t, http.StatusServiceUnavailable, errResp.Status)
	assert.Equal(t, http.StatusServiceUnavailable, errResp.Error.Code)
	assert.Equal(t, "all channels quota exhausted for model gpt-4", errResp.Error.Message)
}

func TestPlaygroundHandleError_OtherError_Returns500(t *testing.T) {
	handlers := &PlaygroundHandlers{}

	otherErr := errors.New("something else")
	errResp := handlers.HandleError(otherErr)

	assert.Equal(t, http.StatusInternalServerError, errResp.Status)
}

func TestFormatStreamError_LlmResponseError_PassesCodeAndRequestID(t *testing.T) {
	respErr := &llm.ResponseError{
		Detail: llm.ErrorDetail{
			Code:      "1311",
			Message:   "当前订阅套餐暂未开放GPT-6权限",
			Type:      "permission_error",
			RequestID: "202603112254417d15bd26697445b0",
		},
	}

	result := FormatStreamError(context.Background(), respErr)
	data, marshalErr := json.Marshal(result)
	require.NoError(t, marshalErr)

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(data, &parsed))

	errorField := parsed["error"].(map[string]any)
	assert.Equal(t, "当前订阅套餐暂未开放GPT-6权限", errorField["message"])
	assert.Equal(t, "permission_error", errorField["type"])
	assert.Equal(t, "1311", errorField["code"])
	assert.Equal(t, "202603112254417d15bd26697445b0", parsed["request_id"])
}

func TestApplyUpstreamErrorPolicy_CustomMessage(t *testing.T) {
	ctx, systemService := setupUpstreamErrorPolicyTest(t, biz.UpstreamErrorPolicy{
		Mode:          biz.UpstreamErrorModeCustom,
		CustomMessage: "模型服务暂时不可用，请稍后再试",
	})

	rawErr := &httpclient.Error{
		StatusCode: http.StatusTooManyRequests,
		Body:       []byte(`{"error":{"message":"raw provider secret","type":"rate_limit_error","code":"provider_rate_limit"},"request_id":"req_123"}`),
	}

	err := applyUpstreamErrorPolicy(ctx, pipeline.WrapUpstreamError(rawErr), systemService)

	respErr := &llm.ResponseError{}
	require.True(t, errors.As(err, &respErr))
	assert.Equal(t, http.StatusTooManyRequests, respErr.StatusCode)
	assert.Equal(t, "模型服务暂时不可用，请稍后再试", respErr.Detail.Message)
	assert.Equal(t, "rate_limit_error", respErr.Detail.Type)
	assert.Equal(t, "provider_rate_limit", respErr.Detail.Code)
	assert.Equal(t, "req_123", respErr.Detail.RequestID)
	assert.NotContains(t, respErr.Error(), "raw provider secret")
}

func TestApplyUpstreamErrorPolicy_PassthroughByDefault(t *testing.T) {
	ctx, systemService := setupUpstreamErrorPolicyTest(t, biz.UpstreamErrorPolicy{
		Mode: biz.UpstreamErrorModePassthrough,
	})

	rawErr := errors.New("raw upstream error")

	err := applyUpstreamErrorPolicy(ctx, rawErr, systemService)

	assert.Equal(t, rawErr, err)
}

func TestApplyUpstreamErrorPolicy_DoesNotRewriteLocalResponseError(t *testing.T) {
	ctx, systemService := setupUpstreamErrorPolicyTest(t, biz.UpstreamErrorPolicy{
		Mode:          biz.UpstreamErrorModeCustom,
		CustomMessage: "模型服务暂时不可用，请稍后再试",
	})

	localErr := &llm.ResponseError{
		StatusCode: http.StatusForbidden,
		Detail: llm.ErrorDetail{
			Code:    "quota_exceeded",
			Message: "API key quota exceeded",
			Type:    "quota_exceeded_error",
		},
	}

	err := applyUpstreamErrorPolicy(ctx, localErr, systemService)

	assert.Equal(t, localErr, err)
}
