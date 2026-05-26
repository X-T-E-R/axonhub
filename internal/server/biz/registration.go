package biz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/ent/userproject"
	"github.com/looplj/axonhub/internal/scopes"
)

type RegistrationConfig struct {
	Enabled                bool     `conf:"enabled" yaml:"enabled" json:"enabled"`
	OIDCEnabled            bool     `conf:"oidc_enabled" yaml:"oidc_enabled" json:"oidc_enabled"`
	InviteCode             string   `conf:"invite_code" yaml:"invite_code" json:"invite_code"`
	DefaultProjectID       int      `conf:"default_project_id" yaml:"default_project_id" json:"default_project_id"`
	AutoJoinFirstProject   bool     `conf:"auto_join_first_project" yaml:"auto_join_first_project" json:"auto_join_first_project"`
	DefaultProjectScopes   []string `conf:"default_project_scopes" yaml:"default_project_scopes" json:"default_project_scopes"`
	AllowRequestDetails    bool     `conf:"allow_request_details" yaml:"allow_request_details" json:"allow_request_details"`
	SelfServicePresetNames []string `conf:"self_service_preset_names" yaml:"self_service_preset_names" json:"self_service_preset_names"`
}

const MinimumPasswordLength = 8

type SignUpInput struct {
	Email      string
	Password   string
	FirstName  string
	LastName   string
	InviteCode string
}

var ErrRegistrationDisabled = errors.New("registration is disabled")
var ErrPasswordRegistrationDisabled = errors.New("password registration is disabled")
var ErrOIDCRegistrationDisabled = errors.New("OIDC registration is disabled")

func (s *SystemService) RegistrationConfig(ctx context.Context, fallback RegistrationConfig) (RegistrationConfig, error) {
	fallback = normalizeRegistrationConfig(fallback)

	if s == nil {
		return fallback, nil
	}

	value, err := authz.RunWithSystemBypass(ctx, "system-registration-policy", func(bypassCtx context.Context) (string, error) {
		return s.getSystemValue(bypassCtx, SystemKeyRegistrationPolicy)
	})
	if err != nil {
		if ent.IsNotFound(err) {
			return fallback, nil
		}

		return RegistrationConfig{}, fmt.Errorf("failed to get registration policy: %w", err)
	}

	var cfg RegistrationConfig
	if err := json.Unmarshal([]byte(value), &cfg); err != nil {
		return RegistrationConfig{}, fmt.Errorf("failed to unmarshal registration policy: %w", err)
	}

	return normalizeRegistrationConfig(cfg), nil
}

func (s *SystemService) SetRegistrationConfig(ctx context.Context, cfg RegistrationConfig) error {
	cfg = normalizeRegistrationConfig(cfg)
	if err := validateRegistrationConfig(cfg); err != nil {
		return err
	}

	jsonBytes, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("failed to marshal registration policy: %w", err)
	}

	return s.setSystemValue(ctx, SystemKeyRegistrationPolicy, string(jsonBytes))
}

func (s *AuthService) RegistrationPolicy(ctx context.Context) (RegistrationConfig, error) {
	if s.SystemService == nil {
		return normalizeRegistrationConfig(s.Registration), nil
	}

	return s.SystemService.RegistrationConfig(ctx, s.Registration)
}

func (s *AuthService) PasswordRegistrationEnabled() bool {
	return s.OIDCService == nil || !s.OIDCService.PasswordAuthDisabled()
}

func (s *AuthService) SignUp(ctx context.Context, input SignUpInput) (*ent.User, error) {
	cfg, err := s.RegistrationPolicy(ctx)
	if err != nil {
		return nil, err
	}

	if !cfg.Enabled {
		return nil, ErrRegistrationDisabled
	}
	if !s.PasswordRegistrationEnabled() {
		return nil, ErrPasswordRegistrationDisabled
	}

	if strings.TrimSpace(cfg.InviteCode) != "" && strings.TrimSpace(input.InviteCode) != strings.TrimSpace(cfg.InviteCode) {
		return nil, fmt.Errorf("invalid invitation code")
	}

	email := strings.ToLower(strings.TrimSpace(input.Email))
	if email == "" {
		return nil, fmt.Errorf("email is required")
	}

	if strings.TrimSpace(input.Password) == "" {
		return nil, fmt.Errorf("password is required")
	}
	if len([]rune(input.Password)) < MinimumPasswordLength {
		return nil, fmt.Errorf("password must be at least %d characters long", MinimumPasswordLength)
	}

	hashedPassword, err := HashPassword(input.Password)
	if err != nil {
		return nil, err
	}

	return authz.RunWithSystemBypass(ctx, "signup-create-user", func(bypassCtx context.Context) (*ent.User, error) {
		client := s.entFromContext(bypassCtx)

		exists, err := client.User.Query().Where(user.EmailEqualFold(email)).Exist(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to check existing user: %w", err)
		}
		if exists {
			return nil, fmt.Errorf("email is already registered")
		}

		var newUser *ent.User
		err = s.RunInTransaction(bypassCtx, func(txCtx context.Context) error {
			txClient := s.entFromContext(txCtx)

			mut := txClient.User.Create().
				SetEmail(email).
				SetPassword(hashedPassword).
				SetStatus(user.StatusActivated).
				SetIsOwner(false).
				SetScopes([]string{})

			if strings.TrimSpace(input.FirstName) != "" {
				mut.SetFirstName(strings.TrimSpace(input.FirstName))
			}
			if strings.TrimSpace(input.LastName) != "" {
				mut.SetLastName(strings.TrimSpace(input.LastName))
			}

			newUser, err = mut.Save(txCtx)
			if err != nil {
				return fmt.Errorf("failed to create user: %w", err)
			}

			return attachUserToRegistrationProject(txCtx, txClient, newUser.ID, cfg)
		})
		if err != nil {
			return nil, err
		}

		return client.User.Query().
			Where(user.IDEQ(newUser.ID)).
			WithRoles().
			WithProjects().
			WithProjectUsers().
			WithOidcIdentities().
			Only(bypassCtx)
	})
}

func normalizeRegistrationConfig(cfg RegistrationConfig) RegistrationConfig {
	cfg.InviteCode = strings.TrimSpace(cfg.InviteCode)
	cfg.DefaultProjectScopes = normalizeStringList(cfg.DefaultProjectScopes)
	cfg.SelfServicePresetNames = normalizeStringList(cfg.SelfServicePresetNames)
	if cfg.DefaultProjectID < 0 {
		cfg.DefaultProjectID = 0
	}

	return cfg
}

func validateRegistrationConfig(cfg RegistrationConfig) error {
	for _, scope := range cfg.DefaultProjectScopes {
		if !scopes.IsValidScope(scope) {
			return fmt.Errorf("invalid default project scope: %s", scope)
		}
	}

	return nil
}

func normalizeStringList(values []string) []string {
	if len(values) == 0 {
		return []string{}
	}

	normalized := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		normalized = append(normalized, value)
	}

	return normalized
}

func attachUserToRegistrationProject(ctx context.Context, client *ent.Client, userID int, cfg RegistrationConfig) error {
	projectID, err := registrationProjectID(ctx, client, cfg)
	if err != nil {
		return err
	}
	if projectID == 0 {
		return nil
	}

	exists, err := client.UserProject.Query().
		Where(userproject.UserIDEQ(userID), userproject.ProjectIDEQ(projectID)).
		Exist(ctx)
	if err != nil {
		return fmt.Errorf("failed to check existing project membership: %w", err)
	}
	if exists {
		return nil
	}

	_, err = client.UserProject.Create().
		SetUserID(userID).
		SetProjectID(projectID).
		SetIsOwner(false).
		SetScopes(cfg.DefaultProjectScopes).
		Save(ctx)
	if err != nil {
		return fmt.Errorf("failed to attach user to default project: %w", err)
	}

	return nil
}

func registrationProjectID(ctx context.Context, client *ent.Client, cfg RegistrationConfig) (int, error) {
	if cfg.DefaultProjectID > 0 {
		exists, err := client.Project.Query().
			Where(project.IDEQ(cfg.DefaultProjectID), project.StatusEQ(project.StatusActive)).
			Exist(ctx)
		if err != nil {
			return 0, fmt.Errorf("failed to check default project: %w", err)
		}
		if !exists {
			return 0, fmt.Errorf("default registration project is not active")
		}

		return cfg.DefaultProjectID, nil
	}

	if !cfg.AutoJoinFirstProject {
		return 0, nil
	}

	proj, err := client.Project.Query().
		Where(project.StatusEQ(project.StatusActive)).
		Order(ent.Asc(project.FieldID)).
		First(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return 0, nil
		}

		return 0, fmt.Errorf("failed to find registration project: %w", err)
	}

	return proj.ID, nil
}
