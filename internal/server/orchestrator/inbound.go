package orchestrator

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"time"
	"unsafe"

	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/dumper"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/pkg/xcontext"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
	"github.com/looplj/axonhub/llm/transformer/shared"
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
	observation     *biz.ObservationStreamBuffer
	metadataOnly    bool
	streamMeta      llm.ResponseMeta
	currentEvent    *httpclient.StreamEvent
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
	obs := requestService.NewObservationStreamBuffer(ctx, nil, false)
	metadataOnly := false
	if obs == nil && requestService != nil && requestService.SystemService != nil {
		policyCtx := authz.WithSystemBypass(context.WithoutCancel(ctx), "inbound-stream-storage-policy")
		if policy, err := requestService.SystemService.StoragePolicy(policyCtx); err == nil && policy != nil {
			if !policy.StoreResponseBody && !policy.StoreChunks {
				metadataOnly = true
			}
		}
	}
	s := &InboundPersistentStream{
		ctx:            ctx,
		stream:         stream,
		request:        request,
		requestExec:    requestExec,
		requestService: requestService,
		transformer:    transformer,
		perf:           perf,
		closed:         false,
		state:          state,
		observation:    obs,
		metadataOnly:   metadataOnly,
	}

	return s
}

func (ts *InboundPersistentStream) Next() bool {
	if !ts.stream.Next() {
		return false
	}
	ts.currentEvent = ts.stream.Current()
	if biz.ForwardingAccountingEnabled(ts.ctx) && !ts.state.providerUsageFinal.Load() {
		format := ts.state.LlmRequest.APIFormat
		if providerStreamTerminal(format, ts.currentEvent) && terminalStreamStatusForFormat(ts.currentEvent, format) == streamTerminalCompleted {
			// A transformer can synthesize success before reading the provider's
			// terminal. Drain that provider tail so late stream errors remain visible.
			for ts.stream.Next() {
				_ = ts.stream.Current()
			}
			if ts.stream.Err() != nil {
				return false
			}
		}
	}
	return true
}

func (ts *InboundPersistentStream) Current() *httpclient.StreamEvent {
	event := ts.currentEvent
	if event != nil {
		// For raw binary audio chunks (TTS stream_format=audio), persist only a size
		// summary to avoid buffering the full audio payload in memory.
		if ts.observation != nil {
			if event.Type == "speech.audio.done" || event.Type == httpclient.BinaryStreamDoneEventType {
				ts.streamMeta.ID = llm.SpeechStreamResponseID
			} else if ts.streamMeta.ID == "" {
				ts.streamMeta.ID = extractStreamResponseID(event)
			}
			ts.responseChunks = ts.observation.Append(httpclient.SummarizeBinaryChunk(event))
			if ClassifyStreamSemanticTerminal(event) == StreamSemanticSucceeded {
				ts.streamMeta.Completed = true
			}
		} else if ts.metadataOnly {
			if event.Type == "speech.audio.done" || event.Type == httpclient.BinaryStreamDoneEventType {
				ts.streamMeta.ID = llm.SpeechStreamResponseID
			} else if ts.streamMeta.ID == "" {
				ts.streamMeta.ID = extractStreamResponseID(event)
			}
			if ClassifyStreamSemanticTerminal(event) == StreamSemanticSucceeded {
				ts.streamMeta.Completed = true
			}
		} else {
			ts.responseChunks = append(ts.responseChunks, httpclient.SummarizeBinaryChunk(event))
		}
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

// extractStreamResponseID extracts a provider response/message ID from early
// stream events using zero-copy gjson lookup plus strings.Clone so the raw
// event buffer is never pinned.
func extractStreamResponseID(event *httpclient.StreamEvent) string {
	if event == nil || len(event.Data) == 0 {
		return ""
	}
	if !bytes.Contains(event.Data, []byte(`"id"`)) {
		return ""
	}
	s := unsafe.String(unsafe.SliceData(event.Data), len(event.Data))
	if id := gjson.Get(s, "message.id"); id.Type == gjson.String && id.Str != "" {
		return strings.Clone(id.Str)
	}
	if id := gjson.Get(s, "response.id"); id.Type == gjson.String && id.Str != "" {
		return strings.Clone(id.Str)
	}
	if id := gjson.Get(s, "id"); id.Type == gjson.String && id.Str != "" {
		return strings.Clone(id.Str)
	}
	return ""
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
	defer ts.observation.Close()
	defer func() { ts.responseChunks = nil }()
	ctx := ts.observation.Context(ts.ctx)
	defer biz.EndForwardingObservation(ctx)
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

	if (ts.metadataOnly || (ts.observation != nil && (!ts.observation.BodyEnabled || ts.observation.Unavailable))) && ts.streamMeta.Completed {
		streamCompleted = true
		ts.state.markStreamCompleted()
	}
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
	if err := ts.requestService.UpdateRequestStatusFromErrorDetails(
		statusCtx,
		ts.request.ID,
		streamErr,
		requestContextCause,
		ts.state.currentAttemptErrorInfo(),
	); err != nil {
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
	if ts.metadataOnly || (ts.observation != nil && (!ts.observation.BodyEnabled || ts.observation.Unavailable)) {
		ts._persistResponse(context.WithoutCancel(ctx), nil, ts.streamMeta)
		return
	}
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
	if p.state != nil {
		p.state.inboundTransformer = p.wrapped
		p.state.llmRequestShallow = false
		p.state.RawBodyAmbiguous = false
	}

	var llmRequest *llm.Request
	var shallow bool
	if p.state != nil && !shared.NativeResponsesWebSocket(ctx) {
		llmRequest, shallow = tryShallowTransformRequest(p.wrapped, request, p.state)
	}
	if !shallow {
		var err error
		llmRequest, err = p.wrapped.TransformRequest(ctx, request)
		if err != nil {
			return nil, err
		}
	} else {
		p.state.llmRequestShallow = true
	}

	llmRequest.RawRequest = request
	p.state.RawRequest = request
	p.state.LlmRequest = llmRequest
	if p.state.RequestedModel == "" {
		p.state.RequestedModel = llmRequest.Model
	}
	p.state.OriginalRequestStream = llmRequest.Stream

	return llmRequest, nil
}

// tryShallowTransformRequest extracts scalar routing/policy fields in a single
// top-level gjson scan for the three high-volume chat inbound formats, avoiding
// a multi-MB AST unmarshal until a downstream stage actually needs Messages or
// Tools. Any missing required field, invalid JSON, duplicate top-level key,
// unsupported content-type, or special-case request falls back to the wrapped
// transformer so validation behavior and error messages remain identical.
func tryShallowTransformRequest(wrapped transformer.Inbound, request *httpclient.Request, state *PersistenceState) (*llm.Request, bool) {
	if request == nil || len(request.Body) == 0 {
		return nil, false
	}
	body := bytes.TrimSpace(request.Body)
	if len(body) < 2 || body[0] != '{' || body[len(body)-1] != '}' {
		return nil, false
	}
	if !gjson.ValidBytes(body) {
		return nil, false
	}
	s := unsafe.String(unsafe.SliceData(body), len(body))

	// Single top-level object scan BEFORE any Content-Type or route early-return:
	// detects duplicate keys (which json.Unmarshal resolves last-wins while gjson/sjson
	// resolve first-wins) so any fallback to full parsing marks RawBodyAmbiguous = true
	// and prevents pass-through / sjson reuse of ambiguous raw JSON bytes.
	var (
		seenKeys  = make(map[string]struct{}, 16)
		hasDupKey bool
		fields    = make(map[string]gjson.Result, 16)
	)
	gjson.Parse(s).ForEach(func(k, v gjson.Result) bool {
		key := k.Str
		lowerKey := strings.ToLower(key)
		if _, exists := seenKeys[lowerKey]; exists {
			hasDupKey = true
			return false
		}
		if key != lowerKey && (lowerKey == "model" || lowerKey == "stream") {
			hasDupKey = true
			return false
		}
		seenKeys[lowerKey] = struct{}{}
		fields[key] = v
		return true
	})
	if hasDupKey {
		if state != nil {
			state.RawBodyAmbiguous = true
		}
		return nil, false
	}

	var isAnthropic, isOpenAIChat, isOpenAIResponses bool
	switch wrapped.(type) {
	case *anthropic.InboundTransformer:
		if !strings.Contains(strings.ToLower(request.Headers.Get("Content-Type")), "application/json") {
			return nil, false
		}
		isAnthropic = true
	case *openai.InboundTransformer:
		if !strings.Contains(strings.ToLower(request.Headers.Get("Content-Type")), "application/json") {
			return nil, false
		}
		isOpenAIChat = true
	case *responses.InboundTransformer:
		if strings.HasSuffix(request.Path, "/compact") || strings.Contains(request.URL, "/compact") {
			return nil, false
		}
		if ct := request.Headers.Get("Content-Type"); ct != "" && !strings.Contains(strings.ToLower(ct), "application/json") {
			return nil, false
		}
		isOpenAIResponses = true
	default:
		return nil, false
	}

	model, ok := fields["model"]
	if !ok || model.Type != gjson.String || model.Str == "" {
		return nil, false
	}

	var streamPtr *bool
	if st, ok := fields["stream"]; ok && st.Type != gjson.Null {
		if st.Type != gjson.True && st.Type != gjson.False {
			return nil, false
		}
		v := st.Bool()
		streamPtr = &v
	}

	switch {
	case isAnthropic:
		msgs, ok := fields["messages"]
		if !ok || !msgs.IsArray() || len(msgs.Array()) == 0 {
			return nil, false
		}
		maxTokens, ok := fields["max_tokens"]
		if !ok || maxTokens.Type != gjson.Number || maxTokens.Int() <= 0 || float64(maxTokens.Int()) != maxTokens.Float() {
			return nil, false
		}
		// Validate system prompts (and detect billing header even if JSON-escaped).
		if sys, ok := fields["system"]; ok && sys.Type != gjson.Null {
			switch {
			case sys.Type == gjson.String:
				if strings.Contains(sys.Str, "x-anthropic-billing-header:") {
					return nil, false
				}
			case sys.IsArray():
				valid := true
				sys.ForEach(func(_, item gjson.Result) bool {
					if !item.IsObject() || item.Get("type").Str != "text" {
						valid = false
						return false
					}
					if strings.Contains(item.Get("text").Str, "x-anthropic-billing-header:") {
						valid = false
						return false
					}
					return true
				})
				if !valid {
					return nil, false
				}
			default:
				return nil, false
			}
		}
		// Validate thinking configuration matching anthropic/inbound.go.
		var reasoningBudget *int64
		if thinking, ok := fields["thinking"]; ok && thinking.Type != gjson.Null {
			if !thinking.IsObject() {
				return nil, false
			}
			tType := thinking.Get("type")
			if tType.Type != gjson.String {
				return nil, false
			}
			switch tType.Str {
			case "disabled":
			case "enabled":
				bt := thinking.Get("budget_tokens")
				if bt.Type != gjson.Number || bt.Int() <= 0 {
					return nil, false
				}
				b := bt.Int()
				reasoningBudget = &b
			case "adaptive":
				if oc, ok := fields["output_config"]; ok && oc.IsObject() {
					if eff := oc.Get("effort"); eff.Exists() && eff.Type != gjson.Null {
						if eff.Type != gjson.String {
							return nil, false
						}
						switch eff.Str {
						case "", "low", "medium", "high", "xhigh", "max":
						default:
							return nil, false
						}
					}
				}
			default:
				return nil, false
			}
		}
		// Validate tool_choice matching anthropic/inbound.go.
		if tc, ok := fields["tool_choice"]; ok && tc.Type != gjson.Null {
			if !tc.IsObject() {
				return nil, false
			}
			tcType := tc.Get("type")
			if tcType.Type != gjson.String {
				return nil, false
			}
			switch tcType.Str {
			case "auto", "none", "any":
			case "tool":
				if name := tc.Get("name"); name.Type != gjson.String || name.Str == "" {
					return nil, false
				}
			default:
				return nil, false
			}
		}
		var metadata map[string]string
		if md, ok := fields["metadata"]; ok && md.Type != gjson.Null {
			if !md.IsObject() {
				return nil, false
			}
			uid := md.Get("user_id")
			if uid.Exists() && uid.Type != gjson.Null && uid.Type != gjson.String {
				return nil, false
			}
			metadata = map[string]string{
				"user_id": strings.Clone(uid.Str),
			}
		}
		mt := maxTokens.Int()
		req := &llm.Request{
			APIFormat:       llm.APIFormatAnthropicMessage,
			RequestType:     llm.RequestTypeChat,
			Model:           strings.Clone(model.Str),
			MaxTokens:       &mt,
			Stream:          streamPtr,
			ReasoningBudget: reasoningBudget,
			Metadata:        metadata,
		}
		if oc, ok := fields["output_config"]; ok && oc.IsObject() {
			if eff := oc.Get("effort"); eff.Type == gjson.String && eff.Str != "" {
				req.ReasoningEffort = strings.Clone(eff.Str)
			}
		}
		return req, true

	case isOpenAIChat:
		msgs, ok := fields["messages"]
		if !ok || !msgs.IsArray() || len(msgs.Array()) == 0 {
			return nil, false
		}
		metadata, ok := extractShallowStringMap(fields["metadata"])
		if !ok {
			return nil, false
		}
		userPtr, ok := extractShallowOptionalString(fields["user"])
		if !ok {
			return nil, false
		}
		req := &llm.Request{
			APIFormat:   llm.APIFormatOpenAIChatCompletion,
			RequestType: llm.RequestTypeChat,
			Model:       strings.Clone(model.Str),
			Stream:      streamPtr,
			Metadata:    metadata,
			User:        userPtr,
		}
		if eff, ok := fields["reasoning_effort"]; ok && eff.Type != gjson.Null {
			if eff.Type != gjson.String {
				return nil, false
			}
			if eff.Str != "" {
				req.ReasoningEffort = strings.Clone(eff.Str)
			}
		}
		if pck, ok := fields["prompt_cache_key"]; ok && pck.Type != gjson.Null {
			if pck.Type != gjson.String {
				return nil, false
			}
			if pck.Str != "" {
				v := strings.Clone(pck.Str)
				req.PromptCacheKey = &v
			}
		}
		return req, true

	case isOpenAIResponses:
		if _, ok := fields["input"]; !ok {
			return nil, false
		}
		metadata, ok := extractShallowStringMap(fields["metadata"])
		if !ok {
			return nil, false
		}
		userPtr, ok := extractShallowOptionalString(fields["user"])
		if !ok {
			return nil, false
		}
		req := &llm.Request{
			APIFormat:   llm.APIFormatOpenAIResponse,
			RequestType: llm.RequestTypeChat,
			Model:       strings.Clone(model.Str),
			Stream:      streamPtr,
			Metadata:    metadata,
			User:        userPtr,
		}
		if reasoning, ok := fields["reasoning"]; ok && reasoning.Type != gjson.Null {
			if !reasoning.IsObject() {
				return nil, false
			}
			if eff := reasoning.Get("effort"); eff.Exists() && eff.Type != gjson.Null {
				if eff.Type != gjson.String {
					return nil, false
				}
				if eff.Str != "" {
					req.ReasoningEffort = strings.Clone(eff.Str)
				}
			}
		}
		if pck, ok := fields["prompt_cache_key"]; ok && pck.Type != gjson.Null {
			if pck.Type != gjson.String {
				return nil, false
			}
			if pck.Str != "" {
				v := strings.Clone(pck.Str)
				req.PromptCacheKey = &v
			}
		}
		if prev, ok := fields["previous_response_id"]; ok && prev.Type != gjson.Null {
			if prev.Type != gjson.String {
				return nil, false
			}
			if prev.Str != "" {
				v := strings.Clone(prev.Str)
				req.PreviousResponseID = &v
			}
		}
		return req, true
	}

	return nil, false
}

func extractShallowStringMap(res gjson.Result) (map[string]string, bool) {
	if !res.Exists() || res.Type == gjson.Null {
		return nil, true
	}
	if !res.IsObject() {
		return nil, false
	}
	out := make(map[string]string)
	valid := true
	res.ForEach(func(k, v gjson.Result) bool {
		if v.Type != gjson.String {
			valid = false
			return false
		}
		out[strings.Clone(k.Str)] = strings.Clone(v.Str)
		return true
	})
	if !valid {
		return nil, false
	}
	return out, true
}

func extractShallowOptionalString(res gjson.Result) (*string, bool) {
	if !res.Exists() || res.Type == gjson.Null {
		return nil, true
	}
	if res.Type != gjson.String {
		return nil, false
	}
	v := strings.Clone(res.Str)
	return &v, true
}

// WrapDirectPassThroughInboundStream wraps a raw pass-through stream with
// InboundPersistentStream without running LLM AST stream conversion.
func (p *PersistentInboundTransformer) WrapDirectPassThroughInboundStream(
	ctx context.Context,
	stream streams.Stream[*httpclient.StreamEvent],
) (streams.Stream[*httpclient.StreamEvent], error) {
	return NewInboundPersistentStream(
		ctx,
		stream,
		p.state.Request,
		p.state.RequestExec,
		p.state.RequestService,
		p,
		p.state.Perf,
		p.state,
	), nil
}

func (p *PersistentInboundTransformer) TransformResponse(ctx context.Context, response *llm.Response) (*httpclient.Response, error) {
	return p.wrapped.TransformResponse(ctx, response)
}

func (p *PersistentInboundTransformer) TransformStream(ctx context.Context, stream streams.Stream[*llm.Response]) (streams.Stream[*httpclient.StreamEvent], error) {
	var meta llm.ResponseMeta
	stream = streams.Map(stream, func(response *llm.Response) *llm.Response {
		if response != nil && response.ID != "" {
			meta.ID = response.ID
		}
		return response
	})
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

	return streams.Map(persistentStream, func(event *httpclient.StreamEvent) *httpclient.StreamEvent {
		if meta.ID != "" {
			persistentStream.streamMeta.ID = meta.ID
		}
		return event
	}), nil
}

func (p *PersistentInboundTransformer) AggregateStreamChunks(ctx context.Context, chunks []*httpclient.StreamEvent) ([]byte, llm.ResponseMeta, error) {
	return p.wrapped.AggregateStreamChunks(ctx, chunks)
}
