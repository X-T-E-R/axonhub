package biz

import (
	"context"
	"fmt"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/cluster"
)

type OwnerChannelSyncRequest struct {
	ChannelID int     `json:"channel_id"`
	Pattern   *string `json:"pattern,omitempty"`
}

func (svc *ChannelService) dispatchModelFetch(ctx context.Context, input FetchModelsInput) (*FetchModelsResult, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeWriteChannels); err != nil {
		return nil, err
	}
	var result FetchModelsResult
	err := cluster.CallOwner(ctx, svc.node, *input.ChannelID, "/__axon/admin/channel-models", input, &result)
	return &result, err
}

func (svc *ChannelService) FetchModelsOnOwner(ctx context.Context, input FetchModelsInput) (*FetchModelsResult, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeWriteChannels); err != nil {
		return nil, err
	}
	if input.ChannelID == nil {
		return nil, fmt.Errorf("channel_id is required")
	}
	if !svc.node.Owns(*input.ChannelID) {
		return nil, svc.ChannelOwnerError(*input.ChannelID)
	}
	return NewModelFetcher(svc.httpClient, svc).FetchModels(ctx, input)
}

func (svc *ChannelService) dispatchModelSync(ctx context.Context, id int, pattern *string) (*ent.Channel, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeWriteChannels); err != nil {
		return nil, err
	}
	var result struct {
		Success bool `json:"success"`
	}
	if err := cluster.CallOwner(ctx, svc.node, id, "/__axon/admin/channel-sync", OwnerChannelSyncRequest{ChannelID: id, Pattern: pattern}, &result); err != nil {
		return nil, err
	}
	return svc.entFromContext(ctx).Channel.Get(ctx, id)
}
