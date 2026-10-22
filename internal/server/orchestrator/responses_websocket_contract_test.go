package orchestrator

import (
	"context"
	"net/http"
	"testing"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
	"github.com/looplj/axonhub/llm/transformer/shared"
	"github.com/stretchr/testify/require"
)

func TestResponsesWebSocketRejectsUnprojectedPromptMaskWithoutChangingHTTP(t *testing.T) {
	request := &httpclient.Request{Headers: http.Header{"Content-Type": []string{"application/json"}}, Body: []byte(`{"model":"m","input":[{"role":"user","content":"secret"}]}`)}
	canonical, err := responses.NewInboundTransformer().TransformRequest(context.Background(), request)
	require.NoError(t, err)
	canonical.RawRequest = request
	masked := "[MASKED]"
	canonical.Messages[0].Content.Content = &masked
	outbound := &PersistentOutboundTransformer{state: &PersistenceState{LlmRequest: canonical}}
	mw := requireNativeResponsesWebSocketFormat(outbound)
	wire := &httpclient.Request{APIFormat: string(llm.APIFormatOpenAIResponse)}
	_, err = mw.OnOutboundRawRequest(shared.WithNativeResponsesWebSocket(context.Background()), wire)
	var responseErr *llm.ResponseError
	require.ErrorAs(t, err, &responseErr)
	require.Equal(t, "unsupported_prompt_policy", responseErr.Detail.Code)
	result, err := mw.OnOutboundRawRequest(context.Background(), wire)
	require.NoError(t, err)
	require.Same(t, wire, result)
}
