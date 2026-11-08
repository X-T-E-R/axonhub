package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/system"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/pkg/xcache/live"
	"github.com/looplj/axonhub/internal/server/cluster"
)

const channelPlacementSystemKey = "channel_execution_placement_v1"
const channelPlacementWorkerAckKey = "channel_execution_placement_worker_ack_v1"

// PlacementChange describes a committed configuration edit, not an upstream
// request. Existing streams continue on the provider selected before the edit.
type PlacementChange struct {
	RequestID  string    `json:"request_id"`
	Version    int64     `json:"placement_version"`
	ChannelIDs []int     `json:"channel_ids"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	SavedAt    time.Time `json:"saved_at"`
}

type ChannelPlacementState struct {
	cluster.PlacementSnapshot
	Changes map[int]PlacementChange `json:"changes"`
}

type PlacementWorkerAck struct {
	Version        int64     `json:"placement_version"`
	WorkerRevision string    `json:"worker_revision"`
	AcknowledgedAt time.Time `json:"acknowledged_at"`
}

// Placement reads bypass only the System privacy policy after the caller's
// channel scope has been checked. They use the primary, never the settings cache.
func placementSystemContext(ctx context.Context) context.Context {
	return authz.WithSystemBypass(ctx, "channel-placement-authority")
}

func readChannelPlacement(ctx context.Context, client *ent.Client) (*ent.System, ChannelPlacementState, error) {
	state := ChannelPlacementState{PlacementSnapshot: cluster.PlacementSnapshot{Owners: map[string]string{}, ChannelIdentities: map[int]string{}}, Changes: map[int]PlacementChange{}}
	row, err := observationPrimaryClient(client).System.Query().Where(system.KeyEQ(channelPlacementSystemKey)).Only(placementSystemContext(ctx))
	if ent.IsNotFound(err) {
		return nil, state, nil
	}
	if err != nil {
		return nil, state, err
	}
	if err := json.Unmarshal([]byte(row.Value), &state); err != nil {
		return nil, state, fmt.Errorf("read channel placement: %w", err)
	}
	return row, state, nil
}

func (svc *ChannelService) syncExecutionPlacement(ctx context.Context) error {
	if !svc.node.Enabled {
		return nil
	}
	_, state, err := readChannelPlacement(ctx, svc.db)
	if err != nil {
		return err
	}
	if state.Version > svc.node.PlacementVersion() {
		svc.node.ApplyPlacement(state.PlacementSnapshot)
	}
	return nil
}

func (svc *ChannelService) notifyPlacementReload() {
	if asyncReloadDisabled {
		return
	}
	// Placement has its own version, not a channel row update timestamp.
	if err := svc.channelNotifier.Notify(context.Background(), live.NewRefreshEvent[struct{}](time.Time{})); err != nil {
		log.Warn(context.Background(), "channel placement cache watcher notify failed", log.Cause(err))
	}
}

func readPlacementWorkerAck(ctx context.Context, client *ent.Client) (*PlacementWorkerAck, error) {
	row, err := observationPrimaryClient(client).System.Query().Where(system.KeyEQ(channelPlacementWorkerAckKey)).Only(placementSystemContext(ctx))
	if ent.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ack PlacementWorkerAck
	if err := json.Unmarshal([]byte(row.Value), &ack); err != nil {
		return nil, err
	}
	return &ack, nil
}

// InfrastructurePlacementContext summarizes authoritative cluster placement and Worker
// acknowledgement state for the native infrastructure control plane without exposing secrets.
type InfrastructurePlacementContext struct {
	ClusterEnabled   bool                `json:"clusterEnabled"`
	ControlNode      string              `json:"controlNode"`
	PlacementVersion int64               `json:"placementVersion"`
	Machines         []string            `json:"machines"`
	WorkerAck        *PlacementWorkerAck `json:"workerAck,omitempty"`
}

func (svc *ChannelService) ReadInfrastructurePlacementContext(ctx context.Context) (*InfrastructurePlacementContext, error) {
	inv, err := svc.ExecutionInventory(ctx)
	if err != nil {
		return nil, err
	}
	_, state, err := readChannelPlacement(ctx, svc.db)
	if err != nil {
		return nil, err
	}
	ack, err := readPlacementWorkerAck(ctx, svc.db)
	if err != nil {
		return nil, err
	}
	controlNode := svc.node.ControlNode
	if controlNode == "" {
		controlNode = "lag"
	}
	return &InfrastructurePlacementContext{
		ClusterEnabled:   inv.ClusterEnabled,
		ControlNode:      controlNode,
		PlacementVersion: state.Version,
		Machines:         inv.Machines,
		WorkerAck:        ack,
	}, nil
}
