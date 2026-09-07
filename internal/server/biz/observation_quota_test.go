package biz

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
)

func newObservationQuotaTestClient(t *testing.T) (*ent.Client, context.Context) {
	t.Helper()
	name := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	client := enttest.NewEntClient(t, "sqlite3", fmt.Sprintf("file:observation-quota-%s?mode=memory&_fk=0", name))
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	return client, ctx
}

func createObservationQuotaUsage(t *testing.T, ctx context.Context, client *ent.Client, requestID, apiKeyID int, createdAt time.Time, totalTokens int64, totalCost float64) *ent.UsageLog {
	t.Helper()
	usage, err := client.UsageLog.Create().
		SetRequestID(requestID).
		SetAPIKeyID(apiKeyID).
		SetModelID("quota-test-model").
		SetPromptTokens(totalTokens).
		SetTotalTokens(totalTokens).
		SetTotalCost(totalCost).
		SetCreatedAt(createdAt).
		Save(ctx)
	require.NoError(t, err)
	return usage
}

func TestAccountedUsageWindowAPIKeyIsolationAndCostUnits(t *testing.T) {
	client, ctx := newObservationQuotaTestClient(t)
	defer client.Close()

	base := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	window := QuotaWindow{
		Start:        lo.ToPtr(base),
		End:          lo.ToPtr(base.Add(time.Hour)),
		EndInclusive: false,
	}
	createObservationQuotaUsage(t, ctx, client, 101, 7, base, 1_000_000, 0.125)
	createObservationQuotaUsage(t, ctx, client, 102, 7, base.Add(30*time.Minute), 500_000, 0.375)
	createObservationQuotaUsage(t, ctx, client, 103, 7, base.Add(time.Hour), 99, 9.0)
	createObservationQuotaUsage(t, ctx, client, 104, 7, base.Add(-time.Second), 88, 8.0)
	createObservationQuotaUsage(t, ctx, client, 105, 8, base.Add(30*time.Minute), 700, 7.0)

	quota := &QuotaService{ent: client}
	usage, err := quota.accountedUsage(ctx, 7, window)
	require.NoError(t, err)
	require.Equal(t, int64(2), usage.RequestCount)
	require.Equal(t, int64(1_500_000), usage.TotalTokens)
	require.True(t, usage.TotalCost.Equal(decimal.NewFromFloat(0.5)), usage.TotalCost)

	otherKeyUsage, err := quota.accountedUsage(ctx, 8, window)
	require.NoError(t, err)
	require.Equal(t, int64(1), otherKeyUsage.RequestCount)
	require.Equal(t, int64(700), otherKeyUsage.TotalTokens)
	require.True(t, otherKeyUsage.TotalCost.Equal(decimal.NewFromFloat(7.0)), otherKeyUsage.TotalCost)
}

func TestAccountedUsageDoesNotDoubleCountPersistedPendingRecord(t *testing.T) {
	client, ctx := newObservationQuotaTestClient(t)
	defer client.Close()

	createdAt := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	row := createObservationQuotaUsage(t, ctx, client, 201, 42, createdAt, 12, 0.25)
	writer := newForwardingObservationLane(ManagedRequestBodyWriterConfig{MaxItems: 8, MaxBytesMiB: 1})
	persisted := &observationPendingUsage{apiKeyID: 42, createdAt: createdAt, tokens: 12, cost: lo.ToPtr(0.25)}
	persisted.requestID.Store(int64(row.RequestID))
	unbound := &observationPendingUsage{apiKeyID: 42, createdAt: createdAt, tokens: 3, cost: lo.ToPtr(0.125)}
	otherKey := &observationPendingUsage{apiKeyID: 99, createdAt: createdAt, tokens: 100, cost: lo.ToPtr(100.0)}
	writer.mu.Lock()
	writer.pendingRecords[persisted] = struct{}{}
	writer.pendingRecords[unbound] = struct{}{}
	writer.pendingRecords[otherKey] = struct{}{}
	writer.mu.Unlock()

	quota := &QuotaService{ent: client, observation: writer}
	usage, err := quota.accountedUsage(ctx, 42, QuotaWindow{End: lo.ToPtr(createdAt.Add(time.Minute)), EndInclusive: true})
	require.NoError(t, err)
	require.Equal(t, int64(2), usage.RequestCount)
	require.Equal(t, int64(15), usage.TotalTokens)
	require.True(t, usage.TotalCost.Equal(decimal.NewFromFloat(0.375)), usage.TotalCost)
}

func TestAccountedUsageRetainsPendingSnapshotAcrossRemovalAndIDPublication(t *testing.T) {
	client, ctx := newObservationQuotaTestClient(t)
	defer client.Close()

	createdAt := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	row := createObservationQuotaUsage(t, ctx, client, 301, 55, createdAt, 20, 0.75)
	writer := newForwardingObservationLane(ManagedRequestBodyWriterConfig{MaxItems: 8, MaxBytesMiB: 1})
	pending := &observationPendingUsage{apiKeyID: 55, createdAt: createdAt, tokens: 20, cost: lo.ToPtr(0.75)}
	writer.mu.Lock()
	writer.pendingRecords[pending] = struct{}{}
	writer.mu.Unlock()

	aggregateStarted := make(chan struct{})
	releaseAggregate := make(chan struct{})
	var aggregateOnce sync.Once
	debugClient := ent.NewClient(
		ent.Driver(client.Driver()),
		ent.Debug(),
		ent.Log(func(values ...any) {
			query := strings.ToUpper(fmt.Sprint(values...))
			if strings.Contains(query, "SUM(") {
				aggregateOnce.Do(func() {
					close(aggregateStarted)
					<-releaseAggregate
				})
			}
		}),
	)
	quota := &QuotaService{ent: debugClient, observation: writer}
	window := QuotaWindow{Start: lo.ToPtr(createdAt.Add(-time.Minute)), End: lo.ToPtr(createdAt.Add(time.Minute)), EndInclusive: true}
	result := make(chan struct {
		usage QuotaUsage
		err   error
	}, 1)
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				result <- struct {
					usage QuotaUsage
					err   error
				}{err: fmt.Errorf("accounted usage panicked: %v", recovered)}
			}
		}()
		usage, err := quota.accountedUsage(ctx, 55, window)
		result <- struct {
			usage QuotaUsage
			err   error
		}{usage: usage, err: err}
	}()

	select {
	case <-aggregateStarted:
	case <-time.After(time.Second):
		t.Fatal("usage aggregate query did not reach synchronization point")
	}
	writer.mu.Lock()
	delete(writer.pendingRecords, pending)
	writer.mu.Unlock()
	pending.requestID.Store(int64(row.RequestID))
	close(releaseAggregate)

	select {
	case output := <-result:
		require.NoError(t, output.err)
		require.Equal(t, int64(1), output.usage.RequestCount)
		require.Equal(t, int64(20), output.usage.TotalTokens)
		require.True(t, output.usage.TotalCost.Equal(decimal.NewFromFloat(0.75)), output.usage.TotalCost)
	case <-time.After(time.Second):
		t.Fatal("accounted usage did not complete")
	}
}
