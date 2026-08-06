package gc

import (
	"context"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"entgo.io/ent/dialect"
	"go.uber.org/fx"

	entsql "entgo.io/ent/dialect/sql"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channelprobe"
	"github.com/looplj/axonhub/internal/ent/datastorage"
	"github.com/looplj/axonhub/internal/ent/predicate"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/requestexecution"
	"github.com/looplj/axonhub/internal/ent/schema/schematype"
	"github.com/looplj/axonhub/internal/ent/thread"
	"github.com/looplj/axonhub/internal/ent/trace"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/log"
	"github.com/looplj/axonhub/internal/server/biz"
	serverdb "github.com/looplj/axonhub/internal/server/db"
	"github.com/looplj/axonhub/internal/server/scheduler"
)

var defaultBatchSize = 500

type TriggerGcCleanupInput struct {
	RequestsCleanupDays  int `json:"requests_cleanup_days"`
	UsageLogsCleanupDays int `json:"usage_logs_cleanup_days"`
}

type GcCleanupPreviewItem struct {
	ResourceType   string    `json:"resource_type"`
	EstimatedCount int       `json:"estimated_count"`
	CutoffTime     time.Time `json:"cutoff_time"`
	RetentionDays  int       `json:"retention_days"`
}

type Config struct {
	CRON          string `json:"cron" yaml:"cron" conf:"cron" validate:"required"`
	VacuumEnabled bool   `json:"vacuum_enabled" yaml:"vacuum_enabled" conf:"vacuum_enabled"`
	VacuumFull    bool   `json:"vacuum_full" yaml:"vacuum_full" conf:"vacuum_full"`
}

type Worker struct {
	SystemService      *biz.SystemService
	DataStorageService *biz.DataStorageService
	Ent                *ent.Client
	Config             Config
	capacityMu         sync.Mutex

	// beforeCandidateDelete is a deterministic test barrier. Production workers
	// leave it nil. Eligibility is always revalidated and locked after this hook.
	beforeCandidateDelete func(resource string, id int)
}

type Params struct {
	fx.In

	Config             Config
	SystemService      *biz.SystemService
	DataStorageService *biz.DataStorageService
	Client             *ent.Client
}

func NewWorker(params Params) *Worker {
	w := &Worker{
		SystemService:      params.SystemService,
		DataStorageService: params.DataStorageService,
		Ent:                params.Client,
		Config:             params.Config,
	}

	return w
}

func (w *Worker) RegisterScheduledTasks(ctx context.Context, s *scheduler.Scheduler) error {
	if err := s.Register(ctx, scheduler.TaskSpec{
		Name:        "gc",
		Description: "Garbage collection — cleanup old requests, traces, usage logs, and channel probes",
		CronExpr:    w.Config.CRON,
		Timezone:    "UTC",
	}, w.runAutomaticCleanup); err != nil {
		return err
	}
	return s.Register(ctx, scheduler.TaskSpec{
		Name:        "managed-observability-capacity-gc",
		Description: "Non-waiting capacity reconciliation for managed observability payloads",
		FixRate:     time.Minute,
		Timezone:    "UTC",
	}, w.runAutomaticCapacityCleanup)
}

// deleteInBatches deletes records in batches to avoid memory issues.
func (w *Worker) deleteInBatches(ctx context.Context, deleteFunc func() (int, error)) (int, error) {
	totalDeleted := 0

	for {
		deleted, err := deleteFunc()
		if err != nil {
			return totalDeleted, fmt.Errorf("failed to delete batch: %w", err)
		}

		if deleted == 0 {
			break
		}

		totalDeleted += deleted
		log.Debug(ctx, "Deleted batch of records", log.Int("batch_size", deleted), log.Int("total_deleted", totalDeleted))
	}

	return totalDeleted, nil
}

// getBatchSize returns the appropriate batch size for cleanup operations.
func (w *Worker) getBatchSize() int {
	return defaultBatchSize
}

// runCleanup executes the cleanup process based on storage policy.
// When manual is true and manualDays is provided, those days override the policy values.
func (w *Worker) runCleanupOwned(ctx context.Context, manual bool, manualDays map[string]int) {
	log.Info(ctx, "Starting cleanup process", log.Bool("manual", manual))

	ctx = ent.NewContext(ctx, w.Ent)
	ctx = schematype.SkipSoftDelete(ctx)
	// GC is a read-before-delete workflow. Replica lag must not let a stale
	// terminal/retention status authorize a delete on the primary.
	ctx = serverdb.WithPrimary(ctx)

	policy, err := w.SystemService.StoragePolicy(ctx)
	if err != nil {
		log.Error(ctx, "Failed to get storage policy for cleanup", log.Cause(err))
		return
	}

	log.Debug(ctx, "Storage policy for cleanup", log.Any("policy", policy))

	for _, option := range policy.CleanupOptions {
		if option.Enabled || manual {
			if manual && manualDays != nil {
				if _, ok := manualDays[option.ResourceType]; !ok {
					continue
				}
			}
			days := option.CleanupDays
			if manual && manualDays != nil {
				if d, ok := manualDays[option.ResourceType]; ok {
					days = d
				}
			}
			switch option.ResourceType {
			case "requests":
				err := w.cleanupRequests(ctx, days, manual)
				if err != nil {
					log.Error(ctx, "Failed to cleanup requests",
						log.String("resource", option.ResourceType),
						log.Cause(err))
				} else {
					log.Info(ctx, "Successfully cleaned up requests",
						log.String("resource", option.ResourceType),
						log.Int("cleanup_days", days))
				}

				err = w.cleanupThreads(ctx, days, manual)
				if err != nil {
					log.Error(ctx, "Failed to cleanup threads",
						log.String("resource", "threads"),
						log.Cause(err))
				} else {
					log.Info(ctx, "Successfully cleaned up threads",
						log.String("resource", "threads"),
						log.Int("cleanup_days", days))
				}

				err = w.cleanupTraces(ctx, days, manual)
				if err != nil {
					log.Error(ctx, "Failed to cleanup traces",
						log.String("resource", "traces"),
						log.Cause(err))
				} else {
					log.Info(ctx, "Successfully cleaned up traces",
						log.String("resource", "traces"),
						log.Int("cleanup_days", days))
				}
			case "usage_logs":
				err := w.cleanupUsageLogs(ctx, days, manual)
				if err != nil {
					log.Error(ctx, "Failed to cleanup usage logs",
						log.String("resource", option.ResourceType),
						log.Cause(err))
				} else {
					log.Info(ctx, "Successfully cleaned up usage logs",
						log.String("resource", option.ResourceType),
						log.Int("cleanup_days", days))
				}
			default:
				log.Warn(ctx, "Unknown resource type for cleanup",
					log.String("resource", option.ResourceType))
			}
		}
	}

	err = w.cleanupChannelProbes(ctx, 3, manual)
	if err != nil {
		log.Error(ctx, "Failed to cleanup channel probes",
			log.Cause(err))
	} else {
		log.Info(ctx, "Successfully cleaned up channel probes",
			log.Int("cleanup_days", 3))
	}

	if err := w.cleanupManagedCapacity(ctx, policy); err != nil {
		w.recordManagedObservabilityFailure(ctx, "capacity_cleanup", "failed")
		log.Error(ctx, "Managed observability capacity cleanup failed; traffic remains available",
			log.String("signal", "managed_observability_cleanup_failed"), log.Cause(err))
	}

	if w.Config.VacuumEnabled {
		if err := w.runVacuum(ctx); err != nil {
			log.Error(ctx, "Failed to run VACUUM after cleanup",
				log.Cause(err))
		}
	}

	log.Info(ctx, "Cleanup process completed")
}

// cleanupRequests deletes requests older than the specified number of days.
func (w *Worker) cleanupRequests(ctx context.Context, cleanupDays int, manual bool) error {
	if cleanupDays <= 0 {
		log.Debug(ctx, "No cleanup needed for requests")
		return nil
	}

	cutoffTime := time.Now().AddDate(0, 0, -cleanupDays)

	execResult, err := w.cleanupOldRequestExecutions(ctx, cutoffTime)
	if err != nil {
		return fmt.Errorf("failed to cleanup request executions: %w", err)
	}

	log.Debug(ctx, "Deleted old request executions",
		log.Int("deleted_executions_count", execResult),
		log.Time("cutoff_time", cutoffTime),
	)

	reqResult, err := w.cleanupOldRequestsRecords(ctx, cutoffTime)
	if err != nil {
		return fmt.Errorf("failed to cleanup requests: %w", err)
	}

	log.Debug(ctx, "Deleted old requests",
		log.Int("deleted_requests_count", reqResult),
		log.Time("cutoff_time", cutoffTime))

	return nil
}

func (w *Worker) cleanupOldRequestExecutions(ctx context.Context, cutoffTime time.Time) (int, error) {
	batchSize := w.getBatchSize()
	totalDeleted := 0
	cache := make(map[int]*ent.DataStorage)

	for {
		executions, err := w.Ent.RequestExecution.Query().
			Select(
				requestexecution.FieldID,
				requestexecution.FieldProjectID,
				requestexecution.FieldDataStorageID,
				requestexecution.FieldRequestID,
			).
			Where(
				requestexecution.CreatedAtLT(cutoffTime),
				requestexecution.StatusIn(
					requestexecution.StatusCompleted,
					requestexecution.StatusFailed,
					requestexecution.StatusCanceled,
				),
				requestexecution.HasRequestWith(
					request.CreatedAtLT(cutoffTime),
					request.StatusIn(request.StatusCompleted, request.StatusFailed, request.StatusCanceled),
					request.Not(request.HasTraceWith(trace.Or(
						trace.StatusEQ(trace.StatusRetained),
						trace.HasThreadWith(thread.StatusEQ(thread.StatusRetained)),
					))),
				),
			).
			Order(ent.Asc(requestexecution.FieldID)).
			Limit(batchSize).
			All(ctx)
		if err != nil {
			return totalDeleted, fmt.Errorf("failed to query old request executions: %w", err)
		}

		if len(executions) == 0 {
			break
		}

		var batchErr error
		batchDeleted := 0
		for _, exec := range executions {
			if w.beforeCandidateDelete != nil {
				w.beforeCandidateDelete("request_execution", exec.ID)
			}
			deleted, err := w.deleteExecutionCandidate(ctx, exec.ID, cutoffTime, cache)
			if err != nil {
				if batchErr == nil {
					batchErr = err
				}
				continue
			}
			if deleted {
				batchDeleted++
			}
		}

		log.Debug(ctx, "Deleted old request executions batch",
			log.Int("deleted_executions_count", batchDeleted),
			log.Time("cutoff_time", cutoffTime),
		)

		totalDeleted += batchDeleted
		if batchErr != nil {
			return totalDeleted, fmt.Errorf("request execution cleanup stopped with retryable records: %w", batchErr)
		}
		if batchDeleted == 0 {
			break
		}
	}

	return totalDeleted, nil
}

func (w *Worker) cleanupOldRequestsRecords(ctx context.Context, cutoffTime time.Time) (int, error) {
	batchSize := w.getBatchSize()
	totalDeleted := 0
	cache := make(map[int]*ent.DataStorage)

	for {
		reqs, err := w.Ent.Request.Query().
			Select(
				request.FieldID,
				request.FieldProjectID,
				request.FieldDataStorageID,
				request.FieldContentStorageID,
				request.FieldContentStorageKey,
			).
			Where(
				request.CreatedAtLT(cutoffTime),
				request.StatusIn(request.StatusCompleted, request.StatusFailed, request.StatusCanceled),
				request.Not(request.HasExecutions()),
				request.Not(request.HasTraceWith(trace.Or(
					trace.StatusEQ(trace.StatusRetained),
					trace.HasThreadWith(thread.StatusEQ(thread.StatusRetained)),
				))),
			).
			Order(ent.Asc(request.FieldID)).
			Limit(batchSize).
			All(ctx)
		if err != nil {
			return totalDeleted, fmt.Errorf("failed to query old requests: %w", err)
		}

		if len(reqs) == 0 {
			break
		}

		var batchErr error
		batchDeleted := 0
		for _, req := range reqs {
			if w.beforeCandidateDelete != nil {
				w.beforeCandidateDelete("request", req.ID)
			}
			deleted, err := w.deleteRequestCandidate(ctx, req.ID, cutoffTime, cache)
			if err != nil {
				if batchErr == nil {
					batchErr = err
				}
				continue
			}
			if deleted {
				batchDeleted++
			}
		}

		totalDeleted += batchDeleted
		if batchErr != nil {
			return totalDeleted, fmt.Errorf("request cleanup stopped with retryable records: %w", batchErr)
		}
		if batchDeleted == 0 {
			break
		}
	}

	return totalDeleted, nil
}

// deleteExecutionCandidate serializes the eligibility decision, external
// deletion, and authoritative row deletion in one database transaction. The
// conditional claim updates acquire write locks on every mutable owner in the
// same order used by retention changes (thread -> trace -> request -> execution).
// This works across PostgreSQL/MySQL row locks and SQLite's database write lock.
func (w *Worker) deleteExecutionCandidate(ctx context.Context, executionID int, cutoffTime time.Time, cache map[int]*ent.DataStorage) (deleted bool, err error) {
	tx, err := w.Ent.Tx(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to start request execution cleanup transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	txClient := tx.Client()
	txCtx := ent.NewTxContext(ctx, tx)
	txCtx = ent.NewContext(txCtx, txClient)

	exec, err := txClient.RequestExecution.Query().
		Where(requestexecution.IDEQ(executionID)).
		Select(
			requestexecution.FieldID,
			requestexecution.FieldCreatedAt,
			requestexecution.FieldUpdatedAt,
			requestexecution.FieldProjectID,
			requestexecution.FieldRequestID,
			requestexecution.FieldDataStorageID,
			requestexecution.FieldStatus,
		).
		Only(txCtx)
	if ent.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to reload request execution %d for cleanup: %w", executionID, err)
	}

	lockedRequest, eligible, err := lockRequestCandidate(txCtx, txClient, exec.RequestID, cutoffTime, false)
	if err != nil || !eligible {
		return false, err
	}
	requestRow := lockedRequest.row

	updated, err := txClient.RequestExecution.Update().
		Where(
			requestexecution.IDEQ(exec.ID),
			requestexecution.RequestIDEQ(requestRow.ID),
			requestexecution.CreatedAtLT(cutoffTime),
			requestexecution.StatusIn(
				requestexecution.StatusCompleted,
				requestexecution.StatusFailed,
				requestexecution.StatusCanceled,
			),
			requestexecution.UpdatedAtEQ(exec.UpdatedAt),
		).
		SetUpdatedAt(gcClaimTimestamp()).
		Save(txCtx)
	if err != nil {
		return false, fmt.Errorf("failed to lock request execution %d for cleanup: %w", exec.ID, err)
	}
	if updated != 1 {
		return false, nil
	}

	if err := w.cleanupExecutionExternalStorage(txCtx, exec, cache); err != nil {
		return false, err
	}
	payloadCandidates, err := managedPayloadCleanupCandidates(txCtx, txClient, exec.RequestID)
	if err != nil {
		return false, fmt.Errorf("failed to list managed payloads for request execution %d: %w", exec.ID, err)
	}
	if err := txClient.RequestExecution.DeleteOneID(exec.ID).Exec(txCtx); err != nil {
		return false, fmt.Errorf("failed to delete request execution %d: %w", exec.ID, err)
	}
	if err := cleanupUnreferencedManagedPayloads(txCtx, txClient, payloadCandidates); err != nil {
		return false, fmt.Errorf("failed to cleanup managed payloads after request execution %d: %w", exec.ID, err)
	}
	if err := lockedRequest.restore(txCtx, txClient, true); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("failed to commit request execution %d cleanup: %w", exec.ID, err)
	}
	committed = true

	return true, nil
}

func (w *Worker) deleteRequestCandidate(ctx context.Context, requestID int, cutoffTime time.Time, cache map[int]*ent.DataStorage) (deleted bool, err error) {
	tx, err := w.Ent.Tx(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to start request cleanup transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	txClient := tx.Client()
	txCtx := ent.NewTxContext(ctx, tx)
	txCtx = ent.NewContext(txCtx, txClient)

	lockedRequest, eligible, err := lockRequestCandidate(txCtx, txClient, requestID, cutoffTime, true)
	if err != nil || !eligible {
		return false, err
	}
	req := lockedRequest.row
	if err := w.cleanupRequestExternalStorage(txCtx, req, cache); err != nil {
		return false, err
	}
	payloadCandidates, err := managedPayloadCleanupCandidates(txCtx, txClient, req.ID)
	if err != nil {
		return false, fmt.Errorf("failed to list managed payloads for request %d: %w", req.ID, err)
	}
	if err := txClient.Request.DeleteOneID(req.ID).Exec(txCtx); err != nil {
		return false, fmt.Errorf("failed to delete request %d: %w", req.ID, err)
	}
	if err := cleanupDeletedRequestManagedPayloads(txCtx, txClient, payloadCandidates); err != nil {
		return false, fmt.Errorf("failed to cleanup managed payloads after request %d: %w", req.ID, err)
	}
	if err := lockedRequest.restore(txCtx, txClient, false); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("failed to commit request %d cleanup: %w", req.ID, err)
	}
	committed = true

	return true, nil
}

type lockedRetentionOwners struct {
	traceID         int
	traceUpdatedAt  time.Time
	threadID        int
	threadUpdatedAt time.Time
}

type lockedRequestCleanup struct {
	row              *ent.Request
	requestUpdatedAt time.Time
	owners           lockedRetentionOwners
}

func gcClaimTimestamp() time.Time {
	// A changed value is required because MySQL reports changed rather than
	// matched rows for no-op updates by default. The claim is restored before a
	// successful commit for owner/request rows that remain.
	return time.Now().UTC().Add(24 * time.Hour)
}

func (l *lockedRequestCleanup) restore(ctx context.Context, client *ent.Client, restoreRequest bool) error {
	if restoreRequest {
		if _, err := client.Request.UpdateOneID(l.row.ID).SetUpdatedAt(l.requestUpdatedAt).Save(ctx); err != nil {
			return fmt.Errorf("failed to restore request %d cleanup claim: %w", l.row.ID, err)
		}
	}
	return l.owners.restore(ctx, client, true, true)
}

func (o lockedRetentionOwners) restore(ctx context.Context, client *ent.Client, restoreTrace, restoreThread bool) error {
	if restoreTrace && o.traceID != 0 {
		if _, err := client.Trace.UpdateOneID(o.traceID).SetUpdatedAt(o.traceUpdatedAt).Save(ctx); err != nil {
			return fmt.Errorf("failed to restore trace %d cleanup claim: %w", o.traceID, err)
		}
	}
	if restoreThread && o.threadID != 0 {
		if _, err := client.Thread.UpdateOneID(o.threadID).SetUpdatedAt(o.threadUpdatedAt).Save(ctx); err != nil {
			return fmt.Errorf("failed to restore thread %d cleanup claim: %w", o.threadID, err)
		}
	}

	return nil
}

func lockRequestCandidate(ctx context.Context, client *ent.Client, requestID int, cutoffTime time.Time, requireNoExecutions bool) (*lockedRequestCleanup, bool, error) {
	req, err := queryRequestCleanupFields(ctx, client, requestID)
	if ent.IsNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("failed to reload request %d for cleanup: %w", requestID, err)
	}

	owners, ownersLocked, err := lockRetentionOwners(ctx, client, req.TraceID)
	if err != nil || !ownersLocked {
		return nil, false, err
	}

	predicates := []predicate.Request{
		request.IDEQ(req.ID),
		request.CreatedAtLT(cutoffTime),
		request.StatusIn(request.StatusCompleted, request.StatusFailed, request.StatusCanceled),
		request.Not(request.HasTraceWith(trace.Or(
			trace.StatusEQ(trace.StatusRetained),
			trace.HasThreadWith(thread.StatusEQ(thread.StatusRetained)),
		))),
		request.UpdatedAtEQ(req.UpdatedAt),
	}
	if requireNoExecutions {
		predicates = append(predicates, request.Not(request.HasExecutions()))
	}
	updated, err := client.Request.Update().
		Where(predicates...).
		SetUpdatedAt(gcClaimTimestamp()).
		Save(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("failed to lock request %d for cleanup: %w", req.ID, err)
	}
	if updated != 1 {
		return nil, false, nil
	}

	// Content metadata is mutable. Reload it after the request write lock is
	// acquired so external cleanup uses the authoritative key/storage pair.
	locked, err := queryRequestCleanupFields(ctx, client, requestID)
	if err != nil {
		return nil, false, fmt.Errorf("failed to reload locked request %d: %w", requestID, err)
	}

	return &lockedRequestCleanup{
		row:              locked,
		requestUpdatedAt: req.UpdatedAt,
		owners:           owners,
	}, true, nil
}

func queryRequestCleanupFields(ctx context.Context, client *ent.Client, requestID int) (*ent.Request, error) {
	selected := client.Request.Query().
		Where(request.IDEQ(requestID)).
		Select(
			request.FieldID,
			request.FieldCreatedAt,
			request.FieldUpdatedAt,
			request.FieldProjectID,
			request.FieldTraceID,
			request.FieldDataStorageID,
			request.FieldStatus,
			request.FieldContentStorageID,
			request.FieldContentStorageKey,
		)
	if requestCleanupUsesForUpdate(client.Driver().Dialect()) {
		selected.Modify(func(selector *entsql.Selector) {
			selector.ForUpdate()
		})
	}

	return selected.Only(ctx)
}

func requestCleanupUsesForUpdate(dialectName string) bool {
	return dialectName == dialect.Postgres || dialectName == dialect.MySQL
}

func lockRetentionOwners(ctx context.Context, client *ent.Client, traceID int) (lockedRetentionOwners, bool, error) {
	var owners lockedRetentionOwners
	if traceID == 0 {
		return owners, true, nil
	}

	traceRow, err := client.Trace.Query().
		Where(trace.IDEQ(traceID)).
		Select(trace.FieldID, trace.FieldUpdatedAt, trace.FieldThreadID, trace.FieldStatus).
		Only(ctx)
	if ent.IsNotFound(err) {
		return owners, true, nil
	}
	if err != nil {
		return owners, false, fmt.Errorf("failed to load trace %d for cleanup lock: %w", traceID, err)
	}
	if traceRow.Status == trace.StatusRetained {
		return owners, false, nil
	}
	owners.traceID = traceRow.ID
	owners.traceUpdatedAt = traceRow.UpdatedAt

	if traceRow.ThreadID != 0 {
		threadRow, err := client.Thread.Query().
			Where(thread.IDEQ(traceRow.ThreadID)).
			Select(thread.FieldID, thread.FieldUpdatedAt, thread.FieldStatus).
			Only(ctx)
		missingThread := ent.IsNotFound(err)
		if err != nil && !missingThread {
			return owners, false, fmt.Errorf("failed to load thread %d for cleanup lock: %w", traceRow.ThreadID, err)
		}
		if !missingThread {
			if threadRow.Status == thread.StatusRetained {
				return owners, false, nil
			}
			owners.threadID = threadRow.ID
			owners.threadUpdatedAt = threadRow.UpdatedAt
			updated, err := client.Thread.Update().
				Where(
					thread.IDEQ(threadRow.ID),
					thread.StatusEQ(threadRow.Status),
					thread.UpdatedAtEQ(threadRow.UpdatedAt),
				).
				SetUpdatedAt(gcClaimTimestamp()).
				Save(ctx)
			if err != nil {
				return owners, false, fmt.Errorf("failed to lock thread %d for cleanup: %w", threadRow.ID, err)
			}
			if updated != 1 {
				return owners, false, nil
			}
		}
	}

	updated, err := client.Trace.Update().
		Where(
			trace.IDEQ(traceRow.ID),
			trace.StatusEQ(traceRow.Status),
			trace.UpdatedAtEQ(traceRow.UpdatedAt),
		).
		SetUpdatedAt(gcClaimTimestamp()).
		Save(ctx)
	if err != nil {
		return owners, false, fmt.Errorf("failed to lock trace %d for cleanup: %w", traceRow.ID, err)
	}
	if updated != 1 {
		return owners, false, nil
	}

	return owners, true, nil
}

func (w *Worker) cleanupExecutionExternalStorage(ctx context.Context, exec *ent.RequestExecution, cache map[int]*ent.DataStorage) error {
	if exec == nil || exec.DataStorageID == 0 {
		return nil
	}
	if w.DataStorageService == nil {
		return fmt.Errorf("execution %d references external storage but data storage service is unavailable", exec.ID)
	}

	ds, err := w.getDataStorageCached(ctx, exec.DataStorageID, cache)
	if err != nil {
		log.Warn(ctx, "Failed to load data storage for execution cleanup",
			log.Cause(err),
			log.Int("execution_id", exec.ID),
		)

		return fmt.Errorf("failed to load data storage for execution %d: %w", exec.ID, err)
	}

	if ds == nil || ds.Primary {
		return nil
	}

	keys := []string{
		biz.GenerateExecutionRequestBodyKey(exec.ProjectID, exec.RequestID, exec.ID),
		biz.GenerateExecutionResponseBodyKey(exec.ProjectID, exec.RequestID, exec.ID),
		biz.GenerateExecutionResponseChunksKey(exec.ProjectID, exec.RequestID, exec.ID),
	}

	// Directory-marker keys only exist as real directories on filesystem-like
	// backends. On object stores (S3/GCS) they were never created, so deleting
	// them only wastes a ListObjectsV2 (Class A); skip them there.
	if hasRealDirectories(ds.Type) {
		keys = append(keys, biz.GenerateExecutionRequestDirKey(exec.ProjectID, exec.RequestID, exec.ID))
	}

	for _, key := range keys {
		if err := w.DataStorageService.DeleteData(ctx, ds, key); err != nil {
			return fmt.Errorf("failed to delete execution %d external key %q: %w", exec.ID, key, err)
		}
	}

	return nil
}

func (w *Worker) cleanupRequestExternalStorage(ctx context.Context, req *ent.Request, cache map[int]*ent.DataStorage) error {
	if req == nil {
		return nil
	}
	if w.DataStorageService == nil {
		hasRecordedContent := req.ContentStorageKey != nil && strings.TrimSpace(*req.ContentStorageKey) != ""
		if req.DataStorageID != 0 || hasRecordedContent {
			return fmt.Errorf("request %d references external storage but data storage service is unavailable", req.ID)
		}
		return nil
	}

	var requestDataStorage *ent.DataStorage
	var directoryKeys []string
	var contentDataStorage *ent.DataStorage
	var contentKey string
	// Validate and resolve all recorded-content metadata before deleting any
	// ordinary request artifact. Invalid ownership metadata must fail closed
	// without leaving a partially cleaned request.
	if req.ContentStorageKey != nil && *req.ContentStorageKey != "" {
		if req.ContentStorageID == nil || *req.ContentStorageID == 0 {
			return fmt.Errorf("request %d has content_storage_key without content_storage_id", req.ID)
		}

		validatedContentKey, err := recordedContentKeyForRequest(req)
		if err != nil {
			return err
		}
		contentKey = validatedContentKey

		ds, err := w.getDataStorageCached(ctx, *req.ContentStorageID, cache)
		if err != nil {
			return fmt.Errorf("failed to load content storage for request %d: %w", req.ID, err)
		}
		if ds == nil || ds.Primary || ds.Type == datastorage.TypeDatabase {
			return fmt.Errorf("request %d recorded content key references a non-file storage", req.ID)
		}
		contentDataStorage = ds
	}

	if req.DataStorageID != 0 {
		ds, err := w.getDataStorageCached(ctx, req.DataStorageID, cache)
		if err != nil {
			return fmt.Errorf("failed to load data storage for request %d: %w", req.ID, err)
		}

		if ds != nil && !ds.Primary {
			requestDataStorage = ds
			keys := []string{
				biz.GenerateRequestBodyKey(req.ProjectID, req.ID),
				biz.GenerateResponseBodyKey(req.ProjectID, req.ID),
				biz.GenerateResponseChunksKey(req.ProjectID, req.ID),
			}

			// See cleanupExecutionExternalStorage: object stores have no real
			// directories, so only attempt directory-marker deletes on FS/WebDAV.
			if hasRealDirectories(ds.Type) {
				directoryKeys = append(directoryKeys,
					biz.GenerateRequestExecutionsDirKey(req.ProjectID, req.ID),
					biz.GenerateRequestDirKey(req.ProjectID, req.ID),
				)
			}

			for _, key := range keys {
				if err := w.DataStorageService.DeleteData(ctx, ds, key); err != nil {
					return fmt.Errorf("failed to delete request %d external key %q: %w", req.ID, key, err)
				}
			}
		}
	}

	if contentDataStorage != nil {
		if err := w.DataStorageService.DeleteData(ctx, contentDataStorage, contentKey); err != nil {
			return fmt.Errorf("failed to delete request %d recorded content key %q: %w", req.ID, contentKey, err)
		}
		if hasRealDirectories(contentDataStorage.Type) {
			if err := w.DataStorageService.DeleteData(ctx, contentDataStorage, path.Dir(contentKey)); err != nil {
				return fmt.Errorf("failed to delete request %d recorded content directory %q: %w", req.ID, path.Dir(contentKey), err)
			}
		}
	}

	// Recorded audio/video lives below the request directory. Remove directory
	// entries only after the recorded content has been deleted, otherwise a
	// filesystem/WebDAV Remove can fail with "directory not empty" and make the
	// record permanently ineligible for cleanup.
	for _, key := range directoryKeys {
		if err := w.DataStorageService.DeleteData(ctx, requestDataStorage, key); err != nil {
			return fmt.Errorf("failed to delete request %d external directory %q: %w", req.ID, key, err)
		}
	}
	if contentDataStorage != nil && hasRealDirectories(contentDataStorage.Type) &&
		(requestDataStorage == nil || contentDataStorage.ID != requestDataStorage.ID) {
		requestDir := biz.GenerateRequestDirKey(req.ProjectID, req.ID)
		if err := w.DataStorageService.DeleteData(ctx, contentDataStorage, requestDir); err != nil {
			return fmt.Errorf("failed to delete request %d recorded content request directory %q: %w", req.ID, requestDir, err)
		}
	}

	return nil
}

func recordedContentKeyForRequest(req *ent.Request) (string, error) {
	raw := strings.TrimSpace(*req.ContentStorageKey)
	if strings.Contains(raw, `\`) {
		return "", fmt.Errorf("request %d has content_storage_key with a noncanonical separator", req.ID)
	}
	key := "/" + strings.TrimPrefix(raw, "/")
	cleaned := path.Clean(key)
	expectedPrefix := fmt.Sprintf("/%d/requests/%d/", req.ProjectID, req.ID)
	if cleaned != key || !strings.HasPrefix(cleaned, expectedPrefix) {
		return "", fmt.Errorf("request %d has content_storage_key outside its owned prefix", req.ID)
	}

	return cleaned, nil
}

// hasRealDirectories reports whether the storage backend materializes
// directories as real entries that must be explicitly removed during cleanup.
// Object stores (S3/GCS) have no real directories — the "*DirKey" paths are
// never created, so attempting to delete them only costs a wasted
// ListObjectsV2 (Class A). Filesystem and WebDAV backends do create real
// directories that should be removed.
func hasRealDirectories(t datastorage.Type) bool {
	return t == datastorage.TypeFs || t == datastorage.TypeWebdav
}

func (w *Worker) getDataStorageCached(ctx context.Context, id int, cache map[int]*ent.DataStorage) (*ent.DataStorage, error) {
	if ds, ok := cache[id]; ok {
		return ds, nil
	}

	ds, err := w.DataStorageService.GetDataStorageByID(ctx, id)
	if err != nil {
		return nil, err
	}

	cache[id] = ds

	return ds, nil
}

func (w *Worker) deleteUsageLogCandidate(ctx context.Context, usageLogID int, cutoffTime time.Time) (deleted bool, err error) {
	tx, err := w.Ent.Tx(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to start usage log cleanup transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	client := tx.Client()
	txCtx := ent.NewContext(ent.NewTxContext(ctx, tx), client)
	row, err := client.UsageLog.Query().
		Where(usagelog.IDEQ(usageLogID)).
		Select(usagelog.FieldID, usagelog.FieldCreatedAt, usagelog.FieldRequestID).
		Only(txCtx)
	if ent.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to reload usage log %d for cleanup: %w", usageLogID, err)
	}
	requestRow, err := client.Request.Query().
		Where(request.IDEQ(row.RequestID)).
		Select(request.FieldID, request.FieldTraceID).
		Only(txCtx)
	if ent.IsNotFound(err) {
		requestRow = &ent.Request{}
		err = nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to load request %d for usage log cleanup: %w", row.RequestID, err)
	}
	owners, eligible, err := lockRetentionOwners(txCtx, client, requestRow.TraceID)
	if err != nil || !eligible {
		return false, err
	}

	count, err := client.UsageLog.Delete().Where(
		usagelog.IDEQ(row.ID),
		usagelog.CreatedAtLT(cutoffTime),
		usagelog.Not(usagelog.HasRequestWith(
			request.HasTraceWith(trace.Or(
				trace.StatusEQ(trace.StatusRetained),
				trace.HasThreadWith(thread.StatusEQ(thread.StatusRetained)),
			)),
		)),
	).Exec(txCtx)
	if err != nil {
		return false, fmt.Errorf("failed to delete usage log %d: %w", row.ID, err)
	}
	if count != 1 {
		return false, nil
	}
	if err := owners.restore(txCtx, client, true, true); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("failed to commit usage log %d cleanup: %w", row.ID, err)
	}
	committed = true

	return true, nil
}

func (w *Worker) deleteTraceCandidate(ctx context.Context, traceID int, cutoffTime time.Time) (deleted bool, err error) {
	tx, err := w.Ent.Tx(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to start trace cleanup transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	client := tx.Client()
	txCtx := ent.NewContext(ent.NewTxContext(ctx, tx), client)
	owners, eligible, err := lockRetentionOwners(txCtx, client, traceID)
	if err != nil || !eligible {
		return false, err
	}
	count, err := client.Trace.Delete().Where(
		trace.IDEQ(traceID),
		trace.CreatedAtLT(cutoffTime),
		trace.StatusNEQ(trace.StatusRetained),
		trace.Not(trace.HasThreadWith(thread.StatusEQ(thread.StatusRetained))),
	).Exec(txCtx)
	if err != nil {
		return false, fmt.Errorf("failed to delete trace %d: %w", traceID, err)
	}
	if count != 1 {
		return false, nil
	}
	if err := owners.restore(txCtx, client, false, true); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("failed to commit trace %d cleanup: %w", traceID, err)
	}
	committed = true

	return true, nil
}

type traceCleanupClaim struct {
	id        int
	updatedAt time.Time
}

func (w *Worker) deleteThreadCandidate(ctx context.Context, threadID int, cutoffTime time.Time) (deleted bool, err error) {
	tx, err := w.Ent.Tx(ctx)
	if err != nil {
		return false, fmt.Errorf("failed to start thread cleanup transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	client := tx.Client()
	txCtx := ent.NewContext(ent.NewTxContext(ctx, tx), client)
	threadRow, err := client.Thread.Query().
		Where(thread.IDEQ(threadID)).
		Select(thread.FieldID, thread.FieldCreatedAt, thread.FieldUpdatedAt, thread.FieldStatus).
		Only(txCtx)
	if ent.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("failed to reload thread %d for cleanup: %w", threadID, err)
	}
	claimed, err := client.Thread.Update().Where(
		thread.IDEQ(threadRow.ID),
		thread.CreatedAtLT(cutoffTime),
		thread.StatusNEQ(thread.StatusRetained),
		thread.UpdatedAtEQ(threadRow.UpdatedAt),
		thread.Not(thread.HasTracesWith(trace.StatusEQ(trace.StatusRetained))),
	).SetUpdatedAt(gcClaimTimestamp()).Save(txCtx)
	if err != nil {
		return false, fmt.Errorf("failed to lock thread %d for cleanup: %w", threadID, err)
	}
	if claimed != 1 {
		return false, nil
	}

	traceRows, err := client.Trace.Query().
		Where(trace.ThreadIDEQ(threadRow.ID)).
		Select(trace.FieldID, trace.FieldUpdatedAt, trace.FieldStatus).
		All(txCtx)
	if err != nil {
		return false, fmt.Errorf("failed to load traces for thread %d cleanup: %w", threadID, err)
	}
	traceClaims := make([]traceCleanupClaim, 0, len(traceRows))
	for _, traceRow := range traceRows {
		if traceRow.Status == trace.StatusRetained {
			return false, nil
		}
		updated, err := client.Trace.Update().Where(
			trace.IDEQ(traceRow.ID),
			trace.StatusEQ(traceRow.Status),
			trace.UpdatedAtEQ(traceRow.UpdatedAt),
		).SetUpdatedAt(gcClaimTimestamp()).Save(txCtx)
		if err != nil {
			return false, fmt.Errorf("failed to lock trace %d for thread cleanup: %w", traceRow.ID, err)
		}
		if updated != 1 {
			return false, nil
		}
		traceClaims = append(traceClaims, traceCleanupClaim{id: traceRow.ID, updatedAt: traceRow.UpdatedAt})
	}

	count, err := client.Thread.Delete().Where(
		thread.IDEQ(threadRow.ID),
		thread.CreatedAtLT(cutoffTime),
		thread.StatusNEQ(thread.StatusRetained),
		thread.Not(thread.HasTracesWith(trace.StatusEQ(trace.StatusRetained))),
	).Exec(txCtx)
	if err != nil {
		return false, fmt.Errorf("failed to delete thread %d: %w", threadID, err)
	}
	if count != 1 {
		return false, nil
	}
	for _, claim := range traceClaims {
		if _, err := client.Trace.UpdateOneID(claim.id).SetUpdatedAt(claim.updatedAt).Save(txCtx); err != nil {
			return false, fmt.Errorf("failed to restore trace %d thread-cleanup claim: %w", claim.id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("failed to commit thread %d cleanup: %w", threadID, err)
	}
	committed = true

	return true, nil
}

// cleanupUsageLogs deletes usage logs older than the specified number of days.
func (w *Worker) cleanupUsageLogs(ctx context.Context, cleanupDays int, manual bool) error {
	if cleanupDays <= 0 {
		return nil
	}

	cutoffTime := time.Now().AddDate(0, 0, -cleanupDays)
	batchSize := w.getBatchSize()

	result := 0
	for {
		ids, err := w.Ent.UsageLog.Query().
			Where(
				usagelog.CreatedAtLT(cutoffTime),
				usagelog.Not(usagelog.HasRequestWith(
					request.HasTraceWith(trace.Or(
						trace.StatusEQ(trace.StatusRetained),
						trace.HasThreadWith(thread.StatusEQ(thread.StatusRetained)),
					)),
				)),
			).
			Order(ent.Asc(usagelog.FieldID)).
			Limit(batchSize).
			IDs(ctx)
		if err != nil {
			return fmt.Errorf("failed to query old usage logs: %w", err)
		}
		if len(ids) == 0 {
			break
		}
		batchDeleted := 0
		for _, id := range ids {
			if w.beforeCandidateDelete != nil {
				w.beforeCandidateDelete("usage_log", id)
			}
			deleted, err := w.deleteUsageLogCandidate(ctx, id, cutoffTime)
			if err != nil {
				return fmt.Errorf("failed to delete old usage log %d: %w", id, err)
			}
			if deleted {
				batchDeleted++
			}
		}
		result += batchDeleted
		if batchDeleted == 0 {
			break
		}
	}

	log.Debug(ctx, "Cleaned up usage logs",
		log.Int("deleted_count", result),
		log.Time("cutoff_time", cutoffTime))

	return nil
}

// cleanupThreads deletes threads older than the specified number of days.
func (w *Worker) cleanupThreads(ctx context.Context, cleanupDays int, manual bool) error {
	if cleanupDays <= 0 {
		log.Debug(ctx, "No cleanup needed for threads")
		return nil
	}

	cutoffTime := time.Now().AddDate(0, 0, -cleanupDays)
	batchSize := w.getBatchSize()

	result := 0
	for {
		ids, err := w.Ent.Thread.Query().
			Where(
				thread.CreatedAtLT(cutoffTime),
				thread.StatusNEQ(thread.StatusRetained),
				thread.Not(thread.HasTracesWith(trace.StatusEQ(trace.StatusRetained))),
			).
			Order(ent.Asc(thread.FieldID)).
			Limit(batchSize).
			IDs(ctx)
		if err != nil {
			return fmt.Errorf("failed to query old threads: %w", err)
		}
		if len(ids) == 0 {
			break
		}
		batchDeleted := 0
		for _, id := range ids {
			if w.beforeCandidateDelete != nil {
				w.beforeCandidateDelete("thread", id)
			}
			deleted, err := w.deleteThreadCandidate(ctx, id, cutoffTime)
			if err != nil {
				return fmt.Errorf("failed to delete old thread %d: %w", id, err)
			}
			if deleted {
				batchDeleted++
			}
		}
		result += batchDeleted
		if batchDeleted == 0 {
			break
		}
	}

	log.Debug(ctx, "Cleaned up threads",
		log.Int("deleted_count", result),
		log.Time("cutoff_time", cutoffTime))

	return nil
}

// cleanupTraces deletes traces older than the specified number of days.
func (w *Worker) cleanupTraces(ctx context.Context, cleanupDays int, manual bool) error {
	if cleanupDays <= 0 {
		log.Debug(ctx, "No cleanup needed for traces")
		return nil
	}

	cutoffTime := time.Now().AddDate(0, 0, -cleanupDays)
	batchSize := w.getBatchSize()

	result := 0
	for {
		ids, err := w.Ent.Trace.Query().
			Where(
				trace.CreatedAtLT(cutoffTime),
				trace.StatusNEQ(trace.StatusRetained),
				trace.Not(trace.HasThreadWith(thread.StatusEQ(thread.StatusRetained))),
			).
			Order(ent.Asc(trace.FieldID)).
			Limit(batchSize).
			IDs(ctx)
		if err != nil {
			return fmt.Errorf("failed to query old traces: %w", err)
		}
		if len(ids) == 0 {
			break
		}
		batchDeleted := 0
		for _, id := range ids {
			if w.beforeCandidateDelete != nil {
				w.beforeCandidateDelete("trace", id)
			}
			deleted, err := w.deleteTraceCandidate(ctx, id, cutoffTime)
			if err != nil {
				return fmt.Errorf("failed to delete old trace %d: %w", id, err)
			}
			if deleted {
				batchDeleted++
			}
		}
		result += batchDeleted
		if batchDeleted == 0 {
			break
		}
	}

	log.Debug(ctx, "Cleaned up traces",
		log.Int("deleted_count", result),
		log.Time("cutoff_time", cutoffTime))

	return nil
}

// cleanupChannelProbes deletes channel probes older than the specified number of days.
func (w *Worker) cleanupChannelProbes(ctx context.Context, cleanupDays int, manual bool) error {
	if cleanupDays <= 0 {
		log.Debug(ctx, "No cleanup needed for channel probes")
		return nil
	}

	cutoffTime := time.Now().AddDate(0, 0, -cleanupDays)
	batchSize := w.getBatchSize()

	result, err := w.deleteInBatches(ctx, func() (int, error) {
		ids, err := w.Ent.ChannelProbe.Query().
			Where(channelprobe.TimestampLT(cutoffTime.Unix())).
			Order(ent.Asc(channelprobe.FieldID)).
			Limit(batchSize).
			IDs(ctx)
		if err != nil {
			return 0, fmt.Errorf("failed to query old channel probes: %w", err)
		}
		if len(ids) == 0 {
			return 0, nil
		}

		return w.Ent.ChannelProbe.Delete().Where(channelprobe.IDIn(ids...)).Exec(ctx)
	})
	if err != nil {
		return fmt.Errorf("failed to delete old channel probes: %w", err)
	}

	log.Debug(ctx, "Cleaned up channel probes",
		log.Int("deleted_count", result),
		log.Time("cutoff_time", cutoffTime))

	return nil
}

// runVacuum executes VACUUM command on SQLite/PostgreSQL database.
func (w *Worker) runVacuum(ctx context.Context) error {
	if !w.Config.VacuumEnabled {
		log.Debug(ctx, "VACUUM is disabled, skipping")
		return nil
	}

	dbDriver := w.Ent.Driver()
	if dbDriver == nil {
		return fmt.Errorf("failed to get database driver")
	}
	if primary, ok := dbDriver.(interface{ PrimaryDriver() dialect.Driver }); ok {
		dbDriver = primary.PrimaryDriver()
	}

	sqlDriver, ok := dbDriver.(*entsql.Driver)
	if !ok {
		log.Debug(ctx, "Database driver is not *entsql.Driver, skipping VACUUM")
		return nil
	}

	if sqlDriver.Dialect() != dialect.SQLite && sqlDriver.Dialect() != dialect.Postgres {
		log.Debug(ctx, "Database does not support VACUUM, skipping",
			log.String("dialect", sqlDriver.Dialect()))

		return nil
	}

	log.Info(ctx, "Starting database VACUUM operation",
		log.String("dialect", sqlDriver.Dialect()),
		log.Bool("vacuum_full", w.Config.VacuumFull))

	startTime := time.Now()

	var vacuumSQL string
	if sqlDriver.Dialect() == dialect.Postgres && w.Config.VacuumFull {
		vacuumSQL = "VACUUM FULL"
	} else {
		vacuumSQL = "VACUUM"
	}

	if _, err := sqlDriver.ExecContext(ctx, vacuumSQL); err != nil {
		return fmt.Errorf("failed to execute %s: %w", vacuumSQL, err)
	}

	duration := time.Since(startTime)
	log.Info(ctx, "Database VACUUM completed successfully",
		log.Duration("duration", duration),
		log.String("command", vacuumSQL))

	return nil
}

// RunVacuumNow manually triggers the VACUUM operation.
func (w *Worker) RunVacuumNow(ctx context.Context) error {
	return w.runVacuum(ctx)
}

// RunCleanupNow manually triggers the cleanup process with the specified days.
func (w *Worker) RunCleanupNow(ctx context.Context, input TriggerGcCleanupInput) error {
	manualDays := make(map[string]int)
	if input.RequestsCleanupDays > 0 {
		manualDays["requests"] = input.RequestsCleanupDays
	}
	if input.UsageLogsCleanupDays > 0 {
		manualDays["usage_logs"] = input.UsageLogsCleanupDays
	}
	w.runCleanup(ctx, true, manualDays)
	return nil
}

// PreviewCleanup estimates how many records would be deleted without actually deleting them.
func (w *Worker) PreviewCleanup(ctx context.Context, input TriggerGcCleanupInput) ([]GcCleanupPreviewItem, error) {
	ctx = ent.NewContext(ctx, w.Ent)
	ctx = schematype.SkipSoftDelete(ctx)
	ctx = serverdb.WithPrimary(ctx)

	var items []GcCleanupPreviewItem

	if input.RequestsCleanupDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -input.RequestsCleanupDays)
		count, err := w.Ent.Request.Query().Where(
			request.CreatedAtLT(cutoff),
			request.StatusIn(request.StatusCompleted, request.StatusFailed, request.StatusCanceled),
			request.Not(request.HasExecutionsWith(requestexecution.Or(
				requestexecution.CreatedAtGTE(cutoff),
				requestexecution.StatusIn(requestexecution.StatusPending, requestexecution.StatusProcessing),
			))),
			request.Not(request.HasTraceWith(trace.Or(
				trace.StatusEQ(trace.StatusRetained),
				trace.HasThreadWith(thread.StatusEQ(thread.StatusRetained)),
			))),
		).Count(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to count requests for preview: %w", err)
		}
		items = append(items, GcCleanupPreviewItem{
			ResourceType:   "requests",
			EstimatedCount: count,
			CutoffTime:     cutoff,
			RetentionDays:  input.RequestsCleanupDays,
		})
	}

	if input.UsageLogsCleanupDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -input.UsageLogsCleanupDays)
		count, err := w.Ent.UsageLog.Query().Where(
			usagelog.CreatedAtLT(cutoff),
			usagelog.Not(usagelog.HasRequestWith(
				request.HasTraceWith(trace.Or(
					trace.StatusEQ(trace.StatusRetained),
					trace.HasThreadWith(thread.StatusEQ(thread.StatusRetained)),
				)),
			)),
		).Count(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to count usage logs for preview: %w", err)
		}
		items = append(items, GcCleanupPreviewItem{
			ResourceType:   "usage_logs",
			EstimatedCount: count,
			CutoffTime:     cutoff,
			RetentionDays:  input.UsageLogsCleanupDays,
		})
	}

	return items, nil
}
