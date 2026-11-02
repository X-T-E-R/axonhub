package orchestrator

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
)

func TestPassThroughChannelStreamPreservesCancellationCause(t *testing.T) {
	providerErr := errors.New("provider stream failure")
	for _, tc := range []struct {
		name     string
		cause    error
		upstream error
		want     error
	}{
		{name: "caller cancellation", cause: context.Canceled, want: context.Canceled},
		{name: "generation deadline", cause: context.DeadlineExceeded, want: context.DeadlineExceeded},
		{name: "provider failure takes precedence", cause: context.Canceled, upstream: providerErr, want: providerErr},
		{name: "clean EOF"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			ch := make(chan *httpclient.StreamEvent)
			close(ch)
			if tc.cause != nil {
				cancel(tc.cause)
			}
			stream := &passThroughChannelStream{ctx: ctx, ch: ch, errRef: &tc.upstream}
			require.False(t, stream.Next())
			require.ErrorIs(t, stream.Err(), tc.want)
		})
	}
}
