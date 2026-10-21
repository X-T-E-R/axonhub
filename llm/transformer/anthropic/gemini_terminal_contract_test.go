package anthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/gemini"
	"github.com/stretchr/testify/require"
)

func geminiMessageEvent(t *testing.T, parts []*gemini.Part, finish string, usage *gemini.UsageMetadata) *httpclient.StreamEvent {
	t.Helper()
	response := &gemini.GenerateContentResponse{ResponseID: "synthetic-message", UsageMetadata: usage}
	if parts != nil || finish != "" {
		response.Candidates = []*gemini.Candidate{{Index: 0, Content: &gemini.Content{Role: "model", Parts: parts}, FinishReason: finish}}
	}
	data, err := json.Marshal(response)
	require.NoError(t, err)
	return &httpclient.StreamEvent{Data: data}
}

func messageEventCounts(t *testing.T, events []*httpclient.StreamEvent) (text, thinking, tools, terminal int, stop string) {
	t.Helper()
	for _, event := range events {
		var d struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
				Stop     string `json:"stop_reason"`
			} `json:"delta"`
		}
		require.NoError(t, json.Unmarshal(event.Data, &d))
		text += len(d.Delta.Text)
		thinking += len(d.Delta.Thinking)
		if d.ContentBlock.Type == "tool_use" {
			tools++
		}
		if d.Type == "message_stop" {
			terminal++
		}
		if d.Delta.Stop != "" {
			stop = d.Delta.Stop
		}
	}
	return
}

func TestGeminiMessagesTerminalContract(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		parts                  []*gemini.Part
		finish                 string
		wantText, wantThinking int
		wantError              bool
	}{
		{name: "thinking_only_real_stop", parts: []*gemini.Part{{Text: "synthetic thought", Thought: true}}, finish: "STOP", wantThinking: 17},
		{name: "answer_real_stop", parts: []*gemini.Part{{Text: "OK"}}, finish: "STOP", wantText: 2},
		{name: "thinking_eof_no_finish", parts: []*gemini.Part{{Text: "synthetic thought", Thought: true}}, wantThinking: 17, wantError: true},
		{name: "answer_eof_no_finish", parts: []*gemini.Part{{Text: "OK"}}, wantText: 2, wantError: true},
		{name: "usage_only_eof_no_finish", wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []*httpclient.StreamEvent{}
			if tc.parts != nil {
				raw = append(raw, geminiMessageEvent(t, tc.parts, "", nil))
			}
			if tc.finish != "" {
				raw = append(raw, geminiMessageEvent(t, nil, tc.finish, nil))
			}
			// Late usage is not a candidate terminal, even when it is the final provider frame.
			raw = append(raw, geminiMessageEvent(t, nil, "", &gemini.UsageMetadata{PromptTokenCount: 11, CandidatesTokenCount: 7, TotalTokenCount: 18}))
			outbound, err := gemini.NewOutboundTransformerWithConfig(gemini.Config{})
			require.NoError(t, err)
			unified, err := outbound.TransformStream(t.Context(), nil, streams.SliceStream(raw))
			require.NoError(t, err)
			target, err := anthropic.NewInboundTransformer().TransformStream(t.Context(), unified)
			require.NoError(t, err)
			var events []*httpclient.StreamEvent
			for target.Next() {
				events = append(events, target.Current())
			}
			if tc.wantError {
				require.ErrorIs(t, target.Err(), transformer.ErrInvalidResponse)
			} else {
				require.NoError(t, target.Err())
			}
			text, thought, tools, terminal, stop := messageEventCounts(t, events)
			require.Equal(t, tc.wantText, text)
			require.Equal(t, tc.wantThinking, thought)
			require.Zero(t, tools)
			if tc.wantError {
				require.Zero(t, terminal)
				require.Empty(t, stop)
			} else {
				require.Equal(t, 1, terminal)
				require.Equal(t, "end_turn", stop)
			}
		})
	}
}

func TestGeminiMessagesTerminalContract_PreservesLegacyNonGeminiEOF(t *testing.T) {
	text := "legacy"
	target, err := anthropic.NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream([]*llm.Response{{ID: "synthetic", Choices: []llm.Choice{{Index: 0, Delta: &llm.Message{Content: llm.MessageContent{Content: &text}}}}}}))
	require.NoError(t, err)
	events, err := streams.All(target)
	require.NoError(t, err)
	got, _, _, terminal, stop := messageEventCounts(t, events)
	require.Equal(t, len(text), got)
	require.Equal(t, 1, terminal)
	require.Equal(t, "end_turn", stop)
}

func TestGeminiMessagesTerminalContract_RemembersFormatAndAllCandidates(t *testing.T) {
	for _, finishBoth := range []bool{false, true} {
		t.Run(map[bool]string{false: "candidate_missing_finish", true: "both_finished"}[finishBoth], func(t *testing.T) {
			text := "OK"
			stop := "stop"
			first := &llm.Response{APIFormat: llm.APIFormatGeminiContents, ID: "synthetic", Choices: []llm.Choice{{Index: 2, Delta: &llm.Message{Content: llm.MessageContent{Content: &text}}}, {Index: 0, Delta: &llm.Message{Content: llm.MessageContent{Content: &text}}}}}
			last := &llm.Response{Choices: []llm.Choice{{Index: 2, FinishReason: &stop}}, Usage: &llm.Usage{CompletionTokens: 7}}
			if finishBoth {
				last.Choices = append(last.Choices, llm.Choice{Index: 0, FinishReason: &stop})
			}
			// Deliberately no APIFormat on terminal/usage or done: recognition belongs to the stream.
			target, err := anthropic.NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream([]*llm.Response{first, last, llm.DoneResponse}))
			require.NoError(t, err)
			var events []*httpclient.StreamEvent
			for target.Next() {
				events = append(events, target.Current())
			}
			_, _, _, terminal, stopReason := messageEventCounts(t, events)
			if finishBoth {
				require.NoError(t, target.Err())
				require.Equal(t, 1, terminal)
				require.Equal(t, "end_turn", stopReason)
			} else {
				require.ErrorIs(t, target.Err(), transformer.ErrInvalidResponse)
				require.Zero(t, terminal)
				require.Empty(t, stopReason)
			}
		})
	}
}

type terminalErrorStream struct {
	streams.Stream[*llm.Response]
	failure error
}

func (s *terminalErrorStream) Err() error { return s.failure }

func TestGeminiMessagesTerminalContract_PreservesSourceErrors(t *testing.T) {
	for _, failure := range []error{context.Canceled, errors.New("synthetic provider failure")} {
		text := "partial"
		source := &terminalErrorStream{Stream: streams.SliceStream([]*llm.Response{{APIFormat: llm.APIFormatGeminiContents, Choices: []llm.Choice{{Index: 0, Delta: &llm.Message{Content: llm.MessageContent{Content: &text}}}}}}), failure: failure}
		target, err := anthropic.NewInboundTransformer().TransformStream(t.Context(), source)
		require.NoError(t, err)
		var events []*httpclient.StreamEvent
		for target.Next() {
			events = append(events, target.Current())
		}
		require.ErrorIs(t, target.Err(), failure)
		_, _, _, terminal, stop := messageEventCounts(t, events)
		require.Zero(t, terminal)
		require.Empty(t, stop)
	}
}
