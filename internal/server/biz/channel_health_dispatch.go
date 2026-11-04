package biz

import (
	"context"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/cluster"
)

type OwnerChannelHealthRequest struct {
	ChannelID int                                    `json:"channel_id"`
	KeyIDs    []string                               `json:"key_ids"`
	Modes     []objects.ChannelAPIKeyHealthCheckMode `json:"modes"`
}

func (svc *ChannelService) dispatchChannelHealth(ctx context.Context, id int, keys []string, modes []objects.ChannelAPIKeyHealthCheckMode) ([]*ChannelAPIKeyInventoryItem, error) {
	var result []*ChannelAPIKeyInventoryItem
	err := cluster.CallOwner(ctx, svc.node, id, "/__axon/admin/channel-health", OwnerChannelHealthRequest{ChannelID: id, KeyIDs: keys, Modes: modes}, &result)
	return result, err
}
