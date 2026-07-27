package aisdk

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/samber/lo"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

// TransformStream transforms LLM response stream to AI SDK data stream protocol format.
func (t *DataStreamTransformer) TransformStream(
	ctx context.Context,
	stream streams.Stream[*llm.Response],
) (streams.Stream[*httpclient.StreamEvent], error) {
	// Create a custom stream that handles the stateful transformation
	aisdkStream := &aiSDKConvertStream{
		source:              stream,
		ctx:                 ctx,
		activeToolCalls:     make(map[toolCallKey]*activeToolCall),
		activeToolCallsByID: make(map[toolCallIDKey]*activeToolCall),
	}
	doneEvent := lo.ToPtr(llm.DoneStreamEvent)
	// Append the DONE event to the filtered stream
	streamWithDone := streams.AppendStream(aisdkStream, doneEvent)

	return streams.NoNil(streamWithDone), nil
}

// aiSDKConvertStream implements the stateful stream transformation for AI SDK data stream protocol.
//
//nolint:containedctx // Checked.
type aiSDKConvertStream struct {
	source      streams.Stream[*llm.Response]
	ctx         context.Context
	hasStarted  bool
	hasFinished bool
	messageID   string
	eventQueue  []*httpclient.StreamEvent
	queueIndex  int
	err         error

	// State tracking for content blocks
	hasTextContentStarted      bool
	hasReasoningContentStarted bool
	hasToolContentStarted      bool
	currentTextID              string
	currentReasoningID         string
	activeToolCalls            map[toolCallKey]*activeToolCall
	activeToolCallsByID        map[toolCallIDKey]*activeToolCall
	activeToolCallOrder        []*activeToolCall
}

type toolCallKey struct {
	choiceIndex int
	callIndex   int
}

type toolCallIDKey struct {
	choiceIndex int
	id          string
}

type activeToolCall struct {
	toolCall         llm.ToolCall
	providerID       string
	startedEmitted   bool
	argumentsEmitted int
	availableEmitted bool
}

func (s *aiSDKConvertStream) enqueueEvent(_ string, data any) error {
	eventData, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("failed to marshal event data: %w", err)
	}

	s.eventQueue = append(s.eventQueue, &httpclient.StreamEvent{
		Type: "", // Set to empty string to avoid sending type.
		Data: eventData,
	})

	return nil
}

//nolint:maintidx // Complex stream processing logic
func (s *aiSDKConvertStream) Next() bool {
	// If we have events in the queue, return them first
	if s.queueIndex < len(s.eventQueue) {
		return true
	}

	// Clear the queue and reset index for new events
	s.eventQueue = nil
	s.queueIndex = 0

	// Try to get the next chunk from source
	if !s.source.Next() {
		if s.source.Err() == nil {
			for _, active := range s.activeToolCallOrder {
				if !active.availableEmitted {
					s.err = fmt.Errorf(
						"%w: stream ended before tool call %q completed",
						transformer.ErrToolCallIntegrity,
						active.toolCall.ID,
					)
					return false
				}
			}
		}
		return false
	}

	chunk := s.source.Current()
	if chunk == nil {
		return s.Next() // Try next chunk
	}

	// Handle [DONE] marker
	if chunk.Object == "[DONE]" {
		return s.Next() // Try next chunk
	}

	// Initialize message ID from first chunk
	if s.messageID == "" && chunk.ID != "" {
		s.messageID = chunk.ID
	}

	// Generate start event if this is the first chunk
	if !s.hasStarted {
		s.hasStarted = true

		startEvent := StreamEvent{
			Type:      "start",
			MessageID: s.messageID,
		}

		err := s.enqueueEvent("start", startEvent)
		if err != nil {
			s.err = fmt.Errorf("failed to enqueue start event: %w", err)
			return false
		}
	}

	// Process the current chunk
	if len(chunk.Choices) > 0 {
		choice := chunk.Choices[0]

		// Handle reasoning content (thinking) delta
		if choice.Delta != nil && choice.Delta.ReasoningContent != nil && *choice.Delta.ReasoningContent != "" {
			// If tool content has started, stop it first
			if s.hasToolContentStarted {
				if err := s.endToolContent(); err != nil {
					s.err = err
					return false
				}
			}

			// If text content has started, stop it first
			if s.hasTextContentStarted {
				if err := s.endTextContent(); err != nil {
					s.err = err
					return false
				}
			}

			// Start reasoning content if not already started
			if !s.hasReasoningContentStarted {
				if err := s.startReasoningContent(); err != nil {
					s.err = err
					return false
				}
			}

			// Reasoning delta
			reasoningDelta := StreamEvent{
				Type:  "reasoning-delta",
				ID:    s.currentReasoningID,
				Delta: *choice.Delta.ReasoningContent,
			}
			if err := s.enqueueEvent("reasoning-delta", reasoningDelta); err != nil {
				s.err = fmt.Errorf("failed to enqueue reasoning-delta event: %w", err)
				return false
			}
		}

		// Handle text content delta
		if choice.Delta != nil && choice.Delta.Content.Content != nil && *choice.Delta.Content.Content != "" {
			// If reasoning content has started, stop it first
			if s.hasReasoningContentStarted {
				if err := s.endReasoningContent(); err != nil {
					s.err = err
					return false
				}
			}

			// If tool content has started, stop it first
			if s.hasToolContentStarted {
				if err := s.endToolContent(); err != nil {
					s.err = err
					return false
				}
			}

			// Start text content if not already started
			if !s.hasTextContentStarted {
				if err := s.startTextContent(); err != nil {
					s.err = err
					return false
				}
			}

			// Text delta
			textDelta := StreamEvent{
				Type:  "text-delta",
				ID:    s.currentTextID,
				Delta: *choice.Delta.Content.Content,
			}
			if err := s.enqueueEvent("text-delta", textDelta); err != nil {
				s.err = fmt.Errorf("failed to enqueue text-delta event: %w", err)
				return false
			}
		}

		// Handle tool calls
		if choice.Delta != nil && len(choice.Delta.ToolCalls) > 0 {
			// If text content has started, stop it first
			if s.hasTextContentStarted {
				if err := s.endTextContent(); err != nil {
					s.err = err
					return false
				}
			}

			// If reasoning content has started, stop it first
			if s.hasReasoningContentStarted {
				if err := s.endReasoningContent(); err != nil {
					s.err = err
					return false
				}
			}

			for _, deltaToolCall := range choice.Delta.ToolCalls {
				active, _, err := s.resolveToolCall(choice.Index, deltaToolCall)
				if err != nil {
					s.err = err
					return false
				}
				if deltaToolCall.Type != "" {
					active.toolCall.Type = deltaToolCall.Type
				}

				active.toolCall.Function.Name += deltaToolCall.Function.Name

				if err := s.ensureToolInputStarted(active); err != nil {
					s.err = err
					return false
				}

				// Update arguments
				if deltaToolCall.Function.Arguments != "" {
					active.toolCall.Function.Arguments += deltaToolCall.Function.Arguments
					if err := s.ensureToolInputStarted(active); err != nil {
						s.err = err
						return false
					}
					if active.startedEmitted {
						if err := s.emitPendingToolArguments(active); err != nil {
							s.err = err
							return false
						}
					}
				}
			}
		}

		// Handle complete tool calls (tool-input-available)
		if choice.Message != nil && len(choice.Message.ToolCalls) > 0 {
			for _, toolCall := range choice.Message.ToolCalls {
				active, _, err := s.resolveToolCall(choice.Index, toolCall)
				if err != nil {
					s.err = err
					return false
				}
				if toolCall.Type != "" {
					active.toolCall.Type = toolCall.Type
				}

				if toolCall.Function.Name != "" {
					if active.toolCall.Function.Name != "" &&
						active.toolCall.Function.Name != toolCall.Function.Name {
						s.err = fmt.Errorf(
							"%w: call %s changed name from %s to %s",
							transformer.ErrToolCallIntegrity,
							active.toolCall.ID,
							active.toolCall.Function.Name,
							toolCall.Function.Name,
						)
						return false
					}
					active.toolCall.Function.Name = toolCall.Function.Name
				}

				if toolCall.Function.Arguments != "" {
					active.toolCall.Function.Arguments = toolCall.Function.Arguments
				}

				if err := s.emitToolInputAvailable(active); err != nil {
					s.err = err
					return false
				}
			}
		}

		// Handle finish reason
		if choice.FinishReason != nil && !s.hasFinished {
			s.hasFinished = true

			for _, active := range s.activeToolCallOrder {
				if err := s.emitToolInputAvailable(active); err != nil {
					s.err = err
					return false
				}
			}

			// End any active content blocks
			if s.hasTextContentStarted {
				if err := s.endTextContent(); err != nil {
					s.err = err
					return false
				}
			}

			if s.hasReasoningContentStarted {
				if err := s.endReasoningContent(); err != nil {
					s.err = err
					return false
				}
			}

			if s.hasToolContentStarted {
				if err := s.endToolContent(); err != nil {
					s.err = err
					return false
				}
			}

			// Finish message
			finish := StreamEvent{
				Type: "finish",
			}
			if err := s.enqueueEvent("finish", finish); err != nil {
				s.err = fmt.Errorf("failed to enqueue finish event: %w", err)
				return false
			}
		}
	}

	// Continue to the next event.
	return s.Next()
}

func (s *aiSDKConvertStream) resolveToolCall(
	choiceIndex int,
	delta llm.ToolCall,
) (*activeToolCall, bool, error) {
	key := toolCallKey{
		choiceIndex: choiceIndex,
		callIndex:   delta.Index,
	}

	if delta.ID != "" {
		if active, ok := s.activeToolCallsByID[toolCallIDKey{choiceIndex: choiceIndex, id: delta.ID}]; ok {
			if keyed, exists := s.activeToolCalls[key]; exists && keyed != active {
				return nil, false, fmt.Errorf(
					"%w: tool index %d conflicts with call id %s",
					transformer.ErrToolCallIntegrity,
					delta.Index,
					delta.ID,
				)
			}
			if active.toolCall.Index != delta.Index {
				return nil, false, fmt.Errorf(
					"%w: call id %s changed index from %d to %d",
					transformer.ErrToolCallIntegrity,
					delta.ID,
					active.toolCall.Index,
					delta.Index,
				)
			}
			s.activeToolCalls[key] = active

			return active, false, nil
		}
	}

	if active, ok := s.activeToolCalls[key]; ok {
		if delta.ID != "" && active.providerID != "" && active.providerID != delta.ID {
			return nil, false, fmt.Errorf(
				"%w: tool index %d changed id from %s to %s",
				transformer.ErrToolCallIntegrity,
				delta.Index,
				active.providerID,
				delta.ID,
			)
		}
		s.bindToolCallID(choiceIndex, active, delta.ID)

		return active, false, nil
	}

	active := &activeToolCall{
		toolCall: llm.ToolCall{
			ID:    delta.ID,
			Index: delta.Index,
		},
	}
	s.activeToolCalls[key] = active
	s.activeToolCallOrder = append(s.activeToolCallOrder, active)
	s.bindToolCallID(choiceIndex, active, delta.ID)

	return active, true, nil
}

func (s *aiSDKConvertStream) bindToolCallID(choiceIndex int, active *activeToolCall, providerID string) {
	if providerID == "" {
		return
	}

	active.providerID = providerID
	active.toolCall.ID = providerID
	s.activeToolCallsByID[toolCallIDKey{choiceIndex: choiceIndex, id: providerID}] = active
}

func (s *aiSDKConvertStream) ensureToolInputStarted(active *activeToolCall) error {
	if active.startedEmitted ||
		active.toolCall.ID == "" ||
		active.toolCall.Function.Name == "" {
		return nil
	}

	if !s.hasToolContentStarted {
		s.hasToolContentStarted = true
	}
	if err := s.enqueueEvent("tool-input-start", StreamEvent{
		Type:       "tool-input-start",
		ToolCallID: active.toolCall.ID,
		ToolName:   active.toolCall.Function.Name,
	}); err != nil {
		return fmt.Errorf("failed to enqueue tool-input-start event: %w", err)
	}
	active.startedEmitted = true

	return s.emitPendingToolArguments(active)
}

func (s *aiSDKConvertStream) emitPendingToolArguments(active *activeToolCall) error {
	arguments := active.toolCall.Function.Arguments
	if active.argumentsEmitted >= len(arguments) {
		return nil
	}

	delta := arguments[active.argumentsEmitted:]
	if err := s.enqueueEvent("tool-input-delta", StreamEvent{
		Type:           "tool-input-delta",
		ToolCallID:     active.toolCall.ID,
		InputTextDelta: delta,
	}); err != nil {
		return fmt.Errorf("failed to enqueue tool-input-delta event: %w", err)
	}
	active.argumentsEmitted = len(arguments)

	return nil
}

func (s *aiSDKConvertStream) emitToolInputAvailable(active *activeToolCall) error {
	if active.availableEmitted {
		return nil
	}

	if err := transformer.ValidateToolCallJSON(
		active.toolCall.Function.Name,
		active.toolCall.Function.Arguments,
	); err != nil {
		return err
	}
	if active.toolCall.ID == "" {
		return fmt.Errorf("%w: missing call id for %s", transformer.ErrIncompleteToolCall, active.toolCall.Function.Name)
	}
	if err := s.ensureToolInputStarted(active); err != nil {
		return err
	}

	toolInputAvailable := StreamEvent{
		Type:       "tool-input-available",
		ToolCallID: active.toolCall.ID,
		ToolName:   active.toolCall.Function.Name,
		Input:      json.RawMessage(active.toolCall.Function.Arguments),
	}
	if err := s.enqueueEvent("data", toolInputAvailable); err != nil {
		return fmt.Errorf("failed to enqueue tool-input-available event: %w", err)
	}

	active.availableEmitted = true

	return nil
}

func (s *aiSDKConvertStream) Current() *httpclient.StreamEvent {
	if s.queueIndex < len(s.eventQueue) {
		event := s.eventQueue[s.queueIndex]
		s.queueIndex++

		return event
	}

	return nil
}

func (s *aiSDKConvertStream) Err() error {
	if s.err != nil {
		return s.err
	}

	return s.source.Err()
}

func (s *aiSDKConvertStream) Close() error {
	return s.source.Close()
}

// Helper methods for content block lifecycle management

func (s *aiSDKConvertStream) startStep() error {
	startStep := StreamEvent{
		Type: "start-step",
	}

	return s.enqueueEvent("start-step", startStep)
}

func (s *aiSDKConvertStream) finishStep() error {
	finishStep := StreamEvent{
		Type: "finish-step",
	}

	return s.enqueueEvent("finish-step", finishStep)
}

func (s *aiSDKConvertStream) startTextContent() error {
	if err := s.startStep(); err != nil {
		return err
	}

	s.hasTextContentStarted = true
	s.currentTextID = generateID("text")

	textStart := StreamEvent{
		Type: "text-start",
		ID:   s.currentTextID,
	}

	return s.enqueueEvent("text-start", textStart)
}

func (s *aiSDKConvertStream) endTextContent() error {
	if !s.hasTextContentStarted {
		return nil
	}

	s.hasTextContentStarted = false

	textEnd := StreamEvent{
		Type: "text-end",
		ID:   s.currentTextID,
	}

	if err := s.enqueueEvent("text-end", textEnd); err != nil {
		return err
	}

	if err := s.finishStep(); err != nil {
		return err
	}

	return nil
}

func (s *aiSDKConvertStream) startReasoningContent() error {
	if err := s.startStep(); err != nil {
		return err
	}

	s.hasReasoningContentStarted = true
	s.currentReasoningID = generateID("reasoning")

	reasoningStart := StreamEvent{
		Type: "reasoning-start",
		ID:   s.currentReasoningID,
	}
	if err := s.enqueueEvent("reasoning-start", reasoningStart); err != nil {
		return err
	}

	return nil
}

func (s *aiSDKConvertStream) endReasoningContent() error {
	if !s.hasReasoningContentStarted {
		return nil
	}

	s.hasReasoningContentStarted = false

	reasoningEnd := StreamEvent{
		Type: "reasoning-end",
		ID:   s.currentReasoningID,
	}
	if err := s.enqueueEvent("reasoning-end", reasoningEnd); err != nil {
		return err
	}

	if err := s.finishStep(); err != nil {
		return err
	}

	return nil
}

func (s *aiSDKConvertStream) endToolContent() error {
	if !s.hasToolContentStarted {
		return nil
	}

	s.hasToolContentStarted = false
	// Tool content doesn't need explicit end events as they are handled per tool call

	if err := s.finishStep(); err != nil {
		return err
	}

	return nil
}

// generateID generates a unique ID with the given prefix.
func generateID(prefix string) string {
	return prefix + "_" + strings.ReplaceAll(uuid.New().String(), "-", "")
}
