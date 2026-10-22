// Package logpolicy carries request-local diagnostic payload logging policy.
package logpolicy

import "context"

type sensitivePayloadKey struct{}

// WithoutPayloads suppresses diagnostic request, response and event bodies.
// It does not alter configured business observation or persistence policies.
func WithoutPayloads(ctx context.Context) context.Context {
	return context.WithValue(ctx, sensitivePayloadKey{}, true)
}

func PayloadsDisabled(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	disabled, _ := ctx.Value(sensitivePayloadKey{}).(bool)
	return disabled
}
