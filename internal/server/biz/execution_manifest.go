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
	Status string   `json:"status"`
}
type ExecutionManifest struct {
	Version          int                   `json:"version"`
	GeneratedAt      time.Time             `json:"generated_at"`
	Nodes            map[string]string     `json:"nodes"`
	Models           []ModelExecutionRoute `json:"models"`
	PlacementVersion int64                 `json:"placement_version,omitempty"`
}

// ExecutionRouteForKey preserves public aliases and both group/model bounds,
// selecting a single static owner by the lowest association priority and sorted owner id.
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

// BuildExecutionManifest projects every enabled Key's current public catalog
// (including public aliases) to a single static owner using lowest association
// priority and a stable sorted owner tie-breaker. It contains no key IDs,
// secrets, credentials or bodies.
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
	channels := svc.routeChannels(ModelRouteQuery{})
	modelLinks := map[string][]routeLink{}
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
				links, err := svc.routeLinks(ctx, key, m.ID, channels, node, ModelRouteQuery{})
				if err != nil {
					return nil, err
				}
				if len(links) == 0 {
					continue
				}
				modelLinks[m.ID] = append(modelLinks[m.ID], links...)
			}
		}
	}
	for model, links := range modelLinks {
		owner, owners, status := selectStaticRoute(links)
		if status == "unavailable" {
			continue
		}
		manifest.Models = append(manifest.Models, ModelExecutionRoute{
			Model:  model,
			Owner:  owner,
			Owners: owners,
			Status: status,
		})
	}
	sort.Slice(manifest.Models, func(i, j int) bool { return manifest.Models[i].Model < manifest.Models[j].Model })
	return manifest, nil
}
