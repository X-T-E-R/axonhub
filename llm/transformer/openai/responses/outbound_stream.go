package responses

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/internal/pkg/xurl"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
	"github.com/looplj/axonhub/llm/transformer/shared"
)

// ErrStreamIncomplete is returned when the stream ends without a terminal event
// (response.completed, response.failed, response.cancelled, or response.incomplete).
var ErrStreamIncomplete = errors.New("stream ended without terminal event")

// TransformStream transforms OpenAI Responses API SSE events to unified llm.Response stream.
func (t *OutboundTransformer) TransformStream(
	ctx context.Context,
	req *httpclient.Request,
	stream streams.Stream[*httpclient.StreamEvent],
) (streams.Stream[*llm.Response], error) {
	// Append the DONE event to the stream
	doneEvent := lo.ToPtr(llm.DoneStreamEvent)
	streamWithDone := streams.AppendStream(stream, doneEvent)

	return streams.NoNil(newResponsesOutboundStream(streamWithDone)), nil
}

// responsesOutboundStream wraps a stream and maintains state during processing.
type responsesOutboundStream struct {
	stream streams.Stream[*httpclient.StreamEvent]
	state  *outboundStreamState

	// Event queue
	eventQueue []*llm.Response
	queueIndex int
	err        error

	// Track whether the response completed successfully
	responseCompleted bool
}

// outboundStreamState holds the state for a streaming session.
type outboundStreamState struct {
	responseID         string
	responseModel      string
	previousResponseID *string
	usage              *llm.Usage
	created            int64

	// Content accumulation
	textContent      strings.Builder
	reasoningContent strings.Builder

	// Tool call tracking
	toolCalls           map[string]*llm.ToolCall // callID -> tool call
	itemToCallID        map[string]string        // item.id -> call_id mapping
	toolCallIndex       map[string]int           // callID -> index in the output
	toolCallPayloadSent map[string]bool          // callID -> final payload emitted

	// Reasoning signature tracking
	pendingReasoningEncryptedContent map[string]*string

	// Transformer metadata tracking
	transformerMetadata        map[string]any
	transformerMetadataEmitted bool
}

func newResponsesOutboundStream(stream streams.Stream[*httpclient.StreamEvent]) *responsesOutboundStream {
	return &responsesOutboundStream{
		stream: stream,
		state: &outboundStreamState{
			toolCalls:                        make(map[string]*llm.ToolCall),
			itemToCallID:                     make(map[string]string),
			toolCallIndex:                    make(map[string]int),
			toolCallPayloadSent:              make(map[string]bool),
			pendingReasoningEncryptedContent: make(map[string]*string),
			transformerMetadata:              make(map[string]any),
		},
	}
}

func (s *responsesOutboundStream) enqueue(resp *llm.Response) {
	s.eventQueue = append(s.eventQueue, resp)
}

func jsonFieldPresent(data []byte, path ...string) bool {
	var current json.RawMessage = data
	for _, field := range path {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(current, &object); err != nil {
			return false
		}
		current = object[field]
		if len(current) == 0 || string(current) == "null" {
			return false
		}
	}

	return true
}

func responseOutputFieldPresent(data []byte, outputIndex int, field string) bool {
	var raw struct {
		Response struct {
			Output []map[string]json.RawMessage `json:"output"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &raw); err != nil || outputIndex >= len(raw.Response.Output) {
		return false
	}
	value := raw.Response.Output[outputIndex][field]

	return len(value) > 0 && string(value) != "null"
}

func (s *responsesOutboundStream) resolveToolCall(callID string, itemID *string) (string, *llm.ToolCall, bool) {
	candidates := make([]string, 0, 3)
	if callID != "" {
		candidates = append(candidates, callID)
	}
	if itemID != nil && *itemID != "" {
		if mappedCallID, ok := s.state.itemToCallID[*itemID]; ok {
			candidates = append(candidates, mappedCallID)
		}
		candidates = append(candidates, *itemID)
	}

	for _, candidate := range candidates {
		if toolCall, ok := s.state.toolCalls[candidate]; ok {
			return candidate, toolCall, true
		}
	}

	return "", nil, false
}

func (s *responsesOutboundStream) resolveOrCreateFunctionCall(
	callID string,
	itemID *string,
	name string,
	namespace string,
) (string, *llm.ToolCall, *llm.Response) {
	if resolvedCallID, toolCall, ok := s.resolveToolCall(callID, itemID); ok {
		if itemID != nil && *itemID != "" {
			s.state.itemToCallID[*itemID] = resolvedCallID
		}

		return resolvedCallID, toolCall, nil
	}

	resolvedCallID := callID
	if resolvedCallID == "" && itemID != nil {
		resolvedCallID = *itemID
	}
	if resolvedCallID == "" {
		return "", nil, nil
	}

	toolCallIdx := len(s.state.toolCalls)
	toolCall := &llm.ToolCall{
		ID:   resolvedCallID,
		Type: "function",
		Function: llm.FunctionCall{
			Name:      name,
			Namespace: namespace,
		},
	}
	s.state.toolCalls[resolvedCallID] = toolCall
	s.state.toolCallIndex[resolvedCallID] = toolCallIdx
	if itemID != nil && *itemID != "" {
		s.state.itemToCallID[*itemID] = resolvedCallID
	}

	return resolvedCallID, toolCall, s.newToolCallDelta(llm.ToolCall{
		ID:    resolvedCallID,
		Type:  "function",
		Index: toolCallIdx,
		Function: llm.FunctionCall{
			Name:      name,
			Namespace: namespace,
		},
	})
}

func (s *responsesOutboundStream) resolveOrCreateCustomToolCall(
	callID string,
	itemID *string,
	name string,
) (string, *llm.ToolCall, *llm.Response) {
	if resolvedCallID, toolCall, ok := s.resolveToolCall(callID, itemID); ok {
		if itemID != nil && *itemID != "" {
			s.state.itemToCallID[*itemID] = resolvedCallID
		}

		return resolvedCallID, toolCall, nil
	}

	resolvedCallID := callID
	if resolvedCallID == "" && itemID != nil {
		resolvedCallID = *itemID
	}
	if resolvedCallID == "" {
		return "", nil, nil
	}

	toolCallIdx := len(s.state.toolCalls)
	toolCall := &llm.ToolCall{
		ID:   resolvedCallID,
		Type: llm.ToolTypeResponsesCustomTool,
		ResponseCustomToolCall: &llm.ResponseCustomToolCall{
			CallID: resolvedCallID,
			Name:   name,
		},
	}
	s.state.toolCalls[resolvedCallID] = toolCall
	s.state.toolCallIndex[resolvedCallID] = toolCallIdx
	if itemID != nil && *itemID != "" {
		s.state.itemToCallID[*itemID] = resolvedCallID
	}

	return resolvedCallID, toolCall, s.newToolCallDelta(llm.ToolCall{
		ID:    resolvedCallID,
		Type:  llm.ToolTypeResponsesCustomTool,
		Index: toolCallIdx,
		ResponseCustomToolCall: &llm.ResponseCustomToolCall{
			CallID: resolvedCallID,
			Name:   name,
		},
	})
}

func (s *responsesOutboundStream) newToolCallDelta(toolCall llm.ToolCall) *llm.Response {
	return &llm.Response{
		Object:             "chat.completion.chunk",
		ID:                 s.state.responseID,
		Model:              s.state.responseModel,
		Created:            s.state.created,
		PreviousResponseID: s.state.previousResponseID,
		Choices: []llm.Choice{
			{
				Index: 0,
				Delta: &llm.Message{
					ToolCalls: []llm.ToolCall{toolCall},
				},
			},
		},
	}
}

func (s *responsesOutboundStream) reconcileFunctionCall(
	callID string,
	itemID *string,
	name string,
	namespace string,
	arguments string,
	argumentsPresent bool,
	finalFallback bool,
) []*llm.Response {
	resolvedCallID, toolCall, initial := s.resolveOrCreateFunctionCall(callID, itemID, name, namespace)
	if toolCall == nil || toolCall.ResponseCustomToolCall != nil {
		return nil
	}
	responses := make([]*llm.Response, 0, 2)
	if initial != nil {
		responses = append(responses, initial)
	}

	nameDelta := ""
	if toolCall.Function.Name == "" && name != "" {
		toolCall.Function.Name = name
		nameDelta = name
	}
	namespaceDelta := ""
	if toolCall.Function.Namespace == "" && namespace != "" {
		toolCall.Function.Namespace = namespace
		namespaceDelta = namespace
	}

	argumentsToEmit := ""
	if !s.state.toolCallPayloadSent[resolvedCallID] && (argumentsPresent || finalFallback) {
		if argumentsPresent {
			toolCall.Function.Arguments = arguments
		}
		argumentsToEmit = toolCall.Function.Arguments
		s.state.toolCallPayloadSent[resolvedCallID] = true
	}

	if nameDelta == "" && namespaceDelta == "" && argumentsToEmit == "" {
		return responses
	}

	return append(responses, s.newToolCallDelta(llm.ToolCall{
		Index: s.state.toolCallIndex[resolvedCallID],
		Function: llm.FunctionCall{
			Name:      nameDelta,
			Namespace: namespaceDelta,
			Arguments: argumentsToEmit,
		},
	}))
}

func (s *responsesOutboundStream) bufferFunctionCallSnapshot(
	callID string,
	itemID *string,
	name string,
	namespace string,
	arguments string,
	argumentsPresent bool,
) error {
	recoveries := s.reconcileFunctionCall(callID, itemID, name, namespace, "", false, false)
	for _, recovery := range recoveries {
		s.enqueue(recovery)
	}

	_, toolCall, ok := s.resolveToolCall(callID, itemID)
	if !ok || toolCall == nil {
		return nil
	}
	if callID != "" && toolCall.ID != "" && toolCall.ID != callID {
		return fmt.Errorf(
			"%w: item changed call id from %s to %s",
			transformer.ErrToolCallIntegrity,
			toolCall.ID,
			callID,
		)
	}
	if name != "" && toolCall.Function.Name != "" && toolCall.Function.Name != name {
		return fmt.Errorf(
			"%w: call %s changed name from %s to %s",
			transformer.ErrIncompleteToolCall,
			toolCall.ID,
			toolCall.Function.Name,
			name,
		)
	}
	if namespace != "" && toolCall.Function.Namespace != "" &&
		toolCall.Function.Namespace != namespace {
		return fmt.Errorf(
			"%w: call %s changed namespace",
			transformer.ErrIncompleteToolCall,
			toolCall.ID,
		)
	}
	if argumentsPresent {
		// Terminal payloads are authoritative snapshots. Keep the latest one
		// buffered until response.completed, where it can be emitted exactly
		// once without concatenating a provisional terminal value.
		toolCall.Function.Arguments = arguments
	}

	return nil
}

func (s *responsesOutboundStream) reconcileCustomToolCall(
	callID string,
	itemID *string,
	name string,
	input string,
	inputPresent bool,
	finalFallback bool,
) []*llm.Response {
	resolvedCallID, toolCall, initial := s.resolveOrCreateCustomToolCall(callID, itemID, name)
	if toolCall == nil || toolCall.ResponseCustomToolCall == nil {
		return nil
	}
	responses := make([]*llm.Response, 0, 2)
	if initial != nil {
		responses = append(responses, initial)
	}

	nameChanged := false
	if toolCall.ResponseCustomToolCall.Name == "" && name != "" {
		toolCall.ResponseCustomToolCall.Name = name
		nameChanged = true
	}

	inputToEmit := ""
	if !s.state.toolCallPayloadSent[resolvedCallID] && (inputPresent || finalFallback) {
		if inputPresent {
			toolCall.ResponseCustomToolCall.Input = input
		}
		inputToEmit = toolCall.ResponseCustomToolCall.Input
		s.state.toolCallPayloadSent[resolvedCallID] = true
	}

	if !nameChanged && inputToEmit == "" {
		return responses
	}

	return append(responses, s.newToolCallDelta(llm.ToolCall{
		Index: s.state.toolCallIndex[resolvedCallID],
		Type:  llm.ToolTypeResponsesCustomTool,
		ResponseCustomToolCall: &llm.ResponseCustomToolCall{
			CallID: resolvedCallID,
			Name:   toolCall.ResponseCustomToolCall.Name,
			Input:  inputToEmit,
		},
	}))
}

func (s *responsesOutboundStream) flushPendingToolCallPayloads() {
	callIDs := make([]string, 0, len(s.state.toolCalls))
	for callID := range s.state.toolCalls {
		callIDs = append(callIDs, callID)
	}
	sort.Slice(callIDs, func(i, j int) bool {
		return s.state.toolCallIndex[callIDs[i]] < s.state.toolCallIndex[callIDs[j]]
	})

	for _, callID := range callIDs {
		if s.state.toolCallPayloadSent[callID] {
			continue
		}
		s.state.toolCallPayloadSent[callID] = true

		toolCall := s.state.toolCalls[callID]
		index := s.state.toolCallIndex[callID]
		if toolCall.ResponseCustomToolCall != nil {
			if toolCall.ResponseCustomToolCall.Input == "" {
				continue
			}
			s.enqueue(s.newToolCallDelta(llm.ToolCall{
				Index: index,
				Type:  llm.ToolTypeResponsesCustomTool,
				ResponseCustomToolCall: &llm.ResponseCustomToolCall{
					CallID: callID,
					Name:   toolCall.ResponseCustomToolCall.Name,
					Input:  toolCall.ResponseCustomToolCall.Input,
				},
			}))

			continue
		}

		if toolCall.Function.Arguments == "" {
			continue
		}
		s.enqueue(s.newToolCallDelta(llm.ToolCall{
			Index: index,
			Function: llm.FunctionCall{
				Arguments: toolCall.Function.Arguments,
			},
		}))
	}
}

func (s *responsesOutboundStream) Next() bool {
	// If we have events in the queue, return them first
	if s.queueIndex < len(s.eventQueue) {
		return true
	}

	// Clear the queue and reset index for new events
	s.eventQueue = nil
	s.queueIndex = 0

	// Try to get the next chunk from source
	if !s.stream.Next() {
		// Stream ended - check if we received a terminal event
		// If not, this is an incomplete stream (e.g., upstream EOF)
		if s.err == nil && !s.responseCompleted && s.stream.Err() == nil {
			// Only set this error if we had started receiving response data
			// This distinguishes between "no response" and "incomplete response"
			if s.state.responseID != "" {
				s.err = ErrStreamIncomplete
			}
		}
		return false
	}

	event := s.stream.Current()

	err := s.transformStreamChunk(event)
	if err != nil {
		s.err = err
		return false
	}

	// Continue to the next event if no events were enqueued
	return s.Next()
}

// transformStreamChunk transforms a single OpenAI Responses API streaming chunk to unified llm.Response.
// Events are enqueued via s.enqueue() instead of being returned.
//
//nolint:maintidx,gocognit // It is complex and hard to split.
func (s *responsesOutboundStream) transformStreamChunk(event *httpclient.StreamEvent) error {
	if event == nil || len(event.Data) == 0 {
		return nil
	}

	// Handle [DONE] marker
	if string(event.Data) == "[DONE]" {
		s.enqueue(llm.DoneResponse)
		return nil
	}

	// Parse the streaming event
	var streamEvent StreamEvent

	err := json.Unmarshal(event.Data, &streamEvent)
	if err != nil {
		return fmt.Errorf("failed to unmarshal responses api stream event: %w", err)
	}

	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		slog.DebugContext(context.Background(), "received response stream event", slog.Any("event", streamEvent))
	}

	// Build base response
	resp := &llm.Response{
		Object:             "chat.completion.chunk",
		ID:                 s.state.responseID,
		Model:              s.state.responseModel,
		Created:            s.state.created,
		PreviousResponseID: s.state.previousResponseID,
	}

	//nolint:exhaustive //Only process events we care about.
	switch streamEvent.Type {
	case StreamEventTypeResponseCreated:
		if streamEvent.Response != nil {
			s.state.responseID = streamEvent.Response.ID
			s.state.responseModel = streamEvent.Response.Model
			s.state.created = streamEvent.Response.CreatedAt
			s.state.previousResponseID = streamEvent.Response.PreviousResponseID

			resp.ID = s.state.responseID
			resp.Model = s.state.responseModel
			resp.Created = s.state.created
			resp.PreviousResponseID = s.state.previousResponseID

			if streamEvent.Response.Usage != nil {
				s.state.usage = streamEvent.Response.Usage.ToUsage()
				resp.Usage = s.state.usage
			}
		}

		resp.Choices = []llm.Choice{
			{
				Index: 0,
				Delta: &llm.Message{
					Role: "assistant",
				},
			},
		}

	case StreamEventTypeResponseInProgress:
		// Update state but don't emit an event
		if streamEvent.Response != nil {
			s.state.responseID = streamEvent.Response.ID
			s.state.responseModel = streamEvent.Response.Model
			s.state.created = streamEvent.Response.CreatedAt
			s.state.previousResponseID = streamEvent.Response.PreviousResponseID

			if streamEvent.Response.Usage != nil {
				s.state.usage = streamEvent.Response.Usage.ToUsage()
			}
		}

		return nil // Intentionally skip this event
	case StreamEventTypeOutputItemAdded:
		// Output item added - check type to determine how to handle
		if streamEvent.Item == nil {
			// No item data, skip
			return nil // Intentionally skip this event
		}

		item := streamEvent.Item
		switch item.Type {
		case "reasoning":
			if item.ID == "" || item.EncryptedContent == nil || *item.EncryptedContent == "" {
				return nil // Intentionally skip this event
			}

			// Responses streams may send a provisional encrypted_content on item.added
			// and the final blob on item.done. Hold the value until item.done so the
			// final blob replaces the provisional one instead of being concatenated.
			s.state.pendingReasoningEncryptedContent[item.ID] = shared.EncodeOpenAIEncryptedContent(item.EncryptedContent)
			return nil

		case "function_call":
			if item.CallID == "" || item.ID == "" {
				return fmt.Errorf("%w: responses function call missing identity", transformer.ErrIncompleteToolCall)
			}
			if _, exists := s.state.toolCalls[item.CallID]; exists {
				return fmt.Errorf(
					"%w: duplicate responses call id %s",
					transformer.ErrIncompleteToolCall,
					item.CallID,
				)
			}
			// Initialize tool call tracking
			toolCallIdx := len(s.state.toolCalls)
			s.state.toolCalls[item.CallID] = &llm.ToolCall{
				ID:   item.CallID,
				Type: "function",
				Function: llm.FunctionCall{
					Name:      item.Name,
					Namespace: item.Namespace,
					Arguments: item.Arguments,
				},
			}
			// Map item.id to call_id for later lookup
			s.state.itemToCallID[item.ID] = item.CallID
			s.state.toolCallIndex[item.CallID] = toolCallIdx

			resp.Choices = []llm.Choice{
				{
					Index: 0,
					Delta: &llm.Message{
						ToolCalls: []llm.ToolCall{
							{
								ID:    item.CallID,
								Type:  "function",
								Index: toolCallIdx,
								Function: llm.FunctionCall{
									Name:      item.Name,
									Namespace: item.Namespace,
								},
							},
						},
					},
				},
			}

		case "custom_tool_call":
			// Custom tool call - initialize tracking, input will be streamed via delta events
			toolCallIdx := len(s.state.toolCalls)
			s.state.toolCalls[item.CallID] = &llm.ToolCall{
				ID:   item.CallID,
				Type: llm.ToolTypeResponsesCustomTool,
				ResponseCustomToolCall: &llm.ResponseCustomToolCall{
					CallID: item.CallID,
					Name:   item.Name,
					Input:  lo.FromPtr(item.Input),
				},
			}
			s.state.itemToCallID[item.ID] = item.CallID
			s.state.toolCallIndex[item.CallID] = toolCallIdx

			resp.Choices = []llm.Choice{
				{
					Index: 0,
					Delta: &llm.Message{
						ToolCalls: []llm.ToolCall{
							{
								ID:    item.CallID,
								Type:  llm.ToolTypeResponsesCustomTool,
								Index: toolCallIdx,
								ResponseCustomToolCall: &llm.ResponseCustomToolCall{
									CallID: item.CallID,
									Name:   item.Name,
								},
							},
						},
					},
				},
			}

		default:
			// For other item types (e.g., message), skip - no meaningful content to emit
			return nil // Intentionally skip this event
		}

	case StreamEventTypeFunctionCallArgumentsDelta:
		// Buffer provisional arguments locally. Only a terminal event publishes
		// the complete value into the unified append-only stream.
		if streamEvent.ItemID != nil {
			// Look up call_id from item_id mapping
			callID, ok := s.state.itemToCallID[*streamEvent.ItemID]
			if !ok {
				// Fallback: item_id might be the call_id itself
				callID = *streamEvent.ItemID
			}

			if tc, ok := s.state.toolCalls[callID]; ok {
				tc.Function.Arguments += streamEvent.Delta
			}
		}

		return nil

	case StreamEventTypeFunctionCallArgumentsDone:
		if err := s.bufferFunctionCallSnapshot(
			streamEvent.CallID,
			streamEvent.ItemID,
			streamEvent.Name,
			streamEvent.Namespace,
			streamEvent.Arguments,
			jsonFieldPresent(event.Data, "arguments"),
		); err != nil {
			return err
		}

		return nil // Intentionally skip this event

	case StreamEventTypeCustomToolCallInputDelta:
		// Buffer provisional input locally. Only a terminal event publishes the
		// complete value into the unified append-only stream.
		if streamEvent.ItemID != nil {
			callID, ok := s.state.itemToCallID[*streamEvent.ItemID]
			if !ok {
				callID = *streamEvent.ItemID
			}

			if tc, ok := s.state.toolCalls[callID]; ok {
				tc.ResponseCustomToolCall.Input += streamEvent.Delta
			}
		}

		return nil

	case StreamEventTypeCustomToolCallInputDone:
		recoveries := s.reconcileCustomToolCall(
			streamEvent.CallID,
			streamEvent.ItemID,
			streamEvent.Name,
			streamEvent.Input,
			jsonFieldPresent(event.Data, "input"),
			false,
		)
		for _, recovery := range recoveries {
			s.enqueue(recovery)
		}

		return nil // Intentionally skip this event

	case StreamEventTypeContentPartAdded:
		// Content part added - skip, no meaningful content to emit
		return nil // Intentionally skip this event

	case StreamEventTypeOutputTextDelta:
		// Text content delta
		s.state.textContent.WriteString(streamEvent.Delta)

		resp.Choices = []llm.Choice{
			{
				Index: 0,
				Delta: &llm.Message{
					Content: llm.MessageContent{
						Content: &streamEvent.Delta,
					},
				},
			},
		}

	case StreamEventTypeReasoningSummaryTextDelta:
		// Reasoning content delta
		s.state.reasoningContent.WriteString(streamEvent.Delta)

		resp.Choices = []llm.Choice{
			{
				Index: 0,
				Delta: &llm.Message{
					ReasoningContent: &streamEvent.Delta,
				},
			},
		}

	case StreamEventTypeOutputTextDone:
		// Text content completed - skip, content was already streamed via deltas
		return nil // Intentionally skip this event

	case StreamEventTypeReasoningSummaryTextDone:
		// Reasoning content completed - skip, content was already streamed via deltas
		return nil // Intentionally skip this event

	case StreamEventTypeOutputItemDone:
		if streamEvent.Item == nil {
			return nil // Intentionally skip this event
		}
		if streamEvent.Item.Type == "function_call" {
			itemID := streamEvent.Item.ID
			if err := s.bufferFunctionCallSnapshot(
				streamEvent.Item.CallID,
				&itemID,
				streamEvent.Item.Name,
				streamEvent.Item.Namespace,
				streamEvent.Item.Arguments,
				jsonFieldPresent(event.Data, "item", "arguments"),
			); err != nil {
				return err
			}

			return nil
		}
		if streamEvent.Item.Type == "custom_tool_call" {
			itemID := streamEvent.Item.ID
			if itemID != "" && streamEvent.Item.CallID != "" {
				s.state.itemToCallID[itemID] = streamEvent.Item.CallID
			}
			recoveries := s.reconcileCustomToolCall(
				streamEvent.Item.CallID,
				&itemID,
				streamEvent.Item.Name,
				lo.FromPtr(streamEvent.Item.Input),
				jsonFieldPresent(event.Data, "item", "input"),
				false,
			)
			for _, recovery := range recoveries {
				s.enqueue(recovery)
			}

			return nil
		}
		if streamEvent.Item.Type == "web_search_call" {
			appendResponseWebSearchCallMetadata(s.state.transformerMetadata, *streamEvent.Item)
			return nil // Intentionally skip this event
		}
		if streamEvent.Item.Type == "reasoning" {
			if streamEvent.Item.ID == "" {
				return nil // Intentionally skip this event
			}

			encryptedContent := shared.EncodeOpenAIEncryptedContent(streamEvent.Item.EncryptedContent)
			if encryptedContent == nil || *encryptedContent == "" {
				encryptedContent = s.state.pendingReasoningEncryptedContent[streamEvent.Item.ID]
			}
			delete(s.state.pendingReasoningEncryptedContent, streamEvent.Item.ID)
			if encryptedContent == nil || *encryptedContent == "" {
				return nil // Intentionally skip this event
			}

			resp.TransformerMetadata = map[string]any{
				responsesReasoningItemTransformerMetadataKey: map[string]any{
					"id":   streamEvent.Item.ID,
					"done": true,
				},
			}
			resp.Choices = []llm.Choice{
				{
					Index: 0,
					Delta: &llm.Message{
						ReasoningSignature: encryptedContent,
					},
				},
			}
			break
		}
		if streamEvent.Item.Type != "message" {
			return nil // Intentionally skip this event
		}

		msg := convertOutputToMessage([]Item{*streamEvent.Item}, s.state.transformerMetadata)
		if len(msg.Annotations) == 0 {
			return nil // Intentionally skip this event
		}
		if len(s.state.transformerMetadata) > 0 {
			resp.TransformerMetadata = s.state.transformerMetadata
			s.state.transformerMetadataEmitted = true
		}

		resp.Choices = []llm.Choice{
			{
				Index: 0,
				Delta: &llm.Message{
					Annotations: msg.Annotations,
				},
			},
		}

	case StreamEventTypeContentPartDone,
		StreamEventTypeReasoningSummaryPartAdded, StreamEventTypeReasoningSummaryPartDone:
		// These events don't need special handling - skip
		return nil // Intentionally skip this event

	case StreamEventTypeResponseCompleted:
		// Response completed - emit two events: one with finish_reason, one with usage
		s.responseCompleted = true
		if streamEvent.Response != nil {
			s.state.previousResponseID = streamEvent.Response.PreviousResponseID
			resp.PreviousResponseID = s.state.previousResponseID
			for i := range streamEvent.Response.Output {
				item := &streamEvent.Response.Output[i]
				if item.Type != "function_call" && item.Type != "custom_tool_call" {
					continue
				}
				itemID := item.ID
				var recoveries []*llm.Response
				if item.Type == "function_call" {
					if err := s.bufferFunctionCallSnapshot(
						item.CallID,
						&itemID,
						item.Name,
						item.Namespace,
						item.Arguments,
						responseOutputFieldPresent(event.Data, i, "arguments"),
					); err != nil {
						return err
					}
				} else {
					if item.ID != "" && item.CallID != "" {
						s.state.itemToCallID[item.ID] = item.CallID
					}
					recoveries = s.reconcileCustomToolCall(
						item.CallID,
						&itemID,
						item.Name,
						lo.FromPtr(item.Input),
						responseOutputFieldPresent(event.Data, i, "input"),
						true,
					)
				}
				for _, recovery := range recoveries {
					s.enqueue(recovery)
				}
			}
		}
		for _, toolCall := range s.state.toolCalls {
			if toolCall.ResponseCustomToolCall != nil {
				continue
			}
			if err := transformer.ValidateFunctionCall(
				toolCall.Function.Name,
				toolCall.Function.Arguments,
			); err != nil {
				return err
			}
		}
		s.flushPendingToolCallPayloads()
		if len(s.state.transformerMetadata) > 0 && !s.state.transformerMetadataEmitted {
			resp.TransformerMetadata = s.state.transformerMetadata
			s.state.transformerMetadataEmitted = true
		}

		finishReason := "stop"
		if len(s.state.toolCalls) > 0 {
			finishReason = "tool_calls"
		}

		// First event: finish_reason with empty delta
		resp.Choices = []llm.Choice{
			{
				Index:        0,
				Delta:        &llm.Message{},
				FinishReason: &finishReason,
			},
		}

		// Second event: usage (if available)
		if streamEvent.Response != nil && streamEvent.Response.Usage != nil {
			s.state.usage = streamEvent.Response.Usage.ToUsage()
			usageResp := &llm.Response{
				Object:             "chat.completion.chunk",
				ID:                 s.state.responseID,
				Model:              s.state.responseModel,
				Created:            s.state.created,
				PreviousResponseID: s.state.previousResponseID,
				Choices:            []llm.Choice{},
				Usage:              s.state.usage,
			}

			s.enqueue(resp)
			s.enqueue(usageResp)

			return nil
		}

	case StreamEventTypeResponseFailed:
		// Response failed
		s.responseCompleted = true
		finishReason := "error"
		resp.Choices = []llm.Choice{
			{
				Index:        0,
				FinishReason: &finishReason,
			},
		}

	case StreamEventTypeResponseIncomplete:
		// Response incomplete (e.g., max tokens)
		s.responseCompleted = true
		finishReason := "length"
		resp.Choices = []llm.Choice{
			{
				Index:        0,
				FinishReason: &finishReason,
			},
		}

	case StreamEventTypeResponseCancelled:
		// Response cancelled
		s.responseCompleted = true
		finishReason := "cancelled"
		resp.Choices = []llm.Choice{
			{
				Index:        0,
				FinishReason: &finishReason,
			},
		}

	case StreamEventTypeError:
		return &llm.ResponseError{
			Detail: llm.ErrorDetail{
				Code:    streamEvent.Code,
				Message: streamEvent.Message,
				Param:   lo.FromPtr(streamEvent.Param),
			},
		}

	case StreamEventTypeImageGenerationPartialImage,
		StreamEventTypeImageGenerationGenerating,
		StreamEventTypeImageGenerationInProgress,
		StreamEventTypeImageGenerationCompleted:
		// Handle image generation events
		if streamEvent.PartialImageB64 != "" {
			imageURL := xurl.BuildDataURL("image/png", streamEvent.PartialImageB64, true)
			resp.Choices = []llm.Choice{
				{
					Index: 0,
					Delta: &llm.Message{
						Content: llm.MessageContent{
							MultipleContent: []llm.MessageContentPart{
								{
									Type: "image_url",
									ImageURL: &llm.ImageURL{
										URL: imageURL,
									},
								},
							},
						},
					},
				},
			}
		} else {
			resp.Choices = []llm.Choice{
				{
					Index: 0,
					Delta: &llm.Message{},
				},
			}
		}

	default:
		// Unknown event type - skip
		return nil // Intentionally skip this event
	}

	s.enqueue(resp)

	return nil
}

func (s *responsesOutboundStream) Current() *llm.Response {
	if s.queueIndex < len(s.eventQueue) {
		event := s.eventQueue[s.queueIndex]
		s.queueIndex++

		return event
	}

	return nil
}

func (s *responsesOutboundStream) Err() error {
	if s.err != nil {
		return s.err
	}

	return s.stream.Err()
}

func (s *responsesOutboundStream) Close() error {
	return s.stream.Close()
}

// AggregateStreamChunks aggregates OpenAI Responses API streaming chunks into a complete response.
func (t *OutboundTransformer) AggregateStreamChunks(
	ctx context.Context, _ *httpclient.Request,
	chunks []*httpclient.StreamEvent,
) ([]byte, llm.ResponseMeta, error) {
	return AggregateStreamChunks(ctx, chunks)
}
