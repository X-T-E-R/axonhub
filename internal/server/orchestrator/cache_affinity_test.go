package orchestrator

import (
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
)

func TestDeriveChannelKeyCacheAffinityID_UsesPromptCacheKeyBeforeRequestBody(t *testing.T) {
	first, err := deriveChannelKeyCacheAffinityID(&llm.Request{
		Model:          "gpt-4",
		PromptCacheKey: lo.ToPtr("shared-cache-key"),
		Messages: []llm.Message{
			{Role: "system"},
			{Role: "user"},
		},
	})
	require.NoError(t, err)

	second, err := deriveChannelKeyCacheAffinityID(&llm.Request{
		Model:          "gpt-4",
		PromptCacheKey: lo.ToPtr("shared-cache-key"),
		Messages: []llm.Message{
			{Role: "system"},
			{Role: "assistant"},
			{Role: "user"},
		},
	})
	require.NoError(t, err)

	third, err := deriveChannelKeyCacheAffinityID(&llm.Request{
		Model:          "gpt-4",
		PromptCacheKey: lo.ToPtr("other-cache-key"),
		Messages: []llm.Message{
			{Role: "system"},
			{Role: "user"},
		},
	})
	require.NoError(t, err)

	require.NotEmpty(t, first)
	require.Equal(t, first, second)
	require.NotEqual(t, first, third)
}

func TestDeriveChannelKeyCacheAffinityID_HashesCacheRelevantRequestPrefix(t *testing.T) {
	first, err := deriveChannelKeyCacheAffinityID(&llm.Request{
		Model: "gpt-4",
		Messages: []llm.Message{
			{Role: "system"},
			{Role: "user"},
		},
	})
	require.NoError(t, err)

	second, err := deriveChannelKeyCacheAffinityID(&llm.Request{
		Model: "gpt-4",
		Messages: []llm.Message{
			{Role: "system"},
			{Role: "user", Name: lo.ToPtr("different-final-user")},
		},
	})
	require.NoError(t, err)

	third, err := deriveChannelKeyCacheAffinityID(&llm.Request{
		Model: "gpt-4",
		Messages: []llm.Message{
			{Role: "developer"},
			{Role: "user"},
		},
	})
	require.NoError(t, err)

	require.NotEmpty(t, first)
	require.Equal(t, first, second)
	require.NotEqual(t, first, third)
}

func TestDeriveChannelKeyCacheAffinityID_UsesCompactPromptCacheKeyBeforeCompactBody(t *testing.T) {
	first, err := deriveChannelKeyCacheAffinityID(&llm.Request{
		Model: "gpt-4",
		Compact: &llm.CompactRequest{
			PromptCacheKey: "compact-cache-key",
			Instructions:   "first instructions",
			Input: []llm.Message{
				{Role: "system"},
				{Role: "user"},
			},
		},
	})
	require.NoError(t, err)

	second, err := deriveChannelKeyCacheAffinityID(&llm.Request{
		Model: "gpt-4",
		Compact: &llm.CompactRequest{
			PromptCacheKey: "compact-cache-key",
			Instructions:   "second instructions",
			Input: []llm.Message{
				{Role: "developer"},
				{Role: "user"},
			},
		},
	})
	require.NoError(t, err)

	require.NotEmpty(t, first)
	require.Equal(t, first, second)
}
