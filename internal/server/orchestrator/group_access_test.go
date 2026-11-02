package orchestrator

import (
	"context"
	"testing"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
	"github.com/samber/lo"
	"github.com/stretchr/testify/require"
)

type portalCandidates struct{ rows []*ChannelModelsCandidate }

func (s portalCandidates) Select(context.Context, *llm.Request) ([]*ChannelModelsCandidate, error) {
	return s.rows, nil
}

func TestPortalCandidateEveryEntryAndChannelBoundary(t *testing.T) {
	allowed := []string{"public", "logical"}
	key := &ent.APIKey{PortalManaged: true, Edges: ent.APIKeyEdges{Project: &ent.Project{Profiles: &objects.ProjectProfiles{ActiveProfile: "a", Profiles: []objects.ProjectProfile{
		{Name: "a", ModelIDs: &allowed, ChannelIDs: []int{1}},
		{Name: "inactive", ModelIDs: lo.ToPtr([]string{"forbidden"}), ChannelIDs: []int{2}},
	}}}}}
	// Provider-side IDs are not public IDs. An allowed entry retains its actual
	// provider model; a disallowed later retry entry must never survive filtering.
	candidates := []*ChannelModelsCandidate{
		{Channel: &biz.Channel{Channel: &ent.Channel{ID: 1}}, Models: []biz.ChannelModelEntry{{RequestModel: "public", ActualModel: "provider-not-public"}, {RequestModel: "forbidden", ActualModel: "provider-secret"}, {RequestModel: "logical", ActualModel: "provider-logical"}}},
		{Channel: &biz.Channel{Channel: &ent.Channel{ID: 2}}, Models: []biz.ChannelModelEntry{{RequestModel: "public", ActualModel: "provider-other"}}},
	}
	selector := &GroupAccessSelector{wrapped: portalCandidates{candidates}, key: key}
	got, err := selector.Select(t.Context(), &llm.Request{Model: "public"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Models, 2)
	require.Equal(t, "provider-not-public", got[0].Models[0].ActualModel)
	require.Len(t, candidates[0].Models, 3, "filter must not mutate association cache")
	got, err = selector.Select(t.Context(), &llm.Request{Model: "forbidden"})
	require.NoError(t, err)
	require.Empty(t, got)
	// Explicit empty is denial for both new and old keys. Missing preserves legacy.
	key.Edges.Project.Profiles.Profiles[0].ModelIDs = lo.ToPtr([]string{})
	got, err = selector.Select(t.Context(), &llm.Request{Model: "public"})
	require.NoError(t, err)
	require.Empty(t, got)
	key.Edges.Project.Profiles.Profiles[0].ModelIDs = nil
	got, err = selector.Select(t.Context(), &llm.Request{Model: "public"})
	require.NoError(t, err)
	require.Empty(t, got)
	key.PortalManaged = false
	got, err = selector.Select(t.Context(), &llm.Request{Model: "public"})
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0].Models, 3)
}
