package biz

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/model"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/samber/lo"
)

func (s *PortalService) Models(ctx context.Context, projectID int, keyID *int, search *string, modelType *string) ([]*objects.PortalModel, error) {
	g, err := s.group(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("group unavailable")
	}
	if ValidatePortalGroup(g) != nil {
		return []*objects.PortalModel{}, nil
	}
	key := &ent.APIKey{ProjectID: projectID, PortalManaged: true}
	key.Edges.Project = g
	if keyID != nil {
		key, err = s.ownKey(ctx, *keyID, false)
		if err != nil || key.ProjectID != projectID {
			return nil, fmt.Errorf("key unavailable")
		}
		if key.Status != apikey.StatusEnabled {
			return []*objects.PortalModel{}, nil
		}
		key.Edges.Project = g
	}
	ctx = authz.WithSystemBypass(contexts.WithAPIKey(ctx, key), "portal-available-models")
	available, err := s.models.ListEnabledModels(ctx)
	if err != nil {
		return nil, err
	}
	ids := lo.Map(available, func(m ModelFacade, _ int) string { return m.ID })
	if len(ids) == 0 {
		return []*objects.PortalModel{}, nil
	}
	if search != nil && len(*search) > 200 {
		return nil, fmt.Errorf("search too long")
	}
	configured, err := s.entFromContext(ctx).Model.Query().Where(model.ModelIDIn(ids...), model.StatusEQ(model.StatusEnabled)).All(ctx)
	if err != nil {
		return nil, err
	}
	byID := lo.SliceToMap(configured, func(m *ent.Model) (string, *ent.Model) { return m.ModelID, m })
	out := make([]*objects.PortalModel, 0, len(available))
	channels := lo.Filter(s.models.channelService.GetEnabledChannels(), func(ch *Channel, _ int) bool { return KeyAllowsChannel(key, ch.ID, ch.Tags) })
	for _, facade := range available {
		m := byID[facade.ID]
		// Channel-derived identifiers have no invented capabilities or price metadata.
		if m == nil {
			m = &ent.Model{ModelID: facade.ID, Name: facade.ID, Type: "unknown"}
		}
		if search != nil && !strings.Contains(strings.ToLower(m.ModelID+" "+m.Name), strings.ToLower(*search)) {
			continue
		}
		if modelType != nil && *modelType != "" && string(m.Type) != *modelType {
			continue
		}
		dto := &objects.PortalModel{ID: m.ModelID, Name: m.Name, Type: string(m.Type), Developer: m.Developer, Icon: m.Icon, Remark: m.Remark, APIFormats: []string{}}
		if card := m.ModelCard; card != nil {
			dto.Vision = card.Vision
			dto.ToolCall = card.ToolCall
			dto.Reasoning = card.Reasoning.Supported
			if card.Limit.Context > 0 {
				dto.ContextLength = lo.ToPtr(card.Limit.Context)
			}
			if card.Limit.Output > 0 {
				dto.MaxOutputTokens = lo.ToPtr(card.Limit.Output)
			}
		}
		connections := MatchConnections(EffectiveModelAssociations(s.models.modelSettingsOrDefault(ctx), m), channels)
		if byID[facade.ID] == nil {
			for _, ch := range channels {
				if entry, ok := ch.GetModelEntries()[facade.ID]; ok {
					connections = append(connections, &ModelChannelConnection{Channel: ch.Channel, Models: []ChannelModelEntry{entry}})
				}
			}
		}
		var prices []*ent.ChannelModelPrice
		for _, conn := range connections {
			ch, found := lo.Find(channels, func(ch *Channel) bool { return ch.ID == conn.Channel.ID })
			if !found {
				continue
			}
			valid := false
			for _, entry := range conn.Models {
				if !GroupAllowsModel(key, entry.RequestModel) {
					continue
				}
				valid = true
				prices = append(prices, ch.cachedModelPrices[entry.RequestModel])
			}
			if !valid {
				continue
			}
			for _, ep := range ch.ResolveEndpoints() {
				if !slices.Contains(dto.APIFormats, ep.APIFormat) {
					dto.APIFormats = append(dto.APIFormats, ep.APIFormat)
				}
			}
		}
		dto.InputPrice, dto.OutputPrice = portalReferencePrices(prices)
		sort.Strings(dto.APIFormats)
		if len(dto.APIFormats) > 0 {
			out = append(out, dto)
		}
	}
	return out, nil
}

// A reference unit price exists only when every eligible route has that same
// configured per-token price. Missing or tiered prices remain null, never zero.
func portalReferencePrices(prices []*ent.ChannelModelPrice) (*float64, *float64) {
	unit := func(code objects.PriceItemCode) *float64 {
		var value *float64
		for _, p := range prices {
			if p == nil {
				return nil
			}
			var price *float64
			for _, item := range p.Price.Items {
				if item.ItemCode == code && item.Pricing.Mode == objects.PricingModeUsagePerUnit && item.Pricing.UsagePerUnit != nil {
					price = lo.ToPtr(item.Pricing.UsagePerUnit.InexactFloat64())
				}
			}
			if price == nil {
				return nil
			}
			if value != nil && *value != *price {
				return nil
			}
			value = price
		}
		return value
	}
	return unit(objects.PriceItemCodeUsage), unit(objects.PriceItemCodeCompletion)
}
