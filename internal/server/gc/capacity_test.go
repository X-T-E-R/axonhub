package gc

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	entchannel "github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/observabilitypayload"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
)

func capacityTestWorker(t *testing.T) (*Worker, *ent.Client, context.Context, *biz.SystemService, *ent.Project) {
	t.Helper()
	client := enttest.NewEntClient(t, "sqlite3", "file:"+strings.ReplaceAll(t.Name(), "/", "-")+"?mode=memory&_fk=1")
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	system := biz.NewSystemService(biz.SystemServiceParams{Ent: client})
	proj := client.Project.Create().SetName(t.Name()).SetStatus(project.StatusActive).SaveX(ctx)
	return &Worker{Ent: client, SystemService: system}, client, ctx, system, proj
}

func addCapacityPayload(t *testing.T, client *ent.Client, ctx context.Context, proj *ent.Project, status request.Status, size int) (*ent.Request, *ent.ObservabilityPayload) {
	t.Helper()
	row := client.Request.Create().
		SetProjectID(proj.ID).
		SetModelID("model").
		SetRequestBody([]byte(`{}`)).
		SetManagedObservability(true).
		SetStatus(status).
		SaveX(ctx)
	length := int64(size)
	disposition := &objects.EvidenceDisposition{Version: 1, RequestBody: objects.Disposition{
		Intent: "persist", Location: "managed", Outcome: "stored", SHA256: strings.Repeat("a", 64), ByteLength: &length,
	}}
	payload := client.ObservabilityPayload.Create().
		SetRequestID(row.ID).
		SetKind(observabilitypayload.KindRequestBody).
		SetSha256(strings.Repeat("a", 64)).
		SetByteLength(length).
		SetChargedBytes(length + (3*length)/4 + 4096).
		SetData([]byte(strings.Repeat("x", size))).
		SaveX(ctx)
	row = client.Request.UpdateOneID(row.ID).SetRequestBodyPayloadID(payload.ID).SetEvidenceDisposition(disposition).SaveX(ctx)
	return row, payload
}

func TestManagedCapacityHysteresisAndFailurePriority(t *testing.T) {
	worker, client, ctx, _, proj := capacityTestWorker(t)
	defer client.Close()
	hard, low := 3, 2
	policy := &biz.StoragePolicy{
		ManagedObservabilityHardMiB: lo.ToPtr(hard),
		ManagedObservabilityLowMiB:  lo.ToPtr(low),
	}
	successOne, _ := addCapacityPayload(t, client, ctx, proj, request.StatusCompleted, 800*1024)
	successTwo, _ := addCapacityPayload(t, client, ctx, proj, request.StatusCanceled, 800*1024)
	failed, failedPayload := addCapacityPayload(t, client, ctx, proj, request.StatusFailed, 800*1024)

	require.NoError(t, worker.cleanupManagedCapacity(ctx, policy))
	require.Zero(t, client.ObservabilityPayload.Query().Where(observabilitypayload.RequestIDIn(successOne.ID, successTwo.ID)).CountX(ctx))
	require.True(t, client.ObservabilityPayload.Query().Where(observabilitypayload.IDEQ(failedPayload.ID)).ExistX(ctx))
	require.Equal(t, 3, client.Request.Query().CountX(ctx), "capacity eviction must preserve request skeletons")
	failed = client.Request.GetX(ctx, failed.ID)
	require.NotNil(t, failed.RequestBodyPayloadID)
	for _, id := range []int{successOne.ID, successTwo.ID} {
		row := client.Request.GetX(ctx, id)
		require.Nil(t, row.RequestBodyPayloadID)
		require.Equal(t, "evicted", row.EvidenceDisposition.RequestBody.Outcome)
		require.NotEmpty(t, row.EvidenceDisposition.RequestBody.SHA256)
	}
	state := client.ManagedObservabilityState.GetX(ctx, 1)
	require.False(t, state.UnderPressure)
	require.LessOrEqual(t, state.ChargedBytes, int64(low)<<20)
	require.Equal(t, proj.ID, client.Project.GetX(ctx, proj.ID).ID, "core project data is outside the managed allowlist")
}

func TestLocalGCOwnershipIsNonWaiting(t *testing.T) {
	worker, client, ctx, _, _ := capacityTestWorker(t)
	defer client.Close()
	entered := make(chan struct{})
	release := make(chan struct{})
	var owners atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		acquired, err := worker.withGCOwnership(ctx, func() {
			owners.Add(1)
			close(entered)
			<-release
		})
		require.NoError(t, err)
		require.True(t, acquired)
	}()
	<-entered
	acquired, err := worker.withGCOwnership(ctx, func() { owners.Add(1) })
	require.NoError(t, err)
	require.False(t, acquired)
	close(release)
	wg.Wait()
	require.Equal(t, int32(1), owners.Load())
}

func TestManagedCapacityFallsBackToExplicitRequestUsageAllowlist(t *testing.T) {
	worker, client, ctx, _, proj := capacityTestWorker(t)
	defer client.Close()
	response := []byte(`"` + strings.Repeat("r", 1536*1024) + `"`)
	for i := 0; i < 2; i++ {
		row := client.Request.Create().SetProjectID(proj.ID).SetModelID("model").SetRequestBody([]byte(`{}`)).
			SetResponseBody(response).SetStatus(request.StatusCompleted).SetManagedObservability(true).SaveX(ctx)
		client.UsageLog.Create().SetRequestID(row.ID).SetProjectID(proj.ID).SetModelID("model").SaveX(ctx)
	}
	policy := &biz.StoragePolicy{ManagedObservabilityHardMiB: lo.ToPtr(3), ManagedObservabilityLowMiB: lo.ToPtr(2)}
	require.NoError(t, worker.cleanupManagedCapacity(ctx, policy))
	require.Equal(t, 1, client.Request.Query().CountX(ctx))
	require.Equal(t, 1, client.UsageLog.Query().CountX(ctx), "usage rows are deleted only with their managed request group")
	require.Equal(t, proj.ID, client.Project.GetX(ctx, proj.ID).ID, "core project data remains excluded")
	state := client.ManagedObservabilityState.GetX(ctx, 1)
	require.False(t, state.UnderPressure)
	require.LessOrEqual(t, state.ChargedBytes, int64(2)<<20)
}

func TestManagedUsageLogCommitSerializesFreshReconciliationProjection(t *testing.T) {
	worker, client, ctx, system, proj := capacityTestWorker(t)
	defer client.Close()
	sqlDriver, ok := worker.sqlDriver()
	require.True(t, ok)
	// A single SQLite connection makes the transaction boundary a deterministic
	// stand-in for the state-row lock used by PostgreSQL/MySQL.
	sqlDriver.DB().SetMaxOpenConns(1)

	require.NoError(t, system.SetStoragePolicy(ctx, &biz.StoragePolicy{
		StoreRequestBody:            true,
		StoreResponseBody:           true,
		ManagedObservabilityHardMiB: lo.ToPtr(2),
		ManagedObservabilityLowMiB:  lo.ToPtr(1),
	}))
	client.ManagedObservabilityState.UpdateOneID(1).
		SetChargedBytes(0).
		SetUnderPressure(false).
		ClearLastError().
		ExecX(ctx)

	channel := client.Channel.Create().
		SetType(entchannel.TypeOpenai).
		SetName("serialized-usage-channel").
		SetCredentials(objects.ChannelCredentials{}).
		SetSupportedModels([]string{"model"}).
		SetDefaultTestModel("model").
		SaveX(ctx)
	parent := client.Request.Create().
		SetProjectID(proj.ID).
		SetChannelID(channel.ID).
		SetModelID("model").
		SetRequestBody(objects.JSONRawMessage(`{}`)).
		SetStatus(request.StatusCompleted).
		SaveX(ctx)
	usageService := biz.NewUsageLogService(client, system, biz.NewChannelServiceForTest(client))

	coreReady := make(chan struct{})
	releaseCore := make(chan struct{})
	usageService.SetAfterManagedCoreInsertHookForTest(func() {
		close(coreReady)
		<-releaseCore
	})
	callbackCount := 0
	usageService.OnUsageLogCreated = func() { callbackCount++ }

	type usageResult struct {
		row *ent.UsageLog
		err error
	}
	usageDone := make(chan usageResult, 1)
	go func() {
		row, err := usageService.CreateUsageLog(ctx, biz.CreateUsageLogParams{
			RequestID: parent.ID, ProjectID: proj.ID, ChannelID: channel.ID,
			ActualModelID: "model", Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
			Source: usagelog.SourceAPI, Format: "openai/chat_completions",
		})
		usageDone <- usageResult{row: row, err: err}
	}()
	<-coreReady

	reconcileStarted := make(chan struct{})
	worker.beforeManagedReconcileForTest = func() { close(reconcileStarted) }
	type reconcileResult struct {
		state *ent.ManagedObservabilityState
		err   error
	}
	reconcileDone := make(chan reconcileResult, 1)
	go func() {
		state, err := worker.reconcileManagedState(ctx, 2<<20)
		reconcileDone <- reconcileResult{state: state, err: err}
	}()
	<-reconcileStarted
	select {
	case result := <-reconcileDone:
		t.Fatalf("reconciliation observed an uncommitted managed UsageLog: state=%v err=%v", result.state, result.err)
	case <-time.After(100 * time.Millisecond):
	}

	close(releaseCore)
	created := <-usageDone
	require.NoError(t, created.err)
	require.NotNil(t, created.row)
	require.Equal(t, int64(30), created.row.TotalTokens)
	reconciled := <-reconcileDone
	require.NoError(t, reconciled.err)

	expectedCharge, err := worker.managedNonPayloadCharge(ctx, client)
	require.NoError(t, err)
	require.Equal(t, expectedCharge, reconciled.state.ChargedBytes)
	require.Equal(t, expectedCharge, client.ManagedObservabilityState.GetX(ctx, 1).ChargedBytes)
	require.Equal(t, 1, client.UsageLog.Query().CountX(ctx))
	require.True(t, client.Request.GetX(ctx, parent.ID).ManagedObservability)
	require.Equal(t, 1, callbackCount)
}

func TestManagedCapacityExcludesTerminalParentWithActiveExecution(t *testing.T) {
	worker, client, ctx, _, proj := capacityTestWorker(t)
	defer client.Close()
	activeParent, activePayload := addCapacityPayload(t, client, ctx, proj, request.StatusCompleted, 800*1024)
	client.RequestExecution.Create().
		SetRequestID(activeParent.ID).
		SetProjectID(proj.ID).
		SetModelID("model").
		SetRequestBody([]byte(`{}`)).
		SetStatus(requestexecution.StatusProcessing).
		SetManagedObservability(true).
		SaveX(ctx)
	lowParent, _ := addCapacityPayload(t, client, ctx, proj, request.StatusCompleted, 800*1024)

	policy := &biz.StoragePolicy{ManagedObservabilityHardMiB: lo.ToPtr(2), ManagedObservabilityLowMiB: lo.ToPtr(1)}
	require.NoError(t, worker.cleanupManagedCapacity(ctx, policy))
	require.True(t, client.ObservabilityPayload.Query().Where(observabilitypayload.IDEQ(activePayload.ID)).ExistX(ctx))
	require.True(t, client.Request.Query().Where(request.IDEQ(activeParent.ID)).ExistX(ctx))
	require.Zero(t, client.ObservabilityPayload.Query().Where(observabilitypayload.RequestIDEQ(lowParent.ID)).CountX(ctx))
	state := client.ManagedObservabilityState.GetX(ctx, 1)
	require.True(t, state.UnderPressure, "active evidence can keep the store above low without becoming a cleanup candidate")
}

func TestRetentionDeletionOwnsManagedPayloadLifecycleWithoutForeignKeys(t *testing.T) {
	worker, client, ctx, _, proj := capacityTestWorker(t)
	defer client.Close()
	old := time.Now().UTC().Add(-48 * time.Hour)
	cutoff := time.Now().UTC().Add(-24 * time.Hour)

	parent := client.Request.Create().
		SetProjectID(proj.ID).
		SetModelID("model").
		SetRequestBody([]byte(`{}`)).
		SetStatus(request.StatusCompleted).
		SetCreatedAt(old).
		SetUpdatedAt(old).
		SetManagedObservability(true).
		SaveX(ctx)
	execution := client.RequestExecution.Create().
		SetRequestID(parent.ID).
		SetProjectID(proj.ID).
		SetModelID("model").
		SetRequestBody([]byte(`{}`)).
		SetStatus(requestexecution.StatusCompleted).
		SetCreatedAt(old).
		SetUpdatedAt(old).
		SetManagedObservability(true).
		SaveX(ctx)
	charge := int64(256 * 1024)
	payload := client.ObservabilityPayload.Create().
		SetRequestID(parent.ID).
		SetKind(observabilitypayload.KindRequestBody).
		SetSha256(strings.Repeat("b", 64)).
		SetByteLength(charge).
		SetChargedBytes(charge).
		SetData(make([]byte, charge)).
		SaveX(ctx)
	client.RequestExecution.UpdateOneID(execution.ID).SetRequestBodyPayloadID(payload.ID).SaveX(ctx)
	client.ManagedObservabilityState.Create().SetID(1).SetChargedBytes(charge).SaveX(ctx)

	deleted, err := worker.deleteExecutionCandidate(ctx, execution.ID, cutoff, map[int]*ent.DataStorage{})
	require.NoError(t, err)
	require.True(t, deleted)
	require.False(t, client.ObservabilityPayload.Query().Where(observabilitypayload.IDEQ(payload.ID)).ExistX(ctx))
	require.Zero(t, client.ManagedObservabilityState.GetX(ctx, 1).ChargedBytes)

	parentPayload := client.ObservabilityPayload.Create().
		SetRequestID(parent.ID).
		SetKind(observabilitypayload.KindRequestBody).
		SetSha256(strings.Repeat("c", 64)).
		SetByteLength(charge).
		SetChargedBytes(charge).
		SetData(make([]byte, charge)).
		SaveX(ctx)
	client.Request.UpdateOneID(parent.ID).SetRequestBodyPayloadID(parentPayload.ID).SaveX(ctx)
	client.ManagedObservabilityState.UpdateOneID(1).SetChargedBytes(charge).SaveX(ctx)

	deleted, err = worker.deleteRequestCandidate(ctx, parent.ID, cutoff, map[int]*ent.DataStorage{})
	require.NoError(t, err)
	require.True(t, deleted)
	require.False(t, client.Request.Query().Where(request.IDEQ(parent.ID)).ExistX(ctx))
	require.Zero(t, client.ObservabilityPayload.Query().Where(observabilitypayload.RequestIDEQ(parent.ID)).CountX(ctx))
	require.Zero(t, client.ManagedObservabilityState.GetX(ctx, 1).ChargedBytes)

	legacy := client.Request.Create().
		SetProjectID(proj.ID).
		SetModelID("legacy").
		SetRequestBody([]byte(`{"legacy":true}`)).
		SetStatus(request.StatusCompleted).
		SetCreatedAt(old).
		SetUpdatedAt(old).
		SaveX(ctx)
	deleted, err = worker.deleteRequestCandidate(ctx, legacy.ID, cutoff, map[int]*ent.DataStorage{})
	require.NoError(t, err)
	require.True(t, deleted)
	require.Zero(t, client.ObservabilityPayload.Query().CountX(ctx))
}
