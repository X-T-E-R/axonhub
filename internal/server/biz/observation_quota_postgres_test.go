package biz

import (
	"context"
	stdsql "database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/migrate"
	"github.com/samber/lo"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestAccountedUsagePostgresCommitAfterSnapshot(t *testing.T) {
	dsn := os.Getenv("AXONHUB_TEST_PG_DSN")
	if dsn == "" || os.Getenv("AXONHUB_TEST_PG_DISPOSABLE") != "1" {
		t.Skip("requires disposable PostgreSQL")
	}
	db, err := stdsql.Open("pgx", dsn)
	require.NoError(t, err)
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.Postgres, db)))
	defer client.Close()
	ctx := authz.WithTestBypass(context.Background())
	// This isolated aggregate test needs no business foreign-key fixtures.
	require.NoError(t, client.Schema.Create(ctx, migrate.WithForeignKeys(false)))
	w := newForwardingObservationLane(ManagedRequestBodyWriterConfig{})
	stamp := time.Now().UTC().Add(-time.Minute)
	pending := &observationPendingUsage{apiKeyID: 55, createdAt: stamp, tokens: 20, cost: lo.ToPtr(0.75)}
	w.pendingRecords[pending] = struct{}{}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	reader := ent.NewClient(ent.Driver(client.Driver()), ent.Debug(), ent.Log(func(values ...any) {
		if strings.Contains(strings.ToUpper(fmt.Sprint(values...)), "SUM(") {
			once.Do(func() { close(entered); <-release })
		}
	}))
	quota := &QuotaService{ent: reader, observation: w}
	result := make(chan struct {
		usage QuotaUsage
		err   error
	}, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				result <- struct {
					usage QuotaUsage
					err   error
				}{err: fmt.Errorf("panic: %v", p)}
			}
		}()
		usage, err := quota.accountedUsage(ctx, 55, QuotaWindow{})
		result <- struct {
			usage QuotaUsage
			err   error
		}{usage, err}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("snapshot aggregate barrier not entered")
	}
	// Publish ID before INSERT, as the real worker does. Commit and remove the
	// pending entry after the reader has established its repeatable snapshot.
	pending.requestID.Store(1001)
	createObservationQuotaUsage(t, ctx, client, 1001, 55, stamp, 20, 0.75)
	w.mu.Lock()
	delete(w.pendingRecords, pending)
	w.mu.Unlock()
	close(release)
	select {
	case got := <-result:
		require.NoError(t, got.err)
		require.Equal(t, int64(1), got.usage.RequestCount)
		require.Equal(t, int64(20), got.usage.TotalTokens)
		require.True(t, got.usage.TotalCost.Equal(decimal.NewFromFloat(0.75)))
	case <-time.After(3 * time.Second):
		t.Fatal("snapshot check did not finish")
	}
}
