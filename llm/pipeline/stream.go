package pipeline

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/logpolicy"
	"github.com/looplj/axonhub/llm/streams"
)

type firstEventTimeoutGuard struct {
	timer    *time.Timer
	cancel   context.CancelFunc
	stopOnce sync.Once
	state    atomic.Uint32
}

const (
	firstEventPending uint32 = iota
	firstEventCompleted
	firstEventTimedOut
)

const (
	maxPreReadEvents             = 3
	maxPreCommitRetryProbeEvents = 16
)

func newFirstEventTimeoutGuard(ctx context.Context, timeout time.Duration) (context.Context, *firstEventTimeoutGuard) {
	if timeout <= 0 {
		return ctx, nil
	}

	streamCtx, cancel := context.WithCancel(ctx)
	guard := &firstEventTimeoutGuard{
		cancel: cancel,
	}
	guard.timer = time.AfterFunc(timeout, func() {
		if guard.state.CompareAndSwap(firstEventPending, firstEventTimedOut) {
			cancel()
		}
	})

	return streamCtx, guard
}

func (g *firstEventTimeoutGuard) timedOut() bool {
	return g != nil && g.state.Load() == firstEventTimedOut
}

func (g *firstEventTimeoutGuard) stop() {
	if g == nil {
		return
	}

	g.stopOnce.Do(func() {
		g.timer.Stop()
	})
}

func (g *firstEventTimeoutGuard) cancelStream() {
	if g == nil {
		return
	}

	g.cancel()
}

func (g *firstEventTimeoutGuard) completeFirstEventPhase() {
	if g == nil {
		return
	}

	g.state.CompareAndSwap(firstEventPending, firstEventCompleted)
	g.stop()
}

func (g *firstEventTimeoutGuard) acceptFirstEvent() bool {
	if g == nil {
		return true
	}

	// If the timeout already canceled the stream, keep that state so callers
	// can retry instead of returning a partially usable stream.
	if g.state.CompareAndSwap(firstEventPending, firstEventCompleted) {
		g.stop()

		return true
	}

	return g.state.Load() == firstEventCompleted
}

func (g *firstEventTimeoutGuard) finishBeforeFirstEvent(err error) error {
	if g == nil {
		return err
	}

	g.completeFirstEventPhase()
	g.cancelStream()
	if g.timedOut() {
		return ErrStreamFirstEventTimeout
	}

	return err
}

type cancelOnCloseStream struct {
	stream streams.Stream[*httpclient.StreamEvent]
	cancel context.CancelFunc
	once   sync.Once
}

func (s *cancelOnCloseStream) Next() bool {
	return s.stream.Next()
}

func (s *cancelOnCloseStream) Current() *httpclient.StreamEvent {
	return s.stream.Current()
}

func (s *cancelOnCloseStream) Err() error {
	return s.stream.Err()
}

func (s *cancelOnCloseStream) Close() error {
	err := s.stream.Close()
	s.once.Do(s.cancel)

	return err
}

// hasFinishReason checks if an llm.Response event contains a finish reason.
func hasFinishReason(resp *llm.Response) bool {
	if resp == nil {
		return false
	}

	for _, choice := range resp.Choices {
		if choice.FinishReason != nil {
			return true
		}
	}

	return false
}

func isTerminalLlmStreamEvent(resp *llm.Response) bool {
	return resp == llm.DoneResponse || (resp != nil && resp.Object == "[DONE]") || hasFinishReason(resp)
}

func (p *pipeline) hasStreamRetryBudget() bool {
	return p.maxSameChannelRetries > 0 || p.maxChannelRetries > 0
}

func shouldWrapPreReadStreamError(err error) bool {
	return !errors.Is(err, ErrStreamFirstEventTimeout) &&
		!errors.Is(err, ErrEmptyResponse) &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded)
}

// preReadLlmStream reads the initial LLM stream events so the pipeline can
// enforce first-event timeout, detect empty responses, and keep retryable stream
// failures inside the pipeline until the first meaningful response content.
// If the stream contains content, it returns a new stream with the pre-read events prepended.
// If the stream is empty (finish reason reached without content), it returns ErrEmptyResponse.
func (p *pipeline) preReadLlmStream(
	ctx context.Context,
	llmStream streams.Stream[*llm.Response],
	firstEventGuard *firstEventTimeoutGuard,
) (streams.Stream[*llm.Response], error) {
	preReadUntilContent := p.hasStreamRetryBudget()
	probeLimit := maxPreReadEvents
	if preReadUntilContent {
		probeLimit = maxPreCommitRetryProbeEvents
	}

	var buffered []*llm.Response
	matcher := newEmptyResponseTextMatcher(p.emptyResponseTextPatterns)

	for i := 0; ; i++ {
		hasNext, err := nextLlmStreamEvent(ctx, llmStream, i == 0, firstEventGuard)
		if err != nil {
			llmStream.Close()

			return nil, err
		}
		if !hasNext {
			break
		}

		event := llmStream.Current()
		buffered = append(buffered, event)

		if len(matcher.patterns) == 0 {
			if hasResponseContent(event) {
				// Has content, not empty — prepend buffered events back
				return streams.PrependStream(llmStream, buffered...), nil
			}
		} else {
			bufferedText, textOnly := bufferedResponsesTextContent(buffered)
			if !textOnly {
				return streams.PrependStream(llmStream, buffered...), nil
			}

			if normalized := normalizeEmptyResponseText(bufferedText); normalized != "" &&
				!matcher.matchesPrefix(bufferedText) {
				return streams.PrependStream(llmStream, buffered...), nil
			}
		}

		if !preReadUntilContent && !p.emptyResponseDetection {
			return streams.PrependStream(llmStream, buffered...), nil
		}

		// Recognize both the shared sentinel pointer (llm.DoneResponse) and a
		// freshly-constructed "[DONE]" terminator: outbound transformers that emit
		// terminal events as new *llm.Response (e.g. OpenAI TTS binary streams) must
		// still trigger empty-response handling when no audio chunks were produced.
		if isTerminalLlmStreamEvent(event) {
			if !p.emptyResponseDetection {
				return streams.PrependStream(llmStream, buffered...), nil
			}

			// Reached end without content — empty response
			slog.WarnContext(ctx, "empty response detected",
				slog.Int("events_read", len(buffered)),
			)

			llmStream.Close()

			return nil, ErrEmptyResponse
		}

		if len(buffered) >= probeLimit {
			break
		}
	}

	if err := llmStream.Err(); err != nil {
		llmStream.Close()

		return nil, err
	}

	// Didn't find content or finish in the bounded empty-response probe - treat
	// as non-empty and let the handler consume the stream.
	if len(buffered) > 0 {
		return streams.PrependStream(llmStream, buffered...), nil
	}

	return llmStream, nil
}

func nextLlmStreamEvent(
	ctx context.Context,
	llmStream streams.Stream[*llm.Response],
	firstEvent bool,
	firstEventGuard *firstEventTimeoutGuard,
) (bool, error) {
	if !firstEvent || firstEventGuard == nil {
		return llmStream.Next(), nil
	}

	hasNext := llmStream.Next()
	if hasNext {
		if !firstEventGuard.acceptFirstEvent() {
			llmStream.Close()

			return false, ErrStreamFirstEventTimeout
		}

		return true, nil
	}

	firstEventGuard.completeFirstEventPhase()
	if firstEventGuard.timedOut() {
		llmStream.Close()

		return false, ErrStreamFirstEventTimeout
	}

	if ctx.Err() != nil {
		firstEventGuard.cancelStream()
		llmStream.Close()

		return false, ctx.Err()
	}

	return hasNext, nil
}

// DirectPassThroughOutbound is optionally implemented by outbound transformers
// that support bypassing LLM AST stream transformation for same-protocol pass-through.
type DirectPassThroughOutbound interface {
	ShouldDirectPassThroughStream(ctx context.Context, request *httpclient.Request) bool
	WrapDirectPassThroughOutboundStream(ctx context.Context, request *httpclient.Request, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*httpclient.StreamEvent], error)
}

// DirectPassThroughInbound is optionally implemented by inbound transformers
// paired with DirectPassThroughOutbound.
type DirectPassThroughInbound interface {
	WrapDirectPassThroughInboundStream(ctx context.Context, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*httpclient.StreamEvent], error)
}

// Process executes the streaming LLM pipeline
// Steps: outbound transform -> HTTP stream -> outbound stream transform -> inbound stream transform.
func (p *pipeline) stream(
	ctx context.Context,
	executor Executor,
	request *httpclient.Request,
	firstEventTimeout time.Duration,
) (streams.Stream[*httpclient.StreamEvent], error) {
	streamCtx, firstEventGuard := newFirstEventTimeoutGuard(ctx, firstEventTimeout)

	outboundStream, err := executor.DoStream(streamCtx, request)
	if firstEventGuard.timedOut() {
		if outboundStream != nil {
			outboundStream.Close()
		}

		timeoutErr := firstEventGuard.finishBeforeFirstEvent(ErrStreamFirstEventTimeout)
		p.applyRawErrorResponseMiddlewares(ctx, timeoutErr)

		return nil, timeoutErr
	}
	if err != nil {
		err = firstEventGuard.finishBeforeFirstEvent(err)

		// Apply error response middlewares
		p.applyRawErrorResponseMiddlewares(ctx, err)

		if errors.Is(err, ErrStreamFirstEventTimeout) {
			return nil, err
		}

		if httpErr, ok := errors.AsType[*httpclient.Error](err); ok {
			return nil, p.transformOutboundError(ctx, request, httpErr)
		}

		return nil, WrapUpstreamError(err)
	}

	if directOut, ok := p.Outbound.(DirectPassThroughOutbound); ok {
		if directIn, ok := p.Inbound.(DirectPassThroughInbound); ok && !p.emptyResponseDetection && directOut.ShouldDirectPassThroughStream(ctx, request) {
			return p.directPassThroughStream(ctx, request, outboundStream, firstEventGuard, firstEventTimeout, directOut, directIn)
		}
	}

	// Apply raw stream middlewares
	rawStream := outboundStream

	outboundStream, err = p.applyRawStreamMiddlewares(ctx, outboundStream)
	if err != nil {
		rawStream.Close()
		err = firstEventGuard.finishBeforeFirstEvent(err)
		p.applyRawErrorResponseMiddlewares(ctx, err)

		if errors.Is(err, ErrStreamFirstEventTimeout) {
			return nil, err
		}

		return nil, fmt.Errorf("failed to apply raw stream middlewares: %w", err)
	}

	if slog.Default().Enabled(ctx, slog.LevelDebug) && !logpolicy.PayloadsDisabled(ctx) {
		outboundStream = streams.Map(outboundStream,
			func(event *httpclient.StreamEvent) *httpclient.StreamEvent {
				slog.DebugContext(ctx, "Outbound stream event", slog.Any("event", event))
				return event
			},
		)
	}

	llmStream, err := p.Outbound.TransformStream(ctx, request, outboundStream)
	if err != nil {
		outboundStream.Close()
		err = firstEventGuard.finishBeforeFirstEvent(err)
		p.applyRawErrorResponseMiddlewares(ctx, err)

		slog.ErrorContext(ctx, "Failed to transform streaming request", slog.Any("error", err))

		if errors.Is(err, ErrStreamFirstEventTimeout) {
			return nil, err
		}

		return nil, WrapUpstreamError(err)
	}

	rawLlmStream := llmStream

	// Apply LLM stream middlewares
	llmStream, err = p.applyLlmStreamMiddlewares(ctx, llmStream)
	if err != nil {
		rawLlmStream.Close()
		err = firstEventGuard.finishBeforeFirstEvent(err)
		p.applyRawErrorResponseMiddlewares(ctx, err)

		if errors.Is(err, ErrStreamFirstEventTimeout) {
			return nil, err
		}

		return nil, fmt.Errorf("failed to apply llm stream middlewares: %w", err)
	}

	if slog.Default().Enabled(ctx, slog.LevelDebug) && !logpolicy.PayloadsDisabled(ctx) {
		llmStream = streams.Map(llmStream, func(event *llm.Response) *llm.Response {
			slog.DebugContext(ctx, "LLM stream event", slog.Any("event", event))
			return event
		})
	}

	// Check stream start before the handler commits the response. This enforces
	// first-event timeout, empty-response detection, and pre-content retry.
	if p.emptyResponseDetection || firstEventTimeout > 0 || p.hasStreamRetryBudget() {
		rawLlmStream := llmStream

		llmStream, err = p.preReadLlmStream(ctx, llmStream, firstEventGuard)
		if err != nil {
			rawLlmStream.Close()
			err = firstEventGuard.finishBeforeFirstEvent(err)
			p.applyRawErrorResponseMiddlewares(ctx, err)

			if !shouldWrapPreReadStreamError(err) {
				return nil, err
			}

			return nil, WrapUpstreamError(err)
		}
	} else if firstEventGuard != nil {
		firstEventGuard.stop()
	}

	terminalErrorReporter := &terminalErrorReporter{
		ctx:     ctx,
		onError: p.applyRawErrorResponseMiddlewares,
	}
	llmStream = &terminalErrorReportingStream[*llm.Response]{
		stream:   llmStream,
		reporter: terminalErrorReporter,
	}

	inboundStream, err := p.Inbound.TransformStream(ctx, llmStream)
	if err != nil {
		llmStream.Close()
		firstEventGuard.cancelStream()
		terminalErrorReporter.report(err)

		slog.ErrorContext(ctx, "Failed to transform streaming request", slog.Any("error", err))

		return nil, err
	}

	rawInboundStream := inboundStream

	inboundStream, err = p.applyInboundRawStreamMiddlewares(ctx, inboundStream)
	if err != nil {
		rawInboundStream.Close()
		firstEventGuard.cancelStream()
		terminalErrorReporter.report(err)

		return nil, fmt.Errorf("failed to apply inbound raw stream middlewares: %w", err)
	}

	if slog.Default().Enabled(ctx, slog.LevelDebug) && !logpolicy.PayloadsDisabled(ctx) {
		inboundStream = streams.Map(
			inboundStream,
			func(event *httpclient.StreamEvent) *httpclient.StreamEvent {
				slog.DebugContext(ctx, "Inbound stream event", slog.Any("event", event))
				return event
			},
		)
	}

	inboundStream = &terminalErrorReportingStream[*httpclient.StreamEvent]{
		stream:   inboundStream,
		reporter: terminalErrorReporter,
	}

	if firstEventGuard != nil {
		inboundStream = &cancelOnCloseStream{
			stream: inboundStream,
			cancel: firstEventGuard.cancelStream,
		}
	}

	return inboundStream, nil
}

func (p *pipeline) directPassThroughStream(
	ctx context.Context,
	request *httpclient.Request,
	outboundStream streams.Stream[*httpclient.StreamEvent],
	firstEventGuard *firstEventTimeoutGuard,
	firstEventTimeout time.Duration,
	directOut DirectPassThroughOutbound,
	directIn DirectPassThroughInbound,
) (streams.Stream[*httpclient.StreamEvent], error) {
	rawStream := outboundStream
	var err error

	outboundStream, err = p.applyRawStreamMiddlewares(ctx, outboundStream)
	if err != nil {
		rawStream.Close()
		err = firstEventGuard.finishBeforeFirstEvent(err)
		p.applyRawErrorResponseMiddlewares(ctx, err)
		if errors.Is(err, ErrStreamFirstEventTimeout) {
			return nil, err
		}
		return nil, fmt.Errorf("failed to apply raw stream middlewares: %w", err)
	}

	outboundStream, err = directOut.WrapDirectPassThroughOutboundStream(ctx, request, outboundStream)
	if err != nil {
		rawStream.Close()
		err = firstEventGuard.finishBeforeFirstEvent(err)
		p.applyRawErrorResponseMiddlewares(ctx, err)
		if errors.Is(err, ErrStreamFirstEventTimeout) {
			return nil, err
		}
		return nil, WrapUpstreamError(err)
	}

	wrappedOutbound := outboundStream
	outboundStream, err = p.applyDirectPassThroughStreamMiddlewares(ctx, outboundStream)
	if err != nil {
		wrappedOutbound.Close()
		err = firstEventGuard.finishBeforeFirstEvent(err)
		p.applyRawErrorResponseMiddlewares(ctx, err)
		if errors.Is(err, ErrStreamFirstEventTimeout) {
			return nil, err
		}
		return nil, fmt.Errorf("failed to apply direct pass-through stream middlewares: %w", err)
	}

	preReadSrc := outboundStream
	outboundStream, err = preReadRawStream(
		ctx,
		llm.APIFormat(request.APIFormat),
		outboundStream,
		firstEventGuard,
		p.hasStreamRetryBudget(),
	)
	if err != nil {
		preReadSrc.Close()
		err = firstEventGuard.finishBeforeFirstEvent(err)
		p.applyRawErrorResponseMiddlewares(ctx, err)
		if !shouldWrapPreReadStreamError(err) {
			return nil, err
		}
		return nil, WrapUpstreamError(err)
	}

	terminalErrorReporter := &terminalErrorReporter{
		ctx:     ctx,
		onError: p.applyRawErrorResponseMiddlewares,
	}
	outboundStream = &postCommitRawErrorStream{
		stream:    outboundStream,
		apiFormat: llm.APIFormat(request.APIFormat),
		reporter:  terminalErrorReporter,
	}
	outboundStream = &terminalErrorReportingStream[*httpclient.StreamEvent]{
		stream:   outboundStream,
		reporter: terminalErrorReporter,
	}

	inboundStream, err := directIn.WrapDirectPassThroughInboundStream(ctx, outboundStream)
	if err != nil {
		outboundStream.Close()
		firstEventGuard.cancelStream()
		terminalErrorReporter.report(err)
		return nil, err
	}

	rawInboundStream := inboundStream
	inboundStream, err = p.applyInboundRawStreamMiddlewares(ctx, inboundStream)
	if err != nil {
		rawInboundStream.Close()
		firstEventGuard.cancelStream()
		terminalErrorReporter.report(err)
		return nil, fmt.Errorf("failed to apply inbound raw stream middlewares: %w", err)
	}

	inboundStream = &terminalErrorReportingStream[*httpclient.StreamEvent]{
		stream:   inboundStream,
		reporter: terminalErrorReporter,
	}

	if firstEventGuard != nil {
		inboundStream = &cancelOnCloseStream{
			stream: inboundStream,
			cancel: firstEventGuard.cancelStream,
		}
	}

	return inboundStream, nil
}

type rawStreamEventClass uint8

const (
	rawEventPreamble rawStreamEventClass = iota
	rawEventContent
	rawEventTerminal
	rawEventProviderError
)

// preReadRawStream mirrors preReadLlmStream for direct pass-through streams:
// it buffers up to maxPreCommitRetryProbeEvents (16) preamble frames until
// meaningful content or an official terminal arrives, classifies pre-content
// provider error frames into typed *llm.ResponseError values, and turns
// empty/pre-content-truncated streams into ErrEmptyResponse so OnOutboundRawError
// and bounded multi-channel failover fire before committing downstream.
// Caller owns closing rawStream on error.
func preReadRawStream(
	ctx context.Context,
	apiFormat llm.APIFormat,
	rawStream streams.Stream[*httpclient.StreamEvent],
	firstEventGuard *firstEventTimeoutGuard,
	preReadUntilContent bool,
) (streams.Stream[*httpclient.StreamEvent], error) {
	probeLimit := 1
	if preReadUntilContent {
		probeLimit = maxPreCommitRetryProbeEvents
	}

	var buffered []*httpclient.StreamEvent

	for i := 0; i < probeLimit; i++ {
		hasNext := rawStream.Next()
		if i == 0 && firstEventGuard != nil {
			if hasNext {
				if !firstEventGuard.acceptFirstEvent() {
					return nil, ErrStreamFirstEventTimeout
				}
			} else {
				firstEventGuard.completeFirstEventPhase()
				if firstEventGuard.timedOut() {
					return nil, ErrStreamFirstEventTimeout
				}
			}
		}

		if !hasNext {
			if err := rawStream.Err(); err != nil {
				return nil, err
			}
			if ctx.Err() != nil {
				if firstEventGuard != nil {
					firstEventGuard.cancelStream()
				}
				return nil, ctx.Err()
			}
			// Clean EOF with zero events or only preamble frames and no content/terminal.
			return nil, ErrEmptyResponse
		}

		event := rawStream.Current()
		class := classifyRawStreamEvent(apiFormat, event)
		switch class {
		case rawEventProviderError:
			return nil, parseRawStreamProviderError(apiFormat, event)
		case rawEventContent, rawEventTerminal:
			buffered = append(buffered, event)
			return streams.PrependStream(rawStream, buffered...), nil
		default:
			buffered = append(buffered, event)
			if !preReadUntilContent {
				return streams.PrependStream(rawStream, buffered...), nil
			}
		}
	}

	if err := rawStream.Err(); err != nil {
		return nil, err
	}
	if len(buffered) > 0 {
		return streams.PrependStream(rawStream, buffered...), nil
	}
	return rawStream, nil
}

func classifyRawStreamEvent(apiFormat llm.APIFormat, event *httpclient.StreamEvent) rawStreamEventClass {
	if event == nil || len(event.Data) == 0 {
		return rawEventPreamble
	}
	data := bytes.TrimSpace(event.Data)
	if len(data) == 0 {
		return rawEventPreamble
	}
	if string(data) == "[DONE]" {
		return rawEventTerminal
	}
	if event.Type == "error" {
		return rawEventProviderError
	}
	if data[0] != '{' {
		return rawEventPreamble
	}

	s := unsafe.String(unsafe.SliceData(data), len(data))
	evType := event.Type
	if evType == "" {
		evType = gjson.Get(s, "type").Str
	}

	if evType == "error" {
		return rawEventProviderError
	}
	if bytes.Contains(data, []byte(`"error"`)) {
		if errObj := gjson.Get(s, "error"); errObj.IsObject() || (errObj.Type == gjson.String && errObj.Str != "") {
			return rawEventProviderError
		}
	}

	switch apiFormat {
	case llm.APIFormatAnthropicMessage:
		switch evType {
		case "message_stop":
			return rawEventTerminal
		case "message_delta":
			if sr := gjson.Get(s, "delta.stop_reason"); sr.Type == gjson.String && sr.Str != "" {
				if sr.Str == "error" {
					return rawEventProviderError
				}
				return rawEventTerminal
			}
			return rawEventPreamble
		case "content_block_start":
			cbType := gjson.Get(s, "content_block.type").Str
			switch cbType {
			case "tool_use", "server_tool_use", "thinking", "redacted_thinking":
				return rawEventContent
			case "text":
				if gjson.Get(s, "content_block.text").Str != "" {
					return rawEventContent
				}
			}
			return rawEventPreamble
		case "content_block_delta":
			d := gjson.Get(s, "delta")
			if d.Get("text").Str != "" || d.Get("thinking").Str != "" || d.Get("partial_json").Str != "" || d.Get("signature").Str != "" {
				return rawEventContent
			}
			return rawEventPreamble
		default:
			return rawEventPreamble
		}

	case llm.APIFormatOpenAIResponse:
		switch evType {
		case "response.failed":
			return rawEventProviderError
		case "response.completed":
			if gjson.Get(s, "response.status").Str == "failed" {
				return rawEventProviderError
			}
			return rawEventTerminal
		case "response.incomplete", "response.cancelled", "response.canceled":
			return rawEventTerminal
		case "response.output_text.delta",
			"response.output_text.done",
			"response.function_call_arguments.delta",
			"response.function_call_arguments.done",
			"response.reasoning_summary_text.delta",
			"response.reasoning_text.delta",
			"response.refusal.delta",
			"response.output_item.done":
			return rawEventContent
		default:
			return rawEventPreamble
		}

	case llm.APIFormatOpenAIChatCompletion:
		choice0 := gjson.Get(s, "choices.0")
		if !choice0.Exists() {
			return rawEventPreamble
		}
		delta := choice0.Get("delta")
		if delta.Get("content").Str != "" ||
			delta.Get("reasoning_content").Str != "" ||
			delta.Get("reasoning").Str != "" ||
			delta.Get("refusal").Str != "" ||
			(delta.Get("tool_calls").IsArray() && len(delta.Get("tool_calls").Array()) > 0) {
			return rawEventContent
		}
		if fr := choice0.Get("finish_reason"); fr.Type == gjson.String && fr.Str != "" {
			if fr.Str == "error" {
				return rawEventProviderError
			}
			return rawEventTerminal
		}
		return rawEventPreamble
	}

	return rawEventContent
}

// postCommitRawErrorStream preserves forwarding raw provider SSE error frames
// downstream once while also dispatching a typed *llm.ResponseError through the
// shared terminalErrorReporter when the stream finishes or closes, so health,
// cooldown, circuit-breaker, and RequestExecution failure hooks stay consistent
// with full-pipeline streams.
type postCommitRawErrorStream struct {
	stream      streams.Stream[*httpclient.StreamEvent]
	apiFormat   llm.APIFormat
	reporter    *terminalErrorReporter
	current     *httpclient.StreamEvent
	providerErr *llm.ResponseError
}

func (s *postCommitRawErrorStream) Next() bool {
	if s.stream.Next() {
		ev := s.stream.Current()
		s.current = ev
		if s.providerErr == nil && classifyRawStreamEvent(s.apiFormat, ev) == rawEventProviderError {
			s.providerErr = parseRawStreamProviderError(s.apiFormat, ev)
		}
		return true
	}
	s.current = nil
	if s.stream.Err() == nil && s.providerErr != nil && s.reporter != nil {
		s.reporter.report(s.providerErr)
	}
	return false
}

func (s *postCommitRawErrorStream) Current() *httpclient.StreamEvent { return s.current }

func (s *postCommitRawErrorStream) Err() error { return s.stream.Err() }

func (s *postCommitRawErrorStream) Close() error {
	if s.stream.Err() == nil && s.providerErr != nil && s.reporter != nil {
		s.reporter.report(s.providerErr)
	}
	return s.stream.Close()
}

func parseRawStreamProviderError(apiFormat llm.APIFormat, event *httpclient.StreamEvent) *llm.ResponseError {
	respErr := &llm.ResponseError{
		APIFormat: apiFormat,
		Detail: llm.ErrorDetail{
			Message: "stream error",
			Type:    "stream_error",
		},
	}
	if event == nil || len(event.Data) == 0 {
		return respErr
	}

	root := gjson.ParseBytes(event.Data)
	candidate := root
	if root.Get("event").Str == "error" {
		if d := root.Get("data"); d.Exists() {
			candidate = d
		}
	}
	errObj := candidate.Get("error")
	if !errObj.Exists() {
		errObj = candidate.Get("response.error")
	}
	detail := llm.ErrorDetail{
		Code:      strings.Clone(errObj.Get("code").String()),
		Message:   strings.Clone(errObj.Get("message").String()),
		Type:      strings.Clone(errObj.Get("type").String()),
		Param:     strings.Clone(errObj.Get("param").String()),
		RequestID: strings.Clone(candidate.Get("request_id").String()),
	}
	if detail.Message == "" {
		detail.Message = strings.Clone(candidate.Get("message").String())
	}
	if detail.Code == "" {
		detail.Code = strings.Clone(candidate.Get("code").String())
	}
	if detail.Param == "" {
		detail.Param = strings.Clone(candidate.Get("param").String())
	}
	if detail.Message == "" && errObj.Exists() {
		detail.Message = strings.Clone(errObj.String())
	}
	if detail.Message == "" {
		detail.Message = "stream error"
	}
	if detail.Type == "" {
		detail.Type = "stream_error"
	}
	respErr.Detail = detail
	return respErr
}
