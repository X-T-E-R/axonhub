package biz

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/metrics"
)

// Close stops admission and drains the bounded queue before closing SQLite.
// If ctx expires the writer continues draining, and a later Close can await it.
func (o *CompactAccountingOutbox) Close(ctx context.Context) error {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	if !o.closed {
		o.closed = true
		close(o.queue)
	}
	o.mu.Unlock()
	select {
	case <-o.done:
		return o.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reserve is best-effort accounting capacity, never inference admission.
// Release the estimate before Append replaces it with actual event capacity.
func (o *CompactAccountingOutbox) Reserve() (func(), error) {
	if o == nil {
		return nil, ErrCompactOutboxUnavailable
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	err := o.capacityErrorLocked(1, compactReservationBytes)
	if err != nil {
		o.admissionFailures++
		o.lastError = err.Error()
		metrics.RecordManagedObservabilityFailure(context.Background(), "compact_accounting", "admission_degraded")
		return nil, err
	}
	o.reservedCount++
	var once sync.Once
	return func() {
		once.Do(func() {
			o.mu.Lock()
			o.reservedCount--
			o.mu.Unlock()
		})
	}, nil
}

func (o *CompactAccountingOutbox) capacityErrorLocked(count int, bytes int64) error {
	if o.closed {
		return ErrCompactOutboxUnavailable
	}
	if o.frozen {
		return ErrCompactOutboxFrozen
	}
	if o.unackedCount+o.pendingCount+o.reservedCount+count > o.cfg.MaxSpoolCount ||
		o.unackedBytes+o.pendingBytes+int64(o.reservedCount)*compactReservationBytes+bytes > o.cfg.MaxSpoolBytes {
		return ErrCompactOutboxSpoolFull
	}
	return nil
}

// Append enqueues an owned event without SQL or waiting for the writer. Seq is
// assigned only on commit, so dropped batches never leave gaps in the ledger.
func (o *CompactAccountingOutbox) Append(ctx context.Context, event CompactUsageEvent) (CompactUsageEvent, error) {
	if o == nil {
		return CompactUsageEvent{}, ErrCompactOutboxUnavailable
	}
	if event.EventUUID == "" {
		event.EventUUID = uuid.NewString()
	}
	if event.CapturedAt.IsZero() {
		event.CapturedAt = time.Now().UTC()
	} else {
		event.CapturedAt = event.CapturedAt.UTC()
	}
	// Reserve the largest int64 sequence encoding; reconcile to exact serialized
	// bytes after commit. This bounds spool bytes even when events exceed 2 KiB.
	event.Seq = 9223372036854775807
	raw, err := json.Marshal(event)
	if err != nil {
		o.recordDrop(ctx, err)
		return CompactUsageEvent{}, err
	}
	var owned CompactUsageEvent
	if err := json.Unmarshal(raw, &owned); err != nil {
		o.recordDrop(ctx, err)
		return CompactUsageEvent{}, err
	}
	owned.Seq = 0
	queued := compactQueuedEvent{event: owned, bytes: int64(len(raw))}
	o.mu.Lock()
	err = o.capacityErrorLocked(1, queued.bytes)
	if err == nil {
		select {
		case o.queue <- queued:
			if o.pendingCount == 0 {
				o.idle = make(chan struct{})
			}
			o.pendingCount++
			o.pendingBytes += queued.bytes
		default:
			err = ErrCompactOutboxSpoolFull
		}
	}
	o.mu.Unlock()
	if err != nil {
		o.recordDrop(ctx, err)
		return CompactUsageEvent{}, err
	}
	return owned, nil
}

func (o *CompactAccountingOutbox) recordDrop(ctx context.Context, err error) {
	o.mu.Lock()
	o.droppedEvents++
	o.lastError = err.Error()
	o.mu.Unlock()
	metrics.RecordManagedObservabilityFailure(ctx, "compact_accounting", "dropped")
	log.Warn(ctx, "Compact accounting event dropped; inference continues", log.Cause(err))
}

// Wait is for diagnostics, flush and migration only; forwarding never waits.
func (o *CompactAccountingOutbox) Wait(ctx context.Context) error {
	o.mu.Lock()
	idle := o.idle
	o.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (o *CompactAccountingOutbox) writeLoop() {
	defer close(o.done)
	defer func() {
		if p := recover(); p != nil {
			log.Error(context.Background(), "Compact accounting writer panicked", log.Any("panic", p))
		}
		o.dbMu.Lock()
		o.closeErr = o.db.Close()
		o.dbMu.Unlock()
	}()
	for first := range o.queue {
		batch := []compactQueuedEvent{first}
		timer := time.NewTimer(10 * time.Millisecond)
	collect:
		for len(batch) < 64 {
			select {
			case event, ok := <-o.queue:
				if !ok {
					break collect
				}
				batch = append(batch, event)
			case <-timer.C:
				break collect
			}
		}
		timer.Stop()
		o.writeBatch(batch)
	}
}

func (o *CompactAccountingOutbox) writeBatch(batch []compactQueuedEvent) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	o.dbMu.Lock()
	writtenBytes, err := o.persistBatch(ctx, batch)
	// Keep DB-derived totals and ACK recounts in the same serialization domain.
	o.mu.Lock()
	for _, queued := range batch {
		o.pendingBytes -= queued.bytes
	}
	o.pendingCount -= len(batch)
	if err == nil {
		o.unackedCount += len(batch)
		o.unackedBytes += writtenBytes
	} else {
		o.writeErrors++
		o.droppedEvents += uint64(len(batch))
		o.lastError = err.Error()
	}
	if o.pendingCount == 0 {
		close(o.idle)
	}
	o.mu.Unlock()
	o.dbMu.Unlock()
	if err != nil {
		metrics.RecordManagedObservabilityFailure(ctx, "compact_accounting", "write_failed")
		log.Warn(ctx, "Compact accounting batch dropped; inference continues", log.Int("events", len(batch)), log.Cause(err))
	}
}

func (o *CompactAccountingOutbox) persistBatch(ctx context.Context, batch []compactQueuedEvent) (int64, error) {
	tx, err := o.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var nextSeq int64
	var frozen int
	if err := tx.QueryRowContext(ctx, `SELECT next_seq, frozen FROM outbox_meta WHERE id = 1`).Scan(&nextSeq, &frozen); err != nil {
		return 0, err
	}
	if frozen != 0 {
		o.mu.Lock()
		o.frozen = true
		o.mu.Unlock()
		return 0, ErrCompactOutboxFrozen
	}
	var totalBytes int64
	for i, queued := range batch {
		event := queued.event
		event.Seq = nextSeq + int64(i)
		payload, err := json.Marshal(event)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO outbox_events (seq, event_uuid, captured_at, payload_json, payload_bytes) VALUES (?, ?, ?, ?, ?)`,
			event.Seq, event.EventUUID, event.CapturedAt.UnixNano(), payload, len(payload)); err != nil {
			return 0, fmt.Errorf("insert compact usage batch: %w", err)
		}
		totalBytes += int64(len(payload))
	}
	if _, err := tx.ExecContext(ctx, `UPDATE outbox_meta SET next_seq = ?, updated_at = ? WHERE id = 1`, nextSeq+int64(len(batch)), time.Now().UTC().UnixNano()); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return totalBytes, nil
}
