package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/hook"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
)

func TestForwardingObservationLargeProfilesPreserveReservedTerminalAndUsage(t *testing.T) {
	legacy, client, base, project := setupRequestExecutionStorageTest(t)
	defer client.Close()
	quota := &objects.APIKeyQuota{Requests: lo.ToPtr(int64(1)), Period: objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime}}
	profiles := &objects.APIKeyProfiles{ActiveProfile: "large", Profiles: []objects.APIKeyProfile{{Name: "large", ModelIDs: []string{strings.Repeat("x", 70<<10)}, Quota: quota}}}
	key := client.APIKey.Create().SetProjectID(project.ID).SetName("isolated-large-profile").SetKey("isolated-test-only").SetProfiles(profiles).SaveX(base)
	expected := cloneObservationAPIKey(key).Profiles
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 12, MaxBytesMiB: 1, AttemptTimeout: 5 * time.Second})
	svc := NewRequestServiceWithObservationWriter(client, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, w)
	require.NoError(t, w.Start(base))
	ctx := w.WithScope(contexts.WithAPIKey(contexts.WithProjectID(base, project.ID), key))
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		EndForwardingObservation(ctx)
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.Stop(stop)
	})
	client.Request.Use(func(next ent.Mutator) ent.Mutator {
		return hook.RequestFunc(func(ctx context.Context, m *ent.RequestMutation) (ent.Value, error) {
			if m.Op().Is(ent.OpCreate) {
				enterOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
			return next.Mutate(ctx, m)
		})
	})
	parent, err := svc.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: []byte(`{"model":"gpt-4o"}`)}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("core INSERT barrier not entered")
	}
	channel := createStorageTestChannel(t, base, client, nil)
	execution, err := svc.CreateRequestExecution(ctx, channel, "gpt-4o", parent, httpclient.Request{JSONBody: []byte(`{"model":"gpt-4o"}`)}, llm.APIFormatOpenAIChatCompletion, false)
	require.NoError(t, err)
	scope := observationFromContext(ctx)
	w.mu.Lock()
	remaining := (int64(1) << 20) - w.bytes
	w.mu.Unlock()
	// Consume every unreserved byte without releasing the blocked core job.
	padding := remaining - 4096 - observationContextBytes(detachedObservationContext(context.Background()))
	require.Positive(t, padding)
	require.NoError(t, scope.submit(context.Background(), padding, func(context.Context) error { return nil }))
	w.mu.Lock()
	require.Equal(t, int64(1)<<20, w.bytes)
	w.mu.Unlock()
	_, err = svc.UsageLogService.CreateUsageLogFromRequest(ctx, parent, execution, &llm.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5})
	require.NoError(t, err, "usage must consume its reserved credit even with large profiles and no free bytes")
	require.NoError(t, svc.UpdateRequestExecutionCompletedForChannel(ctx, execution.ID, "isolated-response", []byte(`{"ok":true}`), nil, channel))
	require.NoError(t, svc.UpdateRequestCompleted(ctx, parent.ID, "isolated-response", []byte(`{"ok":true}`), nil))
	quotaService := NewQuotaServiceWithObservationWriter(client, legacy.SystemService, w)
	pending, err := quotaService.accountedUsage(base, key.ID, QuotaWindow{})
	require.NoError(t, err)
	require.EqualValues(t, 1, pending.RequestCount)
	require.EqualValues(t, 5, pending.TotalTokens)
	decision, err := quotaService.CheckAPIKeyQuota(base, key.ID, quota)
	require.NoError(t, err)
	require.False(t, decision.Allowed, "pending usage must enforce the real quota before storage is released")
	key.Profiles.Profiles[0].ModelIDs[0] = "changed-after-admission"
	EndForwardingObservation(ctx)
	unblock()
	drain, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, w.Stop(drain))
	actual := client.Request.Query().OnlyX(base)
	require.Equal(t, request.StatusCompleted, actual.Status)
	require.Equal(t, expected, actual.RoutingContext.EffectiveProfiles)
	canonical, err := canonicalJSON(expected)
	require.NoError(t, err)
	digest := sha256.Sum256(canonical)
	require.Equal(t, hex.EncodeToString(digest[:]), actual.RoutingContext.EffectiveProfilesSHA256)
	actualExecution := client.RequestExecution.Query().OnlyX(base)
	require.Equal(t, requestexecution.StatusCompleted, actualExecution.Status)
	require.Equal(t, actual.ID, actualExecution.RequestID)
	require.Equal(t, actual.ID, client.UsageLog.Query().OnlyX(base).RequestID)
	w.mu.Lock()
	require.Zero(t, w.items)
	require.Zero(t, w.bytes)
	require.Empty(t, w.pendingRecords)
	w.mu.Unlock()
}
