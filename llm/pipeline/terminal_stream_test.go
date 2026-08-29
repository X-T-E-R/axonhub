package pipeline

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

type deferredErrorStream[T any] struct {
	items     []T
	index     int
	current   T
	err       error
	exhausted bool
}

func (s *deferredErrorStream[T]) Next() bool {
	if s.index >= len(s.items) {
		s.exhausted = true
		return false
	}

	s.current = s.items[s.index]
	s.index++

	return true
}

func (s *deferredErrorStream[T]) Current() T {
	return s.current
}

func (s *deferredErrorStream[T]) Err() error {
	if !s.exhausted {
		return nil
	}

	return s.err
}

func (s *deferredErrorStream[T]) Close() error {
	return nil
}

var _ streams.Stream[string] = (*deferredErrorStream[string])(nil)

type closeUnblocksSSEReadWithError struct {
	data        []byte
	err         error
	readStarted chan struct{}
	release     chan struct{}
	startOnce   sync.Once
	closeOnce   sync.Once
}

type terminalClassificationGateError struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newTerminalClassificationGateError() *terminalClassificationGateError {
	return &terminalClassificationGateError{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (e *terminalClassificationGateError) Error() string { return net.ErrClosed.Error() }

func (e *terminalClassificationGateError) As(any) bool {
	e.once.Do(func() { close(e.started) })
	<-e.release

	return false
}

func newCloseUnblocksSSEReadWithError(err error) *closeUnblocksSSEReadWithError {
	return &closeUnblocksSSEReadWithError{
		data:        []byte("data: [DONE]\n\n"),
		err:         err,
		readStarted: make(chan struct{}),
		release:     make(chan struct{}),
	}
}

func (r *closeUnblocksSSEReadWithError) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]

		return n, nil
	}

	r.startOnce.Do(func() { close(r.readStarted) })
	<-r.release

	return 0, r.err
}

func (r *closeUnblocksSSEReadWithError) unblock() {
	r.closeOnce.Do(func() { close(r.release) })
}

func (r *closeUnblocksSSEReadWithError) Close() error {
	r.unblock()

	return nil
}

func TestTerminalErrorReportingStream_ReportsDeferredErrorExactlyOnce(t *testing.T) {
	terminalErr := errors.New("terminal transform integrity failure")
	source := &deferredErrorStream[string]{
		items: []string{"partial"},
		err:   terminalErr,
	}

	var (
		reported     error
		reportedCall int
	)
	reporter := &terminalErrorReporter{
		ctx: context.Background(),
		onError: func(_ context.Context, err error) {
			reported = err
			reportedCall++
		},
	}
	unifiedStream := &terminalErrorReportingStream[string]{
		stream:   source,
		reporter: reporter,
	}
	stream := &terminalErrorReportingStream[string]{
		stream:   unifiedStream,
		reporter: reporter,
	}

	require.True(t, stream.Next())
	require.Equal(t, "partial", stream.Current())
	require.Zero(t, reportedCall)

	require.False(t, stream.Next())
	require.ErrorIs(t, reported, terminalErr)
	require.True(t, IsDeferredStreamError(reported))
	require.Equal(t, 1, reportedCall, "the deferred error must be persisted before Next exposes stream termination")

	require.ErrorIs(t, stream.Err(), terminalErr)
	require.ErrorIs(t, stream.Err(), terminalErr)
	require.NoError(t, stream.Close())
	require.Equal(t, 1, reportedCall)
}

func TestTerminalErrorReportingStream_DoesNotReportClientCancellationOrSuccess(t *testing.T) {
	tests := []struct {
		name      string
		streamErr error
		cancel    func(context.Context) context.Context
	}{
		{name: "successful completion"},
		{
			name:      "client cancellation",
			streamErr: context.Canceled,
			cancel: func(ctx context.Context) context.Context {
				canceledCtx, cancel := context.WithCancel(ctx)
				cancel()
				return canceledCtx
			},
		},
		{
			name:      "client deadline",
			streamErr: context.DeadlineExceeded,
			cancel: func(ctx context.Context) context.Context {
				deadlineCtx, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
				cancel()
				return deadlineCtx
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			if tt.cancel != nil {
				ctx = tt.cancel(ctx)
			}

			source := &deferredErrorStream[string]{err: tt.streamErr}
			reportedCall := 0
			reporter := &terminalErrorReporter{
				ctx: ctx,
				onError: func(context.Context, error) {
					reportedCall++
				},
			}
			stream := &terminalErrorReportingStream[string]{
				stream:   source,
				reporter: reporter,
			}

			require.False(t, stream.Next())
			require.ErrorIs(t, stream.Err(), tt.streamErr)
			require.NoError(t, stream.Close())
			require.Zero(t, reportedCall)
		})
	}
}

func TestTerminalErrorReportingStream_ReportsInternalContextError(t *testing.T) {
	source := &deferredErrorStream[string]{err: context.DeadlineExceeded}
	reportedCall := 0
	reporter := &terminalErrorReporter{
		ctx: context.Background(),
		onError: func(context.Context, error) {
			reportedCall++
		},
	}
	stream := &terminalErrorReportingStream[string]{
		stream:   source,
		reporter: reporter,
	}

	require.False(t, stream.Next())
	require.Equal(t, 1, reportedCall,
		"a stream-owned timeout must not be suppressed when the request context is healthy")
}

func TestTerminalErrorReportingStream_ReportsDistinctFinalTransformError(t *testing.T) {
	finalErr := errors.New("inbound stream transform failed")
	reportedCall := 0
	reporter := &terminalErrorReporter{
		ctx: context.Background(),
		onError: func(_ context.Context, err error) {
			require.ErrorIs(t, err, finalErr)
			reportedCall++
		},
	}

	unifiedStream := &terminalErrorReportingStream[string]{
		stream:   &deferredErrorStream[string]{},
		reporter: reporter,
	}
	require.False(t, unifiedStream.Next())
	require.Zero(t, reportedCall)

	finalStream := &terminalErrorReportingStream[string]{
		stream:   &deferredErrorStream[string]{err: finalErr},
		reporter: reporter,
	}
	require.False(t, finalStream.Next())
	require.Equal(t, 1, reportedCall)
}

func TestTerminalErrorReportingStream_LocalSSEInterruptDoesNotReportRawError(t *testing.T) {
	transportErr := &net.OpError{
		Op:  "read",
		Net: "tcp",
		Err: net.ErrClosed,
	}
	rc := newCloseUnblocksSSEReadWithError(transportErr)
	decoder := httpclient.NewDefaultSSEDecoder(context.Background(), rc)
	interruptible := decoder.(streams.Interruptible)
	reported := make(chan error, 1)
	stream := &terminalErrorReportingStream[*httpclient.StreamEvent]{
		stream: decoder,
		reporter: &terminalErrorReporter{
			ctx: context.Background(),
			onError: func(_ context.Context, err error) {
				reported <- err
			},
		},
	}

	require.True(t, stream.Next())
	require.Equal(t, []byte("[DONE]"), stream.Current().Data,
		"the terminal response evidence must be consumed before cleanup interrupts the next read")

	nextReturned := make(chan bool, 1)
	go func() { nextReturned <- stream.Next() }()

	<-rc.readStarted
	require.NoError(t, interruptible.Interrupt())
	require.False(t, <-nextReturned)
	require.NoError(t, stream.Err())
	select {
	case err := <-reported:
		t.Fatalf("locally interrupted SSE read reached raw-error middleware: %v", err)
	default:
	}
}

func TestTerminalErrorReportingStream_ProviderDisconnectAfterTerminalEventStillReportsRawError(t *testing.T) {
	classificationGate := newTerminalClassificationGateError()
	transportErr := &net.OpError{
		Op:  "read",
		Net: "tcp",
		Err: classificationGate,
	}
	rc := newCloseUnblocksSSEReadWithError(transportErr)
	decoder := httpclient.NewDefaultSSEDecoder(context.Background(), rc)
	reported := make(chan error, 1)
	stream := &terminalErrorReportingStream[*httpclient.StreamEvent]{
		stream: decoder,
		reporter: &terminalErrorReporter{
			ctx: context.Background(),
			onError: func(_ context.Context, err error) {
				reported <- err
			},
		},
	}

	require.True(t, stream.Next())
	require.Equal(t, []byte("[DONE]"), stream.Current().Data)

	nextReturned := make(chan bool, 1)
	go func() { nextReturned <- stream.Next() }()

	<-rc.readStarted
	rc.unblock()
	<-classificationGate.started
	require.NoError(t, decoder.(streams.Interruptible).Interrupt(),
		"Interrupt occurs after the provider Read completed but before decoder classification")
	close(classificationGate.release)
	require.False(t, <-nextReturned)
	require.ErrorIs(t, stream.Err(), transportErr)
	select {
	case err := <-reported:
		require.ErrorIs(t, err, transportErr,
			"an identical read error without decoder Interrupt remains a provider failure")
	case <-time.After(time.Second):
		t.Fatal("genuine provider disconnect did not reach raw-error middleware")
	}
}
