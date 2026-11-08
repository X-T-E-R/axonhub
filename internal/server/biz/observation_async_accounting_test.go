package biz

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestObservationUnavailableWriterDoesNotGateInferenceOrQuota(t *testing.T) {
	for _, state := range []string{"not_started", "full", "stopped"} {
		t.Run(state, func(t *testing.T) {
			legacy, client, base, project := setupRequestExecutionStorageTest(t)
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 1})
			svc := NewRequestServiceWithObservationWriter(client, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, w)
			var held context.Context
			if state != "not_started" {
				require.NoError(t, w.Start(base))
			}
			if state == "stopped" {
				require.NoError(t, w.Stop(base))
			} else if state == "full" {
				held = w.WithScope(base)
				t.Cleanup(func() { EndForwardingObservation(held) })
			}
			ctx := w.WithScope(contexts.WithProjectID(base, project.ID))
			t.Cleanup(func() {
				EndForwardingObservation(ctx)
				if held != nil {
					EndForwardingObservation(held)
				}
				require.NoError(t, w.Stop(base))
			})
			parent, err := svc.CreateRequest(ctx, &llm.Request{Model: "m"}, &httpclient.Request{}, llm.APIFormatOpenAIResponse)
			require.NoError(t, err)
			require.Negative(t, parent.ID)
			_, err = svc.UsageLogService.CreateUsageLogFromRequest(ctx, parent, &ent.RequestExecution{ChannelID: 1, ModelID: "m"}, &llm.Usage{TotalTokens: 5})
			require.ErrorIs(t, err, errObservationQueueUnavailable)
			require.Zero(t, client.UsageLog.Query().CountX(base), "no usage row is invented after rejection")

			quota := NewQuotaServiceWithObservationWriter(client, legacy.SystemService, w)
			limit := &objects.APIKeyQuota{Requests: lo.ToPtr(int64(1)), Period: objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime}}
			decision, err := quota.CheckAPIKeyQuota(ctx, 7, limit)
			require.NoError(t, err)
			require.True(t, decision.Allowed)
			limit.Requests = lo.ToPtr(int64(0))
			decision, err = quota.CheckAPIKeyQuota(ctx, 7, limit)
			require.NoError(t, err)
			require.False(t, decision.Allowed, "the explicit user limit remains authoritative")
			client.UsageLog.Intercept(ent.InterceptFunc(func(next ent.Querier) ent.Querier {
				return ent.QuerierFunc(func(context.Context, ent.Query) (ent.Value, error) {
					return nil, errors.New("authoritative quota read failed")
				})
			}))
			_, err = quota.CheckAPIKeyQuota(ctx, 7, limit)
			require.ErrorContains(t, err, "authoritative quota read failed")
		})
	}
}

func TestClusterQuotaCountsQueuedUsageOnceAcrossPublication(t *testing.T) {
	legacy, client, base, project := setupRequestExecutionStorageTest(t)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 16, AttemptTimeout: 5 * time.Second})
	svc := NewRequestServiceWithObservationWriter(client, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, w)
	w.node = cluster.Config{Enabled: true, NodeID: "fixture"}
	require.NoError(t, w.Start(base))
	ctx := w.WithScope(contexts.WithProjectID(base, project.ID))
	scope := observationFromContext(ctx)
	entered, release := make(chan struct{}), make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	t.Cleanup(func() {
		unblock()
		EndForwardingObservation(ctx)
		stop, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.NoError(t, w.Stop(stop))
	})
	require.NoError(t, scope.submitMetadata(base, 0, func(context.Context) error {
		close(entered)
		<-release
		return nil
	}))
	<-entered
	stamp := time.Now().UTC()
	parent := &ent.Request{ID: -1, ProjectID: project.ID, APIKeyID: 7, ModelID: "m", Source: "api", Format: "openai/responses", CreatedAt: stamp}
	exec := &ent.RequestExecution{ChannelID: 1, ModelID: "m"}
	usage := &llm.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}
	_, err := svc.UsageLogService.CreateUsageLogFromRequest(ctx, parent, exec, usage)
	require.NoError(t, err)
	_, err = svc.UsageLogService.CreateUsageLogFromRequest(ctx, parent, exec, &llm.Usage{TotalTokens: 999})
	require.NoError(t, err)
	quota := NewQuotaServiceWithObservationWriter(client, legacy.SystemService, w)
	check := func() {
		t.Helper()
		got, err := quota.accountedUsage(base, 7, QuotaWindow{})
		require.NoError(t, err)
		require.EqualValues(t, 1, got.RequestCount)
		require.EqualValues(t, 5, got.TotalTokens)
		require.True(t, got.TotalCost.IsZero())
		decision, err := quota.CheckAPIKeyQuota(ctx, 7, &objects.APIKeyQuota{TotalTokens: lo.ToPtr(int64(5)), Period: objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime}})
		require.NoError(t, err)
		require.False(t, decision.Allowed)
	}
	check() // The worker has not published anything; the local fact still counts.
	require.NoError(t, w.publishPendingUsage(base, scope.usageID, parent, exec, *usage, stamp, observationUsageCost{}))
	check() // Memory and durable pending contain the same UUID, counted once.
	EndForwardingObservation(ctx)
	unblock()
	drain, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	require.NoError(t, w.Wait(drain))
	check() // Atomic handoff consumed pending and the final row still counts once.
	require.Zero(t, client.PendingUsage.Query().CountX(base))
	require.Nil(t, client.UsageLog.Query().OnlyX(base).TotalCost)
}
