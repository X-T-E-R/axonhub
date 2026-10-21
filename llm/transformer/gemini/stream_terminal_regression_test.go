package gemini

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
)

func separatedTerminalEvent(t *testing.T, candidates ...*Candidate) *httpclient.StreamEvent {
	t.Helper()
	body, err := json.Marshal(&GenerateContentResponse{ResponseID: "synthetic-response", Candidates: candidates})
	require.NoError(t, err)
	return &httpclient.StreamEvent{Data: body}
}

func separatedToolCandidate(index int64) *Candidate {
	return &Candidate{Index: index, Content: &Content{Role: "model", Parts: []*Part{{
		FunctionCall:     &FunctionCall{ID: "synthetic-call", Name: "synthetic_tool", Args: map[string]any{"ok": true}},
		ThoughtSignature: "synthetic-signature",
	}}}}
}

func TestGeminiSeparatedTerminal_InboundPreservesFinishAndUsage(t *testing.T) {
	for _, withTool := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "tool"}[withTool], func(t *testing.T) {
			first := &llm.Response{ID: "synthetic-response", Choices: []llm.Choice{{Index: 0, Delta: &llm.Message{Content: llm.MessageContent{Content: lo.ToPtr("synthetic")}}}}}
			finish := "stop"
			if withTool {
				first = geminiIntegrityChunk(0, "synthetic-call", "synthetic_tool", `{"ok":true}`)
				finish = "tool_calls"
			}
			terminal := &llm.Response{ID: "synthetic-response", Choices: []llm.Choice{{Index: 0, FinishReason: &finish}}, Usage: &llm.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18}}
			stream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream([]*llm.Response{first, terminal, llm.DoneResponse}))
			require.NoError(t, err)
			events, err := streams.All(stream)
			require.NoError(t, err)
			require.NotEmpty(t, events)
			var last GenerateContentResponse
			require.NoError(t, json.Unmarshal(events[len(events)-1].Data, &last))
			require.Len(t, last.Candidates, 1)
			require.Equal(t, "STOP", last.Candidates[0].FinishReason)
			require.Nil(t, last.Candidates[0].Content, "a contentless terminal must not invent a content part")
			require.NotNil(t, last.UsageMetadata)
			require.EqualValues(t, 18, last.UsageMetadata.TotalTokenCount)
			tools := 0
			for _, event := range events {
				var response GenerateContentResponse
				require.NoError(t, json.Unmarshal(event.Data, &response))
				for _, candidate := range response.Candidates {
					if candidate.Content != nil {
						for _, part := range candidate.Content.Parts {
							if part.FunctionCall != nil {
								tools++
							}
						}
					}
				}
			}
			if withTool {
				require.Equal(t, 1, tools)
			} else {
				require.Zero(t, tools)
			}
		})
	}
}

func TestGeminiSeparatedTerminal_OutboundTracksToolsPerCandidate(t *testing.T) {
	out, err := NewOutboundTransformerWithConfig(Config{})
	require.NoError(t, err)
	stream, err := out.TransformStream(t.Context(), nil, streams.SliceStream([]*httpclient.StreamEvent{
		separatedTerminalEvent(t, separatedToolCandidate(0)),
		separatedTerminalEvent(t, &Candidate{Index: 0, FinishReason: "STOP"}, &Candidate{Index: 1, FinishReason: "STOP"}),
	}))
	require.NoError(t, err)
	values, err := streams.All(stream)
	require.NoError(t, err)
	require.Len(t, values[1].Choices, 2)
	require.Equal(t, "tool_calls", *values[1].Choices[0].FinishReason)
	require.Equal(t, "stop", *values[1].Choices[1].FinishReason, "another candidate must not inherit tool state")
	// A new stream on the same transformer must not inherit the previous request's tools.
	fresh, err := out.TransformStream(t.Context(), nil, streams.SliceStream([]*httpclient.StreamEvent{separatedTerminalEvent(t, &Candidate{FinishReason: "STOP"})}))
	require.NoError(t, err)
	freshValues, err := streams.All(fresh)
	require.NoError(t, err)
	require.Equal(t, "stop", *freshValues[0].Choices[0].FinishReason)
}

func TestGeminiSeparatedTerminal_RoundTripAndRawInput(t *testing.T) {
	first := separatedTerminalEvent(t, separatedToolCandidate(0))
	// Unknown provider data is retained by the raw pass-through branch; transforms must not mutate it.
	first.Data = bytes.Replace(first.Data, []byte(`"responseId"`), []byte(`"unknownProviderField":{"opaque":"synthetic"},"responseId"`), 1)
	rawBefore := bytes.Clone(first.Data)
	out, err := NewOutboundTransformerWithConfig(Config{})
	require.NoError(t, err)
	normalized, err := out.TransformStream(t.Context(), nil, streams.SliceStream([]*httpclient.StreamEvent{first, separatedTerminalEvent(t, &Candidate{FinishReason: "STOP"})}))
	require.NoError(t, err)
	values, err := streams.All(normalized)
	require.NoError(t, err)
	for _, tc := range values[0].Choices[0].Delta.ToolCalls {
		require.NotEmpty(t, tc.TransformerMetadata, "tool thought signature survives normalization")
	}
	for _, format := range []string{"gemini", "messages"} {
		t.Run(format, func(t *testing.T) {
			var target transformer.Inbound = NewInboundTransformer()
			if format == "messages" {
				target = anthropic.NewInboundTransformer()
			}
			stream, err := target.TransformStream(t.Context(), streams.SliceStream(values))
			require.NoError(t, err)
			events, err := streams.All(stream)
			require.NoError(t, err)
			require.NotEmpty(t, events)
			if format == "gemini" {
				var last GenerateContentResponse
				require.NoError(t, json.Unmarshal(events[len(events)-1].Data, &last))
				require.Equal(t, "STOP", last.Candidates[0].FinishReason)
			} else {
				stops := 0
				for _, event := range events {
					var payload struct {
						Delta struct {
							StopReason string `json:"stop_reason"`
						} `json:"delta"`
					}
					require.NoError(t, json.Unmarshal(event.Data, &payload))
					if payload.Delta.StopReason == "tool_use" {
						stops++
					}
				}
				require.Equal(t, 1, stops)
			}
		})
	}
	require.Equal(t, rawBefore, first.Data, "raw provider bytes and unknown fields must be untouched")
}

func TestGeminiSeparatedTerminal_IncompleteToolStillFails(t *testing.T) {
	for _, finish := range []string{"", "tool_calls"} {
		t.Run(map[bool]string{false: "missing_finish", true: "invalid_arguments"}[finish != ""], func(t *testing.T) {
			args := `{"ok":true}`
			if finish != "" {
				args = `{"ok":`
			}
			chunks := []*llm.Response{geminiIntegrityChunk(0, "synthetic-call", "synthetic_tool", args)}
			if finish != "" {
				chunks = append(chunks, &llm.Response{Choices: []llm.Choice{{Index: 0, FinishReason: &finish}}})
			}
			stream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream(chunks))
			require.NoError(t, err)
			_, err = streams.All(stream)
			require.ErrorIs(t, err, transformer.ErrIncompleteToolCall)
		})
	}
}

func TestGeminiSeparatedTerminal_UsageOnlyIsNotCompletion(t *testing.T) {
	stream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream([]*llm.Response{{
		ID: "synthetic-response", Choices: []llm.Choice{{Index: 0}},
		Usage: &llm.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18},
	}}))
	require.NoError(t, err)
	events, err := streams.All(stream)
	require.NoError(t, err)
	require.Len(t, events, 1)
	var response GenerateContentResponse
	require.NoError(t, json.Unmarshal(events[0].Data, &response))
	require.NotNil(t, response.UsageMetadata)
	require.EqualValues(t, 18, response.UsageMetadata.TotalTokenCount)
	require.Empty(t, response.Candidates[0].FinishReason)
	require.Nil(t, response.Candidates[0].Content)
}

func TestGeminiSeparatedTerminal_NoFinishIsNotFabricated(t *testing.T) {
	out, err := NewOutboundTransformerWithConfig(Config{})
	require.NoError(t, err)
	normalized, err := out.TransformStream(t.Context(), nil, streams.SliceStream([]*httpclient.StreamEvent{separatedTerminalEvent(t, &Candidate{Content: &Content{Role: "model", Parts: []*Part{{Text: "synthetic"}}}})}))
	require.NoError(t, err)
	stream, err := NewInboundTransformer().TransformStream(t.Context(), normalized)
	require.NoError(t, err)
	events, err := streams.All(stream)
	require.NoError(t, err)
	for _, event := range events {
		var response GenerateContentResponse
		require.NoError(t, json.Unmarshal(event.Data, &response))
		for _, candidate := range response.Candidates {
			require.Empty(t, candidate.FinishReason)
		}
	}
}
