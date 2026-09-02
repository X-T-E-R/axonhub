package pipeline

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/streams"
)

func TestPreReadLlmStream_ResponsesUsageOnlyCompletedIsEmpty(t *testing.T) {
	finishReason := "stop"
	usage := &llm.Usage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3}
	source := streams.SliceStream([]*llm.Response{
		{
			Object:  "chat.completion.chunk",
			Choices: []llm.Choice{{Index: 0, Delta: &llm.Message{}, FinishReason: &finishReason}},
		},
		{Object: "chat.completion.chunk", Choices: []llm.Choice{}, Usage: usage},
		llm.DoneResponse,
	})
	p := &pipeline{emptyResponseDetection: true}

	stream, err := p.preReadLlmStream(context.Background(), source, nil)
	require.Nil(t, stream)
	require.ErrorIs(t, err, ErrEmptyResponse)
	require.Equal(t, int64(1), usage.PromptTokens)
}
