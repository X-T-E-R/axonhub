package biz

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
)

type failedResponseWriteFS struct {
	afero.Fs
	cancel context.CancelFunc
}

func (fs *failedResponseWriteFS) OpenFile(string, int, os.FileMode) (afero.File, error) {
	if fs.cancel != nil {
		fs.cancel()
	}
	return nil, errors.New("fixture response evidence write failure")
}

func TestFailedResponsePersistenceContextKeepsOneAbsoluteDeadline(t *testing.T) {
	deadline := time.Now().Add(time.Hour).Round(time.Microsecond)
	callerCtx, cancelCaller := context.WithDeadline(context.Background(), deadline)
	persistenceCtx, cancelPersistence := failedResponsePersistenceContext(callerCtx)
	nestedCtx, cancelNested := failedResponsePersistenceContext(persistenceCtx)
	t.Cleanup(cancelNested)
	t.Cleanup(cancelPersistence)

	persistenceDeadline, ok := persistenceCtx.Deadline()
	require.True(t, ok)
	require.Equal(t, deadline, persistenceDeadline)
	nestedDeadline, ok := nestedCtx.Deadline()
	require.True(t, ok)
	require.Equal(t, deadline, nestedDeadline)

	cancelCaller()
	require.ErrorIs(t, callerCtx.Err(), context.Canceled)
	require.NoError(t, persistenceCtx.Err())
	require.NoError(t, nestedCtx.Err())
}

func TestFailedResponsePersistenceContextAddsOneBoundForDirectCallers(t *testing.T) {
	startedAt := time.Now()
	persistenceCtx, cancelPersistence := failedResponsePersistenceContext(context.Background())
	nestedCtx, cancelNested := failedResponsePersistenceContext(persistenceCtx)
	t.Cleanup(cancelNested)
	t.Cleanup(cancelPersistence)

	persistenceDeadline, ok := persistenceCtx.Deadline()
	require.True(t, ok)
	require.WithinDuration(t, startedAt.Add(failedResponsePersistenceTimeout), persistenceDeadline, 100*time.Millisecond)
	nestedDeadline, ok := nestedCtx.Deadline()
	require.True(t, ok)
	require.Equal(t, persistenceDeadline, nestedDeadline)
}

func TestFailedResponseEvidenceContextReservesTerminalSaveBudget(t *testing.T) {
	deadline := time.Now().Add(time.Hour).Round(time.Microsecond)
	persistenceCtx, cancelPersistence := context.WithDeadline(context.Background(), deadline)
	evidenceCtx, cancelEvidence := failedResponseEvidenceContext(persistenceCtx)
	t.Cleanup(cancelPersistence)

	evidenceDeadline, ok := evidenceCtx.Deadline()
	require.True(t, ok)
	require.Equal(t, deadline.Add(-failedTerminalStatusSaveReserve), evidenceDeadline)

	cancelEvidence()
	require.ErrorIs(t, evidenceCtx.Err(), context.Canceled)
	require.NoError(t, persistenceCtx.Err())
}

func TestFailedResponseShortDeadlineRecordsStorageDisposition(t *testing.T) {
	for _, storeResponseBody := range []bool{true, false} {
		name := "storage disabled"
		if storeResponseBody {
			name = "storage enabled"
		}
		t.Run(name, func(t *testing.T) {
			t.Run("execution", func(t *testing.T) {
				svc, client, baseCtx, proj := setupRequestExecutionStorageTest(t)
				defer client.Close()
				require.NoError(t, svc.SystemService.SetStoragePolicy(baseCtx, &StoragePolicy{StoreResponseBody: storeResponseBody}))
				channel := createStorageTestChannel(t, baseCtx, client, &objects.ChannelSettings{})
				req := client.Request.Create().
					SetProjectID(proj.ID).
					SetModelID("gpt-4o").
					SetRequestBody([]byte(`{}`)).
					SetStatus(request.StatusProcessing).
					SaveX(baseCtx)
				execution := createStorageTestExecutionWithDataStorage(t, baseCtx, client, proj, req, channel, 0)

				callCtx, cancel := context.WithDeadline(baseCtx, time.Now().Add(500*time.Millisecond))
				defer cancel()
				statusCode := 400
				errorInfo := &ExecutionErrorInfo{StatusCode: &statusCode, ResponseBody: objects.JSONRawMessage(`{"error":"bad request"}`)}
				require.NoError(t, svc.UpdateRequestExecutionStatusFromErrorDetails(callCtx, execution.ID, errors.New("bad request"), nil, "bad request", errorInfo))

				stored := client.RequestExecution.GetX(baseCtx, execution.ID)
				require.Equal(t, requestexecution.StatusFailed, stored.Status)
				require.Empty(t, stored.ResponseBody)
				assertFailedResponseDeadlineDisposition(t, stored.EvidenceDisposition, storeResponseBody)
			})

			t.Run("parent", func(t *testing.T) {
				svc, client, baseCtx, proj := setupRequestExecutionStorageTest(t)
				defer client.Close()
				require.NoError(t, svc.SystemService.SetStoragePolicy(baseCtx, &StoragePolicy{StoreResponseBody: storeResponseBody}))
				req := client.Request.Create().
					SetProjectID(proj.ID).
					SetModelID("gpt-4o").
					SetRequestBody([]byte(`{}`)).
					SetStatus(request.StatusProcessing).
					SaveX(baseCtx)

				callCtx, cancel := context.WithDeadline(baseCtx, time.Now().Add(500*time.Millisecond))
				defer cancel()
				statusCode := 400
				errorInfo := &ExecutionErrorInfo{StatusCode: &statusCode, ResponseBody: objects.JSONRawMessage(`{"error":"bad request"}`)}
				require.NoError(t, svc.UpdateRequestStatusFromErrorDetails(callCtx, req.ID, errors.New("bad request"), nil, errorInfo))

				stored := client.Request.GetX(baseCtx, req.ID)
				require.Equal(t, request.StatusFailed, stored.Status)
				require.Empty(t, stored.ResponseBody)
				assertFailedResponseDeadlineDisposition(t, stored.EvidenceDisposition, storeResponseBody)
			})
		})
	}
}

func assertFailedResponseDeadlineDisposition(t *testing.T, disposition *objects.EvidenceDisposition, storeResponseBody bool) {
	t.Helper()
	require.NotNil(t, disposition)
	if !storeResponseBody {
		require.Equal(t, "omit", disposition.ResponseBody.Intent)
		require.Equal(t, "none", disposition.ResponseBody.Location)
		require.Equal(t, "omitted", disposition.ResponseBody.Outcome)
		return
	}

	require.Equal(t, "persist", disposition.ResponseBody.Intent)
	require.Equal(t, "none", disposition.ResponseBody.Location)
	require.Equal(t, "unavailable", disposition.ResponseBody.Outcome)
	require.NotNil(t, disposition.ResponseBody.FailureClass)
	require.Equal(t, failedResponseEvidenceDeadlineFailureClass, *disposition.ResponseBody.FailureClass)
}

func TestFailedDatabaseNonJSONResponseDoesNotBlockTerminalStatus(t *testing.T) {
	t.Run("execution", func(t *testing.T) {
		svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
		defer client.Close()
		require.NoError(t, svc.SystemService.SetStoragePolicy(ctx, &StoragePolicy{StoreResponseBody: true}))
		channel := createStorageTestChannel(t, ctx, client, &objects.ChannelSettings{})
		req := client.Request.Create().
			SetProjectID(proj.ID).
			SetModelID("gpt-4o").
			SetRequestBody([]byte(`{}`)).
			SetStatus(request.StatusProcessing).
			SaveX(ctx)
		execution := createStorageTestExecutionWithDataStorage(t, ctx, client, proj, req, channel, 0)

		statusCode := 502
		errorInfo := &ExecutionErrorInfo{StatusCode: &statusCode, ResponseBody: []byte("synthetic plain-text upstream failure")}
		require.NoError(t, svc.UpdateRequestExecutionStatusFromErrorDetails(ctx, execution.ID, errors.New("upstream failure"), nil, "upstream failure", errorInfo))

		stored := client.RequestExecution.GetX(ctx, execution.ID)
		require.Equal(t, requestexecution.StatusFailed, stored.Status)
		require.Empty(t, stored.ResponseBody)
		assertFailedDatabaseJSONDisposition(t, stored.EvidenceDisposition)
	})

	t.Run("parent", func(t *testing.T) {
		svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
		defer client.Close()
		require.NoError(t, svc.SystemService.SetStoragePolicy(ctx, &StoragePolicy{StoreResponseBody: true}))
		req := client.Request.Create().
			SetProjectID(proj.ID).
			SetModelID("gpt-4o").
			SetRequestBody([]byte(`{}`)).
			SetStatus(request.StatusProcessing).
			SaveX(ctx)

		statusCode := 502
		errorInfo := &ExecutionErrorInfo{StatusCode: &statusCode, ResponseBody: []byte("synthetic plain-text upstream failure")}
		require.NoError(t, svc.UpdateRequestStatusFromErrorDetails(ctx, req.ID, errors.New("upstream failure"), nil, errorInfo))

		stored := client.Request.GetX(ctx, req.ID)
		require.Equal(t, request.StatusFailed, stored.Status)
		require.Empty(t, stored.ResponseBody)
		assertFailedDatabaseJSONDisposition(t, stored.EvidenceDisposition)
	})
}

func assertFailedDatabaseJSONDisposition(t *testing.T, disposition *objects.EvidenceDisposition) {
	t.Helper()
	require.NotNil(t, disposition)
	require.Equal(t, "persist", disposition.ResponseBody.Intent)
	require.Equal(t, "none", disposition.ResponseBody.Location)
	require.Equal(t, "unavailable", disposition.ResponseBody.Outcome)
	require.NotNil(t, disposition.ResponseBody.FailureClass)
	require.Equal(t, failedResponseDatabaseJSONFailureClass, *disposition.ResponseBody.FailureClass)
}

func TestFailedExecutionExternalEvidenceFailureDoesNotBlockTerminalStatus(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		cancelContext bool
	}{
		{name: "immediate write failure"},
		{name: "write consumes caller context", cancelContext: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			svc, client, baseCtx, proj := setupRequestExecutionStorageTest(t)
			defer client.Close()
			require.NoError(t, svc.SystemService.SetStoragePolicy(baseCtx, &StoragePolicy{StoreResponseBody: true}))

			dir := t.TempDir()
			storage := client.DataStorage.Create().
				SetName(t.Name()).
				SetDescription("failed response evidence fixture").
				SetType(datastorage.TypeFs).
				SetStatus(datastorage.StatusActive).
				SetSettings(&objects.DataStorageSettings{Directory: &dir}).
				SaveX(baseCtx)
			channel := createStorageTestChannel(t, baseCtx, client, &objects.ChannelSettings{
				StoreExecutionResponseBody: lo.ToPtr(true),
			})
			req := client.Request.Create().
				SetProjectID(proj.ID).
				SetDataStorageID(storage.ID).
				SetModelID("gpt-4o").
				SetRequestBody([]byte(`{}`)).
				SetStatus(request.StatusProcessing).
				SaveX(baseCtx)
			execution := createStorageTestExecutionWithDataStorage(t, baseCtx, client, proj, req, channel, storage.ID)

			callCtx := baseCtx
			var cancel context.CancelFunc
			if testCase.cancelContext {
				callCtx, cancel = context.WithCancel(baseCtx)
				defer cancel()
			}
			svc.DataStorageService.fsCacheMu.Lock()
			svc.DataStorageService.fsCache[storage.ID] = &failedResponseWriteFS{Fs: afero.NewMemMapFs(), cancel: cancel}
			svc.DataStorageService.fsCacheMu.Unlock()

			statusCode := 400
			errorInfo := &ExecutionErrorInfo{StatusCode: &statusCode, ResponseBody: objects.JSONRawMessage(`{"error":"bad request"}`)}
			require.NoError(t, svc.UpdateRequestExecutionStatusFromErrorDetails(callCtx, execution.ID, errors.New("bad request"), nil, "bad request", errorInfo))
			if testCase.cancelContext {
				require.ErrorIs(t, callCtx.Err(), context.Canceled)
			}

			stored := client.RequestExecution.GetX(baseCtx, execution.ID)
			require.Equal(t, requestexecution.StatusFailed, stored.Status)
			require.Equal(t, "bad request", stored.ErrorMessage)
			require.Equal(t, statusCode, *stored.ResponseStatusCode)
			require.Empty(t, stored.ResponseBody)
			require.Equal(t, "writeFailed", stored.EvidenceDisposition.ResponseBody.Outcome)
			require.Equal(t, "external_write_failed", *stored.EvidenceDisposition.ResponseBody.FailureClass)
		})
	}
}

func TestFailedParentExternalEvidenceFailureDoesNotBlockTerminalStatus(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		cancelContext bool
	}{
		{name: "immediate write failure"},
		{name: "write consumes caller context", cancelContext: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			svc, client, baseCtx, proj := setupRequestExecutionStorageTest(t)
			defer client.Close()
			require.NoError(t, svc.SystemService.SetStoragePolicy(baseCtx, &StoragePolicy{StoreResponseBody: true}))

			dir := t.TempDir()
			storage := client.DataStorage.Create().
				SetName(t.Name()).
				SetDescription("failed parent response evidence fixture").
				SetType(datastorage.TypeFs).
				SetStatus(datastorage.StatusActive).
				SetSettings(&objects.DataStorageSettings{Directory: &dir}).
				SaveX(baseCtx)
			req := client.Request.Create().
				SetProjectID(proj.ID).
				SetDataStorageID(storage.ID).
				SetModelID("gpt-4o").
				SetRequestBody([]byte(`{}`)).
				SetStatus(request.StatusProcessing).
				SaveX(baseCtx)

			callCtx := baseCtx
			var cancel context.CancelFunc
			if testCase.cancelContext {
				callCtx, cancel = context.WithCancel(baseCtx)
				defer cancel()
			}
			svc.DataStorageService.fsCacheMu.Lock()
			svc.DataStorageService.fsCache[storage.ID] = &failedResponseWriteFS{Fs: afero.NewMemMapFs(), cancel: cancel}
			svc.DataStorageService.fsCacheMu.Unlock()

			statusCode := 400
			errorInfo := &ExecutionErrorInfo{StatusCode: &statusCode, ResponseBody: objects.JSONRawMessage(`{"error":"bad request"}`)}
			require.NoError(t, svc.UpdateRequestStatusFromErrorDetails(callCtx, req.ID, errors.New("bad request"), nil, errorInfo))
			if testCase.cancelContext {
				require.ErrorIs(t, callCtx.Err(), context.Canceled)
			}

			stored := client.Request.GetX(baseCtx, req.ID)
			require.Equal(t, request.StatusFailed, stored.Status)
			require.Empty(t, stored.ResponseBody)
			require.Equal(t, "writeFailed", stored.EvidenceDisposition.ResponseBody.Outcome)
			require.Equal(t, "external_write_failed", *stored.EvidenceDisposition.ResponseBody.FailureClass)
		})
	}
}
