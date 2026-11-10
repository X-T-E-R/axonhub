package shared

import (
	"context"

	"github.com/looplj/axonhub/llm/logpolicy"
)

type nativeResponsesWebSocketKey struct{}

// WithNativeResponsesWebSocket requests lossless Responses framing through the
// normal gateway pipeline. It does not enable this policy for ordinary HTTP.
func WithNativeResponsesWebSocket(ctx context.Context) context.Context {
	return context.WithValue(WithSensitivePayloadLoggingDisabled(ctx), nativeResponsesWebSocketKey{}, true)
}

func NativeResponsesWebSocket(ctx context.Context) bool {
	value, _ := ctx.Value(nativeResponsesWebSocketKey{}).(bool)
	return value
}

type responsesWebSocketTurnMetadataKey struct{}

// WithResponsesWebSocketTurnMetadata carries the validated current-frame header,
// independently of the authenticated handshake and continuation checkpoint.
func WithResponsesWebSocketTurnMetadata(ctx context.Context, value string) context.Context {
	return context.WithValue(ctx, responsesWebSocketTurnMetadataKey{}, value)
}

func ResponsesWebSocketTurnMetadata(ctx context.Context) (string, bool) {
	value, ok := ctx.Value(responsesWebSocketTurnMetadataKey{}).(string)
	return value, ok
}

// WithSensitivePayloadLoggingDisabled prevents diagnostic payload logs while
// retaining metadata logs and the application's configured observation policy.
func WithSensitivePayloadLoggingDisabled(ctx context.Context) context.Context {
	return logpolicy.WithoutPayloads(ctx)
}

func SensitivePayloadLoggingDisabled(ctx context.Context) bool {
	return logpolicy.PayloadsDisabled(ctx)
}
