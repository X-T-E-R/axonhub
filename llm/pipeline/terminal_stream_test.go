package pipeline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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
