package orchestrator

import (
	"context"
	"sync"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

// PersistenceState holds shared state with channel management and retry capabilities.
// TODO: move the dependencies out of the state to make it a real state.
type PersistenceState struct {
	APIKey *ent.APIKey

	RequestService      *biz.RequestService
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
	RawRequest    *httpclient.Request
	LlmRequest    *llm.Request

	// OriginalRequestStream stores the client's original stream intent before any
	// candidate-specific forcing to provider-side streaming happens.
	OriginalRequestStream *bool

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

	// RawProviderResponse stores the raw provider response for non-stream response pass-through.
	RawProviderResponse *httpclient.Response

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

	// PassThroughApplied records whether the inbound request body was substituted during pass-through.
	PassThroughApplied bool

	deferredFailureMu                 sync.Mutex
	deferredStreamError               error
	deferredRequestFailurePersisted   bool
	deferredExecutionFailurePersisted bool
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
