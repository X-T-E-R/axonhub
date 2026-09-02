package responses

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

func transformResponsesEvents(t *testing.T, events ...*httpclient.StreamEvent) []*llm.Response {
	t.Helper()
	outbound, err := NewOutboundTransformer("https://api.openai.com", "test")
	require.NoError(t, err)
	stream, err := outbound.TransformStream(t.Context(), nil, streams.SliceStream(events))
	require.NoError(t, err)
	responses, err := streams.All(stream)
	require.NoError(t, err)
	return responses
}

func terminalSemanticMessages(responses []*llm.Response) []llm.Message {
	var messages []llm.Message
	for _, response := range responses {
		if response == nil || response == llm.DoneResponse || len(response.Choices) != 1 || response.Choices[0].Delta == nil {
			continue
		}
		message := *response.Choices[0].Delta
		if message.Content.Content != nil || len(message.Content.MultipleContent) > 0 || message.Refusal != "" ||
			message.ReasoningContent != nil || message.ReasoningSignature != nil || len(message.ToolCalls) > 0 {
			messages = append(messages, message)
		}
	}
	return messages
}

func TestResponsesTerminalSnapshot_OutputItemDoneIsPrimaryAndExactOnce(t *testing.T) {
	item := `{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"terminal text"},{"type":"refusal","refusal":"terminal refusal"}]}`
	responses := transformResponsesEvents(t,
		&httpclient.StreamEvent{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":0,"item":` + item + `}`)},
		&httpclient.StreamEvent{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":7,"model":"gpt-test","status":"completed","output":[` + item + `]}}`)},
	)

	messages := terminalSemanticMessages(responses)
	require.Len(t, messages, 1)
	require.Equal(t, "terminal text", *messages[0].Content.Content)
	require.Equal(t, "terminal refusal", messages[0].Refusal)
	require.Equal(t, "msg_1", messages[0].ID)
	require.Equal(t, 1, countDoneResponses(responses))
}

func TestResponsesTerminalSnapshot_CompletedFallbackRecoversKnownOutputs(t *testing.T) {
	responses := transformResponsesEvents(t, &httpclient.StreamEvent{
		Type: "response.completed",
		Data: []byte(`{"response":{"id":"resp_fallback","object":"response","created_at":11,"model":"gpt-test","status":"completed","output":[` +
			`{"id":"reason_1","type":"reasoning","summary":[{"type":"summary_text","text":"summary "}],"reasoning_content":[{"type":"reasoning_text","text":"detail"}],"encrypted_content":"cipher"},` +
			`{"id":"image_1","type":"image_generation_call","status":"completed","result":"aW1hZ2U="}` +
			`]}}`),
	})

	messages := terminalSemanticMessages(responses)
	require.Len(t, messages, 2)
	require.Equal(t, "summary detail", *messages[0].ReasoningContent)
	require.Equal(t, "cipher", *messages[0].ReasoningSignature)
	require.Len(t, messages[1].Content.MultipleContent, 1)
	require.Equal(t, "data:image/png;base64,aW1hZ2U=", messages[1].Content.MultipleContent[0].ImageURL.URL)
	for _, response := range responses {
		if response == nil || response == llm.DoneResponse {
			continue
		}
		require.Equal(t, "resp_fallback", response.ID)
		require.Equal(t, "gpt-test", response.Model)
		require.Equal(t, int64(11), response.Created)
	}
}

func TestResponsesTerminalSnapshot_TextDeltaSuppressesOnlyTextSnapshot(t *testing.T) {
	responses := transformResponsesEvents(t,
		&httpclient.StreamEvent{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp_delta","created_at":1,"model":"gpt-test","status":"in_progress","output":[]}}`)},
		&httpclient.StreamEvent{Type: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","item_id":"msg_delta","output_index":0,"delta":"hello"}`)},
		&httpclient.StreamEvent{Type: "response.output_item.done", Data: []byte(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg_delta","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello"},{"type":"refusal","refusal":"terminal refusal"}]}}`)},
		&httpclient.StreamEvent{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_delta","created_at":1,"model":"gpt-test","status":"completed","output":[{"id":"msg_delta","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello"},{"type":"refusal","refusal":"terminal refusal"}]}]}}`)},
	)

	messages := terminalSemanticMessages(responses)
	require.Len(t, messages, 2)
	require.Equal(t, "hello", *messages[0].Content.Content)
	require.Nil(t, messages[1].Content.Content)
	require.Equal(t, "terminal refusal", messages[1].Refusal)
}

func TestResponsesTerminalSnapshot_OuterEventTypeFillsMissingDataType(t *testing.T) {
	responses := transformResponsesEvents(t, &httpclient.StreamEvent{
		Type: "response.completed",
		Data: []byte(`{"response":{"id":"resp_outer","created_at":2,"model":"gpt-test","status":"completed","output":[{"id":"msg_outer","type":"message","content":[{"type":"output_text","text":"outer type"}]}]}}`),
	})
	messages := terminalSemanticMessages(responses)
	require.Len(t, messages, 1)
	require.Equal(t, "outer type", *messages[0].Content.Content)
}

func TestResponsesTerminalSnapshot_UsageOnlyAndUnknownOutputRemainEmpty(t *testing.T) {
	responses := transformResponsesEvents(t, &httpclient.StreamEvent{Type: "response.completed", Data: []byte(
		`{"type":"response.completed","response":{"id":"resp_empty","status":"completed","output":[{"id":"unknown","type":"computer_call"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
	)})
	require.Empty(t, terminalSemanticMessages(responses))
	require.Equal(t, 1, countDoneResponses(responses))
	var usage *llm.Usage
	for _, response := range responses {
		if response != nil && response.Usage != nil {
			usage = response.Usage
		}
	}
	require.NotNil(t, usage)
	require.Equal(t, int64(3), usage.TotalTokens)
}

func TestResponsesTerminalSnapshot_ConflictingResponseTerminalFails(t *testing.T) {
	outbound, err := NewOutboundTransformer("https://api.openai.com", "test")
	require.NoError(t, err)
	stream, err := outbound.TransformStream(t.Context(), nil, streams.SliceStream([]*httpclient.StreamEvent{
		{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp_conflict","status":"completed","output":[]}}`)},
		{Type: "response.failed", Data: []byte(`{"type":"response.failed","response":{"id":"resp_conflict","status":"failed","output":[]}}`)},
	}))
	require.NoError(t, err)
	_, err = streams.All(stream)
	require.ErrorIs(t, err, transformer.ErrToolCallIntegrity)
}

func TestResponsesTerminalSnapshot_IdenticalTerminalAndDoneAreEmittedOnce(t *testing.T) {
	terminal := &httpclient.StreamEvent{Type: "response.completed", Data: []byte(
		`{"type":"response.completed","response":{"id":"resp_repeat","status":"completed","output":[{"id":"msg_repeat","type":"message","content":[{"type":"output_text","text":"once"}]}]}}`,
	)}
	responses := transformResponsesEvents(t, terminal, terminal, &httpclient.StreamEvent{Data: []byte("[DONE]")})
	require.Len(t, terminalSemanticMessages(responses), 1)
	require.Equal(t, 1, countDoneResponses(responses))
	finishCount := 0
	for _, response := range responses {
		if response != nil && response != llm.DoneResponse && len(response.Choices) == 1 && response.Choices[0].FinishReason != nil {
			finishCount++
		}
	}
	require.Equal(t, 1, finishCount)
}

func TestResponsesTerminalSnapshot_ResponseDoneAliasIsNotTerminal(t *testing.T) {
	outbound, err := NewOutboundTransformer("https://api.openai.com", "test")
	require.NoError(t, err)
	stream, err := outbound.TransformStream(t.Context(), nil, streams.SliceStream([]*httpclient.StreamEvent{{
		Type: "response.done",
		Data: []byte(`{"type":"response.done","response":{"id":"resp_alias","status":"completed","output":[{"id":"msg_alias","type":"message","content":[{"type":"output_text","text":"ignored"}]}]}}`),
	}}))
	require.NoError(t, err)
	responses, err := streams.All(stream)
	require.ErrorIs(t, err, ErrStreamIncomplete)
	require.Empty(t, terminalSemanticMessages(responses))
}

func TestResponsesTerminalSnapshot_CancellationSpellingsShareTerminalContract(t *testing.T) {
	for _, terminalType := range []string{"response.cancelled", "response.canceled"} {
		t.Run(terminalType, func(t *testing.T) {
			responses := transformResponsesEvents(t, &httpclient.StreamEvent{Type: terminalType, Data: []byte(
				`{"type":"` + terminalType + `","response":{"id":"resp_cancel","status":"canceled","output":[]}}`,
			)})
			require.Equal(t, 1, countDoneResponses(responses))
			var finishReasons []string
			for _, response := range responses {
				if response != nil && response != llm.DoneResponse && len(response.Choices) == 1 && response.Choices[0].FinishReason != nil {
					finishReasons = append(finishReasons, *response.Choices[0].FinishReason)
				}
			}
			require.Equal(t, []string{"cancelled"}, finishReasons)
		})
	}

	responses := transformResponsesEvents(t,
		&httpclient.StreamEvent{Type: "response.cancelled", Data: []byte(`{"type":"response.cancelled","response":{"id":"resp_same","status":"canceled","output":[]}}`)},
		&httpclient.StreamEvent{Type: "response.canceled", Data: []byte(`{"type":"response.canceled","response":{"id":"resp_same","status":"canceled","output":[]}}`)},
	)
	finishCount := 0
	for _, response := range responses {
		if response != nil && response != llm.DoneResponse && len(response.Choices) == 1 && response.Choices[0].FinishReason != nil {
			finishCount++
		}
	}
	require.Equal(t, 1, finishCount)
}

func TestResponsesTerminalSnapshot_CancellationConflictsWithLaterFailure(t *testing.T) {
	outbound, err := NewOutboundTransformer("https://api.openai.com", "test")
	require.NoError(t, err)
	stream, err := outbound.TransformStream(t.Context(), nil, streams.SliceStream([]*httpclient.StreamEvent{
		{Type: "response.canceled", Data: []byte(`{"type":"response.canceled","response":{"id":"resp_conflict","status":"canceled","output":[]}}`)},
		{Type: "response.failed", Data: []byte(`{"type":"response.failed","response":{"id":"resp_conflict","status":"failed","output":[]}}`)},
	}))
	require.NoError(t, err)
	_, err = streams.All(stream)
	require.ErrorIs(t, err, transformer.ErrToolCallIntegrity)
}

func countDoneResponses(responses []*llm.Response) int {
	count := 0
	for _, response := range responses {
		if response == llm.DoneResponse {
			count++
		}
	}
	return count
}
