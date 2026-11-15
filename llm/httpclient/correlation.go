package httpclient

import (
	"context"
	"encoding/json"
	"net/http"
)

const ClientRequestIDHeader = "X-Client-Request-Id"

type clientRequestIDKey struct{}

// WithClientRequestID carries the ingress-validated ID to direct HTTP calls as
// well as pipeline requests, independently of transformer/header overrides.
func WithClientRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, clientRequestIDKey{}, id)
}

// ApplyClientRequestID restores gateway-owned correlation before observation or
// transport. Calls outside an inbound request retain their existing contract.
func ApplyClientRequestID(ctx context.Context, headers http.Header) http.Header {
	if id, ok := ctx.Value(clientRequestIDKey{}).(string); ok {
		if headers == nil {
			headers = make(http.Header)
		}
		headers.Set(ClientRequestIDHeader, id)
	}
	return headers
}

// ValidClientRequestID accepts only bounded, printable identifiers safe in every
// transport and log. Invalid caller input is replaced by the server's request ID.
func ValidClientRequestID(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// CorrelationHeadersJSON serializes the bounded, credential-free lookup metadata.
func CorrelationHeadersJSON(headers http.Header) []byte {
	value, _ := json.Marshal(CorrelationHeaders(headers))
	return value
}

// CorrelationHeaders keeps the small lookup metadata even when payload capture
// is disabled or its capacity budget has been exhausted. Authentication is never
// included. Returned values do not alias the transport's mutable header slices.
func CorrelationHeaders(headers http.Header) http.Header {
	result := make(http.Header)
	for key, values := range headers {
		switch http.CanonicalHeaderKey(key) {
		case ClientRequestIDHeader, "Ah-Request-Id", "Cf-Ray":
			result[http.CanonicalHeaderKey(key)] = append([]string(nil), values...)
		}
	}
	return result
}
