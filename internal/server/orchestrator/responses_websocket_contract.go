package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/transformer/openai/responses"
	"github.com/looplj/axonhub/llm/transformer/shared"
)

// Native WebSocket clients retain opaque Responses items. A lossy cross-format
// conversion cannot satisfy that contract, even when plain HTTP allows it.
func requireNativeResponsesWebSocketFormat(outbound *PersistentOutboundTransformer) pipeline.Middleware {
	return pipeline.OnRawRequest("responses-websocket-native-format", func(ctx context.Context, request *httpclient.Request) (*httpclient.Request, error) {
		if !shared.NativeResponsesWebSocket(ctx) {
			return request, nil
		}
		if request.APIFormat != string(llm.APIFormatOpenAIResponse) {
			return nil, &llm.ResponseError{StatusCode: http.StatusBadRequest, Detail: llm.ErrorDetail{Type: "invalid_request_error", Code: "unsupported_upstream_format", Message: "This WebSocket endpoint requires a native Responses upstream; use the ordinary HTTP endpoint for cross-format models."}}
		}
		// Prompt protection may mask canonical text. Never overwrite that policy
		// with an unmodified native body. Unprojectable edits require ordinary HTTP.
		canonical := outbound.state.LlmRequest
		if canonical != nil && canonical.RawRequest != nil {
			projection, err := responses.NewInboundTransformer().TransformRequest(ctx, canonical.RawRequest)
			if err != nil || !reflect.DeepEqual(projection.Messages, canonical.Messages) {
				return nil, &llm.ResponseError{StatusCode: http.StatusBadRequest, Detail: llm.ErrorDetail{Type: "invalid_request_error", Code: "unsupported_prompt_policy", Message: "This request's prompt transformation cannot preserve native Responses items; use the ordinary HTTP endpoint with full input."}}
			}
		}
		return request, nil
	})
}

// Preserve the native window and the exact prefix/suffix chosen by ApplyPrompts.
// Responses instructions precede input; materialize them as a system item only
// when injection needs to insert a prompt before them.
func applyNativeWSInjectedPrompts(req *llm.Request, prefix, suffix []llm.Message) error {
	if req.RawRequest == nil {
		return fmt.Errorf("native Responses prompt injection requires a raw request")
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(req.RawRequest.Body, &body); err != nil {
		return err
	}
	var input []json.RawMessage
	if err := json.Unmarshal(body["input"], &input); err != nil {
		return err
	}
	messages := make([]json.RawMessage, 0, len(prefix)+len(input)+len(suffix)+1)
	encode := func(message llm.Message) json.RawMessage {
		raw, _ := json.Marshal(map[string]any{"role": message.Role, "content": message.Content.Content})
		return raw
	}
	for _, message := range prefix {
		messages = append(messages, encode(message))
	}
	var instructions string
	if value := body["instructions"]; len(value) > 0 {
		if err := json.Unmarshal(value, &instructions); err != nil {
			return err
		}
	}
	if instructions != "" {
		raw, _ := json.Marshal(map[string]any{"role": "system", "content": instructions})
		messages = append(messages, raw)
	}
	messages = append(messages, input...)
	for _, message := range suffix {
		messages = append(messages, encode(message))
	}
	delete(body, "instructions")
	body["input"], _ = json.Marshal(messages)
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	// Clone the request: pipeline retries and the caller still own their original.
	copied := *req.RawRequest
	copied.Body = raw
	copied.JSONBody = raw
	req.RawRequest = &copied
	return nil
}
