package biz

import (
	"context"
	stdsql "database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/apikeyprofiletemplate"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/ent/userproject"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
)

type MyAPIKeySummary struct {
	ID            int                     `json:"id"`
	Name          string                  `json:"name"`
	Status        string                  `json:"status"`
	Type          string                  `json:"type"`
	CreatedAt     time.Time               `json:"createdAt"`
	UpdatedAt     time.Time               `json:"updatedAt"`
	ActiveProfile string                  `json:"activeProfile"`
	Profiles      *objects.APIKeyProfiles `json:"profiles,omitempty"`
	Key           string                  `json:"key,omitempty"`
}

type MyRoutingPreset struct {
	ID          int                    `json:"id"`
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Profile     *objects.APIKeyProfile `json:"profile"`
}

type MyCreateAPIKeyInput struct {
	ProjectID int
	Name      string
	PresetID  int
}

type MyUpdateAPIKeyInput struct {
	Name *string
}

type MyModelSummary struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Developers []string `json:"developers,omitempty"`
	Groups     []string `json:"groups,omitempty"`
	PresetID   *int     `json:"presetId,omitempty"`
}

type MyRequestSummary struct {
	ID             int       `json:"id"`
	CreatedAt      time.Time `json:"createdAt"`
	ModelID        string    `json:"modelId"`
	APIKeyID       *int      `json:"apiKeyId,omitempty"`
	Status         string    `json:"status"`
	Source         string    `json:"source"`
	Format         string    `json:"format"`
	Stream         bool      `json:"stream"`
	LatencyMs      *int64    `json:"latencyMs,omitempty"`
	DetailsVisible bool      `json:"detailsVisible"`
}

type MyUsageSummary struct {
	Requests         int     `json:"requests"`
	PromptTokens     int64   `json:"promptTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	TotalTokens      int64   `json:"totalTokens"`
	TotalCost        float64 `json:"totalCost"`
}

type myUsageAgg struct {
	Requests         int                `json:"requests"`
	PromptTokens     stdsql.NullInt64   `json:"prompt_tokens"`
	CompletionTokens stdsql.NullInt64   `json:"completion_tokens"`
	TotalTokens      stdsql.NullInt64   `json:"total_tokens"`
	TotalCost        stdsql.NullFloat64 `json:"total_cost"`
}

func (s *APIKeyService) ListMyAPIKeys(ctx context.Context, projectID int) ([]MyAPIKeySummary, error) {
	currentUser, err := s.requireProjectMember(ctx, projectID)
	if err != nil {
		return nil, err
	}

	return authz.RunWithSystemBypass(ctx, "self-api-keys-list", func(bypassCtx context.Context) ([]MyAPIKeySummary, error) {
		keys, err := s.entFromContext(bypassCtx).APIKey.Query().
			Where(
				apikey.ProjectIDEQ(projectID),
				apikey.UserIDEQ(currentUser.ID),
				apikey.TypeEQ(apikey.TypeUser),
				apikey.StatusNEQ(apikey.StatusArchived),
			).
			Order(ent.Desc(apikey.FieldCreatedAt)).
			All(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to list API keys: %w", err)
		}

		return lo.Map(keys, func(key *ent.APIKey, _ int) MyAPIKeySummary {
			return summarizeMyAPIKey(key, "")
		}), nil
	})
}

func (s *APIKeyService) ListMyRoutingPresets(ctx context.Context, projectID int) ([]MyRoutingPreset, error) {
	if _, err := s.requireProjectMember(ctx, projectID); err != nil {
		return nil, err
	}

	return authz.RunWithSystemBypass(ctx, "self-routing-presets-list", func(bypassCtx context.Context) ([]MyRoutingPreset, error) {
		policy, err := s.registrationPolicy(bypassCtx)
		if err != nil {
			return nil, err
		}

		templates, err := s.entFromContext(bypassCtx).APIKeyProfileTemplate.Query().
			Where(apikeyprofiletemplate.ProjectIDEQ(projectID)).
			Order(ent.Asc(apikeyprofiletemplate.FieldName)).
			All(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to list routing presets: %w", err)
		}

		templates = lo.Filter(templates, func(tpl *ent.APIKeyProfileTemplate, _ int) bool {
			return selfServicePresetAllowed(policy, tpl.Name)
		})

		return lo.Map(templates, func(tpl *ent.APIKeyProfileTemplate, _ int) MyRoutingPreset {
			return MyRoutingPreset{
				ID:          tpl.ID,
				Name:        tpl.Name,
				Description: tpl.Description,
				Profile:     tpl.Profile,
			}
		}), nil
	})
}

func (s *APIKeyService) CreateMyAPIKey(ctx context.Context, input MyCreateAPIKeyInput) (MyAPIKeySummary, error) {
	currentUser, err := s.requireProjectMember(ctx, input.ProjectID)
	if err != nil {
		return MyAPIKeySummary{}, err
	}

	name := strings.TrimSpace(input.Name)
	if name == "" {
		return MyAPIKeySummary{}, ErrAPIKeyNameRequired
	}
	if input.PresetID <= 0 {
		return MyAPIKeySummary{}, fmt.Errorf("routing preset is required")
	}

	return authz.RunWithSystemBypass(ctx, "self-api-key-create", func(bypassCtx context.Context) (MyAPIKeySummary, error) {
		policy, err := s.registrationPolicy(bypassCtx)
		if err != nil {
			return MyAPIKeySummary{}, err
		}

		client := s.entFromContext(bypassCtx)

		template, err := client.APIKeyProfileTemplate.Query().
			Where(
				apikeyprofiletemplate.IDEQ(input.PresetID),
				apikeyprofiletemplate.ProjectIDEQ(input.ProjectID),
			).
			Only(bypassCtx)
		if err != nil {
			return MyAPIKeySummary{}, fmt.Errorf("routing preset not found or not available: %w", err)
		}
		if !selfServicePresetAllowed(policy, template.Name) {
			return MyAPIKeySummary{}, fmt.Errorf("routing preset is not available for self-service")
		}

		profile := template.Profile.Clone()
		if profile == nil {
			profile = &objects.APIKeyProfile{}
		}
		if strings.TrimSpace(profile.Name) == "" {
			profile.Name = template.Name
		}

		profiles := objects.APIKeyProfiles{
			ActiveProfile: profile.Name,
			Profiles:      []objects.APIKeyProfile{*profile},
		}

		if err := validateProfileNames(profiles.Profiles); err != nil {
			return MyAPIKeySummary{}, err
		}
		if err := validateActiveProfile(profiles.ActiveProfile, profiles.Profiles); err != nil {
			return MyAPIKeySummary{}, err
		}
		if err := validateProfileFilters(profiles.Profiles); err != nil {
			return MyAPIKeySummary{}, err
		}
		if err := validateProfileQuota(profiles.Profiles); err != nil {
			return MyAPIKeySummary{}, err
		}

		exists, err := client.APIKey.Query().
			Where(apikey.ProjectIDEQ(input.ProjectID), apikey.UserIDEQ(currentUser.ID), apikey.NameEQ(name)).
			Exist(bypassCtx)
		if err != nil {
			return MyAPIKeySummary{}, fmt.Errorf("failed to check API key name uniqueness: %w", err)
		}
		if exists {
			return MyAPIKeySummary{}, fmt.Errorf("API key name already exists")
		}

		generatedKey, err := GenerateAPIKey(s.keyPrefix)
		if err != nil {
			return MyAPIKeySummary{}, fmt.Errorf("failed to generate API key: %w", err)
		}

		key, err := client.APIKey.Create().
			SetName(name).
			SetKey(generatedKey).
			SetUserID(currentUser.ID).
			SetProjectID(input.ProjectID).
			SetType(apikey.TypeUser).
			SetStatus(apikey.StatusEnabled).
			SetScopes([]string{string(scopes.ScopeReadChannels), string(scopes.ScopeWriteRequests)}).
			SetProfiles(&profiles).
			Save(bypassCtx)
		if err != nil {
			return MyAPIKeySummary{}, fmt.Errorf("failed to create API key: %w", err)
		}

		s.invalidateAPIKeyCaches(bypassCtx, key.Key)
		return summarizeMyAPIKey(key, key.Key), nil
	})
}

func (s *APIKeyService) UpdateMyAPIKey(ctx context.Context, id int, input MyUpdateAPIKeyInput) (MyAPIKeySummary, error) {
	key, err := s.getOwnedUserAPIKey(ctx, id)
	if err != nil {
		return MyAPIKeySummary{}, err
	}

	if input.Name == nil {
		return summarizeMyAPIKey(key, ""), nil
	}

	name := strings.TrimSpace(*input.Name)
	if name == "" {
		return MyAPIKeySummary{}, ErrAPIKeyNameRequired
	}

	return authz.RunWithSystemBypass(ctx, "self-api-key-update", func(bypassCtx context.Context) (MyAPIKeySummary, error) {
		updated, err := s.entFromContext(bypassCtx).APIKey.UpdateOneID(key.ID).SetName(name).Save(bypassCtx)
		if err != nil {
			return MyAPIKeySummary{}, fmt.Errorf("failed to update API key: %w", err)
		}
		s.invalidateAPIKeyCaches(bypassCtx, updated.Key)
		return summarizeMyAPIKey(updated, ""), nil
	})
}

func (s *APIKeyService) UpdateMyAPIKeyStatus(ctx context.Context, id int, status apikey.Status) (MyAPIKeySummary, error) {
	key, err := s.getOwnedUserAPIKey(ctx, id)
	if err != nil {
		return MyAPIKeySummary{}, err
	}
	if status != apikey.StatusEnabled && status != apikey.StatusDisabled && status != apikey.StatusArchived {
		return MyAPIKeySummary{}, fmt.Errorf("unsupported API key status")
	}

	return authz.RunWithSystemBypass(ctx, "self-api-key-status", func(bypassCtx context.Context) (MyAPIKeySummary, error) {
		updated, err := s.entFromContext(bypassCtx).APIKey.UpdateOneID(key.ID).SetStatus(status).Save(bypassCtx)
		if err != nil {
			return MyAPIKeySummary{}, fmt.Errorf("failed to update API key status: %w", err)
		}
		s.invalidateAPIKeyCaches(bypassCtx, updated.Key)
		return summarizeMyAPIKey(updated, ""), nil
	})
}

func (s *APIKeyService) RotateMyAPIKey(ctx context.Context, id int) (MyAPIKeySummary, error) {
	key, err := s.getOwnedUserAPIKey(ctx, id)
	if err != nil {
		return MyAPIKeySummary{}, err
	}

	return authz.RunWithSystemBypass(ctx, "self-api-key-rotate", func(bypassCtx context.Context) (MyAPIKeySummary, error) {
		newKey, err := GenerateAPIKey(s.keyPrefix)
		if err != nil {
			return MyAPIKeySummary{}, fmt.Errorf("failed to generate API key: %w", err)
		}

		updated, err := s.entFromContext(bypassCtx).APIKey.UpdateOneID(key.ID).SetKey(newKey).Save(bypassCtx)
		if err != nil {
			return MyAPIKeySummary{}, fmt.Errorf("failed to rotate API key: %w", err)
		}
		s.invalidateAPIKeyCaches(bypassCtx, key.Key, newKey)
		return summarizeMyAPIKey(updated, newKey), nil
	})
}

func (s *APIKeyService) ListMyModels(ctx context.Context, projectID int, presetID *int) ([]MyModelSummary, error) {
	if _, err := s.requireProjectMember(ctx, projectID); err != nil {
		return nil, err
	}

	return authz.RunWithSystemBypass(ctx, "self-models-list", func(bypassCtx context.Context) ([]MyModelSummary, error) {
		policy, err := s.registrationPolicy(bypassCtx)
		if err != nil {
			return nil, err
		}

		client := s.entFromContext(bypassCtx)

		proj, err := client.Project.Query().Where(project.IDEQ(projectID)).Only(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to load project: %w", err)
		}

		templateQuery := client.APIKeyProfileTemplate.Query().
			Where(apikeyprofiletemplate.ProjectIDEQ(projectID)).
			Order(ent.Asc(apikeyprofiletemplate.FieldName))
		if presetID != nil {
			templateQuery = templateQuery.Where(apikeyprofiletemplate.IDEQ(*presetID))
		}

		templates, err := templateQuery.All(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to list routing presets: %w", err)
		}

		templates = lo.Filter(templates, func(tpl *ent.APIKeyProfileTemplate, _ int) bool {
			return selfServicePresetAllowed(policy, tpl.Name)
		})
		if presetID != nil && len(templates) == 0 {
			return nil, fmt.Errorf("routing preset not found or not available")
		}
		if len(templates) == 0 || s.ModelService == nil {
			return []MyModelSummary{}, nil
		}

		allowedModels := map[string]MyModelSummary{}
		for _, template := range templates {
			profile := template.Profile.Clone()
			if profile == nil {
				profile = &objects.APIKeyProfile{}
			}
			if strings.TrimSpace(profile.Name) == "" {
				profile.Name = template.Name
			}

			profiles := &objects.APIKeyProfiles{
				ActiveProfile: profile.Name,
				Profiles:      []objects.APIKeyProfile{*profile},
			}
			syntheticKey := &ent.APIKey{
				ProjectID: projectID,
				Type:      apikey.TypeUser,
				Status:    apikey.StatusEnabled,
				Profiles:  profiles,
			}
			syntheticKey.Edges.Project = proj

			modelCtx := contexts.WithAPIKey(bypassCtx, syntheticKey)
			facades, err := s.ModelService.ListEnabledModels(modelCtx)
			if err != nil {
				return nil, fmt.Errorf("failed to list models for routing preset %q: %w", template.Name, err)
			}

			for _, facade := range facades {
				if facade.ID == "" {
					continue
				}

				entry := allowedModels[facade.ID]
				entry.ID = facade.ID
				if strings.TrimSpace(facade.DisplayName) != "" {
					entry.Name = facade.DisplayName
				} else if entry.Name == "" {
					entry.Name = facade.ID
				}
				if facade.OwnedBy != "" && !lo.Contains(entry.Developers, facade.OwnedBy) {
					entry.Developers = append(entry.Developers, facade.OwnedBy)
				}
				if !lo.Contains(entry.Groups, template.Name) {
					entry.Groups = append(entry.Groups, template.Name)
				}
				if entry.PresetID == nil {
					entry.PresetID = lo.ToPtr(template.ID)
				}
				allowedModels[facade.ID] = entry
			}
		}

		models := lo.Values(allowedModels)
		sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })

		return models, nil
	})
}

func (s *APIKeyService) ListMyRequests(ctx context.Context, projectID int, limit int) ([]MyRequestSummary, error) {
	currentUser, err := s.requireProjectMember(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	return authz.RunWithSystemBypass(ctx, "self-requests-list", func(bypassCtx context.Context) ([]MyRequestSummary, error) {
		rows, err := s.entFromContext(bypassCtx).Request.Query().
			Where(
				request.ProjectIDEQ(projectID),
				request.HasAPIKeyWith(apikey.UserIDEQ(currentUser.ID), apikey.TypeEQ(apikey.TypeUser)),
			).
			Order(ent.Desc(request.FieldCreatedAt)).
			Limit(limit).
			All(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to list requests: %w", err)
		}

		policy, err := s.registrationPolicy(bypassCtx)
		if err != nil {
			return nil, err
		}

		detailsVisible := policy.AllowRequestDetails
		return lo.Map(rows, func(row *ent.Request, _ int) MyRequestSummary {
			return MyRequestSummary{
				ID:             row.ID,
				CreatedAt:      row.CreatedAt,
				ModelID:        row.ModelID,
				APIKeyID:       lo.ToPtr(row.APIKeyID),
				Status:         row.Status.String(),
				Source:         row.Source.String(),
				Format:         row.Format,
				Stream:         row.Stream,
				LatencyMs:      row.MetricsLatencyMs,
				DetailsVisible: detailsVisible,
			}
		}), nil
	})
}

func (s *APIKeyService) MyUsage(ctx context.Context, projectID int) (MyUsageSummary, error) {
	currentUser, err := s.requireProjectMember(ctx, projectID)
	if err != nil {
		return MyUsageSummary{}, err
	}

	return authz.RunWithSystemBypass(ctx, "self-usage", func(bypassCtx context.Context) (MyUsageSummary, error) {
		var rows []myUsageAgg
		err := s.entFromContext(bypassCtx).UsageLog.Query().
			Where(
				usagelog.ProjectIDEQ(projectID),
				usagelog.HasRequestWith(request.HasAPIKeyWith(apikey.UserIDEQ(currentUser.ID), apikey.TypeEQ(apikey.TypeUser))),
			).
			Modify(func(selector *entsql.Selector) {
				selector.Select(
					entsql.As(entsql.Count(selector.C(usagelog.FieldID)), "requests"),
					entsql.As(entsql.Sum(selector.C(usagelog.FieldPromptTokens)), "prompt_tokens"),
					entsql.As(entsql.Sum(selector.C(usagelog.FieldCompletionTokens)), "completion_tokens"),
					entsql.As(entsql.Sum(selector.C(usagelog.FieldTotalTokens)), "total_tokens"),
					entsql.As(entsql.Sum(selector.C(usagelog.FieldTotalCost)), "total_cost"),
				)
			}).
			Scan(bypassCtx, &rows)
		if err != nil {
			return MyUsageSummary{}, fmt.Errorf("failed to aggregate usage: %w", err)
		}

		if len(rows) == 0 {
			return MyUsageSummary{}, nil
		}

		out := MyUsageSummary{Requests: rows[0].Requests}
		if rows[0].PromptTokens.Valid {
			out.PromptTokens = rows[0].PromptTokens.Int64
		}
		if rows[0].CompletionTokens.Valid {
			out.CompletionTokens = rows[0].CompletionTokens.Int64
		}
		if rows[0].TotalTokens.Valid {
			out.TotalTokens = rows[0].TotalTokens.Int64
		}
		if rows[0].TotalCost.Valid {
			out.TotalCost = rows[0].TotalCost.Float64
		}

		return out, nil
	})
}

func (s *APIKeyService) registrationPolicy(ctx context.Context) (RegistrationConfig, error) {
	if s.SystemService == nil {
		return normalizeRegistrationConfig(s.Registration), nil
	}

	return s.SystemService.RegistrationConfig(ctx, s.Registration)
}

func selfServicePresetAllowed(policy RegistrationConfig, name string) bool {
	allowedNames := policy.SelfServicePresetNames
	if len(allowedNames) == 0 {
		return false
	}

	normalizedName := strings.ToLower(strings.TrimSpace(name))
	for _, allowed := range allowedNames {
		normalizedAllowed := strings.ToLower(strings.TrimSpace(allowed))
		if normalizedAllowed == "*" || normalizedAllowed == normalizedName {
			return true
		}
	}

	return false
}

func (s *APIKeyService) getOwnedUserAPIKey(ctx context.Context, id int) (*ent.APIKey, error) {
	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil {
		return nil, fmt.Errorf("user not found in context")
	}

	return authz.RunWithSystemBypass(ctx, "self-api-key-get", func(bypassCtx context.Context) (*ent.APIKey, error) {
		key, err := s.entFromContext(bypassCtx).APIKey.Query().
			Where(apikey.IDEQ(id), apikey.UserIDEQ(currentUser.ID), apikey.TypeEQ(apikey.TypeUser)).
			Only(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("API key not found or not owned by current user: %w", err)
		}
		if _, err := s.requireProjectMember(ctx, key.ProjectID); err != nil {
			return nil, err
		}
		return key, nil
	})
}

func (s *APIKeyService) requireProjectMember(ctx context.Context, projectID int) (*ent.User, error) {
	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil {
		return nil, fmt.Errorf("user not found in context")
	}
	if projectID <= 0 {
		return nil, fmt.Errorf("project is required")
	}

	_, err := authz.RunWithSystemBypass(ctx, "self-project-membership", func(bypassCtx context.Context) (*ent.UserProject, error) {
		return s.entFromContext(bypassCtx).UserProject.Query().
			Where(userproject.UserIDEQ(currentUser.ID), userproject.ProjectIDEQ(projectID)).
			Only(bypassCtx)
	})
	if err != nil {
		return nil, fmt.Errorf("project is not available to current user: %w", err)
	}

	return currentUser, nil
}

func summarizeMyAPIKey(key *ent.APIKey, revealedKey string) MyAPIKeySummary {
	summary := MyAPIKeySummary{
		ID:        key.ID,
		Name:      key.Name,
		Status:    key.Status.String(),
		Type:      key.Type.String(),
		CreatedAt: key.CreatedAt,
		UpdatedAt: key.UpdatedAt,
		Profiles:  key.Profiles,
		Key:       revealedKey,
	}
	if key.Profiles != nil {
		summary.ActiveProfile = key.Profiles.ActiveProfile
	}
	return summary
}
