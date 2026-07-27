package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTerminalErrorCause(t *testing.T) {
	internalTimeout := errors.New("internal response timeout")
	providerErr := errors.New("provider connection reset")

	require.ErrorIs(t, terminalErrorCause(context.Canceled, internalTimeout), internalTimeout)
	require.ErrorIs(t, terminalErrorCause(context.Canceled, context.Canceled), context.Canceled)
	require.ErrorIs(t, terminalErrorCause(providerErr, context.Canceled), providerErr)
	require.ErrorIs(t, terminalErrorCause(context.DeadlineExceeded, context.Canceled), context.DeadlineExceeded)
}
