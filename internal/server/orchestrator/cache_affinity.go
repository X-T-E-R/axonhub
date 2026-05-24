package orchestrator

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"unicode"

	"github.com/cespare/xxhash/v2"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/llm"
	"github.com/looplj/axonhub/llm/pipeline"
)

const channelKeyCacheAffinityVersion = 1
const (
	channelKeyCacheAffinityMaxMessages       = 32
	channelKeyCacheAffinityMaxInspectedBytes = 16 * 1024
	channelKeyCacheAffinityMaxFieldBytes     = 4096
	channelKeyCacheAffinityMinLikelyBytes    = 4096
)

type channelKeyCacheAffinityTier string

const (
	channelKeyCacheAffinityTierNone   channelKeyCacheAffinityTier = ""
	channelKeyCacheAffinityTierExact  channelKeyCacheAffinityTier = "exact"
	channelKeyCacheAffinityTierLikely channelKeyCacheAffinityTier = "likely"
)

type channelKeyCacheAffinityResult struct {
	ID   string
	Tier channelKeyCacheAffinityTier
}

type channelKeyCacheAffinitySource struct {
	Version                   int                    `json:"v"`
	Model                     string                 `json:"model,omitempty"`
	RequestType               llm.RequestType        `json:"request_type,omitempty"`
	APIFormat                 llm.APIFormat          `json:"api_format,omitempty"`
	PromptCacheKey            string                 `json:"prompt_cache_key,omitempty"`
	PreviousResponse          string                 `json:"previous_response_id,omitempty"`
	Messages                  []cacheAffinityMessage `json:"messages,omitempty"`
	ToolsFingerprint          string                 `json:"tools_fingerprint,omitempty"`
	ToolChoiceFingerprint     string                 `json:"tool_choice_fingerprint,omitempty"`
	ResponseFormatFingerprint string                 `json:"response_format_fingerprint,omitempty"`
	Compact                   *compactAffinity       `json:"compact,omitempty"`
}

type compactAffinity struct {
	PromptCacheKey string                 `json:"prompt_cache_key,omitempty"`
	Instructions   string                 `json:"instructions,omitempty"`
	Input          []cacheAffinityMessage `json:"input,omitempty"`
}

type cacheAffinityMessage struct {
	Role        string `json:"role,omitempty"`
	Name        string `json:"name,omitempty"`
	ContentHash string `json:"content_hash,omitempty"`
}

func applyChannelKeyCacheAffinity() pipeline.Middleware {
	return pipeline.OnLlmRequest("derive-channel-key-cache-affinity", func(ctx context.Context, req *llm.Request) (*llm.Request, error) {
		affinity, err := deriveChannelKeyCacheAffinity(req)
		if err != nil {
			log.Warn(ctx, "failed to derive channel key cache affinity", log.Cause(err))

			return req, nil
		}
		if affinity.ID == "" {
			return req, nil
		}

		req.ChannelKeyAffinityID = affinity.ID

		if log.DebugEnabled(ctx) {
			log.Debug(ctx, "derived channel key cache affinity",
				log.String("affinity_tier", string(affinity.Tier)),
				log.String("affinity_id_prefix", safeAffinityIDPrefix(affinity.ID)),
				log.String("model", req.Model),
			)
		}

		return req, nil
	})
}

func deriveChannelKeyCacheAffinityID(req *llm.Request) (string, error) {
	affinity, err := deriveChannelKeyCacheAffinity(req)
	if err != nil {
		return "", err
	}

	return affinity.ID, nil
}

func deriveChannelKeyCacheAffinity(req *llm.Request) (channelKeyCacheAffinityResult, error) {
	if req == nil {
		return channelKeyCacheAffinityResult{}, nil
	}

	source, tier := buildChannelKeyCacheAffinitySource(req)
	if tier == channelKeyCacheAffinityTierNone || isEmptyChannelKeyCacheAffinitySource(source) {
		return channelKeyCacheAffinityResult{}, nil
	}

	payload, err := json.Marshal(source)
	if err != nil {
		return channelKeyCacheAffinityResult{}, err
	}

	sum := xxhash.Sum64(payload)

	var buf [8]byte
	for i := 7; i >= 0; i-- {
		buf[i] = byte(sum)
		sum >>= 8
	}

	return channelKeyCacheAffinityResult{
		ID:   "cache:" + string(tier) + ":" + hex.EncodeToString(buf[:]),
		Tier: tier,
	}, nil
}

func buildChannelKeyCacheAffinitySource(req *llm.Request) (channelKeyCacheAffinitySource, channelKeyCacheAffinityTier) {
	source := channelKeyCacheAffinitySource{
		Version:     channelKeyCacheAffinityVersion,
		Model:       req.Model,
		RequestType: req.RequestType,
		APIFormat:   req.APIFormat,
	}

	if req.PromptCacheKey != nil && strings.TrimSpace(*req.PromptCacheKey) != "" {
		source.PromptCacheKey = normalizeCacheAffinityText(*req.PromptCacheKey, channelKeyCacheAffinityMaxFieldBytes)
		return source, channelKeyCacheAffinityTierExact
	}

	if req.Compact != nil && strings.TrimSpace(req.Compact.PromptCacheKey) != "" {
		source.Compact = &compactAffinity{
			PromptCacheKey: normalizeCacheAffinityText(req.Compact.PromptCacheKey, channelKeyCacheAffinityMaxFieldBytes),
		}

		return source, channelKeyCacheAffinityTierExact
	}

	if previousResponse := strings.TrimSpace(loFromPtr(req.PreviousResponseID)); previousResponse != "" {
		source.PreviousResponse = normalizeCacheAffinityText(previousResponse, channelKeyCacheAffinityMaxFieldBytes)
		return source, channelKeyCacheAffinityTierExact
	}

	var stableBytes int

	source.Messages, stableBytes = boundedCacheRelevantMessagePrefix(req.Messages)
	source.ToolsFingerprint, stableBytes = fingerprintBoundedJSONWithStableBytes(req.Tools, stableBytes)
	source.ToolChoiceFingerprint, stableBytes = fingerprintBoundedJSONWithStableBytes(req.ToolChoice, stableBytes)
	source.ResponseFormatFingerprint, stableBytes = fingerprintBoundedJSONWithStableBytes(req.ResponseFormat, stableBytes)

	if req.Compact != nil {
		compactInput, inputBytes := boundedCacheRelevantMessagePrefix(req.Compact.Input)
		instructions := normalizeCacheAffinityText(req.Compact.Instructions, channelKeyCacheAffinityMaxFieldBytes)
		stableBytes += len(instructions) + inputBytes
		source.Compact = &compactAffinity{
			Instructions: instructions,
			Input:        compactInput,
		}
	}

	if stableBytes < channelKeyCacheAffinityMinLikelyBytes {
		return channelKeyCacheAffinitySource{}, channelKeyCacheAffinityTierNone
	}

	return source, channelKeyCacheAffinityTierLikely
}

func cacheRelevantMessagePrefix(messages []llm.Message) []llm.Message {
	if len(messages) == 0 {
		return nil
	}

	lastRole := strings.ToLower(strings.TrimSpace(messages[len(messages)-1].Role))
	switch lastRole {
	case "user", "tool":
		if len(messages) == 1 {
			return nil
		}
		return messages[:len(messages)-1]
	default:
		return messages
	}
}

func boundedCacheRelevantMessagePrefix(messages []llm.Message) ([]cacheAffinityMessage, int) {
	prefix := cacheRelevantMessagePrefix(messages)
	if len(prefix) > channelKeyCacheAffinityMaxMessages {
		prefix = prefix[:channelKeyCacheAffinityMaxMessages]
	}

	result := make([]cacheAffinityMessage, 0, len(prefix))
	remaining := channelKeyCacheAffinityMaxInspectedBytes
	stableBytes := 0
	for _, msg := range prefix {
		if remaining <= 0 {
			break
		}

		content := normalizeCacheAffinityText(messageTextForAffinity(msg), min(channelKeyCacheAffinityMaxFieldBytes, remaining))
		remaining -= len(content)
		stableBytes += len(content)
		name := ""
		if msg.Name != nil {
			name = normalizeCacheAffinityText(*msg.Name, 256)
		}
		result = append(result, cacheAffinityMessage{
			Role:        strings.ToLower(strings.TrimSpace(msg.Role)),
			Name:        name,
			ContentHash: fingerprintString(content),
		})
	}

	return result, stableBytes
}

func messageTextForAffinity(msg llm.Message) string {
	if msg.Content.Content != nil {
		return *msg.Content.Content
	}

	var parts []string
	for _, part := range msg.Content.MultipleContent {
		if part.Text != nil {
			parts = append(parts, *part.Text)
		}
		if part.Compact != nil {
			parts = append(parts, part.Compact.ID)
			if part.Compact.CreatedBy != nil {
				parts = append(parts, *part.Compact.CreatedBy)
			}
		}
	}

	return strings.Join(parts, "\n")
}

func normalizeCacheAffinityText(value string, maxBytes int) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return ' '
		}
		return r
	}, strings.TrimSpace(value))
	value = strings.Join(strings.Fields(value), " ")
	if maxBytes > 0 && len(value) > maxBytes {
		value = value[:maxBytes]
	}

	return value
}

func fingerprintBoundedJSON(value any) string {
	fingerprint, _ := fingerprintBoundedJSONWithStableBytes(value, 0)

	return fingerprint
}

func fingerprintBoundedJSONWithStableBytes(value any, stableBytes int) (string, int) {
	if value == nil {
		return "", stableBytes
	}
	if isEmptyCacheAffinityValue(value) {
		return "", stableBytes
	}

	data, err := json.Marshal(value)
	if err != nil {
		return "", stableBytes
	}
	if len(data) > channelKeyCacheAffinityMaxFieldBytes {
		data = data[:channelKeyCacheAffinityMaxFieldBytes]
	}

	return fingerprintBytes(data), stableBytes + len(data)
}

func isEmptyCacheAffinityValue(value any) bool {
	v := reflect.ValueOf(value)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if v.IsNil() {
			return true
		}
	}
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	default:
		return false
	}
}

func fingerprintString(value string) string {
	if value == "" {
		return ""
	}

	return fingerprintBytes([]byte(value))
}

func fingerprintBytes(value []byte) string {
	sum := xxhash.Sum64(value)
	var buf [8]byte
	for i := 7; i >= 0; i-- {
		buf[i] = byte(sum)
		sum >>= 8
	}

	return hex.EncodeToString(buf[:])
}

func loFromPtr(value *string) string {
	if value == nil {
		return ""
	}

	return *value
}

func safeAffinityIDPrefix(id string) string {
	if len(id) >= 18 {
		return id[:18]
	}

	return id
}

func isEmptyChannelKeyCacheAffinitySource(source channelKeyCacheAffinitySource) bool {
	if source.PromptCacheKey != "" {
		return false
	}
	if source.PreviousResponse != "" {
		return false
	}
	if len(source.Messages) > 0 || source.ToolsFingerprint != "" || source.ToolChoiceFingerprint != "" || source.ResponseFormatFingerprint != "" {
		return false
	}
	if source.Compact != nil {
		return strings.TrimSpace(source.Compact.PromptCacheKey) == "" &&
			strings.TrimSpace(source.Compact.Instructions) == "" &&
			len(source.Compact.Input) == 0
	}

	return true
}
