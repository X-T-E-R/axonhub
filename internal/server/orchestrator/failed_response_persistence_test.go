package orchestrator

import (
	"context"
	"net/http"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/pipeline/stream"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

func newFailedResponsePersistenceOrchestrator(
	t *testing.T,
	ctx context.Context,
	client *ent.Client,
	channel *ent.Channel,
	outbound transformer.Outbound,
	executor pipeline.Executor,
) (*ChatCompletionOrchestrator, *biz.RequestService) {
	t.Helper()
	channelService, requestService, systemService, usageLogService := setupTestServices(t, client)
	require.NoError(t, systemService.SetStoragePolicy(ctx, &biz.StoragePolicy{
		StoreRequestBody:          true,
		StoreExecutionRequestBody: lo.ToPtr(true),
		StoreResponseBody:         true,
	}))
	bizChannel := &biz.Channel{Channel: channel, Outbound: outbound}
	return &ChatCompletionOrchestrator{
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
	}, requestService
}

func TestChatCompletionOrchestrator_Process_PassThroughBadRequestPersistsEvidence(t *testing.T) {
	ctx := authz.WithTestBypass(context.Background())
	client := enttest.NewEntClient(t, "sqlite3", "file:pass-through-bad-request-evidence?mode=memory&_fk=0")
	defer client.Close()
	ctx = ent.NewContext(ctx, client)

	project := createTestProject(t, ctx, client)
	channel := createTestChannel(t, ctx, client)
	channel.Settings = &objects.ChannelSettings{
		PassThroughBody:            lo.ToPtr(true),
		DisableRetries:             true,
		StoreExecutionRequestBody:  lo.ToPtr(true),
		StoreExecutionResponseBody: lo.ToPtr(true),
	}
	errorBody := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"messages[97]: send a user message or a nonempty assistant text prefill for Gemini continuation"},"access_token":"synthetic-placeholder"}`)
	executor := &mockExecutor{err: &httpclient.Error{
		Method:     http.MethodPost,
		URL:        "http://sub2api:8080/v1/messages",
		StatusCode: http.StatusBadRequest,
		Status:     "400 Bad Request",
		Body:       errorBody,
	}}
	outbound, err := openai.NewOutboundTransformer(channel.BaseURL, channel.Credentials.APIKey)
	require.NoError(t, err)
	orchestrator, requestService := newFailedResponsePersistenceOrchestrator(t, ctx, client, channel, outbound, executor)

	inbound := buildTestRequest("gpt-4", "failure evidence", true)
	_, err = orchestrator.Process(contexts.WithProjectID(ctx, project.ID), inbound)
	require.Error(t, err)

	storedRequest := client.Request.Query().OnlyX(ctx)
	storedExecution := client.RequestExecution.Query().OnlyX(ctx)
	require.Equal(t, request.StatusFailed, storedRequest.Status)
	require.Equal(t, requestexecution.StatusFailed, storedExecution.Status)
	require.True(t, storedExecution.PassThroughApplied)
	require.NotNil(t, storedExecution.ResponseStatusCode)
	require.Equal(t, http.StatusBadRequest, *storedExecution.ResponseStatusCode)
	require.NotNil(t, storedExecution.MetricsLatencyMs)
	require.Positive(t, *storedExecution.MetricsLatencyMs)
	requestBody, err := requestService.LoadRequestBody(ctx, storedRequest)
	require.NoError(t, err)
	require.JSONEq(t, string(inbound.Body), string(requestBody))
	executionBody, err := requestService.LoadRequestExecutionRequestBody(ctx, storedExecution)
	require.NoError(t, err)
	require.JSONEq(t, string(inbound.Body), string(executor.lastRequest.Body))
	require.JSONEq(t, string(executor.lastRequest.Body), string(executionBody))
	require.Equal(t, errorBody, []byte(storedExecution.ResponseBody))
	require.Equal(t, errorBody, []byte(storedRequest.ResponseBody))
	require.Equal(t, "stored", storedExecution.EvidenceDisposition.ResponseBody.Outcome)
	require.Equal(t, "stored", storedRequest.EvidenceDisposition.ResponseBody.Outcome)
	require.False(t, storedRequest.ContentSaved, "generated-content offload state is independent of diagnostic evidence")
}

func TestChatCompletionOrchestrator_Process_TransformedBadRequestPersistsEvidence(t *testing.T) {
	ctx := authz.WithTestBypass(context.Background())
	client := enttest.NewEntClient(t, "sqlite3", "file:transformed-bad-request-evidence?mode=memory&_fk=0")
	defer client.Close()
	ctx = ent.NewContext(ctx, client)

	project := createTestProject(t, ctx, client)
	channel := createTestChannel(t, ctx, client)
	channel.Settings = &objects.ChannelSettings{
		PassThroughBody:            lo.ToPtr(true),
		DisableRetries:             true,
		StoreExecutionRequestBody:  lo.ToPtr(true),
		StoreExecutionResponseBody: lo.ToPtr(true),
	}
	errorBody := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"provider rejected transformed request"}}`)
	executor := &mockExecutor{err: &httpclient.Error{
		Method:     http.MethodPost,
		URL:        "https://api.anthropic.com/v1/messages",
		StatusCode: http.StatusBadRequest,
		Status:     "400 Bad Request",
		Body:       errorBody,
	}}
	outbound, err := anthropic.NewOutboundTransformer("https://api.anthropic.com", channel.Credentials.APIKey)
	require.NoError(t, err)
	orchestrator, requestService := newFailedResponsePersistenceOrchestrator(t, ctx, client, channel, outbound, executor)

	inbound := buildTestRequest("gpt-4", "transform this request", false)
	_, err = orchestrator.Process(contexts.WithProjectID(ctx, project.ID), inbound)
	require.Error(t, err)

	storedRequest := client.Request.Query().OnlyX(ctx)
	storedExecution := client.RequestExecution.Query().OnlyX(ctx)
	require.False(t, storedExecution.PassThroughApplied)
	requestBody, err := requestService.LoadRequestBody(ctx, storedRequest)
	require.NoError(t, err)
	executionBody, err := requestService.LoadRequestExecutionRequestBody(ctx, storedExecution)
	require.NoError(t, err)
	require.JSONEq(t, string(inbound.Body), string(requestBody))
	require.JSONEq(t, string(executor.lastRequest.Body), string(executionBody))
	require.NotEqual(t, string(requestBody), string(executionBody))
	require.Equal(t, errorBody, []byte(storedExecution.ResponseBody))
	require.Equal(t, errorBody, []byte(storedRequest.ResponseBody))
}

func TestChatCompletionOrchestrator_Process_RetryPreservesFailedAttemptEvidence(t *testing.T) {
	ctx := authz.WithTestBypass(context.Background())
	client := enttest.NewEntClient(t, "sqlite3", "file:retry-failed-response-evidence?mode=memory&_fk=0")
	defer client.Close()
	ctx = ent.NewContext(ctx, client)

	project := createTestProject(t, ctx, client)
	channel := createTestChannel(t, ctx, client)
	channel.Settings = &objects.ChannelSettings{
		StoreExecutionRequestBody:  lo.ToPtr(true),
		StoreExecutionResponseBody: lo.ToPtr(true),
	}
	failedBody := []byte(`{"error":{"message":"first attempt failed","type":"api_error"}}`)
	successBody := buildMockOpenAIResponse("chatcmpl-retry-evidence", "gpt-4", "recovered", 10, 20)
	executor := &sequenceExecutor{steps: []executorStep{
		{err: &httpclient.Error{
			Method:     http.MethodPost,
			URL:        channel.BaseURL + "/chat/completions",
			StatusCode: http.StatusInternalServerError,
			Status:     "500 Internal Server Error",
			Body:       failedBody,
		}},
		{resp: &httpclient.Response{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"application/json"}},
			Body:       successBody,
		}},
	}}
	outbound, err := openai.NewOutboundTransformer(channel.BaseURL, channel.Credentials.APIKey)
	require.NoError(t, err)
	orchestrator, _ := newFailedResponsePersistenceOrchestrator(t, ctx, client, channel, outbound, executor)

	result, err := orchestrator.Process(contexts.WithProjectID(ctx, project.ID), buildTestRequest("gpt-4", "retry", false))
	require.NoError(t, err)
	require.NotNil(t, result.ChatCompletion)

	storedRequest := client.Request.Query().OnlyX(ctx)
	require.Equal(t, request.StatusCompleted, storedRequest.Status)
	require.Contains(t, string(storedRequest.ResponseBody), `"content":"recovered"`)
	executions := client.RequestExecution.Query().Order(ent.Asc(requestexecution.FieldCreatedAt)).AllX(ctx)
	require.Len(t, executions, 2)
	require.Equal(t, requestexecution.StatusFailed, executions[0].Status)
	require.NotNil(t, executions[0].MetricsLatencyMs)
	require.Equal(t, failedBody, []byte(executions[0].ResponseBody))
	require.Equal(t, requestexecution.StatusCompleted, executions[1].Status)
	require.JSONEq(t, string(successBody), string(executions[1].ResponseBody))
}

func TestChatCompletionOrchestrator_Process_FinalFailureUsesLastAttemptEvidence(t *testing.T) {
	ctx := authz.WithTestBypass(context.Background())
	client := enttest.NewEntClient(t, "sqlite3", "file:final-failed-response-evidence?mode=memory&_fk=0")
	defer client.Close()
	ctx = ent.NewContext(ctx, client)

	project := createTestProject(t, ctx, client)
	channel := createTestChannel(t, ctx, client)
	channel.Settings = &objects.ChannelSettings{
		StoreExecutionRequestBody:  lo.ToPtr(true),
		StoreExecutionResponseBody: lo.ToPtr(true),
	}
	firstBody := []byte(`{"error":{"message":"first attempt","type":"api_error"}}`)
	lastBody := []byte(`{"error":{"message":"last attempt","type":"api_error"}}`)
	executor := &sequenceExecutor{steps: []executorStep{
		{err: &httpclient.Error{StatusCode: http.StatusInternalServerError, Status: "500 Internal Server Error", Body: firstBody}},
		{err: &httpclient.Error{StatusCode: http.StatusInternalServerError, Status: "500 Internal Server Error", Body: lastBody}},
	}}
	outbound, err := openai.NewOutboundTransformer(channel.BaseURL, channel.Credentials.APIKey)
	require.NoError(t, err)
	orchestrator, _ := newFailedResponsePersistenceOrchestrator(t, ctx, client, channel, outbound, executor)
	require.NoError(t, orchestrator.SystemService.SetRetryPolicy(ctx, &biz.RetryPolicy{
		Enabled:                 true,
		MaxChannelRetries:       0,
		MaxSingleChannelRetries: 1,
		LoadBalancerStrategy:    biz.LoadBalancerStrategyAdaptive,
	}))

	_, err = orchestrator.Process(contexts.WithProjectID(ctx, project.ID), buildTestRequest("gpt-4", "final failure", false))
	require.Error(t, err)

	storedRequest := client.Request.Query().OnlyX(ctx)
	require.Equal(t, request.StatusFailed, storedRequest.Status)
	require.Equal(t, lastBody, []byte(storedRequest.ResponseBody))
	executions := client.RequestExecution.Query().Order(ent.Asc(requestexecution.FieldCreatedAt)).AllX(ctx)
	require.Len(t, executions, 2)
	require.Equal(t, firstBody, []byte(executions[0].ResponseBody))
	require.Equal(t, lastBody, []byte(executions[1].ResponseBody))
}
