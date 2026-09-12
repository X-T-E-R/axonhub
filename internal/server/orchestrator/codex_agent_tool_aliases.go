package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"strconv"
	"sync"

	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/looplj/axonhub/llm/pipeline"
	"github.com/looplj/axonhub/llm/streams"
)

const (
	codexCollaborationNamespace     = "collaboration"
	codexCollaborationAliasBase     = "gateway_collaboration"
	codexEncryptedFunctionArgsField = "encrypted_function_args"
)

var codexPlaintextDispatchTools = map[string]struct{}{
	"spawn_agent":   {},
	"send_message":  {},
	"followup_task": {},
}

type codexAgentToolAliasBinding struct {
	namespace string
	tools     map[string]struct{}
}

func (b *codexAgentToolAliasBinding) hasTool(rawName any) bool {
	if b == nil {
		return false
	}

	name, _ := rawName.(string)
	_, ok := b.tools[name]

	return ok
}

type codexAgentToolAliasState struct {
	outbound *PersistentOutboundTransformer

	mu      sync.RWMutex
	binding *codexAgentToolAliasBinding
}

func newCodexAgentToolAliasState(outbound *PersistentOutboundTransformer) *codexAgentToolAliasState {
	return &codexAgentToolAliasState{outbound: outbound}
}

func (s *codexAgentToolAliasState) requestMiddleware() pipeline.Middleware {
	return pipeline.OnRawRequest("codex-agent-tool-alias-request", func(ctx context.Context, request *httpclient.Request) (*httpclient.Request, error) {
		channel := s.outbound.GetCurrentChannel()
		if channel == nil || channel.Settings == nil || !channel.Settings.TransformOptions.CodexAgentToolAliases ||
			request.APIFormat != string(llm.APIFormatOpenAIResponse) {
			s.setBinding(nil)

			return request, nil
		}

		s.setBinding(nil)
		body, binding, err := aliasCodexAgentTools(request.Body)
		if err != nil {
			return nil, fmt.Errorf("apply Codex agent tool aliases: %w", err)
		}
		s.setBinding(binding)
		if binding == nil {
			return request, nil
		}

		result := *request
		result.Body = body

		return &result, nil
	})
}

func (s *codexAgentToolAliasState) responseMiddleware() pipeline.Middleware {
	return pipeline.OnInboundRawResponse("codex-agent-tool-alias-response", func(_ context.Context, response *httpclient.Response) (*httpclient.Response, error) {
		binding := s.getBinding()
		if binding == nil || response == nil {
			return response, nil
		}

		body, changed := restoreCodexAgentTools(response.Body, binding)
		if !changed {
			return response, nil
		}

		result := *response
		result.Body = body

		return &result, nil
	})
}

func (s *codexAgentToolAliasState) streamMiddleware() pipeline.Middleware {
	return pipeline.OnInboundRawStream("codex-agent-tool-alias-stream", func(_ context.Context, stream streams.Stream[*httpclient.StreamEvent]) (streams.Stream[*httpclient.StreamEvent], error) {
		binding := s.getBinding()
		if binding == nil {
			return stream, nil
		}

		return &codexAgentToolAliasStream{Stream: stream, binding: binding}, nil
	})
}

func (s *codexAgentToolAliasState) setBinding(binding *codexAgentToolAliasBinding) {
	s.mu.Lock()
	s.binding = binding
	s.mu.Unlock()
}

func (s *codexAgentToolAliasState) getBinding() *codexAgentToolAliasBinding {
	s.mu.RLock()
	binding := s.binding
	s.mu.RUnlock()

	return binding
}

type codexAgentToolAliasStream struct {
	streams.Stream[*httpclient.StreamEvent]

	binding *codexAgentToolAliasBinding
	current *httpclient.StreamEvent
}

func (s *codexAgentToolAliasStream) Next() bool {
	if !s.Stream.Next() {
		return false
	}

	event := s.Stream.Current()
	if event == nil {
		s.current = nil

		return true
	}

	data, changed := restoreCodexAgentTools(event.Data, s.binding)
	if !changed {
		s.current = event

		return true
	}

	result := *event
	result.Data = data
	s.current = &result

	return true
}

func (s *codexAgentToolAliasStream) Current() *httpclient.StreamEvent {
	return s.current
}

func aliasCodexAgentTools(body []byte) ([]byte, *codexAgentToolAliasBinding, error) {
	root, err := decodeJSONMap(body)
	if err != nil {
		return nil, nil, err
	}

	aliasNamespace := availableCodexAliasNamespace(root)
	movedTools, err := aliasCodexToolCatalogs(root, aliasNamespace)
	if err != nil {
		return nil, nil, err
	}
	if len(movedTools) == 0 {
		return body, nil, nil
	}

	binding := &codexAgentToolAliasBinding{namespace: aliasNamespace, tools: movedTools}
	aliasCodexToolChoice(root["tool_choice"], binding)
	aliasCodexHistory(root["input"], binding)

	result, err := json.Marshal(root)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal request: %w", err)
	}

	return result, binding, nil
}

func decodeJSONMap(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("decode JSON body: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("decode JSON body: multiple values")
		}

		return nil, fmt.Errorf("decode JSON body: %w", err)
	}

	return root, nil
}

func availableCodexAliasNamespace(root map[string]any) string {
	used := make(map[string]struct{})
	collectNamespaceValues(root, used)
	if _, exists := used[codexCollaborationAliasBase]; !exists {
		return codexCollaborationAliasBase
	}

	for suffix := 2; ; suffix++ {
		candidate := codexCollaborationAliasBase + "_" + strconv.Itoa(suffix)
		if _, exists := used[candidate]; !exists {
			return candidate
		}
	}
}

func collectNamespaceValues(value any, namespaces map[string]struct{}) {
	switch value := value.(type) {
	case map[string]any:
		if namespace, ok := value["namespace"].(string); ok {
			namespaces[namespace] = struct{}{}
		}
		if value["type"] == "namespace" {
			if name, ok := value["name"].(string); ok {
				namespaces[name] = struct{}{}
			}
		}
		for _, child := range value {
			collectNamespaceValues(child, namespaces)
		}
	case []any:
		for _, child := range value {
			collectNamespaceValues(child, namespaces)
		}
	}
}

func aliasCodexToolCatalogs(root map[string]any, aliasNamespace string) (map[string]struct{}, error) {
	movedTools := make(map[string]struct{}, len(codexPlaintextDispatchTools))
	catalog, changed, err := aliasCodexToolCatalog(root["tools"], aliasNamespace, movedTools)
	if err != nil {
		return nil, err
	}
	if changed {
		root["tools"] = catalog
	}

	input, _ := root["input"].([]any)
	for _, rawItem := range input {
		item, _ := rawItem.(map[string]any)
		if item == nil || item["type"] != "additional_tools" {
			continue
		}

		itemCatalog, itemChanged, itemErr := aliasCodexToolCatalog(item["tools"], aliasNamespace, movedTools)
		if itemErr != nil {
			return nil, itemErr
		}
		if itemChanged {
			item["tools"] = itemCatalog
		}
	}

	return movedTools, nil
}

func aliasCodexToolCatalog(rawCatalog any, aliasNamespace string, movedTools map[string]struct{}) ([]any, bool, error) {
	catalog, ok := rawCatalog.([]any)
	if !ok {
		return nil, false, nil
	}

	changed := false
	matchedNamespace := false
	result := make([]any, 0, len(catalog)+1)
	for _, rawTool := range catalog {
		tool, _ := rawTool.(map[string]any)
		if tool == nil || tool["type"] != "namespace" || tool["name"] != codexCollaborationNamespace {
			result = append(result, rawTool)

			continue
		}

		subtools, _ := tool["tools"].([]any)
		remaining := make([]any, 0, len(subtools))
		aliased := make([]any, 0, len(codexPlaintextDispatchTools))
		seen := make(map[string]struct{})
		for _, rawSubtool := range subtools {
			subtool, _ := rawSubtool.(map[string]any)
			name, eligible := codexPlaintextDispatchToolDefinition(subtool)
			if !eligible {
				remaining = append(remaining, rawSubtool)

				continue
			}
			if _, duplicate := seen[name]; duplicate {
				return nil, false, fmt.Errorf("duplicate collaboration tool definition %q", name)
			}
			seen[name] = struct{}{}
			movedTools[name] = struct{}{}
			deleteCodexMessageEncryptedMarker(subtool)
			aliased = append(aliased, subtool)
		}

		if len(aliased) == 0 {
			result = append(result, rawTool)

			continue
		}
		if matchedNamespace {
			return nil, false, fmt.Errorf("multiple collaboration namespaces contain plaintext dispatch tools")
		}
		matchedNamespace = true

		changed = true
		aliasTool := make(map[string]any, len(tool))
		maps.Copy(aliasTool, tool)
		aliasTool["name"] = aliasNamespace
		aliasTool["tools"] = aliased

		if len(remaining) == 0 {
			result = append(result, aliasTool)
		} else {
			tool["tools"] = remaining
			result = append(result, tool, aliasTool)
		}
	}

	if !changed {
		return catalog, false, nil
	}

	return result, true, nil
}

func codexPlaintextDispatchToolDefinition(tool map[string]any) (string, bool) {
	if tool == nil || tool["type"] != "function" {
		return "", false
	}

	name, _ := tool["name"].(string)
	if _, target := codexPlaintextDispatchTools[name]; !target {
		return "", false
	}

	parameters, _ := tool["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	message, _ := properties["message"].(map[string]any)
	encrypted, _ := message["encrypted"].(bool)

	return name, encrypted
}

func deleteCodexMessageEncryptedMarker(tool map[string]any) {
	parameters, _ := tool["parameters"].(map[string]any)
	properties, _ := parameters["properties"].(map[string]any)
	message, _ := properties["message"].(map[string]any)
	delete(message, "encrypted")
}

func restoreCodexMessageEncryptedMarkers(rawTools any, binding *codexAgentToolAliasBinding) {
	tools, _ := rawTools.([]any)
	for _, rawTool := range tools {
		tool, _ := rawTool.(map[string]any)
		name, _ := tool["name"].(string)
		if tool == nil || tool["type"] != "function" {
			continue
		}
		if !binding.hasTool(name) {
			continue
		}

		parameters, _ := tool["parameters"].(map[string]any)
		properties, _ := parameters["properties"].(map[string]any)
		message, _ := properties["message"].(map[string]any)
		if message != nil {
			message["encrypted"] = true
		}
	}
}

func aliasCodexToolChoice(rawChoice any, binding *codexAgentToolAliasBinding) {
	choice, _ := rawChoice.(map[string]any)
	if choice == nil {
		return
	}

	aliasCodexCallReference(choice, binding, false)
	if choices, ok := choice["tools"].([]any); ok {
		for _, raw := range choices {
			item, _ := raw.(map[string]any)
			aliasCodexCallReference(item, binding, false)
		}
	}
}

func aliasCodexHistory(rawInput any, binding *codexAgentToolAliasBinding) {
	input, _ := rawInput.([]any)
	for _, rawItem := range input {
		item, _ := rawItem.(map[string]any)
		if item == nil || item["type"] != "function_call" {
			continue
		}
		aliasCodexCallReference(item, binding, true)
	}
}

func aliasCodexCallReference(item map[string]any, binding *codexAgentToolAliasBinding, requirePlaintext bool) {
	if item == nil || item["namespace"] != codexCollaborationNamespace || !binding.hasTool(item["name"]) {
		return
	}
	if requirePlaintext && !codexFunctionArgsArePlaintext(item) {
		return
	}

	item["namespace"] = binding.namespace
	if requirePlaintext {
		delete(item, codexEncryptedFunctionArgsField)
	}
}

func codexFunctionArgsArePlaintext(item map[string]any) bool {
	raw, exists := item[codexEncryptedFunctionArgsField]
	if !exists || raw == nil {
		return true
	}

	values, ok := raw.([]any)

	return ok && len(values) == 0
}

func restoreCodexAgentTools(body []byte, binding *codexAgentToolAliasBinding) ([]byte, bool) {
	root, err := decodeJSONMap(body)
	if err != nil {
		return body, false
	}

	changed := restoreCodexAgentValue(root, binding)
	if !changed {
		return body, false
	}

	result, err := json.Marshal(root)
	if err != nil {
		return body, false
	}

	return result, true
}

func restoreCodexAgentValue(value any, binding *codexAgentToolAliasBinding) bool {
	changed := false
	switch value := value.(type) {
	case map[string]any:
		itemType, _ := value["type"].(string)
		if itemType == "function_call" && value["namespace"] == binding.namespace && binding.hasTool(value["name"]) {
			value["namespace"] = codexCollaborationNamespace
			if codexFunctionArgsArePlaintext(value) {
				value[codexEncryptedFunctionArgsField] = []any{}
			}
			changed = true
		} else if itemType != "function_call" && value["namespace"] == binding.namespace && binding.hasTool(value["name"]) {
			value["namespace"] = codexCollaborationNamespace
			changed = true
		}

		for key, child := range value {
			if key == "tools" {
				catalog, catalogChanged := restoreCodexToolCatalog(child, binding)
				if catalogChanged {
					value[key] = catalog
					child = catalog
				}
				changed = changed || catalogChanged
			}
			if restoreCodexAgentValue(child, binding) {
				changed = true
			}
		}
	case []any:
		for _, child := range value {
			if restoreCodexAgentValue(child, binding) {
				changed = true
			}
		}
	}

	return changed
}

func restoreCodexToolCatalog(rawCatalog any, binding *codexAgentToolAliasBinding) ([]any, bool) {
	catalog, ok := rawCatalog.([]any)
	if !ok {
		return nil, false
	}

	var original map[string]any
	aliasIndexes := make([]int, 0, 1)
	for index, rawTool := range catalog {
		tool, _ := rawTool.(map[string]any)
		if tool == nil || tool["type"] != "namespace" {
			continue
		}
		switch tool["name"] {
		case codexCollaborationNamespace:
			original = tool
		case binding.namespace:
			aliasIndexes = append(aliasIndexes, index)
		}
	}
	if len(aliasIndexes) == 0 {
		return catalog, false
	}

	if original == nil && len(aliasIndexes) == 1 {
		alias, _ := catalog[aliasIndexes[0]].(map[string]any)
		alias["name"] = codexCollaborationNamespace
		restoreCodexMessageEncryptedMarkers(alias["tools"], binding)

		return catalog, true
	}
	if original == nil {
		original, _ = catalog[aliasIndexes[0]].(map[string]any)
		original["name"] = codexCollaborationNamespace
		restoreCodexMessageEncryptedMarkers(original["tools"], binding)
		aliasIndexes = aliasIndexes[1:]
	}

	originalTools, _ := original["tools"].([]any)
	remove := make(map[int]struct{}, len(aliasIndexes))
	for _, index := range aliasIndexes {
		alias, _ := catalog[index].(map[string]any)
		aliasTools, _ := alias["tools"].([]any)
		restoreCodexMessageEncryptedMarkers(aliasTools, binding)
		originalTools = append(originalTools, aliasTools...)
		remove[index] = struct{}{}
	}
	original["tools"] = originalTools
	if len(remove) > 0 {
		result := catalog[:0]
		for index, tool := range catalog {
			if _, drop := remove[index]; !drop {
				result = append(result, tool)
			}
		}
		return result, true
	}

	return catalog, true
}
