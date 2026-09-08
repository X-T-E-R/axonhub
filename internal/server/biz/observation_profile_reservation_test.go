package biz

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/hook"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
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
	remaining := w.optionalByteLimit() - w.optionalBytes
	w.mu.Unlock()
	// Consume every optional byte without releasing the blocked core job.
	padding := remaining - 4096 - observationContextBytes(detachedObservationContext(context.Background()))
	require.Positive(t, padding)
	require.NoError(t, scope.submit(context.Background(), padding, func(context.Context) error { return nil }))
	w.mu.Lock()
	require.Equal(t, w.optionalByteLimit(), w.optionalBytes)
	w.mu.Unlock()
	_, err = svc.UsageLogService.CreateUsageLogFromRequest(ctx, parent, execution, &llm.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5})
	require.NoError(t, err, "usage must consume its reserved credit even with large profiles and no optional bytes")
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

func TestForwardingObservationLargeProfilesAfterOptionalSaturation(t *testing.T) {
	for _, limited := range []bool{false, true} {
		for _, saving := range []bool{false, true} {
			t.Run(fmt.Sprintf("quota=%t/saving=%t", limited, saving), func(t *testing.T) {
				legacy, client, base, project := setupRequestExecutionStorageTest(t)
				defer client.Close()
				require.NoError(t, legacy.SystemService.SetStoragePolicy(base, &StoragePolicy{StoreRequestBody: saving, StoreExecutionRequestBody: lo.ToPtr(saving), StoreResponseBody: saving}))
				var quota *objects.APIKeyQuota
				if limited {
					quota = &objects.APIKeyQuota{Requests: lo.ToPtr(int64(1)), Period: objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime}}
				}
				profiles := &objects.APIKeyProfiles{ActiveProfile: "large", Profiles: []objects.APIKeyProfile{{Name: "large", ModelIDs: []string{strings.Repeat("x", 70<<10)}, Quota: quota}}}
				key := client.APIKey.Create().SetProjectID(project.ID).SetName("saturated-profile").SetKey("test-saturated-profile").SetProfiles(profiles).SaveX(base)
				expected := cloneObservationAPIKey(key).Profiles
				w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{AttemptTimeout: 10 * time.Second})
				svc := NewRequestServiceWithObservationWriter(client, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, w)
				require.NoError(t, w.Start(base))
				channel := createStorageTestChannel(t, base, client, nil)
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				unblock := func() { once.Do(func() { close(release) }) }
				pressure := w.WithScope(context.Background())
				var ctx context.Context
				defer func() {
					unblock()
					EndForwardingObservation(pressure)
					if ctx != nil {
						EndForwardingObservation(ctx)
					}
					stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					_ = w.Stop(stop)
				}()
				padding := w.optionalByteLimit() - 4096 - observationContextBytes(detachedObservationContext(context.Background()))
				require.NoError(t, observationFromContext(pressure).submit(context.Background(), padding, func(ctx context.Context) error {
					close(entered)
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}))
				<-entered
				ctx = w.WithScope(contexts.WithAPIKey(contexts.WithProjectID(base, project.ID), key))
				quotaService := NewQuotaServiceWithObservationWriter(client, legacy.SystemService, w)
				decision, err := quotaService.CheckAPIKeyQuota(ctx, key.ID, quota)
				require.NoError(t, err)
				require.True(t, decision.Allowed, "optional pressure must not block a fresh reserved quota scope")
				parent, err := svc.CreateRequest(ctx, &llm.Request{Model: "large-profile"}, &httpclient.Request{JSONBody: []byte(`{"ok":true}`)}, llm.APIFormatOpenAIChatCompletion)
				require.NoError(t, err)
				execution, err := svc.CreateRequestExecution(ctx, channel, "large-profile", parent, httpclient.Request{JSONBody: []byte(`{"ok":true}`)}, llm.APIFormatOpenAIChatCompletion, false)
				require.NoError(t, err)
				_, err = svc.UsageLogService.CreateUsageLogFromRequest(ctx, parent, execution, &llm.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5})
				require.NoError(t, err)
				require.NoError(t, svc.UpdateRequestExecutionCompleted(ctx, execution.ID, "response", []byte(`{"ok":true}`), nil))
				require.NoError(t, svc.UpdateRequestCompleted(ctx, parent.ID, "response", []byte(`{"ok":true}`), nil))
				pending, err := quotaService.accountedUsage(base, key.ID, QuotaWindow{})
				require.NoError(t, err)
				require.EqualValues(t, 1, pending.RequestCount)
				require.EqualValues(t, 5, pending.TotalTokens)
				decision, err = quotaService.CheckAPIKeyQuota(base, key.ID, quota)
				require.NoError(t, err)
				require.Equal(t, !limited, decision.Allowed)
				w.mu.Lock()
				require.Equal(t, w.optionalByteLimit(), w.optionalBytes, "profiles must use metadata, not optional bytes")
				require.LessOrEqual(t, w.bytes, int64(64<<20))
				require.LessOrEqual(t, w.items, w.config.MaxItems)
				w.mu.Unlock()
				key.Profiles.Profiles[0].ModelIDs[0] = "mutated-after-capture"
				EndForwardingObservation(ctx)
				EndForwardingObservation(pressure)
				unblock()
				drain, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				require.NoError(t, w.Wait(drain))
				actual, err := client.Request.Query().Only(base)
				require.NoError(t, err)
				require.Equal(t, expected, actual.RoutingContext.EffectiveProfiles)
				canonical, err := canonicalJSON(expected)
				require.NoError(t, err)
				digest := sha256.Sum256(canonical)
				require.Equal(t, hex.EncodeToString(digest[:]), actual.RoutingContext.EffectiveProfilesSHA256)
				require.Equal(t, request.StatusCompleted, actual.Status)
				actualExecution, err := client.RequestExecution.Query().Only(base)
				require.NoError(t, err)
				require.Equal(t, requestexecution.StatusCompleted, actualExecution.Status)
				require.Equal(t, actual.ID, actualExecution.RequestID)
				usage, err := client.UsageLog.Query().Only(base)
				require.NoError(t, err)
				require.Equal(t, actual.ID, usage.RequestID)
				outcome := "omitted"
				if saving {
					outcome = "unavailable"
				}
				require.Equal(t, outcome, actual.EvidenceDisposition.RequestBody.Outcome)
				require.Equal(t, outcome, actual.EvidenceDisposition.ResponseBody.Outcome)
				require.Equal(t, outcome, actualExecution.EvidenceDisposition.RequestBody.Outcome)
				require.Equal(t, outcome, actualExecution.EvidenceDisposition.ResponseBody.Outcome)
				w.mu.Lock()
				require.Zero(t, w.items)
				require.Zero(t, w.bytes)
				require.Zero(t, w.optionalItems)
				require.Zero(t, w.optionalBytes)
				require.Empty(t, w.pendingRecords)
				w.mu.Unlock()
			})
		}
	}
}
