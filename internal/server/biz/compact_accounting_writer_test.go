package biz

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/server/cluster"
)

func TestCompactOutbox_SQLStallDoesNotBlockInferenceAndCloseDrains(t *testing.T) {
	ctx := t.Context()
	o, err := OpenCompactAccountingOutbox(cluster.CompactAccountingConfig{
		OutboxPath: t.TempDir() + "/outbox.db", MaxSpoolCount: 2048, MaxSpoolBytes: 8 << 20,
	}, "test")
	require.NoError(t, err)
	// Simulate a stalled disk/SQL writer. Neither Reserve nor Append may acquire
	// the DB lock, and the pending queue must remain bounded under this stall.
	o.dbMu.Lock()
	result := make(chan int, 1)
	go func() {
		count := 0
		for range 2048 {
			release, reserveErr := o.Reserve()
			if reserveErr != nil {
				break
			}
			release()
			if _, appendErr := o.Append(ctx, CompactUsageEvent{ModelID: "m"}); appendErr != nil {
				break
			}
			count++
		}
		result <- count
	}()
	var accepted int
	select {
	case accepted = <-result:
	case <-time.After(2 * time.Second):
		o.dbMu.Unlock()
		t.Fatal("accounting admission waited for SQLite")
	}
	require.Greater(t, accepted, 0)
	require.LessOrEqual(t, accepted, cap(o.queue)+64)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, o.Close(cancelled), context.Canceled)
	o.dbMu.Unlock()
	require.NoError(t, o.Close(ctx))
	status, err := InspectCompactOutbox(ctx, o.Path())
	require.NoError(t, err)
	require.Equal(t, accepted, status.UnackedCount)
	require.EqualValues(t, accepted+1, status.NextSeq)
}

func TestCompactOutbox_ActualPayloadCapacityAndWriteFailureRecovery(t *testing.T) {
	ctx := t.Context()
	o, err := OpenCompactAccountingOutbox(cluster.CompactAccountingConfig{
		OutboxPath: t.TempDir() + "/outbox.db", MaxSpoolCount: 100, MaxSpoolBytes: 4096,
	}, "test")
	require.NoError(t, err)
	defer o.Close(context.Background())
	release, err := o.Reserve()
	require.NoError(t, err)
	release()
	_, err = o.Append(ctx, CompactUsageEvent{ModelID: strings.Repeat("m", 5000)})
	require.ErrorIs(t, err, ErrCompactOutboxSpoolFull)
	status, err := o.Status(ctx)
	require.NoError(t, err)
	require.Zero(t, status.ReservedCount)
	require.Zero(t, status.QueuedBytes)
	require.EqualValues(t, 1, status.DroppedEvents)

	_, err = o.db.ExecContext(ctx, `CREATE TRIGGER fail_event BEFORE INSERT ON outbox_events BEGIN SELECT RAISE(ABORT, 'test disk write failure'); END`)
	require.NoError(t, err)
	_, err = o.Append(ctx, CompactUsageEvent{ModelID: "failed"})
	require.NoError(t, err, "submission only enqueues; a disk failure is asynchronous")
	require.NoError(t, o.Wait(ctx))
	status, err = o.Status(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, status.WriteErrors)
	require.EqualValues(t, 2, status.DroppedEvents)
	require.Zero(t, status.QueuedCount)
	require.Zero(t, status.UnackedBytes)
	require.NotEmpty(t, status.LastError)

	_, err = o.db.ExecContext(ctx, `DROP TRIGGER fail_event`)
	require.NoError(t, err)
	_, err = o.Append(ctx, CompactUsageEvent{ModelID: strings.Repeat("m", 2500)})
	require.NoError(t, err)
	batch, _, err := o.SealOrLoadNextBatch(ctx)
	require.NoError(t, err)
	require.Len(t, batch.Events, 1)
	require.EqualValues(t, 1, batch.SeqStart, "a dropped write cannot create a sequence gap")
	status, err = o.Status(ctx)
	require.NoError(t, err)
	require.Greater(t, status.UnackedBytes, compactReservationBytes)
	require.LessOrEqual(t, status.UnackedBytes, o.cfg.MaxSpoolBytes)
	require.Zero(t, status.QueuedBytes)
}

func TestCompactAccountingService_StopTimeoutClosesAdmissionAndCanResume(t *testing.T) {
	node := cluster.Config{
		Enabled: true, NodeID: "b", ControlNode: "a",
		Nodes:             map[string]string{"a": "http://127.0.0.1:1", "b": "http://127.0.0.1:2"},
		CompactAccounting: cluster.CompactAccountingConfig{Enabled: true, OutboxPath: t.TempDir() + "/outbox.db", SyncInterval: time.Millisecond, SharedSecret: "review-test"},
	}
	svc := NewCompactAccountingService(CompactAccountingServiceParams{Node: node})
	require.NoError(t, svc.Start(t.Context()))
	o := svc.Outbox()
	require.NoError(t, svc.Flush(t.Context()))
	svc.mu.Lock()
	done := svc.done
	svc.mu.Unlock()

	// Same SQL-stall seam used by the candidate's writer regression. With no
	// pending events, scheduled Flush is waiting for this DB serialization lock.
	o.dbMu.Lock()
	locked := true
	defer func() {
		if locked {
			o.dbMu.Unlock()
		}
		_ = o.Close(context.Background())
	}()
	require.Eventually(t, func() bool {
		if svc.flushMu.TryLock() {
			svc.flushMu.Unlock()
			return false
		}
		return true
	}, time.Second, time.Millisecond)

	stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, svc.Stop(stopCtx), context.DeadlineExceeded)
	o.mu.Lock()
	closed := o.closed
	o.mu.Unlock()
	if !closed {
		t.Error("Stop timed out without invoking outbox.Close: admission is open and the writer cannot finish")
	}
	require.Same(t, o, svc.Outbox(), "retain the draining writer for a subsequent Stop")
	o.dbMu.Unlock()
	locked = false
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sync loop did not finish after releasing the SQL stall")
	}
	require.NoError(t, svc.Stop(t.Context()))
	select {
	case <-o.done:
	default:
		t.Error("retrying service Stop returns nil but leaves the asynchronous writer running")
	}
}
