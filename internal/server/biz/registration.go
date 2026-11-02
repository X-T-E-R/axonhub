package biz

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/registrationinvite"
	"github.com/looplj/axonhub/internal/ent/system"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
)

const SystemKeyInvitationRegistration = "system_invitation_registration"

var (
	ErrRegistrationClosed = errors.New("invitation registration is closed")
	ErrRegistrationInvite = errors.New("invitation is invalid, expired or exhausted")
	ErrRegistrationInput  = errors.New("check your email, name and matching password (at least 8 characters, at most 72 bytes)")
	ErrRegistrationEmail  = errors.New("email is already registered")
)

// This global user-admission capability requires the existing system-level scope,
// not a project owner/scope injected by X-Project-ID.
func registrationAdmin(ctx context.Context, scope scopes.ScopeSlug) (*ent.User, error) {
	u, ok := contexts.GetUser(ctx)
	if !ok || u == nil || u.Status != user.StatusActivated || u.AccessMode == user.AccessModePortal {
		return nil, fmt.Errorf("permission denied")
	}
	if scopes.HasSystemScope(u, scope) {
		return u, nil
	}
	return nil, fmt.Errorf("permission denied")
}

func (s *AuthService) InvitationRegistrationEnabled(ctx context.Context) (bool, error) {
	if s.AllowNoAuth {
		return false, nil
	}
	ctx = authz.WithSystemBypass(ctx, "public-registration-status")
	row, err := s.entFromContext(ctx).System.Query().Where(system.KeyEQ(SystemKeyInvitationRegistration)).Only(ctx)
	if ent.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return row.Value == "true", nil
}

func (s *AuthService) SetInvitationRegistration(ctx context.Context, enabled bool) (bool, error) {
	if _, err := registrationAdmin(ctx, scopes.ScopeWriteSettings); err != nil {
		return false, err
	}
	if enabled && s.AllowNoAuth {
		return false, fmt.Errorf("disable anonymous API access before enabling registration")
	}
	value := "false"
	if enabled {
		value = "true"
	}
	if err := s.SystemService.setSystemValue(ctx, SystemKeyInvitationRegistration, value); err != nil {
		return false, err
	}
	return enabled, nil
}

func inviteDigest(code string) string {
	digest := sha256.Sum256([]byte(code))
	return hex.EncodeToString(digest[:])
}

func inviteDTO(row *ent.RegistrationInvite) *objects.RegistrationInvite {
	status := "valid"
	switch {
	case row.Revoked:
		status = "revoked"
	case row.ExpiresAt != nil && !row.ExpiresAt.After(time.Now()):
		status = "expired"
	case row.UsedUses >= row.MaxUses:
		status = "used"
	case ValidatePortalGroup(row.Edges.Project) != nil:
		status = "unavailable"
	}
	name := ""
	if row.Edges.Project != nil {
		name = row.Edges.Project.Name
	}
	return &objects.RegistrationInvite{ID: row.ID, ProjectID: row.ProjectID, GroupName: name, MaxUses: row.MaxUses, UsedUses: row.UsedUses, ExpiresAt: row.ExpiresAt, CreatedAt: row.CreatedAt, Status: status}
}

func (s *AuthService) CreateRegistrationInvite(ctx context.Context, input objects.RegistrationInviteInput) (*objects.GeneratedRegistrationInvite, error) {
	u, err := registrationAdmin(ctx, scopes.ScopeWriteUsers)
	if err != nil {
		return nil, err
	}
	if input.MaxUses < 1 || input.MaxUses > 10000 {
		return nil, fmt.Errorf("uses must be between 1 and 10000")
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.After(time.Now()) {
		return nil, fmt.Errorf("expiry must be in the future")
	}
	ctx = authz.WithSystemBypass(ctx, "admin-create-registration-invite")
	client := s.entFromContext(ctx)
	g, err := client.Project.Get(ctx, input.ProjectID)
	if err != nil || ValidatePortalGroup(g) != nil {
		return nil, fmt.Errorf("group unavailable")
	}
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return nil, err
	}
	code := "axinv_" + base64.RawURLEncoding.EncodeToString(random)
	row, err := client.RegistrationInvite.Create().SetDigest(inviteDigest(code)).SetProjectID(g.ID).SetCreatedByID(u.ID).SetMaxUses(input.MaxUses).SetNillableExpiresAt(input.ExpiresAt).Save(ctx)
	if err != nil {
		return nil, err
	}
	row.Edges.Project = g
	return &objects.GeneratedRegistrationInvite{Code: code, Invite: inviteDTO(row)}, nil
}

func (s *AuthService) RegistrationInvites(ctx context.Context) ([]*objects.RegistrationInvite, error) {
	if _, err := registrationAdmin(ctx, scopes.ScopeWriteUsers); err != nil {
		return nil, err
	}
	ctx = authz.WithSystemBypass(ctx, "admin-list-registration-invites")
	rows, err := s.entFromContext(ctx).RegistrationInvite.Query().WithProject().Order(ent.Desc(registrationinvite.FieldID)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*objects.RegistrationInvite, 0, len(rows))
	for _, row := range rows {
		out = append(out, inviteDTO(row))
	}
	return out, nil
}

func (s *AuthService) RevokeRegistrationInvite(ctx context.Context, id int) (*objects.RegistrationInvite, error) {
	if _, err := registrationAdmin(ctx, scopes.ScopeWriteUsers); err != nil {
		return nil, err
	}
	ctx = authz.WithSystemBypass(ctx, "admin-revoke-registration-invite")
	row, err := s.entFromContext(ctx).RegistrationInvite.UpdateOneID(id).SetRevoked(true).Save(ctx)
	if err != nil {
		return nil, err
	}
	row.Edges.Project, err = row.QueryProject().Only(ctx)
	if err != nil && !ent.IsNotFound(err) {
		return nil, err
	}
	return inviteDTO(row), nil
}

func (s *AuthService) RegisterWithInvite(ctx context.Context, input objects.RegistrationInput) error {
	input.Email = strings.ToLower(strings.TrimSpace(input.Email))
	input.FirstName, input.LastName = strings.TrimSpace(input.FirstName), strings.TrimSpace(input.LastName)
	email, err := mail.ParseAddress(input.Email)
	if err != nil || email.Address != input.Email || len(input.Email) > 254 || input.FirstName == "" || input.LastName == "" || len(input.FirstName) > 128 || len(input.LastName) > 128 || utf8.RuneCountInString(input.Password) < 8 || len(input.Password) > 72 || input.Password != input.ConfirmPassword || input.Password == OIDC_ONLY_PLACEHOLDER {
		return ErrRegistrationInput
	}
	if len(input.InviteCode) != 49 || !strings.HasPrefix(input.InviteCode, "axinv_") {
		return ErrRegistrationInvite
	}
	enabled, err := s.InvitationRegistrationEnabled(ctx)
	if err != nil {
		return err
	}
	if !enabled {
		return ErrRegistrationClosed
	}
	// Hash before taking a transaction lock. The registration body and code are
	// never logged or put into a generic user-create input.
	hash, err := HashPassword(input.Password)
	if err != nil {
		return err
	}
	ctx = authz.WithSystemBypass(ctx, "public-invitation-registration")
	return s.RunInTransaction(ctx, func(ctx context.Context) error {
		client := s.entFromContext(ctx)
		// First statement is a write, so SQLite obtains its writer lock before a
		// read snapshot. Server databases lock the same settings row. Closing the
		// switch and admitting users therefore have one transaction order.
		_, err := client.System.Update().Where(system.KeyEQ(SystemKeyInvitationRegistration)).SetUpdatedAt(time.Now()).Save(ctx)
		if err != nil {
			return err
		}
		setting, err := client.System.Query().Where(system.KeyEQ(SystemKeyInvitationRegistration)).Only(ctx)
		if ent.IsNotFound(err) {
			return ErrRegistrationClosed
		}
		if err != nil {
			return err
		}
		if setting.Value != "true" {
			return ErrRegistrationClosed
		}
		now := time.Now()
		row, err := client.RegistrationInvite.Query().Where(registrationinvite.DigestEQ(inviteDigest(input.InviteCode)), registrationinvite.RevokedEQ(false), registrationinvite.Or(registrationinvite.ExpiresAtIsNil(), registrationinvite.ExpiresAtGT(now))).WithProject().Only(ctx)
		if ent.IsNotFound(err) {
			return ErrRegistrationInvite
		}
		if err != nil {
			return err
		}
		if row.UsedUses >= row.MaxUses || ValidatePortalGroup(row.Edges.Project) != nil {
			return ErrRegistrationInvite
		}
		// Compare-and-increment in the database, never a read/modify/write counter.
		n, err := client.RegistrationInvite.Update().Where(registrationinvite.IDEQ(row.ID), registrationinvite.UsedUsesEQ(row.UsedUses), registrationinvite.UsedUsesLT(row.MaxUses), registrationinvite.RevokedEQ(false), registrationinvite.Or(registrationinvite.ExpiresAtIsNil(), registrationinvite.ExpiresAtGT(now))).AddUsedUses(1).Save(ctx)
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrRegistrationInvite
		}
		exists, err := client.User.Query().Where(user.EmailEqualFold(input.Email)).Exist(ctx)
		if err != nil {
			return err
		}
		if exists {
			return ErrRegistrationEmail
		}
		u, err := client.User.Create().SetEmail(input.Email).SetPassword(hash).SetFirstName(input.FirstName).SetLastName(input.LastName).SetAccessMode(user.AccessModePortal).SetIsOwner(false).SetScopes([]string{}).Save(ctx)
		if ent.IsConstraintError(err) {
			return ErrRegistrationEmail
		}
		if err != nil {
			return err
		}
		return client.UserProject.Create().SetUserID(u.ID).SetProjectID(row.ProjectID).SetIsOwner(false).SetScopes([]string{}).Exec(ctx)
	})
}

func (s *AuthService) RegistrationGroups(ctx context.Context) ([]*objects.RegistrationGroup, error) {
	if _, err := registrationAdmin(ctx, scopes.ScopeWriteUsers); err != nil {
		return nil, err
	}
	ctx = authz.WithSystemBypass(ctx, "admin-registration-groups")
	rows, err := s.entFromContext(ctx).Project.Query().All(ctx)
	if err != nil {
		return nil, err
	}
	out := []*objects.RegistrationGroup{}
	for _, row := range rows {
		if ValidatePortalGroup(row) == nil {
			out = append(out, &objects.RegistrationGroup{ID: row.ID, Name: row.Name})
		}
	}
	return out, nil
}
