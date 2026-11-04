package biz

import (
	"context"
	"fmt"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/model"
	"github.com/looplj/axonhub/internal/pkg/xregexp"
	"github.com/samber/lo"
)

// modelConnectionsForKey resolves the same logical target as the execution
// mapper. The original public ID is checked against the key; the mapped target
// and every association entry are checked against the group upper boundary.
func (svc *ModelService) modelConnectionsForKey(ctx context.Context, key *ent.APIKey, publicID string) ([]*ModelChannelConnection, error) {
	if !KeyAllowsModel(key, publicID) {
		return nil, nil
	}
	target := KeyMappedModel(key, publicID)
	if !GroupAllowsModel(key, target) {
		return nil, nil
	}
	channels := lo.Filter(svc.channelService.GetEnabledChannels(), func(ch *Channel, _ int) bool {
		return KeyAllowsChannel(key, ch.ID, ch.Tags)
	})
	settings := svc.modelSettingsOrDefault(ctx)
	m, err := svc.GetModelByModelID(ctx, target, model.StatusEnabled)
	var connections []*ModelChannelConnection
	if err == nil {
		connections = MatchConnections(EffectiveModelAssociations(settings, m), channels)
	} else if ent.IsNotFound(err) {
		if !settings.FallbackToChannelsOnModelNotFound {
			return nil, nil
		}
		for _, ch := range channels {
			if entry, ok := ch.GetModelEntries()[target]; ok {
				connections = append(connections, &ModelChannelConnection{Channel: ch.Channel, Models: []ChannelModelEntry{entry}})
			}
		}
	} else {
		return nil, fmt.Errorf("resolve catalog target: %w", err)
	}
	out := make([]*ModelChannelConnection, 0, len(connections))
	for _, conn := range connections {
		entries := lo.Filter(conn.Models, func(entry ChannelModelEntry, _ int) bool { return GroupAllowsModel(key, entry.RequestModel) })
		if len(entries) > 0 {
			copy := *conn
			copy.Models = entries
			out = append(out, &copy)
		}
	}
	return out, nil
}

// listMappedModelFacades keeps the client's public identifiers while checking
// availability on their mapped targets. Explicit finite key/group model lists
// also make aliases discoverable when an alias has no independent model route.
func (svc *ModelService) listMappedModelFacades(ctx context.Context, key *ent.APIKey) ([]ModelFacade, error) {
	configured, err := svc.entFromContext(ctx).Model.Query().Where(model.StatusEQ(model.StatusEnabled)).All(ctx)
	if err != nil {
		return nil, err
	}
	candidates := make([]ModelFacade, 0, len(configured))
	seen := map[string]bool{}
	for _, m := range configured {
		seen[m.ModelID] = true
		candidates = append(candidates, ModelFacade{ID: m.ModelID, DisplayName: m.ModelID, CreatedAt: m.CreatedAt, Created: m.CreatedAt.Unix(), OwnedBy: "configured"})
	}
	settings := svc.modelSettingsOrDefault(ctx)
	if settings.QueryAllChannelModels {
		for _, ch := range svc.channelService.GetEnabledChannels() {
			for id := range ch.GetModelEntries() {
				if seen[id] || (settings.ModelBlacklistRegex != "" && xregexp.MatchString(settings.ModelBlacklistRegex, id)) {
					continue
				}
				seen[id] = true
				candidates = append(candidates, ModelFacade{ID: id, DisplayName: id, CreatedAt: ch.CreatedAt, Created: ch.CreatedAt.Unix(), OwnedBy: ch.Type.String()})
			}
		}
		var aliases []string
		if profile := key.GetActiveProfile(); profile != nil {
			aliases = append(aliases, profile.ModelIDs...)
		}
		if group := key.Edges.Project.GetActiveProfile(); group != nil && group.ModelIDs != nil {
			aliases = append(aliases, *group.ModelIDs...)
		}
		for _, id := range aliases {
			if seen[id] || KeyMappedModel(key, id) == id || (settings.ModelBlacklistRegex != "" && xregexp.MatchString(settings.ModelBlacklistRegex, id)) {
				continue
			}
			seen[id] = true
			candidates = append(candidates, ModelFacade{ID: id, DisplayName: id, OwnedBy: "mapped"})
		}
	}
	out := make([]ModelFacade, 0, len(candidates))
	for _, candidate := range candidates {
		connections, err := svc.modelConnectionsForKey(ctx, key, candidate.ID)
		if err != nil {
			return nil, err
		}
		if len(connections) > 0 {
			out = append(out, candidate)
		}
	}
	return out, nil
}
