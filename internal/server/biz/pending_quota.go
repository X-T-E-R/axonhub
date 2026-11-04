package biz

import (
	"context"
	"database/sql"
	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/pendingusage"
	"github.com/shopspring/decimal"
)

func (s *QuotaService) durableAccountedUsage(ctx context.Context, keyID int, window QuotaWindow) (QuotaUsage, error) {
	opts := &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead}
	if s.ent.Driver().Dialect() == dialect.SQLite {
		opts.Isolation = sql.LevelSerializable
	}
	tx, err := s.ent.BeginTx(ctx, opts)
	if err != nil {
		return QuotaUsage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	reader := &QuotaService{ent: tx.Client(), system: s.system}
	count, err := reader.requestCount(ctx, keyID, window)
	if err != nil {
		return QuotaUsage{}, err
	}
	agg, err := reader.usageAgg(ctx, keyID, window, true, true)
	if err != nil {
		return QuotaUsage{}, err
	}
	q := tx.Client().PendingUsage.Query().Where(pendingusage.APIKeyIDEQ(keyID))
	if window.Start != nil {
		q.Where(pendingusage.CapturedAtGTE(*window.Start))
	}
	if window.End != nil {
		if window.EndInclusive {
			q.Where(pendingusage.CapturedAtLTE(*window.End))
		} else {
			q.Where(pendingusage.CapturedAtLT(*window.End))
		}
	}
	var pending []struct {
		Count  int64           `json:"count"`
		Tokens int64           `json:"tokens"`
		Cost   sql.NullFloat64 `json:"cost"`
	}
	err = q.Aggregate(ent.As(ent.Count(), "count"), func(s *entsql.Selector) string {
		return entsql.As("COALESCE(SUM("+s.C(pendingusage.FieldTotalTokens)+"),0)", "tokens")
	}, ent.As(ent.Sum(pendingusage.FieldTotalCost), "cost")).Scan(ctx, &pending)
	if err != nil {
		return QuotaUsage{}, err
	}
	result := QuotaUsage{RequestCount: count, TotalTokens: agg.TotalTokens, TotalCost: agg.TotalCost}
	if len(pending) > 0 {
		result.RequestCount += pending[0].Count
		result.TotalTokens += pending[0].Tokens
		if pending[0].Cost.Valid {
			result.TotalCost = result.TotalCost.Add(decimal.NewFromFloat(pending[0].Cost.Float64))
		}
	}
	return result, nil
}
