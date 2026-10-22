package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/shared"
)

// diagnosticErrorString never serializes provider-controlled messages, codes or
// details when payload logging is disabled. Classification and numeric status
// remain available; business error persistence retains the original error.
func diagnosticErrorString(ctx context.Context, err error) string {
	if err == nil {
		return ""
	}
	if !shared.SensitivePayloadLoggingDisabled(ctx) {
		return err.Error()
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context_deadline_exceeded"
	}
	var httpErr *httpclient.Error
	if errors.As(err, &httpErr) {
		return fmt.Sprintf("upstream_http_error (status %d)", httpErr.StatusCode)
	}
	var responseErr *llm.ResponseError
	if errors.As(err, &responseErr) {
		return fmt.Sprintf("upstream_response_error (status %d)", responseErr.StatusCode)
	}
	return "error"
}

func diagnosticErrorField(ctx context.Context, key string, err error) log.Field {
	if !shared.SensitivePayloadLoggingDisabled(ctx) {
		return log.NamedError(key, err)
	}
	if err == nil {
		return log.Skip
	}
	return log.String(key, diagnosticErrorString(ctx, err))
}

func diagnosticPanicField(ctx context.Context, key string, cause any) log.Field {
	if !shared.SensitivePayloadLoggingDisabled(ctx) {
		return log.Any(key, cause)
	}
	return log.String(key, "panic")
}
