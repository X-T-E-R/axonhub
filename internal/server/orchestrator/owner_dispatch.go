package orchestrator

import (
	"context"
	"fmt"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/llm/httpclient"
)

type OwnerTestRequest struct {
	Operation string                  `json:"operation"`
	ChannelID int                     `json:"channel_id"`
	ModelID   *string                 `json:"model_id,omitempty"`
	Key       string                  `json:"key,omitempty"`
	Proxy     *httpclient.ProxyConfig `json:"proxy,omitempty"`
}

func (p *TestChannelOrchestrator) dispatchTest(ctx context.Context, r OwnerTestRequest, out any) error {
	if err := authz.RequireScope(ctx, scopes.ScopeReadChannels); err != nil {
		return err
	}
	return cluster.CallOwner(ctx, p.channelService.NodeConfig(), r.ChannelID, "/__axon/admin/channel-test", r, out)
}

func (p *TestChannelOrchestrator) ExecuteOwnerTest(ctx context.Context, r OwnerTestRequest) (any, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeReadChannels); err != nil {
		return nil, err
	}
	if !p.channelService.OwnsChannel(r.ChannelID) {
		return nil, p.channelService.ChannelOwnerError(r.ChannelID)
	}
	id := objects.GUID{ID: r.ChannelID}
	switch r.Operation {
	case "channel":
		return p.TestChannel(ctx, id, r.ModelID, r.Proxy)
	case "keys":
		return p.TestChannelAPIKeys(ctx, id, r.ModelID, r.Proxy)
	case "key":
		return p.TestSingleAPIKey(ctx, id, r.Key, r.ModelID, r.Proxy)
	default:
		return nil, fmt.Errorf("unknown channel-test operation")
	}
}
