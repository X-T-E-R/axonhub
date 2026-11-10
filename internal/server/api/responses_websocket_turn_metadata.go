package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/http/httpguts"

	"github.com/looplj/axonhub/llm/transformer/openai/codex"
)

const responsesWSTurnMetadataKey = "x-codex-turn-metadata"

var responsesWSFixedTurnMetadataKeys = [...]string{"session_id", "thread_id", "installation_id", "window_id"}

// Only the explicit body key opts into per-frame projection. Other metadata and
// clients without this key retain the original handshake behavior.
func responsesWSTurnMetadata(frame responsesWSCreate, headers http.Header) (string, *responsesWSError) {
	var metadata map[string]json.RawMessage
	if json.Unmarshal(frame.body["client_metadata"], &metadata) != nil {
		return "", nil
	}
	value, present := metadata[responsesWSTurnMetadataKey]
	if !present {
		return "", nil
	}
	invalid := wsRejected("invalid_turn_metadata", "Turn metadata must be a header-safe JSON object string with unchanged handshake identity.", "client_metadata."+responsesWSTurnMetadataKey)
	var raw string
	if json.Unmarshal(value, &raw) != nil {
		return "", invalid
	}
	current := responsesWSTurnMetadataObject(raw)
	if current == nil {
		return "", invalid
	}
	var baseline map[string]json.RawMessage
	if values := headers.Values(codex.TurnMetadataHeader); len(values) > 0 {
		if len(values) != 1 {
			return "", invalid
		}
		baseline = responsesWSTurnMetadataObject(values[0])
		if baseline == nil {
			return "", invalid
		}
	}
	inherited := false
	for _, key := range responsesWSFixedTurnMetadataKeys {
		fixed, exists := baseline[key]
		if supplied, present := current[key]; present {
			var original, candidate string
			if !exists || json.Unmarshal(fixed, &original) != nil || json.Unmarshal(supplied, &candidate) != nil || candidate != original {
				return "", invalid
			}
		} else if exists {
			current[key] = fixed
			inherited = true
		}
	}
	if inherited {
		encoded, _ := json.Marshal(current)
		raw = string(encoded)
	}
	return raw, nil
}

func responsesWSTurnMetadataObject(raw string) map[string]json.RawMessage {
	if !httpguts.ValidHeaderFieldValue(raw) || !utf8.ValidString(raw) {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	if start, err := decoder.Token(); err != nil || start != json.Delim('{') {
		return nil
	}
	object := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil
		}
		name := key.(string)
		var value json.RawMessage
		if _, duplicate := object[name]; duplicate || decoder.Decode(&value) != nil {
			return nil
		}
		// Reject aliases a case-insensitive downstream JSON decoder could treat
		// as a different spelling of the fixed identity, rather than a new key.
		for _, fixed := range responsesWSFixedTurnMetadataKeys {
			if strings.EqualFold(name, fixed) && (name != fixed || value[0] != '"') {
				return nil
			}
		}
		object[name] = value
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return nil
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil
	}
	return object
}
