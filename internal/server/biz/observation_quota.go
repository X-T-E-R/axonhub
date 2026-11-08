package biz

import (
	"context"
	"database/sql"
	"slices"
	"sync/atomic"
	"time"

	"entgo.io/ent/dialect"
	"github.com/shopspring/decimal"

	"github.com/looplj/axonhub/internal/ent/pendingusage"
	"github.com/looplj/axonhub/internal/ent/usagelog"
)

type (
	observationPendingUsageKey struct{}
	observationPendingUsage    struct {
		usageID   string
		apiKeyID  int
		createdAt time.Time
		tokens    int64
		cost      *float64
		requestID atomic.Int64
	}
)

func (s *QuotaService) accountedUsage(ctx context.Context, apiKeyID int, window QuotaWindow) (QuotaUsage, error) {
	var local []*observationPendingUsage
	clustered := s.observation != nil && s.observation.node.Enabled
	if s.observation != nil {
		s.observation.mu.Lock()
		for item := range s.observation.pendingRecords {
			if item.apiKeyID != apiKeyID || window.Start != nil && item.createdAt.Before(*window.Start) ||
				window.End != nil && (item.createdAt.After(*window.End) || !window.EndInclusive && item.createdAt.Equal(*window.End)) {
				continue
			}
			local = append(local, item)
		}
		s.observation.mu.Unlock()
	}
	reader := s
	if clustered || len(local) > 0 {
		opts := &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead}
		if s.ent.Driver().Dialect() == dialect.SQLite {
			opts.Isolation = sql.LevelSerializable
		}
		tx, err := s.ent.BeginTx(ctx, opts)
		if err != nil {
			return QuotaUsage{}, err
		}
		defer func() { _ = tx.Rollback() }()
		reader = &QuotaService{ent: tx.Client(), system: s.system}
	}
	count, err := reader.requestCount(ctx, apiKeyID, window)
	if err != nil {
		return QuotaUsage{}, err
	}
	agg, err := reader.usageAgg(ctx, apiKeyID, window, true, true)
	if err != nil {
		return QuotaUsage{}, err
	}
	result := QuotaUsage{RequestCount: count, TotalTokens: agg.TotalTokens, TotalCost: agg.TotalCost}
	if clustered {
		pending, err := reader.durablePendingUsage(ctx, apiKeyID, window)
		if err != nil {
			return QuotaUsage{}, err
		}
		result.RequestCount += pending.RequestCount
		result.TotalTokens += pending.TotalTokens
		result.TotalCost = result.TotalCost.Add(pending.TotalCost)
	}
	// Keep local facts captured before opening the SQL snapshot, even if their
	// writer finishes meanwhile. Check both durable forms in that same snapshot
	// so a fact moving from memory to pending to final is counted exactly once.
	var published []string
	var written []int
	requestIDs := make([]int, len(local))
	var usageIDs []string
	for i, fact := range local {
		if clustered && fact.usageID != "" {
			usageIDs = append(usageIDs, fact.usageID)
		}
		if id := int(fact.requestID.Load()); id != 0 {
			requestIDs[i] = id
			written = append(written, id)
		}
	}
	if len(usageIDs) > 0 {
		published, err = reader.ent.PendingUsage.Query().Where(pendingusage.IDIn(usageIDs...)).IDs(ctx)
		if err != nil {
			return QuotaUsage{}, err
		}
	}
	var persisted []int
	if len(written) > 0 {
		if err := reader.ent.UsageLog.Query().Where(usagelog.APIKeyIDEQ(apiKeyID), usagelog.RequestIDIn(written...)).Select(usagelog.FieldRequestID).Scan(ctx, &persisted); err != nil {
			return QuotaUsage{}, err
		}
	}
	for i, fact := range local {
		if slices.Contains(published, fact.usageID) || slices.Contains(persisted, requestIDs[i]) {
			continue
		}
		result.RequestCount++
		result.TotalTokens += fact.tokens
		if fact.cost != nil {
			result.TotalCost = result.TotalCost.Add(decimal.NewFromFloat(*fact.cost))
		}
	}
	return result, nil
}
