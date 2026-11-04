package biz

import "context"

// ExecutedAccountingError is not an upstream failure: the provider has already
// returned usage, and only accounting publication/recovery may be retried.
type ExecutedAccountingError struct{ Cause error }

func (e *ExecutedAccountingError) Error() string {
	return "accounting failed after upstream execution: " + e.Cause.Error()
}
func (e *ExecutedAccountingError) Unwrap() error { return e.Cause }

func ForwardingAccountingEnabled(ctx context.Context) bool {
	scope := observationFromContext(ctx)
	return scope != nil && scope.writer.node.Enabled
}
