package orchestrator

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline/cc"
	"github.com/looplj/axonhub/llm/transformer"
)

// PersistenceState holds shared state with channel management and retry capabilities.
// TODO: move the dependencies out of the state to make it a real state.
type PersistenceState struct {
	APIKey *ent.APIKey

	cyberSession *cyberSessionRequestState
	// The provider reached its final usage frame or EOF. This describes the
	// wire stream, regardless of whether usage could be submitted to the writer.
	providerUsageFinal atomic.Bool

	RequestService      *biz.RequestService
	SystemService       *biz.SystemService
	UsageLogService     *biz.UsageLogService
	ChannelService      *biz.ChannelService
	PromptProvider      PromptProvider
	PromptProtecter     PromptProtecter
	RetryPolicyProvider RetryPolicyProvider
	CandidateSelector   CandidateSelector
	LoadBalancer        *LoadBalancer

	// Request state
	ModelMapper *ModelMapper
	// Proxy config, will be used to override channel's default proxy config.
	Proxy *httpclient.ProxyConfig

	// OriginalModel is the model after API key profile mapping, used for channel selection
	OriginalModel string
	// RequestedModel is the public model name from the client, before API key mapping.
	RequestedModel string
	RawRequest     *httpclient.Request
	LlmRequest     *llm.Request

	// llmRequestShallow records that LlmRequest was populated via zero-copy scalar
	// extraction for same-protocol pass-through and has not yet unmarshaled the
	// full Messages/Tools AST. EnsureFullLlmRequest materializes it on demand.
	llmRequestShallow  bool
	inboundTransformer transformer.Inbound

	// OriginalRequestStream stores the client's original stream intent before any
	// candidate-specific forcing to provider-side streaming happens.
	OriginalRequestStream *bool

	// ReasoningBoundsActive records that the selected logical model has configured bounds.
	ReasoningBoundsActive bool
	// ReasoningBoundsEnforced records that the inbound effort was a standard level
	// (or was omitted), so the bounds have an ordered meaning for this request.
	ReasoningBoundsEnforced bool
	// ReasoningBoundsChanged records that applying the bounds changed or supplied
	// the logical effort. Numeric reasoning budgets are reconciled only then.
	ReasoningBoundsChanged             bool
	ReasoningBoundsEffort              string
	ReasoningBoundsBudget              *int64
	ReasoningBoundsExtraBody           []byte
	ReasoningBoundsTransformerMetadata map[string]any
	ReasoningWireSnapshot              *reasoningWireSnapshot

	// Persistence state
	Request     *ent.Request
	RequestExec *ent.RequestExecution

	// ChannelModelsCandidates is the primary state for channel selection
	ChannelModelsCandidates []*ChannelModelsCandidate
	// Candidate state - current candidate index of ChannelModelsCandidates
	CurrentCandidateIndex int
	// CurrentCandidate is the currently selected candidate of ChannelModelsCandidates
	CurrentCandidate *ChannelModelsCandidate
	// CurrentModelIndex is the current model index in CurrentCandidate.Models
	CurrentModelIndex int

	// Perf is the performance record for the current request.
	Perf *biz.PerformanceRecord

	// FailurePolicyRoutingChanged is set when the current failed attempt applied
	// a request-time failure policy that changed channel/key routability. Retry
	// selection uses it to retry even for otherwise non-retryable status codes
	// such as 401 after the bad key has been removed from routing.
	FailurePolicyRoutingChanged bool

	// StreamCompleted tracks whether the stream has response successfully completed.
	// This is used to distinguish between a stream that was canceled mid-way
	// versus a stream that completed successfully but the client disconnected
	// immediately after receiving the last chunk.
	StreamCompleted bool
	// providerTerminalStatus records an abnormal provider-declared terminal
	// outcome that arrived as a valid stream event rather than a transport error.
	providerTerminalStatus streamTerminalStatus

	// RawProviderResponse stores the raw provider response for non-stream response pass-through.
	RawProviderResponse *httpclient.Response
	// ProviderStreamResponse stores the headers/status for an established provider
	// stream. Its body remains owned by the stream decoder and is never read here.
	ProviderStreamResponse *httpclient.Response

	// RawProviderRequest stores the actual outbound provider request for pass-through checks.
	RawProviderRequest *httpclient.Request

	// RawStreamCh receives raw provider stream events for stream response pass-through.
	RawStreamCh chan *httpclient.StreamEvent

	// RawStreamErrRef points to the current attempt's local error variable used by the
	// captureRawProviderStream fan-out goroutine. Using a per-attempt pointer (instead of
	// a single shared field) prevents data races when retries spawn a new goroutine before
	// the previous one has exited.
	RawStreamErrRef *error

	// RawStreamCancel cancels the current attempt's fan-out goroutine started by
	// captureRawProviderStream. Must be called in PrepareForRetry and NextChannel so the
	// abandoned goroutine exits promptly and releases its upstream HTTP connection.
	RawStreamCancel context.CancelFunc
	// RawStreamAttempt owns every goroutine created for the current pass-through
	// provider attempt. Retry waits for it before replacing attempt-local state.
	RawStreamAttempt *passThroughAttempt

	// PassThroughApplied records whether the inbound request body was substituted during pass-through.
	PassThroughApplied bool
	// DirectPassThroughStream records whether the current streaming attempt bypasses
	// LLM AST stream conversion and shadow drain goroutines.
	DirectPassThroughStream bool
	// rawStreamObs is shared across direct pass-through performance, TPM, and
	// model circuit-breaker wrappers so each raw SSE frame is scanned once.
	rawStreamObs *rawStreamObserver
	// ASTContentMutated records whether an inbound policy middleware mutated the
	// request content (e.g., prompt injection, prompt protection masking, billing
	// header stripping, or auto reasoning effort), making the transformed AST the
	// authoritative wire source instead of the unmutated raw inbound bytes.
	ASTContentMutated bool
	// RawBodyAmbiguous records that the raw inbound JSON contained duplicate
	// top-level keys (where json.Unmarshal uses last-wins while gjson/sjson uses
	// first-wins), so outbound serialization must emit canonical AST bytes rather
	// than reusing or patching the ambiguous raw inbound body.
	RawBodyAmbiguous bool

	deferredFailureMu                 sync.Mutex
	deferredStreamError               error
	attemptErrorInfo                  *biz.ExecutionErrorInfo
	deferredRequestFailurePersisted   bool
	deferredExecutionFailurePersisted bool
	failedStreamEvidence              *OutboundPersistentStream
	streamCompletionConfirmed         bool
	semanticInterruptStarted          bool
	lifecycleStarted                  time.Time
	lifecycleSequence                 uint64
}

func (s *PersistenceState) hasASTContentMutation() bool {
	if s == nil {
		return false
	}
	if s.ASTContentMutated || s.RawBodyAmbiguous {
		return true
	}
	if s.LlmRequest != nil && s.LlmRequest.TransformerMetadata != nil {
		if _, ok := s.LlmRequest.TransformerMetadata[cc.BillingCCHKey]; ok {
			return true
		}
		if _, ok := s.LlmRequest.TransformerMetadata[cc.BillingHeaderStrippedKey]; ok {
			return true
		}
		if _, ok := s.LlmRequest.TransformerMetadata[autoReasoningEffortAppliedKey]; ok {
			return true
		}
	}
	return false
}

func (s *PersistenceState) cyberPolicyObserved() bool {
	return s != nil && s.cyberSession != nil && s.cyberSession.observedCyberPolicy()
}

func (s *PersistenceState) recordStreamLifecycle(ctx context.Context, phase string, fields ...log.Field) {
	if s == nil {
		return
	}

	s.deferredFailureMu.Lock()
	if s.lifecycleStarted.IsZero() {
		s.lifecycleStarted = time.Now()
	}
	s.lifecycleSequence++
	sequence := s.lifecycleSequence
	elapsed := time.Since(s.lifecycleStarted)
	requestID := 0
	if s.Request != nil {
		requestID = s.Request.ID
	}
	executionID := 0
	if s.RequestExec != nil {
		executionID = s.RequestExec.ID
	}
	attempt := s.CurrentCandidateIndex
	modelAttempt := s.CurrentModelIndex
	logFields := []log.Field{
		log.String("phase", phase),
		log.Int64("lifecycle_sequence", int64(sequence)),
		log.Int64("lifecycle_elapsed_us", elapsed.Microseconds()),
		log.Int("request_id", requestID),
		log.Int("request_execution_id", executionID),
		log.Int("candidate_attempt", attempt),
		log.Int("model_attempt", modelAttempt),
	}
	logFields = append(logFields, fields...)
	// Keep emission under the ledger lock so log order matches sequence order.
	log.Info(ctx, "stream lifecycle", logFields...)
	s.deferredFailureMu.Unlock()
}

// reserveRawStreamFailure linearizes a genuine raw read failure against the
// API's semantic-completion confirmation before terminal reporting middleware
// can persist it. The decoder filters locally interrupted reads by provenance;
// every non-nil error that reaches this boundary remains authoritative.
func (s *PersistenceState) reserveRawStreamFailure(err error) bool {
	if s == nil || err == nil {
		return false
	}

	s.deferredFailureMu.Lock()
	defer s.deferredFailureMu.Unlock()
	if s.deferredStreamError == nil {
		s.deferredStreamError = err
	}
	return true
}

func (s *PersistenceState) reserveDeferredStreamFailure(err error) {
	if s == nil || err == nil {
		return
	}

	s.deferredFailureMu.Lock()
	if s.deferredStreamError == nil {
		s.deferredStreamError = err
	}
	s.deferredFailureMu.Unlock()
}

func (s *PersistenceState) confirmStreamCompletion() bool {
	if s == nil {
		return false
	}

	s.deferredFailureMu.Lock()
	defer s.deferredFailureMu.Unlock()
	if s.deferredStreamError != nil {
		return false
	}
	s.StreamCompleted = true
	s.streamCompletionConfirmed = true
	return true
}

func (s *PersistenceState) markStreamCompleted() {
	if s == nil {
		return
	}
	s.deferredFailureMu.Lock()
	s.StreamCompleted = true
	s.deferredFailureMu.Unlock()
}

func (s *PersistenceState) beginSemanticCompletionInterrupt() bool {
	if s == nil {
		return false
	}

	s.deferredFailureMu.Lock()
	defer s.deferredFailureMu.Unlock()
	if !s.streamCompletionConfirmed || s.deferredStreamError != nil {
		return false
	}
	s.semanticInterruptStarted = true
	return true
}

func (s *PersistenceState) isSemanticCompletionInterruptStarted() bool {
	if s == nil {
		return false
	}

	s.deferredFailureMu.Lock()
	defer s.deferredFailureMu.Unlock()
	return s.streamCompletionConfirmed && s.semanticInterruptStarted
}

func (s *PersistenceState) isStreamCompletionConfirmed() bool {
	if s == nil {
		return false
	}

	s.deferredFailureMu.Lock()
	defer s.deferredFailureMu.Unlock()
	return s.streamCompletionConfirmed
}

func (s *PersistenceState) recordProviderTerminalStatus(status streamTerminalStatus) {
	if s == nil || status == streamTerminalNone || status == streamTerminalCompleted {
		return
	}
	s.deferredFailureMu.Lock()
	s.providerTerminalStatus = status
	s.deferredFailureMu.Unlock()
}

func (s *PersistenceState) resetStreamTerminalState() {
	if s == nil {
		return
	}
	s.deferredFailureMu.Lock()
	s.StreamCompleted = false
	s.DirectPassThroughStream = false
	s.rawStreamObs = nil
	s.providerTerminalStatus = streamTerminalNone
	s.ProviderStreamResponse = nil
	s.deferredStreamError = nil
	s.attemptErrorInfo = nil
	s.deferredRequestFailurePersisted = false
	s.deferredExecutionFailurePersisted = false
	s.failedStreamEvidence = nil
	s.streamCompletionConfirmed = false
	s.semanticInterruptStarted = false
	s.providerUsageFinal.Store(false)
	s.deferredFailureMu.Unlock()
}

func cloneExecutionErrorInfo(info *biz.ExecutionErrorInfo) *biz.ExecutionErrorInfo {
	if info == nil {
		return nil
	}
	cloned := *info
	if info.StatusCode != nil {
		statusCode := *info.StatusCode
		cloned.StatusCode = &statusCode
	}
	cloned.ResponseBody = append(cloned.ResponseBody[:0:0], info.ResponseBody...)
	return &cloned
}

func (s *PersistenceState) recordAttemptErrorInfo(info *biz.ExecutionErrorInfo) {
	if s == nil {
		return
	}
	s.deferredFailureMu.Lock()
	s.attemptErrorInfo = cloneExecutionErrorInfo(info)
	s.deferredFailureMu.Unlock()
}

func (s *PersistenceState) currentAttemptErrorInfo() *biz.ExecutionErrorInfo {
	if s == nil {
		return nil
	}
	s.deferredFailureMu.Lock()
	defer s.deferredFailureMu.Unlock()
	return cloneExecutionErrorInfo(s.attemptErrorInfo)
}

func (s *PersistenceState) registerFailedStreamEvidence(stream *OutboundPersistentStream) {
	if s == nil {
		return
	}
	s.deferredFailureMu.Lock()
	s.failedStreamEvidence = stream
	s.deferredFailureMu.Unlock()
}

func (s *PersistenceState) clearFailedStreamEvidence(stream *OutboundPersistentStream) {
	if s == nil {
		return
	}
	s.deferredFailureMu.Lock()
	if s.failedStreamEvidence == stream {
		s.failedStreamEvidence = nil
	}
	s.deferredFailureMu.Unlock()
}

func (s *PersistenceState) currentFailedStreamErrorInfo(ctx context.Context) *biz.ExecutionErrorInfo {
	if s == nil {
		return nil
	}
	s.deferredFailureMu.Lock()
	stream := s.failedStreamEvidence
	s.deferredFailureMu.Unlock()
	if stream == nil {
		return nil
	}
	return stream.terminalStreamErrorInfo(ctx)
}

func (s *PersistenceState) providerTerminalOutcome() streamTerminalStatus {
	if s == nil {
		return streamTerminalNone
	}
	s.deferredFailureMu.Lock()
	defer s.deferredFailureMu.Unlock()
	return s.providerTerminalStatus
}

func (s *PersistenceState) recordDeferredRequestFailure(err error, persisted bool) {
	if s == nil || err == nil {
		return
	}

	s.deferredFailureMu.Lock()
	defer s.deferredFailureMu.Unlock()

	s.deferredStreamError = err
	s.deferredRequestFailurePersisted = persisted
}

func (s *PersistenceState) recordDeferredExecutionFailure(err error, persisted bool) {
	if s == nil || err == nil {
		return
	}

	s.deferredFailureMu.Lock()
	defer s.deferredFailureMu.Unlock()

	s.deferredStreamError = err
	s.deferredExecutionFailurePersisted = persisted
}

func (s *PersistenceState) deferredStreamFailure() (error, bool, bool) {
	if s == nil {
		return nil, false, false
	}

	s.deferredFailureMu.Lock()
	defer s.deferredFailureMu.Unlock()

	return s.deferredStreamError, s.deferredRequestFailurePersisted, s.deferredExecutionFailurePersisted
}

// EnsureFullLlmRequest materializes the complete llm.Request AST on demand when
// the inbound phase performed a shallow scalar parse. Any scalar mutations made
// by earlier middlewares (Model, Stream, ReasoningEffort, ReasoningBudget,
// ChannelKeyAffinityID) are preserved in place so all existing pointers remain valid.
func (s *PersistenceState) EnsureFullLlmRequest(ctx context.Context) (*llm.Request, error) {
	if s == nil || !s.llmRequestShallow {
		if s == nil {
			return nil, nil
		}
		return s.LlmRequest, nil
	}
	if s.inboundTransformer == nil || s.RawRequest == nil {
		s.llmRequestShallow = false
		return s.LlmRequest, nil
	}

	fullReq, err := s.inboundTransformer.TransformRequest(ctx, s.RawRequest)
	if err != nil {
		return nil, err
	}

	if s.LlmRequest != nil {
		if s.LlmRequest.Model != "" {
			fullReq.Model = s.LlmRequest.Model
		}
		if s.LlmRequest.Stream != nil {
			fullReq.Stream = s.LlmRequest.Stream
		}
		if s.LlmRequest.StreamOptions != nil {
			fullReq.StreamOptions = s.LlmRequest.StreamOptions
		}
		if s.LlmRequest.ReasoningEffort != "" {
			fullReq.ReasoningEffort = s.LlmRequest.ReasoningEffort
		}
		if s.LlmRequest.ReasoningBudget != nil {
			fullReq.ReasoningBudget = s.LlmRequest.ReasoningBudget
		}
		if s.LlmRequest.ChannelKeyAffinityID != "" {
			fullReq.ChannelKeyAffinityID = s.LlmRequest.ChannelKeyAffinityID
		}
		if len(s.LlmRequest.TransformerMetadata) > 0 {
			if fullReq.TransformerMetadata == nil {
				fullReq.TransformerMetadata = make(map[string]any, len(s.LlmRequest.TransformerMetadata))
			}
			for k, v := range s.LlmRequest.TransformerMetadata {
				fullReq.TransformerMetadata[k] = v
			}
		}
		fullReq.RawRequest = s.RawRequest
		*s.LlmRequest = *fullReq
	} else {
		fullReq.RawRequest = s.RawRequest
		s.LlmRequest = fullReq
	}
	s.llmRequestShallow = false
	return s.LlmRequest, nil
}

// ReleaseRequestPayloads drops multi-MB inbound/outbound request bodies and
// parsed LLM AST payloads once a streaming response has been established and
// all pre-first-event retry windows have closed. Scalar metadata (APIFormat,
// Model, Stream, Headers) is preserved via body-less shallow copies so neither
// callers nor async observation writers holding the original structs are mutated.
func (s *PersistenceState) ReleaseRequestPayloads() {
	if s == nil {
		return
	}
	// When legacy shadow-drain pass-through (e.g. NativeResponsesWebSocket) is
	// active, its background drain goroutine is already reading state fields.
	if s.RawStreamAttempt != nil {
		return
	}
	s.RawRequest = nil
	if s.RawProviderRequest != nil {
		metaCopy := *s.RawProviderRequest
		metaCopy.Body = nil
		metaCopy.JSONBody = nil
		metaCopy.RawRequest = nil
		s.RawProviderRequest = &metaCopy
	}
	if s.ProviderStreamResponse != nil {
		respCopy := *s.ProviderStreamResponse
		respCopy.Request = nil
		respCopy.RawRequest = nil
		s.ProviderStreamResponse = &respCopy
	}
	if s.LlmRequest != nil {
		s.LlmRequest = &llm.Request{
			APIFormat:   s.LlmRequest.APIFormat,
			RequestType: s.LlmRequest.RequestType,
			Model:       s.LlmRequest.Model,
			Stream:      s.LlmRequest.Stream,
		}
	}
	s.ReasoningBoundsExtraBody = nil
	s.ReasoningBoundsTransformerMetadata = nil
	s.ReasoningWireSnapshot = nil
}
