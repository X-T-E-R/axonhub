package biz

import (
	"context"
	"fmt"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent/oidcidentity"
)

func (s *PortalService) UnlinkOIDCIdentity(ctx context.Context, id int) (bool, error) {
	u, err := portalCurrentUser(ctx)
	if err != nil {
		return false, err
	}
	err = s.RunInTransaction(ctx, func(ctx context.Context) error {
		ctx = authz.WithSystemBypass(ctx, "portal-own-oidc-identity")
		client := s.entFromContext(ctx)
		identity, err := client.OIDCIdentity.Query().Where(oidcidentity.IDEQ(id), oidcidentity.UserIDEQ(u.ID)).Only(ctx)
		if err != nil {
			return fmt.Errorf("identity unavailable")
		}
		if u.Password == OIDC_ONLY_PLACEHOLDER {
			count, err := client.OIDCIdentity.Query().Where(oidcidentity.UserIDEQ(u.ID)).Count(ctx)
			if err != nil {
				return err
			}
			if count <= 1 {
				return fmt.Errorf("set a local password before unlinking your last identity")
			}
		}
		return client.OIDCIdentity.DeleteOne(identity).Exec(ctx)
	})
	return err == nil, err
}
