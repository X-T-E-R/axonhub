package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/hook"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/pipeline/stream"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

type managedBodyBarrierExecutor struct {
	response *httpclient.Response
	onDo     func()
}

func (e *managedBodyBarrierExecutor) Do(context.Context, *httpclient.Request) (*httpclient.Response, error) {
	e.onDo()
	return e.response, nil
}

func (e *managedBodyBarrierExecutor) DoStream(context.Context, *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
	return nil, errors.New("unexpected streaming request")
}

func setupAsyncManagedBodyOrchestrator(t *testing.T, client *ent.Client, writer *biz.ManagedRequestBodyWriter, executor pipeline.Executor) (*ChatCompletionOrchestrator, context.Context) {
	t.Helper()
	ctx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	project := createTestProject(t, ctx, client)
	channelRow := createTestChannel(t, ctx, client)
	createOutboundTestPrimaryDataStorage(t, ctx, client)
	channelService, legacyRequestService, systemService, usageLogService := setupTestServices(t, client)
	requestService := biz.NewRequestServiceWithManagedRequestBodyWriter(
		client,
		systemService,
		usageLogService,
		legacyRequestService.DataStorageService,
		legacyRequestService.LiveStreamRegistry,
		writer,
	)
	outbound, err := openai.NewOutboundTransformer(channelRow.BaseURL, channelRow.Credentials.APIKey)
	require.NoError(t, err)
	bizChannel := &biz.Channel{Channel: channelRow, Outbound: outbound}
	orchestrator := &ChatCompletionOrchestrator{
		channelSelector:       &staticChannelSelector{candidates: channelsToTestCandidates([]*biz.Channel{bizChannel}, "gpt-4")},
		Inbound:               openai.NewInboundTransformer(),
		RequestService:        requestService,
		ChannelService:        channelService,
		PromptProvider:        &stubPromptProvider{},
		SystemService:         systemService,
		UsageLogService:       usageLogService,
		PipelineFactory:       pipeline.NewFactory(executor),
		ModelMapper:           NewModelMapper(),
		channelLimiterManager: NewChannelLimiterManager(),
		Middlewares:           []pipeline.Middleware{stream.EnsureUsage()},
	}
	return orchestrator, contexts.WithProjectID(ctx, project.ID)
}

func TestManagedRequestBodiesDoNotBlockProviderDispatch(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:async-managed-provider-barrier?mode=memory&_fk=0")
	defer client.Close()
	ctx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	systemService := biz.NewSystemService(biz.SystemServiceParams{Ent: client})
	writer := biz.NewManagedRequestBodyWriter(biz.ManagedRequestBodyWriterConfig{}, client, systemService)
	blocked := make(chan struct{})
	release := make(chan struct{})
	var blockedOnce atomic.Bool
	writer.SetBeforePersistHookForTest(func(context.Context) {
		if blockedOnce.CompareAndSwap(false, true) {
			close(blocked)
		}
		<-release
	})
	require.NoError(t, writer.Start(ctx))

	providerChecked := false
	executor := &managedBodyBarrierExecutor{
		response: &httpclient.Response{
			StatusCode: http.StatusOK,
			Body:       buildMockOpenAIResponse("chatcmpl-async", "gpt-4", "ready", 2, 3),
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
		},
		onDo: func() {
			select {
			case <-blocked:
			case <-time.After(2 * time.Second):
				require.FailNow(t, "managed request-body worker did not reach the persistence barrier")
			}
			parents := client.Request.Query().AllX(ctx)
			executions := client.RequestExecution.Query().AllX(ctx)
			require.Len(t, parents, 1)
			require.Len(t, executions, 1)
			for _, evidence := range []struct {
				pointer     *int
				disposition string
				outcome     string
			}{
				{parents[0].RequestBodyPayloadID, *parents[0].EvidenceDisposition.RequestBody.FailureClass, parents[0].EvidenceDisposition.RequestBody.Outcome},
				{executions[0].RequestBodyPayloadID, *executions[0].EvidenceDisposition.RequestBody.FailureClass, executions[0].EvidenceDisposition.RequestBody.Outcome},
			} {
				require.Nil(t, evidence.pointer)
				require.Equal(t, "async_pending", evidence.disposition)
				require.Equal(t, "unavailable", evidence.outcome)
			}
			providerChecked = true
		},
	}
	orchestrator, requestCtx := setupAsyncManagedBodyOrchestrator(t, client, writer, executor)
	result, err := orchestrator.Process(requestCtx, buildTestRequest("gpt-4", "critical path", false))
	require.NoError(t, err)
	require.NotNil(t, result.ChatCompletion)
	require.True(t, providerChecked)
	close(release)
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, writer.Stop(stopCtx))
}

func TestManagedRequestBodySkeletonFailurePreventsProviderDispatch(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:async-managed-skeleton-failure?mode=memory&_fk=0")
	defer client.Close()
	ctx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	systemService := biz.NewSystemService(biz.SystemServiceParams{Ent: client})
	writer := biz.NewManagedRequestBodyWriter(biz.ManagedRequestBodyWriterConfig{}, client, systemService)
	require.NoError(t, writer.Start(ctx))
	executor := &mockExecutor{response: &httpclient.Response{StatusCode: http.StatusOK, Body: buildMockOpenAIResponse("unused", "gpt-4", "unused", 1, 1)}}
	orchestrator, requestCtx := setupAsyncManagedBodyOrchestrator(t, client, writer, executor)
	client.Request.Use(func(next ent.Mutator) ent.Mutator {
		return hook.RequestFunc(func(hookCtx context.Context, mutation *ent.RequestMutation) (ent.Value, error) {
			if mutation.Op().Is(ent.OpCreate) {
				return nil, errors.New("skeleton database unavailable")
			}
			return next.Mutate(hookCtx, mutation)
		})
	})

	_, err := orchestrator.Process(requestCtx, buildTestRequest("gpt-4", "must not dispatch", false))
	require.ErrorContains(t, err, "skeleton database unavailable")
	require.Zero(t, executor.requestCalls.Load())
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, writer.Stop(stopCtx))
}
