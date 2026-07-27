package biz

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
)

func createTerminalStatusTestRecords(
	t *testing.T,
	ctx context.Context,
	client *ent.Client,
) (*ent.Request, *ent.RequestExecution) {
	t.Helper()

	proj, err := client.Project.Create().
		SetName(t.Name()).
		SetStatus(project.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	req, err := client.Request.Create().
		SetProjectID(proj.ID).
		SetModelID("gpt-4.1").
		SetFormat("openai/chat_completions").
		SetRequestBody([]byte(`{"model":"gpt-4.1"}`)).
		SetStatus(request.StatusProcessing).
		SetStream(true).
		Save(ctx)
	require.NoError(t, err)

	execution, err := client.RequestExecution.Create().
		SetProjectID(proj.ID).
		SetRequestID(req.ID).
		SetModelID("gpt-4.1").
		SetFormat("openai/chat_completions").
		SetRequestBody([]byte(`{"model":"gpt-4.1"}`)).
		SetStatus(requestexecution.StatusProcessing).
		SetStream(true).
		Save(ctx)
	require.NoError(t, err)

	return req, execution
}

func TestRequestService_TerminalStatusClassification(t *testing.T) {
	tests := []struct {
		name              string
		rawErr            error
		requestContextErr error
		want              requestexecution.Status
	}{
		{
			name:              "genuine caller cancellation",
			rawErr:            context.Canceled,
			requestContextErr: context.Canceled,
			want:              requestexecution.StatusCanceled,
		},
		{
			name:              "retry cleanup cancellation without caller cancellation",
			rawErr:            context.Canceled,
			requestContextErr: nil,
			want:              requestexecution.StatusFailed,
		},
		{
			name:              "server shutdown cancellation cause",
			rawErr:            context.Canceled,
			requestContextErr: errors.New("server shutting down"),
			want:              requestexecution.StatusFailed,
		},
		{
			name:              "provider failure wins over later caller context cancellation",
			rawErr:            errors.New("provider connection reset"),
			requestContextErr: context.Canceled,
			want:              requestexecution.StatusFailed,
		},
		{
			name:              "internal deadline is failure",
			rawErr:            context.DeadlineExceeded,
			requestContextErr: context.DeadlineExceeded,
			want:              requestexecution.StatusFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, client, ctx := setupTestRequestService(t)
			defer client.Close()

			req, execution := createTerminalStatusTestRecords(t, ctx, client)
			statusCode := http.StatusBadGateway
			require.NoError(t, svc.UpdateRequestExecutionStatusFromErrorDetails(
				ctx,
				execution.ID,
				tt.rawErr,
				tt.requestContextErr,
				tt.rawErr.Error(),
				&ExecutionErrorInfo{StatusCode: &statusCode},
			))
			require.NoError(t, svc.UpdateRequestStatusFromError(ctx, req.ID, tt.rawErr, tt.requestContextErr))

			execution, err := client.RequestExecution.Get(ctx, execution.ID)
			require.NoError(t, err)
			require.Equal(t, tt.want, execution.Status)
			require.Equal(t, tt.rawErr.Error(), execution.ErrorMessage)
			require.NotNil(t, execution.ResponseStatusCode)
			require.Equal(t, statusCode, *execution.ResponseStatusCode)

			req, err = client.Request.Get(ctx, req.ID)
			require.NoError(t, err)
			if tt.want == requestexecution.StatusCanceled {
				require.Equal(t, request.StatusCanceled, req.Status)
			} else {
				require.Equal(t, request.StatusFailed, req.Status)
			}
		})
	}
}

func TestRequestService_TerminalStatusPreservesFirstCause(t *testing.T) {
	t.Run("failure is not overwritten by cleanup cancellation", func(t *testing.T) {
		svc, client, ctx := setupTestRequestService(t)
		defer client.Close()

		req, execution := createTerminalStatusTestRecords(t, ctx, client)
		statusCode := http.StatusBadGateway
		providerErr := errors.New("upstream returned malformed stream")

		require.NoError(t, svc.UpdateRequestExecutionStatusFromErrorDetails(
			ctx,
			execution.ID,
			providerErr,
			nil,
			"provider detail",
			&ExecutionErrorInfo{StatusCode: &statusCode},
		))
		require.NoError(t, svc.UpdateRequestStatusFromError(ctx, req.ID, providerErr, nil))

		require.NoError(t, svc.UpdateRequestExecutionStatusFromError(
			ctx,
			execution.ID,
			context.Canceled,
			context.Canceled,
		))
		require.NoError(t, svc.UpdateRequestStatusFromError(
			ctx,
			req.ID,
			context.Canceled,
			context.Canceled,
		))

		execution, err := client.RequestExecution.Get(ctx, execution.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusFailed, execution.Status)
		require.Equal(t, "provider detail", execution.ErrorMessage)
		require.NotNil(t, execution.ResponseStatusCode)
		require.Equal(t, statusCode, *execution.ResponseStatusCode)

		req, err = client.Request.Get(ctx, req.ID)
		require.NoError(t, err)
		require.Equal(t, request.StatusFailed, req.Status)
	})

	t.Run("genuine cancellation is not overwritten by later failure", func(t *testing.T) {
		svc, client, ctx := setupTestRequestService(t)
		defer client.Close()

		req, execution := createTerminalStatusTestRecords(t, ctx, client)
		require.NoError(t, svc.UpdateRequestExecutionStatusFromError(
			ctx,
			execution.ID,
			context.Canceled,
			context.Canceled,
		))
		require.NoError(t, svc.UpdateRequestStatusFromError(
			ctx,
			req.ID,
			context.Canceled,
			context.Canceled,
		))

		providerErr := errors.New("late cleanup failure")
		require.NoError(t, svc.UpdateRequestExecutionStatusFromError(ctx, execution.ID, providerErr, nil))
		require.NoError(t, svc.UpdateRequestStatusFromError(ctx, req.ID, providerErr, nil))

		execution, err := client.RequestExecution.Get(ctx, execution.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusCanceled, execution.Status)
		require.Equal(t, context.Canceled.Error(), execution.ErrorMessage)

		req, err = client.Request.Get(ctx, req.ID)
		require.NoError(t, err)
		require.Equal(t, request.StatusCanceled, req.Status)
	})

	t.Run("causal failure supersedes provisional completion", func(t *testing.T) {
		svc, client, ctx := setupTestRequestService(t)
		defer client.Close()
		require.NoError(t, svc.SystemService.SetStoragePolicy(ctx, &StoragePolicy{
			StoreResponseBody: true,
		}))

		req, execution := createTerminalStatusTestRecords(t, ctx, client)
		require.NoError(t, svc.UpdateRequestExecutionCompleted(
			ctx,
			execution.ID,
			"provider-response",
			map[string]string{"output": "provisional"},
			nil,
		))
		require.NoError(t, svc.UpdateRequestCompleted(
			ctx,
			req.ID,
			"client-response",
			map[string]string{"output": "provisional"},
			nil,
		))

		transformErr := errors.New("failed to transform final response")
		require.NoError(t, svc.UpdateRequestExecutionStatusFromError(ctx, execution.ID, transformErr, nil))
		require.NoError(t, svc.UpdateRequestStatusFromError(ctx, req.ID, transformErr, nil))

		execution, err := client.RequestExecution.Get(ctx, execution.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusFailed, execution.Status)
		require.Equal(t, transformErr.Error(), execution.ErrorMessage)
		require.JSONEq(t, `{"output":"provisional"}`, string(execution.ResponseBody))
		ordinaryExecutionBody, err := svc.LoadRequestExecutionResponseBody(ctx, execution)
		require.NoError(t, err)
		require.JSONEq(t, `{}`, string(ordinaryExecutionBody))
		executionEvidence, err := svc.LoadRequestExecutionResponseBodyEvidenceBounded(ctx, execution, 1024)
		require.NoError(t, err)
		require.JSONEq(t, `{"output":"provisional"}`, string(executionEvidence))

		req, err = client.Request.Get(ctx, req.ID)
		require.NoError(t, err)
		require.Equal(t, request.StatusFailed, req.Status)
		require.JSONEq(t, `{"output":"provisional"}`, string(req.ResponseBody))
		ordinaryRequestBody, err := svc.LoadResponseBody(ctx, req)
		require.NoError(t, err)
		require.JSONEq(t, `{}`, string(ordinaryRequestBody))
		requestEvidence, err := svc.LoadResponseBodyEvidenceBounded(ctx, req, 1024)
		require.NoError(t, err)
		require.JSONEq(t, `{"output":"provisional"}`, string(requestEvidence))
	})

	t.Run("late completion cannot overwrite causal failure", func(t *testing.T) {
		svc, client, ctx := setupTestRequestService(t)
		defer client.Close()

		req, execution := createTerminalStatusTestRecords(t, ctx, client)
		transformErr := errors.New("failed to transform provider response")
		require.NoError(t, svc.UpdateRequestExecutionStatusFromError(ctx, execution.ID, transformErr, nil))
		require.NoError(t, svc.UpdateRequestStatusFromError(ctx, req.ID, transformErr, nil))

		require.NoError(t, svc.UpdateRequestExecutionCompleted(
			ctx,
			execution.ID,
			"late-provider-response",
			map[string]string{"output": "late"},
			nil,
		))
		require.NoError(t, svc.UpdateRequestCompleted(
			ctx,
			req.ID,
			"late-client-response",
			map[string]string{"output": "late"},
			nil,
		))

		execution, err := client.RequestExecution.Get(ctx, execution.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusFailed, execution.Status)
		require.Equal(t, transformErr.Error(), execution.ErrorMessage)
		require.Empty(t, execution.ExternalID)
		require.Empty(t, execution.ResponseBody)

		req, err = client.Request.Get(ctx, req.ID)
		require.NoError(t, err)
		require.Equal(t, request.StatusFailed, req.Status)
		require.Empty(t, req.ExternalID)
		require.Empty(t, req.ResponseBody)
	})

	t.Run("cleanup cancellation cannot downgrade completion", func(t *testing.T) {
		svc, client, ctx := setupTestRequestService(t)
		defer client.Close()

		req, execution := createTerminalStatusTestRecords(t, ctx, client)
		require.NoError(t, svc.UpdateRequestExecutionCompleted(
			ctx,
			execution.ID,
			"provider-response",
			map[string]string{"output": "done"},
			nil,
		))
		require.NoError(t, svc.UpdateRequestCompleted(
			ctx,
			req.ID,
			"client-response",
			map[string]string{"output": "done"},
			nil,
		))

		require.NoError(t, svc.UpdateRequestExecutionStatusFromError(
			ctx,
			execution.ID,
			context.Canceled,
			nil,
		))
		require.NoError(t, svc.UpdateRequestStatusFromError(
			ctx,
			req.ID,
			context.Canceled,
			nil,
		))

		execution, err := client.RequestExecution.Get(ctx, execution.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusCompleted, execution.Status)
		require.Equal(t, "provider-response", execution.ExternalID)

		req, err = client.Request.Get(ctx, req.ID)
		require.NoError(t, err)
		require.Equal(t, request.StatusCompleted, req.Status)
		require.Equal(t, "client-response", req.ExternalID)
	})

	t.Run("specific failure refines weak cleanup cancellation", func(t *testing.T) {
		svc, client, ctx := setupTestRequestService(t)
		defer client.Close()

		_, execution := createTerminalStatusTestRecords(t, ctx, client)
		require.NoError(t, svc.UpdateRequestExecutionStatusFromError(
			ctx,
			execution.ID,
			context.Canceled,
			nil,
		))

		timeoutErr := errors.New("stream first event timeout")
		require.NoError(t, svc.UpdateRequestExecutionStatusFromError(ctx, execution.ID, timeoutErr, nil))

		execution, err := client.RequestExecution.Get(ctx, execution.ID)
		require.NoError(t, err)
		require.Equal(t, requestexecution.StatusFailed, execution.Status)
		require.Equal(t, timeoutErr.Error(), execution.ErrorMessage)
	})
}
