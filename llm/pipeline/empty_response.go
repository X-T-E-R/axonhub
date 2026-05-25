package pipeline

import (
	"errors"
	"strings"

	"github.com/looplj/axonhub/llm"
)

// ErrEmptyResponse indicates the response contains no meaningful content.
// This error triggers channel retry when empty response detection is enabled.
var ErrEmptyResponse = errors.New("empty response detected")

// ErrEmptyStreamChunks indicates an auto-upgraded streaming request produced no inbound chunks.
var ErrEmptyStreamChunks = errors.New("empty stream chunks")

// ErrEmptyAggregatedBody indicates inbound chunk aggregation produced an empty body.
var ErrEmptyAggregatedBody = errors.New("empty aggregated body")

func hasMessageContent(msg *llm.Message) bool {
	return hasMessageContentWithPatterns(msg, nil)
}

func hasMessageContentWithPatterns(msg *llm.Message, emptyTextPatterns []string) bool {
	if msg == nil {
		return false
	}

	matcher := newEmptyResponseTextMatcher(emptyTextPatterns)

	if messageTextHasContent(msg.Content.Content, matcher) {
		return true
	}

	if multipleContentHasContent(msg.Content.MultipleContent, matcher) {
		return true
	}

	if len(msg.ToolCalls) > 0 {
		return true
	}

	if msg.ReasoningContent != nil && *msg.ReasoningContent != "" {
		return true
	}

	if msg.Reasoning != nil && *msg.Reasoning != "" {
		return true
	}

	if messageTextHasContent(&msg.Refusal, matcher) {
		return true
	}

	if msg.Audio != nil {
		return true
	}

	return false
}

// hasResponseContent checks if an llm.Response contains meaningful content.
func hasResponseContent(resp *llm.Response) bool {
	return hasResponseContentWithPatterns(resp, nil)
}

func hasResponseContentWithPatterns(resp *llm.Response, emptyTextPatterns []string) bool {
	if resp == nil || resp == llm.DoneResponse {
		return false
	}

	matcher := newEmptyResponseTextMatcher(emptyTextPatterns)

	if resp.Embedding != nil && len(resp.Embedding.Data) > 0 {
		return true
	}

	if resp.Rerank != nil && len(resp.Rerank.Results) > 0 {
		return true
	}

	if resp.Image != nil && len(resp.Image.Data) > 0 {
		return true
	}

	if resp.Video != nil &&
		(resp.Video.ID != "" || resp.Video.Status != "" || resp.Video.VideoURL != "" || resp.Video.Error != nil) {
		return true
	}

	if resp.Compact != nil && len(resp.Compact.Output) > 0 {
		return true
	}

	if resp.Completion != nil {
		for _, choice := range resp.Completion.Choices {
			if messageTextHasContent(&choice.Text, matcher) {
				return true
			}
		}
	}

	for _, choice := range resp.Choices {
		if hasMessageContentWithPatterns(choice.Delta, emptyTextPatterns) ||
			hasMessageContentWithPatterns(choice.Message, emptyTextPatterns) {
			return true
		}
	}

	return false
}

type emptyResponseTextMatcher struct {
	patterns map[string]struct{}
}

func newEmptyResponseTextMatcher(patterns []string) emptyResponseTextMatcher {
	matcher := emptyResponseTextMatcher{}
	for _, pattern := range patterns {
		normalized := normalizeEmptyResponseText(pattern)
		if normalized == "" {
			continue
		}

		if matcher.patterns == nil {
			matcher.patterns = make(map[string]struct{}, len(patterns))
		}

		matcher.patterns[normalized] = struct{}{}
	}

	return matcher
}

func (m emptyResponseTextMatcher) matches(text string) bool {
	if len(m.patterns) == 0 {
		return false
	}

	_, ok := m.patterns[normalizeEmptyResponseText(text)]
	return ok
}

func (m emptyResponseTextMatcher) matchesPrefix(text string) bool {
	if len(m.patterns) == 0 {
		return false
	}

	normalized := normalizeEmptyResponseText(text)
	if normalized == "" {
		return false
	}

	for pattern := range m.patterns {
		if strings.HasPrefix(pattern, normalized) {
			return true
		}
	}

	return false
}

func normalizeEmptyResponseText(text string) string {
	normalized := strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(text)), " "))
	normalized = strings.Trim(normalized, " \t\r\n.。!！")

	return normalized
}

func messageTextHasContent(text *string, matcher emptyResponseTextMatcher) bool {
	if text == nil || strings.TrimSpace(*text) == "" {
		return false
	}

	return !matcher.matches(*text)
}

func multipleContentHasContent(parts []llm.MessageContentPart, matcher emptyResponseTextMatcher) bool {
	if len(parts) == 0 {
		return false
	}

	var textParts []string

	for _, part := range parts {
		if part.Type != "text" {
			return true
		}

		if part.Text != nil && strings.TrimSpace(*part.Text) != "" {
			textParts = append(textParts, *part.Text)
		}
	}

	if len(textParts) == 0 {
		return false
	}

	text := strings.Join(textParts, "")
	return messageTextHasContent(&text, matcher)
}

func bufferedResponsesTextContent(responses []*llm.Response) (string, bool) {
	if len(responses) == 0 {
		return "", true
	}

	var builder strings.Builder

	for _, resp := range responses {
		text, ok := responseTextContent(resp)
		if !ok {
			return "", false
		}

		builder.WriteString(text)
	}

	return builder.String(), true
}

func responseTextContent(resp *llm.Response) (string, bool) {
	if resp == nil || resp == llm.DoneResponse {
		return "", true
	}

	if resp.Embedding != nil && len(resp.Embedding.Data) > 0 {
		return "", false
	}

	if resp.Rerank != nil && len(resp.Rerank.Results) > 0 {
		return "", false
	}

	if resp.Image != nil && len(resp.Image.Data) > 0 {
		return "", false
	}

	if resp.Video != nil &&
		(resp.Video.ID != "" || resp.Video.Status != "" || resp.Video.VideoURL != "" || resp.Video.Error != nil) {
		return "", false
	}

	if resp.Compact != nil && len(resp.Compact.Output) > 0 {
		return "", false
	}

	var builder strings.Builder

	if resp.Completion != nil {
		for _, choice := range resp.Completion.Choices {
			builder.WriteString(choice.Text)
		}
	}

	for _, choice := range resp.Choices {
		text, ok := messageTextContent(choice.Delta)
		if !ok {
			return "", false
		}

		builder.WriteString(text)

		text, ok = messageTextContent(choice.Message)
		if !ok {
			return "", false
		}

		builder.WriteString(text)
	}

	return builder.String(), true
}

func messageTextContent(msg *llm.Message) (string, bool) {
	if msg == nil {
		return "", true
	}

	if len(msg.ToolCalls) > 0 ||
		msg.ReasoningContent != nil ||
		msg.Reasoning != nil ||
		msg.Audio != nil {
		return "", false
	}

	var builder strings.Builder

	if len(msg.Content.MultipleContent) > 0 {
		for _, part := range msg.Content.MultipleContent {
			if part.Type != "text" {
				return "", false
			}

			if part.Text != nil {
				builder.WriteString(*part.Text)
			}
		}
	} else if msg.Content.Content != nil {
		builder.WriteString(*msg.Content.Content)
	}

	builder.WriteString(msg.Refusal)

	return builder.String(), true
}
