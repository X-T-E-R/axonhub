package biz

import (
	"context"
	"testing"
	"time"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/channelkeymonitoringevent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/llm/httpclient"
	"github.com/stretchr/testify/require"
)

func TestClusterMonitoringOwnerSchedule(t *testing.T) {
	disableChannelKeyHealthCheckDelays(t)
	a, client := setupTestChannelService(t)
	defer client.Close()
	b := NewChannelServiceForTest(client)
	defer b.Stop()
	defer a.Stop()
	b.SystemService = a.SystemService
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))
	bindings := map[int]cluster.ChannelBinding{}
	ids, calls := map[string]int{}, map[string]int{}
	for _, id := range []string{"a", "b"} {
		ch := client.Channel.Create().SetName("monitor-" + id).SetType(channel.TypeOpenai).SetStatus(channel.StatusEnabled).
			SetBaseURL("http://127.0.0.1:1").SetCredentials(objects.ChannelCredentials{APIKey: "synthetic-" + id}).
			SetSupportedModels([]string{"m"}).SetDefaultTestModel("m").SaveX(ctx)
		bindings[ch.ID] = cluster.ChannelBinding{Owner: id, Identity: "monitor-" + id}
		ids[id] = ch.ID
	}
	for id, svc := range map[string]*ChannelService{"a": a, "b": b} {
		svc.node = cluster.Config{Enabled: true, NodeID: id, ControlNode: "a", Nodes: map[string]string{"a": "http://127.0.0.1:1", "b": "http://127.0.0.1:2"}, Channels: bindings}
		node := id
		svc.SetChannelKeyHealthCheckTester(channelKeyHealthCheckTesterFunc(func(_ context.Context, target objects.GUID, _ string, _ *string, _ *httpclient.ProxyConfig) ChannelKeyHealthCheckBuiltinResult {
			require.Equal(t, ids[node], target.ID, "a node must only test its own target")
			calls[node]++
			return ChannelKeyHealthCheckBuiltinResult{Success: true, Reason: "synthetic"}
		}))
	}
	require.NoError(t, a.SystemService.SetMonitoringSettings(ctx, MonitoringSettings{Enabled: true, Rules: []MonitoringRule{{ID: "both-owners", Name: "both owners", Schedule: MonitoringRuleSchedule{IntervalMinutes: 60}, Targets: MonitoringRuleTargets{ChannelIDs: []int{ids["a"], ids["b"]}, KeyStatuses: []objects.ChannelKeyStatus{objects.ChannelKeyStatusActive}}}}}))
	start := time.Now()
	for tick := range 3 {
		now := start.Add(time.Duration(tick) * 61 * time.Minute)
		checkedA, _, err := a.runDueMonitoringRules(ctx, now)
		require.NoError(t, err)
		checkedB, _, err := b.runDueMonitoringRules(ctx, now.Add(time.Second))
		require.NoError(t, err)
		require.Equal(t, 1, checkedA)
		require.Equal(t, 1, checkedB)
		t.Logf("tick=%d owner a=%d owner b=%d", tick, checkedA, checkedB)
	}
	for _, id := range []string{"a", "b"} {
		require.Equal(t, 3, calls[id])
		require.Equal(t, 3, client.ChannelKeyMonitoringEvent.Query().Where(channelkeymonitoringevent.ChannelIDEQ(ids[id])).CountX(ctx))
	}
}
