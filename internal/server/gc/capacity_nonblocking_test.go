package gc

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/observabilitypayload"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestManagedCapacityStopsChangedPolicyAndIncompleteSnapshot(t *testing.T) {
	for _, stop := range []string{"disable", "change", "deadline"} {
		t.Run(stop, func(t *testing.T) {
			w, client, ctx, system, proj := capacityTestWorker(t)
			defer client.Close()
			policy := &biz.StoragePolicy{ManagedObservabilityHardMiB: lo.ToPtr(2), ManagedObservabilityLowMiB: lo.ToPtr(1)}
			require.NoError(t, system.SetStoragePolicy(ctx, policy))
			_, payload := addCapacityPayload(t, client, ctx, proj, request.StatusCompleted, 2<<20)
			client.ManagedObservabilityState.UpdateOneID(1).SetChargedBytes(123).SaveX(ctx)
			// A remote instance can change the database while this instance retains
			// a warm policy cache. GC must not consult that cached value.
			system.Cache = xcache.NewMemoryWithOptions[ent.System](time.Hour, time.Hour)
			_, err := system.StoragePolicy(ctx)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			w.afterManagedScanForTest = func() {
				if stop == "deadline" {
					cancel()
					return
				}
				other := biz.NewSystemService(biz.SystemServiceParams{Ent: client})
				next := &biz.StoragePolicy{}
				if stop == "change" {
					next.ManagedObservabilityHardMiB, next.ManagedObservabilityLowMiB = lo.ToPtr(4), lo.ToPtr(3)
				}
				require.NoError(t, other.SetStoragePolicy(ctx, next))
			}
			if stop == "deadline" {
				w.afterManagedSnapshotForTest = func() { cancel() }
				w.afterManagedScanForTest = nil
			}
			err = w.cleanupManagedCapacity(ctx, policy)
			if stop == "deadline" {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
			}
			checkCtx := ent.NewContext(context.WithoutCancel(ctx), client)
			require.True(t, client.ObservabilityPayload.Query().Where(observabilitypayload.IDEQ(payload.ID)).ExistX(checkCtx))
			state := client.ManagedObservabilityState.GetX(checkCtx, 1)
			require.Equal(t, int64(123), state.ChargedBytes, "an incomplete projection must never publish")
			require.Equal(t, stop != "disable", state.UnderPressure)
		})
	}
}

func TestManagedCapacityRoundBoundsGroupsAndReconciliations(t *testing.T) {
	previousBatch := defaultBatchSize
	defaultBatchSize = 2
	defer func() { defaultBatchSize = previousBatch }()
	w, client, ctx, system, proj := capacityTestWorker(t)
	defer client.Close()
	policy := &biz.StoragePolicy{ManagedObservabilityHardMiB: lo.ToPtr(2), ManagedObservabilityLowMiB: lo.ToPtr(1)}
	require.NoError(t, system.SetStoragePolicy(ctx, policy))
	for range 3 {
		client.Request.Create().SetProjectID(proj.ID).SetModelID("model").SetRequestBody([]byte(`{}`)).
			SetResponseBody([]byte(`"` + strings.Repeat("r", 2<<20) + `"`)).
			SetStatus(request.StatusCompleted).SetManagedObservability(true).SaveX(ctx)
	}
	scans := 0
	w.beforeManagedReconcileForTest = func() { scans++ }
	require.NoError(t, w.cleanupManagedCapacity(ctx, policy))
	require.Equal(t, 2, scans, "one initial and one post-delete projection, never per unbounded group")
	require.Equal(t, 1, client.Request.Query().CountX(ctx), "no more than the configured batch is deleted")
	state := client.ManagedObservabilityState.GetX(ctx, 1)
	require.True(t, state.UnderPressure)
	require.Equal(t, "capacity_cleanup_incomplete", state.LastError)
	require.Greater(t, state.ChargedBytes, int64(2<<20))
	for remaining := 0; remaining >= 0; remaining-- {
		scans = 0
		require.NoError(t, w.cleanupManagedCapacity(ctx, policy))
		require.Equal(t, 2, scans)
		require.Equal(t, remaining, client.Request.Query().CountX(ctx))
	}
	require.False(t, client.ManagedObservabilityState.GetX(ctx, 1).UnderPressure, "subsequent bounded rounds converge")
}

func TestManagedCapacityConcurrentRefundDoesNotPublishNegativeCharge(t *testing.T) {
	w, client, ctx, _, _ := capacityTestWorker(t)
	defer client.Close()
	client.ManagedObservabilityState.Create().SetID(1).SetChargedBytes(100).SaveX(ctx)
	w.afterManagedScanForTest = func() {
		client.ManagedObservabilityState.UpdateOneID(1).AddChargedBytes(-100).SaveX(ctx)
	}
	state, err := w.reconcileManagedState(ctx, 1<<20)
	require.NoError(t, err)
	require.Zero(t, state.ChargedBytes)
}

func TestManagedCapacityPostgresSnapshotPreservesCommittedDeltas(t *testing.T) {
	dsn := os.Getenv(postgresManagedIntegrationEnv)
	if dsn == "" || os.Getenv("AXONHUB_TEST_PG_DISPOSABLE") != "1" {
		t.Skip("requires disposable PostgreSQL")
	}
	f := newPostgresManagedFixture(t, dsn)
	parent, payload := addCapacityPayload(t, f.client, f.ctx, f.project, request.StatusCompleted, 1024)
	initial, err := f.worker.reconcileManagedState(f.ctx, 1<<30)
	require.NoError(t, err)
	// Seed drift independently of the projection; publication must correct this
	// baseline while retaining the transaction committed after the snapshot.
	f.client.ManagedObservabilityState.UpdateOneID(1).AddChargedBytes(12345).SaveX(f.ctx)
	snapshot, release := make(chan struct{}), make(chan struct{})
	f.worker.afterManagedSnapshotForTest = func() { close(snapshot); <-release }
	result := make(chan error, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				result <- fmt.Errorf("reconcile panic: %v", p)
			}
		}()
		_, err := f.worker.reconcileManagedState(f.ctx, 1<<30)
		result <- err
	}()
	<-snapshot
	writerCtx, cancel := context.WithTimeout(f.ctx, 2*time.Second)
	defer cancel()
	writerStarted := time.Now()
	tx, err := f.client.Tx(writerCtx)
	require.NoError(t, err)
	defer tx.Rollback()
	newCharge := int64(777)
	_, err = tx.ObservabilityPayload.Create().SetRequestID(parent.ID).SetKind(observabilitypayload.KindRequestBody).
		SetSha256(strings.Repeat("b", 64)).SetByteLength(1).SetChargedBytes(newCharge).SetData([]byte("x")).Save(writerCtx)
	if err == nil {
		err = tx.ObservabilityPayload.DeleteOneID(payload.ID).Exec(writerCtx)
	}
	if err == nil {
		_, err = tx.ManagedObservabilityState.UpdateOneID(1).AddChargedBytes(newCharge - payload.ChargedBytes).Save(writerCtx)
	}
	if err == nil {
		err = tx.Commit()
	}
	close(release)
	require.NoError(t, err, "a writer must commit while the snapshot scan is paused, not wait for its state row lock")
	t.Logf("concurrent insert/delete and charge transaction committed in %s while snapshot remained paused", time.Since(writerStarted))
	require.NoError(t, <-result)
	f.worker.afterManagedSnapshotForTest = nil
	state := f.client.ManagedObservabilityState.GetX(f.ctx, 1)
	require.Equal(t, initial.ChargedBytes+12345-payload.ChargedBytes+newCharge, state.ChargedBytes,
		"retain conservative preexisting drift when a concurrent ledger change could include a refund")
	fresh, err := f.worker.reconcileManagedState(f.ctx, 1<<30)
	require.NoError(t, err)
	require.Equal(t, initial.ChargedBytes-payload.ChargedBytes+newCharge, fresh.ChargedBytes,
		"the next uncontended complete projection retires drift without dropping the insert/delete delta")

	t.Run("initial state creation race preserves the committed initializer", func(t *testing.T) {
		require.NoError(t, f.client.ManagedObservabilityState.DeleteOneID(1).Exec(f.ctx))
		initializer, err := f.client.Tx(f.ctx)
		require.NoError(t, err)
		defer initializer.Rollback()
		_, err = initializer.ManagedObservabilityState.Create().SetID(1).SetChargedBytes(fresh.ChargedBytes).
			SetLastError("initializer-marker").Save(f.ctx)
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() {
			defer func() {
				if p := recover(); p != nil {
					done <- fmt.Errorf("reconcile panic: %v", p)
				}
			}()
			_, err := f.worker.reconcileManagedState(f.ctx, 1<<30)
			done <- err
		}()
		driver, ok := f.worker.sqlDriver()
		require.True(t, ok)
		waiting := false
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && !waiting {
			err = driver.DB().QueryRowContext(f.ctx, `SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
				AND wait_event_type = 'Lock' AND query LIKE 'INSERT INTO "managed_observability_states"%'
			)`).Scan(&waiting)
			if err != nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		require.NoError(t, initializer.Commit())
		require.NoError(t, <-done)
		require.NoError(t, err)
		require.True(t, waiting, "exercise an actual concurrent singleton insert, not a sequential initializer")
		initialized := f.client.ManagedObservabilityState.GetX(f.ctx, 1)
		require.Equal(t, fresh.ChargedBytes, initialized.ChargedBytes)
		require.Equal(t, "initializer-marker", initialized.LastError)
	})

	t.Run("policy disable during open snapshot stops the old round", func(t *testing.T) {
		policy := &biz.StoragePolicy{ManagedObservabilityHardMiB: lo.ToPtr(2), ManagedObservabilityLowMiB: lo.ToPtr(1)}
		require.NoError(t, f.system.SetStoragePolicy(f.ctx, policy))
		before := f.client.ManagedObservabilityState.GetX(f.ctx, 1).ChargedBytes
		f.worker.afterManagedSnapshotForTest = func() {
			require.NoError(t, f.system.SetStoragePolicy(f.ctx, &biz.StoragePolicy{}))
		}
		require.NoError(t, f.worker.cleanupManagedCapacity(f.ctx, policy))
		f.worker.afterManagedSnapshotForTest = nil
		after := f.client.ManagedObservabilityState.GetX(f.ctx, 1)
		require.Equal(t, before, after.ChargedBytes)
		require.False(t, after.UnderPressure)
	})

	t.Run("blocked SQL honors deadline without publication or retry", func(t *testing.T) {
		blocker, err := f.client.Tx(f.ctx)
		require.NoError(t, err)
		defer blocker.Rollback()
		_, err = blocker.ManagedObservabilityState.UpdateOneID(1).SetLastError("held-lock").Save(f.ctx)
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(f.ctx, 100*time.Millisecond)
		defer cancel()
		started := time.Now()
		_, err = f.worker.reconcileManagedState(ctx, 1<<30)
		require.Error(t, err)
		require.ErrorIs(t, ctx.Err(), context.DeadlineExceeded)
		require.Less(t, time.Since(started), 2*time.Second)
		require.NoError(t, blocker.Rollback())
		require.Equal(t, fresh.ChargedBytes, f.client.ManagedObservabilityState.GetX(f.ctx, 1).ChargedBytes)
	})
}
