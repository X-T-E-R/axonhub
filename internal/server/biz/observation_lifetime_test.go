package biz

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestObservationRetainedOwners(t *testing.T) {
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 5, MaxBytesMiB: 1})
	require.NoError(t, w.Start(t.Context()))
	ctx := w.WithScope(t.Context())
	scope := observationFromContext(ctx)
	first, last := RetainForwardingObservation(ctx), RetainForwardingObservation(ctx)
	var cleanup atomic.Int32
	scope.liveCleanup = append(scope.liveCleanup, func() { cleanup.Add(1) })
	EndForwardingObservation(ctx)
	EndForwardingObservation(ctx)
	require.False(t, scope.ended)
	require.Zero(t, cleanup.Load())
	// Retention holds the original reservation, not extra unbounded capacity.
	other := w.WithScope(t.Context())
	require.Zero(t, observationFromContext(other).reservedCore)
	EndForwardingObservation(other)
	first()
	first()
	var calls atomic.Int32
	for _, id := range []int{-1, -2} {
		require.NoError(t, scope.submitCore(ctx, id, 0, func(context.Context) error { calls.Add(1); return nil }))
		require.NoError(t, scope.submitTerminal(ctx, id, 0, func(context.Context) error { calls.Add(1); return nil }))
	}
	require.NoError(t, scope.submitUsage(ctx, 0, func(context.Context) error { calls.Add(1); return nil }))
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("scope release panic: %v", p)
				}
			}()
			last()
			EndForwardingObservation(ctx)
		})
	}
	wg.Wait()
	require.True(t, scope.ended)
	require.Equal(t, int32(1), cleanup.Load())
	require.ErrorIs(t, scope.submitCore(ctx, -3, 0, func(context.Context) error { return nil }), errObservationQueueUnavailable)
	RetainForwardingObservation(ctx)() // An ended scope cannot be reopened.
	drain, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, w.Wait(drain))
	require.Equal(t, int32(5), calls.Load())
	require.Zero(t, w.items)
	require.Zero(t, w.bytes)
	require.Empty(t, w.scopes)
	require.NoError(t, w.Stop(drain))
}

func TestObservationRetainedShutdown(t *testing.T) {
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 5})
	require.NoError(t, w.Start(t.Context()))
	ctx := w.WithScope(t.Context())
	scope := observationFromContext(ctx)
	release := RetainForwardingObservation(ctx)
	var cleanup atomic.Int32
	scope.liveCleanup = append(scope.liveCleanup, func() { cleanup.Add(1) })
	EndForwardingObservation(ctx)
	stop, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, w.Stop(stop), context.DeadlineExceeded)
	<-w.done
	require.True(t, scope.ended)
	require.Equal(t, int32(1), cleanup.Load())
	release()
	release()
	EndForwardingObservation(ctx)
	require.Zero(t, w.items)
	require.Zero(t, w.bytes)
	require.Empty(t, w.scopes)
	require.NoError(t, w.Start(t.Context())) // Start is idempotent, not a restart.
	require.True(t, w.stopping)
	// A new writer has no old reservations or scope state.
	fresh := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 5})
	require.NoError(t, fresh.Start(t.Context()))
	freshCtx := fresh.WithScope(t.Context())
	require.Equal(t, 2, observationFromContext(freshCtx).reservedCore)
	EndForwardingObservation(freshCtx)
	require.NoError(t, fresh.Stop(context.Background()))
}
