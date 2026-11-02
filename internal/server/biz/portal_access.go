package biz

import (
	"context"
	"fmt"
	"slices"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/ent/userproject"
	"github.com/looplj/axonhub/internal/pkg/xregexp"
)

// RefreshAPIKeyAuthorization reads current authorization facts without an auth cache.
// The returned key and project form a snapshot for one HTTP request/WS generation.
func RefreshAPIKeyAuthorization(ctx context.Context, client *ent.Client, secret string) (*ent.APIKey, error) {
	ctx = authz.WithSystemBypass(ctx, "current-api-key-authorization")
	key, err := client.APIKey.Query().Where(apikey.KeyEQ(secret)).WithProject().Only(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: key unavailable", ErrInvalidAPIKey)
	}
	if key.Status != apikey.StatusEnabled {
		return nil, fmt.Errorf("api key not enabled: %w", ErrInvalidAPIKey)
	}
	if key.Edges.Project == nil || key.Edges.Project.Status != project.StatusActive {
		return nil, fmt.Errorf("api key project not valid: %w", ErrInvalidAPIKey)
	}
	if key.UserID != 0 {
		owner, err := client.User.Get(ctx, key.UserID)
		if err != nil {
			if key.PortalManaged {
				return nil, fmt.Errorf("%w: owner unavailable", ErrInvalidAPIKey)
			}
			// A missing legacy creator does not add a new membership prerequisite.
			if !ent.IsNotFound(err) {
				return nil, err
			}
		} else {
			if owner.AccessMode == user.AccessModePortal && !key.PortalManaged {
				return nil, fmt.Errorf("%w: portal owner requires a managed personal key", ErrInvalidAPIKey)
			}
			if key.PortalManaged && (owner.Status != user.StatusActivated || owner.IsOwner) {
				return nil, fmt.Errorf("%w: owner inactive or incompatible", ErrInvalidAPIKey)
			}
		}
	}
	if key.PortalManaged {
		if key.Type != apikey.TypePersonal || key.UserID == 0 {
			return nil, fmt.Errorf("%w: invalid managed key", ErrInvalidAPIKey)
		}
		member, err := client.UserProject.Query().Where(userproject.UserIDEQ(key.UserID), userproject.ProjectIDEQ(key.ProjectID)).Exist(ctx)
		if err != nil {
			return nil, err
		}
		if !member {
			return nil, fmt.Errorf("%w: group membership revoked", ErrInvalidAPIKey)
		}
		if err := ValidatePortalGroup(key.Edges.Project); err != nil {
			return nil, err
		}
	}
	return key, nil
}

func ValidatePortalGroup(group *ent.Project) error {
	p := group.GetActiveProfile()
	if group == nil || group.Status != project.StatusActive || p == nil || p.ModelIDs == nil || len(*p.ModelIDs) == 0 || (len(p.ChannelIDs) == 0 && len(p.ChannelTags) == 0) {
		return fmt.Errorf("%w: group requires explicit models and channels", ErrInvalidAPIKey)
	}
	return nil
}

// GroupAllowsModel deliberately distinguishes missing legacy models from [].
// Channel/provider ActualModel is not a public model identifier.
func GroupAllowsModel(key *ent.APIKey, modelID string) bool {
	if key == nil {
		return true
	}
	p := key.Edges.Project.GetActiveProfile()
	if p == nil || p.ModelIDs == nil {
		return !key.PortalManaged
	}
	return slices.Contains(*p.ModelIDs, modelID)
}

func KeyAllowsModel(key *ent.APIKey, modelID string) bool {
	if !GroupAllowsModel(key, modelID) {
		return false
	}
	p := key.GetActiveProfile()
	return p == nil || len(p.ModelIDs) == 0 || slices.Contains(p.ModelIDs, modelID)
}

func KeyAllowsChannel(key *ent.APIKey, channelID int, tags []string) bool {
	if key == nil {
		return true
	}
	group := key.Edges.Project.GetActiveProfile()
	if key.PortalManaged && (group == nil || (len(group.ChannelIDs) == 0 && len(group.ChannelTags) == 0)) {
		return false
	}
	if group != nil && ((len(group.ChannelIDs) > 0 && !slices.Contains(group.ChannelIDs, channelID)) || !group.MatchChannelTags(tags)) {
		return false
	}
	p := key.GetActiveProfile()
	return p == nil || ((len(p.ChannelIDs) == 0 || slices.Contains(p.ChannelIDs, channelID)) && p.MatchChannelTags(tags))
}

func portalCurrentUser(ctx context.Context) (*ent.User, error) {
	u, ok := contexts.GetUser(ctx)
	if !ok || u == nil || u.Status != user.StatusActivated {
		return nil, fmt.Errorf("authentication required")
	}
	return u, nil
}

// KeyMappedModel resolves the existing first-match profile mapping without changing
// its semantics. Directories and inference use the same logical target boundary.
func KeyMappedModel(key *ent.APIKey, modelID string) string {
	if p := key.GetActiveProfile(); p != nil {
		for _, mapping := range p.ModelMappings {
			if xregexp.MatchString(mapping.From, modelID) {
				return mapping.To
			}
		}
	}
	return modelID
}
