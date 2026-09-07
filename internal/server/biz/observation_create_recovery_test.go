package biz

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/hook"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/stretchr/testify/require"
)

type observationRecoveryObjectStore struct {
	boundedObjectStoreFake
	mu      sync.Mutex
	objects map[string][]byte
	fail    bool
}

func (s *observationRecoveryObjectStore) PutObject(_ context.Context, key string, body []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("isolated unavailable object store")
	}
	s.objects[key] = bytes.Clone(body)
	return nil
}

func TestForwardingObservationAmbiguousCreateResumesBodies(t *testing.T) {
	for _, mode := range []string{"managed", "external", "external_failure"} {
		t.Run(mode, func(t *testing.T) {
			legacy, client, ctx, project := setupRequestExecutionStorageTest(t)
			defer client.Close()
			var bodyWriter *ManagedRequestBodyWriter
			store := &observationRecoveryObjectStore{objects: make(map[string][]byte), fail: mode == "external_failure"}
			if mode == "managed" {
				bodyWriter = NewManagedRequestBodyWriter(ManagedRequestBodyWriterConfig{MaxItems: 4, MaxBytesMiB: 1}, client, legacy.SystemService)
				require.NoError(t, bodyWriter.Start(ctx))
			} else {
				ds := client.DataStorage.Create().SetName("ambiguous-local-store").SetDescription("isolated").SetType(datastorage.TypeS3).SetSettings(&objects.DataStorageSettings{S3: &objects.S3{}}).SetStatus(datastorage.StatusActive).SaveX(ctx)
				legacy.DataStorageService.objectStoreCache[ds.ID] = store
				require.NoError(t, legacy.SystemService.SetDefaultDataStorageID(ctx, ds.ID))
			}
			w := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{MaxItems: 16, MaxBytesMiB: 1, AttemptTimeout: 500 * time.Millisecond})
			svc := NewRequestServiceWithObservationWriter(client, legacy.SystemService, legacy.UsageLogService, legacy.DataStorageService, legacy.LiveStreamRegistry, bodyWriter, w)
			require.NoError(t, w.Start(ctx))
			var requestInjected, executionInjected atomic.Bool
			client.Request.Use(func(next ent.Mutator) ent.Mutator {
				return hook.RequestFunc(func(ctx context.Context, mutation *ent.RequestMutation) (ent.Value, error) {
					value, err := next.Mutate(ctx, mutation)
					if err == nil && mutation.Op().Is(ent.OpCreate) && requestInjected.CompareAndSwap(false, true) {
						return value, errors.New("committed parent INSERT acknowledgement lost")
					}
					return value, err
				})
			})
			client.RequestExecution.Use(func(next ent.Mutator) ent.Mutator {
				return hook.RequestExecutionFunc(func(ctx context.Context, mutation *ent.RequestExecutionMutation) (ent.Value, error) {
					value, err := next.Mutate(ctx, mutation)
					if err == nil && mutation.Op().Is(ent.OpCreate) && executionInjected.CompareAndSwap(false, true) {
						return value, errors.New("committed execution INSERT acknowledgement lost")
					}
					return value, err
				})
			})
			channel := createStorageTestChannel(t, ctx, client, nil)
			ctx = w.WithScope(contexts.WithProjectID(ctx, project.ID))
			body := []byte(`{"model":"gpt-4o","prompt":"preserve exactly"}`)
			parent, err := svc.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion)
			require.NoError(t, err)
			execution, err := svc.CreateRequestExecution(ctx, channel, "gpt-4o", parent, httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion, false)
			require.NoError(t, err)
			_, err = svc.UsageLogService.CreateUsageLogFromRequest(ctx, parent, execution, &llm.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5})
			require.NoError(t, err)
			require.NoError(t, svc.UpdateRequestStatusFromError(ctx, parent.ID, errors.New("provider failed"), nil))
			EndForwardingObservation(ctx)
			drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, w.Stop(drainCtx))
			if bodyWriter != nil {
				require.NoError(t, bodyWriter.Stop(drainCtx))
			}
			actual := client.Request.Query().OnlyX(ctx)
			actualExec := client.RequestExecution.Query().OnlyX(ctx)
			require.True(t, requestInjected.Load())
			require.True(t, executionInjected.Load())
			require.Equal(t, actual.ID, actualExec.RequestID)
			require.Equal(t, request.StatusFailed, actual.Status)
			require.Equal(t, actual.ID, client.UsageLog.Query().OnlyX(ctx).RequestID)
			for _, disposition := range []objects.Disposition{actual.EvidenceDisposition.RequestBody, actualExec.EvidenceDisposition.RequestBody} {
				if mode == "managed" {
					// Managed reads resolve the payload pointer; its attachment does
					// not rewrite the initial raw async disposition JSON.
					continue
				}
				if store.fail {
					require.Equal(t, "writeFailed", disposition.Outcome)
					require.Equal(t, "external_write_failed", *disposition.FailureClass)
				} else {
					require.Equal(t, "stored", disposition.Outcome)
					require.Nil(t, disposition.FailureClass)
					if mode == "external" {
						require.NotNil(t, disposition.StorageKey)
						require.Equal(t, body, store.objects[normalizeObjectKey(*disposition.StorageKey)])
					}
				}
			}
			if mode == "managed" {
				require.NotNil(t, actual.RequestBodyPayloadID)
				require.Equal(t, actual.RequestBodyPayloadID, actualExec.RequestBodyPayloadID)
				loaded, handled, err := svc.loadManagedRequestBody(ctx, actual.RequestBodyPayloadID, actual.ID, int64(len(body)+1))
				require.NoError(t, err)
				require.True(t, handled)
				require.Equal(t, body, []byte(loaded))
				payload := client.ObservabilityPayload.Query().OnlyX(ctx)
				require.Equal(t, payload.ChargedBytes, client.ManagedObservabilityState.GetX(ctx, 1).ChargedBytes)
				bodyWriter.mu.Lock()
				require.Zero(t, bodyWriter.reservedItems)
				require.Zero(t, bodyWriter.reservedBytes)
				bodyWriter.mu.Unlock()
			}
			w.mu.Lock()
			require.Zero(t, w.items)
			require.Zero(t, w.bytes)
			w.mu.Unlock()
		})
	}
}

func TestForwardingObservationAmbiguousRecoveryFullBodyLaneIsExplicit(t *testing.T) {
	svc, writer, client, ctx, project := setupAsyncManagedRequestBodyTest(t, ManagedRequestBodyWriterConfig{MaxItems: 1, MaxBytesMiB: 1})
	defer client.Close()
	body := []byte(`{"model":"gpt-4o"}`)
	ctx = withObservationID(ctx, "isolated-ambiguous-recovery")
	var injected atomic.Bool
	client.Request.Use(func(next ent.Mutator) ent.Mutator {
		return hook.RequestFunc(func(ctx context.Context, mutation *ent.RequestMutation) (ent.Value, error) {
			value, err := next.Mutate(ctx, mutation)
			if err == nil && mutation.Op().Is(ent.OpCreate) && injected.CompareAndSwap(false, true) {
				return value, errors.New("committed INSERT acknowledgement lost")
			}
			return value, err
		})
	})
	_, err := svc.CreateRequest(contexts.WithProjectID(ctx, project.ID), &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion)
	require.Error(t, err)
	held, rejection := writer.reserve(body)
	require.Empty(t, rejection, "ambiguous INSERT must release the original producer reservation")
	require.NotNil(t, held)
	row := client.Request.Query().OnlyX(ctx)
	require.NoError(t, svc.resumeObservedRequestBody(ctx, row, body))
	row = client.Request.Query().OnlyX(ctx)
	require.Nil(t, row.RequestBodyPayloadID)
	require.Equal(t, "omitted", row.EvidenceDisposition.RequestBody.Outcome)
	require.NotNil(t, row.EvidenceDisposition.RequestBody.FailureClass)
	require.NotEqual(t, managedRequestBodyAsyncPending, *row.EvidenceDisposition.RequestBody.FailureClass)
	writer.mu.Lock()
	require.Equal(t, 1, writer.reservedItems)
	require.Equal(t, int64(len(body)), writer.reservedBytes)
	writer.mu.Unlock()
	held.release()
	stopManagedRequestBodyWriter(t, writer)
}
