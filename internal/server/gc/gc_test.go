package gc

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zhenzou/executors"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channelprobe"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/ent/thread"
	"github.com/looplj/axonhub/internal/ent/trace"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestWorker_getBatchSize(t *testing.T) {
	worker := &Worker{
		Ent:    nil,
		Config: Config{CRON: "0 0 * * *"},
	}

	// Test default batch size
	batchSize := worker.getBatchSize()
	if batchSize != defaultBatchSize {
		t.Errorf("Expected batch size %d, got %d", defaultBatchSize, batchSize)
	}

	// Test with overridden batch size
	originalBatchSize := defaultBatchSize
	defaultBatchSize = 20

	defer func() { defaultBatchSize = originalBatchSize }()

	batchSize = worker.getBatchSize()
	if batchSize != 20 {
		t.Errorf("Expected batch size 20, got %d", batchSize)
	}
}

func TestWorker_cleanupRequestExternalStorageDeletesFsArtifacts(t *testing.T) {
	worker, ctx, dataStorage, baseDir := setupWorkerWithFSStorage(t)

	req := &ent.Request{
		ID:            101,
		ProjectID:     202,
		DataStorageID: dataStorage.ID,
	}

	fileKeys := []string{
		biz.GenerateRequestBodyKey(req.ProjectID, req.ID),
		biz.GenerateResponseBodyKey(req.ProjectID, req.ID),
		biz.GenerateResponseChunksKey(req.ProjectID, req.ID),
	}

	dirKeys := []string{
		biz.GenerateRequestExecutionsDirKey(req.ProjectID, req.ID),
		biz.GenerateRequestDirKey(req.ProjectID, req.ID),
	}

	for _, key := range fileKeys {
		createFileForKey(t, baseDir, key)
	}

	for _, key := range dirKeys {
		createDirForKey(t, baseDir, key)
	}

	require.NoError(t, worker.cleanupRequestExternalStorage(ctx, req, make(map[int]*ent.DataStorage)))

	for _, key := range append(fileKeys, dirKeys...) {
		assertRemoved(t, baseDir, key)
	}
}

func TestWorker_cleanupRequestExternalStorageDeletesRecordedContent(t *testing.T) {
	worker, ctx, dataStorage, baseDir := setupWorkerWithFSStorage(t)

	contentKey := "/202/requests/101/audio/request-101.mp3"
	req := &ent.Request{
		ID:                101,
		ProjectID:         202,
		ContentStorageID:  &dataStorage.ID,
		ContentStorageKey: &contentKey,
	}
	createFileForKey(t, baseDir, contentKey)

	require.NoError(t, worker.cleanupRequestExternalStorage(ctx, req, make(map[int]*ent.DataStorage)))
	assertRemoved(t, baseDir, contentKey)
}

func TestWorker_cleanupRequestExternalStorageRejectsUnownedRecordedContent(t *testing.T) {
	worker, ctx, dataStorage, baseDir := setupWorkerWithFSStorage(t)

	contentKey := "/202/requests/999/audio/not-owned.mp3"
	req := &ent.Request{
		ID:                101,
		ProjectID:         202,
		ContentStorageID:  &dataStorage.ID,
		ContentStorageKey: &contentKey,
	}
	createFileForKey(t, baseDir, contentKey)

	err := worker.cleanupRequestExternalStorage(ctx, req, make(map[int]*ent.DataStorage))
	require.ErrorContains(t, err, "outside its owned prefix")
	_, statErr := os.Stat(pathForKey(baseDir, contentKey))
	require.NoError(t, statErr, "unowned content must not be deleted")
}

func TestWorker_cleanupRequestExternalStorageRejectsBackslashTraversalBeforeAnyDelete(t *testing.T) {
	worker, ctx, dataStorage, baseDir := setupWorkerWithFSStorage(t)

	maliciousKey := `/202/requests/101/..\999\video\owned.mp4`
	targetKey := "/202/requests/999/video/owned.mp4"
	req := &ent.Request{
		ID:                101,
		ProjectID:         202,
		DataStorageID:     dataStorage.ID,
		ContentStorageID:  &dataStorage.ID,
		ContentStorageKey: &maliciousKey,
	}
	ordinaryKey := biz.GenerateRequestBodyKey(req.ProjectID, req.ID)
	createFileForKey(t, baseDir, ordinaryKey)
	createFileForKey(t, baseDir, targetKey)

	err := worker.cleanupRequestExternalStorage(ctx, req, make(map[int]*ent.DataStorage))
	require.ErrorContains(t, err, "noncanonical separator")
	for _, key := range []string{ordinaryKey, targetKey} {
		_, statErr := os.Stat(pathForKey(baseDir, key))
		require.NoError(t, statErr, "%s must remain after rejected traversal metadata", key)
	}
}

func TestWorker_cleanupExecutionExternalStorageDeletesFsArtifacts(t *testing.T) {
	worker, ctx, dataStorage, baseDir := setupWorkerWithFSStorage(t)

	req := &ent.Request{
		ID:            303,
		ProjectID:     404,
		DataStorageID: dataStorage.ID,
	}

	exec := &ent.RequestExecution{
		ID:            505,
		RequestID:     req.ID,
		ProjectID:     req.ProjectID,
		DataStorageID: dataStorage.ID,
	}

	fileKeys := []string{
		biz.GenerateExecutionRequestBodyKey(exec.ProjectID, exec.RequestID, exec.ID),
		biz.GenerateExecutionResponseBodyKey(exec.ProjectID, exec.RequestID, exec.ID),
		biz.GenerateExecutionResponseChunksKey(exec.ProjectID, exec.RequestID, exec.ID),
	}

	dirKeys := []string{
		biz.GenerateExecutionRequestDirKey(exec.ProjectID, exec.RequestID, exec.ID),
	}

	for _, key := range fileKeys {
		createFileForKey(t, baseDir, key)
	}

	for _, key := range dirKeys {
		createDirForKey(t, baseDir, key)
	}

	require.NoError(t, worker.cleanupExecutionExternalStorage(ctx, exec, make(map[int]*ent.DataStorage)))

	for _, key := range append(fileKeys, dirKeys...) {
		assertRemoved(t, baseDir, key)
	}
}

func TestWorker_cleanupOldRequestExecutionsDeletesOnlyOldTerminalRows(t *testing.T) {
	worker, ctx, client := setupGCWorker(t)
	cutoff := time.Now().UTC().Add(-72 * time.Hour)
	old := cutoff.Add(-time.Hour)
	recent := cutoff.Add(time.Hour)

	statuses := []requestexecution.Status{
		requestexecution.StatusPending,
		requestexecution.StatusProcessing,
		requestexecution.StatusCompleted,
		requestexecution.StatusFailed,
		requestexecution.StatusCanceled,
	}
	kept := make(map[int]struct{})
	parentUpdatedAt := make(map[int]time.Time)
	for _, status := range statuses {
		req := createGCRequest(t, client, ctx, request.StatusCompleted, old, 0)
		parentUpdatedAt[req.ID] = req.UpdatedAt
		exec := createGCExecution(t, client, ctx, req.ID, status, old)
		if status == requestexecution.StatusPending || status == requestexecution.StatusProcessing {
			kept[exec.ID] = struct{}{}
		}
	}
	recentReq := createGCRequest(t, client, ctx, request.StatusCompleted, old, 0)
	parentUpdatedAt[recentReq.ID] = recentReq.UpdatedAt
	recentExec := createGCExecution(t, client, ctx, recentReq.ID, requestexecution.StatusCompleted, recent)
	kept[recentExec.ID] = struct{}{}
	activeReq := createGCRequest(t, client, ctx, request.StatusProcessing, old, 0)
	parentUpdatedAt[activeReq.ID] = activeReq.UpdatedAt
	activeParentExec := createGCExecution(t, client, ctx, activeReq.ID, requestexecution.StatusCompleted, old)
	kept[activeParentExec.ID] = struct{}{}

	deleted, err := worker.cleanupOldRequestExecutions(ctx, cutoff)
	require.NoError(t, err)
	require.Equal(t, 3, deleted)

	remaining, err := client.RequestExecution.Query().IDs(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, len(kept))
	for _, id := range remaining {
		_, ok := kept[id]
		require.True(t, ok, "unexpected remaining execution %d", id)
	}
	for id, wantUpdatedAt := range parentUpdatedAt {
		gotRequest := client.Request.GetX(ctx, id)
		require.True(t, gotRequest.UpdatedAt.Equal(wantUpdatedAt), "request %d cleanup claim timestamp was not restored", id)
	}
}

func TestWorker_reconcileStaleRequestExecutions(t *testing.T) {
	worker, ctx, client := setupGCWorker(t)
	cutoff := time.Now().UTC().Add(-72 * time.Hour)
	old := cutoff.Add(-time.Hour)

	eligibleParent := createGCRequest(t, client, ctx, request.StatusCompleted, old, 0)
	eligible := createGCExecution(t, client, ctx, eligibleParent.ID, requestexecution.StatusProcessing, old)
	client.RequestExecution.UpdateOneID(eligible.ID).SetManagedObservability(true).SetUpdatedAt(old).ExecX(ctx)
	client.ManagedObservabilityState.Create().SetID(1).SetChargedBytes(100).SaveX(ctx)

	recentlyUpdatedParent := createGCRequest(t, client, ctx, request.StatusCompleted, old, 0)
	recentlyUpdated := createGCExecution(t, client, ctx, recentlyUpdatedParent.ID, requestexecution.StatusProcessing, old)

	activeParent := createGCRequest(t, client, ctx, request.StatusProcessing, old, 0)
	activeChild := createGCExecution(t, client, ctx, activeParent.ID, requestexecution.StatusProcessing, old)
	client.RequestExecution.UpdateOneID(activeChild.ID).SetUpdatedAt(old).ExecX(ctx)

	recentParent := createGCRequest(t, client, ctx, request.StatusCompleted, cutoff.Add(time.Hour), 0)
	recentChild := createGCExecution(t, client, ctx, recentParent.ID, requestexecution.StatusProcessing, old)
	client.RequestExecution.UpdateOneID(recentChild.ID).SetUpdatedAt(old).ExecX(ctx)

	retainedTrace := client.Trace.Create().SetProjectID(1).SetTraceID("stale-reconcile-retained").
		SetStatus(trace.StatusRetained).SetCreatedAt(old).SaveX(ctx)
	retainedParent := createGCRequest(t, client, ctx, request.StatusCompleted, old, retainedTrace.ID)
	retainedChild := createGCExecution(t, client, ctx, retainedParent.ID, requestexecution.StatusProcessing, old)
	client.RequestExecution.UpdateOneID(retainedChild.ID).SetUpdatedAt(old).ExecX(ctx)

	result, err := worker.reconcileStaleRequestExecutions(ctx, cutoff)
	require.NoError(t, err)
	require.Equal(t, 1, result.Scanned)
	require.Equal(t, 1, result.ReconciledStale)
	require.Zero(t, result.SkippedNonterminal)
	updated := client.RequestExecution.GetX(ctx, eligible.ID)
	require.Equal(t, requestexecution.StatusFailed, updated.Status)
	require.Equal(t, staleExecutionError, updated.ErrorMessage)
	require.Equal(t, int64(100+len(staleExecutionError)), client.ManagedObservabilityState.GetX(ctx, 1).ChargedBytes)
	require.Equal(t, requestexecution.StatusProcessing, client.RequestExecution.GetX(ctx, recentlyUpdated.ID).Status)
	require.Equal(t, requestexecution.StatusProcessing, client.RequestExecution.GetX(ctx, activeChild.ID).Status)
	require.Equal(t, requestexecution.StatusProcessing, client.RequestExecution.GetX(ctx, recentChild.ID).Status)
	require.Equal(t, requestexecution.StatusProcessing, client.RequestExecution.GetX(ctx, retainedChild.ID).Status)
}

func TestWorker_reconcileStaleRequestExecutionsRechecksRacyChild(t *testing.T) {
	worker, ctx, client := setupGCWorker(t)
	cutoff := time.Now().UTC().Add(-72 * time.Hour)
	old := cutoff.Add(-time.Hour)
	parent := createGCRequest(t, client, ctx, request.StatusCompleted, old, 0)
	execution := createGCExecution(t, client, ctx, parent.ID, requestexecution.StatusProcessing, old)
	client.RequestExecution.UpdateOneID(execution.ID).SetUpdatedAt(old).ExecX(ctx)
	worker.beforeStaleReconcile = func(executionID int) {
		client.RequestExecution.UpdateOneID(executionID).SetUpdatedAt(time.Now().UTC()).ExecX(ctx)
	}

	result, err := worker.reconcileStaleRequestExecutions(ctx, cutoff)
	require.NoError(t, err)
	require.Equal(t, 1, result.Scanned)
	require.Equal(t, 1, result.SkippedNonterminal)
	require.Zero(t, result.ReconciledStale)
	require.Equal(t, requestexecution.StatusProcessing, client.RequestExecution.GetX(ctx, execution.ID).Status)
}

func TestWorker_cleanupRequestsReconcilesInBatchesThenUsesNormalDeletion(t *testing.T) {
	worker, ctx, client := setupGCWorker(t)
	originalBatchSize := defaultBatchSize
	defaultBatchSize = 2
	t.Cleanup(func() { defaultBatchSize = originalBatchSize })
	old := time.Now().UTC().Add(-10 * 24 * time.Hour)
	for range 5 {
		parent := createGCRequest(t, client, ctx, request.StatusCompleted, old, 0)
		execution := createGCExecution(t, client, ctx, parent.ID, requestexecution.StatusProcessing, old)
		client.RequestExecution.UpdateOneID(execution.ID).SetUpdatedAt(old).ExecX(ctx)
	}

	result, err := worker.cleanupRequestsWithResult(ctx, 3, false)
	require.NoError(t, err)
	require.Equal(t, 15, result.Scanned)
	require.Equal(t, 5, result.ReconciledStale)
	require.Equal(t, 10, result.Deleted)
	require.Zero(t, client.RequestExecution.Query().CountX(ctx))
	require.Zero(t, client.Request.Query().CountX(ctx))
}

func TestWorker_cleanupRequestsReportsSuccessfulZeroDelete(t *testing.T) {
	worker, ctx, _ := setupGCWorker(t)

	result, err := worker.cleanupRequestsWithResult(ctx, 3, false)
	require.NoError(t, err)
	require.Equal(t, requestCleanupResult{}, result)
}

func TestWorker_PreviewAndCleanupUseIndependentRetentionAndActivityCutoffs(t *testing.T) {
	for _, retentionDays := range []int{1, 3} {
		t.Run(fmt.Sprintf("%d_day_retention", retentionDays), func(t *testing.T) {
			worker, ctx, client := setupGCWorker(t)
			now := time.Now().UTC()
			createdOld := now.Add(-time.Duration(retentionDays*24+1) * time.Hour)
			updatedStale := now.Add(-25 * time.Hour)
			if retentionDays == 3 {
				updatedStale = now.Add(-48 * time.Hour)
			}

			eligibleParent := createGCRequest(t, client, ctx, request.StatusCompleted, createdOld, 0)
			eligibleChild := createGCExecution(t, client, ctx, eligibleParent.ID, requestexecution.StatusProcessing, createdOld)
			client.RequestExecution.UpdateOneID(eligibleChild.ID).SetUpdatedAt(updatedStale).ExecX(ctx)
			terminalSibling := createGCExecution(t, client, ctx, eligibleParent.ID, requestexecution.StatusCompleted, createdOld)

			recentlyUpdatedParent := createGCRequest(t, client, ctx, request.StatusCompleted, createdOld, 0)
			recentlyUpdatedChild := createGCExecution(t, client, ctx, recentlyUpdatedParent.ID, requestexecution.StatusProcessing, createdOld)
			client.RequestExecution.UpdateOneID(recentlyUpdatedChild.ID).SetUpdatedAt(now.Add(-12 * time.Hour)).ExecX(ctx)

			recentCreated := now.Add(-time.Duration(retentionDays*24-1) * time.Hour)
			recentParent := createGCRequest(t, client, ctx, request.StatusCompleted, recentCreated, 0)
			recentChild := createGCExecution(t, client, ctx, recentParent.ID, requestexecution.StatusProcessing, recentCreated)
			client.RequestExecution.UpdateOneID(recentChild.ID).SetUpdatedAt(updatedStale).ExecX(ctx)

			activeParent := createGCRequest(t, client, ctx, request.StatusProcessing, createdOld, 0)
			activeChild := createGCExecution(t, client, ctx, activeParent.ID, requestexecution.StatusProcessing, createdOld)
			client.RequestExecution.UpdateOneID(activeChild.ID).SetUpdatedAt(updatedStale).ExecX(ctx)

			retainedTrace := client.Trace.Create().SetProjectID(1).
				SetTraceID(fmt.Sprintf("preview-retained-%d", retentionDays)).
				SetStatus(trace.StatusRetained).SetCreatedAt(createdOld).SaveX(ctx)
			retainedParent := createGCRequest(t, client, ctx, request.StatusCompleted, createdOld, retainedTrace.ID)
			retainedChild := createGCExecution(t, client, ctx, retainedParent.ID, requestexecution.StatusProcessing, createdOld)
			client.RequestExecution.UpdateOneID(retainedChild.ID).SetUpdatedAt(updatedStale).ExecX(ctx)

			preview, err := worker.PreviewCleanup(ctx, TriggerGcCleanupInput{RequestsCleanupDays: retentionDays})
			require.NoError(t, err)
			require.Len(t, preview, 1)
			require.Equal(t, "requests", preview[0].ResourceType)
			require.Equal(t, 1, preview[0].EstimatedCount)

			result, err := worker.cleanupRequestsWithResult(ctx, retentionDays, false)
			require.NoError(t, err)
			require.Equal(t, 1, result.ReconciledStale)
			require.Equal(t, 3, result.Deleted)
			require.False(t, client.Request.Query().Where(request.IDEQ(eligibleParent.ID)).ExistX(ctx))
			require.False(t, client.RequestExecution.Query().Where(requestexecution.IDEQ(eligibleChild.ID)).ExistX(ctx))
			require.False(t, client.RequestExecution.Query().Where(requestexecution.IDEQ(terminalSibling.ID)).ExistX(ctx))
			for _, executionID := range []int{recentlyUpdatedChild.ID, recentChild.ID, activeChild.ID, retainedChild.ID} {
				require.Equal(t, requestexecution.StatusProcessing, client.RequestExecution.GetX(ctx, executionID).Status)
			}
		})
	}
}

func TestWorker_cleanupOldRequestsPreservesActiveRecentAndRetainedRows(t *testing.T) {
	worker, ctx, client := setupGCWorker(t)
	cutoff := time.Now().UTC().Add(-72 * time.Hour)
	old := cutoff.Add(-time.Hour)
	recent := cutoff.Add(time.Hour)

	for _, status := range []request.Status{request.StatusCompleted, request.StatusFailed, request.StatusCanceled} {
		createGCRequest(t, client, ctx, status, old, 0)
	}

	kept := make(map[int]struct{})
	for _, status := range []request.Status{request.StatusPending, request.StatusProcessing} {
		req := createGCRequest(t, client, ctx, status, old, 0)
		kept[req.ID] = struct{}{}
	}
	recentReq := createGCRequest(t, client, ctx, request.StatusCompleted, recent, 0)
	kept[recentReq.ID] = struct{}{}

	retainedTrace, err := client.Trace.Create().
		SetProjectID(1).
		SetTraceID("retained-trace").
		SetStatus(trace.StatusRetained).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)
	retainedTraceReq := createGCRequest(t, client, ctx, request.StatusCompleted, old, retainedTrace.ID)
	kept[retainedTraceReq.ID] = struct{}{}

	retainedThread, err := client.Thread.Create().
		SetProjectID(1).
		SetThreadID("retained-thread").
		SetStatus(thread.StatusRetained).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)
	archivedTrace, err := client.Trace.Create().
		SetProjectID(1).
		SetTraceID("archived-trace-in-retained-thread").
		SetThreadID(retainedThread.ID).
		SetStatus(trace.StatusArchived).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)
	retainedThreadReq := createGCRequest(t, client, ctx, request.StatusCompleted, old, archivedTrace.ID)
	kept[retainedThreadReq.ID] = struct{}{}

	reqWithRecentExec := createGCRequest(t, client, ctx, request.StatusCompleted, old, 0)
	createGCExecution(t, client, ctx, reqWithRecentExec.ID, requestexecution.StatusCompleted, recent)
	kept[reqWithRecentExec.ID] = struct{}{}

	deleted, err := worker.cleanupOldRequestsRecords(ctx, cutoff)
	require.NoError(t, err)
	require.Equal(t, 3, deleted)

	remaining, err := client.Request.Query().IDs(ctx)
	require.NoError(t, err)
	require.Len(t, remaining, len(kept))
	for _, id := range remaining {
		_, ok := kept[id]
		require.True(t, ok, "unexpected remaining request %d", id)
	}
}

func TestWorker_cleanupThreadsAndTracesPreserveEitherRetainedBoundary(t *testing.T) {
	worker, ctx, client := setupGCWorker(t)
	old := time.Now().UTC().Add(-5 * 24 * time.Hour)

	activeThread, err := client.Thread.Create().
		SetProjectID(1).
		SetThreadID("active-thread-with-retained-trace").
		SetStatus(thread.StatusActive).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)
	_, err = client.Trace.Create().
		SetProjectID(1).
		SetTraceID("individually-retained-trace").
		SetThreadID(activeThread.ID).
		SetStatus(trace.StatusRetained).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)

	retainedThread, err := client.Thread.Create().
		SetProjectID(1).
		SetThreadID("retained-thread-with-archived-trace").
		SetStatus(thread.StatusRetained).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)
	archivedTrace, err := client.Trace.Create().
		SetProjectID(1).
		SetTraceID("archived-trace-protected-by-thread").
		SetThreadID(retainedThread.ID).
		SetStatus(trace.StatusArchived).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)

	require.NoError(t, worker.cleanupThreads(ctx, 3, false))
	require.NoError(t, worker.cleanupTraces(ctx, 3, false))
	require.True(t, client.Thread.Query().Where(thread.IDEQ(activeThread.ID)).ExistX(ctx))
	require.True(t, client.Trace.Query().Where(trace.IDEQ(archivedTrace.ID)).ExistX(ctx))
}

func TestWorker_cleanupOldRequestsPreservesRowWhenExternalCleanupFails(t *testing.T) {
	worker, ctx, client := setupGCWorker(t)
	old := time.Now().UTC().Add(-96 * time.Hour)
	contentKey := "/recorded-content/video/missing-storage.mp4"
	req, err := client.Request.Create().
		SetProjectID(1).
		SetModelID("gc-test-model").
		SetRequestBody([]byte(`{"test":true}`)).
		SetStatus(request.StatusCompleted).
		SetContentStorageKey(contentKey).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)

	deleted, err := worker.cleanupOldRequestsRecords(ctx, time.Now().UTC().Add(-72*time.Hour))
	require.ErrorContains(t, err, "content_storage_key without content_storage_id")
	require.Zero(t, deleted)
	require.True(t, client.Request.Query().Where(request.IDEQ(req.ID)).ExistX(ctx))
}

func TestWorker_cleanupOldRequestsPreservesRowWhenStorageDeleteFails(t *testing.T) {
	worker, ctx, client := setupGCWorker(t)
	old := time.Now().UTC().Add(-96 * time.Hour)
	deletable := createGCRequest(t, client, ctx, request.StatusCompleted, old, 0)
	brokenStorage, err := client.DataStorage.Create().
		SetName("broken-fs-storage").
		SetDescription("missing directory for deterministic delete failure").
		SetPrimary(false).
		SetType(datastorage.TypeFs).
		SetSettings(&objects.DataStorageSettings{}).
		SetStatus(datastorage.StatusActive).
		Save(ctx)
	require.NoError(t, err)
	req, err := client.Request.Create().
		SetProjectID(1).
		SetDataStorageID(brokenStorage.ID).
		SetModelID("gc-test-model").
		SetRequestBody([]byte(`{"test":true}`)).
		SetStatus(request.StatusCompleted).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)

	result, err := worker.cleanupRequestsWithResult(ctx, 3, false)
	require.Error(t, err)
	require.Equal(t, 1, result.Deleted)
	require.Equal(t, 2, result.Scanned)
	require.Equal(t, 1, result.Retryable)
	require.Equal(t, 1, result.Errors)
	require.False(t, client.Request.Query().Where(request.IDEQ(deletable.ID)).ExistX(ctx))
	require.True(t, client.Request.Query().Where(request.IDEQ(req.ID)).ExistX(ctx))
}

func TestWorker_cleanupOldRequestExecutionsPreservesRowWhenStorageDeleteFails(t *testing.T) {
	worker, ctx, client := setupGCWorker(t)
	old := time.Now().UTC().Add(-96 * time.Hour)
	brokenStorage, err := client.DataStorage.Create().
		SetName("broken-execution-fs-storage").
		SetDescription("missing directory for deterministic delete failure").
		SetPrimary(false).
		SetType(datastorage.TypeFs).
		SetSettings(&objects.DataStorageSettings{}).
		SetStatus(datastorage.StatusActive).
		Save(ctx)
	require.NoError(t, err)
	req := createGCRequest(t, client, ctx, request.StatusCompleted, old, 0)
	exec, err := client.RequestExecution.Create().
		SetProjectID(1).
		SetRequestID(req.ID).
		SetDataStorageID(brokenStorage.ID).
		SetModelID("gc-test-model").
		SetRequestBody([]byte(`{"test":true}`)).
		SetStatus(requestexecution.StatusCompleted).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)

	deleted, err := worker.cleanupOldRequestExecutions(ctx, time.Now().UTC().Add(-72*time.Hour))
	require.Error(t, err)
	require.Zero(t, deleted)
	require.True(t, client.RequestExecution.Query().Where(requestexecution.IDEQ(exec.ID)).ExistX(ctx))
}

func TestWorker_cleanupOldRequestsRevalidatesConcurrentStatusChangeBeforeExternalDelete(t *testing.T) {
	worker, ctx, client := setupGCWorker(t)
	dataStorage, baseDir := createGCFSStorage(t, client, ctx)
	old := time.Now().UTC().Add(-96 * time.Hour)
	req, err := client.Request.Create().
		SetProjectID(1).
		SetDataStorageID(dataStorage.ID).
		SetModelID("gc-test-model").
		SetRequestBody([]byte(`{"test":true}`)).
		SetStatus(request.StatusCompleted).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)
	bodyKey := biz.GenerateRequestBodyKey(req.ProjectID, req.ID)
	createFileForKey(t, baseDir, bodyKey)

	selected := make(chan struct{}, 1)
	proceed := make(chan struct{})
	worker.beforeCandidateDelete = func(resource string, id int) {
		if resource == "request" && id == req.ID {
			selected <- struct{}{}
			<-proceed
		}
	}
	type cleanupResult struct {
		deleted    int
		accounting requestCleanupResult
		err        error
	}
	result := make(chan cleanupResult, 1)
	go func() {
		var accounting requestCleanupResult
		deleted, cleanupErr := worker.cleanupOldRequestsRecordsWithAccounting(
			ctx,
			time.Now().UTC().Add(-72*time.Hour),
			&accounting,
		)
		result <- cleanupResult{deleted: deleted, accounting: accounting, err: cleanupErr}
	}()

	select {
	case <-selected:
	case <-time.After(5 * time.Second):
		t.Fatal("GC did not reach the pre-delete barrier")
	}
	_, err = client.Request.UpdateOneID(req.ID).SetStatus(request.StatusProcessing).Save(ctx)
	require.NoError(t, err)
	close(proceed)

	got := <-result
	require.NoError(t, got.err)
	require.Zero(t, got.deleted)
	require.Equal(t, 1, got.accounting.Scanned)
	require.Equal(t, 1, got.accounting.SkippedNonterminal)
	updated := client.Request.GetX(ctx, req.ID)
	require.Equal(t, request.StatusProcessing, updated.Status)
	_, err = os.Stat(pathForKey(baseDir, bodyKey))
	require.NoError(t, err, "status transition must preserve the external request artifact")
}

func TestWorker_cleanupOldRequestsRevalidatesConcurrentRetainBeforeExternalDelete(t *testing.T) {
	worker, ctx, client := setupGCWorker(t)
	dataStorage, baseDir := createGCFSStorage(t, client, ctx)
	old := time.Now().UTC().Add(-96 * time.Hour)
	traceRow, err := client.Trace.Create().
		SetProjectID(1).
		SetTraceID("concurrent-retain-trace").
		SetStatus(trace.StatusActive).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)
	req, err := client.Request.Create().
		SetProjectID(1).
		SetTraceID(traceRow.ID).
		SetDataStorageID(dataStorage.ID).
		SetModelID("gc-test-model").
		SetRequestBody([]byte(`{"test":true}`)).
		SetStatus(request.StatusCompleted).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)
	bodyKey := biz.GenerateRequestBodyKey(req.ProjectID, req.ID)
	createFileForKey(t, baseDir, bodyKey)

	selected := make(chan struct{}, 1)
	proceed := make(chan struct{})
	worker.beforeCandidateDelete = func(resource string, id int) {
		if resource == "request" && id == req.ID {
			selected <- struct{}{}
			<-proceed
		}
	}
	type cleanupResult struct {
		deleted int
		err     error
	}
	result := make(chan cleanupResult, 1)
	go func() {
		deleted, cleanupErr := worker.cleanupOldRequestsRecords(ctx, time.Now().UTC().Add(-72*time.Hour))
		result <- cleanupResult{deleted: deleted, err: cleanupErr}
	}()

	select {
	case <-selected:
	case <-time.After(5 * time.Second):
		t.Fatal("GC did not reach the pre-delete barrier")
	}
	_, err = client.Trace.UpdateOneID(traceRow.ID).SetStatus(trace.StatusRetained).Save(ctx)
	require.NoError(t, err)
	close(proceed)

	got := <-result
	require.NoError(t, got.err)
	require.Zero(t, got.deleted)
	require.True(t, client.Request.Query().Where(request.IDEQ(req.ID)).ExistX(ctx))
	require.Equal(t, trace.StatusRetained, client.Trace.GetX(ctx, traceRow.ID).Status)
	_, err = os.Stat(pathForKey(baseDir, bodyKey))
	require.NoError(t, err, "retain transition must preserve the external request artifact")
}

func TestWorker_cleanupOldRequestsPreservesRequestWhenExecutionIsInsertedAfterSelection(t *testing.T) {
	worker, ctx, client := setupGCWorker(t)
	dataStorage, baseDir := createGCFSStorage(t, client, ctx)
	old := time.Now().UTC().Add(-96 * time.Hour)
	req, err := client.Request.Create().
		SetProjectID(1).
		SetDataStorageID(dataStorage.ID).
		SetModelID("gc-test-model").
		SetRequestBody([]byte(`{"test":true}`)).
		SetStatus(request.StatusCompleted).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)
	bodyKey := biz.GenerateRequestBodyKey(req.ProjectID, req.ID)
	createFileForKey(t, baseDir, bodyKey)

	selected := make(chan struct{}, 1)
	proceed := make(chan struct{})
	worker.beforeCandidateDelete = func(resource string, id int) {
		if resource == "request" && id == req.ID {
			selected <- struct{}{}
			<-proceed
		}
	}
	type cleanupResult struct {
		deleted int
		err     error
	}
	result := make(chan cleanupResult, 1)
	go func() {
		deleted, cleanupErr := worker.cleanupOldRequestsRecords(ctx, time.Now().UTC().Add(-72*time.Hour))
		result <- cleanupResult{deleted: deleted, err: cleanupErr}
	}()

	select {
	case <-selected:
	case <-time.After(5 * time.Second):
		t.Fatal("GC did not reach the request pre-delete barrier")
	}
	inserted := createGCExecution(t, client, ctx, req.ID, requestexecution.StatusProcessing, time.Now().UTC())
	close(proceed)

	got := <-result
	require.NoError(t, got.err)
	require.Zero(t, got.deleted)
	require.True(t, client.Request.Query().Where(request.IDEQ(req.ID)).ExistX(ctx))
	require.True(t, client.RequestExecution.Query().Where(requestexecution.IDEQ(inserted.ID)).ExistX(ctx))
	_, err = os.Stat(pathForKey(baseDir, bodyKey))
	require.NoError(t, err, "concurrent execution insertion must preserve request artifacts")
}

func TestWorker_cleanupOldRequestExecutionsRevalidatesConcurrentStatusChangeBeforeExternalDelete(t *testing.T) {
	worker, ctx, client := setupGCWorker(t)
	dataStorage, baseDir := createGCFSStorage(t, client, ctx)
	old := time.Now().UTC().Add(-96 * time.Hour)
	req := createGCRequest(t, client, ctx, request.StatusCompleted, old, 0)
	exec, err := client.RequestExecution.Create().
		SetProjectID(1).
		SetRequestID(req.ID).
		SetDataStorageID(dataStorage.ID).
		SetModelID("gc-test-model").
		SetRequestBody([]byte(`{"test":true}`)).
		SetStatus(requestexecution.StatusCompleted).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)
	bodyKey := biz.GenerateExecutionRequestBodyKey(exec.ProjectID, exec.RequestID, exec.ID)
	createFileForKey(t, baseDir, bodyKey)

	selected := make(chan struct{}, 1)
	proceed := make(chan struct{})
	worker.beforeCandidateDelete = func(resource string, id int) {
		if resource == "request_execution" && id == exec.ID {
			selected <- struct{}{}
			<-proceed
		}
	}
	type cleanupResult struct {
		deleted int
		err     error
	}
	result := make(chan cleanupResult, 1)
	go func() {
		deleted, cleanupErr := worker.cleanupOldRequestExecutions(ctx, time.Now().UTC().Add(-72*time.Hour))
		result <- cleanupResult{deleted: deleted, err: cleanupErr}
	}()

	select {
	case <-selected:
	case <-time.After(5 * time.Second):
		t.Fatal("GC did not reach the execution pre-delete barrier")
	}
	_, err = client.RequestExecution.UpdateOneID(exec.ID).SetStatus(requestexecution.StatusProcessing).Save(ctx)
	require.NoError(t, err)
	close(proceed)

	got := <-result
	require.NoError(t, got.err)
	require.Zero(t, got.deleted)
	updated := client.RequestExecution.GetX(ctx, exec.ID)
	require.Equal(t, requestexecution.StatusProcessing, updated.Status)
	_, err = os.Stat(pathForKey(baseDir, bodyKey))
	require.NoError(t, err, "execution status transition must preserve the external artifact")
}

func TestWorker_cleanupTracesRevalidatesConcurrentDirectRetain(t *testing.T) {
	worker, ctx, client := setupGCWorker(t)
	old := time.Now().UTC().Add(-5 * 24 * time.Hour)
	traceRow, err := client.Trace.Create().
		SetProjectID(1).
		SetTraceID("direct-concurrent-retain-trace").
		SetStatus(trace.StatusActive).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)

	selected := make(chan struct{}, 1)
	proceed := make(chan struct{})
	worker.beforeCandidateDelete = func(resource string, id int) {
		if resource == "trace" && id == traceRow.ID {
			selected <- struct{}{}
			<-proceed
		}
	}
	result := make(chan error, 1)
	go func() { result <- worker.cleanupTraces(ctx, 3, false) }()

	select {
	case <-selected:
	case <-time.After(5 * time.Second):
		t.Fatal("GC did not reach the direct trace barrier")
	}
	_, err = client.Trace.UpdateOneID(traceRow.ID).SetStatus(trace.StatusRetained).Save(ctx)
	require.NoError(t, err)
	close(proceed)

	require.NoError(t, <-result)
	require.Equal(t, trace.StatusRetained, client.Trace.GetX(ctx, traceRow.ID).Status)
}

func TestWorker_cleanupThreadsRevalidatesConcurrentDirectRetain(t *testing.T) {
	worker, ctx, client := setupGCWorker(t)
	old := time.Now().UTC().Add(-5 * 24 * time.Hour)
	threadRow, err := client.Thread.Create().
		SetProjectID(1).
		SetThreadID("direct-concurrent-retain-thread").
		SetStatus(thread.StatusActive).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)

	selected := make(chan struct{}, 1)
	proceed := make(chan struct{})
	worker.beforeCandidateDelete = func(resource string, id int) {
		if resource == "thread" && id == threadRow.ID {
			selected <- struct{}{}
			<-proceed
		}
	}
	result := make(chan error, 1)
	go func() { result <- worker.cleanupThreads(ctx, 3, false) }()

	select {
	case <-selected:
	case <-time.After(5 * time.Second):
		t.Fatal("GC did not reach the direct thread barrier")
	}
	_, err = client.Thread.UpdateOneID(threadRow.ID).SetStatus(thread.StatusRetained).Save(ctx)
	require.NoError(t, err)
	close(proceed)

	require.NoError(t, <-result)
	require.Equal(t, thread.StatusRetained, client.Thread.GetX(ctx, threadRow.ID).Status)
}

func TestWorker_cleanupUsageLogsRevalidatesConcurrentAssociatedTraceRetain(t *testing.T) {
	worker, ctx, client := setupGCWorker(t)
	old := time.Now().UTC().Add(-5 * 24 * time.Hour)
	traceRow, err := client.Trace.Create().
		SetProjectID(1).
		SetTraceID("usage-concurrent-retain-trace").
		SetStatus(trace.StatusActive).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)
	req := createGCRequest(t, client, ctx, request.StatusCompleted, old, traceRow.ID)
	usage, err := client.UsageLog.Create().
		SetRequestID(req.ID).
		SetProjectID(1).
		SetModelID("gc-test-model").
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)

	selected := make(chan struct{}, 1)
	proceed := make(chan struct{})
	worker.beforeCandidateDelete = func(resource string, id int) {
		if resource == "usage_log" && id == usage.ID {
			selected <- struct{}{}
			<-proceed
		}
	}
	result := make(chan error, 1)
	go func() { result <- worker.cleanupUsageLogs(ctx, 3, false) }()

	select {
	case <-selected:
	case <-time.After(5 * time.Second):
		t.Fatal("GC did not reach the usage-log barrier")
	}
	_, err = client.Trace.UpdateOneID(traceRow.ID).SetStatus(trace.StatusRetained).Save(ctx)
	require.NoError(t, err)
	close(proceed)

	require.NoError(t, <-result)
	require.True(t, client.UsageLog.Query().Where(usagelog.IDEQ(usage.ID)).ExistX(ctx))
	require.Equal(t, trace.StatusRetained, client.Trace.GetX(ctx, traceRow.ID).Status)
}

func TestWorker_cleanupDirectResourcesDeletesEligibleRowsTransactionally(t *testing.T) {
	worker, ctx, client := setupGCWorker(t)
	old := time.Now().UTC().Add(-5 * 24 * time.Hour)
	threadRow, err := client.Thread.Create().
		SetProjectID(1).
		SetThreadID("eligible-direct-thread").
		SetStatus(thread.StatusActive).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)
	traceRow, err := client.Trace.Create().
		SetProjectID(1).
		SetTraceID("eligible-direct-trace").
		SetThreadID(threadRow.ID).
		SetStatus(trace.StatusActive).
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)
	req := createGCRequest(t, client, ctx, request.StatusCompleted, old, traceRow.ID)
	usage, err := client.UsageLog.Create().
		SetRequestID(req.ID).
		SetProjectID(1).
		SetModelID("gc-test-model").
		SetCreatedAt(old).
		Save(ctx)
	require.NoError(t, err)

	require.NoError(t, worker.cleanupUsageLogs(ctx, 3, false))
	require.False(t, client.UsageLog.Query().Where(usagelog.IDEQ(usage.ID)).ExistX(ctx))
	require.NoError(t, worker.cleanupThreads(ctx, 3, false))
	require.False(t, client.Thread.Query().Where(thread.IDEQ(threadRow.ID)).ExistX(ctx))
	require.NoError(t, worker.cleanupTraces(ctx, 3, false))
	require.False(t, client.Trace.Query().Where(trace.IDEQ(traceRow.ID)).ExistX(ctx))
}

func setupGCWorker(t *testing.T) (*Worker, context.Context, *ent.Client) {
	t.Helper()
	client := enttest.NewEntClient(t, "sqlite3", "file:gc-retention?mode=memory&_fk=0")
	t.Cleanup(func() { _ = client.Close() })

	cacheConfig := xcache.Config{
		Mode: xcache.ModeMemory,
		Memory: xcache.MemoryConfig{
			Expiration:      5 * time.Minute,
			CleanupInterval: 10 * time.Minute,
		},
	}
	systemService := biz.NewSystemService(biz.SystemServiceParams{CacheConfig: cacheConfig})
	dataStorageService := biz.NewDataStorageService(biz.DataStorageServiceParams{
		SystemService: systemService,
		CacheConfig:   cacheConfig,
		Client:        client,
	})
	ctx := ent.NewContext(authz.WithTestBypass(context.Background()), client)

	return &Worker{Ent: client, DataStorageService: dataStorageService}, ctx, client
}

func createGCFSStorage(t *testing.T, client *ent.Client, ctx context.Context) (*ent.DataStorage, string) {
	t.Helper()
	dir := t.TempDir()
	dirCopy := dir
	dataStorage, err := client.DataStorage.Create().
		SetName("gc-fs-storage-" + filepath.Base(dir)).
		SetDescription("GC filesystem storage").
		SetPrimary(false).
		SetType(datastorage.TypeFs).
		SetSettings(&objects.DataStorageSettings{Directory: &dirCopy}).
		SetStatus(datastorage.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	return dataStorage, dir
}

func createGCRequest(t *testing.T, client *ent.Client, ctx context.Context, status request.Status, createdAt time.Time, traceID int) *ent.Request {
	t.Helper()
	builder := client.Request.Create().
		SetProjectID(1).
		SetModelID("gc-test-model").
		SetRequestBody([]byte(`{"test":true}`)).
		SetStatus(status).
		SetCreatedAt(createdAt)
	if traceID != 0 {
		builder.SetTraceID(traceID)
	}
	req, err := builder.Save(ctx)
	require.NoError(t, err)

	return req
}

func createGCExecution(t *testing.T, client *ent.Client, ctx context.Context, requestID int, status requestexecution.Status, createdAt time.Time) *ent.RequestExecution {
	t.Helper()
	exec, err := client.RequestExecution.Create().
		SetProjectID(1).
		SetRequestID(requestID).
		SetModelID("gc-test-model").
		SetRequestBody([]byte(`{"test":true}`)).
		SetStatus(status).
		SetCreatedAt(createdAt).
		Save(ctx)
	require.NoError(t, err)

	return exec
}

func TestHasRealDirectories(t *testing.T) {
	cases := []struct {
		typ  datastorage.Type
		want bool
	}{
		{datastorage.TypeFs, true},
		{datastorage.TypeWebdav, true},
		{datastorage.TypeS3, false},
		{datastorage.TypeGcs, false},
		{datastorage.TypeDatabase, false},
	}

	for _, c := range cases {
		if got := hasRealDirectories(c.typ); got != c.want {
			t.Errorf("hasRealDirectories(%s) = %v, want %v", c.typ, got, c.want)
		}
	}
}

func TestRequestCleanupUsesForUpdate(t *testing.T) {
	cases := []struct {
		dialect string
		want    bool
	}{
		{dialect: "postgres", want: true},
		{dialect: "mysql", want: true},
		{dialect: "sqlite3", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.dialect, func(t *testing.T) {
			require.Equal(t, tc.want, requestCleanupUsesForUpdate(tc.dialect))
		})
	}
}

func setupWorkerWithFSStorage(t *testing.T) (*Worker, context.Context, *ent.DataStorage, string) {
	t.Helper()

	cacheConfig := xcache.Config{
		Mode: xcache.ModeMemory,
		Memory: xcache.MemoryConfig{
			Expiration:      5 * time.Minute,
			CleanupInterval: 10 * time.Minute,
		},
	}

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")

	executor := executors.NewPoolScheduleExecutor(executors.WithMaxConcurrent(1))

	t.Cleanup(func() {
		_ = executor.Shutdown(context.Background())

		client.Close()
	})

	systemService := biz.NewSystemService(biz.SystemServiceParams{CacheConfig: cacheConfig})
	dataStorageService := biz.NewDataStorageService(biz.DataStorageServiceParams{
		SystemService: systemService,
		CacheConfig:   cacheConfig,
		Client:        client,
	})

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	dir := t.TempDir()
	dirCopy := dir
	settings := &objects.DataStorageSettings{Directory: &dirCopy}

	dataStorage, err := client.DataStorage.Create().
		SetName("fs-storage").
		SetDescription("test fs storage").
		SetPrimary(false).
		SetType(datastorage.TypeFs).
		SetSettings(settings).
		SetStatus(datastorage.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	worker := &Worker{
		DataStorageService: dataStorageService,
		Ent:                client,
	}

	return worker, ctx, dataStorage, dir
}

func createFileForKey(t *testing.T, baseDir, key string) {
	t.Helper()

	path := pathForKey(baseDir, key)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("test"), 0o644))
}

func createDirForKey(t *testing.T, baseDir, key string) {
	t.Helper()

	path := pathForKey(baseDir, key)
	require.NoError(t, os.MkdirAll(path, 0o755))
}

func assertRemoved(t *testing.T, baseDir, key string) {
	t.Helper()

	path := pathForKey(baseDir, key)
	_, err := os.Stat(path)
	require.ErrorIs(t, err, fs.ErrNotExist, "expected %s to be removed", key)
}

func pathForKey(baseDir, key string) string {
	rel := strings.TrimPrefix(key, "/")
	return filepath.Join(baseDir, filepath.FromSlash(rel))
}

func TestWorker_deleteInBatches(t *testing.T) {
	// Test that the deleteInBatches method works correctly
	// This test verifies the loop logic without needing a real database
	worker := &Worker{
		Ent:    nil,
		Config: Config{CRON: "0 0 * * *"},
	}

	// Simulate batch deletion - delete 3 times, with decreasing counts
	callCount := 0
	deleteFunc := func() (int, error) {
		callCount++
		if callCount == 1 {
			return 30, nil
		} else if callCount == 2 {
			return 15, nil
		} else {
			return 0, nil
		}
	}

	deleted, err := worker.deleteInBatches(context.Background(), deleteFunc)
	if err != nil {
		t.Fatalf("deleteInBatches failed: %v", err)
	}

	// Verify total deleted
	if deleted != 45 {
		t.Errorf("Expected to delete 45 records total, got %d", deleted)
	}

	// Verify it stopped after third call (when 0 was returned)
	if callCount != 3 {
		t.Errorf("Expected 3 delete calls, got %d", callCount)
	}
}

func TestWorker_cleanupWithZeroDays(t *testing.T) {
	worker := &Worker{
		Ent:    nil,
		Config: Config{CRON: "0 0 * * *"},
	}

	ctx := context.Background()

	// Test with 0 days - should not error
	err := worker.cleanupRequests(ctx, 0, false)
	if err != nil {
		t.Fatalf("cleanupRequests with 0 days failed: %v", err)
	}

	// Test with negative days - should not error
	err = worker.cleanupUsageLogs(ctx, -1, false)
	if err != nil {
		t.Fatalf("cleanupUsageLogs with negative days failed: %v", err)
	}
}

func TestWorker_cleanupChannelProbesDeletesInBatches(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	t.Cleanup(func() {
		client.Close()
	})

	ctx := authz.WithTestBypass(context.Background())
	ctx = ent.NewContext(ctx, client)

	originalBatchSize := defaultBatchSize
	defaultBatchSize = 2
	t.Cleanup(func() {
		defaultBatchSize = originalBatchSize
	})

	worker := &Worker{Ent: client, Config: Config{CRON: "0 0 * * *"}}
	oldTimestamp := time.Now().AddDate(0, 0, -5).Unix()
	recentTimestamp := time.Now().Unix()

	for range 5 {
		_, err := client.ChannelProbe.Create().
			SetChannelID(1).
			SetTotalRequestCount(1).
			SetSuccessRequestCount(1).
			SetTimestamp(oldTimestamp).
			Save(ctx)
		require.NoError(t, err)
	}

	for range 2 {
		_, err := client.ChannelProbe.Create().
			SetChannelID(1).
			SetTotalRequestCount(1).
			SetSuccessRequestCount(1).
			SetTimestamp(recentTimestamp).
			Save(ctx)
		require.NoError(t, err)
	}

	require.NoError(t, worker.cleanupChannelProbes(ctx, 3, false))

	oldCount, err := client.ChannelProbe.Query().
		Where(channelprobe.TimestampLT(time.Now().AddDate(0, 0, -3).Unix())).
		Count(ctx)
	require.NoError(t, err)
	require.Zero(t, oldCount)

	totalCount, err := client.ChannelProbe.Query().Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, totalCount)
}
