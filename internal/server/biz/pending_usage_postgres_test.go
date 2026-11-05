package biz

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/hook"
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
	dsn := clusterAccountingSchema(t)
	open := func() *ent.Client {
		db, err := sql.Open("pgx", dsn)
		require.NoError(t, err)
		return clusterAccountingClient(t, db, 1)
	}
	a, b := open(), open()
	ctx := authz.WithTestBypass(t.Context())
	require.NoError(t, a.Schema.Create(ctx, migrate.WithForeignKeys(false)))
	return a, b, ctx
}

func clusterAccountingClient(t *testing.T, db *sql.DB, maxIdle int) *ent.Client {
	t.Helper()
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(maxIdle)
	c := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func clusterAccountingSchema(t *testing.T) string {
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
	return dsn + separator + "search_path=" + schema
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

// COMMIT really reaches PostgreSQL; only its acknowledgement is lost. The next
// handoff is parked before BEGIN so the other pool can observe any re-publication.
type lostUsageCommitACKDriver struct {
	dialect.Driver
	attempts     atomic.Int32
	retryEntered chan struct{}
	allowRetry   chan struct{}
}

func (d *lostUsageCommitACKDriver) Tx(ctx context.Context) (dialect.Tx, error) {
	switch d.attempts.Add(1) {
	case 1:
		tx, err := d.Driver.Tx(ctx)
		if err != nil {
			return nil, err
		}
		return lostUsageCommitACK{Tx: tx}, nil
	case 2:
		close(d.retryEntered)
		select {
		case <-d.allowRetry:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return d.Driver.Tx(ctx)
}

type lostUsageCommitACK struct{ dialect.Tx }

func (tx lostUsageCommitACK) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	return context.DeadlineExceeded
}

func TestClusterUsageRetryDoesNotRepublishAfterCommit(t *testing.T) {
	for _, losePublicationACK := range []bool{false, true} {
		t.Run(fmt.Sprintf("publication_ack_lost=%v", losePublicationACK), func(t *testing.T) {
			a, b, ctx := clusterAccountingClients(t)
			driver := &lostUsageCommitACKDriver{Driver: a.Driver(), retryEntered: make(chan struct{}), allowRetry: make(chan struct{})}
			client := ent.NewClient(ent.Driver(driver))
			w := accountingNode(client, "gcp")
			w.config.AttemptTimeout = 500 * time.Millisecond
			startClusterUsageTestWriter(t, w, b, ctx)
			parent := b.Request.Create().SetProjectID(1).SetAPIKeyID(55).SetModelID("m").SetRequestBody([]byte("{}")).SetStatus(request.StatusCompleted).SaveX(ctx)
			exec := &ent.RequestExecution{ChannelID: 1, ModelID: "m"}
			requestCtx, cancel := context.WithCancel(w.WithScope(ctx))
			defer cancel()
			defer EndForwardingObservation(requestCtx)
			type frozenUsage struct {
				id       string
				captured time.Time
				usage    llm.Usage
				hasCost  bool
			}
			publications := make(chan frozenUsage, 4)
			client.PendingUsage.Use(func(next ent.Mutator) ent.Mutator {
				return hook.PendingUsageFunc(func(ctx context.Context, m *ent.PendingUsageMutation) (ent.Value, error) {
					if !m.Op().Is(ent.OpCreate) {
						return next.Mutate(ctx, m)
					}
					id, _ := m.ID()
					captured, _ := m.CapturedAt()
					usage, _ := m.Usage()
					_, hasCost := m.TotalCost()
					first := len(publications) == 0
					publications <- frozenUsage{id, captured, usage, hasCost}
					value, err := next.Mutate(ctx, m)
					if first && losePublicationACK && err == nil {
						cancel()
						return nil, context.Canceled
					}
					return value, err
				})
			})
			// Release the barrier even if an assertion fails while inspecting the retry.
			release := sync.OnceFunc(func() { close(driver.allowRetry) })
			defer release()
			usage := &llm.Usage{PromptTokens: 17, CompletionTokens: 3, TotalTokens: 20}
			_, err := w.usageService.CreateUsageLogFromRequest(requestCtx, parent, exec, usage)
			if losePublicationACK {
				var accountingErr *ExecutedAccountingError
				require.ErrorAs(t, err, &accountingErr)
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.NoError(t, err)
			}
			cancel()
			EndForwardingObservation(requestCtx)
			select {
			case <-driver.retryEntered:
			case <-time.After(4 * time.Second):
				t.Fatal("handoff retry did not reach the transaction barrier")
			}

			quota := &QuotaService{ent: b, observation: accountingNode(b, "lag")}
			snapshot, err := quota.accountedUsage(ctx, 55, QuotaWindow{})
			require.NoError(t, err)
			require.Equal(t, int64(20), snapshot.TotalTokens, "unknown handoff ACK must not revive pending in quota")
			require.Equal(t, int64(1), snapshot.RequestCount)
			require.Zero(t, b.PendingUsage.Query().CountX(ctx))
			release()
			drain, stop := context.WithTimeout(ctx, 3*time.Second)
			defer stop()
			require.NoError(t, w.Wait(drain))
			require.NoError(t, w.recoverPendingUsage(ctx))
			require.Equal(t, int32(2), driver.attempts.Load())
			wantPublications := 1
			if losePublicationACK {
				wantPublications = 2 // Reconcile the unknown publication, not the unknown handoff.
			}
			require.Len(t, publications, wantPublications)
			first := <-publications
			if losePublicationACK {
				require.Equal(t, first, <-publications)
			}
			require.Equal(t, *usage, first.usage)
			require.False(t, first.hasCost)
			row := b.UsageLog.Query().OnlyX(ctx)
			require.Equal(t, parent.ID, row.RequestID)
			require.WithinDuration(t, first.captured, row.CreatedAt, time.Microsecond)
			require.Equal(t, int64(20), row.TotalTokens)
			require.Nil(t, row.TotalCost)
		})
	}
}

// Model one WAN round trip per client protocol flight, including connection
// startup. PostgreSQL itself, its transactions and both pools are real.
type accountingWANConn struct {
	net.Conn
	sent, received *atomic.Int64
}

func (c accountingWANConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.received.Add(int64(n))
	return n, err
}

func (c accountingWANConn) Write(b []byte) (int, error) {
	time.Sleep(140 * time.Millisecond)
	n, err := c.Conn.Write(b)
	c.sent.Add(int64(n))
	return n, err
}

func TestClusterUsageColdWANBoundedDrain(t *testing.T) {
	dsn := clusterAccountingSchema(t)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	reader := clusterAccountingClient(t, db, 1)
	ctx := authz.WithTestBypass(t.Context())
	require.NoError(t, reader.Schema.Create(ctx, migrate.WithForeignKeys(false)))
	config, err := pgx.ParseConfig(dsn)
	require.NoError(t, err)
	var dials atomic.Int32
	var sent, received atomic.Int64
	dial := config.DialFunc
	config.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		time.Sleep(140 * time.Millisecond)
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		dials.Add(1)
		return accountingWANConn{Conn: conn, sent: &sent, received: &received}, nil
	}
	// No idle connections guarantees cold startup for the worker's policy read
	// and handoff transaction, even after the synchronous pending publication.
	client := clusterAccountingClient(t, stdlib.OpenDB(*config), 0)
	w := accountingNode(client, "gcp")
	const attemptBudget = 8 * time.Second
	w.config.AttemptTimeout = attemptBudget
	startClusterUsageTestWriter(t, w, reader, ctx)
	var publishes atomic.Int32
	client.PendingUsage.Use(func(next ent.Mutator) ent.Mutator {
		return hook.PendingUsageFunc(func(ctx context.Context, m *ent.PendingUsageMutation) (ent.Value, error) {
			if m.Op().Is(ent.OpCreate) {
				publishes.Add(1)
			}
			return next.Mutate(ctx, m)
		})
	})
	parent := reader.Request.Create().SetProjectID(1).SetAPIKeyID(55).SetModelID("m").SetRequestBody([]byte("{}")).SetStatus(request.StatusCompleted).SaveX(ctx)
	exec := &ent.RequestExecution{ChannelID: 1, ModelID: "m"}
	scopeCtx := w.WithScope(ctx)
	defer EndForwardingObservation(scopeCtx)
	dialsBefore, sentBefore, receivedBefore := dials.Load(), sent.Load(), received.Load()
	start := time.Now()
	_, err = w.usageService.CreateUsageLogFromRequest(scopeCtx, parent, exec, &llm.Usage{PromptTokens: 17, CompletionTokens: 3, TotalTokens: 20})
	require.NoError(t, err)
	EndForwardingObservation(scopeCtx)
	drain, stop := context.WithDeadline(ctx, start.Add(attemptBudget))
	defer stop()
	require.NoError(t, w.Wait(drain))
	require.NoError(t, w.Stop(drain))
	t.Logf("cold WAN publication, handoff and writer stop: %s (%d PostgreSQL connections, %d B sent, %d B received)",
		time.Since(start), dials.Load()-dialsBefore, sent.Load()-sentBefore, received.Load()-receivedBefore)
	require.GreaterOrEqual(t, dials.Load()-dialsBefore, int32(3))
	require.Equal(t, int32(1), publishes.Load())
	require.Zero(t, reader.PendingUsage.Query().CountX(ctx))
	require.Equal(t, int64(20), reader.UsageLog.Query().OnlyX(ctx).TotalTokens)
}

func startClusterUsageTestWriter(t *testing.T, w *ForwardingObservationWriter, reader *ent.Client, ctx context.Context) {
	t.Helper()
	channels := NewChannelService(ChannelServiceParams{Ent: reader, CacheConfig: xcache.Config{Mode: xcache.ModeMemory}})
	t.Cleanup(channels.Stop)
	w.usageService.ChannelService = channels
	require.NoError(t, w.Start(ctx))
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, w.Stop(stop))
	})
}
