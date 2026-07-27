package pipeline

import (
	"context"
	"errors"
	"sync"

	"github.com/looplj/axonhub/llm/streams"
)

type terminalErrorReporter struct {
	ctx      context.Context
	onError  func(context.Context, error)
	reported sync.Once
}

type deferredStreamError struct {
	err error
}

func (e *deferredStreamError) Error() string {
	return e.err.Error()
}

func (e *deferredStreamError) Unwrap() error {
	return e.err
}

// IsDeferredStreamError reports whether err was discovered after Process
// returned a stream. Deferred errors cannot participate in request retries, so
// persistence may safely finalize the parent request when this marker is set.
func IsDeferredStreamError(err error) bool {
	var deferredErr *deferredStreamError

	return errors.As(err, &deferredErr)
}

// terminalErrorReportingStream reports errors that only become available while
// consuming a stream. Pipeline setup errors are reported at their synchronous
// call sites; wrappers around both unified and final streams share one reporter
// so errors consumed by an inbound transformer and final transform errors are
// both covered without double-calling middleware.
type terminalErrorReportingStream[T any] struct {
	stream   streams.Stream[T]
	reporter *terminalErrorReporter
}

func (s *terminalErrorReportingStream[T]) Next() bool {
	if s.stream.Next() {
		return true
	}

	s.reporter.report(s.stream.Err())

	return false
}

func (s *terminalErrorReportingStream[T]) Current() T {
	return s.stream.Current()
}

func (s *terminalErrorReportingStream[T]) Err() error {
	err := s.stream.Err()
	s.reporter.report(err)

	return err
}

func (s *terminalErrorReportingStream[T]) Close() error {
	// If iteration already exposed a deferred error, make sure persistence sees
	// it even when the caller closes without separately checking Err.
	s.reporter.report(s.stream.Err())

	return s.stream.Close()
}

func (r *terminalErrorReporter) report(err error) {
	if err == nil {
		return
	}

	requestCause := context.Cause(r.ctx)
	if (errors.Is(err, context.Canceled) && errors.Is(requestCause, context.Canceled)) ||
		(errors.Is(err, context.DeadlineExceeded) && errors.Is(requestCause, context.DeadlineExceeded)) {
		return
	}

	r.reported.Do(func() {
		r.onError(r.ctx, &deferredStreamError{err: err})
	})
}
