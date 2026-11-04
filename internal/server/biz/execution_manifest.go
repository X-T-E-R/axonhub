package biz

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
)

const ExecutionManifestVersion = 1

type ModelExecutionRoute struct {
	Model  string   `json:"model"`
	Owner  string   `json:"owner,omitempty"`
	Owners []string `json:"owners"`
	// Ambiguous models need authenticated resolution or configuration alignment;
	// they must never silently fall back to a default origin.
	Status string `json:"status"`
}
type ExecutionManifest struct {
	Version     int                   `json:"version"`
	GeneratedAt time.Time             `json:"generated_at"`
	Nodes       map[string]string     `json:"nodes"`
	Models      []ModelExecutionRoute `json:"models"`
}

// ExecutionRouteForKey preserves public aliases and both group/model bounds.
// Conditions are deliberately not interpreted at the edge: a route is definitive
// only when all authorized connections (including conditional ones) share owner.
func (svc *ModelService) ExecutionRouteForKey(ctx context.Context, key *ent.APIKey, publicModel string) (ModelExecutionRoute, error) {
	route := ModelExecutionRoute{Model: publicModel, Owners: []string{}, Status: "unavailable"}
	connections, err := svc.modelConnectionsForKey(ctx, key, publicModel)
	if err != nil {
		return route, err
	}
	owners := map[string]bool{}
	for _, conn := range connections {
		owner := svc.channelService.node.Owner(conn.Channel.ID)
		if owner == "" {
			return route, fmt.Errorf("channel %d has no owner", conn.Channel.ID)
		}
		owners[owner] = true
	}
	for owner := range owners {
		route.Owners = append(route.Owners, owner)
	}
	sort.Strings(route.Owners)
	if len(route.Owners) == 1 {
		route.Owner = route.Owners[0]
		route.Status = "ready"
	} else if len(route.Owners) > 1 {
		route.Status = "ambiguous"
	}
	return route, nil
}

// BuildExecutionManifest is a conservative projection over every enabled Key's
// current public catalog. It contains no key IDs, secrets, credentials or bodies.
func (svc *ModelService) BuildExecutionManifest(ctx context.Context) (*ExecutionManifest, error) {
	ctx = authz.WithSystemBypass(ctx, "cluster-execution-manifest")
	manifest := &ExecutionManifest{Version: ExecutionManifestVersion, GeneratedAt: time.Now().UTC(), Nodes: svc.channelService.node.Nodes, Models: []ModelExecutionRoute{}}
	modelOwners := map[string]map[string]bool{}
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
			catalogs, err := svc.listMappedModelFacades(ctx, key)
			if err != nil {
				return nil, err
			}
			for _, m := range catalogs {
				route, err := svc.ExecutionRouteForKey(ctx, key, m.ID)
				if err != nil {
					return nil, err
				}
				if modelOwners[m.ID] == nil {
					modelOwners[m.ID] = map[string]bool{}
				}
				for _, owner := range route.Owners {
					modelOwners[m.ID][owner] = true
				}
			}
		}
	}
	for model, owners := range modelOwners {
		route := ModelExecutionRoute{Model: model, Owners: []string{}, Status: "ambiguous"}
		for owner := range owners {
			route.Owners = append(route.Owners, owner)
		}
		sort.Strings(route.Owners)
		if len(route.Owners) == 1 {
			route.Owner = route.Owners[0]
			route.Status = "ready"
		}
		manifest.Models = append(manifest.Models, route)
	}
	sort.Slice(manifest.Models, func(i, j int) bool { return manifest.Models[i].Model < manifest.Models[j].Model })
	return manifest, nil
}
