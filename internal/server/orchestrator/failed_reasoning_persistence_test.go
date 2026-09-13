package orchestrator

import (
	"context"
	"net/http"
	"testing"

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
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func TestChatCompletionOrchestrator_FailedReasoningAliasPersistsEvidence(t *testing.T) {
	ctx := authz.WithTestBypass(context.Background())
	client := enttest.NewEntClient(t, "sqlite3", "file:failed-reasoning-alias?mode=memory&_fk=0")
	defer client.Close()
	ctx = ent.NewContext(ctx, client)
	project := createTestProject(t, ctx, client)
	channel := createTestChannel(t, ctx, client)
	channel.Settings = &objects.ChannelSettings{
		DisableRetries:             true,
		StoreExecutionResponseBody: lo.ToPtr(true),
	}
	executor := &failedEvidenceExecutor{
		responseStatus: http.StatusOK,
		events: []*httpclient.StreamEvent{
			{Data: []byte(`{"id":"synthetic","model":"gpt-4","choices":[{"index":0,"delta":{"role":"assistant","reasoning":"partial "}}]}`)},
			{Data: []byte(`{"id":"synthetic","model":"gpt-4","choices":[{"index":0,"delta":{"reasoning":"reasoning"}}]}`)},
			{Data: []byte(`{"error":{"code":"synthetic_stream_failure","message":"synthetic upstream failure"},"choices":[{"index":0,"delta":{},"finish_reason":"error"}]}`)},
		},
	}
	outbound, err := openai.NewOutboundTransformer(channel.BaseURL, channel.Credentials.APIKey)
	require.NoError(t, err)
	orchestrator, _ := newFailedResponsePersistenceOrchestrator(t, ctx, client, channel, outbound, executor)
	orchestrator.Inbound = responses.NewInboundTransformer()
	inbound := &httpclient.Request{
		Method:  http.MethodPost,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    []byte(`{"model":"gpt-4","input":"synthetic reasoning failure","stream":true}`),
	}
	result, err := orchestrator.Process(contexts.WithProjectID(ctx, project.ID), inbound)
	require.NoError(t, err)
	require.NotNil(t, result.ChatCompletionStream)
	var reasoning, failureCode string
	for result.ChatCompletionStream.Next() {
		data := result.ChatCompletionStream.Current().Data
		switch gjson.GetBytes(data, "type").String() {
		case "response.reasoning_summary_text.delta":
			reasoning += gjson.GetBytes(data, "delta").String()
		case "response.failed":
			failureCode = gjson.GetBytes(data, "response.error.code").String()
		case "response.completed":
			t.Fatal("upstream failure must not become a successful completion")
		}
	}
	require.NoError(t, result.ChatCompletionStream.Close())
	require.Equal(t, "partial reasoning", reasoning)
	require.Equal(t, "synthetic_stream_failure", failureCode)

	storedRequest := client.Request.Query().OnlyX(ctx)
	storedExecution := client.RequestExecution.Query().OnlyX(ctx)
	require.Equal(t, request.StatusFailed, storedRequest.Status)
	require.Equal(t, requestexecution.StatusFailed, storedExecution.Status)
	require.Equal(t, http.StatusOK, *storedExecution.ResponseStatusCode)
	require.Equal(t, "partial reasoning", gjson.GetBytes(storedExecution.ResponseBody, "choices.0.message.reasoning_content").String())
	require.Equal(t, "error", gjson.GetBytes(storedExecution.ResponseBody, "choices.0.finish_reason").String())
	require.Contains(t, storedExecution.ErrorMessage, "synthetic upstream failure")
	require.Empty(t, storedExecution.ResponseChunks, "body aggregation must not enable raw chunk storage")
	require.Empty(t, storedRequest.ResponseChunks)
}
