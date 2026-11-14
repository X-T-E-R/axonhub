package biz

import (
	"context"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/objects"
)

// The health-test override deliberately bypasses quota/status admission, but not
// the credential's network identity or fail-closed egress policy.
type forcedChannelKeyProvider struct {
	channel *Channel
	key     string
}

func (p *forcedChannelKeyProvider) Get(ctx context.Context) string {
	if p.channel.credentialEgress == nil {
		return recordSelectedChannelAPIKey(ctx, p.key)
	}
	client, _, err := p.channel.credentialEgress.resolveCredentialClient(ctx, p.channel.Channel, objects.ChannelAPIKeyFingerprint(p.key), p.channel.HTTPClient)
	if err != nil {
		return recordSelectedChannelAPIKey(ctx, "")
	}
	contexts.WithChannelCredentialEgress(ctx, p.key, client, nil)
	return p.key
}
