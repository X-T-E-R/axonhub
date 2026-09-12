package openai

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/streams"
	"github.com/looplj/axonhub/llm/transformer"
)

const responsesToolsMetadataKey = "openai_chat_responses_tools"

// This map belongs to one outgoing request, not the shared channel transformer.
type responsesToolBinding struct {
	Name      string
	Namespace string
	Custom    bool
}

type responsesToolBindings map[string]responsesToolBinding

type responsesToolDefinition struct {
	Type        string                        `json:"type"`
	Name        string                        `json:"name"`
	Description string                        `json:"description"`
	Parameters  json.RawMessage               `json:"parameters"`
	Strict      *bool                         `json:"strict"`
	Tools       []responsesToolDefinition     `json:"tools"`
	Format      *llm.ResponseCustomToolFormat `json:"format"`
}

func chatToolName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "__" + name
}

func (bindings responsesToolBindings) addDefinitions(dst *Request, tools []responsesToolDefinition, namespace, namespaceDescription string, functionsRepresented bool) {
	for _, tool := range tools {
		if tool.Type == "namespace" {
			bindings.addDefinitions(dst, tool.Tools, tool.Name, strings.TrimSpace(namespaceDescription+"\n"+tool.Description), functionsRepresented)
			continue
		}
		if tool.Type != "function" && tool.Type != "custom" {
			continue
		}
		wireName := chatToolName(namespace, tool.Name)
		if namespace != "" || tool.Type == "custom" {
			bindings[wireName] = responsesToolBinding{Name: tool.Name, Namespace: namespace, Custom: tool.Type == "custom"}
		}
		if tool.Type == "function" && functionsRepresented {
			if namespaceDescription != "" {
				for i := range dst.Tools {
					if dst.Tools[i].Function.Name == wireName {
						dst.Tools[i].Function.Description = strings.TrimSpace(namespaceDescription + "\n" + dst.Tools[i].Function.Description)
					}
				}
			}
			continue
		}
		description := strings.TrimSpace(namespaceDescription + "\n" + tool.Description)
		function := Function{Name: wireName, Description: description, Parameters: tool.Parameters, Strict: tool.Strict}
		if tool.Type == "custom" {
			function.Description = strings.TrimSpace(description + "\nPass the exact freeform tool input in the input string.")
			if tool.Format != nil && tool.Format.Definition != "" {
				function.Description += "\nInput grammar (" + tool.Format.Syntax + "):\n" + tool.Format.Definition
			}
			function.Parameters = json.RawMessage(`{"type":"object","properties":{"input":{"type":"string"}},"required":["input"],"additionalProperties":false}`)
		}
		dst.Tools = append(dst.Tools, Tool{Type: "function", Function: function})
	}
}

type responsesToolStream struct {
	streams.Stream[*llm.Response]
	pending map[int]map[int]*openAIStreamToolCall
}

func (s *responsesToolStream) Err() error {
	if err := s.Stream.Err(); err != nil {
		return err
	}
	if len(s.pending) > 0 {
		return fmt.Errorf("%w: unfinished Chat tool calls", transformer.ErrIncompleteToolCall)
	}
	return nil
}

// Chat has no freeform tools. Encode their input in a function argument and
// retain the inverse mapping outside the provider JSON.
func lowerResponsesTools(src *llm.Request, dst *Request) (responsesToolBindings, error) {
	bindings := responsesToolBindings{}
	if src.ProviderExtensions != nil && src.ProviderExtensions.OpenAIResponses != nil {
		if ext := src.ProviderExtensions.OpenAIResponses.Request; ext != nil {
			for _, raw := range ext.RawTools {
				if raw.Type != "namespace" {
					continue
				}
				var ns responsesToolDefinition
				if err := json.Unmarshal(raw.Raw, &ns); err != nil {
					return nil, err
				}
				bindings.addDefinitions(dst, ns.Tools, ns.Name, ns.Description, true)
			}
			// Codex Responses Lite carries its tool catalog inside input,
			// not in the top-level tools field. Keep these raw input fragments
			// intact for native Responses upstreams; lower only at the Chat edge.
			for _, raw := range ext.RawInputItems {
				if raw.Type != "additional_tools" {
					continue
				}
				var item struct {
					Tools []responsesToolDefinition `json:"tools"`
				}
				if err := json.Unmarshal(raw.Raw, &item); err != nil {
					return nil, fmt.Errorf("%w: invalid additional_tools", transformer.ErrInvalidRequest)
				}
				bindings.addDefinitions(dst, item.Tools, "", "", false)
			}
		}
	}

	names := make(map[string]bool)
	hasCustom := slices.ContainsFunc(src.Tools, func(tool llm.Tool) bool {
		return tool.Type == llm.ToolTypeResponsesCustomTool && tool.ResponseCustomTool != nil
	})
	for _, tool := range dst.Tools {
		if names[tool.Function.Name] && (hasCustom || len(bindings) > 0) {
			return nil, fmt.Errorf("%w: duplicate Chat tool name %q", transformer.ErrInvalidRequest, tool.Function.Name)
		}
		names[tool.Function.Name] = true
	}
	for _, tool := range src.Tools {
		if tool.Type != llm.ToolTypeResponsesCustomTool || tool.ResponseCustomTool == nil {
			continue
		}
		custom := tool.ResponseCustomTool
		if names[custom.Name] {
			return nil, fmt.Errorf("%w: duplicate Chat tool name %q", transformer.ErrInvalidRequest, custom.Name)
		}
		names[custom.Name] = true
		bindings.addDefinitions(dst, []responsesToolDefinition{{
			Type: "custom", Name: custom.Name, Description: custom.Description, Format: custom.Format,
		}}, "", "", false)
	}
	// Exact top-level names must win over namespace shorthand, even when
	// another tool has the same short name.
	if len(bindings) > 0 {
		for _, tool := range dst.Tools {
			if _, ok := bindings[tool.Function.Name]; !ok {
				bindings[tool.Function.Name] = responsesToolBinding{Name: tool.Function.Name}
			}
		}
	}
	for i, message := range src.Messages {
		for j, call := range message.ToolCalls {
			if call.ResponseCustomToolCall != nil {
				custom := call.ResponseCustomToolCall
				args, err := json.Marshal(struct {
					Input string `json:"input"`
				}{custom.Input})
				if err != nil {
					return nil, err
				}
				name := chatToolName(custom.Namespace, custom.Name)
				_, resolved, _, err := bindings.resolve(name)
				if err != nil {
					return nil, err
				}
				dst.Messages[i].ToolCalls[j] = ToolCall{ID: custom.CallID, Type: "function", Index: call.Index,
					Function: FunctionCall{Name: resolved, Arguments: string(args)}}
			} else {
				name := chatToolName(call.Function.Namespace, call.Function.Name)
				_, resolved, _, err := bindings.resolve(name)
				if err != nil {
					return nil, err
				}
				dst.Messages[i].ToolCalls[j].Function.Name = resolved
			}
		}
	}
	if len(dst.Tools) > 0 {
		dst.ParallelToolCalls = src.ParallelToolCalls
	}
	if dst.ToolChoice != nil && dst.ToolChoice.NamedToolChoice != nil {
		choice := dst.ToolChoice.NamedToolChoice
		binding, name, ok, err := bindings.resolve(choice.Function.Name)
		if err != nil {
			return nil, err
		}
		choice.Function.Name = name
		if ok && binding.Custom {
			choice.Type = "function"
		}
	}
	if len(bindings) == 0 {
		return nil, nil
	}
	return bindings, nil
}

func responseBindings(req *httpclient.Request) responsesToolBindings {
	if req == nil {
		return nil
	}
	bindings, _ := req.TransformerMetadata[responsesToolsMetadataKey].(responsesToolBindings)
	return bindings
}

func (bindings responsesToolBindings) resolve(name string) (responsesToolBinding, string, bool, error) {
	if binding, ok := bindings[name]; ok {
		return binding, name, true, nil
	}
	var found responsesToolBinding
	var wireName string
	for wire, binding := range bindings {
		if binding.Name != name {
			continue
		}
		if wireName != "" {
			return responsesToolBinding{}, name, false, fmt.Errorf("%w: ambiguous tool name %q", transformer.ErrToolCallIntegrity, name)
		}
		found, wireName = binding, wire
	}
	if wireName == "" {
		return responsesToolBinding{}, name, false, nil
	}
	return found, wireName, true, nil
}

func (bindings responsesToolBindings) restore(call llm.ToolCall) (llm.ToolCall, error) {
	binding, _, ok, err := bindings.resolve(call.Function.Name)
	if err != nil {
		return call, err
	}
	if !ok {
		return call, nil
	}
	if !binding.Custom {
		call.Function.Name = binding.Name
		call.Function.Namespace = binding.Namespace
		if binding.Namespace == "collaboration" {
			switch binding.Name {
			case "spawn_agent", "send_message", "followup_task":
				// Chat arguments are plaintext. Codex requires an explicit
				// empty list to avoid wrapping the message as encrypted.
				call.Function.EncryptedFunctionArgs = []string{}
			}
		}
		return call, nil
	}
	var args map[string]json.RawMessage
	var input string
	if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil ||
		len(args) != 1 || len(args["input"]) == 0 || string(args["input"]) == "null" ||
		json.Unmarshal(args["input"], &input) != nil {
		return call, fmt.Errorf("%w: custom tool %q requires a string input argument", transformer.ErrIncompleteToolCall, binding.Name)
	}
	if call.ID == "" {
		return call, fmt.Errorf("%w: custom tool %q is missing call id", transformer.ErrIncompleteToolCall, binding.Name)
	}
	call.Type = llm.ToolTypeResponsesCustomTool
	call.ResponseCustomToolCall = &llm.ResponseCustomToolCall{CallID: call.ID, Name: binding.Name, Namespace: binding.Namespace, Input: input}
	call.Function = llm.FunctionCall{}
	return call, nil
}

func (bindings responsesToolBindings) restoreResponse(resp *llm.Response) error {
	if len(bindings) == 0 || resp == nil {
		return nil
	}
	for i := range resp.Choices {
		for _, msg := range []*llm.Message{resp.Choices[i].Message, resp.Choices[i].Delta} {
			if msg == nil {
				continue
			}
			for j, call := range msg.ToolCalls {
				restored, err := bindings.restore(call)
				if err != nil {
					return err
				}
				msg.ToolCalls[j] = restored
			}
		}
	}
	return nil
}

// Only tool arguments wait for completion: text, reasoning and usage continue
// streaming. JSON string escapes must be decoded before emitting freeform input.
func restoreResponsesToolStream(source streams.Stream[*llm.Response], bindings responsesToolBindings) streams.Stream[*llm.Response] {
	if len(bindings) == 0 {
		return source
	}
	pending := map[int]map[int]*openAIStreamToolCall{}
	mapped := streams.MapErr(source, func(resp *llm.Response) (*llm.Response, error) {
		if resp == llm.DoneResponse {
			if len(pending) > 0 {
				return nil, fmt.Errorf("%w: unfinished Chat tool calls", transformer.ErrIncompleteToolCall)
			}
			return resp, nil
		}
		for i := range resp.Choices {
			choice := &resp.Choices[i]
			for _, msg := range []*llm.Message{choice.Delta, choice.Message} {
				if msg == nil {
					continue
				}
				for position, call := range msg.ToolCalls {
					if pending[choice.Index] == nil {
						pending[choice.Index] = map[int]*openAIStreamToolCall{}
					}
					if msg == choice.Message {
						// Full Chat snapshots omit index. Match a prior delta by
						// id, otherwise use the position within this snapshot.
						call.Index = position
						for index, state := range pending[choice.Index] {
							if call.ID != "" && state.accumulated.ID == call.ID {
								call.Index = index
								break
							}
						}
					}
					state := pending[choice.Index][call.Index]
					if state == nil {
						state = &openAIStreamToolCall{accumulated: &llm.ToolCall{Index: call.Index}}
						pending[choice.Index][call.Index] = state
					}
					if call.ID != "" {
						if state.accumulated.ID != "" && state.accumulated.ID != call.ID {
							return nil, fmt.Errorf("%w: Chat tool call changed id", transformer.ErrToolCallIntegrity)
						}
						state.accumulated.ID = call.ID
					}
					if msg == choice.Message {
						copy := call
						state.accumulated = &copy
					} else if err := mergeOpenAIToolCallDelta(state, call); err != nil {
						return nil, err
					}
				}
				msg.ToolCalls = nil
			}
			if choice.FinishReason == nil || len(pending[choice.Index]) == 0 {
				continue
			}
			msg := choice.Delta
			if msg == nil {
				msg = choice.Message
			}
			if msg == nil {
				msg = &llm.Message{Role: "assistant"}
				choice.Delta = msg
			}
			indexes := make([]int, 0, len(pending[choice.Index]))
			for index := range pending[choice.Index] {
				indexes = append(indexes, index)
			}
			slices.Sort(indexes)
			for _, index := range indexes {
				call := *pending[choice.Index][index].accumulated
				restored, err := bindings.restore(call)
				if err != nil {
					return nil, err
				}
				msg.ToolCalls = append(msg.ToolCalls, restored)
			}
			delete(pending, choice.Index)
		}
		return resp, nil
	})
	return &responsesToolStream{Stream: mapped, pending: pending}
}
