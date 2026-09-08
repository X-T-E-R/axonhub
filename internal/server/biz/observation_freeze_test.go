package biz

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm/httpclient"
)

func TestObservationInFlightFreezeDoesNotHoldWriterLock(t *testing.T) {
	for _, outcome := range []string{"publish", "end", "stop", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 64, MaxBytesMiB: 64})
			w.policy.Store(&StoragePolicy{StoreResponseBody: true})
			require.NoError(t, w.Start(t.Context()))
			ctx := w.WithScope(t.Context())
			scope := observationFromContext(ctx)
			t.Cleanup(func() {
				EndForwardingObservation(ctx)
				stop, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = w.Stop(stop)
			})
			require.NoError(t, scope.submitCore(ctx, -1, 0, func(context.Context) error { return nil }))
			entered, release, ran := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			finished := make(chan error, 1)
			frozen := false
			go func() {
				defer func() {
					if cause := recover(); cause != nil {
						finished <- fmt.Errorf("freeze panic: %v", cause)
					}
				}()
				finished <- scope.submitCore(ctx, -2, 8<<20, func(context.Context) error {
					if !frozen {
						return errors.New("partially frozen job published")
					}
					close(ran)
					return nil
				}, func() {
					close(entered)
					<-release
					if outcome == "panic" {
						panic("injected capture failure")
					}
					frozen = true
				})
			}()
			<-entered
			progress := make(chan error, 1)
			go func() {
				defer func() {
					if cause := recover(); cause != nil {
						progress <- fmt.Errorf("independent progress panic: %v", cause)
					}
				}()
				otherCtx := w.WithScope(t.Context())
				defer EndForwardingObservation(otherCtx)
				if err := observationFromContext(otherCtx).submit(otherCtx, 32, func(context.Context) error { return nil }); err != nil {
					progress <- err
					return
				}
				if err := scope.submitTerminal(ctx, -1, 0, func(context.Context) error { return nil }); err != nil {
					progress <- err
					return
				}
				if err := scope.submitUsage(ctx, 0, func(context.Context) error { return nil }); err != nil {
					progress <- err
					return
				}
				svc := &RequestService{SystemService: &SystemService{}}
				buffer := svc.NewObservationStreamBuffer(otherCtx, nil, false)
				if len(buffer.Append(&httpclient.StreamEvent{Data: []byte(`{"text":"forwarded"}`)})) != 1 {
					progress <- errors.New("independent stream append rejected")
					return
				}
				buffer.Close()
				if err := observationFromContext(otherCtx).submit(otherCtx, 60<<20, func(context.Context) error { return nil }); err == nil {
					progress <- errors.New("in-flight bytes were reused for another capture")
					return
				}
				progress <- nil
			}()
			select {
			case err := <-progress:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("optional freeze held up independent scope/terminal/usage/stream admission")
			}
			w.mu.Lock()
			charged := w.bytes
			w.mu.Unlock()
			require.GreaterOrEqual(t, charged, int64(8<<20), "in-flight freeze must remain charged")
			select {
			case <-ran:
				t.Fatal("half-frozen job was published")
			default:
			}
			switch outcome {
			case "end":
				EndForwardingObservation(ctx)
			case "stop":
				stop, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
				require.ErrorIs(t, w.Stop(stop), context.DeadlineExceeded)
				cancel()
			}
			w.mu.Lock()
			charged = w.bytes
			w.mu.Unlock()
			require.GreaterOrEqual(t, charged, int64(8<<20), "cleanup must not reclaim an active freeze")
			unblock()
			select {
			case err := <-finished:
				if outcome == "publish" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
			case <-time.After(time.Second):
				t.Fatal("freeze owner did not finish")
			}
			EndForwardingObservation(ctx)
			drain, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			require.NoError(t, w.Wait(drain))
			w.mu.Lock()
			require.Zero(t, w.bytes)
			require.Zero(t, w.items)
			w.mu.Unlock()
			if outcome == "publish" {
				select {
				case <-ran:
				default:
					t.Fatal("complete frozen job did not run")
				}
			} else {
				select {
				case <-ran:
					t.Fatal("abandoned capture was published")
				default:
				}
			}
		})
	}
}

func TestObservationStreamAppendConcurrentCleanup(t *testing.T) {
	svc, w, ctx := memoryObservationScope(t, true)
	buffer := svc.NewObservationStreamBuffer(ctx, nil, false)
	event := &httpclient.StreamEvent{Data: make([]byte, 4096)}
	require.Len(t, buffer.Append(event), 1)
	started, finished := make(chan struct{}), make(chan error, 1)
	go func() {
		defer func() {
			if cause := recover(); cause != nil {
				finished <- fmt.Errorf("append panic: %v", cause)
			}
		}()
		close(started)
		for range 1000 {
			buffer.Append(event)
		}
		finished <- nil
	}()
	<-started
	EndForwardingObservation(ctx)
	select {
	case err := <-finished:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("append did not finish after concurrent cleanup")
	}
	require.Empty(t, buffer.chunks)
	require.Zero(t, w.bytes)
	require.Zero(t, w.items)
	buffer.Close()
	require.Zero(t, w.bytes)
}
