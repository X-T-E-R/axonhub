package orchestrator

import (
	"strings"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
)

func TestDeriveChannelKeyCacheAffinityID_UsesPromptCacheKeyAsExactAffinity(t *testing.T) {
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
	require.True(t, strings.HasPrefix(first, "cache:exact:"))
	require.Equal(t, first, second)
	require.NotEqual(t, first, third)
}

func TestDeriveChannelKeyCacheAffinityID_HashesLongCacheRelevantRequestPrefixAsLikely(t *testing.T) {
	stablePrefix := strings.Repeat("stable provider-cacheable instructions. ", 140)

	first, err := deriveChannelKeyCacheAffinityID(&llm.Request{
		Model: "gpt-4",
		Messages: []llm.Message{
			textMessage("system", stablePrefix),
			textMessage("user", "dynamic request one"),
		},
	})
	require.NoError(t, err)

	second, err := deriveChannelKeyCacheAffinityID(&llm.Request{
		Model: "gpt-4",
		Messages: []llm.Message{
			textMessage("system", stablePrefix),
			textMessage("user", "dynamic request two"),
		},
	})
	require.NoError(t, err)

	third, err := deriveChannelKeyCacheAffinityID(&llm.Request{
		Model: "gpt-4",
		Messages: []llm.Message{
			textMessage("developer", stablePrefix),
			textMessage("user", "dynamic request one"),
		},
	})
	require.NoError(t, err)

	require.NotEmpty(t, first)
	require.True(t, strings.HasPrefix(first, "cache:likely:"))
	require.Equal(t, first, second)
	require.NotEqual(t, first, third)
}

func TestDeriveChannelKeyCacheAffinityID_ShortSharedPrefixHasNoAffinity(t *testing.T) {
	first, err := deriveChannelKeyCacheAffinityID(&llm.Request{
		Model: "gpt-4",
		Messages: []llm.Message{
			textMessage("system", "You are helpful."),
			textMessage("user", "What is the weather?"),
		},
	})
	require.NoError(t, err)

	second, err := deriveChannelKeyCacheAffinityID(&llm.Request{
		Model: "gpt-4",
		Messages: []llm.Message{
			textMessage("system", "You are helpful."),
			textMessage("user", "Write a poem."),
		},
	})
	require.NoError(t, err)

	require.Empty(t, first)
	require.Empty(t, second)
}

func TestDeriveChannelKeyCacheAffinityID_SingleDynamicUserMessageHasNoAffinity(t *testing.T) {
	affinityID, err := deriveChannelKeyCacheAffinityID(&llm.Request{
		Model: "gpt-4",
		Messages: []llm.Message{
			textMessage("user", strings.Repeat("dynamic request body. ", 300)),
		},
	})
	require.NoError(t, err)

	require.Empty(t, affinityID)
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
	require.True(t, strings.HasPrefix(first, "cache:exact:"))
	require.Equal(t, first, second)
}

func TestDeriveChannelKeyCacheAffinityID_PreviousResponseIDCreatesExactAffinity(t *testing.T) {
	first, err := deriveChannelKeyCacheAffinityID(&llm.Request{
		Model:              "gpt-4",
		PreviousResponseID: lo.ToPtr("resp_shared"),
		Messages: []llm.Message{
			textMessage("user", "short dynamic follow-up"),
		},
	})
	require.NoError(t, err)

	second, err := deriveChannelKeyCacheAffinityID(&llm.Request{
		Model:              "gpt-4",
		PreviousResponseID: lo.ToPtr("resp_shared"),
		Messages: []llm.Message{
			textMessage("user", "different short dynamic follow-up"),
		},
	})
	require.NoError(t, err)

	require.NotEmpty(t, first)
	require.True(t, strings.HasPrefix(first, "cache:exact:"))
	require.Equal(t, first, second)
}

func textMessage(role string, content string) llm.Message {
	return llm.Message{
		Role: role,
		Content: llm.MessageContent{
			Content: lo.ToPtr(content),
		},
	}
}
