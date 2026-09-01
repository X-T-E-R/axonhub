package biz

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/hook"
	"github.com/looplj/axonhub/internal/ent/observabilitypayload"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func setupAsyncManagedRequestBodyTest(t *testing.T, config ManagedRequestBodyWriterConfig) (*RequestService, *ManagedRequestBodyWriter, *ent.Client, context.Context, *ent.Project) {
	t.Helper()
	legacy, client, ctx, project := setupRequestExecutionStorageTest(t)
	writer := NewManagedRequestBodyWriter(config, client, legacy.SystemService)
	service := NewRequestServiceWithManagedRequestBodyWriter(
		client,
		legacy.SystemService,
		legacy.UsageLogService,
		legacy.DataStorageService,
		legacy.LiveStreamRegistry,
		writer,
	)
	require.NoError(t, writer.Start(ctx))
	return service, writer, client, contexts.WithProjectID(ctx, project.ID), project
}

func stopManagedRequestBodyWriter(t *testing.T, writer *ManagedRequestBodyWriter) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, writer.Stop(ctx))
}

func TestManagedRequestBodyWriterExactDedupChargesOnce(t *testing.T) {
	service, writer, client, ctx, _ := setupAsyncManagedRequestBodyTest(t, ManagedRequestBodyWriterConfig{})
	defer client.Close()
	body := []byte(`{"model":"gpt-4o","prompt":"same bytes"}`)
	channel := createStorageTestChannel(t, ctx, client, nil)

	parent, err := service.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	execution, err := service.CreateRequestExecution(ctx, channel, "gpt-4o", parent, httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion, false)
	require.NoError(t, err)
	stopManagedRequestBodyWriter(t, writer)

	parent = client.Request.GetX(ctx, parent.ID)
	execution = client.RequestExecution.GetX(ctx, execution.ID)
	require.NotNil(t, parent.RequestBodyPayloadID)
	require.NotNil(t, execution.RequestBodyPayloadID)
	require.Equal(t, *parent.RequestBodyPayloadID, *execution.RequestBodyPayloadID)
	payloads := client.ObservabilityPayload.Query().Where(observabilitypayload.RequestIDEQ(parent.ID)).AllX(ctx)
	require.Len(t, payloads, 1)
	require.Equal(t, payloads[0].ChargedBytes, client.ManagedObservabilityState.GetX(ctx, 1).ChargedBytes)
}

func TestManagedRequestBodyWriterAttachUncertaintyReusesCharge(t *testing.T) {
	service, writer, client, ctx, _ := setupAsyncManagedRequestBodyTest(t, ManagedRequestBodyWriterConfig{MaxAttempts: 3})
	defer client.Close()
	var injected atomic.Bool
	client.Request.Use(func(next ent.Mutator) ent.Mutator {
		return hook.RequestFunc(func(hookCtx context.Context, mutation *ent.RequestMutation) (ent.Value, error) {
			if _, settingPointer := mutation.RequestBodyPayloadID(); settingPointer && injected.CompareAndSwap(false, true) {
				value, err := next.Mutate(hookCtx, mutation)
				if err != nil {
					return value, err
				}
				return value, errors.New("simulated uncertain attach result")
			}
			return next.Mutate(hookCtx, mutation)
		})
	})

	parent, err := service.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: []byte(`{"prompt":"uncertain"}`)}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	stopManagedRequestBodyWriter(t, writer)

	parent = client.Request.GetX(ctx, parent.ID)
	require.True(t, injected.Load())
	require.NotNil(t, parent.RequestBodyPayloadID)
	payloads := client.ObservabilityPayload.Query().Where(observabilitypayload.RequestIDEQ(parent.ID)).AllX(ctx)
	require.Len(t, payloads, 1)
	require.Equal(t, payloads[0].ChargedBytes, client.ManagedObservabilityState.GetX(ctx, 1).ChargedBytes)
}

func TestManagedRequestBodyWriterPolicyCapturedAtAdmissionAndChannelPrecedence(t *testing.T) {
	service, writer, client, ctx, _ := setupAsyncManagedRequestBodyTest(t, ManagedRequestBodyWriterConfig{})
	defer client.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce atomic.Bool
	writer.SetBeforePersistHookForTest(func(context.Context) {
		if enteredOnce.CompareAndSwap(false, true) {
			close(entered)
		}
		<-release
	})
	body := []byte(`{"prompt":"captured while enabled"}`)
	accepted, err := service.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	<-entered
	require.NoError(t, service.SystemService.SetStoragePolicy(ctx, &StoragePolicy{
		StoreRequestBody:          false,
		StoreExecutionRequestBody: lo.ToPtr(false),
		StoreResponseBody:         true,
	}))
	omitted, err := service.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: []byte(`{"prompt":"after off"}`)}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	require.Equal(t, "omit", omitted.EvidenceDisposition.RequestBody.Intent)

	channelEnabled := createStorageTestChannel(t, ctx, client, &objects.ChannelSettings{StoreExecutionRequestBody: lo.ToPtr(true)})
	executionEnabled, err := service.CreateRequestExecution(ctx, channelEnabled, "gpt-4o", accepted, httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion, false)
	require.NoError(t, err)
	channelEnabled.Settings = &objects.ChannelSettings{StoreExecutionRequestBody: lo.ToPtr(false)}
	executionDisabled, err := service.CreateRequestExecution(ctx, channelEnabled, "gpt-4o", accepted, httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion, false)
	require.NoError(t, err)
	require.Equal(t, "omit", executionDisabled.EvidenceDisposition.RequestBody.Intent)

	close(release)
	stopManagedRequestBodyWriter(t, writer)
	require.NotNil(t, client.Request.GetX(ctx, accepted.ID).RequestBodyPayloadID, "accepted work drains after policy is disabled")
	require.Nil(t, client.Request.GetX(ctx, omitted.ID).RequestBodyPayloadID)
	require.NotNil(t, client.RequestExecution.GetX(ctx, executionEnabled.ID).RequestBodyPayloadID, "channel enable overrides global disable")
	require.Nil(t, client.RequestExecution.GetX(ctx, executionDisabled.ID).RequestBodyPayloadID, "channel disable overrides inherited policy")
}

func TestManagedRequestBodyWriterBackpressureAndSkeletonFailureRelease(t *testing.T) {
	service, writer, client, ctx, _ := setupAsyncManagedRequestBodyTest(t, ManagedRequestBodyWriterConfig{MaxItems: 2, MaxBytesMiB: 1})
	defer client.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce atomic.Bool
	writer.SetBeforePersistHookForTest(func(context.Context) {
		if enteredOnce.CompareAndSwap(false, true) {
			close(entered)
		}
		<-release
	})
	body := []byte(`{"blob":"` + strings.Repeat("a", 700<<10) + `"}`)
	accepted, err := service.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	require.Equal(t, managedRequestBodyAsyncPending, *accepted.EvidenceDisposition.RequestBody.FailureClass)
	<-entered

	capacity, err := service.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	require.Equal(t, "omitted", capacity.EvidenceDisposition.RequestBody.Outcome)
	require.Equal(t, managedRequestBodyRejectCapacity, *capacity.EvidenceDisposition.RequestBody.FailureClass)
	oversized, err := service.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: []byte(`{"blob":"` + strings.Repeat("b", 1<<20) + `"}`)}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	require.Equal(t, managedRequestBodyRejectItemTooLarge, *oversized.EvidenceDisposition.RequestBody.FailureClass)

	close(release)
	stopManagedRequestBodyWriter(t, writer)
}

func TestManagedRequestBodyWriterItemBackpressureIsNonblocking(t *testing.T) {
	legacy, client, ctx, _ := setupRequestExecutionStorageTest(t)
	defer client.Close()
	writer := NewManagedRequestBodyWriter(ManagedRequestBodyWriterConfig{MaxItems: 1, MaxBytesMiB: 64}, client, legacy.SystemService)
	require.NoError(t, writer.Start(ctx))
	first, rejection := writer.reserve([]byte(`{"first":true}`))
	require.NotNil(t, first)
	require.Empty(t, rejection)
	started := time.Now()
	second, rejection := writer.reserve([]byte(`{"second":true}`))
	require.Nil(t, second)
	require.Equal(t, managedRequestBodyRejectCapacity, rejection)
	require.Less(t, time.Since(started), 100*time.Millisecond)
	first.release()
	stopManagedRequestBodyWriter(t, writer)
}

func TestManagedRequestBodyWriterReleasesExactReservedBytes(t *testing.T) {
	legacy, client, ctx, _ := setupRequestExecutionStorageTest(t)
	defer client.Close()
	writer := NewManagedRequestBodyWriter(ManagedRequestBodyWriterConfig{MaxItems: 3, MaxBytesMiB: 1}, client, legacy.SystemService)
	require.NoError(t, writer.Start(ctx))
	large, rejection := writer.reserve(make([]byte, 600<<10))
	require.NotNil(t, large)
	require.Empty(t, rejection)
	small, rejection := writer.reserve(make([]byte, 300<<10))
	require.NotNil(t, small)
	require.Empty(t, rejection)
	large.release()
	replacement, rejection := writer.reserve(make([]byte, 700<<10))
	require.NotNil(t, replacement)
	require.Empty(t, rejection)
	small.release()
	replacement.release()
	stopManagedRequestBodyWriter(t, writer)
}

func TestManagedRequestBodyWriterClonesAcceptedBody(t *testing.T) {
	service, writer, client, ctx, _ := setupAsyncManagedRequestBodyTest(t, ManagedRequestBodyWriterConfig{})
	defer client.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once atomic.Bool
	writer.SetBeforePersistHookForTest(func(context.Context) {
		if once.CompareAndSwap(false, true) {
			close(entered)
		}
		<-release
	})
	body := []byte(`{"prompt":"immutable reservation"}`)
	expected := append([]byte(nil), body...)
	row, err := service.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	<-entered
	clear(body)
	close(release)
	stopManagedRequestBodyWriter(t, writer)
	row = client.Request.GetX(ctx, row.ID)
	require.NotNil(t, row.RequestBodyPayloadID)
	payload := client.ObservabilityPayload.GetX(ctx, *row.RequestBodyPayloadID)
	require.Equal(t, expected, payload.Data)
}

func TestManagedRequestBodyWriterDropsDeletedTarget(t *testing.T) {
	service, writer, client, ctx, _ := setupAsyncManagedRequestBodyTest(t, ManagedRequestBodyWriterConfig{})
	defer client.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once atomic.Bool
	writer.SetBeforePersistHookForTest(func(context.Context) {
		if once.CompareAndSwap(false, true) {
			close(entered)
		}
		<-release
	})
	row, err := service.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: []byte(`{"prompt":"deleted"}`)}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	<-entered
	require.NoError(t, client.Request.DeleteOneID(row.ID).Exec(ctx))
	close(release)
	stopManagedRequestBodyWriter(t, writer)
	require.Zero(t, client.ObservabilityPayload.Query().CountX(ctx))
}

func TestManagedRequestBodyWriterReservationReleasedWhenSkeletonInsertFails(t *testing.T) {
	service, writer, client, ctx, _ := setupAsyncManagedRequestBodyTest(t, ManagedRequestBodyWriterConfig{})
	defer client.Close()
	client.Request.Use(func(next ent.Mutator) ent.Mutator {
		return hook.RequestFunc(func(hookCtx context.Context, mutation *ent.RequestMutation) (ent.Value, error) {
			if mutation.Op().Is(ent.OpCreate) {
				return nil, errors.New("simulated skeleton insert failure")
			}
			return next.Mutate(hookCtx, mutation)
		})
	})

	_, err := service.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: []byte(`{"prompt":"must fail"}`)}, llm.APIFormatOpenAIChatCompletion)
	require.ErrorContains(t, err, "simulated skeleton insert failure")
	writer.mu.Lock()
	require.Zero(t, writer.reservedItems)
	require.Zero(t, writer.reservedBytes)
	writer.mu.Unlock()
	stopManagedRequestBodyWriter(t, writer)
}

func TestManagedRequestBodyWriterPersistentPayloadFailureDoesNotFailSkeleton(t *testing.T) {
	service, writer, client, ctx, _ := setupAsyncManagedRequestBodyTest(t, ManagedRequestBodyWriterConfig{MaxAttempts: 2})
	defer client.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once atomic.Bool
	writer.SetBeforePersistHookForTest(func(context.Context) {
		if once.CompareAndSwap(false, true) {
			close(entered)
		}
		<-release
	})
	client.ObservabilityPayload.Use(func(ent.Mutator) ent.Mutator {
		return hook.ObservabilityPayloadFunc(func(context.Context, *ent.ObservabilityPayloadMutation) (ent.Value, error) {
			return nil, errors.New("persistent payload failure")
		})
	})

	row, err := service.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: []byte(`{"prompt":"still dispatchable"}`)}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	<-entered
	channel := createStorageTestChannel(t, ctx, client, nil)
	execution, err := service.CreateRequestExecution(ctx, channel, "gpt-4o", row, httpclient.Request{JSONBody: []byte(`{"prompt":"still dispatchable"}`)}, llm.APIFormatOpenAIChatCompletion, false)
	require.NoError(t, err)
	require.NoError(t, service.UpdateRequestCompleted(ctx, row.ID, "response-parent", map[string]string{"answer": "parent"}, nil))
	require.NoError(t, service.UpdateRequestExecutionCompleted(ctx, execution.ID, "response-execution", map[string]string{"answer": "execution"}, nil))
	close(release)
	stopManagedRequestBodyWriter(t, writer)
	row = client.Request.GetX(ctx, row.ID)
	execution = client.RequestExecution.GetX(ctx, execution.ID)
	require.Nil(t, row.RequestBodyPayloadID)
	require.Nil(t, execution.RequestBodyPayloadID)
	for _, disposition := range []*objects.EvidenceDisposition{row.EvidenceDisposition, execution.EvidenceDisposition} {
		require.Equal(t, "unavailable", disposition.RequestBody.Outcome)
		require.Equal(t, managedRequestBodyRetryExhausted, *disposition.RequestBody.FailureClass)
		require.Equal(t, "stored", disposition.ResponseBody.Outcome, "field-specific terminal merge preserves response evidence")
	}
}

func TestManagedRequestBodyWriterCapacitySkipIsTerminalForParentAndExecution(t *testing.T) {
	service, writer, client, ctx, _ := setupAsyncManagedRequestBodyTest(t, ManagedRequestBodyWriterConfig{})
	defer client.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once atomic.Bool
	writer.SetBeforePersistHookForTest(func(context.Context) {
		if once.CompareAndSwap(false, true) {
			close(entered)
		}
		<-release
	})
	body := []byte(`{"prompt":"capacity changes after admission"}`)
	row, err := service.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	<-entered
	channel := createStorageTestChannel(t, ctx, client, nil)
	execution, err := service.CreateRequestExecution(ctx, channel, "gpt-4o", row, httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion, false)
	require.NoError(t, err)
	require.NoError(t, service.SystemService.SetStoragePolicy(ctx, &StoragePolicy{
		StoreRequestBody:            true,
		StoreExecutionRequestBody:   lo.ToPtr(true),
		StoreResponseBody:           true,
		ManagedObservabilityHardMiB: lo.ToPtr(2),
		ManagedObservabilityLowMiB:  lo.ToPtr(1),
	}))
	close(release)
	stopManagedRequestBodyWriter(t, writer)
	row = client.Request.GetX(ctx, row.ID)
	execution = client.RequestExecution.GetX(ctx, execution.ID)
	for _, disposition := range []*objects.EvidenceDisposition{row.EvidenceDisposition, execution.EvidenceDisposition} {
		require.Equal(t, "omitted", disposition.RequestBody.Outcome)
		require.Equal(t, managedRequestBodyCapacityPressure, *disposition.RequestBody.FailureClass)
	}
}

func TestManagedRequestBodyWriterShutdownDeadlineAndIdempotency(t *testing.T) {
	service, writer, client, ctx, _ := setupAsyncManagedRequestBodyTest(t, ManagedRequestBodyWriterConfig{})
	defer client.Close()
	entered := make(chan struct{})
	var once atomic.Bool
	writer.SetBeforePersistHookForTest(func(workerCtx context.Context) {
		if once.CompareAndSwap(false, true) {
			close(entered)
		}
		<-workerCtx.Done()
	})
	row, err := service.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: []byte(`{"prompt":"deadline"}`)}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	<-entered
	queued, err := service.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: []byte(`{"prompt":"queued at deadline"}`)}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)

	stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, writer.Stop(stopCtx), context.DeadlineExceeded)
	require.NoError(t, writer.Stop(context.Background()))
	reservation, rejection := writer.reserve([]byte(`{}`))
	require.Nil(t, reservation)
	require.Equal(t, managedRequestBodyRejectStopping, rejection)
	writer.mu.Lock()
	require.Zero(t, writer.reservedItems)
	require.Zero(t, writer.reservedBytes)
	writer.mu.Unlock()
	stoppingRow, err := service.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: []byte(`{"prompt":"after stop"}`)}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	require.Equal(t, "omitted", stoppingRow.EvidenceDisposition.RequestBody.Outcome)
	require.Equal(t, managedRequestBodyRejectStopping, *stoppingRow.EvidenceDisposition.RequestBody.FailureClass)
	row = client.Request.GetX(ctx, row.ID)
	require.Nil(t, row.RequestBodyPayloadID)
	require.Equal(t, "unavailable", row.EvidenceDisposition.RequestBody.Outcome)
	require.Nil(t, client.Request.GetX(ctx, queued.ID).RequestBodyPayloadID)
}

func TestManagedRequestBodyWriterRejectsBeforeStart(t *testing.T) {
	legacy, client, ctx, _ := setupRequestExecutionStorageTest(t)
	defer client.Close()
	writer := NewManagedRequestBodyWriter(ManagedRequestBodyWriterConfig{}, client, legacy.SystemService)
	reservation, rejection := writer.reserve([]byte(`{}`))
	require.Nil(t, reservation)
	require.Equal(t, managedRequestBodyRejectNotStarted, rejection)
	require.NoError(t, writer.Stop(ctx))
}

func TestManagedRequestBodyWriterLateParentSubmitAfterStopDeadline(t *testing.T) {
	service, writer, client, ctx, _ := setupAsyncManagedRequestBodyTest(t, ManagedRequestBodyWriterConfig{})
	defer client.Close()
	var captured *managedRequestBodyReservation
	client.Request.Use(func(next ent.Mutator) ent.Mutator {
		return hook.RequestFunc(func(hookCtx context.Context, mutation *ent.RequestMutation) (ent.Value, error) {
			if mutation.Op().Is(ent.OpCreate) && captured == nil {
				writer.mu.Lock()
				for reservation := range writer.reservations {
					captured = reservation
					break
				}
				writer.mu.Unlock()
				require.NotNil(t, captured)
				stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
				defer cancel()
				require.ErrorIs(t, writer.Stop(stopCtx), context.DeadlineExceeded)
			}
			return next.Mutate(hookCtx, mutation)
		})
	})

	row, err := service.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: []byte(`{"prompt":"late parent"}`)}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	require.NoError(t, writer.Stop(context.Background()))
	row = client.Request.GetX(ctx, row.ID)
	require.Equal(t, "unavailable", row.EvidenceDisposition.RequestBody.Outcome)
	require.Equal(t, managedRequestBodyStopped, *row.EvidenceDisposition.RequestBody.FailureClass)
	require.Empty(t, captured.body)
	require.Equal(t, managedRequestBodyReservationCanceled, captured.state)
	requireManagedRequestBodyWriterEmpty(t, writer)
}

func TestManagedRequestBodyWriterLateExecutionSubmitAfterStopDeadline(t *testing.T) {
	service, writer, client, ctx, _ := setupAsyncManagedRequestBodyTest(t, ManagedRequestBodyWriterConfig{})
	defer client.Close()
	require.NoError(t, service.SystemService.SetStoragePolicy(ctx, &StoragePolicy{
		StoreRequestBody: false, StoreExecutionRequestBody: lo.ToPtr(false), StoreResponseBody: true,
	}))
	parent, err := service.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: []byte(`{"prompt":"parent omitted"}`)}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	channel := createStorageTestChannel(t, ctx, client, &objects.ChannelSettings{StoreExecutionRequestBody: lo.ToPtr(true)})
	var captured *managedRequestBodyReservation
	client.RequestExecution.Use(func(next ent.Mutator) ent.Mutator {
		return hook.RequestExecutionFunc(func(hookCtx context.Context, mutation *ent.RequestExecutionMutation) (ent.Value, error) {
			if mutation.Op().Is(ent.OpCreate) && captured == nil {
				writer.mu.Lock()
				for reservation := range writer.reservations {
					captured = reservation
					break
				}
				writer.mu.Unlock()
				require.NotNil(t, captured)
				stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
				defer cancel()
				require.ErrorIs(t, writer.Stop(stopCtx), context.DeadlineExceeded)
			}
			return next.Mutate(hookCtx, mutation)
		})
	})

	execution, err := service.CreateRequestExecution(ctx, channel, "gpt-4o", parent, httpclient.Request{JSONBody: []byte(`{"prompt":"late execution"}`)}, llm.APIFormatOpenAIChatCompletion, false)
	require.NoError(t, err)
	require.NoError(t, writer.Stop(context.Background()))
	execution = client.RequestExecution.GetX(ctx, execution.ID)
	require.Equal(t, "unavailable", execution.EvidenceDisposition.RequestBody.Outcome)
	require.Equal(t, managedRequestBodyStopped, *execution.EvidenceDisposition.RequestBody.FailureClass)
	require.Empty(t, captured.body)
	require.Equal(t, managedRequestBodyReservationCanceled, captured.state)
	requireManagedRequestBodyWriterEmpty(t, writer)
}

func requireManagedRequestBodyWriterEmpty(t *testing.T, writer *ManagedRequestBodyWriter) {
	t.Helper()
	writer.mu.Lock()
	defer writer.mu.Unlock()
	require.Zero(t, writer.reservedItems)
	require.Zero(t, writer.reservedBytes)
	require.Empty(t, writer.reservations)
	require.Zero(t, len(writer.jobs))
}

func TestManagedTerminalDispositionModifierUsesDialectJSONPatch(t *testing.T) {
	capturedAt := time.Date(2026, time.September, 1, 1, 2, 3, 4, time.UTC)
	for _, testCase := range []struct {
		dialect string
		marker  string
	}{
		{dialect: dialect.Postgres, marker: "jsonb_set(jsonb_set(jsonb_set(jsonb_set("},
		{dialect: dialect.MySQL, marker: "JSON_SET("},
		{dialect: dialect.SQLite, marker: "json_set("},
	} {
		t.Run(testCase.dialect, func(t *testing.T) {
			update := entsql.Dialect(testCase.dialect).Update("requests")
			managedTerminalDispositionModifier("evidence_disposition", "unavailable", managedRequestBodyRetryExhausted, capturedAt)(update)
			query, args := update.Query()
			require.Contains(t, query, testCase.marker)
			require.Len(t, args, 4)
		})
	}
}
