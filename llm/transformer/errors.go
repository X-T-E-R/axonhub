package transformer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrInvalidRequest    = errors.New("invalid request")
	ErrInvalidModel      = errors.New("model not found")
	ErrInvalidResponse   = errors.New("invalid response")
	ErrToolCallIntegrity = errors.New("tool call integrity failure")
	// ErrIncompleteToolCall is retained as a compatibility alias.
	ErrIncompleteToolCall = ErrToolCallIntegrity
)

// ValidateToolCallJSON verifies the minimum lossless streaming contract for a
// tool call whose destination accepts any JSON value.
func ValidateToolCallJSON(name, arguments string) error {
	if name == "" {
		return fmt.Errorf("%w: missing function name", ErrToolCallIntegrity)
	}

	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return fmt.Errorf("%w: missing arguments for %s", ErrToolCallIntegrity, name)
	}
	if !json.Valid([]byte(trimmed)) {
		return fmt.Errorf("%w: invalid arguments for %s", ErrToolCallIntegrity, name)
	}

	return nil
}

// ValidateFunctionCall additionally requires a JSON object, as required by
// OpenAI, Anthropic, Gemini, and Responses function-call destinations.
func ValidateFunctionCall(name, arguments string) error {
	if err := ValidateToolCallJSON(name, arguments); err != nil {
		return err
	}

	trimmed := strings.TrimSpace(arguments)
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &object); err != nil || object == nil {
		return fmt.Errorf("%w: arguments for %s are not a JSON object", ErrToolCallIntegrity, name)
	}

	return nil
}
