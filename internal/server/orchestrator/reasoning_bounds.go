package orchestrator

import (
	"context"
	"fmt"
	"maps"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer/anthropic"
)

type reasoningWireValue struct {
	raw    string
	exists bool
}

type reasoningWireSnapshot struct {
	apiFormat string
	values    map[string]reasoningWireValue
}

const reasoningBoundsGeminiIncludeThoughtsMetadataKey = "axonhub_reasoning_bounds_gemini_include_thoughts"

func applyModelReasoningBounds(ctx context.Context, req *llm.Request, settings *objects.ModelSettings) (enforced, changed bool) {
	if req == nil || settings == nil || (settings.MinReasoningEffort == "" && settings.MaxReasoningEffort == "") {
		return false, false
	}

	effort := req.ReasoningEffort
	if effort == "" {
		if settings.MinReasoningEffort != "" {
			effort = settings.MinReasoningEffort
		} else {
			effort = settings.MaxReasoningEffort
		}
	} else {
		ordinal, ok := objects.ReasoningEffortOrdinal(effort)
		if !ok {
			log.Warn(ctx, "reasoning effort bounds skipped for custom effort",
				log.String("model", req.Model),
				log.String("reasoning_effort", effort),
				log.String("min_reasoning_effort", settings.MinReasoningEffort),
				log.String("max_reasoning_effort", settings.MaxReasoningEffort),
			)

			return false, false
		}

		if minOrdinal, ok := objects.ReasoningEffortOrdinal(settings.MinReasoningEffort); ok && ordinal < minOrdinal {
			effort = settings.MinReasoningEffort
			ordinal = minOrdinal
		}
		if maxOrdinal, ok := objects.ReasoningEffortOrdinal(settings.MaxReasoningEffort); ok && ordinal > maxOrdinal {
			effort = settings.MaxReasoningEffort
		}
	}

	captureInboundReasoningProperties(req)
	changed = effort != req.ReasoningEffort
	req.ReasoningEffort = effort
	if changed {
		req.ReasoningBudget = nil
		clearInboundReasoningOverrides(req)
		log.Debug(ctx, "applied model reasoning effort bounds",
			log.String("model", req.Model),
			log.String("reasoning_effort", effort),
			log.String("min_reasoning_effort", settings.MinReasoningEffort),
			log.String("max_reasoning_effort", settings.MaxReasoningEffort),
		)
	}

	return true, changed
}

func clearInboundReasoningOverrides(req *llm.Request) {
	if len(req.ExtraBody) > 0 {
		if extraBody, err := sjson.DeleteBytes(req.ExtraBody, "google.thinking_config"); err == nil {
			req.ExtraBody = extraBody
		}
	}

	if req.TransformerMetadata != nil {
		delete(req.TransformerMetadata, anthropic.TransformerMetadataKeyThinkingType)
		delete(req.TransformerMetadata, anthropic.TransformerMetadataKeyOutputConfigEffort)
	}
}

func captureInboundReasoningProperties(req *llm.Request) {
	if req.APIFormat == llm.APIFormatGeminiContents && req.RawRequest != nil {
		includeThoughts := gjson.GetBytes(req.RawRequest.Body, "generationConfig.thinkingConfig.includeThoughts")
		if includeThoughts.Exists() {
			setReasoningBoundsGeminiIncludeThoughts(req, includeThoughts.Bool())
		}
	}
	if len(req.ExtraBody) > 0 {
		includeThoughts := gjson.GetBytes(req.ExtraBody, "google.thinking_config.include_thoughts")
		if includeThoughts.Exists() {
			setReasoningBoundsGeminiIncludeThoughts(req, includeThoughts.Bool())
		}
	}
}

func setReasoningBoundsGeminiIncludeThoughts(req *llm.Request, includeThoughts bool) {
	if req.TransformerMetadata == nil {
		req.TransformerMetadata = make(map[string]any)
	}
	req.TransformerMetadata[reasoningBoundsGeminiIncludeThoughtsMetadataKey] = includeThoughts
}

func captureBoundedReasoningRequest(state *PersistenceState, req *llm.Request) {
	state.ReasoningBoundsEffort = req.ReasoningEffort
	if req.ReasoningBudget != nil {
		budget := *req.ReasoningBudget
		state.ReasoningBoundsBudget = &budget
	} else {
		state.ReasoningBoundsBudget = nil
	}
	state.ReasoningBoundsExtraBody = append(state.ReasoningBoundsExtraBody[:0], req.ExtraBody...)
	state.ReasoningBoundsTransformerMetadata = maps.Clone(req.TransformerMetadata)
}

func restoreBoundedReasoningRequest(state *PersistenceState, req *llm.Request) {
	if state == nil || !state.ReasoningBoundsEnforced || req == nil {
		return
	}
	req.ReasoningEffort = state.ReasoningBoundsEffort
	if state.ReasoningBoundsBudget != nil {
		budget := *state.ReasoningBoundsBudget
		req.ReasoningBudget = &budget
	} else {
		req.ReasoningBudget = nil
	}
	req.ExtraBody = append(req.ExtraBody[:0], state.ReasoningBoundsExtraBody...)
	req.TransformerMetadata = maps.Clone(state.ReasoningBoundsTransformerMetadata)
}

func captureBoundedReasoningWire(outbound *PersistentOutboundTransformer) pipeline.Middleware {
	return pipeline.OnRawRequest("capture-bounded-reasoning-wire", func(_ context.Context, request *httpclient.Request) (*httpclient.Request, error) {
		state := outbound.state
		if state == nil || !state.ReasoningBoundsEnforced {
			return request, nil
		}

		if err := restoreBoundedReasoningWireProperties(state, request); err != nil {
			return nil, err
		}

		values := make(map[string]reasoningWireValue)
		for _, path := range boundedReasoningWirePaths(request, state) {
			result := gjson.GetBytes(request.Body, path)
			values[path] = reasoningWireValue{
				raw:    result.Raw,
				exists: result.Exists() || result.Raw == "null",
			}
		}
		state.ReasoningWireSnapshot = &reasoningWireSnapshot{apiFormat: request.APIFormat, values: values}

		return request, nil
	})
}

func reconcileBoundedReasoningWire(outbound *PersistentOutboundTransformer) pipeline.Middleware {
	return pipeline.OnRawRequest("reconcile-bounded-reasoning-wire", func(_ context.Context, request *httpclient.Request) (*httpclient.Request, error) {
		state := outbound.state
		if state == nil || !state.ReasoningBoundsEnforced || state.ReasoningWireSnapshot == nil {
			return request, nil
		}

		snapshot := state.ReasoningWireSnapshot
		if snapshot.apiFormat != request.APIFormat {
			return nil, fmt.Errorf("reasoning bounds wire format changed from %q to %q", snapshot.apiFormat, request.APIFormat)
		}

		body := request.Body
		for path, value := range snapshot.values {
			var err error
			if value.exists {
				body, err = sjson.SetRawBytes(body, path, []byte(value.raw))
			} else {
				body, err = sjson.DeleteBytes(body, path)
			}
			if err != nil {
				return nil, fmt.Errorf("reconcile bounded reasoning field %q: %w", path, err)
			}
		}
		request.Body = body

		return request, nil
	})
}

func boundedReasoningWirePaths(request *httpclient.Request, state *PersistenceState) []string {
	var paths []string
	if request.Metadata["antigravity_model"] != "" {
		paths = []string{
			"request.generationConfig.thinkingConfig.thinkingLevel",
			"request.generationConfig.thinkingConfig.thinkingBudget",
		}
		return paths
	}

	//nolint:exhaustive // Only formats with a reasoning control representation participate.
	switch llm.APIFormat(request.APIFormat) {
	case llm.APIFormatOpenAIChatCompletion:
		paths = []string{
			"reasoning_effort",
			"thinking.type",
			"thinking_level",
			"extra_body.google.thinking_config.thinking_level",
		}
		if state.ReasoningBoundsChanged {
			paths = append(paths,
				"reasoning_budget",
				"thinking_budget",
				"thinking.budget_tokens",
			)
		}
		if isGeminiOpenAIReasoningWire(state) {
			paths = append(paths, "extra_body.google.thinking_config.thinking_budget")
		}
	case llm.APIFormatOpenAIResponse, llm.APIFormatOpenAIResponseCompact:
		paths = []string{"reasoning.effort"}
		if state.ReasoningBoundsChanged {
			paths = append(paths, "reasoning.max_tokens")
		}
	case llm.APIFormatAnthropicMessage:
		paths = []string{"thinking.type", "thinking.budget_tokens", "output_config.effort"}
	case llm.APIFormatGeminiContents:
		paths = []string{
			"generationConfig.thinkingConfig.thinkingLevel",
			"generationConfig.thinkingConfig.thinkingBudget",
		}
	}

	return paths
}

func restoreBoundedReasoningWireProperties(state *PersistenceState, request *httpclient.Request) error {
	includeThoughts, ok := state.ReasoningBoundsTransformerMetadata[reasoningBoundsGeminiIncludeThoughtsMetadataKey].(bool)
	if !ok {
		return nil
	}

	var path string
	switch {
	case request.Metadata["antigravity_model"] != "":
		path = "request.generationConfig.thinkingConfig.includeThoughts"
	case gjson.GetBytes(request.Body, "generationConfig.thinkingConfig").Exists():
		path = "generationConfig.thinkingConfig.includeThoughts"
	case gjson.GetBytes(request.Body, "extra_body.google.thinking_config").Exists():
		path = "extra_body.google.thinking_config.include_thoughts"
	case isGeminiOpenAIReasoningWire(state):
		path = "extra_body.google.thinking_config.include_thoughts"
	default:
		return nil
	}

	body, err := sjson.SetBytes(request.Body, path, includeThoughts)
	if err != nil {
		return fmt.Errorf("restore bounded reasoning property %q: %w", path, err)
	}
	request.Body = body

	return nil
}

func isGeminiOpenAIReasoningWire(state *PersistenceState) bool {
	return state != nil &&
		state.CurrentCandidate != nil &&
		state.CurrentCandidate.Channel != nil &&
		state.CurrentCandidate.Channel.Type == channel.TypeGeminiOpenai
}

func reconcileBoundedResponseModel(outbound *PersistentOutboundTransformer) pipeline.Middleware {
	return pipeline.OnInboundRawResponse("reconcile-bounded-response-model", func(_ context.Context, response *httpclient.Response) (*httpclient.Response, error) {
		if outbound.state == nil || !outbound.state.ReasoningBoundsActive || outbound.state.RequestedModel == "" || response == nil {
			return response, nil
		}
		if !gjson.GetBytes(response.Body, "model").Exists() {
			return response, nil
		}

		body, err := sjson.SetBytes(response.Body, "model", outbound.state.RequestedModel)
		if err != nil {
			return nil, fmt.Errorf("restore bounded response model: %w", err)
		}
		cloned := *response
		cloned.Body = body

		return &cloned, nil
	})
}

func reconcileBoundedResponseStreamModel(outbound *PersistentOutboundTransformer) pipeline.Middleware {
	return pipeline.OnInboundRawStream("reconcile-bounded-response-stream-model", func(_ context.Context, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*httpclient.StreamEvent], error) {
		if outbound.state == nil || !outbound.state.ReasoningBoundsActive || outbound.state.RequestedModel == "" {
			return stream, nil
		}

		requestedModel := outbound.state.RequestedModel
		return streams.Map(stream, func(event *httpclient.StreamEvent) *httpclient.StreamEvent {
			if event == nil {
				return event
			}
			body := event.Data
			changed := false
			for _, path := range []string{"model", "response.model", "message.model"} {
				if !gjson.GetBytes(body, path).Exists() {
					continue
				}
				var err error
				body, err = sjson.SetBytes(body, path, requestedModel)
				if err != nil {
					return event
				}
				changed = true
			}
			if !changed {
				return event
			}
			cloned := *event
			cloned.Data = body
			return &cloned
		}), nil
	})
}
