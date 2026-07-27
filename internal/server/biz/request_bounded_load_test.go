package biz

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
)

func TestRequestServiceBoundedInlineEvidence(t *testing.T) {
	service, client, ctx, project := setupRequestExecutionStorageTest(t)
	defer client.Close()

	body := objects.JSONRawMessage(`{"payload":"bounded"}`)
	chunks := []objects.JSONRawMessage{objects.JSONRawMessage(`{"delta":"one"}`), objects.JSONRawMessage(`{"delta":"two"}`)}
	chunksRaw, err := json.Marshal(chunks)
	require.NoError(t, err)
	req := client.Request.Create().
		SetProjectID(project.ID).
		SetModelID("model").
		SetRequestBody(body).
		SetResponseBody(body).
		SetResponseChunks(chunks).
		SetStatus(request.StatusCompleted).
		SetStream(true).
		SaveX(ctx)
	exec := client.RequestExecution.Create().
		SetProjectID(project.ID).
		SetRequestID(req.ID).
		SetModelID("model").
		SetRequestBody(body).
		SetResponseBody(body).
		SetResponseChunks(chunks).
		SetStatus(requestexecution.StatusCompleted).
		SetStream(true).
		SaveX(ctx)

	_, err = service.LoadRequestBodyBounded(ctx, req, int64(len(body)))
	require.NoError(t, err)
	_, err = service.LoadRequestBodyBounded(ctx, req, int64(len(body)-1))
	require.ErrorIs(t, err, ErrDataTooLarge)
	_, err = service.LoadResponseBodyBounded(ctx, req, int64(len(body)-1))
	require.ErrorIs(t, err, ErrDataTooLarge)
	_, err = service.LoadResponseChunksBounded(ctx, req, int64(len(chunksRaw)))
	require.NoError(t, err)
	_, err = service.LoadResponseChunksBounded(ctx, req, int64(len(chunksRaw)-1))
	require.ErrorIs(t, err, ErrDataTooLarge)

	_, err = service.LoadRequestExecutionRequestBodyBounded(ctx, exec, int64(len(body)-1))
	require.ErrorIs(t, err, ErrDataTooLarge)
	_, err = service.LoadRequestExecutionResponseBodyBounded(ctx, exec, int64(len(body)-1))
	require.ErrorIs(t, err, ErrDataTooLarge)
	_, err = service.LoadRequestExecutionResponseChunksBounded(ctx, exec, int64(len(chunksRaw)-1))
	require.ErrorIs(t, err, ErrDataTooLarge)
}

func TestRequestServiceBoundedTerminalResponseEvidenceDoesNotWeakenSuccessLoaders(t *testing.T) {
	service, client, ctx, project := setupRequestExecutionStorageTest(t)
	defer client.Close()

	body := objects.JSONRawMessage(`{"partial":"preserved"}`)
	for _, status := range []struct {
		name      string
		request   request.Status
		execution requestexecution.Status
	}{
		{name: "failed", request: request.StatusFailed, execution: requestexecution.StatusFailed},
		{name: "canceled", request: request.StatusCanceled, execution: requestexecution.StatusCanceled},
	} {
		t.Run(status.name, func(t *testing.T) {
			req := client.Request.Create().
				SetProjectID(project.ID).
				SetModelID("model").
				SetRequestBody([]byte(`{}`)).
				SetResponseBody(body).
				SetStatus(status.request).
				SaveX(ctx)
			exec := client.RequestExecution.Create().
				SetProjectID(project.ID).
				SetRequestID(req.ID).
				SetModelID("model").
				SetRequestBody([]byte(`{}`)).
				SetResponseBody(body).
				SetStatus(status.execution).
				SaveX(ctx)

			ordinaryRequestBody, err := service.LoadResponseBodyBounded(ctx, req, int64(len(body)))
			require.NoError(t, err)
			require.JSONEq(t, `{}`, string(ordinaryRequestBody))
			ordinaryExecutionBody, err := service.LoadRequestExecutionResponseBodyBounded(ctx, exec, int64(len(body)))
			require.NoError(t, err)
			require.JSONEq(t, `{}`, string(ordinaryExecutionBody))

			requestEvidence, err := service.LoadResponseBodyEvidenceBounded(ctx, req, int64(len(body)))
			require.NoError(t, err)
			require.Equal(t, body, requestEvidence)
			executionEvidence, err := service.LoadRequestExecutionResponseBodyEvidenceBounded(ctx, exec, int64(len(body)))
			require.NoError(t, err)
			require.Equal(t, body, executionEvidence)

			_, err = service.LoadResponseBodyEvidenceBounded(ctx, req, int64(len(body)-1))
			require.ErrorIs(t, err, ErrDataTooLarge)
			_, err = service.LoadRequestExecutionResponseBodyEvidenceBounded(ctx, exec, int64(len(body)-1))
			require.ErrorIs(t, err, ErrDataTooLarge)
		})
	}

	absentRequest := client.Request.Create().
		SetProjectID(project.ID).
		SetModelID("model").
		SetRequestBody([]byte(`{}`)).
		SetStatus(request.StatusFailed).
		SaveX(ctx)
	absentExecution := client.RequestExecution.Create().
		SetProjectID(project.ID).
		SetRequestID(absentRequest.ID).
		SetModelID("model").
		SetRequestBody([]byte(`{}`)).
		SetStatus(requestexecution.StatusFailed).
		SaveX(ctx)
	requestEvidence, err := service.LoadResponseBodyEvidenceBounded(ctx, absentRequest, 1024)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(requestEvidence))
	executionEvidence, err := service.LoadRequestExecutionResponseBodyEvidenceBounded(ctx, absentExecution, 1024)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(executionEvidence))
}

func TestRequestServiceBoundedExternalTerminalResponseEvidence(t *testing.T) {
	service, client, ctx, project := setupRequestExecutionStorageTest(t)
	defer client.Close()

	dir := t.TempDir()
	fsStorage := client.DataStorage.Create().
		SetName("bounded-response-evidence-fs").
		SetDescription("bounded terminal response evidence").
		SetType(datastorage.TypeFs).
		SetStatus(datastorage.StatusActive).
		SetSettings(&objects.DataStorageSettings{Directory: &dir}).
		SaveX(ctx)
	body := objects.JSONRawMessage(`{"partial":"external"}`)
	req := client.Request.Create().
		SetProjectID(project.ID).
		SetDataStorageID(fsStorage.ID).
		SetModelID("model").
		SetRequestBody([]byte(`{}`)).
		SetStatus(request.StatusFailed).
		SaveX(ctx)
	exec := client.RequestExecution.Create().
		SetProjectID(project.ID).
		SetRequestID(req.ID).
		SetDataStorageID(fsStorage.ID).
		SetModelID("model").
		SetRequestBody([]byte(`{}`)).
		SetStatus(requestexecution.StatusFailed).
		SaveX(ctx)
	require.NoError(t, service.DataStorageService.SaveData(ctx, fsStorage, GenerateResponseBodyKey(project.ID, req.ID), body))
	require.NoError(t, service.DataStorageService.SaveData(ctx, fsStorage, GenerateExecutionResponseBodyKey(project.ID, req.ID, exec.ID), body))

	ordinaryRequestBody, err := service.LoadResponseBodyBounded(ctx, req, int64(len(body)))
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(ordinaryRequestBody))
	ordinaryExecutionBody, err := service.LoadRequestExecutionResponseBodyBounded(ctx, exec, int64(len(body)))
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(ordinaryExecutionBody))

	requestEvidence, err := service.LoadResponseBodyEvidenceBounded(ctx, req, int64(len(body)))
	require.NoError(t, err)
	require.Equal(t, body, requestEvidence)
	executionEvidence, err := service.LoadRequestExecutionResponseBodyEvidenceBounded(ctx, exec, int64(len(body)))
	require.NoError(t, err)
	require.Equal(t, body, executionEvidence)

	_, err = service.LoadResponseBodyEvidenceBounded(ctx, req, int64(len(body)-1))
	require.ErrorIs(t, err, ErrDataTooLarge)
	_, err = service.LoadRequestExecutionResponseBodyEvidenceBounded(ctx, exec, int64(len(body)-1))
	require.ErrorIs(t, err, ErrDataTooLarge)
}

func TestRequestServiceBoundedEvidenceHonorsCanceledContext(t *testing.T) {
	service, client, ctx, project := setupRequestExecutionStorageTest(t)
	defer client.Close()
	req := client.Request.Create().
		SetProjectID(project.ID).
		SetModelID("model").
		SetRequestBody([]byte(`{}`)).
		SetStatus(request.StatusCompleted).
		SaveX(ctx)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := service.LoadRequestBodyBounded(canceled, req, 1024)
	require.ErrorIs(t, err, context.Canceled)
}

func TestRequestServiceBoundedExternalChunksRejectBeforeUnmarshal(t *testing.T) {
	service, client, ctx, project := setupRequestExecutionStorageTest(t)
	defer client.Close()
	fsStorage := createTestDataStorage(t, client, ctx, "bounded-request-fs", false, datastorage.TypeFs)
	req := client.Request.Create().
		SetProjectID(project.ID).
		SetDataStorageID(fsStorage.ID).
		SetModelID("model").
		SetRequestBody([]byte(`{}`)).
		SetStatus(request.StatusCompleted).
		SetStream(true).
		SaveX(ctx)
	chunks := []objects.JSONRawMessage{objects.JSONRawMessage(`{"delta":"external"}`)}
	encoded, err := json.Marshal(chunks)
	require.NoError(t, err)
	key := GenerateResponseChunksKey(req.ProjectID, req.ID)
	require.NoError(t, service.DataStorageService.SaveData(ctx, fsStorage, key, encoded))

	loaded, err := service.LoadResponseChunksBounded(ctx, req, int64(len(encoded)))
	require.NoError(t, err)
	require.Equal(t, chunks, loaded)
	_, err = service.LoadResponseChunksBounded(ctx, req, int64(len(encoded)-1))
	require.ErrorIs(t, err, ErrDataTooLarge)
}
