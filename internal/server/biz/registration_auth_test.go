package biz

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/role"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
)

func TestRegistrationEmailAuthentication(t *testing.T) {
	db := enttest.NewEntClient(t, "sqlite3", "file:registration-email-"+uuid.NewString()+"?mode=memory&_fk=1")
	defer db.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), db)
	system := NewSystemService(SystemServiceParams{Ent: db, CacheConfig: xcache.Config{Mode: xcache.ModeMemory}})
	auth := NewAuthService(AuthServiceParams{Ent: db, SystemService: system})
	owner := db.User.Create().SetEmail("Owner.Case@Example.test").SetPassword("unused").SetIsOwner(true).SaveX(ctx)
	admin := contexts.WithUser(ctx, owner)
	ids := []string{"m"}
	group := db.Project.Create().SetName("Email group").SetProfiles(&objects.ProjectProfiles{ActiveProfile: "p", Profiles: []objects.ProjectProfile{{Name: "p", ModelIDs: &ids, ChannelIDs: []int{1}}}}).SaveX(ctx)
	_, err := auth.SetInvitationRegistration(admin, true)
	require.NoError(t, err)
	invite, err := auth.CreateRegistrationInvite(admin, objects.RegistrationInviteInput{ProjectID: group.ID, MaxUses: 3})
	require.NoError(t, err)
	password := "SyntheticPassword1"
	input := objects.RegistrationInput{Email: "New.Case@Example.test", Password: password, ConfirmPassword: password, FirstName: "First", LastName: "Last", InviteCode: invite.Code}
	require.NoError(t, auth.RegisterWithInvite(t.Context(), input))
	registered := db.User.Query().Where(user.EmailEQ("new.case@example.test")).OnlyX(ctx)
	for _, email := range []string{"new.case@example.test", input.Email, "NEW.CASE@EXAMPLE.TEST"} {
		t.Run("registered_"+email, func(t *testing.T) {
			u, err := auth.AuthenticateUser(t.Context(), email, password)
			require.NoError(t, err)
			require.Equal(t, registered.ID, u.ID)
		})
	}
	_, err = auth.AuthenticateUser(t.Context(), input.Email, "wrong-password")
	require.ErrorIs(t, err, ErrInvalidPassword)
	input.Email = "NEW.CASE@example.test"
	require.ErrorIs(t, auth.RegisterWithInvite(t.Context(), input), ErrRegistrationEmail)
	require.Equal(t, 1, db.RegistrationInvite.GetX(ctx, invite.Invite.ID).UsedUses, "duplicate normalized email rolls back consumption")
	require.Equal(t, 1, db.User.Query().Where(user.EmailEqualFold(input.Email)).CountX(ctx))

	hash, err := HashPassword(password)
	require.NoError(t, err)
	db.User.UpdateOne(owner).SetPassword(hash).SaveX(ctx)
	legacy := db.User.Create().SetEmail("Legacy.Case@Example.test").SetPassword(hash).SaveX(ctx)
	legacyLower := db.User.Create().SetEmail("legacy-lower@example.test").SetPassword(hash).SaveX(ctx)
	legacyDuplicate := input
	legacyDuplicate.Email = "LEGACY.CASE@example.test"
	require.ErrorIs(t, auth.RegisterWithInvite(t.Context(), legacyDuplicate), ErrRegistrationEmail)
	require.Equal(t, 1, db.RegistrationInvite.GetX(ctx, invite.Invite.ID).UsedUses)
	require.Equal(t, legacy.Email, db.User.GetX(ctx, legacy.ID).Email)
	// An exact old identity stays authoritative even if another portal identity
	// has its lowercase spelling; neither a bad password nor deactivation falls through.
	twin := db.User.Create().SetEmail("Twin@Example.test").SetPassword(hash).SaveX(ctx)
	portalHash, err := HashPassword("OtherPortalPassword1")
	require.NoError(t, err)
	db.User.Create().SetEmail("twin@example.test").SetPassword(portalHash).SetAccessMode(user.AccessModePortal).SaveX(ctx)
	blocked := db.User.Create().SetEmail("Blocked@Example.test").SetPassword(hash).SetStatus(user.StatusDeactivated).SaveX(ctx)
	db.User.Create().SetEmail("blocked@example.test").SetPassword(portalHash).SetAccessMode(user.AccessModePortal).SaveX(ctx)
	for _, u := range []*ent.User{owner, legacy, legacyLower, twin} {
		got, err := auth.AuthenticateUser(t.Context(), u.Email, password)
		require.NoError(t, err)
		require.Equal(t, u.ID, got.ID)
	}
	for _, email := range []string{"legacy.case@example.test", "LEGACY-LOWER@example.test", "owner.case@example.test", "unknown@example.test"} {
		_, err := auth.AuthenticateUser(t.Context(), email, password)
		require.ErrorIs(t, err, ErrInvalidPassword, "old identities are not made case-insensitive")
	}
	for _, email := range []string{twin.Email, blocked.Email} {
		_, err := auth.AuthenticateUser(t.Context(), email, "OtherPortalPassword1")
		require.ErrorIs(t, err, ErrInvalidPassword, "exact old identity must not fall through to a different account")
	}
	db.User.UpdateOne(registered).SetStatus(user.StatusDeactivated).SaveX(ctx)
	_, err = auth.AuthenticateUser(t.Context(), input.Email, password)
	require.ErrorIs(t, err, ErrInvalidPassword)
	db.User.UpdateOne(registered).SetStatus(user.StatusActivated).SaveX(ctx)

	// Both the original exact lookup and the new portal lookup still apply OIDC restrictions.
	auth.OIDCService = &OIDCService{AbstractService: auth.AbstractService, cfg: OIDCConfig{Providers: []OIDCProvider{{IssuerURL: "https://oidc.example.test", OIDCLoginOnly: true}}}}
	db.OIDCIdentity.Create().SetUserID(registered.ID).SetIssuer("https://oidc.example.test").SetSubject("registered").SetEmail(registered.Email).SaveX(ctx)
	db.OIDCIdentity.Create().SetUserID(legacy.ID).SetIssuer("https://oidc.example.test").SetSubject("legacy").SetEmail(legacy.Email).SaveX(ctx)
	for _, email := range []string{registered.Email, input.Email, legacy.Email} {
		_, err := auth.AuthenticateUser(t.Context(), email, password)
		require.ErrorIs(t, err, ErrOIDCLoginRequired)
	}
	t.Log("F-05: registered email variants resolve one account; normalized duplicate rolls back; legacy/Owner exact identities, password/deactivation precedence and OIDC-only controls preserved")
}

func TestRegistrationAdministrationSystemScopes(t *testing.T) {
	db := enttest.NewEntClient(t, "sqlite3", "file:registration-scopes-"+uuid.NewString()+"?mode=memory&_fk=1")
	defer db.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), db)
	system := NewSystemService(SystemServiceParams{Ent: db, CacheConfig: xcache.Config{Mode: xcache.ModeMemory}})
	auth := NewAuthService(AuthServiceParams{Ent: db, SystemService: system})
	ids := []string{"m"}
	group := db.Project.Create().SetName("Scope group").SetProfiles(&objects.ProjectProfiles{ActiveProfile: "p", Profiles: []objects.ProjectProfile{{Name: "p", ModelIDs: &ids, ChannelIDs: []int{1}}}}).SaveX(ctx)
	owner := db.User.Create().SetEmail("owner@example.test").SetPassword("unused").SetIsOwner(true).SaveX(ctx)
	admin := contexts.WithUser(ctx, owner)
	type accessCase struct {
		name, mode, status                       string
		direct, roleScopes                       []string
		projectRole, projectOwner, projectScopes bool
		settings, users                          bool
	}
	for _, tc := range []accessCase{
		{name: "direct wildcard", direct: []string{"*"}, settings: true, users: true},
		{name: "system role wildcard", roleScopes: []string{"*"}, settings: true, users: true},
		{name: "explicit settings", direct: []string{"write_settings"}, settings: true},
		{name: "explicit users", direct: []string{"write_users"}, users: true},
		{name: "explicit system role", roleScopes: []string{"write_settings", "write_users"}, settings: true, users: true},
		{name: "portal wildcard", mode: "portal", direct: []string{"*"}},
		{name: "deactivated wildcard", status: "deactivated", direct: []string{"*"}},
		{name: "project owner", projectOwner: true},
		{name: "project role wildcard", roleScopes: []string{"*"}, projectRole: true},
		{name: "project scopes", projectScopes: true},
		{name: "plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builder := db.User.Create().SetEmail(uuid.NewString() + "@example.test").SetPassword("unused").SetScopes(tc.direct)
			if tc.mode != "" {
				builder.SetAccessMode(user.AccessMode(tc.mode))
			}
			if tc.status != "" {
				builder.SetStatus(user.Status(tc.status))
			}
			u := builder.SaveX(ctx)
			if tc.roleScopes != nil {
				r := db.Role.Create().SetName(tc.name).SetScopes(tc.roleScopes)
				if tc.projectRole {
					r.SetProjectID(group.ID).SetLevel(role.LevelProject)
				}
				db.User.UpdateOne(u).AddRoleIDs(r.SaveX(ctx).ID).SaveX(ctx)
			}
			membership := db.UserProject.Create().SetUserID(u.ID).SetProjectID(group.ID).SetIsOwner(tc.projectOwner)
			if tc.projectScopes {
				membership.SetScopes([]string{"*"})
			}
			membership.SaveX(ctx)
			u = db.User.Query().Where(user.IDEQ(u.ID)).WithRoles().WithProjectUsers().OnlyX(ctx)
			// Deliberately carry real project context and project ownership/scopes.
			actor := contexts.WithProjectID(contexts.WithUser(authz.NewUserContext(t.Context(), u.ID), u), group.ID)
			assertAccess := func(err error, allowed bool) {
				t.Helper()
				if allowed {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "permission denied")
				}
			}
			_, err := auth.SetInvitationRegistration(admin, false)
			require.NoError(t, err)
			_, err = auth.SetInvitationRegistration(actor, true)
			assertAccess(err, tc.settings)
			enabled, err := auth.InvitationRegistrationEnabled(t.Context())
			require.NoError(t, err)
			require.Equal(t, tc.settings, enabled)
			_, err = auth.RegistrationGroups(actor)
			assertAccess(err, tc.users)
			before := db.RegistrationInvite.Query().CountX(ctx)
			_, err = auth.CreateRegistrationInvite(actor, objects.RegistrationInviteInput{ProjectID: group.ID, MaxUses: 1})
			assertAccess(err, tc.users)
			if !tc.users {
				require.Equal(t, before, db.RegistrationInvite.Query().CountX(ctx))
			}
			_, err = auth.RegistrationInvites(actor)
			assertAccess(err, tc.users)
			invite, err := auth.CreateRegistrationInvite(admin, objects.RegistrationInviteInput{ProjectID: group.ID, MaxUses: 1})
			require.NoError(t, err)
			_, err = auth.RevokeRegistrationInvite(actor, invite.Invite.ID)
			assertAccess(err, tc.users)
			require.Equal(t, tc.users, db.RegistrationInvite.GetX(ctx, invite.Invite.ID).Revoked)
		})
	}
	t.Log("F-06: switch, groups, create, list and revoke use system scopes; portal/deactivated/project ownership, roles and scopes cannot grant global admission")
}
