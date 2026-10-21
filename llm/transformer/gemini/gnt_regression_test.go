package gemini

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
)

func TestGNT001_CandidateToolTerminalsStayIndependent(t *testing.T) {
	for _, priorCalls := range []bool{true, false} {
		t.Run(fmt.Sprintf("prior_calls_%t", priorCalls), func(t *testing.T) {
			// Distinct indices and signatures prove identity, not array position, owns each call.
			candidates := []*Candidate{}
			for _, index := range []int64{0, 2} {
				c := &Candidate{Index: index, Content: &Content{Role: "model"}}
				for call := range 2 {
					c.Content.Parts = append(c.Content.Parts, &Part{FunctionCall: &FunctionCall{
						ID: fmt.Sprintf("call-%d-%d", index, call), Name: "synthetic_tool",
						Args: map[string]any{"candidate": index, "call": call},
					}, ThoughtSignature: fmt.Sprintf("signature-%d-%d", index, call)})
				}
				candidates = append(candidates, c)
			}
			var raw []*httpclient.StreamEvent
			if priorCalls {
				raw = append(raw, separatedTerminalEvent(t, candidates...))
				raw = append(raw, separatedTerminalEvent(t, &Candidate{Index: 2, FinishReason: "STOP"}, &Candidate{Index: 0, FinishReason: "STOP"}))
			} else {
				for _, c := range candidates {
					c.FinishReason = "STOP"
				}
				raw = append(raw, separatedTerminalEvent(t, candidates[1], candidates[0]))
			}
			var terminal GenerateContentResponse
			require.NoError(t, json.Unmarshal(raw[len(raw)-1].Data, &terminal))
			terminal.UsageMetadata = &UsageMetadata{TotalTokenCount: 18}
			var err error
			raw[len(raw)-1].Data, err = json.Marshal(terminal)
			require.NoError(t, err)
			out, err := NewOutboundTransformerWithConfig(Config{})
			require.NoError(t, err)
			normalized, err := out.TransformStream(t.Context(), nil, streams.SliceStream(raw))
			require.NoError(t, err)
			inbound, err := NewInboundTransformer().TransformStream(t.Context(), normalized)
			require.NoError(t, err)
			events, err := streams.All(inbound)
			require.NoError(t, err)
			got := map[int64]map[string]int{}
			finishes := map[int64]int{}
			usageFrames := 0
			for _, event := range events {
				var response GenerateContentResponse
				require.NoError(t, json.Unmarshal(event.Data, &response))
				if response.UsageMetadata != nil {
					usageFrames++
					require.EqualValues(t, 18, response.UsageMetadata.TotalTokenCount)
				}
				for _, c := range response.Candidates {
					if c.FinishReason != "" {
						require.Equal(t, "STOP", c.FinishReason)
						finishes[c.Index]++
					}
					if c.Content == nil {
						continue
					}
					for _, part := range c.Content.Parts {
						if part.FunctionCall == nil {
							continue
						}
						tc := part.FunctionCall
						call, ok := tc.Args["call"].(float64)
						require.True(t, ok)
						require.EqualValues(t, c.Index, tc.Args["candidate"])
						require.Equal(t, fmt.Sprintf("call-%d-%d", c.Index, int(call)), tc.ID)
						require.Equal(t, fmt.Sprintf("signature-%d-%d", c.Index, int(call)), part.ThoughtSignature)
						if got[c.Index] == nil {
							got[c.Index] = map[string]int{}
						}
						got[c.Index][tc.ID]++
					}
				}
			}
			require.Equal(t, 1, usageFrames)
			for _, index := range []int64{0, 2} {
				require.Equal(t, 1, finishes[index])
				require.Len(t, got[index], 2)
				for call := range 2 {
					require.Equal(t, 1, got[index][fmt.Sprintf("call-%d-%d", index, call)])
				}
			}
		})
	}
}

func TestGNT002_AggregationRequiresExplicitCandidateFinish(t *testing.T) {
	for _, name := range []string{"usage_no_choices", "usage_empty_choice", "text_without_finish", "one_candidate_missing_finish"} {
		t.Run(name, func(t *testing.T) {
			response := &GenerateContentResponse{ResponseID: "synthetic", UsageMetadata: &UsageMetadata{TotalTokenCount: 18}}
			switch name {
			case "usage_empty_choice":
				response.Candidates = []*Candidate{{Index: 0}}
			case "text_without_finish":
				response.Candidates = []*Candidate{{Index: 0, Content: &Content{Role: "model", Parts: []*Part{{Text: "synthetic"}}}}}
			case "one_candidate_missing_finish":
				response.Candidates = []*Candidate{{Index: 2, FinishReason: "STOP"}, {Index: 0}}
			}
			body, err := json.Marshal(response)
			require.NoError(t, err)
			data, meta, err := AggregateStreamChunks(t.Context(), []*httpclient.StreamEvent{{Data: body}})
			require.NoError(t, err)
			require.False(t, meta.Completed)
			var aggregated GenerateContentResponse
			require.NoError(t, json.Unmarshal(data, &aggregated))
			require.NotNil(t, aggregated.UsageMetadata)
			require.EqualValues(t, 18, aggregated.UsageMetadata.TotalTokenCount)
			require.Len(t, aggregated.Candidates, len(response.Candidates))
			finishes := map[int64]string{}
			for _, c := range aggregated.Candidates {
				require.NotNil(t, c)
				finishes[c.Index] = c.FinishReason
			}
			for _, c := range response.Candidates {
				require.Equal(t, c.FinishReason, finishes[c.Index], "finish must belong only to its original candidate")
			}
		})
	}
}

func TestGNT002_ExplicitTerminalsAndTrailingUsageArePreserved(t *testing.T) {
	for _, finish := range []string{"STOP", "MAX_TOKENS"} {
		t.Run(finish, func(t *testing.T) {
			terminal := &GenerateContentResponse{ResponseID: "synthetic", Candidates: []*Candidate{{Index: 2, FinishReason: finish, Content: &Content{Role: "model", Parts: []*Part{{FunctionCall: &FunctionCall{ID: "call", Name: "synthetic_tool", Args: map[string]any{"ok": true}}, ThoughtSignature: "signature"}}}}}}
			body, err := json.Marshal(terminal)
			require.NoError(t, err)
			usage := &GenerateContentResponse{ResponseID: "synthetic", Candidates: []*Candidate{{Index: 2}}, UsageMetadata: &UsageMetadata{TotalTokenCount: 18}}
			tail, err := json.Marshal(usage)
			require.NoError(t, err)
			data, _, err := AggregateStreamChunks(t.Context(), []*httpclient.StreamEvent{{Data: body}, {Data: tail}})
			require.NoError(t, err)
			var aggregated GenerateContentResponse
			require.NoError(t, json.Unmarshal(data, &aggregated))
			require.Len(t, aggregated.Candidates, 1)
			c := aggregated.Candidates[0]
			require.NotNil(t, c)
			require.EqualValues(t, 2, c.Index)
			require.Equal(t, finish, c.FinishReason)
			require.EqualValues(t, 18, aggregated.UsageMetadata.TotalTokenCount)
			require.Len(t, c.Content.Parts, 1)
			require.Equal(t, "call", c.Content.Parts[0].FunctionCall.ID)
			require.Equal(t, "signature", c.Content.Parts[0].ThoughtSignature)
			require.Equal(t, true, c.Content.Parts[0].FunctionCall.Args["ok"])
		})
	}
}

func TestGNT002_UsageOnlyRoundTripStaysIncomplete(t *testing.T) {
	stream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream([]*llm.Response{{ID: "synthetic", Choices: []llm.Choice{{Index: 0}}, Usage: &llm.Usage{TotalTokens: 18}}}))
	require.NoError(t, err)
	events, err := streams.All(stream)
	require.NoError(t, err)
	body, meta, err := AggregateStreamChunks(t.Context(), events)
	require.NoError(t, err)
	require.False(t, meta.Completed)
	var response GenerateContentResponse
	require.NoError(t, json.Unmarshal(body, &response))
	require.Len(t, response.Candidates, 1)
	require.Empty(t, response.Candidates[0].FinishReason)
	require.EqualValues(t, 18, response.UsageMetadata.TotalTokenCount)
}
