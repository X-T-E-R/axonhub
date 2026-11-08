package biz

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestObservationGenerationScopeReservationsAndRetention(t *testing.T) {
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 5, MaxBytesMiB: 1})
	require.NoError(t, w.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, w.Stop(context.Background())) })
	parent := w.WithScope(t.Context())
	EndForwardingObservation(parent)
	var calls atomic.Int32
	for range 3 {
		ctx := NewForwardingObservationScope(parent)
		scope := observationFromContext(ctx)
		require.NotSame(t, observationFromContext(parent), scope)
		require.Same(t, scope, observationFromContext(w.WithScope(ctx)), "ordinary HTTP scope acquisition remains idempotent")
		require.Equal(t, 2, scope.reservedCore, "handshake credits were returned before generation admission")
		require.False(t, scope.usageSubmitted)
		release := RetainForwardingObservation(ctx)
		EndForwardingObservation(ctx)
		require.False(t, scope.ended)
		for range 2 {
			id := scope.newID()
			require.NoError(t, scope.submitCore(ctx, id, 0, func(context.Context) error { calls.Add(1); return nil }))
			require.NoError(t, scope.submitTerminal(ctx, id, 0, func(context.Context) error { calls.Add(1); return nil }))
		}
		for range 2 {
			require.NoError(t, scope.submitUsage(ctx, 0, func(context.Context) error { calls.Add(1); return nil }))
		}
		release()
		release()
		drain, cancel := context.WithTimeout(context.Background(), time.Second)
		require.NoError(t, w.Wait(drain))
		cancel()
		require.True(t, scope.ended)
		require.Empty(t, w.scopes)
		require.Zero(t, w.items)
		require.Zero(t, w.bytes)
	}
	require.Equal(t, int32(15), calls.Load(), "usage is deduplicated within each generation, not across generations")
	require.Same(t, t.Context(), NewForwardingObservationScope(t.Context()), "no writer keeps the normal orchestrator path")
}

func TestObservationGenerationResolvesPendingTransportAssociations(t *testing.T) {
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 16, AttemptTimeout: 3 * time.Second})
	require.NoError(t, w.Start(t.Context()))
	parentCtx := w.WithScope(t.Context())
	parent := observationFromContext(parentCtx)
	metadataID := parent.newID()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, w.Stop(stop))
	})
	require.NoError(t, parent.submit(parentCtx, 0, func(context.Context) error {
		close(entered)
		<-release
		parent.bindID(metadataID, 456)
		return nil
	}))
	<-entered
	EndForwardingObservation(parentCtx)
	var resolved atomic.Int32
	var children []*observationScope
	for range 2 {
		ctx := NewForwardingObservationScope(parentCtx)
		scope := observationFromContext(ctx)
		children = append(children, scope)
		id := scope.newID()
		require.NoError(t, scope.submitCore(ctx, id, 0, func(context.Context) error {
			actual, err := scope.resolve(metadataID)
			if err != nil {
				return err
			}
			require.Equal(t, 456, actual)
			scope.bindID(id, 1000+int(resolved.Add(1)))
			return nil
		}))
		EndForwardingObservation(ctx)
	}
	once.Do(func() { close(release) })
	drain, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, w.Wait(drain))
	require.Equal(t, int32(2), resolved.Load())
	require.Len(t, parent.ids, 1, "generation IDs must not accumulate on the transport")
	for _, child := range children {
		require.Len(t, child.ids, 1)
		for _, sibling := range children {
			if child == sibling {
				continue
			}
			for id := range sibling.ids {
				_, err := child.resolve(id)
				require.ErrorIs(t, err, errObservationParentUnavailable)
			}
		}
	}
}

func TestObservationGenerationAdmissionIsIndependentOfInference(t *testing.T) {
	legacy, client, base, project := setupRequestExecutionStorageTest(t)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 5, MaxBytesMiB: 1})
	require.NoError(t, w.Start(base))
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, w.Stop(stop))
	})
	svc := NewRequestServiceWithObservationWriter(client, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, w)
	parent := w.WithScope(contexts.WithProjectID(base, project.ID))
	release := RetainForwardingObservation(parent)
	t.Cleanup(release)
	EndForwardingObservation(parent)
	quota := &objects.APIKeyQuota{Requests: lo.ToPtr(int64(5)), Period: objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime}}
	key := client.APIKey.Create().SetName("limited").SetKey("synthetic-generation-quota").SetType(apikey.TypeUser).SetProjectID(project.ID).
		SetProfiles(&objects.APIKeyProfiles{ActiveProfile: "limited", Profiles: []objects.APIKeyProfile{{Name: "limited", Quota: quota}}}).SaveX(base)
	blocked := NewForwardingObservationScope(parent)
	require.False(t, observationFromContext(blocked).reservedUsage)
	for _, apiKey := range []*ent.APIKey{key, nil} {
		req, err := svc.CreateRequest(contexts.WithAPIKey(blocked, apiKey), &llm.Request{Model: "m"}, &httpclient.Request{}, llm.APIFormatOpenAIResponse)
		require.NoError(t, err, "a generation can forward without observation capacity")
		require.Negative(t, req.ID)
	}
	EndForwardingObservation(blocked)
	release()
	ctx := NewForwardingObservationScope(parent)
	request, err := svc.CreateRequest(contexts.WithAPIKey(ctx, key), &llm.Request{Model: "quota"}, &httpclient.Request{}, llm.APIFormatOpenAIResponse)
	require.NoError(t, err, "new generation can reserve only after the retained owner releases capacity")
	require.Negative(t, request.ID)
	EndForwardingObservation(ctx)
	drain, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, w.Wait(drain))
	require.Equal(t, 1, client.Request.Query().CountX(base))
}
