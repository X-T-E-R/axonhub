package biz

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	entchannel "github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/observabilitypayload"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/chunkbuffer"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func setupRequestExecutionStorageTest(t *testing.T) (*RequestService, *ent.Client, context.Context, *ent.Project) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	systemService := NewSystemService(SystemServiceParams{Ent: client})
	channelService := NewChannelServiceForTest(client)
	usageLogService := NewUsageLogService(client, systemService, channelService)
	dataStorageService := NewDataStorageService(DataStorageServiceParams{
		SystemService: systemService,
		CacheConfig:   xcache.Config{},
		Client:        client,
	})

	require.NoError(t, systemService.SetStoragePolicy(ctx, &StoragePolicy{
		StoreChunks:       true,
		LivePreview:       false,
		StoreRequestBody:  true,
		StoreResponseBody: true,
	}))

	proj, err := client.Project.Create().
		SetName("request-execution-storage").
		SetStatus(project.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	_, err = client.DataStorage.Create().
		SetName("request-execution-storage-primary").
		SetDescription("request execution storage test primary").
		SetPrimary(true).
		SetType(datastorage.TypeDatabase).
		SetSettings(&objects.DataStorageSettings{}).
		SetStatus(datastorage.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	return NewRequestService(client, systemService, usageLogService, dataStorageService, NewLiveStreamRegistry()), client, ctx, proj
}

func TestManagedRequestBodyBoundRejectsStoredLengthBeforeSelectingData(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()

	bodySize := int64(24 << 20)
	req := client.Request.Create().
		SetProjectID(proj.ID).
		SetModelID("bounded-managed-body").
		SetRequestBody([]byte(`{}`)).
		SetStatus(request.StatusCompleted).
		SetManagedObservability(true).
		SaveX(ctx)
	payload := client.ObservabilityPayload.Create().
		SetRequestID(req.ID).
		SetKind(observabilitypayload.KindRequestBody).
		SetSha256(strings.Repeat("d", 64)).
		SetByteLength(bodySize).
		SetChargedBytes(bodySize).
		SetData(make([]byte, bodySize)).
		SaveX(ctx)
	req = client.Request.UpdateOneID(req.ID).SetRequestBodyPayloadID(payload.ID).SaveX(ctx)

	var statements []string
	debugClient := ent.NewClient(
		ent.Driver(client.Driver()),
		ent.Debug(),
		ent.Log(func(values ...any) { statements = append(statements, fmt.Sprint(values...)) }),
	)
	debugService := NewRequestService(debugClient, svc.SystemService, svc.UsageLogService, svc.DataStorageService, svc.LiveStreamRegistry)
	_, err := debugService.LoadRequestBodyBounded(ent.NewContext(ctx, debugClient), req, 2<<20)
	require.ErrorIs(t, err, ErrDataTooLarge)
	require.Len(t, statements, 1, "stored byte_length must reject before issuing a data query")
	require.NotContains(t, statements[0], `"data"`, "metadata query must not select the payload blob")
}

func TestManagedCapacityPressureSkipsVariableEvidenceWhenRequestBodiesDisabled(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()
	require.NoError(t, svc.SystemService.SetStoragePolicy(ctx, &StoragePolicy{
		StoreChunks:                 true,
		StoreRequestBody:            false,
		StoreExecutionRequestBody:   lo.ToPtr(false),
		StoreResponseBody:           true,
		ManagedObservabilityHardMiB: lo.ToPtr(2),
		ManagedObservabilityLowMiB:  lo.ToPtr(1),
	}))

	requestCtx := contexts.WithProjectID(ctx, proj.ID)
	req, err := svc.CreateRequest(requestCtx,
		&llm.Request{Model: "gpt-4o"},
		&httpclient.Request{JSONBody: []byte(`{"secret":"not persisted"}`)},
		llm.APIFormatOpenAIChatCompletion,
	)
	require.NoError(t, err)
	require.True(t, req.ManagedObservability)
	require.Nil(t, req.RequestBodyPayloadID)

	channel := createStorageTestChannel(t, ctx, client, &objects.ChannelSettings{
		StoreExecutionRequestBody:  lo.ToPtr(false),
		StoreExecutionResponseBody: lo.ToPtr(true),
		StoreExecutionStreamChunks: lo.ToPtr(true),
	})
	execution, err := svc.CreateRequestExecution(ctx, channel, "gpt-4o", req,
		httpclient.Request{JSONBody: []byte(`{"execution":"not persisted"}`)}, llm.APIFormatOpenAIChatCompletion, false)
	require.NoError(t, err)
	require.True(t, execution.ManagedObservability)
	require.Nil(t, execution.RequestBodyPayloadID)

	require.NoError(t, svc.UpdateRequestExecutionCompletedForChannel(ctx, execution.ID, "external-exec",
		map[string]any{"response": strings.Repeat("x", 256<<10)}, nil, channel))
	require.NoError(t, svc.SaveRequestExecutionChunksForChannel(ctx, execution.ID, []*httpclient.StreamEvent{{
		Type: "data", Data: []byte(`{"chunk":"not persisted"}`),
	}}, channel))
	require.NoError(t, svc.UpdateRequestCompleted(ctx, req.ID, "external-request",
		map[string]any{"response": strings.Repeat("y", 256<<10)}, nil))
	pressureReason := client.ManagedObservabilityState.GetX(ctx, 1).LastError

	usage, err := svc.UsageLogService.CreateUsageLog(ctx, CreateUsageLogParams{
		RequestID: req.ID, ProjectID: req.ProjectID, ChannelID: channel.ID,
		ActualModelID: "gpt-4o", Usage: &llm.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
		Source: usagelog.SourceAPI, Format: string(llm.APIFormatOpenAIChatCompletion),
	})
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.Equal(t, int64(10), usage.PromptTokens)
	require.Equal(t, int64(20), usage.CompletionTokens)
	require.Equal(t, int64(30), usage.TotalTokens)

	updatedReq := client.Request.GetX(ctx, req.ID)
	updatedExecution := client.RequestExecution.GetX(ctx, execution.ID)
	require.Equal(t, request.StatusCompleted, updatedReq.Status)
	require.Empty(t, updatedReq.ResponseBody)
	require.Contains(t, *updatedReq.EvidenceDisposition.ResponseBody.FailureClass, "capacity_pressure")
	require.Equal(t, requestexecution.StatusCompleted, updatedExecution.Status)
	require.Empty(t, updatedExecution.ResponseBody)
	require.Empty(t, updatedExecution.ResponseChunks)
	require.Contains(t, *updatedExecution.EvidenceDisposition.ResponseBody.FailureClass, "capacity_pressure")
	require.Contains(t, *updatedExecution.EvidenceDisposition.ResponseChunks.FailureClass, "capacity_pressure")
	require.Equal(t, 1, client.UsageLog.Query().CountX(ctx))
	state := client.ManagedObservabilityState.GetX(ctx, 1)
	require.True(t, state.UnderPressure)
	require.Positive(t, state.ChargedBytes)
	require.Equal(t, pressureReason, state.LastError)
}

func TestManagedObservabilityFailureIsVisibleWithoutFailingStatus(t *testing.T) {
	svc, client, ctx, _ := setupRequestExecutionStorageTest(t)
	defer client.Close()
	svc.SystemService.RecordManagedObservabilityFailure(ctx, "gc_owner_lock", "failed")
	status, err := svc.SystemService.ManagedObservabilityStatus(ctx)
	require.NoError(t, err)
	require.True(t, status.UnderPressure)
	require.Equal(t, "gc_owner_lock:failed", status.LastError)
}

func createStorageTestChannel(t *testing.T, ctx context.Context, client *ent.Client, settings *objects.ChannelSettings) *Channel {
	t.Helper()

	ch, err := client.Channel.Create().
		SetType(entchannel.TypeAxonhub).
		SetName(t.Name()).
		SetBaseURL("http://upstream.local:8090").
		SetCredentials(objects.ChannelCredentials{}).
		SetSupportedModels([]string{"gpt-4o"}).
		SetManualModels([]string{}).
		SetDefaultTestModel("gpt-4o").
		SetSettings(settings).
		Save(ctx)
	require.NoError(t, err)

	return &Channel{Channel: ch}
}

func createStorageTestRequest(t *testing.T, ctx context.Context, client *ent.Client, proj *ent.Project, stream bool) *ent.Request {
	t.Helper()

	req, err := client.Request.Create().
		SetProjectID(proj.ID).
		SetModelID("gpt-4o").
		SetFormat(string(llm.APIFormatOpenAIChatCompletion)).
		SetRequestBody([]byte(`{"model":"gpt-4o"}`)).
		SetStatus(request.StatusProcessing).
		SetStream(stream).
		Save(ctx)
	require.NoError(t, err)

	return req
}

func createStorageTestExecution(t *testing.T, ctx context.Context, client *ent.Client, proj *ent.Project, req *ent.Request, channel *Channel) *ent.RequestExecution {
	t.Helper()
	return createStorageTestExecutionWithDataStorage(t, ctx, client, proj, req, channel, 0)
}

func createStorageTestExecutionWithDataStorage(t *testing.T, ctx context.Context, client *ent.Client, proj *ent.Project, req *ent.Request, channel *Channel, dataStorageID int) *ent.RequestExecution {
	t.Helper()

	create := client.RequestExecution.Create().
		SetProjectID(proj.ID).
		SetRequestID(req.ID).
		SetChannelID(channel.ID).
		SetModelID("gpt-4o").
		SetFormat(string(llm.APIFormatOpenAIChatCompletion)).
		SetRequestBody([]byte(`{"model":"gpt-4o"}`)).
		SetStatus(requestexecution.StatusProcessing).
		SetStream(req.Stream)
	if dataStorageID != 0 {
		create.SetDataStorageID(dataStorageID)
	}

	execution, err := create.Save(ctx)
	require.NoError(t, err)

	return execution
}

func TestRequestService_CreateRequestExecutionHonorsChannelRequestBodyStorageOverride(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()

	channel := createStorageTestChannel(t, ctx, client, &objects.ChannelSettings{
		StoreExecutionRequestBody: lo.ToPtr(false),
	})
	req := createStorageTestRequest(t, ctx, client, proj, false)

	execution, err := svc.CreateRequestExecution(ctx, channel, "gpt-4o", req, httpclient.Request{
		Body: []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
	}, llm.APIFormatOpenAIChatCompletion, false)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(execution.RequestBody))
	require.Equal(t, "omit", execution.EvidenceDisposition.RequestBody.Intent)
	require.Equal(t, "omitted", execution.EvidenceDisposition.RequestBody.Outcome)
}

func TestRequestService_CreateRequestExecutionCanEnableRequestBodyStoragePerChannel(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()
	require.NoError(t, svc.SystemService.SetStoragePolicy(ctx, &StoragePolicy{
		StoreChunks:       true,
		StoreRequestBody:  false,
		StoreResponseBody: true,
	}))

	channel := createStorageTestChannel(t, ctx, client, &objects.ChannelSettings{
		StoreExecutionRequestBody: lo.ToPtr(true),
	})
	req := createStorageTestRequest(t, ctx, client, proj, false)

	execution, err := svc.CreateRequestExecution(ctx, channel, "gpt-4o", req, httpclient.Request{
		Body: []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
	}, llm.APIFormatOpenAIChatCompletion, false)
	require.NoError(t, err)
	loaded, err := svc.LoadRequestExecutionRequestBody(ctx, execution)
	require.NoError(t, err)
	require.Contains(t, string(loaded), `"content":"hi"`)
	require.Equal(t, "persist", execution.EvidenceDisposition.RequestBody.Intent)
}

func TestRequestServiceExecutionGlobalSwitchSplitsParentSemantics(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()
	channel := createStorageTestChannel(t, ctx, client, nil)
	req := createStorageTestRequest(t, ctx, client, proj, false)
	body := []byte(`{"model":"gpt-4o","split":true}`)

	require.NoError(t, svc.SystemService.SetStoragePolicy(ctx, &StoragePolicy{
		StoreRequestBody: true, StoreExecutionRequestBody: lo.ToPtr(false), StoreResponseBody: true,
	}))
	omitted, err := svc.CreateRequestExecution(ctx, channel, "gpt-4o", req, httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion, false)
	require.NoError(t, err)
	require.Nil(t, omitted.RequestBodyPayloadID)
	require.Equal(t, "omit", omitted.EvidenceDisposition.RequestBody.Intent)

	require.NoError(t, svc.SystemService.SetStoragePolicy(ctx, &StoragePolicy{
		StoreRequestBody: false, StoreExecutionRequestBody: lo.ToPtr(true), StoreResponseBody: true,
	}))
	stored, err := svc.CreateRequestExecution(ctx, channel, "gpt-4o", req, httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion, false)
	require.NoError(t, err)
	require.NotNil(t, stored.RequestBodyPayloadID)
	loaded, err := svc.LoadRequestExecutionRequestBody(ctx, stored)
	require.NoError(t, err)
	require.Equal(t, body, []byte(loaded))
}

func managedStorageTestBody(t *testing.T, rawBytes int) []byte {
	t.Helper()
	raw := make([]byte, rawBytes)
	_, err := rand.Read(raw)
	require.NoError(t, err)
	return []byte(`{"model":"gpt-4o","blob":"` + base64.RawStdEncoding.EncodeToString(raw) + `"}`)
}

func TestRequestServiceManagedRequestBodyExactDedupAndVariants(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()
	ctx = contexts.WithProjectID(ctx, proj.ID)
	channel := createStorageTestChannel(t, ctx, client, nil)
	body := managedStorageTestBody(t, 768*1024)

	req, err := svc.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	require.JSONEq(t, `{}`, string(req.RequestBody))
	require.NotNil(t, req.RequestBodyPayloadID)

	execution, err := svc.CreateRequestExecution(ctx, channel, "gpt-4o", req, httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion, true)
	require.NoError(t, err)
	require.NotNil(t, execution.RequestBodyPayloadID)
	require.Equal(t, *req.RequestBodyPayloadID, *execution.RequestBodyPayloadID)
	require.Equal(t, 1, client.ObservabilityPayload.Query().Where(observabilitypayload.RequestIDEQ(req.ID)).CountX(ctx))

	loadedParent, err := svc.LoadRequestBody(ctx, req)
	require.NoError(t, err)
	require.Equal(t, body, []byte(loadedParent))
	loadedExecution, err := svc.LoadRequestExecutionRequestBody(ctx, execution)
	require.NoError(t, err)
	require.Equal(t, body, []byte(loadedExecution))

	variant := append([]byte(nil), body...)
	variant[len(variant)-3] ^= 1
	second, err := svc.CreateRequestExecution(ctx, channel, "gpt-4o", req, httpclient.Request{JSONBody: variant}, llm.APIFormatOpenAIChatCompletion, false)
	require.NoError(t, err)
	require.NotNil(t, second.RequestBodyPayloadID)
	require.NotEqual(t, *execution.RequestBodyPayloadID, *second.RequestBodyPayloadID)
	require.Equal(t, 2, client.ObservabilityPayload.Query().Where(observabilitypayload.RequestIDEQ(req.ID)).CountX(ctx))
	loadedVariant, err := svc.LoadRequestExecutionRequestBody(ctx, second)
	require.NoError(t, err)
	require.Equal(t, variant, []byte(loadedVariant))
	retry, err := svc.CreateRequestExecution(ctx, channel, "gpt-4o", req, httpclient.Request{JSONBody: variant}, llm.APIFormatOpenAIChatCompletion, true)
	require.NoError(t, err)
	require.Equal(t, *second.RequestBodyPayloadID, *retry.RequestBodyPayloadID, "retry/failover reuses the exact final variant")
	require.Equal(t, 2, client.ObservabilityPayload.Query().Where(observabilitypayload.RequestIDEQ(req.ID)).CountX(ctx))
}

func TestRequestServiceManagedCapacityPressureKeepsSkeleton(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()
	ctx = contexts.WithProjectID(ctx, proj.ID)
	require.Error(t, svc.SystemService.SetStoragePolicy(ctx, &StoragePolicy{
		StoreRequestBody:            true,
		StoreExecutionRequestBody:   lo.ToPtr(true),
		StoreResponseBody:           true,
		ManagedObservabilityHardMiB: lo.ToPtr(1),
		ManagedObservabilityLowMiB:  lo.ToPtr(1),
	}))
	require.NoError(t, svc.SystemService.SetStoragePolicy(ctx, &StoragePolicy{
		StoreRequestBody:            true,
		StoreExecutionRequestBody:   lo.ToPtr(true),
		StoreResponseBody:           true,
		ManagedObservabilityHardMiB: lo.ToPtr(2),
		ManagedObservabilityLowMiB:  lo.ToPtr(1),
	}))
	body := managedStorageTestBody(t, 2*1024*1024)
	req, err := svc.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: body}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)
	require.Nil(t, req.RequestBodyPayloadID)
	require.JSONEq(t, `{}`, string(req.RequestBody))
	require.Equal(t, "capacity_pressure", *req.EvidenceDisposition.RequestBody.FailureClass)
	require.Equal(t, "omitted", req.EvidenceDisposition.RequestBody.Outcome)
	require.NotEmpty(t, req.EvidenceDisposition.RequestBody.SHA256)
	require.Equal(t, int64(len(body)), *req.EvidenceDisposition.RequestBody.ByteLength)
	require.Zero(t, client.ObservabilityPayload.Query().CountX(ctx))
	state := client.ManagedObservabilityState.GetX(ctx, 1)
	require.True(t, state.UnderPressure)
}

func TestRequestService_UpdateRequestExecutionCompletedLoadsChannelResponseBodyOverride(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()

	channel := createStorageTestChannel(t, ctx, client, &objects.ChannelSettings{
		StoreExecutionResponseBody: lo.ToPtr(false),
	})
	req := createStorageTestRequest(t, ctx, client, proj, false)
	execution := createStorageTestExecution(t, ctx, client, proj, req, channel)

	require.NoError(t, svc.UpdateRequestExecutionCompleted(ctx, execution.ID, "upstream-id", map[string]string{"answer": "hi"}, nil))

	updated, err := client.RequestExecution.Get(ctx, execution.ID)
	require.NoError(t, err)
	require.Empty(t, updated.ResponseBody)
	require.Equal(t, requestexecution.StatusCompleted, updated.Status)
	require.Equal(t, "omit", updated.EvidenceDisposition.ResponseBody.Intent)
}

func TestRequestService_UpdateRequestExecutionCompletedCanEnableResponseBodyStoragePerChannel(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()
	require.NoError(t, svc.SystemService.SetStoragePolicy(ctx, &StoragePolicy{
		StoreChunks:       true,
		StoreRequestBody:  true,
		StoreResponseBody: false,
	}))

	channel := createStorageTestChannel(t, ctx, client, &objects.ChannelSettings{
		StoreExecutionResponseBody: lo.ToPtr(true),
	})
	req := createStorageTestRequest(t, ctx, client, proj, false)
	execution := createStorageTestExecution(t, ctx, client, proj, req, channel)

	require.NoError(t, svc.UpdateRequestExecutionCompletedForChannel(
		ctx,
		execution.ID,
		"upstream-id",
		map[string]string{"answer": "hi"},
		nil,
		channel,
	))

	updated, err := client.RequestExecution.Get(ctx, execution.ID)
	require.NoError(t, err)
	require.JSONEq(t, `{"answer":"hi"}`, string(updated.ResponseBody))
	require.Equal(t, "persist", updated.EvidenceDisposition.ResponseBody.Intent)
}

func TestRequestService_FailedResponseBodyStoragePolicy(t *testing.T) {
	tests := []struct {
		name              string
		storeResponseBody bool
		channelOverride   *bool
		wantRequestBody   bool
		wantExecutionBody bool
	}{
		{
			name:              "channel disable overrides global enable",
			storeResponseBody: true,
			channelOverride:   lo.ToPtr(false),
			wantRequestBody:   true,
			wantExecutionBody: false,
		},
		{
			name:              "channel enable overrides global disable",
			storeResponseBody: false,
			channelOverride:   lo.ToPtr(true),
			wantRequestBody:   false,
			wantExecutionBody: true,
		},
		{
			name:              "global and channel disabled",
			storeResponseBody: false,
			channelOverride:   lo.ToPtr(false),
			wantRequestBody:   false,
			wantExecutionBody: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
			defer client.Close()
			require.NoError(t, svc.SystemService.SetStoragePolicy(ctx, &StoragePolicy{
				StoreRequestBody:          true,
				StoreExecutionRequestBody: lo.ToPtr(true),
				StoreResponseBody:         tt.storeResponseBody,
			}))

			ctx = contexts.WithProjectID(ctx, proj.ID)
			channel := createStorageTestChannel(t, ctx, client, &objects.ChannelSettings{
				StoreExecutionResponseBody: tt.channelOverride,
			})
			requestBody := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"failure"}]}`)
			req, err := svc.CreateRequest(ctx, &llm.Request{Model: "gpt-4o"}, &httpclient.Request{JSONBody: requestBody}, llm.APIFormatOpenAIChatCompletion)
			require.NoError(t, err)
			execution, err := svc.CreateRequestExecution(ctx, channel, "gpt-4o", req, httpclient.Request{JSONBody: requestBody}, llm.APIFormatOpenAIChatCompletion, false)
			require.NoError(t, err)

			statusCode := 400
			errorBody := []byte(`{"error":{"message":"bad request"},"access_token":"synthetic-placeholder"}`)
			errorInfo := &ExecutionErrorInfo{StatusCode: &statusCode, ResponseBody: errorBody}
			require.NoError(t, svc.UpdateRequestExecutionStatusFromErrorDetails(ctx, execution.ID, errors.New("bad request"), nil, "bad request", errorInfo))
			require.NoError(t, svc.UpdateRequestStatusFromErrorDetails(ctx, req.ID, errors.New("bad request"), nil, errorInfo))

			storedRequest := client.Request.GetX(ctx, req.ID)
			storedExecution := client.RequestExecution.GetX(ctx, execution.ID)
			if tt.wantRequestBody {
				require.Equal(t, errorBody, []byte(storedRequest.ResponseBody))
				require.Equal(t, "stored", storedRequest.EvidenceDisposition.ResponseBody.Outcome)
			} else {
				require.Empty(t, storedRequest.ResponseBody)
				require.Equal(t, "omit", storedRequest.EvidenceDisposition.ResponseBody.Intent)
				require.Equal(t, "omitted", storedRequest.EvidenceDisposition.ResponseBody.Outcome)
			}
			if tt.wantExecutionBody {
				require.Equal(t, errorBody, []byte(storedExecution.ResponseBody))
				require.Equal(t, "stored", storedExecution.EvidenceDisposition.ResponseBody.Outcome)
			} else {
				require.Empty(t, storedExecution.ResponseBody)
				require.Equal(t, "omit", storedExecution.EvidenceDisposition.ResponseBody.Intent)
				require.Equal(t, "omitted", storedExecution.EvidenceDisposition.ResponseBody.Outcome)
			}
		})
	}
}

func TestRequestService_FailedResponseBodyUsesExistingExternalStoragePaths(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()
	dir := t.TempDir()
	fsStorage := client.DataStorage.Create().
		SetName("failed-response-evidence-fs").
		SetDescription("failed response evidence").
		SetType(datastorage.TypeFs).
		SetStatus(datastorage.StatusActive).
		SetSettings(&objects.DataStorageSettings{Directory: &dir}).
		SaveX(ctx)
	channel := createStorageTestChannel(t, ctx, client, &objects.ChannelSettings{
		StoreExecutionResponseBody: lo.ToPtr(true),
	})
	req := client.Request.Create().
		SetProjectID(proj.ID).
		SetDataStorageID(fsStorage.ID).
		SetModelID("gpt-4o").
		SetRequestBody([]byte(`{}`)).
		SetStatus(request.StatusProcessing).
		SaveX(ctx)
	execution := createStorageTestExecutionWithDataStorage(t, ctx, client, proj, req, channel, fsStorage.ID)

	statusCode := 400
	errorBody := []byte("{\n  \"error\": {\"message\": \"external bad request\"},\n  \"access_token\": \"synthetic-placeholder\"\n}\n")
	errorInfo := &ExecutionErrorInfo{StatusCode: &statusCode, ResponseBody: errorBody}
	require.NoError(t, svc.UpdateRequestExecutionStatusFromErrorDetails(ctx, execution.ID, errors.New("bad request"), nil, "bad request", errorInfo))
	require.NoError(t, svc.UpdateRequestStatusFromErrorDetails(ctx, req.ID, errors.New("bad request"), nil, errorInfo))

	storedRequest := client.Request.GetX(ctx, req.ID)
	storedExecution := client.RequestExecution.GetX(ctx, execution.ID)
	require.Empty(t, storedRequest.ResponseBody)
	require.Empty(t, storedExecution.ResponseBody)
	require.Equal(t, "external", storedRequest.EvidenceDisposition.ResponseBody.Location)
	require.Equal(t, "stored", storedRequest.EvidenceDisposition.ResponseBody.Outcome)
	require.Equal(t, "external", storedExecution.EvidenceDisposition.ResponseBody.Location)
	require.Equal(t, "stored", storedExecution.EvidenceDisposition.ResponseBody.Outcome)
	requestEvidence, err := svc.LoadResponseBodyEvidenceBounded(ctx, storedRequest, int64(len(errorBody)))
	require.NoError(t, err)
	require.Equal(t, errorBody, []byte(requestEvidence))
	executionEvidence, err := svc.LoadRequestExecutionResponseBodyEvidenceBounded(ctx, storedExecution, int64(len(errorBody)))
	require.NoError(t, err)
	require.Equal(t, errorBody, []byte(executionEvidence))
}

func TestRequestService_FailedTextResponseBodyKeepsRawExternalBytes(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()
	dir := t.TempDir()
	fsStorage := client.DataStorage.Create().
		SetName("failed-text-response-evidence-fs").
		SetDescription("failed text response evidence").
		SetType(datastorage.TypeFs).
		SetStatus(datastorage.StatusActive).
		SetSettings(&objects.DataStorageSettings{Directory: &dir}).
		SaveX(ctx)
	channel := createStorageTestChannel(t, ctx, client, &objects.ChannelSettings{
		StoreExecutionResponseBody: lo.ToPtr(true),
	})
	req := client.Request.Create().
		SetProjectID(proj.ID).
		SetDataStorageID(fsStorage.ID).
		SetModelID("gpt-4o").
		SetRequestBody([]byte(`{}`)).
		SetStatus(request.StatusProcessing).
		SaveX(ctx)
	execution := createStorageTestExecutionWithDataStorage(t, ctx, client, proj, req, channel, fsStorage.ID)

	statusCode := 502
	errorBody := []byte("upstream text failure: synthetic-placeholder\n")
	errorInfo := &ExecutionErrorInfo{StatusCode: &statusCode, ResponseBody: errorBody}
	require.NoError(t, svc.UpdateRequestExecutionStatusFromErrorDetails(ctx, execution.ID, errors.New("upstream text failure"), nil, "upstream text failure", errorInfo))
	require.NoError(t, svc.UpdateRequestStatusFromErrorDetails(ctx, req.ID, errors.New("upstream text failure"), nil, errorInfo))

	requestBytes, err := svc.DataStorageService.LoadData(ctx, fsStorage, GenerateResponseBodyKey(proj.ID, req.ID))
	require.NoError(t, err)
	require.Equal(t, errorBody, requestBytes)
	executionBytes, err := svc.DataStorageService.LoadData(ctx, fsStorage, GenerateExecutionResponseBodyKey(proj.ID, req.ID, execution.ID))
	require.NoError(t, err)
	require.Equal(t, errorBody, executionBytes)
}

func TestRequestService_SaveRequestExecutionChunksHonorsChannelOverride(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()

	channel := createStorageTestChannel(t, ctx, client, &objects.ChannelSettings{
		StoreExecutionStreamChunks: lo.ToPtr(false),
	})
	req := createStorageTestRequest(t, ctx, client, proj, true)
	execution := createStorageTestExecution(t, ctx, client, proj, req, channel)

	require.NoError(t, svc.SaveRequestExecutionChunksForChannel(ctx, execution.ID, []*httpclient.StreamEvent{
		{Type: "message", Data: []byte(`{"delta":"hello"}`)},
	}, channel))

	updated, err := client.RequestExecution.Get(ctx, execution.ID)
	require.NoError(t, err)
	require.Empty(t, updated.ResponseChunks)
	require.Equal(t, "omit", updated.EvidenceDisposition.ResponseChunks.Intent)
}

func TestRequestService_SaveRequestExecutionChunksCanEnableStoragePerChannel(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()
	require.NoError(t, svc.SystemService.SetStoragePolicy(ctx, &StoragePolicy{
		StoreChunks:       false,
		StoreRequestBody:  true,
		StoreResponseBody: true,
	}))

	channel := createStorageTestChannel(t, ctx, client, &objects.ChannelSettings{
		StoreExecutionStreamChunks: lo.ToPtr(true),
	})
	req := createStorageTestRequest(t, ctx, client, proj, true)
	execution := createStorageTestExecution(t, ctx, client, proj, req, channel)

	require.NoError(t, svc.SaveRequestExecutionChunksForChannel(ctx, execution.ID, []*httpclient.StreamEvent{
		{Type: "message", Data: []byte(`{"delta":"hello"}`)},
	}, channel))

	updated, err := client.RequestExecution.Get(ctx, execution.ID)
	require.NoError(t, err)
	require.Len(t, updated.ResponseChunks, 1)
	require.Equal(t, "persist", updated.EvidenceDisposition.ResponseChunks.Intent)
}

func TestRequestService_LoadRequestExecutionResponseChunksByStatus(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()

	channel := createStorageTestChannel(t, ctx, client, nil)
	req := createStorageTestRequest(t, ctx, client, proj, true)
	storedEvent := &httpclient.StreamEvent{Type: "message", Data: []byte(`{"delta":"stored"}`)}

	for _, status := range []requestexecution.Status{
		requestexecution.StatusCompleted,
		requestexecution.StatusFailed,
		requestexecution.StatusCanceled,
	} {
		t.Run(string(status), func(t *testing.T) {
			execution := createStorageTestExecution(t, ctx, client, proj, req, channel)
			require.NoError(t, svc.SaveRequestExecutionChunksForChannel(ctx, execution.ID, []*httpclient.StreamEvent{storedEvent}, channel))
			execution, err := client.RequestExecution.UpdateOneID(execution.ID).SetStatus(status).Save(ctx)
			require.NoError(t, err)

			chunks, err := svc.LoadRequestExecutionResponseChunks(ctx, execution)
			require.NoError(t, err)
			require.Len(t, chunks, 1)
			require.JSONEq(t, `{"event":"message","data":{"delta":"stored"}}`, string(chunks[0]))
		})
	}

	t.Run("processing uses live chunks", func(t *testing.T) {
		execution := createStorageTestExecution(t, ctx, client, proj, req, channel)
		require.NoError(t, svc.SaveRequestExecutionChunksForChannel(ctx, execution.ID, []*httpclient.StreamEvent{storedEvent}, channel))
		buffer := chunkbuffer.New()
		require.True(t, buffer.Append(&httpclient.StreamEvent{Type: "message", Data: []byte(`{"delta":"live"}`)}))
		svc.LiveStreamRegistry.RegisterExecution(execution.ID, buffer)

		chunks, err := svc.LoadRequestExecutionResponseChunks(ctx, execution)
		require.NoError(t, err)
		require.Len(t, chunks, 1)
		require.JSONEq(t, `{"event":"message","data":{"delta":"live"}}`, string(chunks[0]))
	})

	t.Run("pending does not expose stored chunks", func(t *testing.T) {
		execution := createStorageTestExecution(t, ctx, client, proj, req, channel)
		require.NoError(t, svc.SaveRequestExecutionChunksForChannel(ctx, execution.ID, []*httpclient.StreamEvent{storedEvent}, channel))
		execution, err := client.RequestExecution.UpdateOneID(execution.ID).SetStatus(requestexecution.StatusPending).Save(ctx)
		require.NoError(t, err)

		chunks, err := svc.LoadRequestExecutionResponseChunks(ctx, execution)
		require.NoError(t, err)
		require.Empty(t, chunks)
	})

	t.Run("failed execution with no chunks remains empty", func(t *testing.T) {
		execution := createStorageTestExecution(t, ctx, client, proj, req, channel)
		execution, err := client.RequestExecution.UpdateOneID(execution.ID).SetStatus(requestexecution.StatusFailed).Save(ctx)
		require.NoError(t, err)

		chunks, err := svc.LoadRequestExecutionResponseChunks(ctx, execution)
		require.NoError(t, err)
		require.Empty(t, chunks)
	})
}

func TestRequestService_LoadFailedExecutionResponseChunksFromExternalStorage(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()

	dir := t.TempDir()
	dataStorage, err := client.DataStorage.Create().
		SetName("failed-execution-fs").
		SetDescription("failed execution chunks").
		SetPrimary(false).
		SetType(datastorage.TypeFs).
		SetSettings(&objects.DataStorageSettings{Directory: &dir}).
		SetStatus(datastorage.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	channel := createStorageTestChannel(t, ctx, client, nil)
	req := createStorageTestRequest(t, ctx, client, proj, true)
	execution := createStorageTestExecutionWithDataStorage(t, ctx, client, proj, req, channel, dataStorage.ID)

	require.NoError(t, svc.SaveRequestExecutionChunksForChannel(ctx, execution.ID, []*httpclient.StreamEvent{
		{Type: "message", Data: []byte(`{"delta":"external partial"}`)},
	}, channel))
	execution, err = client.RequestExecution.UpdateOneID(execution.ID).SetStatus(requestexecution.StatusFailed).Save(ctx)
	require.NoError(t, err)
	require.Empty(t, execution.ResponseChunks)

	chunks, err := svc.LoadRequestExecutionResponseChunks(ctx, execution)
	require.NoError(t, err)
	require.Len(t, chunks, 1)
	require.JSONEq(t, `{"event":"message","data":{"delta":"external partial"}}`, string(chunks[0]))
}

func TestRequestService_LoadTerminalRequestResponseChunks(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()

	require.NoError(t, svc.SystemService.SetStoragePolicy(ctx, &StoragePolicy{StoreChunks: true}))
	storedEvent := &httpclient.StreamEvent{Type: "message", Data: []byte(`{"delta":"partial"}`)}

	for _, status := range []request.Status{
		request.StatusCompleted,
		request.StatusFailed,
		request.StatusCanceled,
	} {
		t.Run(string(status), func(t *testing.T) {
			req := createStorageTestRequest(t, ctx, client, proj, true)
			require.NoError(t, svc.SaveRequestChunks(ctx, req.ID, []*httpclient.StreamEvent{storedEvent}))
			req, err := client.Request.UpdateOneID(req.ID).SetStatus(status).Save(ctx)
			require.NoError(t, err)

			chunks, err := svc.LoadResponseChunks(ctx, req)
			require.NoError(t, err)
			require.Len(t, chunks, 1)
			require.JSONEq(t, `{"event":"message","data":{"delta":"partial"}}`, string(chunks[0]))
		})
	}

	t.Run("pending does not expose stored chunks", func(t *testing.T) {
		req := createStorageTestRequest(t, ctx, client, proj, true)
		require.NoError(t, svc.SaveRequestChunks(ctx, req.ID, []*httpclient.StreamEvent{storedEvent}))

		chunks, err := svc.LoadResponseChunks(ctx, req)
		require.NoError(t, err)
		require.Empty(t, chunks)
	})
}
