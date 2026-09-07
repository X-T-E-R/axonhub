package biz

import (
	"context"
	"database/sql"
	"sync/atomic"
	"time"

	"entgo.io/ent/dialect"
	"github.com/shopspring/decimal"

	"github.com/looplj/axonhub/internal/ent/usagelog"
)

type (
	observationPendingUsageKey struct{}
	observationPendingUsage    struct {
		apiKeyID  int
		createdAt time.Time
		tokens    int64
		cost      *float64
		requestID atomic.Int64
	}
)

// A pending record is captured before opening the database snapshot and stays
// alive even if the worker removes it meanwhile. The worker publishes requestID
// before INSERT. Reading that ID after the aggregate and checking visibility in
// the same snapshot prevents both a commit/removal gap and double counting.
func (s *QuotaService) accountedUsage(ctx context.Context, apiKeyID int, window QuotaWindow) (QuotaUsage, error) {
	var pending []*observationPendingUsage
	if s.observation != nil {
		s.observation.mu.Lock()
		for item := range s.observation.pendingRecords {
			if item.apiKeyID == apiKeyID {
				pending = append(pending, item)
			}
		}
		s.observation.mu.Unlock()
	}
	reader := s
	if len(pending) > 0 {
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
	for _, item := range pending {
		if window.Start != nil && item.createdAt.Before(*window.Start) {
			continue
		}
		if window.End != nil && (item.createdAt.After(*window.End) || !window.EndInclusive && item.createdAt.Equal(*window.End)) {
			continue
		}
		id := int(item.requestID.Load())
		if id != 0 {
			exists, err := reader.ent.UsageLog.Query().Where(usagelog.RequestIDEQ(id), usagelog.APIKeyIDEQ(apiKeyID)).Exist(ctx)
			if err != nil {
				return QuotaUsage{}, err
			}
			if exists {
				continue
			}
		}
		result.RequestCount++
		result.TotalTokens += item.tokens
		if item.cost != nil {
			result.TotalCost = result.TotalCost.Add(decimal.NewFromFloat(*item.cost))
		}
	}
	return result, nil
}
