package orchestrator

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/pipeline/stream"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

type failedEvidenceExecutor struct {
	responseStatus int
	events         []*httpclient.StreamEvent
	streamErr      error
	lastRequest    *httpclient.Request
}

func (e *failedEvidenceExecutor) Do(context.Context, *httpclient.Request) (*httpclient.Response, error) {
	return nil, errors.New("non-stream execution is not expected")
}

func (e *failedEvidenceExecutor) DoStream(_ context.Context, request *httpclient.Request) (streams.Stream[*httpclient.StreamEvent], error) {
	e.lastRequest = request
	return &failedEvidenceResponseStream{
		Stream: &errorAfterEventsStream{items: e.events, err: e.streamErr},
		response: &httpclient.Response{
			StatusCode: e.responseStatus,
			Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
			Request:    request,
		},
	}, nil
}

type failedEvidenceResponseStream struct {
	streams.Stream[*httpclient.StreamEvent]
	response *httpclient.Response
}

func (s *failedEvidenceResponseStream) ResponseMetadata() *httpclient.Response {
	return s.response
}

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

func TestChatCompletionOrchestrator_FailedEstablishedStreamPersistsAvailableEvidence(t *testing.T) {
	for _, test := range []struct {
		name       string
		streamErr  error
		events     []*httpclient.StreamEvent
		wantBody   bool
		storeBody  bool
		wantFinish string
	}{
		{
			name:       "read error",
			streamErr:  errors.New("synthetic upstream stream read failure"),
			events:     []*httpclient.StreamEvent{{Data: []byte(`{"id":"chatcmpl-failed-evidence","object":"chat.completion.chunk","created":1,"model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant","content":"partial synthetic output"},"finish_reason":null}]}`)}},
			wantBody:   true,
			storeBody:  true,
			wantFinish: "null",
		},
		{
			name:       "observed finish reason before read error",
			streamErr:  errors.New("synthetic post-terminal stream read failure"),
			events:     []*httpclient.StreamEvent{{Data: []byte(`{"id":"chatcmpl-terminal-evidence","object":"chat.completion.chunk","created":1,"model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant","content":"partial synthetic output"},"finish_reason":"stop"}]}`)}},
			wantBody:   true,
			storeBody:  true,
			wantFinish: `"stop"`,
		},
		{name: "eof without terminal event", storeBody: true},
		{
			name:      "response body disabled",
			streamErr: errors.New("synthetic disabled-evidence stream failure"),
			events:    []*httpclient.StreamEvent{{Data: []byte(`{"id":"chatcmpl-disabled-evidence","object":"chat.completion.chunk","created":1,"model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant","content":"must not persist"},"finish_reason":null}]}`)}},
		},
		{
			name:      "truncated tool arguments",
			streamErr: errors.New("synthetic truncated tool stream failure"),
			events:    []*httpclient.StreamEvent{{Data: []byte(`{"id":"chatcmpl-tool-evidence","object":"chat.completion.chunk","created":1,"model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_partial","type":"function","function":{"name":"synthetic_tool","arguments":"{\"value\":"}}]},"finish_reason":null}]}`)}},
			storeBody: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := authz.WithTestBypass(context.Background())
			client := enttest.NewEntClient(t, "sqlite3", "file:failed-established-stream-evidence?mode=memory&_fk=0")
			defer client.Close()
			ctx = ent.NewContext(ctx, client)

			project := createTestProject(t, ctx, client)
			channel := createTestChannel(t, ctx, client)
			channel.Settings = &objects.ChannelSettings{
				DisableRetries:             true,
				StoreExecutionRequestBody:  lo.ToPtr(true),
				StoreExecutionResponseBody: lo.ToPtr(test.storeBody),
			}
			executor := &failedEvidenceExecutor{
				responseStatus: http.StatusOK,
				events:         test.events,
				streamErr:      test.streamErr,
			}
			outbound, err := openai.NewOutboundTransformer(channel.BaseURL, channel.Credentials.APIKey)
			require.NoError(t, err)
			orchestrator, requestService := newFailedResponsePersistenceOrchestrator(t, ctx, client, channel, outbound, executor)
			writer := biz.NewForwardingObservationWriter(biz.ManagedRequestBodyWriterConfig{})
			requestService = biz.NewRequestServiceWithObservationWriter(
				client,
				orchestrator.SystemService,
				orchestrator.UsageLogService,
				requestService.DataStorageService,
				requestService.LiveStreamRegistry,
				nil,
				writer,
			)
			orchestrator.RequestService = requestService
			require.NoError(t, writer.Start(ctx))
			defer func() {
				stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				require.NoError(t, writer.Stop(stopCtx))
			}()
			require.NoError(t, orchestrator.SystemService.SetStoragePolicy(ctx, &biz.StoragePolicy{
				StoreRequestBody:          true,
				StoreExecutionRequestBody: lo.ToPtr(true),
				StoreResponseBody:         test.storeBody,
				StoreChunks:               false,
			}))

			prompt := "synthetic failed stream evidence"
			if test.wantBody {
				prompt += strings.Repeat("x", (2<<20)+1)
			}
			inbound := buildTestRequest("gpt-4", prompt, true)
			result, processErr := orchestrator.Process(contexts.WithProjectID(ctx, project.ID), inbound)
			if result.ChatCompletionStream != nil {
				for result.ChatCompletionStream.Next() {
					_ = result.ChatCompletionStream.Current()
				}
				if test.streamErr != nil {
					require.ErrorContains(t, result.ChatCompletionStream.Err(), test.streamErr.Error())
				} else {
					require.NoError(t, result.ChatCompletionStream.Err())
				}
				require.NoError(t, result.ChatCompletionStream.Close())
			} else {
				require.Error(t, processErr)
			}
			drainCtx, cancelDrain := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancelDrain()
			require.NoError(t, writer.Wait(drainCtx))

			storedRequest := client.Request.Query().OnlyX(ctx)
			storedExecution := client.RequestExecution.Query().OnlyX(ctx)
			require.Equal(t, request.StatusFailed, storedRequest.Status)
			require.Equal(t, requestexecution.StatusFailed, storedExecution.Status)
			require.NotNil(t, storedExecution.ResponseStatusCode)
			require.Equal(t, http.StatusOK, *storedExecution.ResponseStatusCode)
			require.Contains(t, storedExecution.ErrorMessage, `upstream content-type "text/event-stream"`)
			require.Empty(t, storedRequest.ResponseChunks, "disabled chunk capture must stay disabled")
			require.Empty(t, storedExecution.ResponseChunks, "disabled chunk capture must stay disabled")

			requestBody, err := requestService.LoadRequestBody(ctx, storedRequest)
			require.NoError(t, err)
			require.JSONEq(t, string(inbound.Body), string(requestBody))
			if test.wantBody {
				require.Greater(t, len(requestBody), 2<<20, "forwarding callback must retain bodies above the former admin read limit")
			}
			executionBody, err := requestService.LoadRequestExecutionRequestBody(ctx, storedExecution)
			require.NoError(t, err)
			require.JSONEq(t, string(executor.lastRequest.Body), string(executionBody))
			if test.wantBody {
				require.Contains(t, string(storedRequest.ResponseBody), "partial synthetic output")
				require.Contains(t, string(storedExecution.ResponseBody), "partial synthetic output")
				require.Contains(t, string(storedRequest.ResponseBody), `"finish_reason":`+test.wantFinish)
				require.Contains(t, string(storedExecution.ResponseBody), `"finish_reason":`+test.wantFinish)
				if test.wantFinish == "null" {
					require.NotContains(t, string(storedRequest.ResponseBody), `"finish_reason":"stop"`)
					require.NotContains(t, string(storedExecution.ResponseBody), `"finish_reason":"stop"`)
				}
			} else {
				require.Empty(t, storedRequest.ResponseBody)
				require.Empty(t, storedExecution.ResponseBody)
			}
		})
	}
}

func TestPersistRequestExecution_TransformerErrorUsesCapturedStreamEvidence(t *testing.T) {
	ctx := authz.WithTestBypass(context.Background())
	client := enttest.NewEntClient(t, "sqlite3", "file:transformer-failed-stream-evidence?mode=memory&_fk=0")
	defer client.Close()
	ctx = ent.NewContext(ctx, client)
	project := createTestProject(t, ctx, client)
	channelRow := createTestChannel(t, ctx, client)
	_, requestService, systemService, usageLogService := setupTestServices(t, client)
	require.NoError(t, systemService.SetStoragePolicy(ctx, &biz.StoragePolicy{StoreResponseBody: true}))
	req := client.Request.Create().SetProjectID(project.ID).SetModelID("gpt-4").SetRequestBody([]byte(`{}`)).SetStatus(request.StatusProcessing).SaveX(ctx)
	exec := client.RequestExecution.Create().SetProjectID(project.ID).SetRequestID(req.ID).SetChannelID(channelRow.ID).SetModelID("gpt-4").SetRequestBody([]byte(`{}`)).SetStatus(requestexecution.StatusProcessing).SetStream(true).SaveX(ctx)
	channel := &biz.Channel{Channel: channelRow}
	state := &PersistenceState{
		Request:                req,
		RequestExec:            exec,
		RequestService:         requestService,
		UsageLogService:        usageLogService,
		CurrentCandidate:       &ChannelModelsCandidate{Channel: channel, Models: []biz.ChannelModelEntry{{ActualModel: "gpt-4"}}},
		ProviderStreamResponse: &httpclient.Response{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"application/json"}}},
	}
	transformer := &mockTransformer{apiFormat: llm.APIFormatOpenAIChatCompletion, aggregatedResponse: []byte(`{"id":"partial-transform","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"partial transform output"},"finish_reason":"stop"}]}`)}
	persistent := NewOutboundPersistentStream(ctx, streams.SliceStream([]*httpclient.StreamEvent{{Data: []byte(`{"type":"response.output_text.delta","delta":"partial"}`)}}), req, exec, requestService, usageLogService, transformer, nil, state)
	require.True(t, persistent.Next())
	_ = persistent.Current()

	outbound := &PersistentOutboundTransformer{state: state}
	persistRequestExecution(outbound).OnOutboundRawError(ctx, errors.New("synthetic transformer stream failure"))

	stored := client.RequestExecution.GetX(ctx, exec.ID)
	require.Equal(t, requestexecution.StatusFailed, stored.Status)
	require.Equal(t, http.StatusOK, *stored.ResponseStatusCode)
	require.Contains(t, string(stored.ResponseBody), "partial-transform")
	require.Contains(t, string(stored.ResponseBody), `"finish_reason":null`)
	require.NotContains(t, string(stored.ResponseBody), `"finish_reason":"stop"`)
	require.Contains(t, stored.ErrorMessage, `upstream content-type "application/json"`)
	require.Equal(t, 1, transformer.aggregateCalls)
	state.clearFailedStreamEvidence(persistent)
}

func TestPersistRequestExecution_ResponsesPartialEOFKeepsIncompleteEvidence(t *testing.T) {
	ctx := authz.WithTestBypass(context.Background())
	client := enttest.NewEntClient(t, "sqlite3", "file:responses-partial-eof-evidence?mode=memory&_fk=0")
	defer client.Close()
	ctx = ent.NewContext(ctx, client)
	project := createTestProject(t, ctx, client)
	channelRow := createTestChannel(t, ctx, client)
	_, requestService, systemService, usageLogService := setupTestServices(t, client)
	require.NoError(t, systemService.SetStoragePolicy(ctx, &biz.StoragePolicy{StoreResponseBody: true}))
	req := client.Request.Create().SetProjectID(project.ID).SetModelID("gpt-4").SetRequestBody([]byte(`{}`)).SetStatus(request.StatusProcessing).SaveX(ctx)
	exec := client.RequestExecution.Create().SetProjectID(project.ID).SetRequestID(req.ID).SetChannelID(channelRow.ID).SetModelID("gpt-4").SetRequestBody([]byte(`{}`)).SetStatus(requestexecution.StatusProcessing).SetStream(true).SaveX(ctx)
	channel := &biz.Channel{Channel: channelRow}
	state := &PersistenceState{
		Request:                req,
		RequestExec:            exec,
		RequestService:         requestService,
		UsageLogService:        usageLogService,
		CurrentCandidate:       &ChannelModelsCandidate{Channel: channel, Models: []biz.ChannelModelEntry{{ActualModel: "gpt-4"}}},
		ProviderStreamResponse: &httpclient.Response{StatusCode: http.StatusOK, Headers: http.Header{"Content-Type": []string{"text/event-stream"}}},
	}
	provider, err := responses.NewOutboundTransformer(channelRow.BaseURL, channelRow.Credentials.APIKey)
	require.NoError(t, err)
	raw := streams.SliceStream([]*httpclient.StreamEvent{{
		Type: "response.created",
		Data: []byte(`{"type":"response.created","response":{"id":"resp_partial_eof","object":"response","created_at":1,"model":"gpt-4","status":"in_progress","output":[]}}`),
	}})
	persistent := NewOutboundPersistentStream(ctx, raw, req, exec, requestService, usageLogService, provider, nil, state)
	transformed, err := provider.TransformStream(ctx, &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIResponse)}, persistent)
	require.NoError(t, err)
	for transformed.Next() {
		_ = transformed.Current()
	}
	streamErr := transformed.Err()
	require.Error(t, streamErr)

	outbound := &PersistentOutboundTransformer{state: state}
	persistRequestExecution(outbound).OnOutboundRawError(ctx, streamErr)

	stored := client.RequestExecution.GetX(ctx, exec.ID)
	require.Equal(t, requestexecution.StatusFailed, stored.Status)
	require.Equal(t, http.StatusOK, *stored.ResponseStatusCode)
	require.Contains(t, string(stored.ResponseBody), `"id":"resp_partial_eof"`)
	require.Contains(t, string(stored.ResponseBody), `"status":"in_progress"`)
	require.NotContains(t, string(stored.ResponseBody), `"status":"completed"`)
	state.clearFailedStreamEvidence(persistent)
}

func TestIncompleteStreamEvidenceBodyPreservesOnlyObservedTerminalFields(t *testing.T) {
	for _, reason := range []string{"length", "content_filter", "error"} {
		t.Run("chat observed "+reason, func(t *testing.T) {
			body := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"partial"},"finish_reason":"stop"}]}`)
			chunks := []*httpclient.StreamEvent{{Data: []byte(`{"choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":"` + reason + `"}]}`)}}
			got, err := incompleteStreamEvidenceBody(body, llm.APIFormatOpenAIChatCompletion, chunks)
			require.NoError(t, err)
			require.Equal(t, reason, gjson.GetBytes(got, "choices.0.finish_reason").String())
		})
	}

	t.Run("chat mixed choices", func(t *testing.T) {
		body := []byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"},{"index":1,"message":{"role":"assistant","content":"partial"},"finish_reason":"stop"}]}`)
		chunks := []*httpclient.StreamEvent{{Data: []byte(`{"choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"},{"index":1,"delta":{"content":"partial"},"finish_reason":null}]}`)}}
		got, err := incompleteStreamEvidenceBody(body, llm.APIFormatOpenAIChatCompletion, chunks)
		require.NoError(t, err)
		require.Equal(t, "stop", gjson.GetBytes(got, "choices.0.finish_reason").String())
		require.Equal(t, gjson.Null, gjson.GetBytes(got, "choices.1.finish_reason").Type)
	})

	t.Run("anthropic stop before message_stop", func(t *testing.T) {
		body := []byte(`{"id":"msg_partial","type":"message","content":[{"type":"text","text":"partial"}],"stop_reason":"end_turn","stop_sequence":null}`)
		chunks := []*httpclient.StreamEvent{{
			Type: "message_delta",
			Data: []byte(`{"type":"message_delta","delta":{"stop_reason":"max_tokens","stop_sequence":"observed-sequence"}}`),
		}}
		got, err := incompleteStreamEvidenceBody(body, llm.APIFormatAnthropicMessage, chunks)
		require.NoError(t, err)
		require.Equal(t, "max_tokens", gjson.GetBytes(got, "stop_reason").String())
		require.Equal(t, "observed-sequence", gjson.GetBytes(got, "stop_sequence").String())
	})
}
