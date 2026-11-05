package biz

import (
	"context"
	"fmt"

	"entgo.io/ent/dialect/sql"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/pendingusage"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/llm"
	"github.com/samber/lo"
	"time"
)

func (w *ForwardingObservationWriter) publishPendingUsage(ctx context.Context, id string, req *ent.Request, exec *ent.RequestExecution, usage llm.Usage, stamp time.Time, cost observationUsageCost) error {
	ctx = authz.WithSystemBypass(ent.NewContext(ctx, w.primary), "cluster-accounting-capture")
	nonce := ""
	if req.EvidenceDisposition != nil {
		nonce = req.EvidenceDisposition.ObservationID
	}
	created := req.CreatedAt
	if created.IsZero() {
		created = stamp
	}
	return w.primary.PendingUsage.Create().SetID(id).SetNodeID(w.node.NodeID).
		SetAPIKeyID(req.APIKeyID).SetProjectID(req.ProjectID).SetChannelID(exec.ChannelID).
		SetModelID(exec.ModelID).SetSource(string(req.Source)).SetFormat(req.Format).
		SetCapturedAt(stamp).SetRequestCreatedAt(created).SetRequestID(max(0, req.ID)).SetRequestObservationID(nonce).
		SetUsage(usage).SetTotalTokens(usage.TotalTokens).SetNillableTotalCost(cost.total).
		SetCostItems(cost.items).SetPriceReferenceID(cost.referenceID).
		OnConflict(sql.ConflictColumns(pendingusage.FieldID)).Ignore().Exec(ctx)
}

// A write of the existing ownership value locks the row. Competing recovery or
// ambiguous-commit retries either own this fact or find it already consumed.
func (w *ForwardingObservationWriter) handoffPendingUsage(ctx context.Context, id string, requestID int) error {
	ctx = authz.WithSystemBypass(ent.NewContext(ctx, w.primary), "cluster-accounting-handoff")
	// Capacity bookkeeping may use the last verified policy; an optional
	// policy read is not part of the durable pending-to-usage transaction.
	if policy := w.policy.Load(); policy != nil {
		ctx = context.WithValue(ctx, observationPolicyContextKey{}, policy)
	}
	svc := &AbstractService{db: w.primary}
	return svc.RunInTransaction(ctx, func(txCtx context.Context) error {
		client := ent.FromContext(txCtx)
		n, err := client.PendingUsage.Update().Where(pendingusage.IDEQ(id)).SetNodeID(w.node.NodeID).Save(txCtx)
		if err != nil || n == 0 {
			return err
		}
		p, err := client.PendingUsage.Get(txCtx, id)
		if err != nil {
			return err
		}
		if requestID <= 0 {
			requestID, err = w.recoverPendingRequest(txCtx, p)
			if err != nil {
				return err
			}
		}
		exists, err := client.UsageLog.Query().Where(usagelog.RequestIDEQ(requestID), usagelog.APIKeyIDEQ(p.APIKeyID)).Exist(txCtx)
		if err != nil {
			return err
		}
		if !exists {
			txCtx = context.WithValue(txCtx, observationUsageTimeKey{}, p.CapturedAt)
			txCtx = context.WithValue(txCtx, observationUsageCostKey{}, observationUsageCost{items: p.CostItems, total: p.TotalCost, referenceID: p.PriceReferenceID})
			_, err = w.usageService.CreateUsageLog(txCtx, CreateUsageLogParams{RequestID: requestID, ProjectID: p.ProjectID, ChannelID: p.ChannelID, ActualModelID: p.ModelID, Usage: &p.Usage, Source: usagelog.Source(p.Source), Format: p.Format, APIKeyID: lo.ToPtr(p.APIKeyID)})
			if err != nil {
				return err
			}
		}
		return client.PendingUsage.DeleteOneID(id).Exec(txCtx)
	})
}

func (w *ForwardingObservationWriter) recoverPendingRequest(ctx context.Context, p *ent.PendingUsage) (int, error) {
	client := ent.FromContext(ctx)
	if p.RequestID > 0 {
		exists, err := client.Request.Query().Where(request.IDEQ(p.RequestID)).Exist(ctx)
		if err != nil {
			return 0, err
		}
		if exists {
			return p.RequestID, nil
		}
	}
	if p.RequestObservationID != "" {
		reader := &RequestService{AbstractService: &AbstractService{db: client}}
		req, err := reader.findObservedRequest(ctx, p.ProjectID, p.RequestCreatedAt, p.RequestObservationID)
		if err == nil {
			return req.ID, nil
		}
		if !ent.IsNotFound(err) {
			return 0, err
		}
	}
	// The asynchronous parent was never durable. Preserve accounting and its real
	// ownership in a compact skeleton. Usage alone is not proof of a successful
	// business terminal (it can accompany cancellation/failure); leave that state
	// unknown rather than inventing successful response evidence.
	disposition := forwardingUnavailableEvidenceDisposition(p.RequestCreatedAt)
	disposition.ObservationID = p.RequestObservationID
	req, err := client.Request.Create().SetAPIKeyID(p.APIKeyID).SetProjectID(p.ProjectID).
		SetChannelID(p.ChannelID).SetModelID(p.ModelID).SetFormat(p.Format).SetSource(request.Source(p.Source)).
		SetCreatedAt(p.RequestCreatedAt).SetUpdatedAt(p.CapturedAt).SetStatus(request.StatusProcessing).
		SetRequestBody([]byte("{}")).SetRequestHeaders([]byte("{}")).SetEvidenceDisposition(disposition).Save(ctx)
	if err != nil {
		return 0, fmt.Errorf("recover pending usage request: %w", err)
	}
	return req.ID, nil
}

func (w *ForwardingObservationWriter) recoverPendingUsage(ctx context.Context) error {
	// Page by immutable ID, so neither a backlog nor a large capture can require an
	// unbounded in-memory recovery slice. A failed row blocks admission for its node.
	for {
		rows, err := w.primary.PendingUsage.Query().Where(pendingusage.NodeIDEQ(w.node.NodeID)).Limit(64).All(ctx)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		for _, row := range rows {
			if err := w.handoffPendingUsage(ctx, row.ID, 0); err != nil {
				return err
			}
		}
	}
}
