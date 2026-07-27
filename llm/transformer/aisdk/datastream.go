package aisdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	transformer "github.com/looplj/axonhub/llm/transformer"
)

// DataStreamTransformer implements the AI SDK Data Stream Protocol.
type DataStreamTransformer struct{}

// NewDataStreamTransformer creates a new AI SDK data stream transformer.
func NewDataStreamTransformer() *DataStreamTransformer {
	return &DataStreamTransformer{}
}

// TransformRequest transforms AI SDK request to LLM request.
func (t *DataStreamTransformer) TransformRequest(
	ctx context.Context,
	req *httpclient.Request,
) (*llm.Request, error) {
	// Parse JSON body
	var aiSDKReq Request

	err := json.Unmarshal(req.Body, &aiSDKReq)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to parse AI SDK request: %w", transformer.ErrInvalidRequest, err)
	}

	return convertToLLMRequestWithAPIFormat(&aiSDKReq, nil, llm.APIFormatAiSDKDataStream)
}

// TransformResponse transforms LLM response to AI SDK response.
func (t *DataStreamTransformer) TransformResponse(
	ctx context.Context,
	resp *llm.Response,
) (*httpclient.Response, error) {
	// For data stream protocol, we don't use non-streaming responses
	// This should not be called in streaming mode
	return nil, fmt.Errorf("data stream protocol only supports streaming responses")
}

func (t *DataStreamTransformer) AggregateStreamChunks(
	ctx context.Context,
	chunks []*httpclient.StreamEvent,
) ([]byte, llm.ResponseMeta, error) {
	// Aggregate AI SDK data stream events into a final UIMessage JSON.
	// The transformer emits JSON events per chunk (see convert_stream.go),
	// and we reconstruct high-level parts:
	// - text: aggregate between text-start/text-end
	// - reasoning: aggregate between reasoning-start/reasoning-end
	// - tool inputs: aggregate start/delta/available events by toolCallId
	type aggregatedToolInput struct {
		partIndex      int
		inputText      strings.Builder
		inputAvailable bool
	}

	var (
		result        UIMessage
		meta          llm.ResponseMeta
		currentText   strings.Builder
		textOpen      bool
		currentReason strings.Builder
		reasoningOpen bool
		parts         []UIMessagePart
		toolInputs    = make(map[string]*aggregatedToolInput)
	)

	// Always assistant for aggregated assistant output
	result.Role = "assistant"

	closeText := func() {
		if !textOpen {
			return
		}

		parts = append(parts, UIMessagePart{Type: "text", Text: currentText.String()})
		currentText.Reset()
		textOpen = false
	}

	closeReasoning := func() {
		if !reasoningOpen {
			return
		}

		parts = append(parts, UIMessagePart{Type: "reasoning", Text: currentReason.String()})
		currentReason.Reset()
		reasoningOpen = false
	}

	getToolInput := func(toolCallID, toolName string) *aggregatedToolInput {
		toolInput, ok := toolInputs[toolCallID]
		if !ok {
			// TransformStream normally closes text/reasoning before tool input starts.
			// Do the same defensively when an end event is missing so part order is kept.
			closeText()
			closeReasoning()

			part := UIMessagePart{
				Type:       "dynamic-tool",
				State:      "input-streaming",
				ToolCallID: toolCallID,
			}
			if toolName != "" {
				part.Type = "tool-" + toolName
			}

			parts = append(parts, part)
			toolInput = &aggregatedToolInput{partIndex: len(parts) - 1}
			toolInputs[toolCallID] = toolInput
		}

		if toolName != "" {
			// The AI SDK represents named tool UI parts as tool-<NAME>. toolName is
			// reserved for dynamic-tool parts and is therefore intentionally omitted.
			parts[toolInput.partIndex].Type = "tool-" + toolName
			parts[toolInput.partIndex].ToolName = ""
		}

		return toolInput
	}

	for _, ev := range chunks {
		if ev == nil || len(ev.Data) == 0 {
			continue
		}

		// Skip [DONE] marker lines
		if string(ev.Data) == "[DONE]" {
			continue
		}

		// Events produced by TransformStream (convert_stream.go) are raw JSON of StreamEvent
		var se StreamEvent
		if err := json.Unmarshal(ev.Data, &se); err != nil {
			// If it's not valid JSON (e.g., SSE formatted), skip for now
			// since current tests use JSON events.
			continue
		}

		switch se.Type {
		case "start":
			// Capture message ID
			result.ID = se.MessageID
			meta.ID = se.MessageID

		case "text-start":
			// Close any open text block defensively
			closeText()

			textOpen = true

		case "text-delta":
			if textOpen {
				currentText.WriteString(se.Delta)
			}

		case "text-end":
			closeText()

		case "reasoning-start":
			closeReasoning()

			reasoningOpen = true

		case "reasoning-delta":
			if reasoningOpen {
				currentReason.WriteString(se.Delta)
			}

		case "reasoning-end":
			closeReasoning()

		case "finish-step", "finish":
			// Nothing to aggregate; markers for UI flows.
		case "tool-input-start":
			toolInput := getToolInput(se.ToolCallID, se.ToolName)
			if !toolInput.inputAvailable {
				parts[toolInput.partIndex].State = "input-streaming"
			}

		case "tool-input-delta":
			toolInput := getToolInput(se.ToolCallID, "")
			toolInput.inputText.WriteString(se.InputTextDelta)
			if toolInput.inputAvailable {
				// A complete input event is authoritative even if a malformed stream
				// sends additional deltas afterwards.
				continue
			}

			part := &parts[toolInput.partIndex]
			part.State = "input-streaming"

			inputText := toolInput.inputText.String()
			if json.Valid([]byte(inputText)) {
				part.Input = json.RawMessage(append([]byte(nil), inputText...))
				part.InputTextDelta = ""
			} else {
				// Preserve an incomplete delta sequence for audit/debug consumers.
				// Once it becomes valid JSON, Input replaces this transitional field.
				part.Input = nil
				part.InputTextDelta = inputText
			}

		case "tool-input-available":
			toolInput := getToolInput(se.ToolCallID, se.ToolName)
			toolInput.inputAvailable = true

			part := &parts[toolInput.partIndex]
			part.State = "input-available"
			part.InputTextDelta = ""
			if len(se.Input) > 0 {
				// Keep the terminal input as raw JSON so explicit null and the exact
				// JSON value survive marshaling. It also repairs any partial delta.
				part.Input = json.RawMessage(append([]byte(nil), se.Input...))
			} else {
				part.Input = nil
			}
		default:
			// Ignore unknown types in aggregation
		}
	}

	// Close any dangling blocks
	closeText()
	closeReasoning()

	result.Parts = parts

	b, err := json.Marshal(result)
	if err != nil {
		return nil, llm.ResponseMeta{}, fmt.Errorf("failed to marshal aggregated UIMessage: %w", err)
	}

	return b, meta, nil
}

func (t *DataStreamTransformer) TransformError(ctx context.Context, rawErr error) *httpclient.Error {
	if rawErr == nil {
		return &httpclient.Error{
			StatusCode: http.StatusInternalServerError,
			Status:     http.StatusText(http.StatusInternalServerError),
			Body:       []byte(`{"message":"internal server error","type":"internal_server_error"}`),
		}
	}

	if httpErr, ok := errors.AsType[*httpclient.Error](rawErr); ok {
		return httpErr
	}

	// Handle validation errors
	if errors.Is(rawErr, transformer.ErrInvalidRequest) {
		return &httpclient.Error{
			StatusCode: http.StatusBadRequest,
			Status:     http.StatusText(http.StatusBadRequest),
			Body:       fmt.Appendf(nil, `{"message":"%s","type":"invalid_request"}`, strings.TrimPrefix(rawErr.Error(), transformer.ErrInvalidRequest.Error()+": ")),
		}
	}

	if llmErr, ok := errors.AsType[*llm.ResponseError](rawErr); ok {
		return &httpclient.Error{
			StatusCode: llmErr.StatusCode,
			Status:     http.StatusText(llmErr.StatusCode),
			Body:       fmt.Appendf(nil, `{"message":"%s","type":"%s"}`, llmErr.Detail.Message, llmErr.Detail.Type),
		}
	}

	return &httpclient.Error{
		StatusCode: http.StatusInternalServerError,
		Status:     http.StatusText(http.StatusInternalServerError),
		Body:       fmt.Appendf(nil, `{"message":"%s","type":"internal_server_error"}`, rawErr.Error()),
	}
}

// generateTextID generates a unique ID for text blocks.
func generateTextID() string {
	return "msg_" + strings.ReplaceAll(uuid.New().String(), "-", "")
}

// SetDataStreamHeaders sets the required headers for AI SDK data stream protocol.
func SetDataStreamHeaders(headers http.Header) {
	headers.Set("X-Vercel-Ai-Ui-Message-Stream", "v1")
	headers.Set("Content-Type", "text/event-stream")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Connection", "keep-alive")
}
