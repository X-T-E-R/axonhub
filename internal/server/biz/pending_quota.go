package biz

import (
	"context"
	"database/sql"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/shopspring/decimal"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/pendingusage"
)

func (s *QuotaService) durablePendingUsage(ctx context.Context, keyID int, window QuotaWindow) (QuotaUsage, error) {
	q := s.ent.PendingUsage.Query().Where(pendingusage.APIKeyIDEQ(keyID))
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
	err := q.Aggregate(ent.As(ent.Count(), "count"), func(s *entsql.Selector) string {
		return entsql.As("COALESCE(SUM("+s.C(pendingusage.FieldTotalTokens)+"),0)", "tokens")
	}, ent.As(ent.Sum(pendingusage.FieldTotalCost), "cost")).Scan(ctx, &pending)
	if err != nil || len(pending) == 0 {
		return QuotaUsage{}, err
	}
	result := QuotaUsage{RequestCount: pending[0].Count, TotalTokens: pending[0].Tokens}
	if pending[0].Cost.Valid {
		result.TotalCost = decimal.NewFromFloat(pending[0].Cost.Float64)
	}
	return result, nil
}
