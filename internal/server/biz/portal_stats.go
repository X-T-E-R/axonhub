package biz

import (
	"context"
	"fmt"
	"sort"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/schema/schematype"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/samber/lo"
)

func (s *PortalService) Dashboard(ctx context.Context, projectID *int, period string, keyID *int) (*objects.PortalDashboard, error) {
	if _, err := portalCurrentUser(ctx); err != nil {
		return nil, err
	}
	ctx = authz.WithSystemBypass(schematype.SkipSoftDelete(ctx), "portal-retained-personal-statistics")
	keys, err := s.ownKeys(ctx, projectID).All(ctx)
	if err != nil {
		return nil, err
	}
	ids := lo.Map(keys, func(k *ent.APIKey, _ int) int { return k.ID })
	if keyID != nil {
		if !lo.Contains(ids, *keyID) {
			return nil, fmt.Errorf("key unavailable")
		}
		ids = []int{*keyID}
	}
	dto := &objects.PortalDashboard{Models: []*objects.PortalModelUsage{}, Days: []*objects.PortalDayUsage{}, RecentRequests: []*objects.PortalRequest{}}
	loc := s.system.TimeLocation(ctx)
	now := time.Now().In(loc)
	var start time.Time
	switch period {
	case "today":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	case "week":
		start = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(int(now.Weekday())+6)%7)
	case "month":
		start = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
	case "all":
	default:
		return nil, fmt.Errorf("invalid statistics period")
	}
	if len(ids) == 0 {
		return dto, nil
	}
	client := s.entFromContext(ctx)
	rq := client.Request.Query().Where(request.APIKeyIDIn(ids...))
	uq := client.UsageLog.Query().Where(usagelog.APIKeyIDIn(ids...))
	if !start.IsZero() {
		rq.Where(request.CreatedAtGTE(start))
		uq.Where(usagelog.CreatedAtGTE(start))
	}
	dto.Requests, err = rq.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}
	dto.SuccessfulRequests, err = rq.Clone().Where(request.StatusEQ(request.StatusCompleted)).Count(ctx)
	if err != nil {
		return nil, err
	}
	// UsageLog owns its APIKey ID: this does not join Request and survives Request GC.
	var rows []struct {
		ModelID      string   `json:"model_id"`
		UsageCount   int      `json:"usage_count"`
		InputTokens  int64    `json:"input_tokens"`
		OutputTokens int64    `json:"output_tokens"`
		CachedTokens int64    `json:"cached_tokens"`
		TotalTokens  int64    `json:"total_tokens"`
		PricedCount  int      `json:"priced_count"`
		Cost         *float64 `json:"cost"`
	}
	err = uq.GroupBy(usagelog.FieldModelID).Aggregate(
		ent.As(ent.Count(), "usage_count"), ent.As(ent.Sum(usagelog.FieldPromptTokens), "input_tokens"), ent.As(ent.Sum(usagelog.FieldCompletionTokens), "output_tokens"), ent.As(ent.Sum(usagelog.FieldPromptCachedTokens), "cached_tokens"), ent.As(ent.Sum(usagelog.FieldTotalTokens), "total_tokens"), ent.As(ent.Sum(usagelog.FieldTotalCost), "cost"),
		func(sel *sql.Selector) string {
			return sql.As(sql.Count(sel.C(usagelog.FieldTotalCost)), "priced_count")
		},
	).Scan(ctx, &rows)
	if err != nil {
		return nil, err
	}
	var totalCost float64
	for _, r := range rows {
		dto.Models = append(dto.Models, &objects.PortalModelUsage{ModelID: r.ModelID, UsageCount: r.UsageCount, InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, CachedTokens: r.CachedTokens, TotalTokens: r.TotalTokens, PricedCount: r.PricedCount, Cost: r.Cost})
		dto.UsageCount += r.UsageCount
		dto.InputTokens += r.InputTokens
		dto.OutputTokens += r.OutputTokens
		dto.CachedTokens += r.CachedTokens
		dto.TotalTokens += r.TotalTokens
		dto.PricedCount += r.PricedCount
		if r.Cost != nil {
			totalCost += *r.Cost
		}
	}
	if dto.PricedCount > 0 {
		dto.Cost = &totalCost
	}
	sort.Slice(dto.Models, func(i, j int) bool { return dto.Models[i].TotalTokens > dto.Models[j].TotalTokens })
	recent, err := rq.Clone().Order(ent.Desc(request.FieldCreatedAt), ent.Desc(request.FieldID)).Limit(20).WithUsageLogs().All(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range recent {
		row := &objects.PortalRequest{ID: r.ID, CreatedAt: r.CreatedAt, Status: string(r.Status), ModelID: r.ModelID}
		if len(r.Edges.UsageLogs) > 0 {
			row.UsageReported = true
			var tokens int64
			var cost float64
			for _, usage := range r.Edges.UsageLogs {
				tokens += usage.TotalTokens
				if usage.TotalCost != nil {
					cost += *usage.TotalCost
					row.Cost = &cost
				}
			}
			row.TotalTokens = lo.ToPtr(tokens)
		}
		dto.RecentRequests = append(dto.RecentRequests, row)
	}
	// Only the recent 30-day series is read row-wise; identity remains fixed even
	// when a former group member asks for retained history.
	dailyStart := now.AddDate(0, 0, -30)
	if start.After(dailyStart) {
		dailyStart = start
	}
	dailyRows, err := client.UsageLog.Query().Where(usagelog.APIKeyIDIn(ids...), usagelog.CreatedAtGTE(dailyStart)).Select(usagelog.FieldCreatedAt, usagelog.FieldTotalTokens).All(ctx)
	if err != nil {
		return nil, err
	}
	days := map[string]int64{}
	for _, r := range dailyRows {
		days[r.CreatedAt.In(loc).Format("2006-01-02")] += r.TotalTokens
	}
	for day, tokens := range days {
		dto.Days = append(dto.Days, &objects.PortalDayUsage{Day: day, TotalTokens: tokens})
	}
	sort.Slice(dto.Days, func(i, j int) bool { return dto.Days[i].Day < dto.Days[j].Day })
	return dto, nil
}
