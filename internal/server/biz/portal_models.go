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

func (s *PortalService) modelCatalogKey(ctx context.Context, projectID int, keyID *int) (*ent.APIKey, error) {
	g, err := s.group(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("group unavailable")
	}
	if ValidatePortalGroup(g) != nil {
		return nil, nil
	}
	key := &ent.APIKey{ProjectID: projectID, PortalManaged: true}
	key.Edges.Project = g
	if keyID != nil {
		key, err = s.ownKey(ctx, *keyID, false)
		if err != nil || key.ProjectID != projectID {
			return nil, fmt.Errorf("key unavailable")
		}
		if key.Status != apikey.StatusEnabled {
			return nil, nil
		}
		key.Edges.Project = g
	}
	return key, nil
}

func (s *PortalService) Models(ctx context.Context, projectID int, keyID *int, search *string, modelType *string) ([]*objects.PortalModel, error) {
	key, err := s.modelCatalogKey(ctx, projectID, keyID)
	if err != nil {
		return nil, err
	}
	if key == nil {
		return []*objects.PortalModel{}, nil
	}
	return s.modelsForKey(ctx, key, search, modelType)
}

func (s *PortalService) modelsForKey(ctx context.Context, key *ent.APIKey, search, modelType *string) ([]*objects.PortalModel, error) {
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
	for _, facade := range available {
		ids = append(ids, KeyMappedModel(key, facade.ID))
	}
	configured, err := s.entFromContext(ctx).Model.Query().Where(model.ModelIDIn(ids...), model.StatusEQ(model.StatusEnabled)).All(ctx)
	if err != nil {
		return nil, err
	}
	byID := lo.SliceToMap(configured, func(m *ent.Model) (string, *ent.Model) { return m.ModelID, m })
	out := make([]*objects.PortalModel, 0, len(available))
	channels := lo.Filter(s.models.channelService.GetEnabledChannels(), func(ch *Channel, _ int) bool { return KeyAllowsChannel(key, ch.ID, ch.Tags) })
	for _, facade := range available {
		m := byID[KeyMappedModel(key, facade.ID)]
		if m != nil {
			copy := *m
			copy.ModelID, copy.Name = facade.ID, facade.DisplayName
			if alias := byID[facade.ID]; alias != nil {
				copy.Name, copy.Icon, copy.Remark = alias.Name, alias.Icon, alias.Remark
			}
			m = &copy
		}
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
		connections, err := s.models.modelConnectionsForKey(ctx, key, facade.ID)
		if err != nil {
			return nil, err
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

// ModelChannels groups a model once per currently eligible channel. Only display
// DTOs leave this boundary; no channel configuration or credentials are exposed.
func (s *PortalService) ModelChannels(ctx context.Context, projectID int, keyID *int, search, modelType *string) ([]*objects.PortalModelChannel, error) {
	key, err := s.modelCatalogKey(ctx, projectID, keyID)
	if err != nil {
		return nil, err
	}
	out := []*objects.PortalModelChannel{}
	if key == nil {
		return out, nil
	}
	models, err := s.modelsForKey(ctx, key, search, modelType)
	if err != nil {
		return nil, err
	}
	ctx = authz.WithSystemBypass(contexts.WithAPIKey(ctx, key), "portal-model-channels")
	channels := lo.SliceToMap(s.models.channelService.GetEnabledChannels(), func(ch *Channel) (int, *Channel) { return ch.ID, ch })
	groups := map[int]*objects.PortalModelChannel{}
	for _, m := range models {
		connections, err := s.models.modelConnectionsForKey(ctx, key, m.ID)
		if err != nil {
			return nil, err
		}
		prices := map[int][]*ent.ChannelModelPrice{}
		for _, conn := range connections {
			ch := channels[conn.Channel.ID]
			if ch == nil || len(ch.ResolveEndpoints()) == 0 {
				continue
			}
			for _, entry := range conn.Models {
				prices[ch.ID] = append(prices[ch.ID], ch.cachedModelPrices[entry.RequestModel])
			}
		}
		for id, routePrices := range prices {
			ch := channels[id]
			if groups[id] == nil {
				groups[id] = &objects.PortalModelChannel{ID: fmt.Sprintf("channel-%d", id), Name: ch.Name, Models: []*objects.PortalModel{}}
				out = append(out, groups[id])
			}
			copy := *m
			copy.APIFormats = []string{}
			for _, ep := range ch.ResolveEndpoints() {
				if !slices.Contains(copy.APIFormats, ep.APIFormat) {
					copy.APIFormats = append(copy.APIFormats, ep.APIFormat)
				}
			}
			sort.Strings(copy.APIFormats)
			copy.InputPrice, copy.OutputPrice = portalReferencePrices(routePrices)
			groups[id].Models = append(groups[id].Models, &copy)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
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
