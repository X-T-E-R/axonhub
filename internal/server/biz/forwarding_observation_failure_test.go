package biz

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/hook"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestForwardingObservationCoreInsertTerminalFailureAndCancellation(t *testing.T) {
	for _, testCase := range []struct {
		name            string
		executionState  requestexecution.Status
		requestState    request.Status
		updateExecution func(context.Context, *RequestService, int) error
		updateRequest   func(context.Context, *RequestService, int) error
	}{
		{
			name:           "failure",
			executionState: requestexecution.StatusFailed,
			requestState:   request.StatusFailed,
			updateExecution: func(ctx context.Context, svc *RequestService, id int) error {
				return svc.UpdateRequestExecutionStatusFromError(ctx, id, errors.New("provider failed"), context.DeadlineExceeded)
			},
			updateRequest: func(ctx context.Context, svc *RequestService, id int) error {
				return svc.UpdateRequestStatusFromError(ctx, id, errors.New("provider failed"), context.DeadlineExceeded)
			},
		},
		{
			name:           "cancellation",
			executionState: requestexecution.StatusCanceled,
			requestState:   request.StatusCanceled,
			updateExecution: func(ctx context.Context, svc *RequestService, id int) error {
				return svc.UpdateRequestExecutionStatusFromError(ctx, id, context.Canceled, context.Canceled)
			},
			updateRequest: func(ctx context.Context, svc *RequestService, id int) error {
				return svc.UpdateRequestStatusFromError(ctx, id, context.Canceled, context.Canceled)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			legacy, client, baseCtx, project := setupRequestExecutionStorageTest(t)
			defer client.Close()

			writer := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{
				MaxItems:       8,
				MaxBytesMiB:    1,
				AttemptTimeout: time.Second,
			})
			svc := NewRequestServiceWithObservationWriter(
				client,
				legacy.SystemService,
				legacy.UsageLogService,
				legacy.DataStorageService,
				legacy.LiveStreamRegistry,
				nil,
				writer,
			)
			require.NoError(t, writer.Start(baseCtx))
			scopedCtx := writer.WithScope(contexts.WithProjectID(baseCtx, project.ID))
			var stopped atomic.Bool
			t.Cleanup(func() {
				EndForwardingObservation(scopedCtx)
				if !stopped.Load() {
					stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = writer.Stop(stopCtx)
				}
			})

			channel := createStorageTestChannel(t, baseCtx, client, nil)
			entered := make(chan struct{})
			release := make(chan struct{})
			var enteredOnce sync.Once
			var releaseOnce sync.Once
			client.Request.Use(func(next ent.Mutator) ent.Mutator {
				return hook.RequestFunc(func(hookCtx context.Context, mutation *ent.RequestMutation) (ent.Value, error) {
					if mutation.Op().Is(ent.OpCreate) {
						enteredOnce.Do(func() { close(entered) })
						select {
						case <-release:
						case <-hookCtx.Done():
							return nil, hookCtx.Err()
						}
					}
					return next.Mutate(hookCtx, mutation)
				})
			})
			releaseBarrier := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(releaseBarrier)

			parent, err := svc.CreateRequest(
				scopedCtx,
				&llm.Request{Model: "gpt-4o"},
				&httpclient.Request{JSONBody: []byte(`{"model":"gpt-4o"}`)},
				llm.APIFormatOpenAIChatCompletion,
			)
			require.NoError(t, err)
			require.Less(t, parent.ID, 0)
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("request INSERT barrier was not entered")
			}

			execution, err := svc.CreateRequestExecution(
				scopedCtx,
				channel,
				"gpt-4o",
				parent,
				httpclient.Request{JSONBody: []byte(`{"model":"gpt-4o"}`)},
				llm.APIFormatOpenAIChatCompletion,
				false,
			)
			require.NoError(t, err)
			require.Less(t, execution.ID, 0)
			require.NoError(t, testCase.updateExecution(scopedCtx, svc, execution.ID))
			require.NoError(t, testCase.updateRequest(scopedCtx, svc, parent.ID))

			releaseBarrier()
			EndForwardingObservation(scopedCtx)
			drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			require.NoError(t, writer.Wait(drainCtx))
			cancel()

			persistedParent := client.Request.Query().OnlyX(baseCtx)
			persistedExecution := client.RequestExecution.Query().OnlyX(baseCtx)
			require.Equal(t, testCase.requestState, persistedParent.Status)
			require.Equal(t, testCase.executionState, persistedExecution.Status)
			require.Equal(t, persistedParent.ID, persistedExecution.RequestID)

			stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			require.NoError(t, writer.Stop(stopCtx))
			stopCancel()
			stopped.Store(true)
		})
	}
}

func TestForwardingObservationTerminalRetryAssociatesUsageOnce(t *testing.T) {
	legacy, client, baseCtx, project := setupRequestExecutionStorageTest(t)
	defer client.Close()

	writer := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{
		MaxItems:       8,
		MaxBytesMiB:    1,
		AttemptTimeout: 500 * time.Millisecond,
	})
	svc := NewRequestServiceWithObservationWriter(
		client,
		legacy.SystemService,
		legacy.UsageLogService,
		legacy.DataStorageService,
		legacy.LiveStreamRegistry,
		nil,
		writer,
	)
	require.NoError(t, writer.Start(baseCtx))
	scopedCtx := writer.WithScope(contexts.WithProjectID(baseCtx, project.ID))
	var stopped atomic.Bool
	t.Cleanup(func() {
		EndForwardingObservation(scopedCtx)
		if !stopped.Load() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = writer.Stop(stopCtx)
		}
	})

	channel := createStorageTestChannel(t, baseCtx, client, nil)
	var terminalAttempts atomic.Int32
	client.RequestExecution.Use(func(next ent.Mutator) ent.Mutator {
		return hook.RequestExecutionFunc(func(hookCtx context.Context, mutation *ent.RequestExecutionMutation) (ent.Value, error) {
			status, hasStatus := mutation.Status()
			if mutation.Op().Is(ent.OpUpdateOne) && hasStatus && status == requestexecution.StatusFailed {
				if terminalAttempts.Add(1) == 1 {
					return nil, errors.New("transient execution terminal update failure")
				}
			}
			return next.Mutate(hookCtx, mutation)
		})
	})

	parent, err := svc.CreateRequest(
		scopedCtx,
		&llm.Request{Model: "gpt-4o"},
		&httpclient.Request{JSONBody: []byte(`{"model":"gpt-4o"}`)},
		llm.APIFormatOpenAIChatCompletion,
	)
	require.NoError(t, err)
	execution, err := svc.CreateRequestExecution(
		scopedCtx,
		channel,
		"gpt-4o",
		parent,
		httpclient.Request{JSONBody: []byte(`{"model":"gpt-4o"}`)},
		llm.APIFormatOpenAIChatCompletion,
		false,
	)
	require.NoError(t, err)
	require.NoError(t, svc.UpdateRequestExecutionFailed(scopedCtx, execution.ID, "provider failed", nil))
	require.NoError(t, svc.UpdateRequestCompleted(scopedCtx, parent.ID, "request-response", map[string]any{"ok": true}, nil))

	usage := &llm.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}
	_, err = svc.UsageLogService.CreateUsageLogFromRequest(scopedCtx, parent, execution, usage)
	require.NoError(t, err)
	_, err = svc.UsageLogService.CreateUsageLogFromRequest(scopedCtx, parent, execution, usage)
	require.NoError(t, err)

	EndForwardingObservation(scopedCtx)
	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	require.NoError(t, writer.Wait(drainCtx))
	cancel()

	require.Equal(t, int32(2), terminalAttempts.Load())
	persistedParent := client.Request.Query().OnlyX(baseCtx)
	persistedExecution := client.RequestExecution.Query().OnlyX(baseCtx)
	require.Equal(t, request.StatusCompleted, persistedParent.Status)
	require.Equal(t, requestexecution.StatusFailed, persistedExecution.Status)
	require.Equal(t, persistedParent.ID, persistedExecution.RequestID)
	usageLogs := client.UsageLog.Query().AllX(baseCtx)
	require.Len(t, usageLogs, 1)
	require.Equal(t, persistedParent.ID, usageLogs[0].RequestID)
	require.Equal(t, int64(5), usageLogs[0].TotalTokens)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	require.NoError(t, writer.Stop(stopCtx))
	stopCancel()
	stopped.Store(true)
}

func TestForwardingObservationTerminalReservationSurvivesQueueBytesFull(t *testing.T) {
	legacy, client, baseCtx, project := setupRequestExecutionStorageTest(t)
	defer client.Close()

	writer := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{
		MaxItems:       8,
		MaxBytesMiB:    1,
		AttemptTimeout: time.Second,
	})
	svc := NewRequestServiceWithObservationWriter(
		client,
		legacy.SystemService,
		legacy.UsageLogService,
		legacy.DataStorageService,
		legacy.LiveStreamRegistry,
		nil,
		writer,
	)
	require.NoError(t, writer.Start(baseCtx))
	scopedCtx := writer.WithScope(contexts.WithProjectID(baseCtx, project.ID))
	var stopped atomic.Bool
	t.Cleanup(func() {
		EndForwardingObservation(scopedCtx)
		if !stopped.Load() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = writer.Stop(stopCtx)
		}
	})

	channel := createStorageTestChannel(t, baseCtx, client, nil)
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	client.Request.Use(func(next ent.Mutator) ent.Mutator {
		return hook.RequestFunc(func(hookCtx context.Context, mutation *ent.RequestMutation) (ent.Value, error) {
			if mutation.Op().Is(ent.OpCreate) {
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-hookCtx.Done():
					return nil, hookCtx.Err()
				}
			}
			return next.Mutate(hookCtx, mutation)
		})
	})
	releaseBarrier := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseBarrier)

	parent, err := svc.CreateRequest(
		scopedCtx,
		&llm.Request{Model: "gpt-4o"},
		&httpclient.Request{JSONBody: []byte(`{"model":"gpt-4o"}`)},
		llm.APIFormatOpenAIChatCompletion,
	)
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request INSERT barrier was not entered")
	}

	execution, err := svc.CreateRequestExecution(
		scopedCtx,
		channel,
		"gpt-4o",
		parent,
		httpclient.Request{JSONBody: []byte(`{"model":"gpt-4o"}`)},
		llm.APIFormatOpenAIChatCompletion,
		false,
	)
	require.NoError(t, err)

	ordinaryChunk := &httpclient.StreamEvent{
		Type: "data",
		Data: []byte("\"" + strings.Repeat("q", 700<<10) + "\""),
	}
	require.NoError(t, svc.SaveRequestChunks(scopedCtx, parent.ID, []*httpclient.StreamEvent{ordinaryChunk}))
	writer.mu.Lock()
	queuedBytes := writer.bytes
	writer.mu.Unlock()
	require.Greater(t, queuedBytes, int64(700<<10), "ordinary persistence must occupy the bounded byte lane")
	largeResponse := map[string]any{"body": strings.Repeat("r", 400<<10)}
	require.NoError(t, svc.UpdateRequestExecutionCompletedForChannel(scopedCtx, execution.ID, "execution-response", largeResponse, nil, channel))
	require.NoError(t, svc.UpdateRequestCompleted(scopedCtx, parent.ID, "request-response", largeResponse, nil))

	releaseBarrier()
	EndForwardingObservation(scopedCtx)
	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	require.NoError(t, writer.Wait(drainCtx))
	cancel()

	persistedParent := client.Request.Query().OnlyX(baseCtx)
	persistedExecution := client.RequestExecution.Query().OnlyX(baseCtx)
	require.Equal(t, request.StatusCompleted, persistedParent.Status)
	require.Equal(t, requestexecution.StatusCompleted, persistedExecution.Status)
	require.Equal(t, persistedParent.ID, persistedExecution.RequestID)
	require.Equal(t, "unavailable", persistedParent.EvidenceDisposition.ResponseBody.Outcome)
	require.Equal(t, "unavailable", persistedExecution.EvidenceDisposition.ResponseBody.Outcome)
	require.Empty(t, persistedParent.ResponseBody)
	require.Empty(t, persistedExecution.ResponseBody)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	require.NoError(t, writer.Stop(stopCtx))
	stopCancel()
	stopped.Store(true)
}
