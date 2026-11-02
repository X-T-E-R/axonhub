package llm

import (
	"encoding/json"
	"net/http"
)

// DecisionRequest is deliberately not a chat DTO. The original JSON is retained
// so typed values, ordered arrays, extension fields and large integers survive.
type DecisionRequest struct {
	Body json.RawMessage `json:"body"`
}

type DecisionResponse struct {
	Body       json.RawMessage `json:"body"`
	StatusCode int             `json:"status_code"`
	Headers    http.Header     `json:"headers,omitempty"`
}

func IsDecisionAPIFormat(format APIFormat) bool {
	return format == APIFormatTypeSafeSystemOne || format == APIFormatCommandCodeSystemOne || format == APIFormatOpenAIDecisions
}

func IsSystemOneAPIFormat(format APIFormat) bool {
	return format == APIFormatTypeSafeSystemOne || format == APIFormatCommandCodeSystemOne
}

// DecisionFormatsCompatible means identical wire semantics, not a conversion.
func DecisionFormatsCompatible(a, b APIFormat) bool {
	return IsDecisionAPIFormat(a) && (a == b || (IsSystemOneAPIFormat(a) && IsSystemOneAPIFormat(b)))
}
