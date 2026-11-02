package biz

import (
	"context"
	"fmt"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/user"
)

func (s *AuthService) PortalEnabled(ctx context.Context) (bool, error) {
	ctx = authz.WithSystemBypass(ctx, "portal-anonymous-conflict")
	users, err := s.entFromContext(ctx).User.Query().Where(user.AccessModeEQ(user.AccessModePortal)).Exist(ctx)
	if err != nil || users {
		return users, err
	}
	return s.entFromContext(ctx).APIKey.Query().Where(apikey.PortalManagedEQ(true)).Exist(ctx)
}

// SetUserAccessMode requires the legacy user-management policy. Enabling portal
// is an explicit operation: existing shared credentials are never reclassified.
func (s *AuthService) SetUserAccessMode(ctx context.Context, id int, mode user.AccessMode, disableLegacyKeys bool) (*ent.User, error) {
	if err := s.UserService.permissionValidator.CanEditUserPermissions(ctx, id, nil); err != nil {
		return nil, fmt.Errorf("permission denied: %w", err)
	}
	if mode != user.AccessModeLegacy && mode != user.AccessModePortal {
		return nil, fmt.Errorf("invalid access mode")
	}
	if mode == user.AccessModePortal && s.AllowNoAuth {
		return nil, fmt.Errorf("disable anonymous API access before enabling portal users")
	}
	var result *ent.User
	err := s.RunInTransaction(ctx, func(ctx context.Context) error {
		client := s.entFromContext(ctx)
		u, err := client.User.Get(ctx, id)
		if err != nil {
			return err
		}
		if mode == user.AccessModePortal {
			if u.IsOwner {
				return fmt.Errorf("owner cannot use portal access mode")
			}
			bypass := authz.WithSystemBypass(ctx, "portal-mode-existing-credentials")
			old, err := client.APIKey.Query().Where(apikey.UserIDEQ(id), apikey.PortalManagedEQ(false), apikey.StatusEQ(apikey.StatusEnabled)).All(bypass)
			if err != nil {
				return err
			}
			if len(old) > 0 && !disableLegacyKeys {
				return fmt.Errorf("user has enabled legacy/shared keys; explicitly disable them before switching mode")
			}
			if len(old) > 0 {
				_, err = client.APIKey.Update().Where(apikey.UserIDEQ(id), apikey.PortalManagedEQ(false)).SetStatus(apikey.StatusDisabled).Save(bypass)
				if err != nil {
					return err
				}
			}
		}
		result, err = client.User.UpdateOneID(id).SetAccessMode(mode).Save(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.UserService.invalidateUserCache(ctx, id)
	return result, nil
}
