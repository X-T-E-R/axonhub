package gc

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	entschema "entgo.io/ent/dialect/sql/schema"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/managedobservabilitystate"
	"github.com/looplj/axonhub/internal/ent/migrate"
	"github.com/looplj/axonhub/internal/ent/observabilitypayload"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/ent/thread"
	"github.com/looplj/axonhub/internal/ent/trace"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/server/biz"
	serverdb "github.com/looplj/axonhub/internal/server/db"
)

func TestManagedCapacityComponentDebitsBeforeProjection(t *testing.T) {
	w, client, ctx, _, proj := capacityTestWorker(t)
	defer client.Close()
	parent := client.Request.Create().SetProjectID(proj.ID).SetModelID("m").SetRequestBody([]byte(`{}`)).
		SetManagedObservability(true).SetStatus(request.StatusCompleted).SaveX(ctx)
	execution := client.RequestExecution.Create().SetProjectID(proj.ID).SetRequestID(parent.ID).SetModelID("m").
		SetRequestBody([]byte(`{}`)).SetManagedObservability(true).SetStatus(requestexecution.StatusFailed).
		SetErrorMessage("failure").SetRequestURL("http://x").SaveX(ctx)
	usage := client.UsageLog.Create().SetProjectID(proj.ID).SetRequestID(parent.ID).SetModelID("m").
		SetFormat("f").SetCostPriceReferenceID("p").SaveX(ctx)
	const requestCharge = int64(65536 + 12)
	const executionCharge = int64(65536 + 8 + 7 + 8)
	const usageCharge = int64(4096 + 1 + 1 + 1 + 2) // CostItems defaults to [] (two JSON bytes).
	state, err := w.reconcileManagedState(ctx, 1<<30)
	require.NoError(t, err)
	require.Equal(t, requestCharge+executionCharge+usageCharge, state.ChargedBytes)
	var reclaimed int64
	ctx = context.WithValue(ctx, managedReclaimedKey{}, &reclaimed)
	cutoff := time.Now().Add(time.Hour)
	deleted, err := w.deleteUsageLogCandidate(ctx, usage.ID, cutoff)
	require.NoError(t, err)
	require.True(t, deleted)
	require.Equal(t, requestCharge+executionCharge, client.ManagedObservabilityState.GetX(ctx, 1).ChargedBytes)
	deleted, err = w.deleteExecutionCandidate(ctx, execution.ID, cutoff, map[int]*ent.DataStorage{})
	require.NoError(t, err)
	require.True(t, deleted)
	require.Equal(t, requestCharge, client.ManagedObservabilityState.GetX(ctx, 1).ChargedBytes)
	deleted, err = w.deleteRequestCandidate(ctx, parent.ID, cutoff, map[int]*ent.DataStorage{})
	require.NoError(t, err)
	require.True(t, deleted)
	require.Zero(t, client.ManagedObservabilityState.GetX(ctx, 1).ChargedBytes)
	require.Equal(t, requestCharge+executionCharge+usageCharge, reclaimed)
}

func TestManagedCapacityPayloadPriorityDoesNotStarveBacklog(t *testing.T) {
	previousBatch := defaultBatchSize
	defaultBatchSize = 2
	defer func() { defaultBatchSize = previousBatch }()
	w, client, ctx, system, proj := capacityTestWorker(t)
	defer client.Close()
	policy := &biz.StoragePolicy{ManagedObservabilityHardMiB: lo.ToPtr(3), ManagedObservabilityLowMiB: lo.ToPtr(1)}
	require.NoError(t, system.SetStoragePolicy(ctx, policy))
	_, first := addCapacityPayload(t, client, ctx, proj, request.StatusFailed, 800<<10)
	_, second := addCapacityPayload(t, client, ctx, proj, request.StatusFailed, 800<<10)
	for cycle := range 2 {
		_, success := addCapacityPayload(t, client, ctx, proj, request.StatusCompleted, 800<<10)
		require.NoError(t, w.cleanupManagedCapacity(ctx, policy))
		require.False(t, client.ObservabilityPayload.Query().Where(observabilitypayload.IDEQ(success.ID)).ExistX(ctx))
		require.Equal(t, 1-cycle, client.ObservabilityPayload.Query().Where(observabilitypayload.IDIn(first.ID, second.ID)).CountX(ctx),
			"unused batch capacity must reach failed evidence despite ongoing successful arrivals")
	}
	require.Equal(t, 4, client.Request.Query().CountX(ctx))
	require.False(t, client.ManagedObservabilityState.GetX(ctx, 1).UnderPressure)
}

func TestManagedCapacityMySQLTwoConnectionRecovery(t *testing.T) {
	dsn := os.Getenv("AXONHUB_TEST_MYSQL_DSN")
	if dsn == "" || os.Getenv("AXONHUB_TEST_MYSQL_DISPOSABLE") != "1" {
		t.Skip("requires disposable MySQL")
	}
	client := serverdb.NewEntClient(serverdb.Config{Dialect: dialect.MySQL, DSN: dsn, MaxOpenConns: 2, MaxIdleConns: 2})
	defer client.Close()
	ctx, cancel := context.WithTimeout(authz.WithTestBypass(ent.NewContext(context.Background(), client)), 10*time.Second)
	defer cancel()
	system := biz.NewSystemService(biz.SystemServiceParams{Ent: client})
	w := &Worker{Ent: client, SystemService: system}
	proj := client.Project.Create().SetName("mysql-recovery").SetStatus(project.StatusActive).SaveX(ctx)
	makeGroup := func() *ent.Request {
		return client.Request.Create().SetProjectID(proj.ID).SetModelID("model").SetRequestBody([]byte(`{}`)).
			SetResponseBody([]byte(`"` + strings.Repeat("r", 3<<20) + `"`)).SetManagedObservability(true).SetStatus(request.StatusCompleted).SaveX(ctx)
	}
	policy := &biz.StoragePolicy{ManagedObservabilityHardMiB: lo.ToPtr(2), ManagedObservabilityLowMiB: lo.ToPtr(1)}
	require.NoError(t, system.SetStoragePolicy(ctx, policy))
	makeGroup()
	var cleanupErr error
	owned, err := w.withGCOwnership(ctx, func(ownerCtx context.Context) { cleanupErr = w.cleanupManagedCapacity(ownerCtx, policy) })
	require.NoError(t, err)
	require.True(t, owned)
	require.NoError(t, cleanupErr)
	require.Zero(t, client.Request.Query().CountX(ctx))
	require.Zero(t, client.ManagedObservabilityState.GetX(ctx, 1).ChargedBytes)
	require.False(t, client.ManagedObservabilityState.GetX(ctx, 1).UnderPressure)
	row := makeGroup()
	w.afterManagedScanForTest = func() { require.NoError(t, system.SetStoragePolicy(ctx, &biz.StoragePolicy{})) }
	owned, err = w.withGCOwnership(ctx, func(ownerCtx context.Context) { cleanupErr = w.cleanupManagedCapacity(ownerCtx, policy) })
	require.NoError(t, err)
	require.True(t, owned)
	require.NoError(t, cleanupErr)
	require.True(t, client.Request.Query().Where(request.IDEQ(row.ID)).ExistX(ctx))
	require.False(t, client.ManagedObservabilityState.GetX(ctx, 1).UnderPressure)
}

func TestManagedCapacitySQLiteTwoConnectionsPolicyChange(t *testing.T) {
	client := enttest.NewEntClient(t, dialect.SQLite, "file:capacity-policy-shared?mode=memory&cache=shared&_fk=1")
	defer client.Close()
	system := biz.NewSystemService(biz.SystemServiceParams{Ent: client})
	w := &Worker{Ent: client, SystemService: system}
	driver, ok := w.sqlDriver()
	require.True(t, ok)
	driver.DB().SetMaxOpenConns(2)
	ctx, cancel := context.WithTimeout(authz.WithTestBypass(ent.NewContext(context.Background(), client)), 3*time.Second)
	defer cancel()
	policy := &biz.StoragePolicy{ManagedObservabilityHardMiB: lo.ToPtr(2), ManagedObservabilityLowMiB: lo.ToPtr(1)}
	require.NoError(t, system.SetStoragePolicy(ctx, policy))
	conn, err := driver.DB().Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	other, err := ent.Open(dialect.SQLite, "file:capacity-policy-shared?mode=memory&cache=shared&_fk=1")
	require.NoError(t, err)
	defer other.Close()
	otherSystem := biz.NewSystemService(biz.SystemServiceParams{Ent: other})
	w.afterManagedScanForTest = func() {
		require.NoError(t, otherSystem.SetStoragePolicy(ent.NewContext(ctx, other), &biz.StoragePolicy{}))
	}
	require.NoError(t, w.cleanupManagedCapacity(ctx, policy))
	require.False(t, client.ManagedObservabilityState.GetX(ctx, 1).UnderPressure)
}

func TestManagedCapacityRevalidatesGroupBeforeDeletingUsage(t *testing.T) {
	w, client, ctx, system, proj := capacityTestWorker(t)
	defer client.Close()
	policy := &biz.StoragePolicy{ManagedObservabilityHardMiB: lo.ToPtr(2), ManagedObservabilityLowMiB: lo.ToPtr(1)}
	require.NoError(t, system.SetStoragePolicy(ctx, policy))
	row := client.Request.Create().SetProjectID(proj.ID).SetModelID("model").SetRequestBody([]byte(`{}`)).
		SetResponseBody([]byte(`"` + strings.Repeat("r", 3<<20) + `"`)).SetManagedObservability(true).SetStatus(request.StatusCompleted).SaveX(ctx)
	usage := client.UsageLog.Create().SetProjectID(proj.ID).SetRequestID(row.ID).SetModelID("model").SaveX(ctx)
	w.beforeCandidateDelete = func(resource string, id int) {
		if resource == "capacity_request_group" {
			client.Request.UpdateOneID(id).SetStatus(request.StatusProcessing).SaveX(ctx)
		}
	}
	require.NoError(t, w.cleanupManagedCapacity(ctx, policy))
	require.True(t, client.Request.Query().Where(request.IDEQ(row.ID)).ExistX(ctx))
	require.True(t, client.UsageLog.Query().Where(usagelog.IDEQ(usage.ID)).ExistX(ctx))
	require.True(t, client.ManagedObservabilityState.GetX(ctx, 1).UnderPressure)
	require.Equal(t, proj.ID, client.Project.Query().Where(project.IDEQ(proj.ID)).OnlyX(ctx).ID)
}

func TestManagedLedgerRevisionUpgradeAndMutations(t *testing.T) {
	targets := map[string]string{dialect.SQLite: "file:managed-ledger-revision?mode=memory&cache=shared"}
	if os.Getenv("AXONHUB_TEST_PG_DISPOSABLE") == "1" && os.Getenv(postgresManagedIntegrationEnv) != "" {
		targets[dialect.Postgres] = os.Getenv(postgresManagedIntegrationEnv)
	}
	if os.Getenv("AXONHUB_TEST_MYSQL_DISPOSABLE") == "1" && os.Getenv("AXONHUB_TEST_MYSQL_DSN") != "" {
		targets[dialect.MySQL] = os.Getenv("AXONHUB_TEST_MYSQL_DSN")
	}
	for name, dsn := range targets {
		t.Run(name, func(t *testing.T) {
			client, err := func() (*ent.Client, error) {
				if name == dialect.Postgres {
					db, err := sql.Open("pgx", dsn)
					if err != nil {
						return nil, err
					}
					return ent.NewClient(ent.Driver(entsql.OpenDB(name, db))), nil
				}
				return ent.Open(name, dsn)
			}()
			require.NoError(t, err)
			defer client.Close()
			ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
			// Build the prior table using Ent's schema migration API, omitting only
			// the newly introduced column. This is fixture setup, not migration SQL.
			current := migrate.ManagedObservabilityStatesTable
			legacy := entschema.NewTable(current.Name)
			for _, column := range current.Columns {
				if column.Name != managedobservabilitystate.FieldLedgerRevision {
					legacy.AddColumn(column)
				}
			}
			legacy.PrimaryKey = current.PrimaryKey
			require.NoError(t, migrate.Create(ctx, client.Schema, []*entschema.Table{legacy}, migrate.WithForeignKeys(false)))
			query, args := entsql.Dialect(name).Insert(current.Name).Columns("id", "charged_bytes", "under_pressure", "last_error", "updated_at").
				Values(1, 1000, false, "legacy", time.Now().UTC()).Query()
			require.NoError(t, client.Driver().Exec(ctx, query, args, nil))
			require.NoError(t, migrate.Create(ctx, client.Schema, []*entschema.Table{current}, migrate.WithForeignKeys(false)))
			state := client.ManagedObservabilityState.GetX(ctx, 1)
			require.Equal(t, int64(1000), state.ChargedBytes)
			require.Zero(t, state.LedgerRevision, "existing rows receive revision zero through Ent migration")
			fixedTime := state.UpdatedAt
			state = client.ManagedObservabilityState.UpdateOneID(1).AddChargedBytes(-100).SetUpdatedAt(fixedTime).SaveX(ctx)
			require.Equal(t, int64(1), state.LedgerRevision)
			client.ManagedObservabilityState.Update().Where(managedobservabilitystate.IDEQ(1)).AddChargedBytes(100).SetUpdatedAt(fixedTime).SaveX(ctx)
			state = client.ManagedObservabilityState.GetX(ctx, 1)
			require.Equal(t, int64(2), state.LedgerRevision, "net-zero bytes and unchanged wall time still have a new version")
			require.Equal(t, int64(1000), state.ChargedBytes)
			client.ManagedObservabilityState.UpdateOneID(1).SetUnderPressure(true).SetLastError("metadata-only").SaveX(ctx)
			require.Equal(t, int64(2), client.ManagedObservabilityState.GetX(ctx, 1).LedgerRevision,
				"repeated pressure diagnostics must not prevent retiring conservative accounting drift")
			tx, err := client.Tx(ctx)
			require.NoError(t, err)
			tx.ManagedObservabilityState.UpdateOneID(1).AddChargedBytes(1).SaveX(ctx)
			require.NoError(t, tx.Rollback())
			require.Equal(t, int64(2), client.ManagedObservabilityState.GetX(ctx, 1).LedgerRevision)
			require.NoError(t, client.ManagedObservabilityState.Create().SetID(1).OnConflictColumns(managedobservabilitystate.FieldID).
				Update(func(u *ent.ManagedObservabilityStateUpsert) { u.SetUnderPressure(true) }).Exec(ctx))
			state = client.ManagedObservabilityState.GetX(ctx, 1)
			require.Equal(t, int64(1000), state.ChargedBytes)
			require.Equal(t, int64(2), state.LedgerRevision, "policy-only upsert does not rewrite the charged-byte ledger")
		})
	}
}

func TestManagedCapacityReservationRefundKeepsPersistedCharge(t *testing.T) {
	for _, newPayload := range []bool{false, true} {
		t.Run(fmt.Sprint(newPayload), func(t *testing.T) {
			w, client, ctx, _, proj := capacityTestWorker(t)
			defer client.Close()
			parent := client.Request.Create().SetProjectID(proj.ID).SetModelID("legacy").SetRequestBody([]byte(`{}`)).SetStatus(request.StatusCompleted).SaveX(ctx)
			client.ObservabilityPayload.Create().SetRequestID(parent.ID).SetKind(observabilitypayload.KindRequestBody).
				SetSha256(strings.Repeat("a", 64)).SetByteLength(1).SetChargedBytes(1000).SetData([]byte("x")).SaveX(ctx)
			state := client.ManagedObservabilityState.Create().SetID(1).SetChargedBytes(1100).SaveX(ctx)
			w.afterManagedScanForTest = func() {
				client.ManagedObservabilityState.UpdateOneID(1).AddChargedBytes(-100).SetUpdatedAt(state.UpdatedAt).SaveX(ctx)
				if newPayload {
					client.ObservabilityPayload.Create().SetRequestID(parent.ID).SetKind(observabilitypayload.KindRequestBody).
						SetSha256(strings.Repeat("b", 64)).SetByteLength(1).SetChargedBytes(100).SetData([]byte("y")).SaveX(ctx)
					client.ManagedObservabilityState.UpdateOneID(1).AddChargedBytes(100).SetUpdatedAt(state.UpdatedAt).SaveX(ctx)
				}
			}
			state, err := w.reconcileManagedState(ctx, 1<<20)
			require.NoError(t, err)
			expected := int64(1000)
			if newPayload {
				expected += 100
			}
			require.Equal(t, expected, state.ChargedBytes, "persisted 1000 plus reservation 100 must not become 900 after refund")
		})
	}
}

func TestManagedCapacitySharedOwnerDebitsOnce(t *testing.T) {
	w, client, ctx, _, proj := capacityTestWorker(t)
	defer client.Close()
	owner := client.Thread.Create().SetProjectID(proj.ID).SetThreadID("shared").SetStatus(thread.StatusActive).SaveX(ctx)
	first := client.Trace.Create().SetProjectID(proj.ID).SetTraceID("first").SetThreadID(owner.ID).SetStatus(trace.StatusActive).SaveX(ctx)
	second := client.Trace.Create().SetProjectID(proj.ID).SetTraceID("second").SetThreadID(owner.ID).SetStatus(trace.StatusActive).SaveX(ctx)
	var ids []int
	for _, traceID := range []int{first.ID, first.ID, second.ID} {
		row := client.Request.Create().SetProjectID(proj.ID).SetModelID("model").SetRequestBody([]byte(`{}`)).
			SetTraceID(traceID).SetManagedObservability(true).SetStatus(request.StatusCompleted).SaveX(ctx)
		ids = append(ids, row.ID)
	}
	// Three fixed skeletons with null chunks/disposition/routing JSON, two
	// referenced traces, one referenced thread. No measured implementation total.
	const skeleton = int64(65536 + 4 + 4 + 4)
	const initial = 3*skeleton + 3*4096
	state, err := w.reconcileManagedState(ctx, 1<<30)
	require.NoError(t, err)
	require.Equal(t, initial, state.ChargedBytes)
	var reclaimed int64
	ctx = context.WithValue(ctx, managedReclaimedKey{}, &reclaimed)
	for index, remaining := range []int64{2*skeleton + 3*4096, skeleton + 2*4096, 0} {
		deleted, err := w.deleteRequestCandidate(ctx, ids[index], time.Now().Add(time.Hour), map[int]*ent.DataStorage{})
		require.NoError(t, err)
		require.True(t, deleted)
		require.Equal(t, remaining, client.ManagedObservabilityState.GetX(ctx, 1).ChargedBytes,
			"verify the committed debit before any correcting full projection")
		require.Equal(t, initial-remaining, reclaimed)
	}
	require.Equal(t, 2, client.Trace.Query().CountX(ctx))
	require.Equal(t, 1, client.Thread.Query().CountX(ctx), "unreferenced owner rows are not additionally charged or deleted")
}

func TestManagedCapacityScanOutlivesMutationBudget(t *testing.T) {
	if os.Getenv("AXONHUB_TEST_SLOW_GC_SCAN") != "1" {
		t.Skip("controlled scan-delay proof is opt-in")
	}
	w, client, ctx, system, proj := capacityTestWorker(t)
	defer client.Close()
	policy := &biz.StoragePolicy{ManagedObservabilityHardMiB: lo.ToPtr(2), ManagedObservabilityLowMiB: lo.ToPtr(1)}
	require.NoError(t, system.SetStoragePolicy(ctx, policy))
	client.Request.Create().SetProjectID(proj.ID).SetModelID("model").SetRequestBody([]byte(`{}`)).
		SetResponseBody([]byte(`"` + strings.Repeat("r", 3<<20) + `"`)).SetManagedObservability(true).SetStatus(request.StatusCompleted).SaveX(ctx)
	paused := false
	w.afterManagedSnapshotForTest = func() {
		if !paused {
			paused = true
			time.Sleep(managedCapacityRoundTimeout + 100*time.Millisecond)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, managedCapacityRoundTimeout+15*time.Second)
	defer cancel()
	started := time.Now()
	require.NoError(t, w.cleanupManagedCapacity(ctx, policy))
	require.Greater(t, time.Since(started), managedCapacityRoundTimeout)
	require.Zero(t, client.Request.Query().CountX(ctx))
	require.Zero(t, client.ManagedObservabilityState.GetX(ctx, 1).ChargedBytes)
	require.False(t, client.ManagedObservabilityState.GetX(ctx, 1).UnderPressure)
}

func TestManagedCapacityPostgresRepairContracts(t *testing.T) {
	dsn := os.Getenv(postgresManagedIntegrationEnv)
	if dsn == "" || os.Getenv("AXONHUB_TEST_PG_DISPOSABLE") != "1" {
		t.Skip("requires disposable PostgreSQL")
	}
	f := newPostgresManagedFixture(t, dsn)
	driver, ok := f.worker.sqlDriver()
	require.True(t, ok)
	policy := &biz.StoragePolicy{ManagedObservabilityHardMiB: lo.ToPtr(2), ManagedObservabilityLowMiB: lo.ToPtr(1)}
	require.NoError(t, f.system.SetStoragePolicy(f.ctx, policy))

	t.Run("two pooled connections include owner and full scan", func(t *testing.T) {
		driver.DB().SetMaxOpenConns(2)
		ctx, cancel := context.WithTimeout(f.ctx, 3*time.Second)
		defer cancel()
		var cleanupErr error
		owned, err := f.worker.withGCOwnership(ctx, func(ownerCtx context.Context) { cleanupErr = f.worker.cleanupManagedCapacity(ownerCtx, policy) })
		require.NoError(t, err)
		require.True(t, owned)
		require.NoError(t, cleanupErr)
	})

	t.Run("saturated pool waiting on state cannot block its lock holder", func(t *testing.T) {
		driver.DB().SetMaxOpenConns(3)
		require.NoError(t, f.system.SetStoragePolicy(f.ctx, policy))
		monitor, err := sql.Open("pgx", dsn)
		require.NoError(t, err)
		defer monitor.Close()
		ctx, cancel := context.WithTimeout(f.ctx, 4*time.Second)
		defer cancel()
		writer := make(chan error, 2)
		f.worker.afterManagedPublishLockForTest = func() {
			go func() {
				defer func() {
					if p := recover(); p != nil {
						writer <- fmt.Errorf("writer panic: %v", p)
					}
				}()
				_, err := f.client.ManagedObservabilityState.UpdateOneID(1).AddChargedBytes(1).Save(ctx)
				writer <- err
			}()
			require.Eventually(t, func() bool {
				var waiting bool
				err := monitor.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
				WHERE datname = current_database() AND wait_event_type = 'Lock'
				AND query LIKE 'UPDATE "managed_observability_states"%')`).Scan(&waiting)
				return err == nil && waiting
			}, time.Second, 10*time.Millisecond)
		}
		f.worker.afterManagedFinishLockForTest = f.worker.afterManagedPublishLockForTest
		defer func() { f.worker.afterManagedPublishLockForTest = nil; f.worker.afterManagedFinishLockForTest = nil }()
		var cleanupErr error
		owned, err := f.worker.withGCOwnership(ctx, func(ownerCtx context.Context) { cleanupErr = f.worker.cleanupManagedCapacity(ownerCtx, policy) })
		require.NoError(t, err)
		require.True(t, owned)
		require.NoError(t, cleanupErr)
		require.NoError(t, <-writer)
		require.NoError(t, <-writer)
	})

	t.Run("continuous arrivals still allow net reclamation without undercharge", func(t *testing.T) {
		driver.DB().SetMaxOpenConns(8)
		// A managed skeleton has three null JSON fields (4 bytes each).
		const charge = int64(65536 + (256 << 10) + 2 + 12)
		makeGroup := func(status request.Status) {
			tx, err := f.client.Tx(f.ctx)
			require.NoError(t, err)
			defer tx.Rollback()
			_, err = tx.Request.Create().SetProjectID(f.project.ID).SetModelID("model").SetRequestBody([]byte(`{}`)).
				SetResponseBody([]byte(`"` + strings.Repeat("r", 256<<10) + `"`)).SetManagedObservability(true).SetStatus(status).Save(f.ctx)
			require.NoError(t, err)
			_, err = tx.ManagedObservabilityState.UpdateOneID(1).AddChargedBytes(charge).Save(f.ctx)
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
		}
		f.client.ManagedObservabilityState.UpdateOneID(1).SetChargedBytes(0).SaveX(f.ctx)
		for range 10 {
			makeGroup(request.StatusFailed)
		}
		for cycle := range 8 {
			arrived := false
			f.worker.afterManagedSnapshotForTest = func() {
				if !arrived {
					arrived = true
					makeGroup(request.StatusCompleted)
				}
			}
			before := f.client.Request.Query().CountX(f.ctx)
			f.cleanupOwned(t, policy)
			remaining := f.client.Request.Query().CountX(f.ctx)
			state := f.client.ManagedObservabilityState.GetX(f.ctx, 1)
			require.GreaterOrEqual(t, state.ChargedBytes, int64(remaining)*charge)
			require.False(t, state.UnderPressure)
			if cycle == 0 {
				require.Less(t, remaining, before, "reclaim more groups than the ongoing one-group arrival")
				require.LessOrEqual(t, state.ChargedBytes, int64(1<<20))
			}
			t.Logf("arrival cycle=%d before=%d remaining=%d charged=%d", cycle, before, remaining, state.ChargedBytes)
		}
		f.worker.afterManagedSnapshotForTest = nil
	})
}

func TestManagedCapacityPostgresMutationPolicyDeadline(t *testing.T) {
	dsn := os.Getenv(postgresManagedIntegrationEnv)
	if dsn == "" || os.Getenv("AXONHUB_TEST_PG_DISPOSABLE") != "1" {
		t.Skip("requires disposable PostgreSQL")
	}
	f := newPostgresManagedFixture(t, dsn)
	monitor, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	defer monitor.Close()
	policy := &biz.StoragePolicy{ManagedObservabilityHardMiB: lo.ToPtr(2), ManagedObservabilityLowMiB: lo.ToPtr(1)}
	require.NoError(t, f.system.SetStoragePolicy(f.ctx, policy))
	row := f.client.Request.Create().SetProjectID(f.project.ID).SetModelID("model").SetRequestBody([]byte(`{}`)).
		SetResponseBody([]byte(`"` + strings.Repeat("r", 3<<20) + `"`)).SetManagedObservability(true).
		SetStatus(request.StatusCompleted).SaveX(f.ctx)
	usage := f.client.UsageLog.Create().SetProjectID(f.project.ID).SetRequestID(row.ID).SetModelID("model").
		SetFormat("f").SetCostPriceReferenceID("p").SaveX(f.ctx)
	for _, owned := range []bool{false, true} {
		t.Run(fmt.Sprintf("reserved_owner=%t", owned), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(f.ctx, managedCapacityRoundTimeout+10*time.Second)
			defer cancel()
			blocker, err := monitor.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer blocker.Rollback()
			locked := make(chan error, 1)
			f.worker.beforeCandidateDelete = func(resource string, id int) {
				if resource == "capacity_request_group" && id == row.ID {
					_, err := blocker.ExecContext(ctx, `LOCK TABLE systems IN ACCESS EXCLUSIVE MODE`)
					locked <- err
				}
			}
			defer func() { f.worker.beforeCandidateDelete = nil }()
			result := make(chan error, 1)
			go func() {
				if !owned {
					result <- f.worker.cleanupManagedCapacity(ctx, policy)
					return
				}
				var cleanupErr error
				_, ownerErr := f.worker.withGCOwnership(ctx, func(ownerCtx context.Context) {
					cleanupErr = f.worker.cleanupManagedCapacity(ownerCtx, policy)
				})
				if ownerErr != nil {
					result <- ownerErr
				} else {
					result <- cleanupErr
				}
			}()
			select {
			case err := <-locked:
				require.NoError(t, err)
			case err := <-result:
				t.Fatalf("round ended before mutation barrier: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var pid int
			var started time.Time
			require.Eventually(t, func() bool {
				return monitor.QueryRowContext(ctx, `SELECT pid, query_start FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock'
					AND query LIKE 'SELECT %FROM "systems"%' LIMIT 1`).Scan(&pid, &started) == nil
			}, time.Second, 10*time.Millisecond)
			// Keep the table lock held: only the mutation deadline can end this
			// particular SQL. A later outer-context final check may wait again,
			// so compare query_start as well as PID rather than all policy waits.
			require.Eventually(t, func() bool {
				var waiting bool
				err := monitor.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
					WHERE pid = $1 AND query_start = $2 AND wait_event_type = 'Lock')`, pid, started).Scan(&waiting)
				return err == nil && !waiting
			}, managedCapacityRoundTimeout+2*time.Second, 20*time.Millisecond)
			require.NoError(t, ctx.Err(), "outer context must still be live when mutation SQL stops")
			require.NoError(t, blocker.Rollback())
			select {
			case err := <-result:
				// Drivers may wrap their cancellation; the invariant here is the
				// blocked mutation SQL stopped while the outer context was live.
				t.Logf("round result after mutation deadline: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			require.True(t, f.client.Request.Query().Where(request.IDEQ(row.ID)).ExistX(f.ctx))
			require.True(t, f.client.UsageLog.Query().Where(usagelog.IDEQ(usage.ID)).ExistX(f.ctx))
			require.True(t, f.client.ManagedObservabilityState.GetX(f.ctx, 1).UnderPressure)
		})
	}
}
