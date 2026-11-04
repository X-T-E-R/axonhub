package biz

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/migrate"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/llm"
	"github.com/samber/lo"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func clusterAccountingClients(t *testing.T) (*ent.Client, *ent.Client, context.Context) {
	t.Helper()
	dsn := os.Getenv("AXONHUB_TEST_PG_DSN")
	if dsn == "" || os.Getenv("AXONHUB_TEST_PG_DISPOSABLE") != "1" {
		t.Skip("requires disposable PostgreSQL")
	}
	root, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	schema := "task49_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = root.Exec("CREATE SCHEMA " + schema)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = root.Exec("DROP SCHEMA " + schema + " CASCADE"); _ = root.Close() })
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	dsn += separator + "search_path=" + schema
	open := func() *ent.Client {
		db, err := sql.Open("pgx", dsn)
		require.NoError(t, err)
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(1)
		c := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	a, b := open(), open()
	ctx := authz.WithTestBypass(t.Context())
	require.NoError(t, a.Schema.Create(ctx, migrate.WithForeignKeys(false)))
	return a, b, ctx
}

func accountingNode(client *ent.Client, id string) *ForwardingObservationWriter {
	system := NewSystemService(SystemServiceParams{Ent: client, CacheConfig: xcache.Config{Mode: xcache.ModeMemory}})
	usage := &UsageLogService{AbstractService: &AbstractService{db: client}, SystemService: system}
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{})
	w.primary = client
	w.system = system
	w.usageService = usage
	w.node = cluster.Config{Enabled: true, NodeID: id}
	return w
}

func TestClusterPendingUsageTwoInstancesSnapshotAndRecovery(t *testing.T) {
	a, b, ctx := clusterAccountingClients(t)
	wa, wb := accountingNode(a, "a"), accountingNode(b, "b")
	stamp := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	req := &ent.Request{ID: -1, APIKeyID: 55, ProjectID: 1, CreatedAt: stamp, Source: request.SourceAPI, Format: "openai/responses"}
	exec := &ent.RequestExecution{ChannelID: 1, ModelID: "m"}
	require.NoError(t, wa.publishPendingUsage(ctx, "pending-a", req, exec, llm.Usage{TotalTokens: 20}, stamp, observationUsageCost{total: lo.ToPtr(.75)}))
	// No local pending map is shared. B sees A's captured fact via its own DB pool.
	qb := &QuotaService{ent: b, observation: wb}
	before, err := qb.accountedUsage(ctx, 55, QuotaWindow{})
	require.NoError(t, err)
	require.Equal(t, int64(1), before.RequestCount)
	require.Equal(t, int64(20), before.TotalTokens)
	require.True(t, before.TotalCost.Equal(decimal.NewFromFloat(.75)))
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	reader := ent.NewClient(ent.Driver(b.Driver()), ent.Debug(), ent.Log(func(args ...any) {
		if strings.Contains(strings.ToUpper(fmt.Sprint(args...)), "SUM(") {
			once.Do(func() { close(entered); <-release })
		}
	}))
	qb.ent = reader
	type result struct {
		usage QuotaUsage
		err   error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- result{err: fmt.Errorf("panic: %v", p)}
			}
		}()
		u, e := qb.accountedUsage(ctx, 55, QuotaWindow{})
		done <- result{u, e}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("snapshot barrier missing")
	}
	// Simulate the lost in-memory parent after a process crash: A's new writer
	// reconstructs only the compact parent and atomically consumes durable usage.
	restarted := accountingNode(a, "a")
	require.NoError(t, restarted.recoverPendingUsage(ctx))
	require.NoError(t, restarted.recoverPendingUsage(ctx))
	close(release)
	got := <-done
	require.NoError(t, got.err)
	require.Equal(t, before, got.usage)
	qb.ent = b
	after, err := qb.accountedUsage(ctx, 55, QuotaWindow{})
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, 0, a.PendingUsage.Query().CountX(ctx))
	row := a.UsageLog.Query().Where(usagelog.APIKeyIDEQ(55)).OnlyX(ctx)
	require.True(t, stamp.Equal(row.CreatedAt), "capture instant must survive database timezone decoding")
	require.Equal(t, 1, row.ProjectID)
	require.Equal(t, int64(20), row.TotalTokens)
}

func TestClusterPendingUsageWindowsNullCostAndStandalone(t *testing.T) {
	a, b, ctx := clusterAccountingClients(t)
	wa, wb := accountingNode(a, "a"), accountingNode(b, "b")
	stamp := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	req := &ent.Request{ID: -1, APIKeyID: 7, ProjectID: 1, CreatedAt: stamp, Source: request.SourceAPI, Format: "openai/responses"}
	exec := &ent.RequestExecution{ChannelID: 1, ModelID: "m"}
	for i, at := range []time.Time{stamp.Add(-time.Microsecond), stamp, stamp.Add(30 * time.Minute), stamp.Add(time.Hour)} {
		require.NoError(t, wa.publishPendingUsage(ctx, fmt.Sprint(i), req, exec, llm.Usage{TotalTokens: 5}, at, observationUsageCost{}))
	}
	quota := &QuotaService{ent: b, observation: wb}
	u, err := quota.accountedUsage(ctx, 7, QuotaWindow{Start: &stamp, End: lo.ToPtr(stamp.Add(time.Hour))})
	require.NoError(t, err)
	require.Equal(t, int64(2), u.RequestCount)
	require.Equal(t, int64(10), u.TotalTokens)
	require.True(t, u.TotalCost.IsZero())
	other, err := quota.accountedUsage(ctx, 8, QuotaWindow{})
	require.NoError(t, err)
	require.Zero(t, other.RequestCount)
	// Unknown cost is retained as NULL in the final usage row, not repriced to zero.
	require.NoError(t, wa.handoffPendingUsage(ctx, "1", 0))
	row := a.UsageLog.Query().OnlyX(ctx)
	require.Nil(t, row.TotalCost)
	// Old readers are allowed in standalone operation and retain their old source;
	// rollback must therefore drain durable pending before starting an old reader.
	standalone := &QuotaService{ent: b}
	old, err := standalone.accountedUsage(ctx, 7, QuotaWindow{})
	require.NoError(t, err)
	require.Equal(t, int64(1), old.RequestCount)
	allowed, err := quota.CheckAPIKeyQuota(ctx, 7, nil)
	require.NoError(t, err)
	require.True(t, allowed.Allowed)
}

func TestClusterAccountingAdditiveMigrationAndDrainedRollback(t *testing.T) {
	a, b, ctx := clusterAccountingClients(t)
	// Start from the actual generated schema minus the one additive table. The
	// old standalone quota path remains usable before the new migration runs.
	var result sql.Result
	require.NoError(t, a.Driver().Exec(ctx, "DROP TABLE pending_usages", []any{}, &result))
	req := a.Request.Create().SetProjectID(1).SetAPIKeyID(7).SetModelID("m").SetRequestBody([]byte("{}")).SetStatus(request.StatusCompleted).SaveX(ctx)
	a.UsageLog.Create().SetRequestID(req.ID).SetProjectID(1).SetAPIKeyID(7).SetChannelID(1).SetModelID("m").SetTotalTokens(9).SetSource(usagelog.SourceAPI).SetFormat("openai/responses").SaveX(ctx)
	legacy := &QuotaService{ent: b}
	before, err := legacy.accountedUsage(ctx, 7, QuotaWindow{})
	require.NoError(t, err)
	require.Equal(t, int64(9), before.TotalTokens)
	require.NoError(t, a.Schema.Create(ctx, migrate.WithForeignKeys(false)))
	require.NoError(t, a.Schema.Create(ctx, migrate.WithForeignKeys(false)), "the additive migration is repeatable")
	require.Equal(t, 1, a.UsageLog.Query().CountX(ctx))
	wa, wb := accountingNode(a, "a"), accountingNode(b, "b")
	pending := &ent.Request{APIKeyID: 7, ProjectID: 1, CreatedAt: time.Now().UTC(), Source: request.SourceAPI, Format: "openai/responses"}
	require.NoError(t, wa.publishPendingUsage(ctx, "migration", pending, &ent.RequestExecution{ChannelID: 1, ModelID: "m"}, llm.Usage{TotalTokens: 5}, pending.CreatedAt, observationUsageCost{}))
	clusterQuota := &QuotaService{ent: b, observation: wb}
	current, err := clusterQuota.accountedUsage(ctx, 7, QuotaWindow{})
	require.NoError(t, err)
	require.Equal(t, int64(14), current.TotalTokens)
	// Reconcile with the new writer before switching back to the old reader.
	require.NoError(t, wa.recoverPendingUsage(ctx))
	require.Equal(t, 0, a.PendingUsage.Query().CountX(ctx))
	after, err := legacy.accountedUsage(ctx, 7, QuotaWindow{})
	require.NoError(t, err)
	require.Equal(t, current, after)
}
