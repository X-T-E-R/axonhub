package orchestrator

import (
	"context"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/server/biz"
	"github.com/looplj/axonhub/llm"
)

// GroupAccessSelector filters each retry entry, not just the first candidate.
// Copy candidates: association-cache objects belong to other requests too.
type GroupAccessSelector struct {
	wrapped  CandidateSelector
	key      *ent.APIKey
	channels *biz.ChannelService
}

func (s *GroupAccessSelector) Select(ctx context.Context, req *llm.Request) ([]*ChannelModelsCandidate, error) {
	if !biz.GroupAllowsModel(s.key, req.Model) {
		return nil, nil
	}
	candidates, err := s.wrapped.Select(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make([]*ChannelModelsCandidate, 0, len(candidates))
	for _, c := range candidates {
		if c == nil || c.Channel == nil || !biz.KeyAllowsChannel(s.key, c.Channel.ID, c.Channel.Tags) {
			continue
		}
		if c.Channel.RemoteOwner != "" {
			continue
		}
		if s.channels != nil {
			allowed, err := s.channels.AuthorizeExecutionChannel(ctx, c.Channel.ID, s.key)
			if err != nil {
				return nil, err
			}
			if !allowed {
				continue
			}
		}
		copy := *c
		copy.Models = make([]biz.ChannelModelEntry, 0, len(c.Models))
		for _, entry := range c.Models {
			if biz.GroupAllowsModel(s.key, entry.RequestModel) {
				copy.Models = append(copy.Models, entry)
			}
		}
		if len(copy.Models) > 0 {
			out = append(out, &copy)
		}
	}
	return out, nil
}
