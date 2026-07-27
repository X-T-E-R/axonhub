package openai

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

func TestAggregateToolCallIntegrity_InterleavedFragments(t *testing.T) {
	chunks := []*httpclient.StreamEvent{
		{Data: []byte(`{"id":"resp_1","model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"spawn_agent","arguments":"{\"task\":"}}]}}]}`)},
		{Data: []byte(`{"id":"resp_1","model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"wait_agent","arguments":"{\"ms\":"}}]}}]}`)},
		{Data: []byte(`{"id":"resp_1","model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"audit\"}"}}]}}]}`)},
		{Data: []byte(`{"id":"resp_1","model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"1}"}}]},"finish_reason":"tool_calls"}]}`)},
	}

	body, _, err := AggregateStreamChunks(t.Context(), chunks, DefaultTransformChunk)
	require.NoError(t, err)
	var response llm.Response
	require.NoError(t, json.Unmarshal(body, &response))
	require.Len(t, response.Choices, 1)
	require.NotNil(t, response.Choices[0].Message)
	require.Equal(t, "call_a", response.Choices[0].Message.ToolCalls[0].ID)
	require.Equal(t, "spawn_agent", response.Choices[0].Message.ToolCalls[0].Function.Name)
	require.JSONEq(t, `{"task":"audit"}`, response.Choices[0].Message.ToolCalls[0].Function.Arguments)
	require.Equal(t, "call_b", response.Choices[0].Message.ToolCalls[1].ID)
	require.Equal(t, "wait_agent", response.Choices[0].Message.ToolCalls[1].Function.Name)
	require.JSONEq(t, `{"ms":1}`, response.Choices[0].Message.ToolCalls[1].Function.Arguments)
}

func TestAggregateToolCallIntegrity_TruncatedFails(t *testing.T) {
	chunks := []*httpclient.StreamEvent{{
		Data: []byte(`{"id":"resp_1","model":"gpt-test","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"spawn_agent","arguments":"{\"task\":"}}]}}]}`),
	}}

	_, _, err := AggregateStreamChunks(t.Context(), chunks, DefaultTransformChunk)
	require.ErrorIs(t, err, transformer.ErrIncompleteToolCall)
}

func TestInboundToolCallIntegrity_LateProviderIDReplaysBufferedCall(t *testing.T) {
	finish := "tool_calls"
	source := streams.SliceStream([]*llm.Response{
		{
			Choices: []llm.Choice{{
				Index: 0,
				Delta: &llm.Message{ToolCalls: []llm.ToolCall{{
					Index: 0,
					Type:  "function",
					Function: llm.FunctionCall{
						Name:      "get_data",
						Arguments: `{"page":`,
					},
				}}},
			}},
		},
		{
			Choices: []llm.Choice{{
				Index: 0,
				Delta: &llm.Message{ToolCalls: []llm.ToolCall{{
					Index: 0,
					ID:    "call_late",
					Function: llm.FunctionCall{
						Arguments: `1}`,
					},
				}}},
			}},
		},
		{Choices: []llm.Choice{{Index: 0, FinishReason: &finish}}},
	})

	stream, err := NewInboundTransformer().TransformStream(t.Context(), source)
	require.NoError(t, err)
	events, err := streams.All(stream)
	require.NoError(t, err)

	var calls []ToolCall
	for _, event := range events {
		var response Response
		require.NoError(t, json.Unmarshal(event.Data, &response))
		for _, choice := range response.Choices {
			if choice.Delta != nil {
				calls = append(calls, choice.Delta.ToolCalls...)
			}
			if choice.Message != nil {
				calls = append(calls, choice.Message.ToolCalls...)
			}
		}
	}
	require.Len(t, calls, 1)
	require.Equal(t, "call_late", calls[0].ID)
	require.Equal(t, "get_data", calls[0].Function.Name)
	require.JSONEq(t, `{"page":1}`, calls[0].Function.Arguments)
}

func TestInboundToolCallIntegrity_MissingProviderIDFailsAtTerminal(t *testing.T) {
	finish := "tool_calls"
	source := streams.SliceStream([]*llm.Response{
		{
			Choices: []llm.Choice{{
				Index: 0,
				Delta: &llm.Message{ToolCalls: []llm.ToolCall{{
					Index: 0,
					Type:  "function",
					Function: llm.FunctionCall{
						Name:      "get_data",
						Arguments: `{}`,
					},
				}}},
			}},
		},
		{Choices: []llm.Choice{{Index: 0, FinishReason: &finish}}},
	})

	stream, err := NewInboundTransformer().TransformStream(t.Context(), source)
	require.NoError(t, err)
	_, err = streams.All(stream)
	require.ErrorIs(t, err, transformer.ErrToolCallIntegrity)
}

func TestInboundToolCallIntegrity_InterleavedLateIDsDoNotCrossPair(t *testing.T) {
	finish := "tool_calls"
	fragment := func(index int, id, name, arguments string) *llm.Response {
		return &llm.Response{
			Choices: []llm.Choice{{
				Index: 0,
				Delta: &llm.Message{ToolCalls: []llm.ToolCall{{
					Index: index,
					ID:    id,
					Type:  "function",
					Function: llm.FunctionCall{
						Name:      name,
						Arguments: arguments,
					},
				}}},
			}},
		}
	}
	source := streams.SliceStream([]*llm.Response{
		fragment(0, "", "spawn_agent", `{"task":`),
		fragment(1, "", "wait_agent", `{"ms":`),
		fragment(1, "call_wait", "", `1}`),
		fragment(0, "call_spawn", "", `"audit"}`),
		{Choices: []llm.Choice{{Index: 0, FinishReason: &finish}}},
	})

	stream, err := NewInboundTransformer().TransformStream(t.Context(), source)
	require.NoError(t, err)
	events, err := streams.All(stream)
	require.NoError(t, err)

	calls := make(map[string]ToolCall)
	for _, event := range events {
		var response Response
		require.NoError(t, json.Unmarshal(event.Data, &response))
		for _, choice := range response.Choices {
			if choice.Delta == nil {
				continue
			}
			for _, call := range choice.Delta.ToolCalls {
				if call.ID != "" {
					calls[call.ID] = call
				}
			}
		}
	}
	require.Len(t, calls, 2)
	require.Equal(t, "spawn_agent", calls["call_spawn"].Function.Name)
	require.JSONEq(t, `{"task":"audit"}`, calls["call_spawn"].Function.Arguments)
	require.Equal(t, "wait_agent", calls["call_wait"].Function.Name)
	require.JSONEq(t, `{"ms":1}`, calls["call_wait"].Function.Arguments)
}

func TestInboundToolCallIntegrity_MissingProviderIDFailsAtEOF(t *testing.T) {
	source := streams.SliceStream([]*llm.Response{{
		Choices: []llm.Choice{{
			Index: 0,
			Delta: &llm.Message{ToolCalls: []llm.ToolCall{{
				Index: 0,
				Type:  "function",
				Function: llm.FunctionCall{
					Name:      "get_data",
					Arguments: `{}`,
				},
			}}},
		}},
	}})

	stream, err := NewInboundTransformer().TransformStream(t.Context(), source)
	require.NoError(t, err)
	_, err = streams.All(stream)
	require.ErrorIs(t, err, transformer.ErrToolCallIntegrity)
}

func collectOpenAIInboundToolCalls(
	t *testing.T,
	input []*llm.Response,
) ([]ToolCall, error) {
	t.Helper()

	stream, err := NewInboundTransformer().TransformStream(t.Context(), streams.SliceStream(input))
	require.NoError(t, err)
	events, err := streams.All(stream)
	if err != nil {
		return nil, err
	}

	var calls []ToolCall
	for _, event := range events {
		if string(event.Data) == "[DONE]" {
			continue
		}
		var response Response
		require.NoError(t, json.Unmarshal(event.Data, &response))
		for _, choice := range response.Choices {
			if choice.Delta != nil {
				calls = append(calls, choice.Delta.ToolCalls...)
			}
			if choice.Message != nil {
				calls = append(calls, choice.Message.ToolCalls...)
			}
		}
	}

	return calls, nil
}

func TestInboundToolCallIntegrity_LateIDInMessageSnapshotUsesAuthoritativePayload(t *testing.T) {
	finish := "tool_calls"
	calls, err := collectOpenAIInboundToolCalls(t, []*llm.Response{
		{
			Choices: []llm.Choice{{
				Index: 0,
				Delta: &llm.Message{ToolCalls: []llm.ToolCall{{
					Index: 0,
					Type:  "function",
					Function: llm.FunctionCall{
						Name:      "spawn_",
						Arguments: `{"task":`,
					},
				}}},
			}},
		},
		{
			Choices: []llm.Choice{{
				Index: 0,
				Message: &llm.Message{ToolCalls: []llm.ToolCall{{
					Index: 0,
					ID:    "call_snapshot",
					Type:  "function",
					Function: llm.FunctionCall{
						Name:      "spawn_agent",
						Arguments: `{"task":"audit"}`,
					},
				}}},
				FinishReason: &finish,
			}},
		},
	})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.Equal(t, "call_snapshot", calls[0].ID)
	require.Equal(t, "spawn_agent", calls[0].Function.Name)
	require.JSONEq(t, `{"task":"audit"}`, calls[0].Function.Arguments)
}

func TestInboundToolCallIntegrity_SnapshotAfterEmittedDeltaIsIdempotent(t *testing.T) {
	finish := "tool_calls"
	calls, err := collectOpenAIInboundToolCalls(t, []*llm.Response{
		{
			Choices: []llm.Choice{{
				Index: 0,
				Delta: &llm.Message{ToolCalls: []llm.ToolCall{{
					Index: 0,
					ID:    "call_1",
					Type:  "function",
					Function: llm.FunctionCall{
						Name:      "spawn_agent",
						Arguments: `{"task":"audit"}`,
					},
				}}},
			}},
		},
		{
			Choices: []llm.Choice{{
				Index: 0,
				Message: &llm.Message{ToolCalls: []llm.ToolCall{{
					Index: 0,
					ID:    "call_1",
					Type:  "function",
					Function: llm.FunctionCall{
						Name:      "spawn_agent",
						Arguments: `{"task":"audit"}`,
					},
				}}},
				FinishReason: &finish,
			}},
		},
	})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.Equal(t, "call_1", calls[0].ID)
	require.JSONEq(t, `{"task":"audit"}`, calls[0].Function.Arguments)
}

func TestInboundToolCallIntegrity_PostEmissionCorrectionsFail(t *testing.T) {
	tests := []struct {
		name              string
		emittedName       string
		snapshotName      string
		emittedArguments  string
		snapshotArguments string
	}{
		{
			name:              "name prefix extension",
			emittedName:       "spawn_",
			snapshotName:      "spawn_agent",
			emittedArguments:  `{}`,
			snapshotArguments: `{}`,
		},
		{
			name:              "empty name becomes named",
			emittedName:       "",
			snapshotName:      "spawn_agent",
			emittedArguments:  `{}`,
			snapshotArguments: `{}`,
		},
		{
			name:              "arguments differ",
			emittedName:       "spawn_agent",
			snapshotName:      "spawn_agent",
			emittedArguments:  `{"task":"old"}`,
			snapshotArguments: `{"task":"corrected"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			finish := "tool_calls"
			_, err := collectOpenAIInboundToolCalls(t, []*llm.Response{
				{
					Choices: []llm.Choice{{
						Index: 0,
						Delta: &llm.Message{ToolCalls: []llm.ToolCall{{
							Index: 0,
							ID:    "call_1",
							Type:  "function",
							Function: llm.FunctionCall{
								Name:      tt.emittedName,
								Arguments: tt.emittedArguments,
							},
						}}},
					}},
				},
				{
					Choices: []llm.Choice{{
						Index: 0,
						Message: &llm.Message{ToolCalls: []llm.ToolCall{{
							Index: 0,
							ID:    "call_1",
							Type:  "function",
							Function: llm.FunctionCall{
								Name:      tt.snapshotName,
								Arguments: tt.snapshotArguments,
							},
						}}},
						FinishReason: &finish,
					}},
				},
			})
			require.ErrorIs(t, err, transformer.ErrToolCallIntegrity)
		})
	}
}

func TestInboundToolCallIntegrity_NonPrefixCorrectedSnapshotReplacesBufferedArguments(t *testing.T) {
	finish := "tool_calls"
	calls, err := collectOpenAIInboundToolCalls(t, []*llm.Response{
		{
			Choices: []llm.Choice{{
				Index: 0,
				Delta: &llm.Message{ToolCalls: []llm.ToolCall{{
					Index: 0,
					Type:  "function",
					Function: llm.FunctionCall{
						Name:      "search",
						Arguments: `{"query":"provisional"}`,
					},
				}}},
			}},
		},
		{
			Choices: []llm.Choice{{
				Index: 0,
				Message: &llm.Message{ToolCalls: []llm.ToolCall{{
					Index: 0,
					ID:    "call_search",
					Type:  "function",
					Function: llm.FunctionCall{
						Name:      "search",
						Arguments: `{"query":"corrected"}`,
					},
				}}},
				FinishReason: &finish,
			}},
		},
	})
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.Equal(t, "call_search", calls[0].ID)
	require.JSONEq(t, `{"query":"corrected"}`, calls[0].Function.Arguments)
}

func TestInboundToolCallIntegrity_ParallelMessageSnapshotsStayPaired(t *testing.T) {
	finish := "tool_calls"
	calls, err := collectOpenAIInboundToolCalls(t, []*llm.Response{
		{
			Choices: []llm.Choice{{
				Index: 0,
				Delta: &llm.Message{ToolCalls: []llm.ToolCall{
					{
						Index: 0,
						Type:  "function",
						Function: llm.FunctionCall{
							Name:      "spawn_agent",
							Arguments: `{"task":"old"}`,
						},
					},
					{
						Index: 1,
						Type:  "function",
						Function: llm.FunctionCall{
							Name:      "wait_agent",
							Arguments: `{"ms":0}`,
						},
					},
				}},
			}},
		},
		{
			Choices: []llm.Choice{{
				Index: 0,
				Message: &llm.Message{ToolCalls: []llm.ToolCall{
					{
						Index: 0,
						ID:    "call_spawn",
						Type:  "function",
						Function: llm.FunctionCall{
							Name:      "spawn_agent",
							Arguments: `{"task":"audit"}`,
						},
					},
					{
						Index: 1,
						ID:    "call_wait",
						Type:  "function",
						Function: llm.FunctionCall{
							Name:      "wait_agent",
							Arguments: `{"ms":1}`,
						},
					},
				}},
				FinishReason: &finish,
			}},
		},
	})
	require.NoError(t, err)
	require.Len(t, calls, 2)
	require.Equal(t, "call_spawn", calls[0].ID)
	require.JSONEq(t, `{"task":"audit"}`, calls[0].Function.Arguments)
	require.Equal(t, "call_wait", calls[1].ID)
	require.JSONEq(t, `{"ms":1}`, calls[1].Function.Arguments)
}
