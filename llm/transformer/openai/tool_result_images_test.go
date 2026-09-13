package openai

import (
	"encoding/json"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
)

func TestLiftToolResultImages(t *testing.T) {
	text := MessageContentPart{Type: "text", Text: lo.ToPtr("exact tool text")}
	image := MessageContentPart{Type: "image_url", ImageURL: &ImageURL{URL: "data:image/png;base64,c3ludGhldGlj", Detail: lo.ToPtr("high")}}
	for _, test := range []struct {
		name  string
		parts []MessageContentPart
	}{
		{"image only", []MessageContentPart{image}},
		{"text then image", []MessageContentPart{text, image}},
		{"image then text", []MessageContentPart{image, text}},
		{"interleaved", []MessageContentPart{text, image, text, image}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := []Message{{Role: "tool", ToolCallID: lo.ToPtr("call_image"), Content: MessageContent{MultipleContent: test.parts}}}
			before, err := json.Marshal(source)
			require.NoError(t, err)
			got := liftToolResultImages(source)
			require.Len(t, got, 2)
			require.Equal(t, "tool", got[0].Role)
			require.Equal(t, "call_image", *got[0].ToolCallID)
			require.Equal(t, "user", got[1].Role)
			require.Len(t, got[0].Content.MultipleContent, len(test.parts))
			var imageCount int
			for i, original := range test.parts {
				converted := got[0].Content.MultipleContent[i]
				require.Equal(t, "text", converted.Type)
				if original.Type == "text" {
					require.Equal(t, original, converted)
				} else {
					require.Contains(t, *converted.Text, "call_image")
					imageCount++
				}
			}
			require.Len(t, got[1].Content.MultipleContent, imageCount*2)
			for i := range imageCount {
				require.Contains(t, *got[1].Content.MultipleContent[2*i].Text, "call_image")
				require.Equal(t, image, got[1].Content.MultipleContent[2*i+1])
			}
			after, err := json.Marshal(source)
			require.NoError(t, err)
			require.Equal(t, before, after, "conversion must not mutate the input")
		})
	}
}

func TestLiftToolResultImagesKeepsParallelResultsTogether(t *testing.T) {
	image := func(url string) MessageContent {
		return MessageContent{MultipleContent: []MessageContentPart{{Type: "image_url", ImageURL: &ImageURL{URL: url}}}}
	}
	source := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "a"}, {ID: "b"}}},
		{Role: "tool", ToolCallID: lo.ToPtr("a"), Content: image("https://example.invalid/a.png")},
		{Role: "tool", ToolCallID: lo.ToPtr("b"), Content: image("https://example.invalid/b.png")},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c"}}},
		{Role: "tool", ToolCallID: lo.ToPtr("c"), Content: image("https://example.invalid/c.png")},
		{Role: "user", Content: MessageContent{Content: lo.ToPtr("next")}},
	}
	got := liftToolResultImages(source)
	require.Equal(t, []string{"assistant", "tool", "tool", "user", "assistant", "tool", "user", "user"},
		lo.Map(got, func(m Message, _ int) string { return m.Role }))
	require.Equal(t, "https://example.invalid/a.png", got[3].Content.MultipleContent[1].ImageURL.URL)
	require.Equal(t, "https://example.invalid/b.png", got[3].Content.MultipleContent[3].ImageURL.URL)
	require.Equal(t, "https://example.invalid/c.png", got[6].Content.MultipleContent[1].ImageURL.URL)
	require.Equal(t, source[3].ToolCalls, got[4].ToolCalls)
	require.Equal(t, source[5], got[7])
}

func TestLiftToolResultImagesLeavesTextAndUserImagesUnchanged(t *testing.T) {
	source := []Message{
		{Role: "user", Content: MessageContent{MultipleContent: []MessageContentPart{{Type: "image_url", ImageURL: &ImageURL{URL: "https://example.invalid/user.png"}}}}},
		{Role: "tool", ToolCallID: lo.ToPtr("a"), Content: MessageContent{Content: lo.ToPtr("exact")}},
		{Role: "tool", ToolCallID: lo.ToPtr("b"), Content: MessageContent{MultipleContent: []MessageContentPart{{Type: "text", Text: lo.ToPtr("parts")}}}},
	}
	require.Equal(t, source, liftToolResultImages(source))
}

func TestLiftToolResultImagesRespectsContentArrayPrecedence(t *testing.T) {
	source := []Message{{Role: "tool", ToolCallID: lo.ToPtr("call_image"), Content: MessageContent{
		Content: lo.ToPtr("unused string"),
		MultipleContent: []MessageContentPart{{
			Type: "image_url", ImageURL: &ImageURL{URL: "https://example.invalid/a.png"},
		}},
	}}}
	got := liftToolResultImages(source)
	require.Len(t, got, 2)
	require.Equal(t, "text", got[0].Content.MultipleContent[0].Type)
	require.Equal(t, "https://example.invalid/a.png", got[1].Content.MultipleContent[1].ImageURL.URL)
}

func TestNativeChatKeepsExtendedToolImageShape(t *testing.T) {
	source := &llm.Request{Model: "test", APIFormat: llm.APIFormatOpenAIChatCompletion, Messages: []llm.Message{{
		Role: "tool", ToolCallID: lo.ToPtr("call_image"), Content: llm.MessageContent{MultipleContent: []llm.MessageContentPart{{
			Type: "image_url", ImageURL: &llm.ImageURL{URL: "https://example.invalid/a.png"},
		}}},
	}}}
	out, err := NewOutboundTransformer("https://example.invalid", "synthetic")
	require.NoError(t, err)
	wire, err := out.TransformRequest(t.Context(), source)
	require.NoError(t, err)
	var got Request
	require.NoError(t, json.Unmarshal(wire.Body, &got))
	require.Len(t, got.Messages, 1)
	require.Equal(t, "tool", got.Messages[0].Role)
	require.Equal(t, "image_url", got.Messages[0].Content.MultipleContent[0].Type)
}
