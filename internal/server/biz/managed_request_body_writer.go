package biz

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql/sqljson"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/metrics"
	"github.com/looplj/axonhub/internal/objects"
)

const (
	managedRequestBodyRejectCapacity     = "managed_async_capacity"
	managedRequestBodyRejectItemTooLarge = "managed_async_item_too_large"
	managedRequestBodyRejectNotStarted   = "managed_async_not_started"
	managedRequestBodyRejectStopping     = "managed_async_stopping"
	managedRequestBodyAsyncPending       = "async_pending"
	managedRequestBodyCapacityPressure   = "capacity_pressure"
	managedRequestBodyRetryExhausted     = "managed_write_failed:retry_exhausted"
	managedRequestBodyStopped            = "managed_async_stopped"
)

// ManagedRequestBodyWriterConfig bounds the process-owned queue used for
// primary-database parent and execution request bodies.
type ManagedRequestBodyWriterConfig struct {
	Workers        int           `conf:"workers" yaml:"workers" json:"workers"`
	MaxItems       int           `conf:"max_items" yaml:"max_items" json:"max_items"`
	MaxBytesMiB    int           `conf:"max_bytes_mib" yaml:"max_bytes_mib" json:"max_bytes_mib"`
	AttemptTimeout time.Duration `conf:"attempt_timeout" yaml:"attempt_timeout" json:"attempt_timeout"`
	MaxAttempts    int           `conf:"max_attempts" yaml:"max_attempts" json:"max_attempts"`
}

func (c ManagedRequestBodyWriterConfig) withDefaults() ManagedRequestBodyWriterConfig {
	if c.Workers <= 0 {
		c.Workers = 1
	}
	if c.MaxItems <= 0 {
		c.MaxItems = 64
	}
	if c.MaxBytesMiB <= 0 {
		c.MaxBytesMiB = 64
	}
	if c.AttemptTimeout <= 0 {
		c.AttemptTimeout = 2 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 3
	}
	return c
}

type managedRequestBodyTargetKind uint8

const (
	managedRequestBodyTargetRequest managedRequestBodyTargetKind = iota + 1
	managedRequestBodyTargetExecution
)

type managedRequestBodyTarget struct {
	kind      managedRequestBodyTargetKind
	requestID int
	targetID  int
}

type managedRequestBodyJob struct {
	target      managedRequestBodyTarget
	reservation *managedRequestBodyReservation
}

type managedRequestBodyReservation struct {
	writer     *ManagedRequestBodyWriter
	body       []byte
	byteLength int64
	state      managedRequestBodyReservationState
}

func (r *managedRequestBodyReservation) release() {
	if r == nil {
		return
	}
	r.writer.releaseReservation(r)
}

type managedRequestBodyReservationState uint8

const (
	managedRequestBodyReservationProducer managedRequestBodyReservationState = iota + 1
	managedRequestBodyReservationQueued
	managedRequestBodyReservationWorker
	managedRequestBodyReservationReleased
	managedRequestBodyReservationCanceled
)

// ManagedRequestBodyWriter owns the bounded queue and independent persistence
// contexts for managed parent/execution request bodies.
type ManagedRequestBodyWriter struct {
	config ManagedRequestBodyWriterConfig
	ent    *ent.Client
	system *SystemService

	mu            sync.Mutex
	started       bool
	stopping      bool
	reservedItems int
	reservedBytes int64
	reservations  map[*managedRequestBodyReservation]struct{}
	idle          chan struct{}
	jobs          chan managedRequestBodyJob
	workerCtx     context.Context
	cancelWorkers context.CancelFunc
	liveWorkers   int
	workersDone   chan struct{}
	stopDone      chan struct{}
	stopOnce      sync.Once

	beforePersistForTest func(context.Context)
}

func NewManagedRequestBodyWriter(config ManagedRequestBodyWriterConfig, client *ent.Client, system *SystemService) *ManagedRequestBodyWriter {
	config = config.withDefaults()
	idle := make(chan struct{})
	close(idle)
	return &ManagedRequestBodyWriter{
		config:               config,
		ent:                  client,
		system:               system,
		mu:                   sync.Mutex{},
		started:              false,
		stopping:             false,
		reservedItems:        0,
		reservedBytes:        0,
		reservations:         make(map[*managedRequestBodyReservation]struct{}),
		idle:                 idle,
		jobs:                 make(chan managedRequestBodyJob, config.MaxItems),
		workerCtx:            nil,
		cancelWorkers:        nil,
		liveWorkers:          0,
		workersDone:          make(chan struct{}),
		stopDone:             make(chan struct{}),
		stopOnce:             sync.Once{},
		beforePersistForTest: nil,
	}
}

// SetBeforePersistHookForTest installs a deterministic worker barrier for
// cross-package critical-path tests.
func (w *ManagedRequestBodyWriter) SetBeforePersistHookForTest(hook func(context.Context)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.beforePersistForTest = hook
}

// Start launches the configured process-owned workers.
func (w *ManagedRequestBodyWriter) Start(_ context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.started {
		return nil
	}
	if w.stopping {
		return errors.New("managed request-body writer is stopping")
	}
	w.workerCtx, w.cancelWorkers = context.WithCancel(context.Background())
	w.started = true
	w.liveWorkers = w.config.Workers
	for workerID := range w.config.Workers {
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					log.Error(context.Background(), "Managed request-body writer panicked",
						log.Int("worker_id", workerID), log.Any("panic", recovered), log.String("stack", string(debug.Stack())))
				}
				w.workerExited()
			}()
			w.runWorker(workerID)
		}()
	}
	return nil
}

func (w *ManagedRequestBodyWriter) workerExited() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.liveWorkers--
	if w.liveWorkers == 0 {
		close(w.workersDone)
	}
}

func (w *ManagedRequestBodyWriter) reserve(body []byte) (*managedRequestBodyReservation, string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.started {
		return nil, managedRequestBodyRejectNotStarted
	}
	if w.stopping {
		return nil, managedRequestBodyRejectStopping
	}
	byteLength := int64(len(body))
	maxBytes := int64(w.config.MaxBytesMiB) << 20
	if byteLength > maxBytes {
		return nil, managedRequestBodyRejectItemTooLarge
	}
	if w.reservedItems >= w.config.MaxItems || w.reservedBytes+byteLength > maxBytes {
		return nil, managedRequestBodyRejectCapacity
	}
	if w.reservedItems == 0 {
		w.idle = make(chan struct{})
	}
	w.reservedItems++
	w.reservedBytes += byteLength
	reservation := &managedRequestBodyReservation{
		writer: w, body: append([]byte(nil), body...), byteLength: byteLength,
		state: managedRequestBodyReservationProducer,
	}
	w.reservations[reservation] = struct{}{}
	return reservation, ""
}

func (w *ManagedRequestBodyWriter) releaseReservation(reservation *managedRequestBodyReservation) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.releaseReservationLocked(reservation, managedRequestBodyReservationReleased)
}

func (w *ManagedRequestBodyWriter) releaseReservationLocked(reservation *managedRequestBodyReservation, finalState managedRequestBodyReservationState) {
	if reservation.state == managedRequestBodyReservationReleased || reservation.state == managedRequestBodyReservationCanceled {
		return
	}
	clear(reservation.body)
	reservation.body = nil
	delete(w.reservations, reservation)
	reservation.state = finalState
	w.reservedItems--
	w.reservedBytes -= reservation.byteLength
	if w.reservedItems < 0 || w.reservedBytes < 0 {
		panic("managed request-body writer reservation accounting became negative")
	}
	if w.reservedItems == 0 {
		if w.reservedBytes != 0 {
			panic("managed request-body writer reached idle with reserved bytes")
		}
		close(w.idle)
	}
}

func (w *ManagedRequestBodyWriter) submit(reservation *managedRequestBodyReservation, target managedRequestBodyTarget) {
	w.mu.Lock()
	switch reservation.state {
	case managedRequestBodyReservationProducer:
		reservation.state = managedRequestBodyReservationQueued
		// A successful reservation owns one of MaxItems queue positions until
		// completion. Sending while holding mu makes transfer atomic with Stop.
		w.jobs <- managedRequestBodyJob{target: target, reservation: reservation}
		w.mu.Unlock()
		return
	case managedRequestBodyReservationCanceled:
		w.mu.Unlock()
		w.markTerminalTargets([]managedRequestBodyTarget{target}, "unavailable", managedRequestBodyStopped)
		return
	default:
		w.mu.Unlock()
		return
	}
}

func (w *ManagedRequestBodyWriter) runWorker(_ int) {
	for {
		select {
		case <-w.workerCtx.Done():
			return
		case job := <-w.jobs:
			if !w.claimJob(job.reservation) {
				continue
			}
			w.processJob(job)
		}
	}
}

func (w *ManagedRequestBodyWriter) claimJob(reservation *managedRequestBodyReservation) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if reservation.state != managedRequestBodyReservationQueued {
		return false
	}
	reservation.state = managedRequestBodyReservationWorker
	return true
}

func (w *ManagedRequestBodyWriter) processJob(job managedRequestBodyJob) {
	defer job.reservation.release()
	if hook := w.beforePersistHook(); hook != nil {
		hook(w.workerCtx)
	}

	var lastPayloadID int
	var lastErr error
	for attempt := 1; attempt <= w.config.MaxAttempts; attempt++ {
		if w.workerCtx.Err() != nil {
			lastErr = w.workerCtx.Err()
			break
		}
		attemptCtx, cancel := context.WithTimeout(w.workerCtx, w.config.AttemptTimeout)
		attemptCtx = authz.WithSystemBypass(attemptCtx, "managed-request-body-writer")
		payloadID, err := w.writeAttempt(attemptCtx, job.target, job.reservation.body)
		cancel()
		if payloadID != 0 {
			lastPayloadID = payloadID
		}
		if err == nil {
			return
		}
		lastErr = err
	}

	if lastPayloadID != 0 {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), w.config.AttemptTimeout)
		w.discardUnreferencedManagedPayload(cleanupCtx, lastPayloadID)
		cancel()
	}
	terminalClass := managedRequestBodyRetryExhausted
	if w.workerCtx.Err() != nil {
		terminalClass = managedRequestBodyStopped
	}
	w.markTerminalTargets([]managedRequestBodyTarget{job.target}, "unavailable", terminalClass)
	if terminalClass == managedRequestBodyStopped {
		return
	}
	recordCtx, cancel := context.WithTimeout(context.Background(), w.config.AttemptTimeout)
	w.system.RecordManagedObservabilityFailure(authz.WithSystemBypass(recordCtx, "managed-request-body-failure-state"), "async_request_body", "failed")
	cancel()
	metrics.RecordManagedObservabilityAdmissionSkippedComponent(context.Background(), "write_failed", "async_request_body")
	log.Warn(context.Background(), "Managed request-body async persistence exhausted retries",
		log.Int("request_id", job.target.requestID), log.Int("target_id", job.target.targetID), log.Cause(lastErr))
}

func (w *ManagedRequestBodyWriter) beforePersistHook() func(context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.beforePersistForTest
}

func (w *ManagedRequestBodyWriter) writeAttempt(ctx context.Context, target managedRequestBodyTarget, body []byte) (int, error) {
	pointer, exists, err := w.requestBodyPointer(ctx, target)
	if err != nil {
		return 0, err
	}
	if !exists || pointer != nil {
		return 0, nil
	}

	managed, err := persistManagedRequestBody(ctx, w.system, target.requestID, objects.JSONRawMessage(body))
	if err != nil {
		return 0, err
	}
	if managed.skipped {
		metrics.RecordManagedObservabilityAdmissionSkippedComponent(ctx, "capacity_pressure", "async_request_body")
		w.markTerminalTargets([]managedRequestBodyTarget{target}, "omitted", managedRequestBodyCapacityPressure)
		return 0, nil
	}
	if managed.payload == nil {
		return 0, errors.New("managed request-body persistence returned no payload")
	}

	exists, err = w.attachRequestBodyPointer(ctx, target, managed.payload.ID)
	if err != nil {
		return managed.payload.ID, err
	}
	if !exists && !managed.reused {
		w.discardUnreferencedManagedPayload(ctx, managed.payload.ID)
	}
	return managed.payload.ID, nil
}

func (w *ManagedRequestBodyWriter) requestBodyPointer(ctx context.Context, target managedRequestBodyTarget) (*int, bool, error) {
	client := w.system.entFromContext(ctx)
	switch target.kind {
	case managedRequestBodyTargetRequest:
		row, err := client.Request.Query().Where(request.IDEQ(target.targetID)).
			Select(request.FieldID, request.FieldRequestBodyPayloadID).Only(ctx)
		if ent.IsNotFound(err) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("load managed request-body target request %d: %w", target.targetID, err)
		}
		return row.RequestBodyPayloadID, true, nil
	case managedRequestBodyTargetExecution:
		row, err := client.RequestExecution.Query().Where(requestexecution.IDEQ(target.targetID)).
			Select(requestexecution.FieldID, requestexecution.FieldRequestBodyPayloadID).Only(ctx)
		if ent.IsNotFound(err) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, fmt.Errorf("load managed request-body target execution %d: %w", target.targetID, err)
		}
		return row.RequestBodyPayloadID, true, nil
	default:
		return nil, false, fmt.Errorf("unknown managed request-body target kind %d", target.kind)
	}
}

func (w *ManagedRequestBodyWriter) attachRequestBodyPointer(ctx context.Context, target managedRequestBodyTarget, payloadID int) (bool, error) {
	client := w.system.entFromContext(ctx)
	var err error
	switch target.kind {
	case managedRequestBodyTargetRequest:
		_, err = client.Request.UpdateOneID(target.targetID).SetRequestBodyPayloadID(payloadID).Save(ctx)
	case managedRequestBodyTargetExecution:
		_, err = client.RequestExecution.UpdateOneID(target.targetID).SetRequestBodyPayloadID(payloadID).Save(ctx)
	default:
		return false, fmt.Errorf("unknown managed request-body target kind %d", target.kind)
	}
	if ent.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return true, fmt.Errorf("attach managed request-body payload %d to target %d: %w", payloadID, target.targetID, err)
	}
	return true, nil
}

func (w *ManagedRequestBodyWriter) discardUnreferencedManagedPayload(ctx context.Context, payloadID int) {
	requestService := &RequestService{
		AbstractService:          &AbstractService{db: w.ent},
		SystemService:            w.system,
		UsageLogService:          nil,
		DataStorageService:       nil,
		LiveStreamRegistry:       nil,
		ManagedRequestBodyWriter: nil,
		channelCache:             nil,
	}
	requestService.discardUnreferencedManagedPayload(authz.WithSystemBypass(ctx, "managed-request-body-orphan-cleanup"), payloadID)
}

func (w *ManagedRequestBodyWriter) markTerminalTargets(targets []managedRequestBodyTarget, outcome, failureClass string) {
	if len(targets) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), w.config.AttemptTimeout)
	defer cancel()
	ctx = authz.WithSystemBypass(ctx, "managed-request-body-terminal-disposition")
	if err := w.updateTerminalTargets(ctx, targets, outcome, failureClass); err != nil {
		log.Warn(context.Background(), "Failed to mark managed request-body terminal disposition",
			log.String("failure_class", failureClass), log.Cause(err))
	}
}

func (w *ManagedRequestBodyWriter) updateTerminalTargets(ctx context.Context, targets []managedRequestBodyTarget, outcome, failureClass string) error {
	requestIDs := make([]int, 0, len(targets))
	executionIDs := make([]int, 0, len(targets))
	for _, target := range targets {
		switch target.kind {
		case managedRequestBodyTargetRequest:
			requestIDs = append(requestIDs, target.targetID)
		case managedRequestBodyTargetExecution:
			executionIDs = append(executionIDs, target.targetID)
		}
	}
	client := w.system.entFromContext(ctx)
	capturedAt := time.Now().UTC()
	if len(requestIDs) > 0 {
		_, err := client.Request.Update().Where(
			request.IDIn(requestIDs...),
			request.RequestBodyPayloadIDIsNil(),
			func(selector *entsql.Selector) {
				selector.Where(sqljson.ValueEQ(request.FieldEvidenceDisposition, managedRequestBodyAsyncPending,
					sqljson.Path("requestBody", "failureClass"), sqljson.Unquote(true)))
			},
		).Modify(managedTerminalDispositionModifier(request.FieldEvidenceDisposition, outcome, failureClass, capturedAt)).Save(ctx)
		if err != nil {
			return fmt.Errorf("mark parent request-body terminal disposition: %w", err)
		}
	}
	if len(executionIDs) > 0 {
		_, err := client.RequestExecution.Update().Where(
			requestexecution.IDIn(executionIDs...),
			requestexecution.RequestBodyPayloadIDIsNil(),
			func(selector *entsql.Selector) {
				selector.Where(sqljson.ValueEQ(requestexecution.FieldEvidenceDisposition, managedRequestBodyAsyncPending,
					sqljson.Path("requestBody", "failureClass"), sqljson.Unquote(true)))
			},
		).Modify(managedTerminalDispositionModifier(requestexecution.FieldEvidenceDisposition, outcome, failureClass, capturedAt)).Save(ctx)
		if err != nil {
			return fmt.Errorf("mark execution request-body terminal disposition: %w", err)
		}
	}
	return nil
}

// managedTerminalDispositionModifier atomically changes only requestBody's
// terminal fields. Response/chunk dispositions written concurrently remain
// untouched, and the async_pending predicate provides compare-and-swap
// ownership of the transition.
func managedTerminalDispositionModifier(column, outcome, failureClass string, capturedAt time.Time) func(*entsql.UpdateBuilder) {
	return func(update *entsql.UpdateBuilder) {
		update.Set(column, entsql.ExprFunc(func(builder *entsql.Builder) {
			switch builder.Dialect() {
			case dialect.Postgres:
				builder.WriteString("jsonb_set(jsonb_set(jsonb_set(jsonb_set(").Ident(column).
					WriteString(", '{requestBody,location}', ").Arg(`"none"`).WriteString("::jsonb, true)").
					WriteString(", '{requestBody,outcome}', ").Arg(strconv.Quote(outcome)).WriteString("::jsonb, true)").
					WriteString(", '{requestBody,failureClass}', ").Arg(strconv.Quote(failureClass)).WriteString("::jsonb, true)").
					WriteString(", '{requestBody,capturedAt}', ").Arg(strconv.Quote(capturedAt.Format(time.RFC3339Nano))).WriteString("::jsonb, true)")
			case dialect.MySQL:
				builder.WriteString("JSON_SET(").Ident(column).
					WriteString(", '$.requestBody.location', ").Arg("none").
					WriteString(", '$.requestBody.outcome', ").Arg(outcome).
					WriteString(", '$.requestBody.failureClass', ").Arg(failureClass).
					WriteString(", '$.requestBody.capturedAt', ").Arg(capturedAt.Format(time.RFC3339Nano)).WriteString(")")
			default:
				builder.WriteString("json_set(").Ident(column).
					WriteString(", '$.requestBody.location', ").Arg("none").
					WriteString(", '$.requestBody.outcome', ").Arg(outcome).
					WriteString(", '$.requestBody.failureClass', ").Arg(failureClass).
					WriteString(", '$.requestBody.capturedAt', ").Arg(capturedAt.Format(time.RFC3339Nano)).WriteString(")")
			}
		}))
	}
}

// Stop rejects new reservations, drains accepted work until ctx expires, then
// cancels all independent attempt contexts. It is safe to call repeatedly.
func (w *ManagedRequestBodyWriter) Stop(ctx context.Context) error {
	w.stopOnce.Do(func() {
		w.mu.Lock()
		w.stopping = true
		started := w.started
		idle := w.idle
		w.mu.Unlock()

		if !started {
			close(w.stopDone)
			return
		}

		select {
		case <-idle:
			w.cancelWorkers()
			<-w.workersDone
			close(w.stopDone)
		case <-ctx.Done():
			w.cancelProducerReservations()
			w.cancelWorkers()
			<-w.workersDone
			jobs := w.takeQueuedJobs()
			targets := make([]managedRequestBodyTarget, 0, len(jobs))
			for _, job := range jobs {
				targets = append(targets, job.target)
			}
			w.markTerminalTargets(targets, "unavailable", managedRequestBodyStopped)
			for _, job := range jobs {
				job.reservation.release()
			}
			close(w.stopDone)
		}
	})
	select {
	case <-w.stopDone:
		if err := ctx.Err(); err != nil {
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *ManagedRequestBodyWriter) cancelProducerReservations() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for reservation := range w.reservations {
		if reservation.state == managedRequestBodyReservationProducer {
			w.releaseReservationLocked(reservation, managedRequestBodyReservationCanceled)
		}
	}
}

func (w *ManagedRequestBodyWriter) takeQueuedJobs() []managedRequestBodyJob {
	jobs := make([]managedRequestBodyJob, 0, len(w.jobs))
	for {
		select {
		case job := <-w.jobs:
			jobs = append(jobs, job)
		default:
			return jobs
		}
	}
}
