package gc

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/managedobservabilitystate"
	"github.com/looplj/axonhub/internal/ent/observabilitypayload"
	"github.com/looplj/axonhub/internal/ent/predicate"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/ent/thread"
	"github.com/looplj/axonhub/internal/ent/trace"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/metrics"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

const managedObservabilityGCAdvisoryKey int64 = 0x41584f4e4f425347 // "AXONOBSG"

const (
	managedRequestSkeletonChargeBytes   int64 = 64 << 10
	managedExecutionSkeletonChargeBytes int64 = 64 << 10
	managedUsageLogChargeBytes          int64 = 4 << 10
	managedTraceChargeBytes             int64 = 4 << 10
	managedThreadChargeBytes            int64 = 4 << 10
)

func (w *Worker) recordManagedObservabilityFailure(ctx context.Context, component, reason string) {
	if w.SystemService != nil {
		w.SystemService.RecordManagedObservabilityFailure(ctx, component, reason)
		return
	}
	metrics.RecordManagedObservabilityFailure(ctx, component, reason)
}

// runCleanup puts retention, capacity, manual and vacuum work behind one
// database-coordinated owner. Failure to acquire ownership is an observability
// skip, never a forwarding/readiness failure.
func (w *Worker) runCleanup(ctx context.Context, manual bool, manualDays map[string]int) {
	acquired, err := w.withGCOwnership(ctx, func() { w.runCleanupOwned(ctx, manual, manualDays) })
	if err != nil {
		w.recordManagedObservabilityFailure(ctx, "gc_owner_lock", "failed")
		log.Error(ctx, "GC ownership check failed; cleanup will retry later",
			log.String("signal", "managed_observability_gc_owner_error"), log.Cause(err))
		return
	}
	if !acquired {
		log.Info(ctx, "GC already owned by another instance",
			log.String("signal", "managed_observability_gc_not_owner"))
	}
}

func (w *Worker) sqlDriver() (*entsql.Driver, bool) {
	driver := w.Ent.Driver()
	if primary, ok := driver.(interface{ PrimaryDriver() dialect.Driver }); ok {
		driver = primary.PrimaryDriver()
	}
	sqlDriver, ok := driver.(*entsql.Driver)
	return sqlDriver, ok
}

func (w *Worker) withGCOwnership(ctx context.Context, fn func()) (bool, error) {
	driver, ok := w.sqlDriver()
	if !ok || driver.Dialect() != dialect.Postgres {
		// SQLite/MySQL retain compatibility with an explicit single-process
		// bound. Only PostgreSQL is claimed to coordinate independent instances.
		if !w.capacityMu.TryLock() {
			return false, nil
		}
		defer w.capacityMu.Unlock()
		fn()
		return true, nil
	}

	conn, err := driver.DB().Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire dedicated PostgreSQL GC connection: %w", err)
	}
	defer conn.Close()
	var acquired bool
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", managedObservabilityGCAdvisoryKey).Scan(&acquired); err != nil {
		return false, fmt.Errorf("try PostgreSQL GC advisory lock: %w", err)
	}
	if !acquired {
		return false, nil
	}
	defer func() {
		var unlocked bool
		if err := conn.QueryRowContext(context.Background(), "SELECT pg_advisory_unlock($1)", managedObservabilityGCAdvisoryKey).Scan(&unlocked); err != nil || !unlocked {
			w.recordManagedObservabilityFailure(context.Background(), "gc_owner_unlock", "unconfirmed")
			log.Warn(context.Background(), "PostgreSQL GC advisory unlock was not confirmed; connection close releases ownership",
				log.String("signal", "managed_observability_gc_unlock_unconfirmed"), log.Cause(err))
		}
	}()
	fn()
	return true, nil
}

func managedCapacity(policy *biz.StoragePolicy) (hard, low int64, enabled bool) {
	if policy == nil || policy.ManagedObservabilityHardMiB == nil || policy.ManagedObservabilityLowMiB == nil {
		return 0, 0, false
	}
	return int64(*policy.ManagedObservabilityHardMiB) << 20,
		int64(*policy.ManagedObservabilityLowMiB) << 20, true
}

func jsonSize(value any) int64 {
	encoded, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return int64(len(encoded))
}

// managedNonPayloadCharge covers the explicit managed-observability allowlist
// without ever selecting the giant legacy request_body columns. Fixed
// per-skeleton margins are combined with the actual variable response/header
// JSON sizes selected in bounded batches.
func (w *Worker) managedNonPayloadCharge(ctx context.Context, client *ent.Client) (int64, error) {
	batch := w.getBatchSize()
	var charged int64
	lastID := 0
	for {
		rows, err := client.Request.Query().Where(request.ManagedObservabilityEQ(true), request.IDGT(lastID)).
			Select(request.FieldID, request.FieldRequestHeaders, request.FieldResponseBody, request.FieldResponseChunks,
				request.FieldEvidenceDisposition, request.FieldRoutingContext, request.FieldContentStorageKey).
			Order(ent.Asc(request.FieldID)).Limit(batch).All(ctx)
		if err != nil {
			return 0, err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			charged += managedRequestSkeletonChargeBytes + int64(len(row.RequestHeaders)+len(row.ResponseBody))
			charged += jsonSize(row.ResponseChunks) + jsonSize(row.EvidenceDisposition) + jsonSize(row.RoutingContext)
			if row.ContentStorageKey != nil {
				charged += int64(len(*row.ContentStorageKey))
			}
		}
		lastID = rows[len(rows)-1].ID
	}

	lastID = 0
	for {
		rows, err := client.RequestExecution.Query().Where(requestexecution.ManagedObservabilityEQ(true), requestexecution.IDGT(lastID)).
			Select(requestexecution.FieldID, requestexecution.FieldRequestHeaders, requestexecution.FieldResponseBody,
				requestexecution.FieldResponseChunks, requestexecution.FieldEvidenceDisposition,
				requestexecution.FieldErrorMessage, requestexecution.FieldRequestURL).
			Order(ent.Asc(requestexecution.FieldID)).Limit(batch).All(ctx)
		if err != nil {
			return 0, err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			charged += managedExecutionSkeletonChargeBytes + int64(len(row.RequestHeaders)+len(row.ResponseBody))
			charged += jsonSize(row.ResponseChunks) + jsonSize(row.EvidenceDisposition)
			charged += int64(len(row.ErrorMessage) + len(row.RequestURL))
		}
		lastID = rows[len(rows)-1].ID
	}

	lastID = 0
	for {
		rows, err := client.UsageLog.Query().Where(
			usagelog.IDGT(lastID),
			usagelog.HasRequestWith(request.ManagedObservabilityEQ(true)),
		).Select(usagelog.FieldID, usagelog.FieldModelID, usagelog.FieldFormat, usagelog.FieldCostItems, usagelog.FieldCostPriceReferenceID).
			Order(ent.Asc(usagelog.FieldID)).Limit(batch).All(ctx)
		if err != nil {
			return 0, err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			charged += managedUsageLogChargeBytes + int64(len(row.ModelID)+len(row.Format)+len(row.CostPriceReferenceID)) + jsonSize(row.CostItems)
		}
		lastID = rows[len(rows)-1].ID
	}

	traceCount, err := client.Trace.Query().Where(trace.HasRequestsWith(request.ManagedObservabilityEQ(true))).Count(ctx)
	if err != nil {
		return 0, err
	}
	threadCount, err := client.Thread.Query().Where(thread.HasTracesWith(trace.HasRequestsWith(request.ManagedObservabilityEQ(true)))).Count(ctx)
	if err != nil {
		return 0, err
	}
	charged += int64(traceCount)*managedTraceChargeBytes + int64(threadCount)*managedThreadChargeBytes
	return charged, nil
}

func (w *Worker) reconcileManagedState(ctx context.Context, hard int64) (*ent.ManagedObservabilityState, error) {
	tx, err := w.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	client := tx.Client()
	txCtx := ent.NewContext(ent.NewTxContext(ctx, tx), client)
	if err := client.ManagedObservabilityState.Create().SetID(1).SetChargedBytes(0).
		OnConflictColumns(managedobservabilitystate.FieldID).Ignore().Exec(txCtx); err != nil {
		return nil, err
	}
	stateQuery := client.ManagedObservabilityState.Query().Where(managedobservabilitystate.IDEQ(1))
	if client.Driver().Dialect() == dialect.Postgres || client.Driver().Dialect() == dialect.MySQL {
		stateQuery.Modify(func(selector *entsql.Selector) { selector.ForUpdate() })
	}
	state, err := stateQuery.Only(txCtx)
	if err != nil {
		return nil, err
	}
	payloads, err := client.ObservabilityPayload.Query().Select(observabilitypayload.FieldChargedBytes).All(txCtx)
	if err != nil {
		return nil, fmt.Errorf("list managed observability payload charges: %w", err)
	}
	var charged int64
	for _, payload := range payloads {
		charged += payload.ChargedBytes
	}
	nonPayloadCharge, err := w.managedNonPayloadCharge(txCtx, client)
	if err != nil {
		return nil, fmt.Errorf("charge managed observability skeletons: %w", err)
	}
	charged += nonPayloadCharge
	state, err = client.ManagedObservabilityState.UpdateOneID(1).
		SetChargedBytes(charged).
		SetUnderPressure(state.UnderPressure || charged > hard).
		Save(txCtx)
	if err != nil {
		return nil, fmt.Errorf("reconcile managed observability state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return state, nil
}

func (w *Worker) finishManagedCapacityState(ctx context.Context, low int64) (*ent.ManagedObservabilityState, error) {
	tx, err := w.Ent.Tx(ctx)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	client := tx.Client()
	txCtx := ent.NewContext(ent.NewTxContext(ctx, tx), client)
	query := client.ManagedObservabilityState.Query().Where(managedobservabilitystate.IDEQ(1))
	if client.Driver().Dialect() == dialect.Postgres || client.Driver().Dialect() == dialect.MySQL {
		query.Modify(func(selector *entsql.Selector) { selector.ForUpdate() })
	}
	state, err := query.Only(txCtx)
	if err != nil {
		return nil, err
	}
	update := client.ManagedObservabilityState.UpdateOneID(1)
	if state.ChargedBytes <= low {
		update.SetUnderPressure(false).ClearLastError()
	} else {
		update.SetUnderPressure(true).SetLastError("capacity_cleanup_incomplete")
	}
	state, err = update.Save(txCtx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return state, nil
}

func (w *Worker) capacityCandidates(ctx context.Context) ([]*ent.ObservabilityPayload, error) {
	selectMetadata := func(query *ent.ObservabilityPayloadQuery) *ent.ObservabilityPayloadQuery {
		return query.Select(
			observabilitypayload.FieldID,
			observabilitypayload.FieldCreatedAt,
			observabilitypayload.FieldRequestID,
			observabilitypayload.FieldChargedBytes,
		).Order(ent.Asc(observabilitypayload.FieldCreatedAt), ent.Asc(observabilitypayload.FieldID)).Limit(w.getBatchSize())
	}
	// Successful/canceled groups are lower diagnostic value than any group
	// containing a failed parent or execution.
	payloads, err := selectMetadata(w.Ent.ObservabilityPayload.Query().Where(
		observabilitypayload.HasRequestWith(
			request.StatusIn(request.StatusCompleted, request.StatusCanceled),
			request.Not(request.HasExecutionsWith(requestexecution.StatusIn(
				requestexecution.StatusPending,
				requestexecution.StatusProcessing,
			))),
			request.Not(request.HasExecutionsWith(requestexecution.StatusEQ(requestexecution.StatusFailed))),
		),
	)).All(ctx)
	if err != nil || len(payloads) > 0 {
		return payloads, err
	}
	return selectMetadata(w.Ent.ObservabilityPayload.Query().Where(
		observabilitypayload.HasRequestWith(
			request.StatusIn(request.StatusCompleted, request.StatusFailed, request.StatusCanceled),
			request.Not(request.HasExecutionsWith(requestexecution.StatusIn(
				requestexecution.StatusPending,
				requestexecution.StatusProcessing,
			))),
		),
	)).All(ctx)
}

func evictedDisposition(current *objects.EvidenceDisposition) *objects.EvidenceDisposition {
	if current == nil {
		current = &objects.EvidenceDisposition{Version: 1}
	} else {
		clone := *current
		current = &clone
	}
	failureClass := "capacity_evicted"
	current.RequestBody.Location = "none"
	current.RequestBody.Outcome = "evicted"
	current.RequestBody.FailureClass = &failureClass
	return current
}

type managedPayloadCleanupCandidate struct {
	id      int
	charged int64
}

func managedPayloadCleanupCandidates(ctx context.Context, client *ent.Client, requestID int) ([]managedPayloadCleanupCandidate, error) {
	rows, err := client.ObservabilityPayload.Query().
		Where(observabilitypayload.RequestIDEQ(requestID)).
		Select(observabilitypayload.FieldID, observabilitypayload.FieldChargedBytes).
		All(ctx)
	if err != nil {
		return nil, err
	}
	candidates := make([]managedPayloadCleanupCandidate, 0, len(rows))
	for _, row := range rows {
		candidates = append(candidates, managedPayloadCleanupCandidate{id: row.ID, charged: row.ChargedBytes})
	}
	return candidates, nil
}

func subtractManagedPayloadCharge(ctx context.Context, client *ent.Client, charged int64) error {
	if charged <= 0 {
		return nil
	}
	query := client.ManagedObservabilityState.Query().Where(managedobservabilitystate.IDEQ(1))
	if client.Driver().Dialect() == dialect.Postgres || client.Driver().Dialect() == dialect.MySQL {
		query.Modify(func(selector *entsql.Selector) { selector.ForUpdate() })
	}
	state, err := query.Only(ctx)
	if ent.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = client.ManagedObservabilityState.UpdateOneID(1).
		SetChargedBytes(max(int64(0), state.ChargedBytes-charged)).
		Save(ctx)
	return err
}

// cleanupUnreferencedManagedPayloads removes payloads whose last execution
// reference was just deleted. Runtime migrations disable foreign keys, so this
// application-owned cleanup is the lifecycle authority.
func cleanupUnreferencedManagedPayloads(ctx context.Context, client *ent.Client, candidates []managedPayloadCleanupCandidate) error {
	var reclaimed int64
	for _, candidate := range candidates {
		requestRef, err := client.Request.Query().Where(request.RequestBodyPayloadIDEQ(candidate.id)).Exist(ctx)
		if err != nil {
			return err
		}
		executionRef, err := client.RequestExecution.Query().Where(requestexecution.RequestBodyPayloadIDEQ(candidate.id)).Exist(ctx)
		if err != nil {
			return err
		}
		if requestRef || executionRef {
			continue
		}
		if err := client.ObservabilityPayload.DeleteOneID(candidate.id).Exec(ctx); err != nil && !ent.IsNotFound(err) {
			return err
		}
		reclaimed += candidate.charged
	}
	return subtractManagedPayloadCharge(ctx, client, reclaimed)
}

// cleanupDeletedRequestManagedPayloads removes the request-owned catalog rows
// after the skeleton is deleted. The pre-delete charge snapshot is intentional:
// it also repairs accounting if a future FK-enabled deployment cascades rows
// before the explicit delete executes.
func cleanupDeletedRequestManagedPayloads(ctx context.Context, client *ent.Client, candidates []managedPayloadCleanupCandidate) error {
	var reclaimed int64
	for _, candidate := range candidates {
		reclaimed += candidate.charged
		if err := client.ObservabilityPayload.DeleteOneID(candidate.id).Exec(ctx); err != nil && !ent.IsNotFound(err) {
			return err
		}
	}
	return subtractManagedPayloadCharge(ctx, client, reclaimed)
}

func (w *Worker) evictManagedPayload(ctx context.Context, payloadID int) (int64, error) {
	tx, err := w.Ent.Tx(ctx)
	if err != nil {
		return 0, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	client := tx.Client()
	txCtx := ent.NewContext(ent.NewTxContext(ctx, tx), client)

	payloadQuery := client.ObservabilityPayload.Query().Where(observabilitypayload.IDEQ(payloadID))
	if client.Driver().Dialect() == dialect.Postgres || client.Driver().Dialect() == dialect.MySQL {
		payloadQuery.Modify(func(selector *entsql.Selector) { selector.ForUpdate() })
	}
	payload, err := payloadQuery.Only(txCtx)
	if ent.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}

	requests, err := client.Request.Query().Where(request.RequestBodyPayloadIDEQ(payload.ID)).All(txCtx)
	if err != nil {
		return 0, err
	}
	for _, row := range requests {
		if _, err := client.Request.UpdateOneID(row.ID).
			ClearRequestBodyPayloadID().
			SetEvidenceDisposition(evictedDisposition(row.EvidenceDisposition)).
			Save(txCtx); err != nil {
			return 0, err
		}
	}
	executions, err := client.RequestExecution.Query().Where(requestexecution.RequestBodyPayloadIDEQ(payload.ID)).All(txCtx)
	if err != nil {
		return 0, err
	}
	for _, row := range executions {
		if _, err := client.RequestExecution.UpdateOneID(row.ID).
			ClearRequestBodyPayloadID().
			SetEvidenceDisposition(evictedDisposition(row.EvidenceDisposition)).
			Save(txCtx); err != nil {
			return 0, err
		}
	}
	if err := client.ObservabilityPayload.DeleteOneID(payload.ID).Exec(txCtx); err != nil {
		return 0, err
	}
	stateQuery := client.ManagedObservabilityState.Query().Where(managedobservabilitystate.IDEQ(1))
	if client.Driver().Dialect() == dialect.Postgres || client.Driver().Dialect() == dialect.MySQL {
		stateQuery.Modify(func(selector *entsql.Selector) { selector.ForUpdate() })
	}
	state, err := stateQuery.Only(txCtx)
	if err == nil {
		next := max(int64(0), state.ChargedBytes-payload.ChargedBytes)
		if _, err := client.ManagedObservabilityState.UpdateOneID(1).SetChargedBytes(next).Save(txCtx); err != nil {
			return 0, err
		}
	} else if !ent.IsNotFound(err) {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	committed = true
	return payload.ChargedBytes, nil
}

func (w *Worker) managedRequestGroupCandidates(ctx context.Context) ([]int, error) {
	base := []predicate.Request{
		request.ManagedObservabilityEQ(true),
		request.StatusIn(request.StatusCompleted, request.StatusFailed, request.StatusCanceled),
		request.Not(request.HasExecutionsWith(requestexecution.StatusIn(requestexecution.StatusPending, requestexecution.StatusProcessing))),
		request.Not(request.HasTraceWith(trace.Or(trace.StatusEQ(trace.StatusRetained), trace.HasThreadWith(thread.StatusEQ(thread.StatusRetained))))),
	}
	lowValue := append([]predicate.Request{}, base...)
	lowValue = append(lowValue,
		request.StatusIn(request.StatusCompleted, request.StatusCanceled),
		request.Not(request.HasExecutionsWith(requestexecution.StatusEQ(requestexecution.StatusFailed))),
	)
	ids, err := w.Ent.Request.Query().Where(lowValue...).Order(ent.Asc(request.FieldCreatedAt), ent.Asc(request.FieldID)).Limit(1).IDs(ctx)
	if err != nil || len(ids) > 0 {
		return ids, err
	}
	return w.Ent.Request.Query().Where(base...).Order(ent.Asc(request.FieldCreatedAt), ent.Asc(request.FieldID)).Limit(1).IDs(ctx)
}

func (w *Worker) cleanupManagedRequestGroupsBatch(ctx context.Context) (int, error) {
	ids, err := w.managedRequestGroupCandidates(ctx)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	cutoff := time.Now().UTC().Add(time.Hour)
	cache := make(map[int]*ent.DataStorage)
	deletedGroups := 0
	for _, requestID := range ids {
		usageIDs, err := w.Ent.UsageLog.Query().Where(usagelog.RequestIDEQ(requestID)).IDs(ctx)
		if err != nil {
			return deletedGroups, err
		}
		for _, usageID := range usageIDs {
			if _, err := w.deleteUsageLogCandidate(ctx, usageID, cutoff); err != nil {
				return deletedGroups, err
			}
		}
		executionIDs, err := w.Ent.RequestExecution.Query().Where(requestexecution.RequestIDEQ(requestID)).IDs(ctx)
		if err != nil {
			return deletedGroups, err
		}
		for _, executionID := range executionIDs {
			if _, err := w.deleteExecutionCandidate(ctx, executionID, cutoff, cache); err != nil {
				return deletedGroups, err
			}
		}
		deleted, err := w.deleteRequestCandidate(ctx, requestID, cutoff, cache)
		if err != nil {
			return deletedGroups, err
		}
		if deleted {
			deletedGroups++
		}
	}
	return deletedGroups, nil
}

func (w *Worker) cleanupManagedCapacity(ctx context.Context, policy *biz.StoragePolicy) error {
	hard, low, enabled := managedCapacity(policy)
	if !enabled {
		return nil
	}
	state, err := w.reconcileManagedState(ctx, hard)
	if err != nil {
		return err
	}
	if !state.UnderPressure && state.ChargedBytes <= hard {
		return nil
	}
	charged := state.ChargedBytes
	var evicted int
	var reclaimed int64
	for charged > low {
		candidates, err := w.capacityCandidates(ctx)
		if err != nil {
			return fmt.Errorf("list managed observability capacity candidates: %w", err)
		}
		if len(candidates) == 0 {
			break
		}
		batchReclaimed := int64(0)
		for _, candidate := range candidates {
			if charged <= low {
				break
			}
			bytes, err := w.evictManagedPayload(ctx, candidate.ID)
			if err != nil {
				return fmt.Errorf("evict managed observability payload %d: %w", candidate.ID, err)
			}
			if bytes > 0 {
				charged -= bytes
				reclaimed += bytes
				batchReclaimed += bytes
				evicted++
			}
		}
		if batchReclaimed == 0 {
			break
		}
	}
	for charged > low {
		deletedGroups, err := w.cleanupManagedRequestGroupsBatch(ctx)
		if err != nil {
			return fmt.Errorf("cleanup managed observability request groups: %w", err)
		}
		if deletedGroups == 0 {
			break
		}
		state, err = w.reconcileManagedState(ctx, hard)
		if err != nil {
			return err
		}
		charged = state.ChargedBytes
	}
	finalState, err := w.finishManagedCapacityState(ctx, low)
	if err != nil {
		return err
	}
	charged = finalState.ChargedBytes
	log.Info(ctx, "Managed observability capacity cleanup completed",
		log.String("signal", "managed_observability_capacity_gc"),
		log.Int("evicted_payloads", evicted),
		log.Int64("reclaimed_charged_bytes", reclaimed),
		log.Int64("remaining_charged_bytes", max(int64(0), charged)),
		log.Int64("hard_bytes", hard), log.Int64("low_bytes", low),
		log.Time("completed_at", time.Now().UTC()))
	metrics.RecordManagedObservabilityCapacity(ctx, max(int64(0), charged), evicted)
	return nil
}
