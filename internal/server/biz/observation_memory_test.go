package biz

import (
	"bytes"
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

// No worker is started: this models persistence blocked indefinitely without
// making the test depend on database scheduling or process RSS.
func memoryObservationScope(t testing.TB, enabled bool) (*RequestService, *ForwardingObservationWriter, context.Context) {
	t.Helper()
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 128, MaxBytesMiB: 64})
	w.started = true
	w.policy.Store(&StoragePolicy{StoreRequestBody: enabled, StoreExecutionRequestBody: lo.ToPtr(enabled), StoreResponseBody: enabled, StoreChunks: enabled})
	return &RequestService{SystemService: &SystemService{}}, w, w.WithScope(context.Background())
}

func TestObservationOmissionSurvivesWorkerPolicyChange(t *testing.T) {
	legacy, client, base, project := setupRequestExecutionStorageTest(t)
	defer client.Close()
	require.NoError(t, legacy.SystemService.SetStoragePolicy(base, &StoragePolicy{}))
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 64, AttemptTimeout: 5 * time.Second})
	svc := NewRequestServiceWithObservationWriter(client, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, w)
	require.NoError(t, w.Start(base))
	ctx := w.WithScope(contexts.WithProjectID(base, project.ID))
	entered, release := make(chan struct{}), make(chan struct{})
	require.NoError(t, observationFromContext(ctx).submit(ctx, 0, func(worker context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-worker.Done():
			return worker.Err()
		}
	}))
	<-entered
	t.Cleanup(func() {
		EndForwardingObservation(ctx)
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = w.Stop(stop)
	})
	requestBody := []byte(`{"model":"memory-contract"}`)
	responseBody := []byte(`{"output":"preserved"}`)
	parent, err := svc.CreateRequest(ctx, &llm.Request{Model: "memory-contract"}, &httpclient.Request{Body: requestBody}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	first := createStorageTestChannel(t, base, client, &objects.ChannelSettings{StoreExecutionRequestBody: lo.ToPtr(false), StoreExecutionResponseBody: lo.ToPtr(false)})
	require.NoError(t, client.Channel.UpdateOneID(first.ID).SetName("first-attempt").Exec(base))
	second := createStorageTestChannel(t, base, client, &objects.ChannelSettings{StoreExecutionRequestBody: lo.ToPtr(true), StoreExecutionResponseBody: lo.ToPtr(true)})
	one, err := svc.CreateRequestExecution(ctx, first, "memory-contract", parent, httpclient.Request{Body: requestBody}, llm.APIFormatOpenAIChatCompletion, false)
	require.NoError(t, err)
	require.NoError(t, svc.UpdateRequestExecutionFailedWithMetrics(ctx, one.ID, "attempt failed", &ExecutionErrorInfo{ResponseBody: responseBody}, nil))
	two, err := svc.CreateRequestExecution(ctx, second, "memory-contract", parent, httpclient.Request{Body: requestBody}, llm.APIFormatOpenAIChatCompletion, false)
	require.NoError(t, err)
	require.NoError(t, svc.UpdateRequestExecutionCompleted(ctx, two.ID, "response", responseBody, nil))
	require.NoError(t, svc.UpdateRequestCompleted(ctx, parent.ID, "response", responseBody, nil))
	// A later policy enables storage, but no captured-off payload may turn into
	// an empty "stored" record. The second attempt's explicit override is saved.
	require.NoError(t, svc.SystemService.SetStoragePolicy(base, &StoragePolicy{StoreRequestBody: true, StoreResponseBody: true, StoreChunks: true}))
	close(release)
	EndForwardingObservation(ctx)
	drain, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, w.Wait(drain))
	stored := client.Request.Query().OnlyX(base)
	require.Equal(t, "omitted", stored.EvidenceDisposition.RequestBody.Outcome)
	require.Equal(t, "omitted", stored.EvidenceDisposition.ResponseBody.Outcome)
	executions := client.RequestExecution.Query().Order(ent.Asc(requestexecution.FieldID)).AllX(base)
	require.Len(t, executions, 2)
	require.Equal(t, requestexecution.StatusFailed, executions[0].Status)
	require.Equal(t, "omitted", executions[0].EvidenceDisposition.RequestBody.Outcome)
	require.Equal(t, "omitted", executions[0].EvidenceDisposition.ResponseBody.Outcome)
	require.Equal(t, requestexecution.StatusCompleted, executions[1].Status)
	got, err := svc.LoadRequestExecutionRequestBody(base, executions[1])
	require.NoError(t, err)
	require.JSONEq(t, string(requestBody), string(got))
	require.JSONEq(t, string(responseBody), string(executions[1].ResponseBody))
}

func TestObservationLargeRequestMemory(t *testing.T) {
	body := append([]byte(`{"text":"`), bytes.Repeat([]byte("x"), 30<<20)...)
	body = append(body, []byte(`"}`)...)
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			svc, writer, ctx := memoryObservationScope(t, enabled)
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			var wg sync.WaitGroup
			results := make(chan *ent.Request, 3)
			errors := make(chan error, 3)
			for range 3 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() {
						if cause := recover(); cause != nil {
							errors <- fmt.Errorf("capture panic: %v", cause)
						}
					}()
					req, err := svc.CreateRequest(ctx, &llm.Request{Model: "memory-contract"}, &httpclient.Request{Body: body, JSONBody: body}, llm.APIFormatOpenAIChatCompletion)
					if err != nil {
						errors <- err
						return
					}
					results <- req
				}()
			}
			wg.Wait()
			close(results)
			close(errors)
			for err := range errors {
				require.NoError(t, err)
			}
			var synthetics []*ent.Request
			for req := range results {
				require.Equal(t, objects.JSONRawMessage(`{}`), req.RequestBody, "synthetic entities must not hold queue payloads")
				synthetics = append(synthetics, req)
			}
			require.Len(t, synthetics, 3)
			runtime.GC()
			runtime.ReadMemStats(&after)
			allocated := after.TotalAlloc - before.TotalAlloc
			limit := uint64(1 << 20)
			if enabled {
				limit = 63 << 20 // Two admitted canonical bodies; the third is metadata-only.
			}
			require.Less(t, allocated, limit)
			require.LessOrEqual(t, writer.bytes, int64(64<<20))
			retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
			require.Less(t, retained, int64(limit))
			t.Logf("enabled=%v: three concurrent 30MiB requests, allocated=%d, live heap delta=%d, charged=%d", enabled, allocated, retained, writer.bytes)
			runtime.KeepAlive(synthetics)
			runtime.KeepAlive(writer)
		})
	}
}

func TestObservationAdmissionFreezesOnlyAcceptedPayload(t *testing.T) {
	_, w, ctx := memoryObservationScope(t, true)
	scope := observationFromContext(ctx)
	called := false
	err := scope.submit(ctx, 65<<20, func(context.Context) error { return nil }, func() { called = true })
	require.Error(t, err)
	require.False(t, called)
	require.NoError(t, scope.submit(ctx, 32, func(context.Context) error { return nil }, func() { called = true }))
	require.True(t, called)
	require.LessOrEqual(t, w.bytes, int64(64<<20))
}

func TestObservationCaptureWithoutPolicySnapshotDoesNotReadDatabase(t *testing.T) {
	svc, writer, ctx := memoryObservationScope(t, true)
	writer.policy.Store(nil)
	// SystemService deliberately has no database/cache. The documented default
	// policy must be available without performing a cold synchronous lookup.
	_, err := svc.CreateRequest(ctx, &llm.Request{Model: "cold-policy"}, &httpclient.Request{Body: []byte(`{}`)}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	require.Len(t, writer.jobs, 1)
	require.Equal(t, defaultStoragePolicy.StoreResponseBody, svc.NewObservationStreamBuffer(ctx, nil, false).BodyEnabled)
}

func TestObservationResponseValueOwnership(t *testing.T) {
	for _, body := range []any{objects.JSONRawMessage(`{"ok":true}`), []byte(`{"ok":true}`)} {
		encoded, err := observationResponseValue(body, true)
		require.NoError(t, err)
		frozen := freezeObservationValue(body, encoded)
		encoded[0] = '['
		require.JSONEq(t, `{"ok":true}`, string(frozen))
	}
	_, err := observationResponseValue(objects.JSONRawMessage(`invalid`), true)
	require.Error(t, err)
	omitted, err := observationResponseValue(objects.JSONRawMessage(`invalid`), false)
	require.NoError(t, err)
	require.Nil(t, omitted, "disabled evidence must not serialize or validate payloads")
}

func TestObservationStreamBufferShutdownAccounting(t *testing.T) {
	svc, writer, ctx := memoryObservationScope(t, true)
	buffer := svc.NewObservationStreamBuffer(ctx, nil, false)
	buffer.Append(&httpclient.StreamEvent{Data: []byte(`{"delta":"text"}`)})
	require.Positive(t, buffer.bytes)
	writer.abandonPending()
	require.Equal(t, buffer.bytes, writer.bytes)
	EndForwardingObservation(ctx)
	require.Zero(t, writer.bytes)
	buffer.Close()
	require.Zero(t, writer.bytes, "scope and stream cleanup must release exactly once")
}

func BenchmarkObservationLargeRequestCapture(b *testing.B) {
	body := bytes.Repeat([]byte("x"), 30<<20)
	raw := &httpclient.Request{Body: body}
	for _, mode := range []string{"before", "disabled", "enabled"} {
		b.Run(mode, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if mode == "before" {
					snapshot := cloneObservationHTTPClientRequest(raw)
					info := observationBodyInfo(snapshot)
					syntheticBody := bytes.Clone(observationRequestBodyBytes(snapshot))
					runtime.KeepAlive(info)
					runtime.KeepAlive(snapshot)
					runtime.KeepAlive(syntheticBody)
					continue
				}
				svc, writer, ctx := memoryObservationScope(b, mode == "enabled")
				_, err := svc.CreateRequest(ctx, &llm.Request{Model: "memory-contract"}, raw, llm.APIFormatOpenAIChatCompletion)
				if err != nil {
					b.Fatal(err)
				}
				runtime.KeepAlive(writer)
			}
		})
	}
}

func TestObservationStreamBufferSharedBudget(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			svc, writer, ctx := memoryObservationScope(t, enabled)
			baseline := writer.bytes
			inbound := svc.NewObservationStreamBuffer(ctx, nil, false)
			outbound := svc.NewObservationStreamBuffer(ctx, nil, true)
			event := &httpclient.StreamEvent{Data: bytes.Repeat([]byte("x"), 1024)}
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			for range 10000 {
				inbound.Append(event)
				outbound.Append(event)
				if writer.bytes > 64<<20 {
					t.Fatalf("live capture exceeded global budget: %d", writer.bytes)
				}
			}
			runtime.ReadMemStats(&after)
			if enabled {
				require.True(t, inbound.Unavailable)
				require.True(t, outbound.Unavailable)
			} else {
				require.False(t, inbound.Unavailable)
				require.False(t, outbound.Unavailable)
				require.Less(t, after.TotalAlloc-before.TotalAlloc, uint64(1<<20))
			}
			require.Empty(t, inbound.chunks)
			require.Empty(t, outbound.chunks)
			inbound.Close()
			outbound.Close()
			require.Equal(t, baseline, writer.bytes)
			t.Logf("enabled=%v: 20000 events, allocated=%d, retained chunks=0", enabled, after.TotalAlloc-before.TotalAlloc)
		})
	}
}
