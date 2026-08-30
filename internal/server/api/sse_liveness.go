package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/orchestrator"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

type sseCloseCause string

const (
	sseCloseStreamCompleted        sseCloseCause = "stream_completed"
	sseCloseTerminalEvent          sseCloseCause = "terminal_event"
	sseCloseSemanticCompletion     sseCloseCause = "semantic_completion"
	sseCloseUpstreamEOF            sseCloseCause = "upstream_eof"
	sseCloseUpstreamError          sseCloseCause = "upstream_error"
	sseCloseDownstreamWriteFailed  sseCloseCause = "downstream_write_failed"
	sseCloseClientDisconnect       sseCloseCause = "client_disconnect"
	sseCloseRequestContextCanceled sseCloseCause = "request_context_canceled"
	sseCloseRequestDeadline        sseCloseCause = "request_deadline_exceeded"
)

type downstreamCancellationCause struct {
	reason sseCloseCause
}

func (e downstreamCancellationCause) Error() string { return string(e.reason) }
func (e downstreamCancellationCause) Unwrap() error { return context.Canceled }

type sseProcessOutcome struct {
	result orchestrator.ChatCompletionResult
	err    error
}

type sseProcessFunc func(
	context.Context,
	*httpclient.Request,
	orchestrator.StreamLivenessObserver,
) (orchestrator.ChatCompletionResult, error)

type sseLivenessLedger struct {
	ctx     context.Context
	started time.Time

	mu                       sync.Mutex
	upstreamHeadersAt        time.Time
	firstRawSSEAt            time.Time
	firstTransformableAt     time.Time
	lastHeartbeatWriteAt     time.Time
	lastBusinessWriteAt      time.Time
	heartbeatWrites          int
	businessWrites           int
	writeFailures            int
	upstreamErrors           int
	upstreamResponseAttempts int
	closeReason              sseCloseCause
	closeOnce                sync.Once
}

func newSSELivenessLedger(ctx context.Context) *sseLivenessLedger {
	return &sseLivenessLedger{ctx: ctx, started: time.Now()}
}

func (l *sseLivenessLedger) recordUpstreamHeaders(attempt orchestrator.StreamLivenessAttempt) {
	now := time.Now()
	l.mu.Lock()
	if l.upstreamHeadersAt.IsZero() {
		l.upstreamHeadersAt = now
	}
	l.upstreamResponseAttempts++
	l.mu.Unlock()

	log.Debug(l.ctx, "SSE liveness milestone",
		log.String("milestone", "upstream_response_headers"),
		log.Int("channel_id", attempt.ChannelID),
		log.String("channel_name", attempt.ChannelName))
}

func (l *sseLivenessLedger) recordFirstRawSSE() {
	l.mu.Lock()
	if !l.firstRawSSEAt.IsZero() {
		l.mu.Unlock()
		return
	}
	l.firstRawSSEAt = time.Now()
	l.mu.Unlock()

	log.Debug(l.ctx, "SSE liveness milestone", log.String("milestone", "first_raw_sse"))
}

func (l *sseLivenessLedger) recordFirstTransformableEvent() {
	l.mu.Lock()
	if !l.firstTransformableAt.IsZero() {
		l.mu.Unlock()
		return
	}
	l.firstTransformableAt = time.Now()
	l.mu.Unlock()

	log.Debug(l.ctx, "SSE liveness milestone", log.String("milestone", "first_transformable_event"))
}

func (l *sseLivenessLedger) recordWrite(heartbeat bool) {
	l.mu.Lock()
	if heartbeat {
		l.lastHeartbeatWriteAt = time.Now()
		l.heartbeatWrites++
	} else {
		l.lastBusinessWriteAt = time.Now()
		l.businessWrites++
	}
	l.mu.Unlock()
}

func (l *sseLivenessLedger) recordWriteFailure() {
	l.mu.Lock()
	l.writeFailures++
	l.mu.Unlock()
}

func (l *sseLivenessLedger) recordUpstreamError(err error) {
	l.mu.Lock()
	l.upstreamErrors++
	l.mu.Unlock()
	log.Debug(l.ctx, "SSE liveness milestone",
		log.String("milestone", "upstream_error"),
		log.String("error_type", classifySSECause(err)))
}

func (l *sseLivenessLedger) close(reason sseCloseCause, cause error) {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closeReason = reason
		headersAt := l.upstreamHeadersAt
		rawAt := l.firstRawSSEAt
		transformableAt := l.firstTransformableAt
		heartbeatAt := l.lastHeartbeatWriteAt
		businessAt := l.lastBusinessWriteAt
		heartbeats := l.heartbeatWrites
		business := l.businessWrites
		failures := l.writeFailures
		upstreamErrors := l.upstreamErrors
		attempts := l.upstreamResponseAttempts
		l.mu.Unlock()

		log.Info(l.ctx, "SSE liveness closed",
			log.String("close_reason", string(reason)),
			log.String("context_cause", classifySSECause(cause)),
			log.Int("upstream_response_attempts", attempts),
			log.Int("heartbeat_writes", heartbeats),
			log.Int("business_writes", business),
			log.Int("write_failures", failures),
			log.Int("upstream_errors", upstreamErrors),
			log.Int64("upstream_headers_after_ms", elapsedMillis(l.started, headersAt)),
			log.Int64("first_raw_sse_after_ms", elapsedMillis(l.started, rawAt)),
			log.Int64("first_transformable_after_ms", elapsedMillis(l.started, transformableAt)),
			log.Int64("last_heartbeat_after_ms", elapsedMillis(l.started, heartbeatAt)),
			log.Int64("last_business_event_after_ms", elapsedMillis(l.started, businessAt)))
	})
}

func (l *sseLivenessLedger) reason() sseCloseCause {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closeReason
}

func elapsedMillis(start, event time.Time) int64 {
	if event.IsZero() {
		return -1
	}
	return event.Sub(start).Milliseconds()
}

func classifySSECause(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return fmt.Sprintf("%T", err)
	}
}

func closeReasonFromContext(ctx context.Context) (sseCloseCause, bool) {
	cause := context.Cause(ctx)
	if cause == nil {
		return "", false
	}

	var downstream downstreamCancellationCause
	if errors.As(cause, &downstream) {
		return downstream.reason, true
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return sseCloseRequestDeadline, true
	}
	if cause == context.Canceled {
		return sseCloseClientDisconnect, true
	}
	return sseCloseRequestContextCanceled, true
}

type sseLivenessSession struct {
	ctx             context.Context
	cancel          context.CancelCauseFunc
	global          SSEKeepAliveConfig
	heartbeat       sseHeartbeatFormat
	streaming       bool
	ledger          *sseLivenessLedger
	readySignal     chan struct{}
	semanticSuccess chan struct{}
	abandoned       chan struct{}
	abandonOnce     sync.Once
	semanticOnce    sync.Once
	confirmOnce     sync.Once

	mu                        sync.Mutex
	effective                 SSEKeepAliveConfig
	interrupt                 *sseStreamInterrupt
	confirmSemanticCompletion func() bool
	confirmAccepted           bool
	semanticResponse          *llm.Response
	semanticChoices           map[int]struct{}
	expectedChoices           int
	terminalGrace             time.Duration
	committed                 atomic.Bool
}

const defaultSSETerminalGrace = 250 * time.Millisecond

type sseStreamInterrupt struct {
	once sync.Once
	fn   func() error
	err  error
}

func (i *sseStreamInterrupt) run() error {
	if i == nil || i.fn == nil {
		return nil
	}
	i.once.Do(func() { i.err = i.fn() })
	return i.err
}

func newSSELivenessSession(
	ctx context.Context,
	cancel context.CancelCauseFunc,
	global SSEKeepAliveConfig,
	heartbeat sseHeartbeatFormat,
	streaming bool,
) *sseLivenessSession {
	return &sseLivenessSession{
		ctx:             ctx,
		cancel:          cancel,
		global:          global,
		heartbeat:       heartbeat,
		streaming:       streaming,
		ledger:          newSSELivenessLedger(ctx),
		readySignal:     make(chan struct{}, 1),
		semanticSuccess: make(chan struct{}),
		abandoned:       make(chan struct{}),
		effective:       global,
		semanticChoices: make(map[int]struct{}),
		expectedChoices: 1,
		terminalGrace:   defaultSSETerminalGrace,
	}
}

func (s *sseLivenessSession) IsDownstreamCommitted() bool {
	return s.committed.Load()
}

func (s *sseLivenessSession) OnUpstreamResponseHeaders(attempt orchestrator.StreamLivenessAttempt) {
	s.ledger.recordUpstreamHeaders(attempt)
	s.mu.Lock()
	s.effective = resolveSSEKeepAlive(s.global, attempt.KeepAlive)
	s.interrupt = &sseStreamInterrupt{fn: attempt.Interrupt}
	s.confirmSemanticCompletion = attempt.ConfirmSemanticCompletion
	s.mu.Unlock()
	select {
	case s.readySignal <- struct{}{}:
	default:
	}
}

func (s *sseLivenessSession) OnRawSSE(event *httpclient.StreamEvent) {
	s.ledger.recordFirstRawSSE()
	if orchestrator.ClassifyStreamSemanticTerminal(event) != orchestrator.StreamSemanticSucceeded {
		return
	}
	choices := gjson.GetBytes(event.Data, "choices")
	if !choices.IsArray() || len(choices.Array()) == 0 {
		s.signalSemanticSuccess(nil)
		return
	}
	indices := make([]int, 0, len(choices.Array()))
	for _, choice := range choices.Array() {
		indices = append(indices, int(choice.Get("index").Int()))
	}
	s.recordSemanticChoices(indices, nil)
}

func (s *sseLivenessSession) OnTransformableEvent(response *llm.Response) {
	s.ledger.recordFirstTransformableEvent()
	if orchestrator.ClassifyLLMStreamSemanticTerminal(response) == orchestrator.StreamSemanticSucceeded {
		indices := make([]int, 0, len(response.Choices))
		for _, choice := range response.Choices {
			indices = append(indices, choice.Index)
		}
		s.recordSemanticChoices(indices, response)
	}
}

func (s *sseLivenessSession) OnUpstreamError(err error) {
	s.ledger.recordUpstreamError(err)
}

func (s *sseLivenessSession) effectiveConfig() SSEKeepAliveConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.effective
}

// writeHeartbeatIfEnabled linearizes a timer tick with channel-attempt config
// changes. Once a disabled override is installed under mu, no timer branch can
// write using the superseded enabled config.
func (s *sseLivenessSession) writeHeartbeatIfEnabled(writer http.ResponseWriter) (bool, time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.effective.Enabled || s.effective.Interval <= 0 {
		return false, 0, nil
	}
	if err := writeAndFlushSSEHeartbeat(writer, s.heartbeat); err != nil {
		return false, 0, err
	}
	return true, s.effective.Interval, nil
}

func (s *sseLivenessSession) interruptUpstream() error {
	s.mu.Lock()
	interrupt := s.interrupt
	s.mu.Unlock()
	return interrupt.run()
}

func (s *sseLivenessSession) signalSemanticSuccess(response *llm.Response) {
	s.mu.Lock()
	if response != nil {
		s.semanticResponse = response
	}
	s.mu.Unlock()
	s.semanticOnce.Do(func() { close(s.semanticSuccess) })
}

func (s *sseLivenessSession) recordSemanticChoices(indices []int, response *llm.Response) {
	s.mu.Lock()
	for _, index := range indices {
		s.semanticChoices[index] = struct{}{}
	}
	if response != nil {
		s.semanticResponse = response
	}
	ready := len(s.semanticChoices) >= s.expectedChoices
	s.mu.Unlock()
	if ready {
		s.semanticOnce.Do(func() { close(s.semanticSuccess) })
	}
}

func (s *sseLivenessSession) semanticSuccessSignal() <-chan struct{} {
	return s.semanticSuccess
}

func (s *sseLivenessSession) semanticCompletionResponse() *llm.Response {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.semanticResponse
}

func (s *sseLivenessSession) confirmSemanticSuccess() bool {
	s.confirmOnce.Do(func() {
		s.mu.Lock()
		confirm := s.confirmSemanticCompletion
		s.mu.Unlock()
		if confirm == nil {
			s.confirmAccepted = true
		} else {
			s.confirmAccepted = confirm()
		}
	})
	return s.confirmAccepted
}

func resolveSSEKeepAlive(global SSEKeepAliveConfig, override *objects.ChannelSSEKeepAlive) SSEKeepAliveConfig {
	resolved := global
	if override == nil {
		return resolved
	}
	if override.Enabled != nil {
		resolved.Enabled = *override.Enabled
	}
	if override.IntervalSeconds != nil {
		resolved.Interval = time.Duration(*override.IntervalSeconds) * time.Second
	}
	return resolved
}

func (s *sseLivenessSession) abandon() {
	s.abandonOnce.Do(func() { close(s.abandoned) })
}

func (s *sseLivenessSession) failDownstream(err error) {
	s.ledger.recordWriteFailure()
	s.cancel(downstreamCancellationCause{reason: sseCloseDownstreamWriteFailed})
	_ = s.interruptUpstream()
	reason, _ := closeReasonFromContext(s.ctx)
	s.ledger.close(reason, context.Cause(s.ctx))
	log.Warn(s.ctx, "Downstream SSE write failed", log.String("error_type", classifySSECause(err)))
}

func (s *sseLivenessSession) awaitProcess(
	c *gin.Context,
	process sseProcessFunc,
	request *httpclient.Request,
) (sseProcessOutcome, bool, bool) {
	var timer *time.Timer
	var timerC <-chan time.Time
	committed := false

	if config := s.effectiveConfig(); s.streaming && config.Enabled && config.Interval > 0 {
		setSSEHeaders(c)
		committed = true
		s.committed.Store(true)
		if err := flushSSE(c.Writer); err != nil {
			s.failDownstream(err)
			s.abandon()
			return sseProcessOutcome{}, committed, true
		}
		timer = time.NewTimer(config.Interval)
		timerC = timer.C
	}

	// Unbuffered handoff keeps ownership explicit: if the handler abandons the
	// request after a downstream failure, the worker must close any late stream
	// instead of depositing it into an unread result buffer.
	outcomes := make(chan sseProcessOutcome)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error(s.ctx, "Panic while processing SSE request", log.Any("panic", recovered))
				select {
				case outcomes <- sseProcessOutcome{err: errors.New("stream processing stopped unexpectedly")}:
				case <-s.abandoned:
				}
			}
		}()

		result, err := process(s.ctx, request, s)
		outcome := sseProcessOutcome{result: result, err: err}
		select {
		case <-s.abandoned:
			if result.ChatCompletionStream != nil {
				_ = result.ChatCompletionStream.Close()
			}
			return
		default:
		}
		select {
		case outcomes <- outcome:
			select {
			case <-s.abandoned:
				if result.ChatCompletionStream != nil {
					_ = result.ChatCompletionStream.Close()
				}
			default:
			}
		case <-s.abandoned:
			if result.ChatCompletionStream != nil {
				_ = result.ChatCompletionStream.Close()
			}
		}
	}()

	defer func() {
		if timer != nil {
			stopTimer(timer)
		}
	}()

	for {
		select {
		case outcome := <-outcomes:
			if reason, canceled := closeReasonFromContext(s.ctx); canceled {
				if outcome.result.ChatCompletionStream != nil {
					_ = outcome.result.ChatCompletionStream.Close()
				}
				s.ledger.close(reason, context.Cause(s.ctx))
				return sseProcessOutcome{}, committed, true
			}
			return outcome, committed, false

		case <-s.readySignal:
			config := s.effectiveConfig()
			if !config.Enabled || config.Interval <= 0 {
				if timer != nil {
					stopTimer(timer)
					timerC = nil
				}
				continue
			}

			if !committed {
				setSSEHeaders(c)
				committed = true
				s.committed.Store(true)
				if err := flushSSE(c.Writer); err != nil {
					s.failDownstream(err)
					s.abandon()
					return sseProcessOutcome{}, true, true
				}
			}

			if timer == nil {
				timer = time.NewTimer(config.Interval)
			} else {
				resetTimer(timer, config.Interval)
			}
			timerC = timer.C

		case <-timerC:
			wrote, interval, err := s.writeHeartbeatIfEnabled(c.Writer)
			if err != nil {
				s.failDownstream(err)
				s.abandon()
				return sseProcessOutcome{}, committed, true
			}
			if !wrote {
				stopTimer(timer)
				timerC = nil
				continue
			}
			s.ledger.recordWrite(true)
			timer.Reset(interval)

		case <-s.ctx.Done():
			reason, _ := closeReasonFromContext(s.ctx)
			s.ledger.close(reason, context.Cause(s.ctx))
			s.abandon()
			return sseProcessOutcome{}, committed, true
		}
	}
}

func (s *sseLivenessSession) writeError(c *gin.Context, value any) error {
	if err := writeAndFlushSSEEvent(c.Writer, "error", value); err != nil {
		s.failDownstream(err)
		return err
	}
	s.ledger.recordWrite(false)
	return nil
}

func (s *sseLivenessSession) finish(reason sseCloseCause) {
	s.ledger.close(reason, context.Cause(s.ctx))
}

func requestWithSSELivenessContext(c *gin.Context) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(c.Request.Context())
	c.Request = c.Request.WithContext(ctx)
	return ctx, cancel
}
