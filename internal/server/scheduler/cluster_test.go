package scheduler

import (
	"context"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/stretchr/testify/require"
	"github.com/zhenzou/executors"
	"sync/atomic"
	"testing"
	"time"
)

func TestClusterGlobalMaintenanceAndLocalConsumers(t *testing.T) {
	ea, eb := executors.NewPoolScheduleExecutor(), executors.NewPoolScheduleExecutor()
	defer ea.Shutdown(context.Background())
	defer eb.Shutdown(context.Background())
	a, b := New(ea), New(eb)
	a.node = cluster.Config{Enabled: true, NodeID: "a", ControlNode: "a"}
	b.node = cluster.Config{Enabled: true, NodeID: "b", ControlNode: "a"}
	defer a.Shutdown(context.Background())
	defer b.Shutdown(context.Background())
	var ag, bg, local atomic.Int32
	for _, s := range []*Scheduler{a, b} {
		counter := &bg
		if s == a {
			counter = &ag
		}
		require.NoError(t, s.Register(t.Context(), TaskSpec{Name: "backup", FixRate: 20 * time.Millisecond}, func(context.Context) { counter.Add(1) }))
		require.NoError(t, s.Register(t.Context(), TaskSpec{Name: "datastorage-fs-reload", FixRate: 20 * time.Millisecond}, func(context.Context) { local.Add(1) }))
	}
	require.Eventually(t, func() bool { return ag.Load() > 0 && local.Load() >= 2 }, time.Second, 10*time.Millisecond)
	require.Zero(t, bg.Load())
	require.NoError(t, b.Reschedule(t.Context(), "backup", TaskSpec{Name: "backup", FixRate: 20 * time.Millisecond}))
	require.Nil(t, b.tasks["backup"].cancelFunc, "rescheduling cannot start a non-owned global task")
	require.NotNil(t, b.tasks["datastorage-fs-reload"].cancelFunc)
}
