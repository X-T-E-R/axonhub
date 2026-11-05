package dependencies

import (
	"context"
	"time"

	"github.com/looplj/axonhub/internal/server/maintenance"
	"github.com/zhenzou/executors"
)

// Count actual callbacks, not future recurring reservations. Pause rejects every
// later callback, including ones scheduled before the maintenance operation.
type maintenanceExecutor struct {
	executors.ScheduledExecutor
	gate *maintenance.Gate
}

func (e *maintenanceExecutor) wrap(r executors.Runnable) executors.Runnable {
	return executors.RunnableFunc(func(ctx context.Context) {
		done, ok := e.gate.Enter("background")
		if !ok {
			return
		}
		defer done()
		r.Run(ctx)
	})
}
func (e *maintenanceExecutor) Execute(r executors.Runnable) error {
	return e.ScheduledExecutor.Execute(e.wrap(r))
}
func (e *maintenanceExecutor) ExecuteFunc(fn func(context.Context)) error {
	return e.Execute(executors.RunnableFunc(fn))
}
func (e *maintenanceExecutor) Schedule(r executors.Runnable, d time.Duration) (executors.CancelFunc, error) {
	return e.ScheduledExecutor.Schedule(e.wrap(r), d)
}
func (e *maintenanceExecutor) ScheduleFunc(fn func(context.Context), d time.Duration) (executors.CancelFunc, error) {
	return e.Schedule(executors.RunnableFunc(fn), d)
}
func (e *maintenanceExecutor) ScheduleAtFixRate(r executors.Runnable, d time.Duration) (executors.CancelFunc, error) {
	return e.ScheduledExecutor.ScheduleAtFixRate(e.wrap(r), d)
}
func (e *maintenanceExecutor) ScheduleFuncAtFixRate(fn func(context.Context), d time.Duration) (executors.CancelFunc, error) {
	return e.ScheduleAtFixRate(executors.RunnableFunc(fn), d)
}
func (e *maintenanceExecutor) ScheduleAtCronRate(r executors.Runnable, rule executors.CRONRule) (executors.CancelFunc, error) {
	return e.ScheduledExecutor.ScheduleAtCronRate(e.wrap(r), rule)
}
func (e *maintenanceExecutor) ScheduleFuncAtCronRate(fn func(context.Context), rule executors.CRONRule) (executors.CancelFunc, error) {
	return e.ScheduleAtCronRate(executors.RunnableFunc(fn), rule)
}
