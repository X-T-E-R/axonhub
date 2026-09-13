package orchestrator

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
)

func TestResponsesToolImageChatPipelinePersistsActualWire(t *testing.T) {
	ctx, client := setupTest(t)
	project := createTestProject(t, ctx, client)
	channel := createTestChannel(t, ctx, client)
	channel.Settings = &objects.ChannelSettings{PassThroughBody: lo.ToPtr(true)}
	executor := &mockExecutor{response: &httpclient.Response{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       buildMockOpenAIResponse("chat_image", "gpt-4", "image accepted", 1, 1),
	}}
	out, err := openai.NewOutboundTransformer(channel.BaseURL, channel.Credentials.APIKey)
	require.NoError(t, err)
	orch, requestService := newFailedResponsePersistenceOrchestrator(t, ctx, client, channel, out, executor)
	orch.Inbound = responses.NewInboundTransformer()
	body := []byte(`{"model":"gpt-4","input":[
		{"type":"function_call","call_id":"call_image","name":"inspect","arguments":"{}"},
		{"type":"function_call_output","call_id":"call_image","output":[
			{"type":"input_image","image_url":"https://example.invalid/image.png"}]}
	],"tools":[{"type":"function","name":"inspect","parameters":{"type":"object"}}]}`)
	request := &httpclient.Request{Body: body, Headers: http.Header{"Content-Type": []string{"application/json"}}}
	result, err := orch.Process(contexts.WithProjectID(ctx, project.ID), request)
	require.NoError(t, err)
	require.Contains(t, string(result.ChatCompletion.Body), "image accepted")
	var provider openai.Request
	require.NoError(t, json.Unmarshal(executor.lastRequest.Body, &provider))
	require.Len(t, provider.Messages, 3)
	require.Equal(t, "tool", provider.Messages[1].Role)
	require.NotNil(t, provider.Messages[1].Content.Content)
	require.Contains(t, *provider.Messages[1].Content.Content, "attached below")
	require.Equal(t, "user", provider.Messages[2].Role)
	require.Equal(t, "https://example.invalid/image.png", provider.Messages[2].Content.MultipleContent[1].ImageURL.URL)
	execution := client.RequestExecution.Query().OnlyX(ctx)
	require.False(t, execution.PassThroughApplied, "cross-format requests must use the transformed provider body")
	stored, err := requestService.LoadRequestExecutionRequestBody(ctx, execution)
	require.NoError(t, err)
	require.JSONEq(t, string(executor.lastRequest.Body), string(stored))
	require.Equal(t, body, request.Body)
}
