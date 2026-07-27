package orchestrator

import (
	"context"
	"errors"
)

// terminalErrorCause keeps a concrete provider/transport error, but replaces a
// generic context.Canceled result with a more specific context cause such as an
// internal response timeout or server shutdown.
func terminalErrorCause(rawErr, requestContextCause error) error {
	if rawErr == nil {
		return requestContextCause
	}

	if errors.Is(rawErr, context.Canceled) &&
		requestContextCause != nil &&
		!errors.Is(requestContextCause, context.Canceled) {
		return requestContextCause
	}

	return rawErr
}
