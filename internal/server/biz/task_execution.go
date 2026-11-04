package biz

import (
	"context"
	"fmt"
	"strings"

	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/request"
)

// AuthorizedTask resolves persisted ownership without constructing a provider or
// querying the upstream. Both query protocols address the same persisted task;
// the chosen protocol only controls the response representation.
func (s *VideoService) AuthorizedTask(ctx context.Context, externalID string) (*ent.Request, error) {
	client := ent.FromContext(ctx)
	if client == nil {
		return nil, fmt.Errorf("%w: ent client not found in context", ErrInternal)
	}
	task, err := client.Request.Query().Where(request.ExternalID(externalID)).Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("task unavailable")
	}
	if err := s.authorizeTask(ctx, task); err != nil {
		return nil, fmt.Errorf("task unavailable")
	}
	return task, nil
}

func (s *VideoService) authorizeTask(ctx context.Context, task *ent.Request) error {
	if strings.TrimSpace(task.ExternalID) == "" || task.ChannelID == 0 {
		return fmt.Errorf("task unavailable")
	}
	ch, err := observationPrimaryClient(s.ChannelService.db).Channel.Get(ctx, task.ChannelID)
	if err != nil {
		return err
	}
	if key, ok := contexts.GetAPIKey(ctx); ok {
		if s.ChannelService.node.Enabled {
			current, err := RefreshAPIKeyAuthorization(ctx, observationPrimaryClient(s.ChannelService.db), key.Key)
			if err != nil {
				return err
			}
			key = current
			if ch.Status != channel.StatusEnabled || !KeyAllowsModel(key, task.ModelID) || !GroupAllowsModel(key, KeyMappedModel(key, task.ModelID)) || !KeyAllowsChannel(key, ch.ID, ch.Tags) {
				return fmt.Errorf("task unavailable")
			}
		}
		if key.PortalManaged && (task.APIKeyID != key.ID || task.ProjectID != key.ProjectID || !KeyAllowsModel(key, task.ModelID) || !KeyAllowsChannel(key, ch.ID, ch.Tags)) {
			return fmt.Errorf("%w: task unavailable", ErrInvalidAPIKey)
		}
	}
	return nil
}
