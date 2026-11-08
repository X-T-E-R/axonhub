package biz

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/hook"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestObservationMetadataFailureDoesNotBlockRequiredUsage(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%v", timeout), func(t *testing.T) {
			w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{AttemptTimeout: 10 * time.Millisecond, MaxAttempts: 2})
			require.NoError(t, w.Start(t.Context()))
			ctx := w.WithScope(t.Context())
			t.Cleanup(func() {
				EndForwardingObservation(ctx)
				stop, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = w.Stop(stop)
			})
			scope := observationFromContext(ctx)
			var metadata, terminal, missingParent, usage atomic.Int32
			require.NoError(t, scope.submitMetadata(ctx, 0, func(ctx context.Context) error {
				metadata.Add(1)
				if timeout {
					<-ctx.Done()
					return ctx.Err()
				}
				return errors.New("unrecoverable optional metadata")
			}))
			require.NoError(t, scope.submitCore(ctx, -1, 0, func(context.Context) error {
				missingParent.Add(1)
				return errObservationParentUnavailable
			}))
			require.NoError(t, scope.submitTerminal(ctx, -1, 0, func(ctx context.Context) error {
				terminal.Add(1)
				if timeout {
					<-ctx.Done()
					return ctx.Err()
				}
				return errors.New("unrecoverable optional terminal")
			}))
			require.NoError(t, scope.submitUsage(ctx, 0, func(context.Context) error {
				if usage.Add(1) <= 4 {
					return errors.New("accounting unavailable until recovery")
				}
				return nil
			}))
			EndForwardingObservation(ctx)
			drain, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			require.NoError(t, w.Wait(drain), "optional metadata must not permanently occupy the required FIFO")
			require.Equal(t, int32(2), metadata.Load())
			require.Equal(t, int32(2), terminal.Load())
			require.Equal(t, int32(1), missingParent.Load(), "a known missing parent was already non-retriable")
			require.Equal(t, int32(5), usage.Load(), "required facts must survive more than MaxAttempts")
		})
	}
}

func TestObservationAccountingCapacityDoesNotGateRequest(t *testing.T) {
	legacy, client, base, project := setupRequestExecutionStorageTest(t)
	defer client.Close()
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 5})
	svc := NewRequestServiceWithObservationWriter(client, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, w)
	w.node = cluster.Config{Enabled: true, NodeID: "test"}
	require.NoError(t, w.Start(base))
	first := w.WithScope(base) // All five lifecycle credits remain held.
	second := w.WithScope(contexts.WithProjectID(base, project.ID))
	defer func() {
		EndForwardingObservation(first)
		EndForwardingObservation(second)
		require.NoError(t, w.Stop(context.Background()))
	}()
	parent, err := svc.CreateRequest(second, &llm.Request{Model: "m"}, &httpclient.Request{}, llm.APIFormatOpenAIResponse)
	require.NoError(t, err)
	_, err = svc.UsageLogService.CreateUsageLogFromRequest(second, parent, &ent.RequestExecution{ChannelID: 1, ModelID: "m"}, &llm.Usage{TotalTokens: 5})
	require.ErrorIs(t, err, errObservationQueueUnavailable, "the capture caller can observe a dropped fact")
	require.Zero(t, client.PendingUsage.Query().CountX(base))
	require.Zero(t, client.UsageLog.Query().CountX(base))
}

func TestObservationPreservesRequiredVideoTaskIdentity(t *testing.T) {
	legacy, client, base, project := setupRequestExecutionStorageTest(t)
	defer client.Close()
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 8, MaxAttempts: 1, AttemptTimeout: 100 * time.Millisecond})
	svc := NewRequestServiceWithObservationWriter(client, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, w)
	require.NoError(t, w.Start(base))
	ctx := w.WithScope(contexts.WithProjectID(base, project.ID))
	defer func() {
		EndForwardingObservation(ctx)
		stop, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, w.Stop(stop))
	}()
	video := &llm.Request{Model: "video", RequestType: llm.RequestTypeVideo}
	usageOnly := w.WithScope(base)
	defer EndForwardingObservation(usageOnly)
	_, err := svc.CreateRequest(usageOnly, video, &httpclient.Request{}, llm.APIFormatOpenAIVideo)
	require.ErrorContains(t, err, "video task identity admission unavailable")
	EndForwardingObservation(usageOnly)
	ch := createStorageTestChannel(t, base, client, nil)
	client.System.Intercept(ent.InterceptFunc(func(next ent.Querier) ent.Querier {
		return ent.QuerierFunc(func(ctx context.Context, q ent.Query) (ent.Value, error) {
			value, err := next.Query(ctx, q)
			if rows, ok := value.([]*ent.System); err == nil && ok && len(rows) == 1 && rows[0].Key == SystemKeyStoragePolicy {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return value, err
		})
	}))
	var attempts atomic.Int32
	client.Request.Use(func(next ent.Mutator) ent.Mutator {
		return hook.RequestFunc(func(ctx context.Context, m *ent.RequestMutation) (ent.Value, error) {
			if m.Op().Is(ent.OpCreate) && attempts.Add(1) == 1 {
				return nil, errors.New("transient task identity INSERT error")
			}
			return next.Mutate(ctx, m)
		})
	})
	parent, err := svc.CreateRequest(ctx, video, &httpclient.Request{Body: []byte(`{"prompt":"optional"}`)}, llm.APIFormatOpenAIVideo)
	require.NoError(t, err)
	_, err = svc.CreateRequestExecution(ctx, ch, "video", parent, httpclient.Request{}, llm.APIFormatOpenAIVideo, false)
	require.NoError(t, err)
	require.NoError(t, svc.UpdateRequestChannelID(ctx, parent.ID, ch.ID))
	require.NoError(t, svc.UpdateRequestStatusExternalIDAndResponseBody(ctx, parent.ID, request.StatusProcessing, "opaque-video-task", []byte(`{"optional":true}`), nil))
	EndForwardingObservation(ctx)
	drain, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, w.Wait(drain))
	require.EqualValues(t, 2, attempts.Load(), "provider task identity must survive more than the optional attempt budget")
	row := client.Request.Query().OnlyX(base)
	require.Equal(t, "opaque-video-task", row.ExternalID)
	require.Equal(t, ch.ID, row.ChannelID)
	require.JSONEq(t, `{}`, string(row.RequestBody))
	require.Empty(t, row.ResponseBody)
}

func TestObservationPolicyFailureOmitsCaptureButPreservesAccounting(t *testing.T) {
	for _, failAtStart := range []bool{false, true} {
		t.Run(fmt.Sprintf("startup=%v", failAtStart), func(t *testing.T) {
			legacy, client, base, project := setupRequestExecutionStorageTest(t)
			defer client.Close()
			var policyUnavailable atomic.Bool
			policyUnavailable.Store(failAtStart)
			client.System.Intercept(ent.InterceptFunc(func(next ent.Querier) ent.Querier {
				return ent.QuerierFunc(func(ctx context.Context, q ent.Query) (ent.Value, error) {
					if policyUnavailable.Load() {
						return nil, context.DeadlineExceeded
					}
					return next.Query(ctx, q)
				})
			}))
			w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{AttemptTimeout: 50 * time.Millisecond, MaxAttempts: 2})
			svc := NewRequestServiceWithObservationWriter(client, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, w)
			w.node = cluster.Config{Enabled: true, NodeID: "policy-test"}
			require.NoError(t, w.Start(base), "optional capture policy must not be a startup prerequisite")
			ctx := w.WithScope(contexts.WithProjectID(base, project.ID))
			defer func() {
				EndForwardingObservation(ctx)
				stop, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = w.Stop(stop)
			}()
			policyUnavailable.Store(true)
			ch := createStorageTestChannel(t, base, client, nil)
			parent, err := svc.CreateRequest(ctx, &llm.Request{Model: "m"}, &httpclient.Request{Body: []byte(`{"optional":true}`)}, llm.APIFormatOpenAIResponse)
			require.NoError(t, err)
			_, err = svc.UsageLogService.CreateUsageLogFromRequest(ctx, parent, &ent.RequestExecution{ChannelID: ch.ID, ModelID: "m"}, &llm.Usage{TotalTokens: 17})
			require.NoError(t, err)
			EndForwardingObservation(ctx)
			drain, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
			defer cancel()
			require.NoError(t, w.Wait(drain), "policy-only failure must not strand the usage handoff")
			require.EqualValues(t, 17, client.UsageLog.Query().OnlyX(base).TotalTokens)
			require.Zero(t, client.PendingUsage.Query().CountX(base))
			require.JSONEq(t, `{}`, string(client.Request.Query().OnlyX(base).RequestBody))
			captureCtx := w.WithScope(base)
			defer EndForwardingObservation(captureCtx)
			policy := svc.SystemService.StoragePolicyOrDefault(captureCtx)
			require.False(t, policy.StoreRequestBody)
			require.False(t, policy.StoreResponseBody)
			require.False(t, policy.StoreChunks)
			require.False(t, policy.LivePreview)
		})
	}
}

func TestObservationAccountingRecoversUnadmittedParent(t *testing.T) {
	for _, clustered := range []bool{false, true} {
		t.Run(fmt.Sprintf("cluster=%v", clustered), func(t *testing.T) {
			legacy, client, base, project := setupRequestExecutionStorageTest(t)
			defer client.Close()
			// One required usage credit fits; optional parent/execution pairs do not.
			w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 1, AttemptTimeout: 10 * time.Millisecond})
			svc := NewRequestServiceWithObservationWriter(client, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, w)
			w.node = cluster.Config{Enabled: clustered, NodeID: "test"}
			require.NoError(t, w.Start(base))
			ctx := w.WithScope(contexts.WithProjectID(base, project.ID))
			defer func() {
				EndForwardingObservation(ctx)
				stop, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = w.Stop(stop)
			}()
			require.True(t, observationFromContext(ctx).reservedUsage, "accounting is reserved independently of optional metadata")
			ch := createStorageTestChannel(t, base, client, nil)
			parent, err := svc.CreateRequest(ctx, &llm.Request{Model: "m"}, &httpclient.Request{}, llm.APIFormatOpenAIResponse)
			require.NoError(t, err)
			exec := &ent.RequestExecution{ChannelID: ch.ID, ModelID: "m"}
			_, err = svc.UsageLogService.CreateUsageLogFromRequest(ctx, parent, exec, &llm.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5})
			require.NoError(t, err)
			_, err = svc.UsageLogService.CreateUsageLogFromRequest(ctx, parent, exec, &llm.Usage{TotalTokens: 999})
			require.NoError(t, err)
			EndForwardingObservation(ctx)
			drain, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			require.NoError(t, w.Wait(drain))
			bill := client.UsageLog.Query().OnlyX(base)
			require.EqualValues(t, 5, bill.TotalTokens)
			require.Nil(t, bill.TotalCost)
			require.Equal(t, ch.ID, bill.ChannelID)
			row := client.Request.Query().OnlyX(base)
			require.Equal(t, row.ID, bill.RequestID)
			require.Equal(t, project.ID, row.ProjectID)
			require.Equal(t, parent.EvidenceDisposition.ObservationID, row.EvidenceDisposition.ObservationID)
			require.Zero(t, client.PendingUsage.Query().CountX(base))
		})
	}
}
