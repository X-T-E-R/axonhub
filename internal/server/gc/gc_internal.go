package gc

import (
	"context"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/schema/schematype"
	"github.com/looplj/axonhub/internal/log"
	serverdb "github.com/looplj/axonhub/internal/server/db"
)

func (w *Worker) runAutomaticCleanup(ctx context.Context) {
	ctx = authz.WithSystemBypass(ctx, "gc-cleanup")
	w.runCleanup(ctx, false, nil)
}

func (w *Worker) runAutomaticCapacityCleanup(ctx context.Context) {
	ctx = authz.WithSystemBypass(ctx, "managed-observability-capacity-gc")
	ctx = ent.NewContext(ctx, w.Ent)
	ctx = schematype.SkipSoftDelete(ctx)
	ctx = serverdb.WithPrimary(ctx)
	policy, err := w.SystemService.StoragePolicy(ctx)
	if err != nil {
		w.recordManagedObservabilityFailure(ctx, "capacity_policy", "load_failed")
		log.Error(ctx, "Managed observability capacity policy unavailable; retrying later",
			log.String("signal", "managed_observability_capacity_policy_error"), log.Cause(err))
		return
	}
	var cleanupErr error
	acquired, ownerErr := w.withGCOwnership(ctx, func() {
		cleanupErr = w.cleanupManagedCapacity(ctx, policy)
	})
	if ownerErr != nil {
		w.recordManagedObservabilityFailure(ctx, "gc_owner_lock", "failed")
		log.Error(ctx, "Managed observability capacity ownership failed; retrying later",
			log.String("signal", "managed_observability_gc_owner_error"), log.Cause(ownerErr))
		return
	}
	if !acquired {
		return
	}
	if cleanupErr != nil {
		w.recordManagedObservabilityFailure(ctx, "capacity_cleanup", "failed")
		log.Error(ctx, "Managed observability capacity cleanup failed; traffic remains available",
			log.String("signal", "managed_observability_cleanup_failed"), log.Cause(cleanupErr))
	}
}
