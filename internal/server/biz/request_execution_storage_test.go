package biz

import (
	"context"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	entchannel "github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
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

	return NewRequestService(client, systemService, usageLogService, dataStorageService, NewLiveStreamRegistry()), client, ctx, proj
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

	execution, err := client.RequestExecution.Create().
		SetProjectID(proj.ID).
		SetRequestID(req.ID).
		SetChannelID(channel.ID).
		SetModelID("gpt-4o").
		SetFormat(string(llm.APIFormatOpenAIChatCompletion)).
		SetRequestBody([]byte(`{"model":"gpt-4o"}`)).
		SetStatus(requestexecution.StatusProcessing).
		SetStream(req.Stream).
		Save(ctx)
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
	require.Contains(t, string(execution.RequestBody), `"content":"hi"`)
	require.Equal(t, "persist", execution.EvidenceDisposition.RequestBody.Intent)
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
