package diagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

// These are process-owned safety ceilings, deliberately lower than the
// protocol maxima. Protocol maxima describe what a client may ask for; they
// must not determine how much work one server invocation is allowed to retain.
const (
	serverMaxRequests       = 50
	serverMaxExecutions     = 200
	serverMaxRelatedRecords = 500
	serverMaxResponseBytes  = 8 << 20
	serverMaxEvidenceBytes  = 2 << 20
	serverPullTimeout       = 20 * time.Second
	serverConcurrentPulls   = 1
)

var diagnosticsPullSlots = make(chan struct{}, serverConcurrentPulls)

type responseBudget struct {
	remaining int
}

func newResponseBudget(maxBytes int) *responseBudget {
	// Reserve space for the fixed envelope, selection metadata, cursors, and
	// issue projections before retaining any record payloads.
	return &responseBudget{remaining: maxBytes - min(maxBytes, 64<<10)}
}

func (b *responseBudget) add(ctx context.Context, value any) error {
	if err := checkPullContext(ctx); err != nil {
		return err
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return serviceError(http.StatusInternalServerError, "SERIALIZATION_FAILED", "diagnostics record could not be serialized")
	}
	if err := checkPullContext(ctx); err != nil {
		return err
	}
	if len(raw) > b.remaining {
		return serviceError(http.StatusRequestEntityTooLarge, "RESPONSE_TOO_LARGE", "use the next cursor or request fewer sections")
	}
	b.remaining -= len(raw)
	return nil
}

func effectiveLimits(client Limits) Limits {
	client = defaultLimits(client)
	client.MaxRequests = min(client.MaxRequests, serverMaxRequests)
	client.MaxExecutions = min(client.MaxExecutions, serverMaxExecutions)
	client.MaxRelatedRecords = min(client.MaxRelatedRecords, serverMaxRelatedRecords)
	client.MaxResponseBytes = min(client.MaxResponseBytes, serverMaxResponseBytes)
	return client
}

func acquirePull(ctx context.Context) (func(), error) {
	if err := contextServiceError(ctx, ctx.Err()); err != nil {
		return nil, err
	}
	select {
	case diagnosticsPullSlots <- struct{}{}:
		return func() { <-diagnosticsPullSlots }, nil
	default:
		return nil, transientServiceError(http.StatusTooManyRequests, "DIAGNOSTICS_BUSY", "another diagnostics pull is already running")
	}
}

func contextServiceError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return transientServiceError(http.StatusGatewayTimeout, "DIAGNOSTICS_DEADLINE_EXCEEDED", "diagnostics pull exceeded its deadline")
	case errors.Is(ctx.Err(), context.Canceled), errors.Is(err, context.Canceled):
		// 499 is intentionally used internally for a disconnected caller. The
		// transport normally has nobody left to receive this response.
		return serviceError(499, "DIAGNOSTICS_CANCELED", "diagnostics pull was canceled by the caller")
	default:
		return nil
	}
}

func transientServiceError(status int, code, message string) error {
	return &ServiceError{Status: status, Code: code, Message: message, Retryable: true}
}

func queryError(ctx context.Context, err error) error {
	if cancellation := contextServiceError(ctx, err); cancellation != nil {
		return cancellation
	}
	var serviceErr *ServiceError
	if errors.As(err, &serviceErr) {
		return serviceErr
	}
	return coreDBError()
}

func checkPullContext(ctx context.Context) error {
	return contextServiceError(ctx, ctx.Err())
}

func ensureEvidenceBudget(raw []byte) error {
	if len(raw) > serverMaxEvidenceBytes {
		return serviceError(http.StatusRequestEntityTooLarge, "EVIDENCE_BUDGET_EXCEEDED", "one evidence item exceeds the server diagnostics budget")
	}
	return nil
}

func ensureChunkBudget(chunks []json.RawMessage) error {
	total := 2 // surrounding JSON array
	for _, chunk := range chunks {
		total += len(chunk) + 1
		if total > serverMaxEvidenceBytes {
			return serviceError(http.StatusRequestEntityTooLarge, "EVIDENCE_BUDGET_EXCEEDED", "one evidence item exceeds the server diagnostics budget")
		}
	}
	return nil
}
