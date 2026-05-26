package biz

import (
	"context"
	"testing"

	"entgo.io/ent/dialect"
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

	client := enttest.Open(t, dialect.SQLite, "file:ent?mode=memory&_fk=1")
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	systemService := NewSystemService(SystemServiceParams{
		Ent: client,
	})
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

	exec, err := client.RequestExecution.Create().
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

	return exec
}

func TestRequestService_CreateRequestExecutionHonorsChannelRequestBodyStorageOverride(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()

	channel := createStorageTestChannel(t, ctx, client, &objects.ChannelSettings{
		StoreExecutionRequestBody: lo.ToPtr(false),
	})
	req := createStorageTestRequest(t, ctx, client, proj, false)

	exec, err := svc.CreateRequestExecution(ctx, channel, "gpt-4o", req, httpclient.Request{
		Body: []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
	}, llm.APIFormatOpenAIChatCompletion)
	require.NoError(t, err)

	require.JSONEq(t, `{}`, string(exec.RequestBody))
}

func TestRequestService_UpdateRequestExecutionCompletedHonorsChannelResponseBodyStorageOverride(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()

	channel := createStorageTestChannel(t, ctx, client, &objects.ChannelSettings{
		StoreExecutionResponseBody: lo.ToPtr(false),
	})
	req := createStorageTestRequest(t, ctx, client, proj, false)
	exec := createStorageTestExecution(t, ctx, client, proj, req, channel)

	err := svc.UpdateRequestExecutionCompletedForChannel(ctx, exec.ID, "upstream-id", map[string]string{"answer": "hi"}, nil, channel)
	require.NoError(t, err)

	updated, err := client.RequestExecution.Get(ctx, exec.ID)
	require.NoError(t, err)
	require.Empty(t, updated.ResponseBody)
	require.Equal(t, requestexecution.StatusCompleted, updated.Status)
}

func TestRequestService_SaveRequestExecutionChunksHonorsChannelStreamChunkStorageOverride(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()

	channel := createStorageTestChannel(t, ctx, client, &objects.ChannelSettings{
		StoreExecutionStreamChunks: lo.ToPtr(false),
	})
	req := createStorageTestRequest(t, ctx, client, proj, true)
	exec := createStorageTestExecution(t, ctx, client, proj, req, channel)

	err := svc.SaveRequestExecutionChunksForChannel(ctx, exec.ID, []*httpclient.StreamEvent{
		{Type: "message", Data: []byte(`{"delta":"hello"}`)},
	}, channel)
	require.NoError(t, err)

	updated, err := client.RequestExecution.Get(ctx, exec.ID)
	require.NoError(t, err)
	require.Empty(t, updated.ResponseChunks)
}

func TestRequestService_SaveRequestExecutionChunksCanEnableStoragePerChannel(t *testing.T) {
	svc, client, ctx, proj := setupRequestExecutionStorageTest(t)
	defer client.Close()

	require.NoError(t, svc.SystemService.SetStoragePolicy(ctx, &StoragePolicy{
		StoreChunks:       false,
		LivePreview:       false,
		StoreRequestBody:  true,
		StoreResponseBody: true,
	}))

	channel := createStorageTestChannel(t, ctx, client, &objects.ChannelSettings{
		StoreExecutionStreamChunks: lo.ToPtr(true),
	})
	req := createStorageTestRequest(t, ctx, client, proj, true)
	exec := createStorageTestExecution(t, ctx, client, proj, req, channel)

	err := svc.SaveRequestExecutionChunksForChannel(ctx, exec.ID, []*httpclient.StreamEvent{
		{Type: "message", Data: []byte(`{"delta":"hello"}`)},
	}, channel)
	require.NoError(t, err)

	updated, err := client.RequestExecution.Get(ctx, exec.ID)
	require.NoError(t, err)
	require.Len(t, updated.ResponseChunks, 1)
}
