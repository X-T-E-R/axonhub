package anthropic_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
	"github.com/looplj/axonhub/llm/transformer/gemini"
	"github.com/stretchr/testify/require"
)

// Source errors become observable only after every provider frame was consumed.
type geminiTerminalSource struct {
	streams.Stream[*httpclient.StreamEvent]
	read      int
	exhausted bool
	failure   error
}

func (s *geminiTerminalSource) Next() bool {
	if !s.Stream.Next() {
		s.exhausted = true
		return false
	}
	s.read++
	return true
}

func (s *geminiTerminalSource) Err() error {
	if s.exhausted {
		return s.failure
	}
	return nil
}

func geminiCandidateEvent(t *testing.T, index int64, text, finish string, usage *gemini.UsageMetadata) *httpclient.StreamEvent {
	t.Helper()
	candidate := &gemini.Candidate{Index: index, FinishReason: finish}
	if text != "" {
		candidate.Content = &gemini.Content{Role: "model", Parts: []*gemini.Part{{Text: text}}}
	}
	data, err := json.Marshal(&gemini.GenerateContentResponse{
		ResponseID: "synthetic-whole-stream", Candidates: []*gemini.Candidate{candidate}, UsageMetadata: usage,
	})
	require.NoError(t, err)
	return &httpclient.StreamEvent{Data: data}
}

func TestGeminiMessagesTerminalContract_WholeStreamEOF(t *testing.T) {
	providerFailure := errors.New("synthetic late provider failure")
	for _, tc := range []struct {
		name       string
		lastFinish string
		failure    error
		wantError  error
	}{
		{name: "late_candidate_missing_finish", wantError: transformer.ErrInvalidResponse},
		{name: "late_candidate_provider_failure", failure: providerFailure, wantError: providerFailure},
		{name: "late_candidate_canceled", failure: context.Canceled, wantError: context.Canceled},
		{name: "late_candidate_real_finish", lastFinish: "STOP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frames := []*httpclient.StreamEvent{
				geminiCandidateEvent(t, 0, "first", "", nil),
				geminiCandidateEvent(t, 0, "", "STOP", &gemini.UsageMetadata{PromptTokenCount: 11, CandidatesTokenCount: 7, TotalTokenCount: 18}),
				geminiCandidateEvent(t, 1, "later", tc.lastFinish, nil),
			}
			if tc.wantError == nil {
				// Final usage must replace the earlier usage, not be lost at the first STOP.
				frames = append(frames, geminiMessageEvent(t, nil, "", &gemini.UsageMetadata{PromptTokenCount: 11, CandidatesTokenCount: 9, TotalTokenCount: 20}))
			}
			raw := &geminiTerminalSource{Stream: streams.SliceStream(frames), failure: tc.failure}
			outbound, err := gemini.NewOutboundTransformerWithConfig(gemini.Config{})
			require.NoError(t, err)
			unified, err := outbound.TransformStream(t.Context(), nil, raw)
			require.NoError(t, err)
			target, err := anthropic.NewInboundTransformer().TransformStream(t.Context(), unified)
			require.NoError(t, err)

			var events []*httpclient.StreamEvent
			text := ""
			terminalSeen := false
			for target.Next() {
				event := target.Current()
				require.False(t, terminalSeen, "no event may follow message_stop")
				var decoded struct {
					Delta struct {
						Text string `json:"text"`
					} `json:"delta"`
					Usage *anthropic.Usage `json:"usage"`
				}
				require.NoError(t, json.Unmarshal(event.Data, &decoded))
				if decoded.Delta.Text != "" {
					if text == "" {
						require.Equal(t, 1, raw.read, "first text must stream before later frames are read")
					}
					require.False(t, raw.exhausted, "text must not be buffered until EOF")
					text += decoded.Delta.Text
				}
				if event.Type == "message_delta" || event.Type == "message_stop" {
					require.True(t, raw.exhausted, "normal terminal must wait for actual source EOF")
				}
				if event.Type == "message_delta" {
					require.NotNil(t, decoded.Usage)
					require.EqualValues(t, 11, decoded.Usage.InputTokens)
					require.EqualValues(t, 9, decoded.Usage.OutputTokens)
				}
				terminalSeen = event.Type == "message_stop"
				events = append(events, event)
			}
			if tc.wantError != nil {
				require.ErrorIs(t, target.Err(), tc.wantError)
			} else {
				require.NoError(t, target.Err())
			}
			require.True(t, raw.exhausted)
			require.Equal(t, "firstlater", text)
			_, _, _, terminals, stopReason := messageEventCounts(t, events)
			if tc.wantError != nil {
				require.Zero(t, terminals)
				require.Empty(t, stopReason)
			} else {
				require.Equal(t, 1, terminals)
				require.Equal(t, "end_turn", stopReason)
			}
		})
	}
}

func TestGeminiMessagesTerminalContract_ValidatesLateToolBeforeEOF(t *testing.T) {
	for _, name := range []string{"synthetic_tool", ""} {
		t.Run(map[bool]string{true: "complete_tool", false: "missing_tool_name"}[name != ""], func(t *testing.T) {
			data, err := json.Marshal(&gemini.GenerateContentResponse{
				ResponseID: "synthetic-whole-stream",
				Candidates: []*gemini.Candidate{{Index: 1, FinishReason: "STOP", Content: &gemini.Content{Role: "model", Parts: []*gemini.Part{{FunctionCall: &gemini.FunctionCall{ID: "synthetic_call", Name: name, Args: map[string]any{"value": 1}}}}}}},
			})
			require.NoError(t, err)
			raw := &geminiTerminalSource{Stream: streams.SliceStream([]*httpclient.StreamEvent{
				geminiCandidateEvent(t, 0, "first", "", nil),
				geminiCandidateEvent(t, 0, "", "STOP", &gemini.UsageMetadata{PromptTokenCount: 11, CandidatesTokenCount: 7, TotalTokenCount: 18}),
				{Data: data},
			})}
			outbound, err := gemini.NewOutboundTransformerWithConfig(gemini.Config{})
			require.NoError(t, err)
			unified, err := outbound.TransformStream(t.Context(), nil, raw)
			require.NoError(t, err)
			target, err := anthropic.NewInboundTransformer().TransformStream(t.Context(), unified)
			require.NoError(t, err)
			var events []*httpclient.StreamEvent
			for target.Next() {
				events = append(events, target.Current())
			}
			_, _, tools, terminals, stopReason := messageEventCounts(t, events)
			if name == "" {
				require.ErrorIs(t, target.Err(), transformer.ErrIncompleteToolCall)
				require.Zero(t, terminals)
				require.Empty(t, stopReason)
			} else {
				require.NoError(t, target.Err())
				require.True(t, raw.exhausted)
				require.Equal(t, 1, tools)
				require.Equal(t, 1, terminals)
				require.Equal(t, "tool_use", stopReason)
				require.Equal(t, "message_stop", events[len(events)-1].Type)
			}
		})
	}
}
