package orchestrator

import (
	"context"
	"sync"

	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
)

// StreamLivenessAttempt describes a provider stream after its response headers
// have been accepted. It intentionally excludes request and response bodies.
type StreamLivenessAttempt struct {
	ChannelID   int
	ChannelName string
	KeepAlive   *objects.ChannelSSEKeepAlive
	// Interrupt targets only the concurrency-safe raw decoder. It must not
	// close terminal/persistence wrappers while their reader owns Next/Current.
	Interrupt func() error
}

// StreamLivenessObserver receives transport-only milestones. Implementations
// must not turn these callbacks into semantic stream events or retry signals.
type StreamLivenessObserver interface {
	OnUpstreamResponseHeaders(StreamLivenessAttempt)
	OnRawSSE()
	OnTransformableEvent()
	OnUpstreamError(error)
}

type streamLivenessMiddleware struct {
	*pipeline.DummyMiddleware
	state    *PersistenceState
	observer StreamLivenessObserver
}

type livenessObservedStream[T any] struct {
	stream    streams.Stream[T]
	onCurrent func()
	closeOnce sync.Once
	closeErr  error
}

func (s *livenessObservedStream[T]) Next() bool { return s.stream.Next() }
func (s *livenessObservedStream[T]) Current() T {
	s.onCurrent()
	return s.stream.Current()
}
func (s *livenessObservedStream[T]) Err() error { return s.stream.Err() }
func (s *livenessObservedStream[T]) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.stream.Close() })
	return s.closeErr
}

func newStreamLivenessMiddleware(state *PersistenceState, observer StreamLivenessObserver) pipeline.Middleware {
	return &streamLivenessMiddleware{
		DummyMiddleware: &pipeline.DummyMiddleware{},
		state:           state,
		observer:        observer,
	}
}

func (m *streamLivenessMiddleware) Name() string {
	return "stream_liveness"
}

func (m *streamLivenessMiddleware) downstreamWantsStream() bool {
	return m.state != nil && m.state.OriginalRequestStream != nil && *m.state.OriginalRequestStream
}

func (m *streamLivenessMiddleware) OnOutboundRawStream(
	_ context.Context,
	stream streams.Stream[*httpclient.StreamEvent],
) (streams.Stream[*httpclient.StreamEvent], error) {
	if !m.downstreamWantsStream() {
		return stream, nil
	}

	attempt := StreamLivenessAttempt{}
	if interruptible, ok := stream.(streams.Interruptible); ok {
		attempt.Interrupt = interruptible.Interrupt
	}
	if m.state != nil && m.state.CurrentCandidate != nil && m.state.CurrentCandidate.Channel != nil {
		channel := m.state.CurrentCandidate.Channel
		attempt.ChannelID = channel.ID
		attempt.ChannelName = channel.Name
		if channel.Settings != nil {
			attempt.KeepAlive = channel.Settings.SSEKeepAlive
		}
	}
	m.observer.OnUpstreamResponseHeaders(attempt)

	return &livenessObservedStream[*httpclient.StreamEvent]{
		stream:    stream,
		onCurrent: m.observer.OnRawSSE,
	}, nil
}

func (m *streamLivenessMiddleware) OnOutboundLlmStream(
	_ context.Context,
	stream streams.Stream[*llm.Response],
) (streams.Stream[*llm.Response], error) {
	if !m.downstreamWantsStream() {
		return stream, nil
	}

	return &livenessObservedStream[*llm.Response]{
		stream:    stream,
		onCurrent: m.observer.OnTransformableEvent,
	}, nil
}

func (m *streamLivenessMiddleware) OnOutboundRawError(_ context.Context, err error) {
	if !m.downstreamWantsStream() {
		return
	}
	m.observer.OnUpstreamError(err)
}
