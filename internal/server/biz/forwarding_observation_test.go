package biz

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/hook"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestForwardingObservationBoundedReservationAndCancellation(t *testing.T) {
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 6, MaxBytesMiB: 1, AttemptTimeout: time.Second})
	require.NoError(t, w.Start(t.Context()))
	ctx, cancel := context.WithCancel(w.WithScope(t.Context()))
	scope := observationFromContext(ctx)
	entered, release := make(chan struct{}), make(chan struct{})
	require.NoError(t, scope.submit(ctx, 0, func(workerCtx context.Context) error {
		close(entered)
		select {
		case <-release:
			return workerCtx.Err()
		case <-workerCtx.Done():
			return workerCtx.Err()
		}
	}))
	<-entered
	cancel()
	require.Error(t, scope.submit(ctx, 0, func(context.Context) error { t.Error("overload callback ran"); return nil }))
	require.Error(t, scope.submit(ctx, 2<<20, func(context.Context) error { t.Error("oversize callback ran"); return nil }))
	// Reserved parent/execution and terminal slots remain usable while the
	// ordinary queue is full; none waits for the running persistence operation.
	var completed atomic.Int32
	for _, id := range []int{-1, -2} {
		require.NoError(t, scope.submitCore(ctx, id, 0, func(context.Context) error { completed.Add(1); return nil }))
		require.NoError(t, scope.submitTerminal(ctx, id, 0, func(context.Context) error { completed.Add(1); return nil }))
	}
	require.NoError(t, scope.submitUsage(ctx, 0, func(context.Context) error { completed.Add(1); return nil }))
	w.mu.Lock()
	require.LessOrEqual(t, w.items, w.config.MaxItems)
	require.LessOrEqual(t, w.bytes, int64(1<<20))
	w.mu.Unlock()
	EndForwardingObservation(ctx)
	close(release)
	drainCtx, drainCancel := context.WithTimeout(context.Background(), time.Second)
	defer drainCancel()
	require.NoError(t, w.Wait(drainCtx), "caller cancellation must not cancel the writer")
	require.Equal(t, int32(5), completed.Load())
	require.NoError(t, w.Stop(drainCtx))
	w.mu.Lock()
	defer w.mu.Unlock()
	require.Zero(t, w.items)
	require.Zero(t, w.bytes)
}

func TestForwardingObservationDoesNotRetainPipelineContext(t *testing.T) {
	type pipelineKey struct{}
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxBytesMiB: 1})
	require.NoError(t, w.Start(t.Context()))
	ctx := context.WithValue(contexts.WithAPIKey(t.Context(), &ent.APIKey{ID: 17}), pipelineKey{}, make([]byte, 2<<20))
	ctx = w.WithScope(ctx)
	require.NoError(t, observationFromContext(ctx).submit(ctx, 0, func(worker context.Context) error {
		if worker.Value(pipelineKey{}) != nil {
			return errors.New("queued context retained unaccounted pipeline buffers")
		}
		key, ok := contexts.GetAPIKey(worker)
		if !ok || key.ID != 17 {
			return errors.New("detached context lost the accounting identity")
		}
		return nil
	}))
	EndForwardingObservation(ctx)
	drain, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, w.Stop(drain))
}

type observationReplicaTrap struct {
	dialect.Driver

	reads atomic.Int32
}

func (d *observationReplicaTrap) PrimaryDriver() dialect.Driver { return d.Driver }
func (d *observationReplicaTrap) Query(context.Context, string, any, any) error {
	d.reads.Add(1)
	return errors.New("read replica is deliberately stale")
}

func TestForwardingObservationUsesPrimaryForAccountingAndNonceReads(t *testing.T) {
	legacy, client, ctx, project := setupRequestExecutionStorageTest(t)
	defer client.Close()
	trap := &observationReplicaTrap{Driver: client.Driver()}
	routed := ent.NewClient(ent.Driver(trap))
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{})
	svc := NewRequestServiceWithObservationWriter(routed, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, w)
	require.NoError(t, w.Start(context.Background()))
	scoped := w.WithScope(contexts.WithProjectID(ctx, project.ID))
	_, err := svc.CreateRequest(scoped, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: []byte(`{}`)}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	EndForwardingObservation(scoped)
	drain, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, w.Wait(drain))
	pending := &observationPendingUsage{apiKeyID: 7, createdAt: time.Now().UTC()}
	w.mu.Lock()
	w.pendingRecords[pending] = struct{}{}
	w.mu.Unlock()
	quota := NewQuotaServiceWithObservationWriter(routed, legacy.SystemService, w)
	result, err := quota.CheckAPIKeyQuota(ctx, 7, &objects.APIKeyQuota{Requests: lo.ToPtr(int64(2)), Period: objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime}})
	require.NoError(t, err)
	require.True(t, result.Allowed)
	require.Zero(t, trap.reads.Load())
	w.mu.Lock()
	delete(w.pendingRecords, pending)
	w.mu.Unlock()
	require.NoError(t, w.Stop(drain))
}

func TestForwardingObservationQuotaFreshnessRecovers(t *testing.T) {
	legacy, client, ctx, _ := setupRequestExecutionStorageTest(t)
	defer client.Close()
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 8, AttemptTimeout: time.Second})
	require.NoError(t, w.Start(ctx))
	svc := NewQuotaServiceWithObservationWriter(client, legacy.SystemService, w)
	ctx = w.WithScope(contexts.WithAPIKey(ctx, &ent.APIKey{ID: 42}))
	scope := observationFromContext(ctx)
	entered, release := make(chan struct{}), make(chan struct{})
	require.NoError(t, scope.submitUsage(ctx, 0, func(workerCtx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-workerCtx.Done():
			return workerCtx.Err()
		}
	}))
	<-entered
	quota := &objects.APIKeyQuota{Requests: lo.ToPtr(int64(5)), Period: objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime}}
	checkCtx := context.WithValue(ctx, observationContextKey{}, (*observationScope)(nil))
	result, err := svc.CheckAPIKeyQuota(checkCtx, 42, quota)
	require.NoError(t, err)
	require.True(t, result.Allowed, "pending usage consumes quota without rejecting available balance")
	quota.Requests = lo.ToPtr(int64(1))
	result, err = svc.CheckAPIKeyQuota(checkCtx, 42, quota)
	require.NoError(t, err)
	require.False(t, result.Allowed, "pending requests enforce the actual threshold")
	saturated := w.WithScope(checkCtx)
	_, err = svc.CheckAPIKeyQuota(saturated, 42, quota)
	require.ErrorContains(t, err, "quota accounting queue is full")
	EndForwardingObservation(saturated)
	result, err = svc.CheckAPIKeyQuota(checkCtx, 42, nil)
	require.NoError(t, err)
	require.True(t, result.Allowed, "keys without a quota are not gated by observations")
	EndForwardingObservation(ctx)
	close(release)
	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, w.Wait(drainCtx))
	// Completed request scopes have consumed their reservation; use a fresh
	// request just as the real middleware does before evaluating quota.
	fresh := w.WithScope(contexts.WithAPIKey(checkCtx, &ent.APIKey{ID: 42}))
	result, err = svc.CheckAPIKeyQuota(fresh, 42, quota)
	require.NoError(t, err)
	require.True(t, result.Allowed)
	EndForwardingObservation(fresh)
	require.NoError(t, w.Stop(drainCtx))
}

type observationBlockedObjectStore struct {
	boundedObjectStoreFake

	entered chan struct{}
	release chan struct{}
	once    atomic.Bool
	failure error
}

func (s *observationBlockedObjectStore) PutObject(ctx context.Context, _ string, _ []byte) error {
	if s.once.CompareAndSwap(false, true) {
		close(s.entered)
	}
	select {
	case <-s.release:
		return s.failure
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestForwardingObservationExternalSaveBarrier(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(fmt.Sprintf("payload_full=%v", full), func(t *testing.T) {
			legacy, client, ctx, project := setupRequestExecutionStorageTest(t)
			defer client.Close()
			ds := client.DataStorage.Create().SetName("observation-external").SetDescription("isolated test storage").SetType(datastorage.TypeS3).
				SetSettings(&objects.DataStorageSettings{S3: &objects.S3{}}).SetStatus(datastorage.StatusActive).SaveX(ctx)
			store := &observationBlockedObjectStore{entered: make(chan struct{}), release: make(chan struct{})}
			legacy.DataStorageService.fsCacheMu.Lock()
			legacy.DataStorageService.objectStoreCache[ds.ID] = store
			legacy.DataStorageService.fsCacheMu.Unlock()
			require.NoError(t, legacy.SystemService.SetDefaultDataStorageID(ctx, ds.ID))
			w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{AttemptTimeout: 10 * time.Second})
			svc := NewRequestServiceWithObservationWriter(client, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, w)
			require.NoError(t, w.Start(ctx))
			key := client.APIKey.Create().SetProjectID(project.ID).SetName("isolated-quota").SetKey("isolated-test-only").SaveX(ctx)
			baseCtx := contexts.WithAPIKey(contexts.WithProjectID(ctx, project.ID), key)
			ctx = w.WithScope(baseCtx)
			parent, err := svc.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: []byte(`{"model":"gpt-4o"}`)}, llm.APIFormatOpenAIChatCompletion)
			require.NoError(t, err)
			select {
			case <-store.entered:
			case <-time.After(time.Second):
				t.Fatal("external Save was not entered")
			}
			if full {
				laneScope := &observationScope{writer: w.payloadLane}
				for i := 1; i < w.payloadLane.config.MaxItems; i++ {
					require.NoError(t, laneScope.submit(baseCtx, 0, func(context.Context) error { return nil }))
				}
			}
			channel := createStorageTestChannel(t, baseCtx, client, nil)
			execution, err := svc.CreateRequestExecution(ctx, channel, "gpt-4o", parent, httpclient.Request{JSONBody: []byte(`{}`)}, llm.APIFormatOpenAIChatCompletion, false)
			require.NoError(t, err)
			require.NoError(t, svc.UpdateRequestExecutionCompletedForChannel(ctx, execution.ID, "response", []byte(`{}`), nil, channel))
			_, err = svc.UsageLogService.CreateUsageLogFromRequest(ctx, parent, execution, &llm.Usage{TotalTokens: 5})
			require.NoError(t, err)
			require.NoError(t, svc.UpdateRequestCompleted(ctx, parent.ID, "response", []byte(`{"ok":true}`), nil))
			EndForwardingObservation(ctx)
			coreDrained := make(chan struct{})
			require.NoError(t, (&observationScope{writer: w}).submit(baseCtx, 0, func(context.Context) error { close(coreDrained); return nil }))
			select {
			case <-coreDrained:
			case <-time.After(2 * time.Second):
				t.Fatal("external payload Save blocked core usage persistence")
			}
			second := w.WithScope(baseCtx)
			quota := &objects.APIKeyQuota{Requests: lo.ToPtr(int64(2)), TotalTokens: lo.ToPtr(int64(10)), Period: objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime}}
			result, err := NewQuotaServiceWithObservationWriter(client, legacy.SystemService, w).CheckAPIKeyQuota(second, key.ID, quota)
			require.NoError(t, err)
			require.True(t, result.Allowed, "a healthy quota request retains available balance while external Save is blocked")
			EndForwardingObservation(second)
			close(store.release)
			drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, w.Wait(drainCtx))
			row := client.Request.Query().OnlyX(ctx)
			require.Equal(t, request.StatusCompleted, row.Status)
			require.Equal(t, ds.ID, row.DataStorageID)
			require.Equal(t, "stored", row.EvidenceDisposition.RequestBody.Outcome)
			if full {
				require.Equal(t, "writeFailed", row.EvidenceDisposition.ResponseBody.Outcome)
			}
			require.NoError(t, w.Stop(drainCtx))
		})
	}
}

func TestForwardingObservationExternalFailureRetainsTerminalEvidence(t *testing.T) {
	legacy, client, ctx, project := setupRequestExecutionStorageTest(t)
	defer client.Close()
	ds := client.DataStorage.Create().SetName("observation-failure").SetDescription("isolated failing store").SetType(datastorage.TypeS3).
		SetSettings(&objects.DataStorageSettings{S3: &objects.S3{}}).SetStatus(datastorage.StatusActive).SaveX(ctx)
	store := &observationBlockedObjectStore{entered: make(chan struct{}), release: make(chan struct{}), failure: errors.New("isolated object store failure")}
	close(store.release)
	legacy.DataStorageService.objectStoreCache[ds.ID] = store
	require.NoError(t, legacy.SystemService.SetDefaultDataStorageID(ctx, ds.ID))
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{AttemptTimeout: 50 * time.Millisecond, MaxAttempts: 2})
	svc := NewRequestServiceWithObservationWriter(client, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, w)
	require.NoError(t, w.Start(ctx))
	ctx = w.WithScope(contexts.WithProjectID(ctx, project.ID))
	parent, err := svc.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: []byte(`{"model":"gpt-4o"}`)}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	require.NoError(t, svc.UpdateRequestCompleted(ctx, parent.ID, "response", []byte(`{"ok":true}`), nil))
	EndForwardingObservation(ctx)
	drainCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, w.Wait(drainCtx))
	row := client.Request.Query().OnlyX(ctx)
	require.Equal(t, request.StatusCompleted, row.Status)
	for _, disposition := range []objects.Disposition{row.EvidenceDisposition.RequestBody, row.EvidenceDisposition.ResponseBody} {
		require.Equal(t, "writeFailed", disposition.Outcome)
		require.Equal(t, "external_write_failed", *disposition.FailureClass)
	}
	require.NoError(t, w.Stop(drainCtx))
}

func TestForwardingObservationAmbiguousCreateCommitsOnce(t *testing.T) {
	legacy, client, ctx, project := setupRequestExecutionStorageTest(t)
	defer client.Close()
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{AttemptTimeout: 50 * time.Millisecond})
	svc := NewRequestServiceWithObservationWriter(client, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, w)
	require.NoError(t, w.Start(ctx))
	channel := createStorageTestChannel(t, ctx, client, nil)
	ctx = w.WithScope(contexts.WithProjectID(ctx, project.ID))
	var injected atomic.Bool
	var usageNotifications atomic.Int32
	svc.UsageLogService.OnUsageLogCreated = func() { usageNotifications.Add(1) }
	client.Request.Use(func(next ent.Mutator) ent.Mutator {
		return hook.RequestFunc(func(hookCtx context.Context, mutation *ent.RequestMutation) (ent.Value, error) {
			value, err := next.Mutate(hookCtx, mutation)
			if err == nil && mutation.Op().Is(ent.OpCreate) && injected.CompareAndSwap(false, true) {
				return value, errors.New("connection lost after committed INSERT")
			}
			return value, err
		})
	})
	parent, err := svc.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: []byte(`{"model":"gpt-4o","messages":[]}`)}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	execution, err := svc.CreateRequestExecution(ctx, channel, "gpt-4o", parent, httpclient.Request{JSONBody: []byte(`{"model":"gpt-4o"}`)}, llm.APIFormatOpenAIChatCompletion, false)
	require.NoError(t, err)
	require.NoError(t, svc.UpdateRequestExecutionCompletedForChannel(ctx, execution.ID, "response", []byte(`{"ok":true}`), nil, channel))
	_, err = svc.UsageLogService.CreateUsageLogFromRequest(ctx, parent, execution, &llm.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5})
	require.NoError(t, err)
	_, err = svc.UsageLogService.CreateUsageLogFromRequest(ctx, parent, execution, &llm.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5})
	require.NoError(t, err)
	require.NoError(t, svc.UpdateRequestCompleted(ctx, parent.ID, "response", []byte(`{"ok":true}`), nil))
	EndForwardingObservation(ctx)
	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, w.Wait(drainCtx))
	row := client.Request.Query().OnlyX(ctx)
	require.Equal(t, request.StatusCompleted, row.Status)
	require.WithinDuration(t, parent.CreatedAt, row.CreatedAt, time.Microsecond)
	actualExecution := client.RequestExecution.Query().OnlyX(ctx)
	require.Equal(t, requestexecution.StatusCompleted, actualExecution.Status)
	require.Equal(t, row.ID, actualExecution.RequestID)
	require.Equal(t, row.ID, client.UsageLog.Query().OnlyX(ctx).RequestID)
	require.Equal(t, int32(1), usageNotifications.Load())
	require.NoError(t, w.Stop(drainCtx))
}

func TestForwardingObservationShutdownCancelsBlockedStorage(t *testing.T) {
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 8, MaxBytesMiB: 1, AttemptTimeout: time.Minute})
	require.NoError(t, w.Start(t.Context()))
	ctx := w.WithScope(t.Context())
	scope := observationFromContext(ctx)
	entered := make(chan struct{})
	require.NoError(t, scope.submit(ctx, 0, func(workerCtx context.Context) error {
		close(entered)
		<-workerCtx.Done()
		return workerCtx.Err()
	}))
	<-entered
	stopCtx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, w.Stop(stopCtx), context.Canceled)
	select {
	case <-w.done:
	case <-time.After(time.Second):
		t.Fatal("shutdown failed to cancel running storage")
	}
	EndForwardingObservation(ctx)
	w.mu.Lock()
	defer w.mu.Unlock()
	require.Zero(t, w.items)
	require.Zero(t, w.bytes)
	require.Empty(t, w.scopes)
}

func TestForwardingObservationRetriesAcceptedWorkInOrder(t *testing.T) {
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 8, AttemptTimeout: 10 * time.Millisecond})
	require.NoError(t, w.Start(t.Context()))
	ctx := w.WithScope(t.Context())
	scope := observationFromContext(ctx)
	var attempts atomic.Int32
	require.NoError(t, scope.submitCore(ctx, -1, 0, func(context.Context) error {
		if attempts.Add(1) == 1 {
			return errors.New("transient storage outage")
		}
		scope.bindID(-1, 42)
		return nil
	}))
	require.NoError(t, scope.submitTerminal(ctx, -1, 0, func(context.Context) error {
		id, err := scope.resolve(-1)
		if err == nil && id != 42 {
			return errors.New("incorrect association")
		}
		return err
	}))
	EndForwardingObservation(ctx)
	drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, w.Wait(drainCtx))
	require.Equal(t, int32(2), attempts.Load())
	require.NoError(t, w.Stop(drainCtx))
}
