package orchestrator

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
)

func TestSelectAPIFormat(t *testing.T) {
	endpoints := []objects.ChannelEndpoint{
		{APIFormat: "openai/responses"},
		{APIFormat: "openai/embeddings"},
		{APIFormat: "openai/image_generation"},
		{APIFormat: "openai/moderations"},
	}

	require.Equal(t, "openai/responses", SelectAPIFormat(endpoints, &llm.Request{RequestType: llm.RequestTypeChat}))
	require.Equal(t, "openai/embeddings", SelectAPIFormat(endpoints, &llm.Request{RequestType: llm.RequestTypeEmbedding}))
	require.Equal(t, "openai/image_generation", SelectAPIFormat(endpoints, &llm.Request{RequestType: llm.RequestTypeImage}))
	require.Equal(t, "openai/moderations", SelectAPIFormat(endpoints, &llm.Request{RequestType: llm.RequestTypeModeration}))
	require.Empty(t, SelectAPIFormat(
		[]objects.ChannelEndpoint{{APIFormat: llm.APIFormatAnthropicMessage.String()}},
		&llm.Request{RequestType: llm.RequestTypeModeration},
	))
	require.Empty(t, SelectAPIFormat(
		[]objects.ChannelEndpoint{{APIFormat: llm.APIFormatOpenAIChatCompletion.String()}},
		&llm.Request{RequestType: llm.RequestTypeModeration},
	))

	geminiEndpoints := []objects.ChannelEndpoint{
		{APIFormat: llm.APIFormatGeminiContents.String()},
		{APIFormat: llm.APIFormatGeminiEmbedding.String()},
	}

	require.Equal(t, llm.APIFormatGeminiContents.String(), SelectAPIFormat(geminiEndpoints, &llm.Request{RequestType: llm.RequestTypeChat}))
	require.Equal(t, llm.APIFormatGeminiEmbedding.String(), SelectAPIFormat(geminiEndpoints, &llm.Request{RequestType: llm.RequestTypeEmbedding}))
	require.Equal(t, llm.APIFormatGeminiContents.String(), SelectAPIFormat(geminiEndpoints, &llm.Request{RequestType: llm.RequestTypeImage}))
}

func TestSelectAPIFormat_PrefersMatchingFormat(t *testing.T) {
	endpoints := []objects.ChannelEndpoint{
		{APIFormat: "openai/responses"},
		{APIFormat: "openai/chat_completions"},
	}

	require.Equal(t, "openai/chat_completions", SelectAPIFormat(endpoints, &llm.Request{
		RequestType: llm.RequestTypeChat,
		APIFormat:   llm.APIFormatOpenAIChatCompletion,
	}))
}

func TestSelectAPIFormat_FallsBackWhenNoMatch(t *testing.T) {
	endpoints := []objects.ChannelEndpoint{
		{APIFormat: "openai/responses"},
	}

	require.Equal(t, "openai/responses", SelectAPIFormat(endpoints, &llm.Request{
		RequestType: llm.RequestTypeChat,
		APIFormat:   llm.APIFormatOpenAIChatCompletion,
	}))
}

func TestSelectAPIFormat_Video(t *testing.T) {
	endpoints := []objects.ChannelEndpoint{
		{APIFormat: "openai/video"},
		{APIFormat: "seedance/video"},
	}

	require.Equal(t, "openai/video", SelectAPIFormat(endpoints, &llm.Request{
		RequestType: llm.RequestTypeVideo,
		APIFormat:   llm.APIFormatOpenAIVideo,
	}))

	require.Equal(t, "seedance/video", SelectAPIFormat(endpoints, &llm.Request{
		RequestType: llm.RequestTypeVideo,
		APIFormat:   llm.APIFormatSeedanceVideo,
	}))
}

func TestSelectAPIFormat_Compact(t *testing.T) {
	endpoints := []objects.ChannelEndpoint{
		{APIFormat: "openai/responses"},
		{APIFormat: "openai/responses_compact"},
	}

	require.Equal(t, "openai/responses_compact", SelectAPIFormat(endpoints, &llm.Request{
		RequestType: llm.RequestTypeCompact,
		APIFormat:   llm.APIFormatOpenAIResponseCompact,
	}))
}

func TestSelectAPIFormatNativeDecisionIsolation(t *testing.T) {
	native := []objects.ChannelEndpoint{{APIFormat: "typesafe/systemone"}, {APIFormat: "commandcode/systemone"}, {APIFormat: "openai/decisions"}}
	for _, format := range []llm.APIFormat{llm.APIFormatTypeSafeSystemOne, llm.APIFormatCommandCodeSystemOne, llm.APIFormatOpenAIDecisions} {
		require.Equal(t, format.String(), SelectAPIFormat(native, &llm.Request{RequestType: llm.RequestTypeDecision, APIFormat: format}))
	}
	require.Equal(t, "commandcode/systemone", SelectAPIFormat(native[1:2], &llm.Request{RequestType: llm.RequestTypeDecision, APIFormat: llm.APIFormatTypeSafeSystemOne}))
	require.Empty(t, SelectAPIFormat(native[:1], &llm.Request{RequestType: llm.RequestTypeDecision, APIFormat: llm.APIFormatOpenAIDecisions}))
	require.Empty(t, SelectAPIFormat(native, &llm.Request{RequestType: llm.RequestTypeChat}))
	require.Empty(t, SelectAPIFormat([]objects.ChannelEndpoint{{APIFormat: "openai/chat_completions"}}, &llm.Request{RequestType: llm.RequestTypeDecision, APIFormat: llm.APIFormatTypeSafeSystemOne}))
	mixed := append(append([]objects.ChannelEndpoint{}, native...), objects.ChannelEndpoint{APIFormat: "openai/chat_completions"})
	require.Equal(t, "openai/chat_completions", SelectAPIFormat(mixed, &llm.Request{RequestType: llm.RequestTypeChat}))
}
