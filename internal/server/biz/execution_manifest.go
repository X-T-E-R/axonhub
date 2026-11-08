package biz

import (
	"context"
	"sort"
	"time"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/server/cluster"
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
	Version          int                   `json:"version"`
	GeneratedAt      time.Time             `json:"generated_at"`
	Nodes            map[string]string     `json:"nodes"`
	Models           []ModelExecutionRoute `json:"models"`
	PlacementVersion int64                 `json:"placement_version,omitempty"`
}

// ExecutionRouteForKey preserves public aliases and both group/model bounds.
// An unconditional model uses the lowest association priority and the sorted
// owner id. Conditional connections that still span machines stay ambiguous.
func (svc *ModelService) ExecutionRouteForKey(ctx context.Context, key *ent.APIKey, publicModel string) (ModelExecutionRoute, error) {
	node, err := svc.routeNode(ctx)
	if err != nil {
		return ModelExecutionRoute{}, err
	}
	return svc.executionRouteForKey(ctx, key, publicModel, node)
}

func (svc *ModelService) executionRouteForKey(ctx context.Context, key *ent.APIKey, publicModel string, node cluster.Config) (ModelExecutionRoute, error) {
	route := ModelExecutionRoute{Model: publicModel, Owners: []string{}, Status: "unavailable"}
	links, err := svc.routeLinks(ctx, key, publicModel, svc.routeChannels(ModelRouteQuery{}), node, ModelRouteQuery{})
	if err != nil {
		return route, err
	}
	owner, owners, status := selectStaticRoute(links)
	route.Owner = owner
	route.Owners = owners
	route.Status = status
	return route, nil
}

// BuildExecutionManifest is a conservative projection over every enabled Key's
// current public catalog. It contains no key IDs, secrets, credentials or bodies.
func (svc *ModelService) BuildExecutionManifest(ctx context.Context) (*ExecutionManifest, error) {
	ctx = authz.WithSystemBypass(ctx, "cluster-execution-manifest")
	node := svc.channelService.node
	if node.Enabled {
		_, state, err := readChannelPlacement(ctx, svc.channelService.db)
		if err != nil {
			return nil, err
		}
		node = node.AtPlacement(state.PlacementSnapshot)
	}
	manifest := &ExecutionManifest{Version: ExecutionManifestVersion, GeneratedAt: time.Now().UTC(), Nodes: node.Nodes, Models: []ModelExecutionRoute{}, PlacementVersion: node.PlacementVersion()}
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
				route, err := svc.executionRouteForKey(ctx, key, m.ID, node)
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
