package biz

import (
	"context"
	"fmt"
	"sort"

	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/model"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xregexp"
	"github.com/looplj/axonhub/internal/server/cluster"
)

type routeLink struct {
	ChannelID   int
	Owner       string
	Priority    int
	Conditional bool
}

type ModelRouteQuery struct {
	OwnerOverride  map[int]string
	Focus          map[int]bool
	ExcludeChannel int
	Include        *Channel
	DraftModel     string
	Draft          []*objects.ModelAssociation
	UseDraft       bool
	Structural     bool
}

type ModelMachineNotice struct {
	Model    string   `json:"model"`
	Machines []string `json:"machines"`
	Winner   string   `json:"winner,omitempty"`
}

func associationConditional(assoc *objects.ModelAssociation) bool {
	return assoc != nil && assoc.When != nil && assoc.When.Enabled
}

// selectStaticRoute always converges to a single static winner machine when
// candidate links exist: the smallest association priority wins, and equal
// priorities break ties by the sorted owner id.
func selectStaticRoute(links []routeLink) (string, []string, string) {
	valid := lo.Filter(links, func(link routeLink, _ int) bool { return link.Owner != "" })
	if len(valid) == 0 {
		return "", []string{}, "unavailable"
	}
	winner := lowestPriorityOwner(valid)
	return winner, []string{winner}, "ready"
}

func lowestPriorityOwner(links []routeLink) string {
	best := map[string]int{}
	for _, link := range links {
		if link.Owner == "" {
			continue
		}
		if priority, ok := best[link.Owner]; !ok || link.Priority < priority {
			best[link.Owner] = link.Priority
		}
	}
	minPriority := 0
	first := true
	for _, priority := range best {
		if first || priority < minPriority {
			minPriority = priority
			first = false
		}
	}
	tied := make([]string, 0, 1)
	for owner, priority := range best {
		if priority == minPriority {
			tied = append(tied, owner)
		}
	}
	sort.Strings(tied)
	return tied[0]
}

func (svc *ModelService) routeLinks(ctx context.Context, key *ent.APIKey, publicID string, channels []*Channel, node cluster.Config, query ModelRouteQuery) ([]routeLink, error) {
	if key != nil && !KeyAllowsModel(key, publicID) {
		return nil, nil
	}
	target := publicID
	if key != nil {
		target = KeyMappedModel(key, publicID)
		if !GroupAllowsModel(key, target) {
			return nil, nil
		}
	}
	visible := channels
	if key != nil {
		visible = lo.Filter(channels, func(ch *Channel, _ int) bool { return KeyAllowsChannel(key, ch.ID, ch.Tags) })
	}
	settings := svc.modelSettingsOrDefault(ctx)
	var associations []*objects.ModelAssociation
	if query.UseDraft && !query.Structural && target == query.DraftModel {
		associations = query.Draft
	} else {
		row, err := svc.GetModelByModelID(ctx, target, model.StatusEnabled)
		if err != nil {
			if !ent.IsNotFound(err) {
				return nil, fmt.Errorf("resolve catalog target: %w", err)
			}
			if !settings.FallbackToChannelsOnModelNotFound {
				return nil, nil
			}
			return svc.fallbackRouteLinks(visible, node, query, target)
		}
		if query.UseDraft && query.Structural && row.Developer == query.DraftModel {
			if row.Settings != nil && row.Settings.DisableDeveloperSettingsInheritance {
				associations = mergeInheritedModelAssociations(nil, row.Settings.Associations)
			} else {
				var modelAssocs []*objects.ModelAssociation
				if row.Settings != nil {
					modelAssocs = row.Settings.Associations
				}
				associations = mergeInheritedModelAssociations(
					inheritDeveloperAssociationsForModel(query.Draft, row.ModelID),
					modelAssocs,
				)
			}
		} else {
			associations = EffectiveModelAssociations(settings, row)
		}
	}
	return svc.associationRouteLinks(associations, visible, key, node, query)
}

func (svc *ModelService) fallbackRouteLinks(channels []*Channel, node cluster.Config, query ModelRouteQuery, target string) ([]routeLink, error) {
	links := make([]routeLink, 0)
	for _, ch := range channels {
		if _, ok := ch.GetModelEntries()[target]; !ok {
			continue
		}
		owner, err := routeOwner(node, query.OwnerOverride, ch.ID)
		if err != nil {
			return nil, err
		}
		links = append(links, routeLink{ChannelID: ch.ID, Owner: owner, Priority: 0})
	}
	return links, nil
}

func (svc *ModelService) associationRouteLinks(associations []*objects.ModelAssociation, channels []*Channel, key *ent.APIKey, node cluster.Config, query ModelRouteQuery) ([]routeLink, error) {
	links := make([]routeLink, 0)
	for _, match := range MatchAssociations(associations, channels) {
		conditional := associationConditional(match.Association)
		for _, conn := range match.Connections {
			if key != nil && !lo.SomeBy(conn.Models, func(entry ChannelModelEntry) bool { return GroupAllowsModel(key, entry.RequestModel) }) {
				continue
			}
			owner, err := routeOwner(node, query.OwnerOverride, conn.Channel.ID)
			if err != nil {
				return nil, err
			}
			links = append(links, routeLink{ChannelID: conn.Channel.ID, Owner: owner, Priority: conn.Priority, Conditional: conditional})
		}
	}
	return links, nil
}

func routeOwner(node cluster.Config, override map[int]string, channelID int) (string, error) {
	if owner, ok := override[channelID]; ok && owner != "" {
		return owner, nil
	}
	owner := node.Owner(channelID)
	if owner == "" {
		return "", fmt.Errorf("channel %d has no owner", channelID)
	}
	return owner, nil
}

func (svc *ModelService) routeChannels(query ModelRouteQuery) []*Channel {
	channels := append([]*Channel{}, svc.channelService.GetEnabledChannels()...)
	if query.ExcludeChannel > 0 {
		channels = lo.Filter(channels, func(ch *Channel, _ int) bool { return ch.ID != query.ExcludeChannel })
	}
	if query.Include != nil && !lo.ContainsBy(channels, func(ch *Channel) bool { return ch.ID == query.Include.ID }) {
		channels = append(channels, query.Include)
	}
	return channels
}

func (svc *ModelService) routeNode(ctx context.Context) (cluster.Config, error) {
	node := svc.channelService.node
	if !node.Enabled {
		return node, nil
	}
	_, state, err := readChannelPlacement(ctx, svc.channelService.db)
	if err != nil {
		return node, err
	}
	return node.AtPlacement(state.PlacementSnapshot), nil
}

func (svc *ModelService) ModelMachineNotices(ctx context.Context, query ModelRouteQuery) ([]ModelMachineNotice, error) {
	node, err := svc.routeNode(ctx)
	if err != nil {
		return nil, err
	}
	if !node.Enabled && len(query.OwnerOverride) == 0 {
		return []ModelMachineNotice{}, nil
	}
	channels := svc.routeChannels(query)
	configured, err := svc.entFromContext(ctx).Model.Query().Where(model.StatusEQ(model.StatusEnabled)).All(ctx)
	if err != nil {
		return nil, err
	}
	var inheriting map[string]bool
	if query.Structural {
		inheriting = map[string]bool{}
		for _, row := range configured {
			if row.Developer == query.DraftModel && (row.Settings == nil || !row.Settings.DisableDeveloperSettingsInheritance) {
				inheriting[row.ModelID] = true
			}
		}
		if len(inheriting) == 0 {
			links, err := svc.associationRouteLinks(query.Draft, channels, nil, node, query)
			if err != nil {
				return nil, err
			}
			return noticesFromLinks(query.DraftModel, links, len(query.Focus) > 0 && !linksTouch(links, query.Focus)), nil
		}
	}
	settings := svc.modelSettingsOrDefault(ctx)
	ids := lo.Map(configured, func(row *ent.Model, _ int) string { return row.ModelID })
	if settings.QueryAllChannelModels {
		for _, ch := range channels {
			for id := range ch.GetModelEntries() {
				if settings.ModelBlacklistRegex != "" && xregexp.MatchString(settings.ModelBlacklistRegex, id) {
					continue
				}
				ids = append(ids, id)
			}
		}
	}
	ids = lo.Uniq(ids)
	type acc struct {
		links    []routeLink
		raw      map[string]bool
		focused  bool
		seenKeys int
	}
	rows := map[string]*acc{}
	cursor := 0
	for {
		keys, err := svc.entFromContext(ctx).APIKey.Query().Where(apikey.IDGT(cursor), apikey.StatusEQ(apikey.StatusEnabled)).Order(ent.Asc(apikey.FieldID)).WithProject().Limit(100).All(ctx)
		if err != nil {
			return nil, err
		}
		if len(keys) == 0 {
			break
		}
		for _, key := range keys {
			cursor = key.ID
			if key.Type == apikey.TypeNoauth {
				continue
			}
			for _, publicID := range routePublicIDs(key, ids, query) {
				links, err := svc.routeLinks(ctx, key, publicID, channels, node, query)
				if err != nil {
					return nil, err
				}
				if len(links) == 0 {
					continue
				}
				row := rows[publicID]
				if row == nil {
					row = &acc{raw: map[string]bool{}}
					rows[publicID] = row
				}
				row.seenKeys++
				if linksTouch(links, query.Focus) {
					row.focused = true
				}
				for _, link := range links {
					row.raw[link.Owner] = true
				}
				row.links = append(row.links, links...)
			}
		}
	}
	notices := make([]ModelMachineNotice, 0)
	for name, row := range rows {
		if len(query.Focus) > 0 && !row.focused {
			continue
		}
		if query.UseDraft {
			if query.Structural {
				if !inheriting[name] {
					continue
				}
			} else if query.DraftModel != "" && name != query.DraftModel {
				continue
			}
		}
		machines := lo.Keys(row.raw)
		sort.Strings(machines)
		if len(machines) < 2 {
			continue
		}
		winner, _, _ := selectStaticRoute(row.links)
		notices = append(notices, ModelMachineNotice{Model: name, Machines: machines, Winner: winner})
	}
	sort.Slice(notices, func(i, j int) bool { return notices[i].Model < notices[j].Model })
	return notices, nil
}

func routePublicIDs(key *ent.APIKey, configured []string, query ModelRouteQuery) []string {
	seen := map[string]bool{}
	ids := make([]string, 0, len(configured)+1)
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	for _, id := range configured {
		add(id)
	}
	if query.UseDraft && !query.Structural {
		add(query.DraftModel)
	}
	if profile := key.GetActiveProfile(); profile != nil {
		for _, id := range profile.ModelIDs {
			add(id)
		}
		for _, mapping := range profile.ModelMappings {
			if isLiteralModelAlias(mapping.From) {
				add(mapping.From)
			}
		}
	}
	if group := key.Edges.Project.GetActiveProfile(); group != nil && group.ModelIDs != nil {
		for _, id := range *group.ModelIDs {
			add(id)
		}
	}
	return ids
}

func linksTouch(links []routeLink, focus map[int]bool) bool {
	if len(focus) == 0 {
		return false
	}
	for _, link := range links {
		if focus[link.ChannelID] {
			return true
		}
	}
	return false
}

func noticesFromLinks(model string, links []routeLink, hidden bool) []ModelMachineNotice {
	if hidden || model == "" {
		return []ModelMachineNotice{}
	}
	owners := map[string]bool{}
	for _, link := range links {
		owners[link.Owner] = true
	}
	machines := lo.Keys(owners)
	sort.Strings(machines)
	if len(machines) < 2 {
		return []ModelMachineNotice{}
	}
	owner, _, status := selectStaticRoute(links)
	notice := ModelMachineNotice{Model: model, Machines: machines}
	if status == "ready" {
		notice.Winner = owner
	}
	return []ModelMachineNotice{notice}
}
