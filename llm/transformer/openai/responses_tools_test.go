package openai

import (
	"encoding/json"
	"testing"

	"github.com/samber/lo"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/streams"
)

func TestResponsesToolBindingRejectsInvalidWrapper(t *testing.T) {
	bindings := responsesToolBindings{"emit": {Name: "emit", Custom: true}}
	for _, args := range []string{`{`, `null`, `[]`, `{}`, `{"input":null}`, `{"input":1}`, `{"input":"ok","other":true}`} {
		t.Run(args, func(t *testing.T) {
			_, err := bindings.restore(llm.ToolCall{ID: "call", Function: llm.FunctionCall{Name: "emit", Arguments: args}})
			require.Error(t, err)
		})
	}
	for _, input := range []string{"", "line1\nline2", "\"quote\"\\path", "中文 😀"} {
		args, err := json.Marshal(map[string]string{"input": input})
		require.NoError(t, err)
		call, err := bindings.restore(llm.ToolCall{ID: "call", Function: llm.FunctionCall{Name: "emit", Arguments: string(args)}})
		require.NoError(t, err)
		require.Equal(t, input, call.ResponseCustomToolCall.Input)
	}
	_, err := bindings.restore(llm.ToolCall{Function: llm.FunctionCall{Name: "emit", Arguments: `{"input":"ok"}`}})
	require.Error(t, err)
}

func TestResponsesToolStreamParallelSnapshotsAndFragments(t *testing.T) {
	bindings := responsesToolBindings{
		"emit":     {Name: "emit", Custom: true},
		"ns__echo": {Name: "echo", Namespace: "ns"},
	}
	for _, snapshots := range []bool{false, true} {
		t.Run(map[bool]string{false: "deltas", true: "snapshots"}[snapshots], func(t *testing.T) {
			calls := []llm.ToolCall{
				{ID: "a", Type: "function", Function: llm.FunctionCall{Name: "emit", Arguments: `{"input":"exact"}`}},
				{ID: "b", Type: "function", Function: llm.FunctionCall{Name: "ns__echo", Arguments: `{"value":"ok"}`}},
			}
			var chunks []*llm.Response
			if snapshots {
				chunks = append(chunks, &llm.Response{Choices: []llm.Choice{{Index: 0, Message: &llm.Message{ToolCalls: calls}, FinishReason: lo.ToPtr("tool_calls")}}})
			} else {
				calls[1].Index = 1
				first := calls[0]
				first.Function.Name = "em"
				first.Function.Arguments = `{"input":`
				chunks = append(chunks, &llm.Response{Choices: []llm.Choice{{Delta: &llm.Message{ToolCalls: []llm.ToolCall{calls[1], first}}}}})
				chunks = append(chunks, &llm.Response{Choices: []llm.Choice{{Delta: &llm.Message{ToolCalls: []llm.ToolCall{{Index: 0, Function: llm.FunctionCall{Name: "it", Arguments: `"exact"}`}}}}, FinishReason: lo.ToPtr("tool_calls")}}})
			}
			chunks = append(chunks, llm.DoneResponse)
			s := restoreResponsesToolStream(streams.SliceStream(chunks), bindings)
			var got []llm.ToolCall
			for s.Next() {
				r := s.Current()
				for _, choice := range r.Choices {
					for _, msg := range []*llm.Message{choice.Message, choice.Delta} {
						if msg != nil {
							got = append(got, msg.ToolCalls...)
						}
					}
				}
			}
			require.NoError(t, s.Err())
			require.Len(t, got, 2)
			require.Equal(t, "a", got[0].ID)
			require.Equal(t, "exact", got[0].ResponseCustomToolCall.Input)
			require.Equal(t, "b", got[1].ID)
			require.Equal(t, "ns", got[1].Function.Namespace)
			require.Equal(t, "echo", got[1].Function.Name)
		})
	}
}

func TestResponsesToolStreamUnfinishedAndIDMismatch(t *testing.T) {
	for _, done := range []bool{false, true} {
		chunks := []*llm.Response{{Choices: []llm.Choice{{Delta: &llm.Message{ToolCalls: []llm.ToolCall{{ID: "call", Type: "function", Function: llm.FunctionCall{Name: "emit", Arguments: `{"input":`}}}}}}}}
		if done {
			chunks = append(chunks, llm.DoneResponse)
		}
		s := restoreResponsesToolStream(streams.SliceStream(chunks), responsesToolBindings{"emit": {Name: "emit", Custom: true}})
		for s.Next() {
			_ = s.Current()
		}
		require.Error(t, s.Err(), "EOF and DONE must not silently discard a buffered tool call")
	}
}

func TestResponsesToolBindingDoesNotInferToolsFromText(t *testing.T) {
	resp := &llm.Response{Choices: []llm.Choice{{Message: &llm.Message{Content: llm.MessageContent{Content: lo.ToPtr("<｜｜DSML｜｜ calls>example</｜｜DSML｜｜ calls>")}}}}}
	err := (responsesToolBindings{"emit": {Name: "emit", Custom: true}}).restoreResponse(resp)
	require.NoError(t, err)
	require.Empty(t, resp.Choices[0].Message.ToolCalls)
	require.Contains(t, *resp.Choices[0].Message.Content.Content, "DSML")
}

func TestResponsesToolBindingUniqueAliasesAndExactNames(t *testing.T) {
	bindings := responsesToolBindings{"functions__exec": {Name: "exec", Namespace: "functions", Custom: true}}
	call := llm.ToolCall{ID: "call", Function: llm.FunctionCall{Name: "exec", Arguments: `{"input":"text(1)"}`}}
	restored, err := bindings.restore(call)
	require.NoError(t, err)
	require.Equal(t, "functions", restored.ResponseCustomToolCall.Namespace)
	require.Equal(t, "text(1)", restored.ResponseCustomToolCall.Input)

	bindings["editor__exec"] = responsesToolBinding{Name: "exec", Namespace: "editor", Custom: true}
	_, err = bindings.restore(call)
	require.ErrorContains(t, err, "ambiguous")

	bindings["exec"] = responsesToolBinding{Name: "exec"}
	restored, err = bindings.restore(call)
	require.NoError(t, err)
	require.Nil(t, restored.ResponseCustomToolCall, "an exact ordinary function must not be captured by a custom alias")
	require.Equal(t, "exec", restored.Function.Name)
}
