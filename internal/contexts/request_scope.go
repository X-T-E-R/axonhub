package contexts

import "context"

// NewRequestScope gives a long-lived authenticated transport a fresh mutable
// container for one generation. Request/channel/error state must never leak
// between frames. Identity is reauthenticated by the caller, not copied here.
// Trace/thread references are immutable request associations, not history state.
func NewRequestScope(ctx context.Context) context.Context {
	old := getContainer(ctx)
	fresh := &contextContainer{
		TraceID: old.TraceID, OperationName: old.OperationName,
		Source: old.Source, Thread: old.Thread, Trace: old.Trace,
	}
	return context.WithValue(ctx, containerContextKey, fresh)
}
