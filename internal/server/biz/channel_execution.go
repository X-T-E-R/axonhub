package biz

import (
	"context"
	"fmt"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/cluster"
	"github.com/looplj/axonhub/llm/oauth"
)

type ExecutionOwnerError struct {
	ChannelID int
	Owner     string
}

func (e *ExecutionOwnerError) Error() string {
	return fmt.Sprintf("channel %d executes on node %q", e.ChannelID, e.Owner)
}
func (svc *ChannelService) ChannelOwnerError(id int) error {
	return &ExecutionOwnerError{ChannelID: id, Owner: svc.node.Owner(id)}
}
func (svc *ChannelService) NodeConfig() cluster.Config { return svc.node }
func (svc *ChannelService) OwnsChannel(id int) bool    { return svc.node.Owns(id) }

// Fresh execution fencing complements the global metadata cache. Revoking tags or
// disabling a channel cannot leave an authorization window until the next reload.
func (svc *ChannelService) AuthorizeExecutionChannel(ctx context.Context, id int, key *ent.APIKey) (bool, error) {
	if !svc.node.Enabled {
		return true, nil
	}
	if !svc.node.Owns(id) {
		return false, nil
	}
	ch, err := observationPrimaryClient(svc.db).Channel.Get(ctx, id)
	if ent.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	// Only the internal explicit management-test source retains the original
	// disabled-channel contract. The owner and current tags remain mandatory.
	source, _ := contexts.GetSource(ctx)
	managementTest := source == request.SourceTest && authz.RequireScope(ctx, scopes.ScopeReadChannels) == nil
	return (ch.Status == channel.StatusEnabled || managementTest) && KeyAllowsChannel(key, ch.ID, ch.Tags), nil
}

func (svc *ChannelService) requireExecutionPlacement(ctx context.Context, ids ...int) error {
	if !svc.node.Enabled {
		return nil
	}
	if err := authz.RequireScope(ctx, scopes.ScopeWriteChannels); err != nil {
		return err
	}
	_, state, err := readChannelPlacement(ctx, svc.entFromContext(ctx))
	if err != nil {
		return err
	}
	node := svc.node.AtPlacement(state.PlacementSnapshot)
	for _, id := range ids {
		if node.Owner(id) == "" {
			return fmt.Errorf("channel %d must have an execution machine before enabling it", id)
		}
	}
	return nil
}

// Validate identity placement before constructing any provider. Stable identities
// preserve placement across token rotation; duplicate current credentials provide
// a second check against accidentally splitting an account across nodes.
func (svc *ChannelService) validateExecutionIdentities(ctx context.Context) error {
	if !svc.node.Enabled {
		return nil
	}
	if err := svc.node.Validate(); err != nil {
		return err
	}
	channels, err := svc.entFromContext(ctx).Channel.Query().All(ctx)
	if err != nil {
		return err
	}
	owners := map[string]string{}
	for _, ch := range channels {
		owner := svc.node.Owner(ch.ID)
		if owner == "" {
			continue
		}
		keys := ch.Credentials.GetAllAPIKeys()
		creds := ch.Credentials.OAuth
		if creds == nil && ch.Credentials.IsOAuth() {
			creds, err = oauth.ParseCredentialsJSON(ch.Credentials.APIKey)
			if err != nil {
				return fmt.Errorf("channel %d OAuth identity: %w", ch.ID, err)
			}
		}
		if creds != nil {
			keys = append(keys, creds.AccessToken)
			if creds.RefreshToken != "" {
				keys = append(keys, creds.RefreshToken)
			}
		}
		for _, secret := range keys {
			if previous, exists := owners[secret]; exists && previous != owner {
				return fmt.Errorf("channel %d shares an upstream credential with a different owner", ch.ID)
			}
			owners[secret] = owner
		}
	}
	return nil
}
