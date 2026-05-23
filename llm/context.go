package llm

import "context"

type channelKeyAffinityContextKey struct{}

// WithChannelKeyAffinityID stores a non-secret routing hint in the context.
func WithChannelKeyAffinityID(ctx context.Context, affinityID string) context.Context {
	return context.WithValue(ctx, channelKeyAffinityContextKey{}, affinityID)
}

// GetChannelKeyAffinityID retrieves the channel key affinity identifier.
func GetChannelKeyAffinityID(ctx context.Context) (string, bool) {
	affinityID, ok := ctx.Value(channelKeyAffinityContextKey{}).(string)
	return affinityID, ok
}
