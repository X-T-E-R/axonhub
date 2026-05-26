package biz

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/pkg/xcache"
)

func newRegistrationTestAuthService(t *testing.T, registration RegistrationConfig) (*AuthService, *ent.Client, context.Context) {
	t.Helper()

	client := setupTestDB(t)
	ctx := ent.NewContext(context.Background(), client)
	ctx = authz.WithTestBypass(ctx)

	authService := &AuthService{
		AbstractService: &AbstractService{db: client},
		Registration:    registration,
	}

	return authService, client, ctx
}

func TestAuthService_SignUpRejectsShortPassword(t *testing.T) {
	authService, client, ctx := newRegistrationTestAuthService(t, RegistrationConfig{Enabled: true})
	defer client.Close()

	_, err := authService.SignUp(ctx, SignUpInput{
		Email:    "short@example.com",
		Password: "short",
	})

	require.Error(t, err)
	require.Contains(t, err.Error(), "password must be at least")
}

func TestAuthService_SignUpUsesSystemRegistrationPolicyOverride(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	ctx := ent.NewContext(context.Background(), client)
	ctx = authz.WithTestBypass(ctx)

	systemService := &SystemService{
		Cache: xcache.NewFromConfig[ent.System](xcache.Config{}),
	}
	require.NoError(t, systemService.SetRegistrationConfig(ctx, RegistrationConfig{Enabled: true}))

	authService := &AuthService{
		AbstractService: &AbstractService{db: client},
		SystemService:   systemService,
		Registration:    RegistrationConfig{Enabled: false},
	}

	createdUser, err := authService.SignUp(ctx, SignUpInput{
		Email:     "enabled-by-system@example.com",
		Password:  "password123",
		FirstName: "Enabled",
		LastName:  "User",
	})

	require.NoError(t, err)
	require.Equal(t, "enabled-by-system@example.com", createdUser.Email)
	require.False(t, createdUser.IsOwner)
	require.Equal(t, user.StatusActivated, createdUser.Status)
}

func TestOIDCService_ResolveUserRejectsJITWhenOIDCRegistrationDisabled(t *testing.T) {
	client := setupTestDB(t)
	defer client.Close()

	ctx := ent.NewContext(context.Background(), client)
	ctx = authz.WithTestBypass(ctx)

	service := &OIDCService{
		AbstractService: &AbstractService{db: client},
		registration:    RegistrationConfig{OIDCEnabled: false},
	}
	provider := &oidcProvider{
		config: OIDCProvider{
			Name:       "test",
			JITEnabled: true,
		},
	}

	_, err := service.resolveUser(ctx, provider, "subject-1", "oidc@example.com", true, "", "", "", "", nil)

	require.ErrorIs(t, err, ErrOIDCRegistrationDisabled)
}
