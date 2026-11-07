package biz

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/server/cluster"
)

func TestChannelExecutionInventory(t *testing.T) {
	ctx := authz.WithTestBypass(context.Background())
	standalone := &ChannelService{}
	inventory, err := standalone.ExecutionInventory(ctx)
	require.NoError(t, err)
	require.False(t, inventory.ClusterEnabled)
	require.Empty(t, inventory.Machines)
	owner, err := standalone.ExecutionMachine(ctx, 7)
	require.NoError(t, err)
	require.Nil(t, owner)

	svc := &ChannelService{node: cluster.Config{Enabled: true, Nodes: map[string]string{"lag": "http://lag", "control": "http://control"},
		Channels: map[int]cluster.ChannelBinding{7: {Owner: "lag", Identity: "shared"}, 9: {Owner: "lag", Identity: "shared"}}}}
	for _, id := range []int{7, 9} {
		owner, err = svc.ExecutionMachine(ctx, id)
		require.NoError(t, err)
		require.Equal(t, "lag", *owner)
	}
	owner, err = svc.ExecutionMachine(ctx, 99)
	require.NoError(t, err)
	require.Nil(t, owner)
	_, err = svc.ExecutionInventory(context.Background())
	require.Error(t, err)
	_, err = svc.ExecutionMachine(context.Background(), 7)
	require.Error(t, err)
}
