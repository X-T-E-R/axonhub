package dependencies

import (
	"context"
	"testing"
	"time"

	"github.com/looplj/axonhub/internal/server/maintenance"
	"github.com/stretchr/testify/require"
	"github.com/zhenzou/executors"
)

type captureExecutor struct {
	executors.ScheduledExecutor
	callback executors.Runnable
}

func (c *captureExecutor) Execute(r executors.Runnable) error { c.callback = r; return nil }
func (c *captureExecutor) ScheduleAtFixRate(r executors.Runnable, _ time.Duration) (executors.CancelFunc, error) {
	c.callback = r
	return func() {}, nil
}

func TestMaintenanceExecutorFencesAlreadyScheduledCallbacks(t *testing.T) {
	gate := maintenance.NewGate()
	raw := &captureExecutor{}
	e := &maintenanceExecutor{ScheduledExecutor: raw, gate: gate}
	calls := 0
	_, err := e.ScheduleFuncAtFixRate(func(context.Context) { calls++; require.Equal(t, 1, gate.Snapshot().ActiveBackground) }, time.Second)
	require.NoError(t, err)
	raw.callback.Run(context.Background())
	require.Equal(t, 1, calls)
	gate.Pause()
	raw.callback.Run(context.Background())
	require.Equal(t, 1, calls)
	require.NoError(t, e.ExecuteFunc(func(context.Context) { calls++ }))
	raw.callback.Run(context.Background())
	require.Equal(t, 1, calls)
	require.NoError(t, gate.Wait(context.Background()))
}
