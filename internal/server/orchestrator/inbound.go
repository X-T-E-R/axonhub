package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/dumper"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

// InboundPersistentStream wraps a stream and tracks all responses for final saving to database.
// It implements the streams.Stream interface and handles persistence in the Close method.
//
//nolint:containedctx // Checked.
type InboundPersistentStream struct {
	ctx             context.Context
	stream          streams.Stream[*httpclient.StreamEvent]
	request         *ent.Request
	requestExec     *ent.RequestExecution
	requestService  *biz.RequestService
	transformer     transformer.Inbound
	perf            *biz.PerformanceRecord
	responseChunks  []*httpclient.StreamEvent
	closed          bool
	state           *PersistenceState
	streamCompleted bool
	providerStatus  streamTerminalStatus
}

var _ streams.Stream[*httpclient.StreamEvent] = (*InboundPersistentStream)(nil)

func NewInboundPersistentStream(
	ctx context.Context,
	stream streams.Stream[*httpclient.StreamEvent],
	request *ent.Request,
	requestExec *ent.RequestExecution,
	requestService *biz.RequestService,
	transformer transformer.Inbound,
	perf *biz.PerformanceRecord,
	state *PersistenceState,
) *InboundPersistentStream {
	s := &InboundPersistentStream{
		ctx:            ctx,
		stream:         stream,
		request:        request,
		requestExec:    requestExec,
		requestService: requestService,
		transformer:    transformer,
		perf:           perf,
		responseChunks: make([]*httpclient.StreamEvent, 0),
		closed:         false,
		state:          state,
	}

	return s
}

func (ts *InboundPersistentStream) Next() bool {
	return ts.stream.Next()
}

func (ts *InboundPersistentStream) Current() *httpclient.StreamEvent {
	event := ts.stream.Current()
	if event != nil {
		// For raw binary audio chunks (TTS stream_format=audio), persist only a size
		// summary to avoid buffering the full audio payload in memory.
		ts.responseChunks = append(ts.responseChunks, httpclient.SummarizeBinaryChunk(event))
		apiFormat := llm.APIFormat("")
		if ts.state != nil && ts.state.LlmRequest != nil {
			apiFormat = ts.state.LlmRequest.APIFormat
		}
		status := terminalStreamStatusForFormat(event, apiFormat)
		switch status {
		case streamTerminalCompleted:
			ts.streamCompleted = true
			ts.state.markStreamCompleted()
		case streamTerminalFailed, streamTerminalIncomplete, streamTerminalCanceled:
			ts.providerStatus = status
			ts.state.recordProviderTerminalStatus(status)
		}
	}

	return event
}

// isTerminalStreamEvent checks if the event represents the end of a successfully completed stream.
// For Chat Completions API this is the raw [DONE] event; for Responses API this is response.completed.
func isTerminalStreamEvent(event *httpclient.StreamEvent) bool {
	return terminalStreamStatus(event) == streamTerminalCompleted
}

type streamTerminalStatus uint8

const (
	streamTerminalNone streamTerminalStatus = iota
	streamTerminalCompleted
	streamTerminalFailed
	streamTerminalIncomplete
	streamTerminalCanceled
)

func terminalStreamStatus(event *httpclient.StreamEvent) streamTerminalStatus {
	return terminalStreamStatusForFormat(event, "")
}

func terminalStreamStatusForFormat(event *httpclient.StreamEvent, apiFormat llm.APIFormat) streamTerminalStatus {
	if event == nil {
		return streamTerminalNone
	}
	if bytes.Equal(event.Data, llm.DoneStreamEvent.Data) {
		if apiFormat == llm.APIFormatOpenAIResponse || apiFormat == llm.APIFormatAnthropicMessage {
			return streamTerminalNone
		}
		return streamTerminalCompleted
	}

	eventType := event.Type
	if eventType == "" {
		eventType = gjson.GetBytes(event.Data, "type").String()
	}
	switch eventType {
	case "response.failed":
		if apiFormat != "" && apiFormat != llm.APIFormatOpenAIResponse {
			return streamTerminalNone
		}
		return streamTerminalFailed
	case "response.incomplete":
		if apiFormat != "" && apiFormat != llm.APIFormatOpenAIResponse {
			return streamTerminalNone
		}
		return streamTerminalIncomplete
	case "response.cancelled", "response.canceled":
		if apiFormat != "" && apiFormat != llm.APIFormatOpenAIResponse {
			return streamTerminalNone
		}
		return streamTerminalCanceled
	case "response.completed":
		if apiFormat != "" && apiFormat != llm.APIFormatOpenAIResponse {
			return streamTerminalNone
		}
		switch gjson.GetBytes(event.Data, "response.status").String() {
		case "failed":
			return streamTerminalFailed
		case "incomplete":
			return streamTerminalIncomplete
		case "cancelled", "canceled":
			return streamTerminalCanceled
		default:
			return streamTerminalCompleted
		}
	case "message_stop", "speech.audio.done", "transcript.text.done", httpclient.BinaryStreamDoneEventType:
		if eventType == "message_stop" && apiFormat != "" && apiFormat != llm.APIFormatAnthropicMessage {
			return streamTerminalNone
		}
		return streamTerminalCompleted
	}

	choices := gjson.GetBytes(event.Data, "choices")
	if !choices.IsArray() {
		return streamTerminalNone
	}
	status := streamTerminalNone
	choices.ForEach(func(_, choice gjson.Result) bool {
		finishReason := choice.Get("finish_reason")
		if finishReason.Type != gjson.String || finishReason.String() == "" {
			return true
		}
		switch finishReason.String() {
		case "error":
			status = streamTerminalFailed
		case "length", "content_filter":
			status = streamTerminalIncomplete
		case "cancelled", "canceled":
			status = streamTerminalCanceled
		default:
			status = streamTerminalCompleted
		}
		return false
	})
	return status
}

func (ts *InboundPersistentStream) Err() error {
	return ts.stream.Err()
}

func (ts *InboundPersistentStream) Close() error {
	if ts.closed {
		return nil
	}

	ts.closed = true
	ctx := ts.ctx
	// The wrapped transformed stream owns RequestExecution finalization. Close
	// it before persisting the parent Request so the parent cannot become
	// terminal while its current execution is still processing.
	ts.state.recordStreamLifecycle(ctx, "stream_close_start")
	closeErr := ts.stream.Close()
	ts.state.recordStreamLifecycle(ctx, "stream_close_end")
	ts.state.recordStreamLifecycle(ctx, "parent_persist_start")
	defer ts.state.recordStreamLifecycle(ctx, "parent_persist_end")

	streamCompleted := ts.streamCompleted || ts.state.isStreamCompletionConfirmed()
	log.Debug(ctx, "Closing persistent stream", log.Int("chunk_count", len(ts.responseChunks)), log.Bool("received_done", streamCompleted))

	streamErr := ts.stream.Err()
	ctxErr := ctx.Err()
	requestFailurePersisted := false
	if deferredErr, persisted, _ := ts.state.deferredStreamFailure(); deferredErr != nil {
		streamErr = deferredErr
		requestFailurePersisted = persisted
	}
	requestContextCause := context.Cause(ctx)
	requestCancellation := (errors.Is(streamErr, context.Canceled) && errors.Is(requestContextCause, context.Canceled)) ||
		(errors.Is(streamErr, context.DeadlineExceeded) && errors.Is(requestContextCause, context.DeadlineExceeded))

	// A provider terminal event only proves that the raw upstream stream
	// completed. A later outbound/unified/inbound transform error still makes
	// the client-visible response invalid and must supersede that provisional
	// success. Cancellation remains handled below so a valid client disconnect
	// after a terminal event does not overwrite completion.
	if streamErr != nil && !requestCancellation {
		if requestFailurePersisted {
			ts.persistTerminalStreamFailureChunks(ctx)
		} else {
			ts.persistTerminalStreamFailure(ctx, streamErr)
		}

		return closeErr
	}

	providerStatus := ts.providerStatus
	if providerStatus == streamTerminalNone {
		providerStatus = ts.state.providerTerminalOutcome()
	}
	if providerStatus != streamTerminalNone {
		ts.persistProviderTerminalStatus(ctx, providerStatus)
		return closeErr
	}

	// If we received the [DONE] event, treat the stream as successfully completed
	// even if there's a context cancellation error. This handles the case where
	// the client disconnects immediately after receiving the last chunk.
	if streamCompleted {
		// Stream completed successfully - perform final persistence
		log.Debug(ctx, "Stream completed successfully (received terminal event), performing final persistence")
		ts.persistResponseChunks(ctx)

		return closeErr
	}

	// If we haven't received a terminal event, check if the chunks we DO have form a complete response.
	// This handles models that aggregate internally (like Codex) or upstream proxy hung connections
	// where the provider sent the full JSON payload but failed to send [DONE] before dropping.
	var responseBody []byte
	var meta llm.ResponseMeta
	var aggErr error

	if len(ts.responseChunks) > 0 && !streamCompleted {
		responseBody, meta, aggErr = ts.transformer.AggregateStreamChunks(context.WithoutCancel(ctx), ts.responseChunks)
		if aggErr == nil && meta.ID != "" && len(responseBody) > 0 && isCompletedAggregated(responseBody, meta) {
			log.Debug(ctx, "Stream has valid complete response without terminal event, treating as completed")
			ts.streamCompleted = true
			streamCompleted = true
			ts.state.markStreamCompleted()
		}
	}

	// Check if context was canceled (client disconnected before [DONE]).
	// Skip the error path if we determined the stream actually completed successfully above.
	if (ctxErr != nil || streamErr != nil) && !streamCompleted {
		errToReport := streamErr
		if errToReport == nil {
			errToReport = ctxErr
		}
		ts.persistTerminalStreamFailure(ctx, errToReport)

		return closeErr
	}

	// If the stream ended without a terminal event and we couldn't determine it was
	// completed through aggregation, mark it as incomplete/failed. This handles the case
	// where the upstream connection drops silently (EOF) without sending a terminal event,
	// which would otherwise fall through and incorrectly mark the request as "completed".
	if !streamCompleted {
		log.Debug(ctx, "Stream ended without terminal event or completed response, treating as incomplete")

		errToReport := errors.New("stream ended without terminal event or completed response")
		ts.persistTerminalStreamFailure(ctx, errToReport)

		return closeErr
	}

	// Stream completed successfully - perform final persistence
	log.Debug(ctx, "Stream completed successfully, performing final persistence")

	// We already aggregated the chunks above, so pass them directly to avoid double-aggregation
	if len(responseBody) > 0 {
		ts._persistResponse(context.WithoutCancel(ctx), responseBody, meta)
	} else {
		ts.persistResponseChunks(ctx)
	}

	return closeErr
}

func (ts *InboundPersistentStream) persistProviderTerminalStatus(ctx context.Context, status streamTerminalStatus) {
	if ts.request == nil {
		return
	}

	statusCtx, cancel := xcontext.DetachWithTimeout(ctx, 10*time.Second)
	defer cancel()
	var err error
	if status == streamTerminalCanceled {
		err = ts.requestService.MarkRequestCanceled(statusCtx, ts.request.ID)
	} else {
		err = ts.requestService.MarkRequestFailed(statusCtx, ts.request.ID)
	}
	if err != nil {
		log.Warn(statusCtx, "Failed to persist provider terminal request status", log.Cause(err))
	}
	ts.persistTerminalStreamFailureChunks(ctx)
}

func (ts *InboundPersistentStream) persistTerminalStreamFailure(ctx context.Context, streamErr error) {
	if ts.request == nil {
		return
	}

	requestContextCause := context.Cause(ctx)
	streamErr = terminalErrorCause(streamErr, requestContextCause)

	statusCtx, cancelStatus := xcontext.DetachWithTimeout(ctx, 10*time.Second)
	if err := ts.requestService.UpdateRequestStatusFromError(statusCtx, ts.request.ID, streamErr, requestContextCause); err != nil {
		log.Warn(statusCtx, "Failed to update request status from error", log.Cause(err))
	}
	cancelStatus()

	ts.persistTerminalStreamFailureChunks(ctx)
}

func (ts *InboundPersistentStream) persistTerminalStreamFailureChunks(ctx context.Context) {
	if ts.request == nil {
		return
	}

	chunksCtx, cancelChunks := xcontext.DetachWithTimeout(ctx, 10*time.Second)
	defer cancelChunks()

	if err := ts.requestService.SaveRequestChunks(chunksCtx, ts.request.ID, ts.responseChunks); err != nil {
		log.Warn(chunksCtx, "Failed to save terminal request chunks", log.Cause(err))
	}
}

func (ts *InboundPersistentStream) persistResponseChunks(ctx context.Context) {
	defer func() {
		if cause := recover(); cause != nil {
			log.Warn(ctx, "Failed to persist inbound response chunks", log.Any("cause", cause))
		}
	}()

	// Use context without cancellation to ensure persistence even if client canceled
	persistCtx := context.WithoutCancel(ctx)

	// Aggregate stream chunks first, then delegate to _persistResponse
	responseBody, meta, err := ts.transformer.AggregateStreamChunks(persistCtx, ts.responseChunks)
	if err != nil {
		log.Warn(persistCtx, "Failed to aggregate chunks for main request", log.Cause(err))
		dumper.DumpStreamEvents(persistCtx, ts.responseChunks, "response_chunks.json")
	}

	ts._persistResponse(persistCtx, responseBody, meta)
}

// _persistResponse performs the actual persistence with pre-aggregated data.
// This avoids redundant aggregation when the data is already available.
func (ts *InboundPersistentStream) _persistResponse(ctx context.Context, responseBody []byte, meta llm.ResponseMeta) {
	if ts.request == nil {
		return
	}

	// Build latency metrics from performance record
	var metrics *biz.LatencyMetrics

	if ts.perf != nil {
		firstTokenLatencyMs, requestLatencyMs, _ := ts.perf.Calculate()

		metrics = &biz.LatencyMetrics{
			LatencyMs: &requestLatencyMs,
		}
		if ts.perf.Stream && ts.perf.FirstTokenTime != nil {
			metrics.FirstTokenLatencyMs = &firstTokenLatencyMs
		}
	}

	err := ts.requestService.UpdateRequestCompleted(ctx, ts.request.ID, meta.ID, responseBody, metrics)
	if err != nil {
		log.Warn(ctx, "Failed to update request status to completed", log.Cause(err))
	}

	// Save all response chunks at once
	if err := ts.requestService.SaveRequestChunks(ctx, ts.request.ID, ts.responseChunks); err != nil {
		log.Warn(ctx, "Failed to save request chunks", log.Cause(err))
	}
}

// PersistentInboundTransformer wraps an inbound transformer with enhanced capabilities.
type PersistentInboundTransformer struct {
	wrapped transformer.Inbound
	state   *PersistenceState
}

func (p *PersistentInboundTransformer) TransformError(ctx context.Context, rawErr error) *httpclient.Error {
	return p.wrapped.TransformError(ctx, rawErr)
}

func (p *PersistentInboundTransformer) TransformRequest(ctx context.Context, request *httpclient.Request) (*llm.Request, error) {
	llmRequest, err := p.wrapped.TransformRequest(ctx, request)
	if err != nil {
		return nil, err
	}

	llmRequest.RawRequest = request
	p.state.RawRequest = request
	p.state.LlmRequest = llmRequest
	p.state.OriginalRequestStream = llmRequest.Stream

	return llmRequest, nil
}

func (p *PersistentInboundTransformer) TransformResponse(ctx context.Context, response *llm.Response) (*httpclient.Response, error) {
	return p.wrapped.TransformResponse(ctx, response)
}

func (p *PersistentInboundTransformer) TransformStream(ctx context.Context, stream streams.Stream[*llm.Response]) (streams.Stream[*httpclient.StreamEvent], error) {
	channelStream, err := p.wrapped.TransformStream(ctx, stream)
	if err != nil {
		return nil, err
	}

	persistentStream := NewInboundPersistentStream(
		ctx,
		channelStream,
		p.state.Request,
		p.state.RequestExec,
		p.state.RequestService,
		p, // Use the PersistentInboundTransformer as the transformer
		p.state.Perf,
		p.state,
	)

	return persistentStream, nil
}

func (p *PersistentInboundTransformer) AggregateStreamChunks(ctx context.Context, chunks []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	return p.wrapped.AggregateStreamChunks(ctx, chunks)
}
