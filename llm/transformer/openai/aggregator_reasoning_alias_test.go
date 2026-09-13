package openai

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
)

func TestAggregateStreamChunks_ReasoningAlias(t *testing.T) {
	for _, test := range []struct {
		name  string
		delta string
		want  *string
	}{
		{name: "alias", delta: `{"reasoning":"alias"}`, want: lo.ToPtr("alias")},
		{name: "empty alias", delta: `{"reasoning":""}`, want: lo.ToPtr("")},
		{name: "canonical wins", delta: `{"reasoning_content":"canonical","reasoning":"alias"}`, want: lo.ToPtr("canonical")},
		{name: "empty canonical wins", delta: `{"reasoning_content":"","reasoning":"alias"}`, want: lo.ToPtr("")},
		{name: "null canonical", delta: `{"reasoning_content":null,"reasoning":"alias"}`, want: lo.ToPtr("alias")},
		{name: "absent", delta: `{"content":"answer"}`},
		{name: "null alias", delta: `{"reasoning":null}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, eventType := range []string{"", httpclient.JSONStreamEventType} {
				t.Run("event="+eventType, func(t *testing.T) {
					field := "delta"
					if eventType == httpclient.JSONStreamEventType {
						field = "message"
					}
					chunks := []*httpclient.StreamEvent{{
						Type: eventType,
						Data: []byte(`{"id":"synthetic","choices":[{"index":0,"` + field + `":` + test.delta + `,"finish_reason":"stop"}]}`),
					}}
					data, _, err := AggregateStreamChunks(context.Background(), chunks, DefaultTransformChunk)
					require.NoError(t, err)
					var response llm.Response
					require.NoError(t, json.Unmarshal(data, &response))
					require.Len(t, response.Choices, 1)
					require.Equal(t, test.want, response.Choices[0].Message.ReasoningContent)
				})
			}
		})
	}
}

func TestAggregateStreamChunks_ReasoningAliasChoiceIsolation(t *testing.T) {
	chunks := []*httpclient.StreamEvent{
		{Data: []byte(`{"choices":[{"index":1,"delta":{"reasoning":"other"}},{"index":0,"delta":{"reasoning":"first "}}]}`)},
		{Data: []byte(`{"choices":[{"index":0,"delta":{"reasoning_content":"second","reasoning":"second"}},{"index":1,"delta":{"content":"answer"},"finish_reason":"stop"}]}`)},
		{Data: []byte(`{"error":{"code":"synthetic_stream_failure","message":"synthetic upstream failure"},"choices":[{"index":0,"delta":{},"finish_reason":"error"}]}`)},
	}
	data, _, err := AggregateStreamChunks(context.Background(), chunks, DefaultTransformChunk)
	require.NoError(t, err)
	var response llm.Response
	require.NoError(t, json.Unmarshal(data, &response))
	require.Len(t, response.Choices, 2)
	require.Equal(t, "first second", *response.Choices[0].Message.ReasoningContent)
	require.Equal(t, "error", *response.Choices[0].FinishReason)
	require.Equal(t, "other", *response.Choices[1].Message.ReasoningContent)
	require.Equal(t, "answer", *response.Choices[1].Message.Content.Content)
	require.Equal(t, "stop", *response.Choices[1].FinishReason)
}
