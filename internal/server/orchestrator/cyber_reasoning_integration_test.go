package orchestrator

import (
	"net/http"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai"
)

func TestCyberSessionBlockWithReasoningBoundsAndPassThrough(t *testing.T) {
	ctx, client := setupTest(t)
	project := createTestProject(t, ctx, client)
	apiKey := client.APIKey.Create().SetName("combined-controls").SetKey("ah-combined-test").SetProjectID(project.ID).SaveX(ctx)
	channel := createTestChannel(t, ctx, client)
	channel.Settings = &objects.ChannelSettings{
		PassThroughBody: lo.ToPtr(true),
		BodyOverrideOperations: []objects.OverrideOperation{
			{Op: objects.OverrideOpSet, Path: "reasoning_effort", Value: "ultra"},
		},
	}
	executor := &mockExecutor{err: &httpclient.Error{
		StatusCode: http.StatusForbidden,
		Body:       []byte(`{"error":{"code":"cyber_policy","message":"synthetic refusal"}}`),
	}}
	outbound, err := openai.NewOutboundTransformer(channel.BaseURL, channel.Credentials.APIKey)
	require.NoError(t, err)
	orch, requestService := newFailedResponsePersistenceOrchestrator(t, ctx, client, channel, outbound, executor)
	selector := orch.channelSelector.(*staticChannelSelector)
	selector.candidates[0].ModelSettings = &objects.ModelSettings{MaxReasoningEffort: "low"}
	require.NoError(t, orch.SystemService.SetSecuritySettings(ctx, biz.SecuritySettings{
		BlockedIPs:                  []string{},
		CyberSessionBlockEnabled:    true,
		CyberSessionBlockTTLSeconds: 60,
	}))
	requestCtx := contexts.WithAPIKey(contexts.WithProjectID(ctx, project.ID), apiKey)
	first := buildTestRequest("gpt-4", "synthetic request", false)
	first.Headers.Set("Session-Id", "combined-session")
	original := append([]byte(nil), first.Body...)
	_, err = orch.Process(requestCtx, first)
	require.Error(t, err)
	require.EqualValues(t, 1, executor.requestCalls.Load())
	require.Equal(t, "low", gjson.GetBytes(executor.lastRequest.Body, "reasoning_effort").String())
	require.Equal(t, original, first.Body)
	execution := client.RequestExecution.Query().OnlyX(ctx)
	saved, err := requestService.LoadRequestExecutionRequestBody(ctx, execution)
	require.NoError(t, err)
	require.JSONEq(t, string(executor.lastRequest.Body), string(saved))
	_, mark, ok := cyberSessionCacheKeys(&PersistenceState{APIKey: apiKey}, &llm.Request{RawRequest: first})
	require.True(t, ok)
	requireCyberSessionBlocked(t, orch.SystemService, mark)

	second := buildTestRequest("gpt-4", "next turn", false)
	second.Headers.Set("Session-Id", "combined-session")
	_, err = orch.Process(requestCtx, second)
	var localError *llm.ResponseError
	require.ErrorAs(t, err, &localError)
	require.Equal(t, cyberSessionBlockedErrorCode, localError.Detail.Code)
	require.EqualValues(t, 1, executor.requestCalls.Load())
	require.Equal(t, 2, client.Request.Query().CountX(ctx))
	require.Equal(t, 1, client.RequestExecution.Query().CountX(ctx))

	executor.err = nil
	executor.response = &httpclient.Response{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       buildMockOpenAIResponse("chatcmpl-combined", "gpt-4", "ok", 1, 1),
	}
	other := buildTestRequest("gpt-4", "independent conversation", false)
	other.Headers.Set("Session-Id", "other-session")
	result, err := orch.Process(requestCtx, other)
	require.NoError(t, err)
	require.NotNil(t, result.ChatCompletion)
	require.EqualValues(t, 2, executor.requestCalls.Load())
	require.Equal(t, "low", gjson.GetBytes(executor.lastRequest.Body, "reasoning_effort").String())
	require.Equal(t, 2, client.RequestExecution.Query().CountX(ctx))
}
