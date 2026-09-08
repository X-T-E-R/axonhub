package biz

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestObservationAdmissionWriterDefaults(t *testing.T) {
	defaults := ManagedRequestBodyWriterConfig{}.withDefaults()
	require.Equal(t, 64, defaults.MaxItems)
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{})
	require.Equal(t, 256, w.config.MaxItems)
	require.Equal(t, 64, w.payloadLane.config.MaxItems)
	w = NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 64})
	require.Equal(t, 64, w.config.MaxItems, "explicit limits are not silently expanded")
}

func TestObservationAdmissionHeadroomForExplicitLimits(t *testing.T) {
	for _, items := range []int{1, 2, 7, 64, 256} {
		for _, mib := range []int{1, 64} {
			w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: items, MaxBytesMiB: mib})
			limit := w.optionalByteLimit()
			total := int64(mib) << 20
			require.Positive(t, limit)
			require.Less(t, limit, total)
			scopeGroups := int64((items + 4) / 5)
			require.Equal(t, min(total/4, scopeGroups*48*1024), total-limit,
				"each five-slot lifecycle has two 4KiB creates, one 8KiB usage, and two 16KiB terminals")
		}
	}
}

func TestObservationAdmissionConcurrentLifecycles(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, pressure := range []string{"normal", "items", "bytes", "large-burst"} {
			t.Run(fmt.Sprintf("saving=%t/%s", enabled, pressure), func(t *testing.T) {
				legacy, client, base, project := setupRequestExecutionStorageTest(t)
				defer client.Close()
				require.NoError(t, legacy.SystemService.SetStoragePolicy(base, &StoragePolicy{
					StoreRequestBody: enabled, StoreExecutionRequestBody: lo.ToPtr(enabled), StoreResponseBody: enabled, StoreChunks: enabled,
				}))
				w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{AttemptTimeout: 10 * time.Second})
				svc := NewRequestServiceWithObservationWriter(client, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, w)
				require.NoError(t, w.Start(base))
				channel := createStorageTestChannel(t, base, client, nil)
				const concurrent = 18
				projectCtx := contexts.WithProjectID(base, project.ID)
				contextsForRequests := lo.Times(concurrent, func(int) context.Context {
					return w.WithScope(projectCtx)
				})
				release := make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				t.Cleanup(func() {
					unblock()
					for _, ctx := range contextsForRequests {
						EndForwardingObservation(ctx)
					}
					stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					_ = w.Stop(stop)
				})
				if pressure != "normal" {
					entered := make(chan struct{})
					scope := observationFromContext(contextsForRequests[0])
					require.NoError(t, scope.submit(context.Background(), 0, func(ctx context.Context) error {
						close(entered)
						select {
						case <-release:
							return nil
						case <-ctx.Done():
							return ctx.Err()
						}
					}))
					<-entered
					if pressure == "items" {
						for range 63 {
							require.NoError(t, scope.submit(context.Background(), 0, func(context.Context) error { return nil }))
						}
					} else if pressure == "bytes" {
						w.mu.Lock()
						padding := w.optionalByteLimit() - w.optionalBytes - 4096 - observationContextBytes(detachedObservationContext(context.Background()))
						w.mu.Unlock()
						require.NoError(t, scope.submit(context.Background(), padding, func(context.Context) error { return nil }))
					}
					if pressure != "large-burst" {
						require.ErrorIs(t, scope.submit(context.Background(), 0, func(context.Context) error { return nil }), errObservationQueueUnavailable)
					}
				}
				bodySize := 1 << 20
				if pressure == "large-burst" {
					bodySize = 30 << 20
				}
				body := append([]byte(`{"text":"`), bytes.Repeat([]byte("x"), bodySize)...)
				body = append(body, []byte(`"}`)...)
				results := make(chan error, concurrent)
				var workers sync.WaitGroup
				for _, ctx := range contextsForRequests {
					workers.Go(func() {
						defer func() {
							if cause := recover(); cause != nil {
								results <- fmt.Errorf("lifecycle panic: %v", cause)
							}
						}()
						defer EndForwardingObservation(ctx)
						results <- func() error {
							parent, err := svc.CreateRequest(ctx, &llm.Request{Model: "admission-contract"}, &httpclient.Request{Body: body}, llm.APIFormatOpenAIChatCompletion)
							if err != nil {
								return err
							}
							if err = svc.UpdateRequestChannelID(ctx, parent.ID, channel.ID); err != nil {
								return err
							}
							execution, err := svc.CreateRequestExecution(ctx, channel, "admission-contract", parent, httpclient.Request{Body: body}, llm.APIFormatOpenAIChatCompletion, false)
							if err != nil {
								return err
							}
							for range 2 {
								if _, err = svc.UsageLogService.CreateUsageLogFromRequest(ctx, parent, execution, &llm.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}); err != nil {
									return err
								}
							}
							if err = svc.UpdateRequestExecutionCompleted(ctx, execution.ID, "response", []byte(`{"ok":true}`), nil); err != nil {
								return err
							}
							return svc.UpdateRequestCompleted(ctx, parent.ID, "response", []byte(`{"ok":true}`), nil)
						}()
					})
				}
				workers.Wait()
				close(results)
				for err := range results {
					require.NoError(t, err)
				}
				w.mu.Lock()
				require.LessOrEqual(t, w.items, w.config.MaxItems)
				require.LessOrEqual(t, w.bytes, int64(64<<20))
				require.LessOrEqual(t, w.optionalItems, 64)
				require.LessOrEqual(t, w.optionalBytes, w.optionalByteLimit())
				w.mu.Unlock()
				unblock()
				drain, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				require.NoError(t, w.Wait(drain))
				parents := client.Request.Query().AllX(base)
				executions := client.RequestExecution.Query().AllX(base)
				usages := client.UsageLog.Query().AllX(base)
				require.Len(t, parents, concurrent)
				require.Len(t, executions, concurrent)
				require.Len(t, usages, concurrent, "duplicate submissions must not duplicate usage")
				savedBodies := 0
				for _, parent := range parents {
					require.Equal(t, request.StatusCompleted, parent.Status)
					require.Equal(t, channel.ID, parent.ChannelID)
					if enabled && pressure != "bytes" && (pressure != "large-burst" || parent.EvidenceDisposition.RequestBody.Outcome != "unavailable") {
						stored, err := svc.LoadRequestBody(base, parent)
						require.NoError(t, err)
						require.Equal(t, body, []byte(stored))
						savedBodies++
					} else if enabled {
						require.Equal(t, "unavailable", parent.EvidenceDisposition.RequestBody.Outcome)
					} else {
						require.Equal(t, "omitted", parent.EvidenceDisposition.RequestBody.Outcome)
					}
				}
				for _, execution := range executions {
					require.Equal(t, requestexecution.StatusCompleted, execution.Status)
					if enabled && pressure != "bytes" && (pressure != "large-burst" || execution.EvidenceDisposition.RequestBody.Outcome != "unavailable") {
						stored, err := svc.LoadRequestExecutionRequestBody(base, execution)
						require.NoError(t, err)
						require.Equal(t, body, []byte(stored))
						savedBodies++
					} else if enabled {
						require.Equal(t, "unavailable", execution.EvidenceDisposition.RequestBody.Outcome)
					}
				}
				if enabled && pressure == "large-burst" {
					require.Equal(t, 2, savedBodies, "two 30MiB captures fit; the remaining 34 must compact without losing metadata")
				}
				for _, usage := range usages {
					require.EqualValues(t, 5, usage.TotalTokens)
				}
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

func TestObservationAdmissionChannelUnavailableDoesNotAbort(t *testing.T) {
	legacy, client, base, project := setupRequestExecutionStorageTest(t)
	defer client.Close()
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{})
	svc := NewRequestServiceWithObservationWriter(client, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, w)
	ctx := w.WithScope(contexts.WithProjectID(base, project.ID))
	defer EndForwardingObservation(ctx)
	parent, err := svc.CreateRequest(ctx, &llm.Request{Model: "no-quota"}, &httpclient.Request{}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	require.NoError(t, svc.UpdateRequestChannelID(ctx, parent.ID, 1))
	require.Equal(t, "unavailable", parent.EvidenceDisposition.RequestBody.Outcome)
	require.Error(t, svc.UpdateRequestChannelID(base, 99999, 1), "synchronous persistence errors remain visible")
	quota := &objects.APIKeyQuota{Requests: lo.ToPtr(int64(5)), Period: objects.APIKeyQuotaPeriod{Type: objects.APIKeyQuotaPeriodTypeAllTime}}
	key := &ent.APIKey{ID: 42, Profiles: &objects.APIKeyProfiles{ActiveProfile: "limited", Profiles: []objects.APIKeyProfile{{Name: "limited", Quota: quota}}}}
	quotaCtx := contexts.WithAPIKey(ctx, key)
	_, err = svc.CreateRequest(quotaCtx, &llm.Request{Model: "quota"}, &httpclient.Request{}, llm.APIFormatOpenAIChatCompletion)
	require.ErrorContains(t, err, "quota accounting observation admission unavailable")
	quotaService := NewQuotaServiceWithObservationWriter(client, legacy.SystemService, w)
	_, err = quotaService.CheckAPIKeyQuota(quotaCtx, key.ID, quota)
	require.ErrorContains(t, err, "quota accounting queue is full")
	decision, err := quotaService.CheckAPIKeyQuota(quotaCtx, key.ID, nil)
	require.NoError(t, err)
	require.True(t, decision.Allowed)
}

func TestObservationAdmissionTwoThirtyMiBBodies(t *testing.T) {
	legacy, client, base, project := setupRequestExecutionStorageTest(t)
	defer client.Close()
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{AttemptTimeout: 10 * time.Second})
	svc := NewRequestServiceWithObservationWriter(client, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, nil, w)
	require.NoError(t, w.Start(base))
	ctx := w.WithScope(contexts.WithProjectID(base, project.ID))
	channel := createStorageTestChannel(t, base, client, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer func() {
		unblock()
		EndForwardingObservation(ctx)
		stop, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = w.Stop(stop)
	}()
	require.NoError(t, observationFromContext(ctx).submit(context.Background(), 0, func(ctx context.Context) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}))
	<-entered
	body := append([]byte(`{"text":"`), bytes.Repeat([]byte("x"), 30<<20)...)
	body = append(body, []byte(`"}`)...)
	parent, err := svc.CreateRequest(ctx, &llm.Request{Model: "large-pair"}, &httpclient.Request{Body: body, JSONBody: body}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	_, err = svc.CreateRequestExecution(ctx, channel, "large-pair", parent, httpclient.Request{Body: body, JSONBody: body}, llm.APIFormatOpenAIChatCompletion, false)
	require.NoError(t, err)
	w.mu.Lock()
	t.Logf("two 30MiB captures: total=%d optional=%d optional_limit=%d", w.bytes, w.optionalBytes, w.optionalByteLimit())
	require.Greater(t, w.bytes, int64(60<<20), "both payloads must be admitted while persistence is blocked")
	require.LessOrEqual(t, w.bytes, int64(64<<20))
	w.mu.Unlock()
	EndForwardingObservation(ctx)
	unblock()
	drain, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	require.NoError(t, w.Wait(drain))
	storedParent := client.Request.Query().OnlyX(base)
	storedExecution := client.RequestExecution.Query().OnlyX(base)
	parentBody, err := svc.LoadRequestBody(base, storedParent)
	require.NoError(t, err)
	executionBody, err := svc.LoadRequestExecutionRequestBody(base, storedExecution)
	require.NoError(t, err)
	require.Equal(t, body, []byte(parentBody))
	require.Equal(t, body, []byte(executionBody))
}

func TestObservationAdmissionLateCoreCreditsAreAbandoned(t *testing.T) {
	w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 6})
	w.started = true // Freeze the queue without a persistence goroutine.
	first := w.WithScope(t.Context())
	require.NoError(t, observationFromContext(first).submit(first, 0, func(context.Context) error { return nil }))
	late := w.WithScope(t.Context())
	require.Zero(t, observationFromContext(late).reservedCore)
	EndForwardingObservation(first)
	require.NoError(t, observationFromContext(late).submitCore(late, -1, 0, func(context.Context) error { return nil }))
	w.abandonPending()
	require.Zero(t, w.items)
	require.Zero(t, w.bytes)
	require.Zero(t, w.optionalItems)
	require.Zero(t, w.optionalBytes)
	EndForwardingObservation(late)
	require.Zero(t, w.items, "End must not release already abandoned credits twice")
}
