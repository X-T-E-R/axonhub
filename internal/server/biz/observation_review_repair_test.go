package biz

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/hook"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestForwardingObservationReviewNegativeRequestErrorEvidence(t *testing.T) {
	legacy, client, baseCtx, project := setupRequestExecutionStorageTest(t)
	t.Cleanup(func() { _ = client.Close() })

	writer := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{
		MaxItems:       8,
		MaxBytesMiB:    1,
		AttemptTimeout: 5 * time.Second,
	})
	svc := NewRequestServiceWithObservationWriter(
		client,
		legacy.SystemService,
		legacy.UsageLogService,
		legacy.DataStorageService,
		legacy.LiveStreamRegistry,
		nil,
		writer,
	)
	require.NoError(t, writer.Start(baseCtx))
	scopedCtx := writer.WithScope(contexts.WithProjectID(baseCtx, project.ID))
	var stopped atomic.Bool
	t.Cleanup(func() {
		EndForwardingObservation(scopedCtx)
		if !stopped.Load() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = writer.Stop(stopCtx)
		}
	})

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	client.Request.Use(func(next ent.Mutator) ent.Mutator {
		return hook.RequestFunc(func(hookCtx context.Context, mutation *ent.RequestMutation) (ent.Value, error) {
			if mutation.Op().Is(ent.OpCreate) {
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-hookCtx.Done():
					return nil, hookCtx.Err()
				}
			}
			return next.Mutate(hookCtx, mutation)
		})
	})
	releaseBarrier := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseBarrier)

	parent, err := svc.CreateRequest(
		scopedCtx,
		&llm.Request{Model: "gpt-4o"},
		&httpclient.Request{JSONBody: []byte(`{"model":"gpt-4o"}`)},
		llm.APIFormatOpenAIChatCompletion,
	)
	require.NoError(t, err)
	require.Less(t, parent.ID, 0)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request INSERT barrier was not entered")
	}

	errorBody := objects.JSONRawMessage(`{"error":{"message":"upstream failed"}}`)
	errorInfo := &ExecutionErrorInfo{ResponseBody: errorBody}
	require.NoError(t, svc.UpdateRequestStatusFromErrorDetails(
		scopedCtx,
		parent.ID,
		errors.New("upstream failed"),
		context.DeadlineExceeded,
		errorInfo,
	))

	releaseBarrier()
	EndForwardingObservation(scopedCtx)
	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	require.NoError(t, writer.Wait(drainCtx))
	cancel()

	persisted := client.Request.Query().OnlyX(baseCtx)
	require.Equal(t, request.StatusFailed, persisted.Status)
	require.Equal(t, []byte(errorBody), []byte(persisted.ResponseBody))
	require.Equal(t, "stored", persisted.EvidenceDisposition.ResponseBody.Outcome)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	require.NoError(t, writer.Stop(stopCtx))
	stopCancel()
	stopped.Store(true)
}

func TestForwardingObservationReviewAPIKeyProfilesSnapshotAndBudget(t *testing.T) {
	t.Run("profile snapshot and digest", func(t *testing.T) {
		legacy, client, baseCtx, project := setupRequestExecutionStorageTest(t)
		t.Cleanup(func() { _ = client.Close() })

		writer := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{
			MaxItems:       8,
			MaxBytesMiB:    1,
			AttemptTimeout: time.Second,
		})
		svc := NewRequestServiceWithObservationWriter(
			client,
			legacy.SystemService,
			legacy.UsageLogService,
			legacy.DataStorageService,
			legacy.LiveStreamRegistry,
			nil,
			writer,
		)
		require.NoError(t, writer.Start(baseCtx))
		scopedCtx := writer.WithScope(contexts.WithProjectID(baseCtx, project.ID))
		var stopped atomic.Bool
		t.Cleanup(func() {
			EndForwardingObservation(scopedCtx)
			if !stopped.Load() {
				stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = writer.Stop(stopCtx)
			}
		})

		originalProfiles := &objects.APIKeyProfiles{
			ActiveProfile: "before",
			Profiles: []objects.APIKeyProfile{{
				Name:       "before",
				ChannelIDs: []int{17},
				ModelIDs:   []string{"gpt-4o"},
				ModelMappings: []objects.ModelMapping{{
					From: "gpt-4o",
					To:   "original-gpt-4o",
				}},
			}},
		}
		expectedProfiles := &objects.APIKeyProfiles{
			ActiveProfile: "before",
			Profiles: []objects.APIKeyProfile{{
				Name:       "before",
				ChannelIDs: []int{17},
				ModelIDs:   []string{"gpt-4o"},
				ModelMappings: []objects.ModelMapping{{
					From: "gpt-4o",
					To:   "original-gpt-4o",
				}},
			}},
		}
		apiKey := client.APIKey.Create().
			SetProjectID(project.ID).
			SetName("profile-snapshot-key").
			SetKey("profile-snapshot-key-value").
			SetProfiles(originalProfiles).
			SaveX(baseCtx)
		requestCtx := contexts.WithAPIKey(scopedCtx, apiKey)

		entered := make(chan struct{})
		release := make(chan struct{})
		var enteredOnce sync.Once
		var releaseOnce sync.Once
		client.Request.Use(func(next ent.Mutator) ent.Mutator {
			return hook.RequestFunc(func(hookCtx context.Context, mutation *ent.RequestMutation) (ent.Value, error) {
				if mutation.Op().Is(ent.OpCreate) {
					enteredOnce.Do(func() { close(entered) })
					select {
					case <-release:
					case <-hookCtx.Done():
						return nil, hookCtx.Err()
					}
				}
				return next.Mutate(hookCtx, mutation)
			})
		})
		releaseBarrier := func() { releaseOnce.Do(func() { close(release) }) }
		t.Cleanup(releaseBarrier)

		_, err := svc.CreateRequest(
			requestCtx,
			&llm.Request{Model: "gpt-4o"},
			&httpclient.Request{JSONBody: []byte(`{"model":"gpt-4o"}`)},
			llm.APIFormatOpenAIChatCompletion,
		)
		require.NoError(t, err)
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("request INSERT barrier was not entered")
		}

		originalProfiles.ActiveProfile = "after"
		originalProfiles.Profiles[0].Name = "after"
		originalProfiles.Profiles[0].ModelMappings[0].To = "mutated-gpt-4o"
		originalProfiles.Profiles[0].ChannelIDs[0] = 99
		releaseBarrier()
		EndForwardingObservation(requestCtx)
		drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		require.NoError(t, writer.Wait(drainCtx))
		cancel()

		persisted := client.Request.Query().OnlyX(baseCtx)
		require.Equal(t, apiKey.ID, persisted.APIKeyID)
		require.NotNil(t, persisted.RoutingContext)
		require.Equal(t, expectedProfiles, persisted.RoutingContext.EffectiveProfiles)
		expectedJSON, err := canonicalJSON(expectedProfiles)
		require.NoError(t, err)
		digest := sha256.Sum256(expectedJSON)
		require.Equal(t, hex.EncodeToString(digest[:]), persisted.RoutingContext.EffectiveProfilesSHA256)

		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		require.NoError(t, writer.Stop(stopCtx))
		stopCancel()
		stopped.Store(true)
	})

	t.Run("detached context drops pipeline and charges profiles", func(t *testing.T) {
		profileValue := strings.Repeat("profile", 10<<10)
		apiKey := &ent.APIKey{
			ID: 17,
			Profiles: &objects.APIKeyProfiles{
				ActiveProfile: "budget",
				Profiles: []objects.APIKeyProfile{{
					Name: "budget",
					ModelMappings: []objects.ModelMapping{{
						From: profileValue,
						To:   "gpt-4o",
					}},
				}},
			},
		}
		type pipelineKey struct{}
		ctx := context.WithValue(contexts.WithAPIKey(t.Context(), apiKey), pipelineKey{}, make([]byte, 2<<20))
		writer := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{
			MaxItems:       8,
			MaxBytesMiB:    1,
			AttemptTimeout: time.Second,
		})
		require.NoError(t, writer.Start(ctx))
		scopedCtx := writer.WithScope(ctx)
		var stopped atomic.Bool
		t.Cleanup(func() {
			EndForwardingObservation(scopedCtx)
			if !stopped.Load() {
				stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = writer.Stop(stopCtx)
			}
		})

		creationCtx := context.WithValue(scopedCtx, requestObservationProfilesKey{}, true)
		detached := detachedObservationContext(creationCtx)
		require.Nil(t, detached.Value(pipelineKey{}))
		detachedKey, ok := observationAPIKey(detached)
		require.True(t, ok)
		require.NotNil(t, detachedKey.Profiles)
		profileJSON, err := json.Marshal(apiKey.Profiles)
		require.NoError(t, err)
		require.GreaterOrEqual(t, observationContextBytes(detached), int64(len(profileJSON)))

		callbackResult := make(chan error, 1)
		scope := observationFromContext(scopedCtx)
		// This payload fits without the retained profiles, but exceeds the
		// writer budget once their bytes are charged by actual admission.
		require.ErrorIs(t, scope.submitCore(creationCtx, -1, (1<<20)-int64(len(profileJSON))+1, func(context.Context) error {
			return errors.New("oversized profile job must not be admitted")
		}), errObservationQueueUnavailable)
		require.NoError(t, scope.submit(scopedCtx, 0, func(workerCtx context.Context) error {
			if workerCtx.Value(pipelineKey{}) != nil {
				callbackResult <- errors.New("detached observation context retained pipeline value")
				return nil
			}
			workerKey, ok := observationAPIKey(workerCtx)
			if !ok || workerKey == nil || workerKey.Profiles != nil {
				callbackResult <- errors.New("non-creation job retained API key profiles")
				return nil
			}
			callbackResult <- nil
			return nil
		}))
		EndForwardingObservation(scopedCtx)
		drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		require.NoError(t, writer.Wait(drainCtx))
		cancel()
		require.NoError(t, <-callbackResult)

		stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
		require.NoError(t, writer.Stop(stopCtx))
		stopCancel()
		stopped.Store(true)
	})
}

func TestForwardingObservationReviewExternalChannelOverrides(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		globalResponse bool
		globalChunks   bool
		channel        *objects.ChannelSettings
		wantWrites     int
		wantResponse   string
		wantChunks     string
	}{
		{
			name:           "channel enables globally disabled evidence",
			globalResponse: false,
			globalChunks:   false,
			channel: &objects.ChannelSettings{
				StoreExecutionResponseBody: lo.ToPtr(true),
				StoreExecutionStreamChunks: lo.ToPtr(true),
			},
			wantWrites:   2,
			wantResponse: "stored",
			wantChunks:   "stored",
		},
		{
			name:           "channel disables globally enabled evidence",
			globalResponse: true,
			globalChunks:   true,
			channel: &objects.ChannelSettings{
				StoreExecutionResponseBody: lo.ToPtr(false),
				StoreExecutionStreamChunks: lo.ToPtr(false),
			},
			wantWrites:   0,
			wantResponse: "omitted",
			wantChunks:   "omitted",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			svc, writer, baseCtx, client, project, channel, store, scopedCtx, _, execution := newReviewExternalFixture(t, testCase.globalResponse, testCase.globalChunks, testCase.channel)
			responseBody := objects.JSONRawMessage(`{"answer":"ok"}`)
			require.NoError(t, svc.UpdateRequestExecutionCompletedForChannel(scopedCtx, execution.ID, "external-execution", responseBody, nil, channel))
			require.NoError(t, svc.SaveRequestExecutionChunksForChannel(scopedCtx, execution.ID, []*httpclient.StreamEvent{{
				Type: "message",
				Data: []byte(`{"delta":"ok"}`),
			}}, channel))

			EndForwardingObservation(scopedCtx)
			drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			require.NoError(t, writer.Wait(drainCtx))
			cancel()

			persisted := client.RequestExecution.Query().OnlyX(baseCtx)
			require.Equal(t, testCase.wantResponse, persisted.EvidenceDisposition.ResponseBody.Outcome)
			require.Equal(t, testCase.wantChunks, persisted.EvidenceDisposition.ResponseChunks.Outcome)
			require.Equal(t, testCase.wantWrites, store.writeCount())
			if testCase.wantWrites == 2 {
				require.Empty(t, persisted.ResponseBody)
				require.Empty(t, persisted.ResponseChunks)
				require.NotEmpty(t, store.data(strings.TrimPrefix(GenerateExecutionResponseBodyKey(project.ID, persisted.RequestID, persisted.ID), "/")))
				require.NotEmpty(t, store.data(strings.TrimPrefix(GenerateExecutionResponseChunksKey(project.ID, persisted.RequestID, persisted.ID), "/")))
			}

			stopReviewExternalFixture(t, writer, scopedCtx)
		})
	}
}

func TestForwardingObservationReviewExternalPayloadChannelSettingsChangeOmitsSave(t *testing.T) {
	svc, writer, baseCtx, client, _, channel, store, scopedCtx, _, execution := newReviewExternalFixture(t, false, false, &objects.ChannelSettings{
		StoreExecutionResponseBody: lo.ToPtr(true),
	})

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	var releaseOnce sync.Once
	client.RequestExecution.Use(func(next ent.Mutator) ent.Mutator {
		return hook.RequestExecutionFunc(func(hookCtx context.Context, mutation *ent.RequestExecutionMutation) (ent.Value, error) {
			status, hasStatus := mutation.Status()
			if mutation.Op().Is(ent.OpUpdateOne) && hasStatus && status == requestexecution.StatusCompleted {
				enteredOnce.Do(func() { close(entered) })
				select {
				case <-release:
				case <-hookCtx.Done():
					return nil, hookCtx.Err()
				}
			}
			return next.Mutate(hookCtx, mutation)
		})
	})
	releaseBarrier := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseBarrier)

	require.NoError(t, svc.UpdateRequestExecutionCompletedForChannel(
		scopedCtx,
		execution.ID,
		"external-execution",
		objects.JSONRawMessage(`{"answer":"waiting"}`),
		nil,
		channel,
	))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("terminal update barrier was not entered")
	}
	require.Zero(t, store.writeCount())

	_, err := client.Channel.UpdateOneID(channel.ID).SetSettings(&objects.ChannelSettings{
		StoreExecutionResponseBody: lo.ToPtr(false),
	}).Save(baseCtx)
	require.NoError(t, err)
	releaseBarrier()
	EndForwardingObservation(scopedCtx)
	drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	require.NoError(t, writer.Wait(drainCtx))
	cancel()

	persisted := client.RequestExecution.Query().OnlyX(baseCtx)
	require.Equal(t, "omitted", persisted.EvidenceDisposition.ResponseBody.Outcome)
	require.Equal(t, "storage_disabled", *persisted.EvidenceDisposition.ResponseBody.FailureClass)
	require.Zero(t, store.writeCount())

	stopReviewExternalFixture(t, writer, scopedCtx)
}

type observationReviewObjectStore struct {
	mu     sync.Mutex
	writes map[string][]byte
}

func (s *observationReviewObjectStore) PutObject(_ context.Context, key string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writes == nil {
		s.writes = make(map[string][]byte)
	}
	s.writes[key] = append([]byte(nil), data...)
	return nil
}

func (s *observationReviewObjectStore) PutObjectStream(ctx context.Context, key string, reader io.Reader, _ int64) (int64, error) {
	data, err := io.ReadAll(reader)
	if err != nil {
		return 0, err
	}
	if err := s.PutObject(ctx, key, data); err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

func (s *observationReviewObjectStore) GetObject(_ context.Context, key string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.writes[key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), data...), nil
}

func (s *observationReviewObjectStore) OpenObject(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	data, err := s.GetObject(ctx, key)
	if err != nil {
		return nil, 0, err
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

func (s *observationReviewObjectStore) DeleteObject(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.writes, key)
	return nil
}

func (s *observationReviewObjectStore) writeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.writes)
}

func (s *observationReviewObjectStore) data(key string) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.writes[key]...)
}

func newReviewExternalFixture(
	t *testing.T,
	globalResponse, globalChunks bool,
	channelSettings *objects.ChannelSettings,
) (
	*RequestService,
	*ForwardingObservationWriter,
	context.Context,
	*ent.Client,
	*ent.Project,
	*Channel,
	*observationReviewObjectStore,
	context.Context,
	*ent.Request,
	*ent.RequestExecution,
) {
	t.Helper()
	legacy, client, baseCtx, project := setupRequestExecutionStorageTest(t)
	t.Cleanup(func() { _ = client.Close() })
	storage := client.DataStorage.Create().
		SetName("observation-review-external").
		SetDescription("external channel override review fixture").
		SetType(datastorage.TypeS3).
		SetSettings(&objects.DataStorageSettings{S3: &objects.S3{}}).
		SetStatus(datastorage.StatusActive).
		SaveX(baseCtx)
	store := &observationReviewObjectStore{}
	legacy.DataStorageService.fsCacheMu.Lock()
	legacy.DataStorageService.objectStoreCache[storage.ID] = store
	legacy.DataStorageService.fsCacheMu.Unlock()
	require.NoError(t, legacy.SystemService.SetDefaultDataStorageID(baseCtx, storage.ID))
	require.NoError(t, legacy.SystemService.SetStoragePolicy(baseCtx, &StoragePolicy{
		StoreRequestBody:          false,
		StoreExecutionRequestBody: lo.ToPtr(false),
		StoreResponseBody:         globalResponse,
		StoreChunks:               globalChunks,
	}))

	writer := NewForwardingObservationWriter(ManagedRequestBodyWriterConfig{
		MaxItems:       16,
		MaxBytesMiB:    1,
		AttemptTimeout: time.Second,
	})
	svc := NewRequestServiceWithObservationWriter(
		client,
		legacy.SystemService,
		legacy.UsageLogService,
		legacy.DataStorageService,
		legacy.LiveStreamRegistry,
		nil,
		writer,
	)
	require.NoError(t, writer.Start(baseCtx))
	scopedCtx := writer.WithScope(contexts.WithProjectID(baseCtx, project.ID))
	var stopped atomic.Bool
	t.Cleanup(func() {
		EndForwardingObservation(scopedCtx)
		if !stopped.Load() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = writer.Stop(stopCtx)
		}
	})

	channel := createStorageTestChannel(t, baseCtx, client, channelSettings)
	parent, err := svc.CreateRequest(
		scopedCtx,
		&llm.Request{Model: "gpt-4o"},
		&httpclient.Request{JSONBody: []byte(`{"model":"gpt-4o"}`)},
		llm.APIFormatOpenAIChatCompletion,
	)
	require.NoError(t, err)
	execution, err := svc.CreateRequestExecution(
		scopedCtx,
		channel,
		"gpt-4o",
		parent,
		httpclient.Request{JSONBody: []byte(`{"model":"gpt-4o"}`)},
		llm.APIFormatOpenAIChatCompletion,
		false,
	)
	require.NoError(t, err)
	return svc, writer, baseCtx, client, project, channel, store, scopedCtx, parent, execution
}

func stopReviewExternalFixture(t *testing.T, writer *ForwardingObservationWriter, scopedCtx context.Context) {
	t.Helper()
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, writer.Stop(stopCtx))
	EndForwardingObservation(scopedCtx)
}

var _ ObjectStore = (*observationReviewObjectStore)(nil)
