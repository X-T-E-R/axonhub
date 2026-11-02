package biz

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/require"
	"path/filepath"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/registrationinvite"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
)

func TestRegistrationTransactionAndConcurrentConsumption(t *testing.T) {
	db := enttest.NewEntClient(t, "sqlite3", "file:"+filepath.ToSlash(filepath.Join(t.TempDir(), "registration.db"))+"?_pragma=busy_timeout%2810000%29&_pragma=journal_mode%28WAL%29")
	defer db.Close()
	db.Driver().(*entsql.Driver).DB().SetMaxOpenConns(8)
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), db)
	owner := db.User.Create().SetEmail("owner@example.test").SetPassword("synthetic").SetIsOwner(true).SaveX(ctx)
	admin := contexts.WithUser(ctx, owner)
	system := NewSystemService(SystemServiceParams{Ent: db, CacheConfig: xcache.Config{Mode: xcache.ModeMemory}})
	auth := NewAuthService(AuthServiceParams{Ent: db, SystemService: system})
	ids := []string{"m"}
	g := db.Project.Create().SetName("Invite group").SetProfiles(&objects.ProjectProfiles{ActiveProfile: "p", Profiles: []objects.ProjectProfile{{Name: "p", ModelIDs: &ids, ChannelIDs: []int{1}}}}).SaveX(ctx)
	input := func(code, email string) objects.RegistrationInput {
		return objects.RegistrationInput{Email: email, Password: "SyntheticPassword1", ConfirmPassword: "SyntheticPassword1", FirstName: "First", LastName: "Last", InviteCode: code}
	}
	code := func(uses int) *objects.GeneratedRegistrationInvite {
		v, err := auth.CreateRegistrationInvite(admin, objects.RegistrationInviteInput{ProjectID: g.ID, MaxUses: uses})
		require.NoError(t, err)
		return v
	}
	uses := func(id int) int { return db.RegistrationInvite.GetX(ctx, id).UsedUses }
	enabled, err := auth.InvitationRegistrationEnabled(t.Context())
	require.NoError(t, err)
	require.False(t, enabled)
	one := code(1)
	require.ErrorIs(t, auth.RegisterWithInvite(t.Context(), input(one.Code, "closed@example.test")), ErrRegistrationClosed)
	require.Zero(t, uses(one.Invite.ID))
	_, err = auth.SetInvitationRegistration(admin, true)
	require.NoError(t, err)
	require.NoError(t, auth.RegisterWithInvite(t.Context(), input(one.Code, "New@Example.test")))
	u := db.User.Query().Where(user.EmailEQ("new@example.test")).WithRoles().WithProjectUsers().OnlyX(ctx)
	require.Equal(t, user.AccessModePortal, u.AccessMode)
	require.False(t, u.IsOwner)
	require.Empty(t, u.Scopes)
	require.Empty(t, u.Edges.Roles)
	require.Len(t, u.Edges.ProjectUsers, 1)
	require.Equal(t, g.ID, u.Edges.ProjectUsers[0].ProjectID)
	require.False(t, u.Edges.ProjectUsers[0].IsOwner)
	require.Empty(t, u.Edges.ProjectUsers[0].Scopes)
	require.NoError(t, VerifyPassword(u.Password, "SyntheticPassword1"))
	require.NotContains(t, db.RegistrationInvite.GetX(ctx, one.Invite.ID).String(), one.Code)
	require.NotEqual(t, one.Code, db.RegistrationInvite.GetX(ctx, one.Invite.ID).Digest)
	require.ErrorIs(t, auth.RegisterWithInvite(t.Context(), input(one.Code, "exhausted@example.test")), ErrRegistrationInvite)
	multi := code(2)
	require.ErrorIs(t, auth.RegisterWithInvite(t.Context(), input(multi.Code, "NEW@example.test")), ErrRegistrationEmail)
	require.Zero(t, uses(multi.Invite.ID), "duplicate email rolls back counter")
	require.NoError(t, auth.RegisterWithInvite(t.Context(), input(multi.Code, "next@example.test")))
	require.Equal(t, 1, uses(multi.Invite.ID))
	revoked := code(1)
	_, err = auth.RevokeRegistrationInvite(admin, revoked.Invite.ID)
	require.NoError(t, err)
	require.ErrorIs(t, auth.RegisterWithInvite(t.Context(), input(revoked.Code, "revoked@example.test")), ErrRegistrationInvite)
	require.Zero(t, uses(revoked.Invite.ID))
	expired := code(1)
	// Expiry is immutable in product operations; create an expired fixture directly.
	oldCode := "axinv_" + "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	old := db.RegistrationInvite.Create().SetDigest(inviteDigest(oldCode)).SetProjectID(g.ID).SetCreatedByID(owner.ID).SetMaxUses(1).SetExpiresAt(time.Now().Add(-time.Minute)).SaveX(ctx)
	require.ErrorIs(t, auth.RegisterWithInvite(t.Context(), input(oldCode, "expired@example.test")), ErrRegistrationInvite)
	require.Zero(t, uses(old.ID))
	db.Project.UpdateOne(g).SetStatus(project.StatusArchived).SaveX(ctx)
	require.ErrorIs(t, auth.RegisterWithInvite(t.Context(), input(expired.Code, "group@example.test")), ErrRegistrationInvite)
	require.Zero(t, uses(expired.Invite.ID))
	unavailable, err := auth.RegistrationInvites(admin)
	require.NoError(t, err)
	for _, row := range unavailable {
		if row.ID == expired.Invite.ID {
			require.Equal(t, "unavailable", row.Status)
		}
	}
	db.Project.UpdateOne(g).SetStatus(project.StatusActive).SaveX(ctx)
	_, err = auth.SetInvitationRegistration(admin, false)
	require.NoError(t, err)
	require.ErrorIs(t, auth.RegisterWithInvite(t.Context(), input(expired.Code, "disabled@example.test")), ErrRegistrationClosed)
	_, err = auth.SetInvitationRegistration(admin, true)
	require.NoError(t, err)
	last := code(1)
	var wg sync.WaitGroup
	var success atomic.Int32
	start := make(chan struct{})
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results <- fmt.Errorf("panic: %v", r)
				}
			}()
			<-start
			err := auth.RegisterWithInvite(t.Context(), input(last.Code, fmt.Sprintf("race-%d@example.test", i)))
			if err == nil {
				success.Add(1)
			}
			results <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			require.ErrorIs(t, err, ErrRegistrationInvite)
		}
	}
	require.EqualValues(t, 1, success.Load())
	require.Equal(t, 1, uses(last.Invite.ID))
	require.Equal(t, 1, db.User.Query().Where(user.EmailHasPrefix("race-")).CountX(ctx))
	rows, err := auth.RegistrationInvites(admin)
	require.NoError(t, err)
	states := map[int]string{}
	for _, row := range rows {
		states[row.ID] = row.Status
	}
	require.Equal(t, "used", states[last.Invite.ID])
	require.Equal(t, "revoked", states[revoked.Invite.ID])
	require.Equal(t, "expired", states[old.ID])
	plain := db.User.Create().SetEmail("plain@example.test").SetPassword("synthetic").SaveX(ctx)
	plainCtx := contexts.WithUser(authz.NewUserContext(t.Context(), plain.ID), plain)
	_, err = auth.CreateRegistrationInvite(plainCtx, objects.RegistrationInviteInput{ProjectID: g.ID, MaxUses: 1})
	require.Error(t, err)
	_, err = auth.SetInvitationRegistration(plainCtx, true)
	require.Error(t, err)
	_, err = auth.RegistrationInvites(plainCtx)
	require.Error(t, err)
	require.Equal(t, 1, db.RegistrationInvite.Query().Where(registrationinvite.IDEQ(last.Invite.ID), registrationinvite.UsedUsesEQ(1)).CountX(ctx))
	manager := db.User.UpdateOne(plain).SetScopes([]string{"write_users"}).SaveX(ctx)
	managerCtx := contexts.WithUser(authz.NewUserContext(t.Context(), manager.ID), manager)
	future := time.Now().Add(time.Hour).Truncate(time.Second)
	configured, err := auth.CreateRegistrationInvite(managerCtx, objects.RegistrationInviteInput{ProjectID: g.ID, MaxUses: 3, ExpiresAt: &future})
	require.NoError(t, err)
	require.Equal(t, 3, configured.Invite.MaxUses)
	require.True(t, configured.Invite.ExpiresAt.Equal(future))
	_, err = auth.SetInvitationRegistration(managerCtx, false)
	require.Error(t, err, "user management alone cannot change global settings")
	for _, uses := range []int{0, -1, 10001} {
		_, err = auth.CreateRegistrationInvite(admin, objects.RegistrationInviteInput{ProjectID: g.ID, MaxUses: uses})
		require.Error(t, err)
	}
	for _, patch := range []func(*objects.RegistrationInput){
		func(in *objects.RegistrationInput) { in.ConfirmPassword = "different" },
		func(in *objects.RegistrationInput) { in.FirstName = "" },
		func(in *objects.RegistrationInput) { in.Password = "字字字"; in.ConfirmPassword = in.Password },
		func(in *objects.RegistrationInput) { in.Email = "not-email" },
	} {
		in := input(configured.Code, "invalid@example.test")
		patch(&in)
		require.ErrorIs(t, auth.RegisterWithInvite(t.Context(), in), ErrRegistrationInput)
	}
	require.Zero(t, uses(configured.Invite.ID))
	auth.AllowNoAuth = true
	enabled, err = auth.InvitationRegistrationEnabled(t.Context())
	require.NoError(t, err)
	require.False(t, enabled)
	_, err = auth.SetInvitationRegistration(admin, true)
	require.Error(t, err)
	require.ErrorIs(t, auth.RegisterWithInvite(t.Context(), input(configured.Code, "anonymous@example.test")), ErrRegistrationClosed)
}
