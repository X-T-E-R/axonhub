package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/internal/pkg/xjson"
	"github.com/looplj/axonhub/llm/streams"
	transformer "github.com/looplj/axonhub/llm/transformer"
)

// InboundTransformer implements transformer.Inbound for OpenAI format.
type InboundTransformer struct{}

// NewInboundTransformer creates a new OpenAI InboundTransformer.
func NewInboundTransformer() *InboundTransformer {
	return &InboundTransformer{}
}

// TransformRequest transforms HTTP request to ChatCompletionRequest.
func (t *InboundTransformer) TransformRequest(
	ctx context.Context,
	httpReq *httpclient.Request,
) (*llm.Request, error) {
	if httpReq == nil {
		return nil, fmt.Errorf("%w: http request is nil", transformer.ErrInvalidRequest)
	}

	if len(httpReq.Body) == 0 {
		return nil, fmt.Errorf("%w: request body is empty", transformer.ErrInvalidRequest)
	}

	// Check content type
	contentType := httpReq.Headers.Get("Content-Type")
	if contentType == "" {
		contentType = httpReq.Headers.Get("Content-Type")
	}

	if !strings.Contains(strings.ToLower(contentType), "application/json") {
		return nil, fmt.Errorf("%w: unsupported content type: %s", transformer.ErrInvalidRequest, contentType)
	}

	// Parse into OpenAI-specific Request type
	var oaiReq Request

	err := json.Unmarshal(httpReq.Body, &oaiReq)
	if err != nil {
		return nil, fmt.Errorf("%w: failed to decode openai request: %w", transformer.ErrInvalidRequest, err)
	}

	// Validate required fields
	if oaiReq.Model == "" {
		return nil, fmt.Errorf("%w: model is required", transformer.ErrInvalidRequest)
	}

	if len(oaiReq.Messages) == 0 {
		return nil, fmt.Errorf("%w: messages are required", transformer.ErrInvalidRequest)
	}

	// Convert to unified llm.Request
	chatReq := oaiReq.ToLLMRequest()
	chatReq.RawRequest = httpReq
	chatReq.RequestType = llm.RequestTypeChat
	chatReq.APIFormat = llm.APIFormatOpenAIChatCompletion

	return chatReq, nil
}

// TransformResponse transforms ChatCompletionResponse to Response.
func (t *InboundTransformer) TransformResponse(
	ctx context.Context,
	chatResp *llm.Response,
) (*httpclient.Response, error) {
	if chatResp == nil {
		return nil, fmt.Errorf("chat completion response is nil")
	}

	// Convert to OpenAI Response format
	oaiResp := ResponseFromLLM(chatResp)

	body, err := json.Marshal(oaiResp)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal chat completion response: %w", err)
	}

	// Create generic response
	return &httpclient.Response{
		StatusCode: http.StatusOK,
		Body:       body,
		Headers: http.Header{
			"Content-Type":  []string{"application/json"},
			"Cache-Control": []string{"no-cache"},
		},
	}, nil
}

func (t *InboundTransformer) TransformStream(
	ctx context.Context,
	stream streams.Stream[*llm.Response],
) (streams.Stream[*httpclient.StreamEvent], error) {
	return &openAIInboundStream{
		ctx:         ctx,
		transformer: t,
		source:      stream,
		toolCalls:   make(map[openAIStreamToolCallKey]*openAIStreamToolCall),
		toolCallIDs: make(map[string]openAIStreamToolCallKey),
	}, nil
}

type openAIStreamToolCallKey struct {
	choiceIndex int
	toolIndex   int
}

type openAIStreamToolCall struct {
	id          string
	accumulated *llm.ToolCall
	emitted     bool
}

type openAIInboundStream struct {
	ctx         context.Context
	transformer *InboundTransformer
	source      streams.Stream[*llm.Response]
	toolCalls   map[openAIStreamToolCallKey]*openAIStreamToolCall
	toolCallIDs map[string]openAIStreamToolCallKey
	current     *httpclient.StreamEvent
	err         error
}

func (s *openAIInboundStream) Next() bool {
	for s.source.Next() {
		chunk, err := s.prepareChunk(s.source.Current())
		if err != nil {
			s.err = err
			return false
		}
		if chunk == nil {
			continue
		}

		event, err := s.transformer.TransformStreamChunk(s.ctx, chunk)
		if err != nil {
			s.err = err
			return false
		}
		if event == nil {
			continue
		}
		s.current = event
		return true
	}

	if s.source.Err() == nil {
		for key, call := range s.toolCalls {
			if call.accumulated != nil && call.id == "" {
				s.err = fmt.Errorf(
					"%w: OpenAI tool call at choice %d index %d reached EOF without an id",
					transformer.ErrToolCallIntegrity,
					key.choiceIndex,
					key.toolIndex,
				)
				break
			}
		}
	}

	return false
}

func (s *openAIInboundStream) prepareChunk(chunk *llm.Response) (*llm.Response, error) {
	if chunk == nil {
		return nil, nil
	}
	if chunk.Object == "[DONE]" {
		if err := s.validateTerminalToolCallIDs(nil); err != nil {
			return nil, err
		}
		return chunk, nil
	}

	prepared := *chunk
	prepared.Choices = append([]llm.Choice(nil), chunk.Choices...)
	for i := range prepared.Choices {
		choice := &prepared.Choices[i]
		if choice.Delta != nil {
			delta := *choice.Delta
			toolCalls, err := s.prepareDeltaToolCalls(choice.Index, delta.ToolCalls)
			if err != nil {
				return nil, err
			}
			delta.ToolCalls = toolCalls
			choice.Delta = &delta
		}
		if choice.Message != nil {
			message := *choice.Message
			toolCalls, err := s.prepareToolCallSnapshots(choice.Index, message.ToolCalls)
			if err != nil {
				return nil, err
			}
			message.ToolCalls = toolCalls
			choice.Message = &message
		}
		if choice.FinishReason != nil {
			choiceIndex := choice.Index
			if err := s.validateTerminalToolCallIDs(&choiceIndex); err != nil {
				return nil, err
			}
		}
	}

	return &prepared, nil
}

func (s *openAIInboundStream) prepareDeltaToolCalls(
	choiceIndex int,
	toolCalls []llm.ToolCall,
) ([]llm.ToolCall, error) {
	if len(toolCalls) == 0 {
		return toolCalls, nil
	}

	prepared := make([]llm.ToolCall, 0, len(toolCalls))
	for _, toolCall := range toolCalls {
		if toolCall.ResponseCustomToolCall != nil {
			prepared = append(prepared, toolCall)
			continue
		}

		key := openAIStreamToolCallKey{
			choiceIndex: choiceIndex,
			toolIndex:   toolCall.Index,
		}
		state := s.toolCalls[key]
		if state == nil {
			state = &openAIStreamToolCall{}
			s.toolCalls[key] = state
		}

		if toolCall.ID != "" && state.id != "" && toolCall.ID != state.id {
			return nil, fmt.Errorf(
				"%w: OpenAI tool call at choice %d index %d changed id from %s to %s",
				transformer.ErrToolCallIntegrity,
				choiceIndex,
				toolCall.Index,
				state.id,
				toolCall.ID,
			)
		}
		if err := mergeOpenAIToolCallDelta(state, toolCall); err != nil {
			return nil, err
		}
		if toolCall.ID != "" {
			if err := s.bindOpenAIToolCallID(key, toolCall.ID); err != nil {
				return nil, err
			}
			state.id = toolCall.ID
			state.accumulated.ID = toolCall.ID
		}
		if state.id == "" {
			continue
		}

		if !state.emitted {
			prepared = append(prepared, *state.accumulated)
			state.emitted = true
			continue
		}
		prepared = append(prepared, toolCall)
	}

	return prepared, nil
}

func mergeOpenAIToolCallDelta(state *openAIStreamToolCall, fragment llm.ToolCall) error {
	if state.accumulated == nil {
		pending := fragment
		pending.ID = ""
		state.accumulated = &pending
		return nil
	}

	if fragment.Type != "" {
		if state.accumulated.Type != "" && state.accumulated.Type != fragment.Type {
			return fmt.Errorf(
				"%w: OpenAI tool call type changed from %s to %s",
				transformer.ErrToolCallIntegrity,
				state.accumulated.Type,
				fragment.Type,
			)
		}
		state.accumulated.Type = fragment.Type
	}
	state.accumulated.Function.Name += fragment.Function.Name
	if fragment.Function.Namespace != "" {
		if state.accumulated.Function.Namespace != "" &&
			state.accumulated.Function.Namespace != fragment.Function.Namespace {
			return fmt.Errorf(
				"%w: OpenAI tool call namespace changed",
				transformer.ErrToolCallIntegrity,
			)
		}
		state.accumulated.Function.Namespace = fragment.Function.Namespace
	}
	state.accumulated.Function.Arguments += fragment.Function.Arguments
	if fragment.CacheControl != nil {
		state.accumulated.CacheControl = fragment.CacheControl
	}
	if len(fragment.TransformerMetadata) > 0 {
		state.accumulated.TransformerMetadata = fragment.TransformerMetadata
	}

	return nil
}

func (s *openAIInboundStream) prepareToolCallSnapshots(
	choiceIndex int,
	toolCalls []llm.ToolCall,
) ([]llm.ToolCall, error) {
	if len(toolCalls) == 0 {
		return toolCalls, nil
	}

	prepared := make([]llm.ToolCall, 0, len(toolCalls))
	for _, snapshot := range toolCalls {
		if snapshot.ResponseCustomToolCall != nil {
			prepared = append(prepared, snapshot)
			continue
		}

		key := openAIStreamToolCallKey{
			choiceIndex: choiceIndex,
			toolIndex:   snapshot.Index,
		}
		state := s.toolCalls[key]
		if state == nil {
			state = &openAIStreamToolCall{}
			s.toolCalls[key] = state
		}
		if snapshot.ID != "" && state.id != "" && snapshot.ID != state.id {
			return nil, fmt.Errorf(
				"%w: OpenAI tool call at choice %d index %d changed id from %s to %s",
				transformer.ErrToolCallIntegrity,
				choiceIndex,
				snapshot.Index,
				state.id,
				snapshot.ID,
			)
		}
		if state.emitted {
			if err := validateEmittedOpenAIToolCallSnapshot(key, state, snapshot); err != nil {
				return nil, err
			}
			continue
		}

		authoritative, err := reconcileOpenAIToolCallSnapshot(state.accumulated, snapshot)
		if err != nil {
			return nil, err
		}
		if snapshot.ID != "" {
			if err := s.bindOpenAIToolCallID(key, snapshot.ID); err != nil {
				return nil, err
			}
			state.id = snapshot.ID
		}
		if state.id != "" {
			authoritative.ID = state.id
		}

		state.accumulated = &authoritative
		if state.id == "" {
			continue
		}
		prepared = append(prepared, authoritative)
		state.emitted = true
	}

	return prepared, nil
}

func validateEmittedOpenAIToolCallSnapshot(
	key openAIStreamToolCallKey,
	state *openAIStreamToolCall,
	snapshot llm.ToolCall,
) error {
	if state.accumulated == nil {
		return fmt.Errorf(
			"%w: emitted OpenAI tool call %s has no accumulated state",
			transformer.ErrToolCallIntegrity,
			state.id,
		)
	}

	emitted := state.accumulated
	if snapshot.ID != state.id ||
		snapshot.Index != emitted.Index ||
		snapshot.Type != emitted.Type ||
		snapshot.Function.Name != emitted.Function.Name ||
		snapshot.Function.Namespace != emitted.Function.Namespace ||
		snapshot.Function.Arguments != emitted.Function.Arguments {
		return fmt.Errorf(
			"%w: emitted OpenAI tool call at choice %d index %d received a corrected terminal snapshot",
			transformer.ErrToolCallIntegrity,
			key.choiceIndex,
			key.toolIndex,
		)
	}

	return nil
}

func (s *openAIInboundStream) bindOpenAIToolCallID(
	key openAIStreamToolCallKey,
	id string,
) error {
	if existing, ok := s.toolCallIDs[id]; ok && existing != key {
		return fmt.Errorf(
			"%w: OpenAI call id %s was reused for choice %d index %d",
			transformer.ErrToolCallIntegrity,
			id,
			key.choiceIndex,
			key.toolIndex,
		)
	}
	s.toolCallIDs[id] = key
	return nil
}

func reconcileOpenAIToolCallSnapshot(
	accumulated *llm.ToolCall,
	snapshot llm.ToolCall,
) (llm.ToolCall, error) {
	if accumulated == nil {
		return snapshot, nil
	}

	if accumulated.Type != "" && snapshot.Type != "" && accumulated.Type != snapshot.Type {
		return llm.ToolCall{}, fmt.Errorf(
			"%w: OpenAI tool call type changed from %s to %s",
			transformer.ErrToolCallIntegrity,
			accumulated.Type,
			snapshot.Type,
		)
	}
	if accumulated.Function.Name != "" && snapshot.Function.Name != "" &&
		accumulated.Function.Name != snapshot.Function.Name &&
		!strings.HasPrefix(snapshot.Function.Name, accumulated.Function.Name) {
		return llm.ToolCall{}, fmt.Errorf(
			"%w: OpenAI tool call name changed from %s to %s",
			transformer.ErrToolCallIntegrity,
			accumulated.Function.Name,
			snapshot.Function.Name,
		)
	}
	if accumulated.Function.Namespace != "" && snapshot.Function.Namespace != "" &&
		accumulated.Function.Namespace != snapshot.Function.Namespace {
		return llm.ToolCall{}, fmt.Errorf(
			"%w: OpenAI tool call namespace changed",
			transformer.ErrToolCallIntegrity,
		)
	}

	authoritative := snapshot
	if authoritative.Type == "" {
		authoritative.Type = accumulated.Type
	}
	if authoritative.Function.Name == "" {
		authoritative.Function.Name = accumulated.Function.Name
	}
	if authoritative.Function.Namespace == "" {
		authoritative.Function.Namespace = accumulated.Function.Namespace
	}
	if authoritative.CacheControl == nil {
		authoritative.CacheControl = accumulated.CacheControl
	}
	if len(authoritative.TransformerMetadata) == 0 {
		authoritative.TransformerMetadata = accumulated.TransformerMetadata
	}

	return authoritative, nil
}

func (s *openAIInboundStream) validateTerminalToolCallIDs(choiceIndex *int) error {
	for key, call := range s.toolCalls {
		if choiceIndex != nil && key.choiceIndex != *choiceIndex {
			continue
		}
		if call.accumulated != nil && call.id == "" {
			return fmt.Errorf(
				"%w: OpenAI tool call at choice %d index %d finished without an id",
				transformer.ErrToolCallIntegrity,
				key.choiceIndex,
				key.toolIndex,
			)
		}
	}

	return nil
}

func (s *openAIInboundStream) Current() *httpclient.StreamEvent {
	return s.current
}

func (s *openAIInboundStream) Err() error {
	if s.err != nil {
		return s.err
	}
	return s.source.Err()
}

func (s *openAIInboundStream) Close() error {
	return s.source.Close()
}

func (t *InboundTransformer) TransformStreamChunk(
	ctx context.Context,
	chatResp *llm.Response,
) (*httpclient.StreamEvent, error) {
	if chatResp == nil {
		return nil, fmt.Errorf("chat completion response is nil")
	}

	if chatResp.Object == "[DONE]" {
		return &httpclient.StreamEvent{
			Data: []byte("[DONE]"),
		}, nil
	}

	// Skip events that only contain ReasoningSignature (used by Anthropic inbound)
	// OpenAI format doesn't support ReasoningSignature in streaming
	if isReasoningSignatureEvent(chatResp) {
		//nolint:nilnil // Skip this event
		return nil, nil
	}

	// Convert to OpenAI Response format
	oaiResp := ResponseFromLLM(chatResp)

	// For OpenAI, we keep the original response format as the event data
	eventData, err := json.Marshal(oaiResp)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal chat completion response: %w", err)
	}

	return &httpclient.StreamEvent{
		Type: "",
		Data: eventData,
	}, nil
}

// isReasoningSignatureEvent checks if the response contains ONLY ReasoningSignature.
// This is a helper function to filter out reasoning signature events when transforming
// to OpenAI format, since OpenAI format doesn't support ReasoningSignature in streaming.
// If the response contains ONLY ReasoningSignature (pure signature event), we skip it.
// If the chunk also contains other content (text, reasoning_content, tool_calls, etc.),
// we should NOT skip it (e.g., thinking chunks with both signature and content).
func isReasoningSignatureEvent(resp *llm.Response) bool {
	if len(resp.Choices) != 1 {
		return false
	}

	delta := resp.Choices[0].Delta
	if delta == nil {
		return false
	}

	// Check if ReasoningSignature is set
	if delta.ReasoningSignature == nil || *delta.ReasoningSignature == "" {
		return false
	}

	// Check if there's any other content besides the signature
	hasContent := delta.Content.Content != nil || len(delta.Content.MultipleContent) > 0
	hasReasoningContent := delta.ReasoningContent != nil && *delta.ReasoningContent != ""
	hasToolCalls := len(delta.ToolCalls) > 0
	hasRefusal := delta.Refusal != ""

	// Only skip if ONLY ReasoningSignature is present (pure signature event)
	return !hasContent && !hasReasoningContent && !hasToolCalls && !hasRefusal
}

func (t *InboundTransformer) AggregateStreamChunks(
	ctx context.Context,
	chunks []*httpclient.StreamEvent,
) ([]byte, llm.ResponseMeta, error) {
	return AggregateStreamChunks(ctx, chunks, DefaultTransformChunk)
}

// TransformError transforms LLM error response to HTTP error response.
func (t *InboundTransformer) TransformError(ctx context.Context, rawErr error) *httpclient.Error {
	if rawErr == nil {
		return &httpclient.Error{
			StatusCode: http.StatusInternalServerError,
			Status:     http.StatusText(http.StatusInternalServerError),
			Body:       xjson.MustMarshal(&OpenAIError{Detail: llm.ErrorDetail{Message: "An unexpected error occurred", Type: "unexpected_error"}}),
		}
	}

	if errors.Is(rawErr, transformer.ErrInvalidModel) {
		return &httpclient.Error{
			StatusCode: http.StatusUnprocessableEntity,
			Status:     http.StatusText(http.StatusUnprocessableEntity),
			Body:       xjson.MustMarshal(&OpenAIError{Detail: llm.ErrorDetail{Message: rawErr.Error(), Type: "invalid_model_error"}}),
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
			Body:       xjson.MustMarshal(&OpenAIError{Detail: llm.ErrorDetail{Message: rawErr.Error(), Type: "invalid_request_error"}}),
		}
	}

	if llmErr, ok := errors.AsType[*llm.ResponseError](rawErr); ok {
		return &httpclient.Error{
			StatusCode: llmErr.StatusCode,
			Status:     http.StatusText(llmErr.StatusCode),
			Body:       xjson.MustMarshal(&OpenAIError{Detail: llmErr.Detail}),
		}
	}

	return &httpclient.Error{
		StatusCode: http.StatusInternalServerError,
		Status:     http.StatusText(http.StatusInternalServerError),
		Body:       xjson.MustMarshal(&OpenAIError{Detail: llm.ErrorDetail{Message: rawErr.Error(), Type: "internal_server_error"}}),
	}
}
