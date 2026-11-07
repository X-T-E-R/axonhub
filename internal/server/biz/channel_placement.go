package biz

import (
	"context"
	"sort"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/scopes"
)

// ChannelExecutionInventory reports registered node IDs, not inferred hostnames or
// UI labels. Static placement remains authoritative until a migration is fenced.
type ChannelExecutionInventory struct {
	ClusterEnabled bool     `json:"cluster_enabled"`
	Machines       []string `json:"machines"`
}

func (svc *ChannelService) ExecutionInventory(ctx context.Context) (*ChannelExecutionInventory, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeReadChannels); err != nil {
		return nil, err
	}
	inventory := &ChannelExecutionInventory{ClusterEnabled: svc.node.Enabled, Machines: []string{}}
	if svc.node.Enabled {
		for id := range svc.node.Nodes {
			inventory.Machines = append(inventory.Machines, id)
		}
		sort.Strings(inventory.Machines)
	}
	return inventory, nil
}

func (svc *ChannelService) ExecutionMachine(ctx context.Context, channelID int) (*string, error) {
	if err := authz.RequireScope(ctx, scopes.ScopeReadChannels); err != nil {
		return nil, err
	}
	owner := svc.node.Owner(channelID)
	if !svc.node.Enabled || owner == "" {
		return nil, nil
	}
	return &owner, nil
}
