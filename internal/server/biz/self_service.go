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
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/usagelog"
	"github.com/looplj/axonhub/internal/ent/userproject"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
)

type MyAPIKeySummary struct {
	ID            int             `json:"id"`
	ProjectID     int             `json:"projectId"`
	Name          string          `json:"name"`
	Status        string          `json:"status"`
	Type          string          `json:"type"`
	CreatedAt     time.Time       `json:"createdAt"`
	UpdatedAt     time.Time       `json:"updatedAt"`
	ActiveProfile string          `json:"activeProfile"`
	QuotaSummary  *MyQuotaSummary `json:"quotaSummary,omitempty"`
	Key           string          `json:"key,omitempty"`
}

type MyRoutingPreset struct {
	ID           int             `json:"id"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Enabled      bool            `json:"enabled"`
	ProfileLabel string          `json:"profileLabel,omitempty"`
	ModelCount   int             `json:"modelCount,omitempty"`
	ModelPreview []string        `json:"modelPreview,omitempty"`
	QuotaSummary *MyQuotaSummary `json:"quotaSummary,omitempty"`
}

type SelfAccessGroup struct {
	ID          int                      `json:"id"`
	ProjectID   int                      `json:"projectId"`
	Name        string                   `json:"name"`
	Description string                   `json:"description,omitempty"`
	Enabled     bool                     `json:"enabled"`
	Profiles    []SelfAccessGroupProfile `json:"profiles"`
}

type SelfAccessGroupProfile struct {
	ID           int             `json:"id"`
	Name         string          `json:"name"`
	ModelCount   int             `json:"modelCount,omitempty"`
	ModelPreview []string        `json:"modelPreview,omitempty"`
	QuotaSummary *MyQuotaSummary `json:"quotaSummary,omitempty"`
}

type SelfModelAccessGroupRef struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	ProfileID int    `json:"profileId"`
}

type AdminAccessGroup struct {
	ID                 int                          `json:"id"`
	ProjectID          int                          `json:"projectId"`
	Name               string                       `json:"name"`
	Description        string                       `json:"description"`
	Enabled            bool                         `json:"enabled"`
	SelfServiceVisible bool                         `json:"selfServiceVisible"`
	Profiles           []SelfAccessGroupProfile     `json:"profiles"`
	ChannelAssignment  AccessGroupChannelAssignment `json:"channelAssignment"`
}

type AccessGroupChannelAssignment struct {
	Mode         string   `json:"mode"`
	Tags         []string `json:"tags,omitempty"`
	ChannelIDs   []int    `json:"channelIds,omitempty"`
	Assignable   bool     `json:"assignable"`
	ChannelCount int      `json:"channelCount"`
	Reason       string   `json:"reason,omitempty"`
}

type MyQuotaSummary struct {
	Requests    *int64  `json:"requests,omitempty"`
	TotalTokens *int64  `json:"totalTokens,omitempty"`
	Cost        *string `json:"cost,omitempty"`
	Period      string  `json:"period,omitempty"`
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
	ID            string                    `json:"id"`
	Name          string                    `json:"name"`
	Developers    []string                  `json:"developers,omitempty"`
	Groups        []string                  `json:"groups,omitempty"` // Deprecated compatibility field; use AccessGroups.
	PresetID      *int                      `json:"presetId,omitempty"`
	AccessGroupID *int                      `json:"accessGroupId,omitempty"`
	ProfileID     *int                      `json:"profileId,omitempty"`
	AccessGroups  []SelfModelAccessGroupRef `json:"accessGroups,omitempty"`
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
	if _, err := s.requireSelfServiceEnabled(ctx); err != nil {
		return nil, err
	}

	resolvedProjectID, err := s.resolveSelfProjectID(ctx, projectID)
	if err != nil {
		return nil, err
	}

	currentUser, err := s.requireProjectMember(ctx, resolvedProjectID)
	if err != nil {
		return nil, err
	}

	return authz.RunWithSystemBypass(ctx, "self-api-keys-list", func(bypassCtx context.Context) ([]MyAPIKeySummary, error) {
		keys, err := s.entFromContext(bypassCtx).APIKey.Query().
			Where(
				apikey.ProjectIDEQ(resolvedProjectID),
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
	groups, err := s.ListMyAccessGroups(ctx, projectID)
	if err != nil {
		return nil, err
	}

	return lo.FlatMap(groups, func(group SelfAccessGroup, _ int) []MyRoutingPreset {
		return lo.Map(group.Profiles, func(profile SelfAccessGroupProfile, _ int) MyRoutingPreset {
			return MyRoutingPreset{
				ID:           profile.ID,
				Name:         profile.Name,
				Description:  group.Description,
				Enabled:      group.Enabled,
				ProfileLabel: profile.Name,
				ModelCount:   profile.ModelCount,
				ModelPreview: profile.ModelPreview,
				QuotaSummary: profile.QuotaSummary,
			}
		})
	}), nil
}

func (s *APIKeyService) ListMyAccessGroups(ctx context.Context, projectID int) ([]SelfAccessGroup, error) {
	if _, err := s.requireSelfServiceEnabled(ctx); err != nil {
		return nil, err
	}

	resolvedProjectID, err := s.resolveSelfProjectID(ctx, projectID)
	if err != nil {
		return nil, err
	}

	if _, err := s.requireProjectMember(ctx, resolvedProjectID); err != nil {
		return nil, err
	}

	return authz.RunWithSystemBypass(ctx, "self-access-groups-list", func(bypassCtx context.Context) ([]SelfAccessGroup, error) {
		policy, err := s.registrationPolicy(bypassCtx)
		if err != nil {
			return nil, err
		}

		templates, err := s.entFromContext(bypassCtx).APIKeyProfileTemplate.Query().
			Where(apikeyprofiletemplate.ProjectIDEQ(resolvedProjectID)).
			Order(ent.Asc(apikeyprofiletemplate.FieldName)).
			All(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to list access groups: %w", err)
		}

		templates = lo.Filter(templates, func(tpl *ent.APIKeyProfileTemplate, _ int) bool {
			return selfServicePresetAllowed(policy, tpl.Name)
		})

		return lo.Map(templates, func(tpl *ent.APIKeyProfileTemplate, _ int) SelfAccessGroup {
			return summarizeSelfAccessGroup(tpl)
		}), nil
	})
}

func (s *APIKeyService) CreateMyAPIKey(ctx context.Context, input MyCreateAPIKeyInput) (MyAPIKeySummary, error) {
	if _, err := s.requireSelfServiceEnabled(ctx); err != nil {
		return MyAPIKeySummary{}, err
	}

	resolvedProjectID, err := s.resolveSelfProjectID(ctx, input.ProjectID)
	if err != nil {
		return MyAPIKeySummary{}, err
	}

	currentUser, err := s.requireProjectMember(ctx, resolvedProjectID)
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
				apikeyprofiletemplate.ProjectIDEQ(resolvedProjectID),
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
			Where(apikey.ProjectIDEQ(resolvedProjectID), apikey.NameEQ(name)).
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
			SetProjectID(resolvedProjectID).
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
	if _, err := s.requireSelfServiceEnabled(ctx); err != nil {
		return MyAPIKeySummary{}, err
	}

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
		exists, err := s.entFromContext(bypassCtx).APIKey.Query().
			Where(apikey.ProjectIDEQ(key.ProjectID), apikey.NameEQ(name), apikey.IDNEQ(key.ID)).
			Exist(bypassCtx)
		if err != nil {
			return MyAPIKeySummary{}, fmt.Errorf("failed to check API key name uniqueness: %w", err)
		}
		if exists {
			return MyAPIKeySummary{}, fmt.Errorf("API key name already exists")
		}

		updated, err := s.entFromContext(bypassCtx).APIKey.UpdateOneID(key.ID).SetName(name).Save(bypassCtx)
		if err != nil {
			return MyAPIKeySummary{}, fmt.Errorf("failed to update API key: %w", err)
		}
		s.invalidateAPIKeyCaches(bypassCtx, updated.Key)
		return summarizeMyAPIKey(updated, ""), nil
	})
}

func (s *APIKeyService) UpdateMyAPIKeyStatus(ctx context.Context, id int, status apikey.Status) (MyAPIKeySummary, error) {
	if _, err := s.requireSelfServiceEnabled(ctx); err != nil {
		return MyAPIKeySummary{}, err
	}

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
	if _, err := s.requireSelfServiceEnabled(ctx); err != nil {
		return MyAPIKeySummary{}, err
	}

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
	if _, err := s.requireSelfServiceEnabled(ctx); err != nil {
		return nil, err
	}

	resolvedProjectID, err := s.resolveSelfProjectID(ctx, projectID)
	if err != nil {
		return nil, err
	}

	if _, err := s.requireProjectMember(ctx, resolvedProjectID); err != nil {
		return nil, err
	}

	return authz.RunWithSystemBypass(ctx, "self-models-list", func(bypassCtx context.Context) ([]MyModelSummary, error) {
		policy, err := s.registrationPolicy(bypassCtx)
		if err != nil {
			return nil, err
		}

		client := s.entFromContext(bypassCtx)

		proj, err := client.Project.Query().Where(project.IDEQ(resolvedProjectID)).Only(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to load project: %w", err)
		}

		templateQuery := client.APIKeyProfileTemplate.Query().
			Where(apikeyprofiletemplate.ProjectIDEQ(resolvedProjectID)).
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
				ProjectID: resolvedProjectID,
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
				groupRef := SelfModelAccessGroupRef{ID: template.ID, Name: template.Name, ProfileID: template.ID}
				if !lo.ContainsBy(entry.AccessGroups, func(existing SelfModelAccessGroupRef) bool {
					return existing.ID == groupRef.ID && existing.ProfileID == groupRef.ProfileID
				}) {
					entry.AccessGroups = append(entry.AccessGroups, groupRef)
				}
				if entry.PresetID == nil {
					entry.PresetID = lo.ToPtr(template.ID)
				}
				if entry.AccessGroupID == nil {
					entry.AccessGroupID = lo.ToPtr(template.ID)
				}
				if entry.ProfileID == nil {
					entry.ProfileID = lo.ToPtr(template.ID)
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
	if _, err := s.requireSelfServiceEnabled(ctx); err != nil {
		return nil, err
	}

	resolvedProjectID, err := s.resolveSelfProjectID(ctx, projectID)
	if err != nil {
		return nil, err
	}

	currentUser, err := s.requireProjectMember(ctx, resolvedProjectID)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}

	return authz.RunWithSystemBypass(ctx, "self-requests-list", func(bypassCtx context.Context) ([]MyRequestSummary, error) {
		rows, err := s.entFromContext(bypassCtx).Request.Query().
			Where(
				request.ProjectIDEQ(resolvedProjectID),
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
	if _, err := s.requireSelfServiceEnabled(ctx); err != nil {
		return MyUsageSummary{}, err
	}

	resolvedProjectID, err := s.resolveSelfProjectID(ctx, projectID)
	if err != nil {
		return MyUsageSummary{}, err
	}

	currentUser, err := s.requireProjectMember(ctx, resolvedProjectID)
	if err != nil {
		return MyUsageSummary{}, err
	}

	return authz.RunWithSystemBypass(ctx, "self-usage", func(bypassCtx context.Context) (MyUsageSummary, error) {
		var rows []myUsageAgg
		err := s.entFromContext(bypassCtx).UsageLog.Query().
			Where(
				usagelog.ProjectIDEQ(resolvedProjectID),
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

func (s *APIKeyService) ListAdminAccessGroups(ctx context.Context, projectID int) ([]AdminAccessGroup, error) {
	if projectID <= 0 {
		return nil, fmt.Errorf("project is required")
	}

	templates, err := s.entFromContext(ctx).APIKeyProfileTemplate.Query().
		Where(apikeyprofiletemplate.ProjectIDEQ(projectID)).
		Order(ent.Asc(apikeyprofiletemplate.FieldName)).
		All(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to list access groups: %w", err)
	}

	return authz.RunWithSystemBypass(ctx, "admin-access-groups-summarize", func(bypassCtx context.Context) ([]AdminAccessGroup, error) {
		policy, err := s.registrationPolicy(bypassCtx)
		if err != nil {
			return nil, err
		}

		groups := make([]AdminAccessGroup, 0, len(templates))
		for _, tpl := range templates {
			group, err := s.summarizeAdminAccessGroup(bypassCtx, policy, tpl)
			if err != nil {
				return nil, err
			}
			groups = append(groups, group)
		}

		return groups, nil
	})
}

func (s *APIKeyService) GetAdminAccessGroup(ctx context.Context, id int) (AdminAccessGroup, error) {
	template, err := s.entFromContext(ctx).APIKeyProfileTemplate.Get(ctx, id)
	if err != nil {
		return AdminAccessGroup{}, fmt.Errorf("failed to get access group: %w", err)
	}

	return authz.RunWithSystemBypass(ctx, "admin-access-group-summarize", func(bypassCtx context.Context) (AdminAccessGroup, error) {
		policy, err := s.registrationPolicy(bypassCtx)
		if err != nil {
			return AdminAccessGroup{}, err
		}

		return s.summarizeAdminAccessGroup(bypassCtx, policy, template)
	})
}

func (s *APIKeyService) AddChannelsToAccessGroup(ctx context.Context, accessGroupID int, channelIDs []int) (AdminAccessGroup, error) {
	if accessGroupID <= 0 {
		return AdminAccessGroup{}, fmt.Errorf("access group is required")
	}
	if len(channelIDs) == 0 {
		return AdminAccessGroup{}, fmt.Errorf("at least one channel is required")
	}

	client := s.entFromContext(ctx)
	template, err := client.APIKeyProfileTemplate.Get(ctx, accessGroupID)
	if err != nil {
		return AdminAccessGroup{}, fmt.Errorf("failed to get access group: %w", err)
	}

	assignment := accessGroupChannelAssignment(template)
	if !assignment.Assignable {
		return AdminAccessGroup{}, fmt.Errorf("access group cannot be assigned to channels: %s", assignment.Reason)
	}

	profile := template.Profile.Clone()
	if profile == nil {
		profile = &objects.APIKeyProfile{Name: template.Name}
	}
	if strings.TrimSpace(profile.Name) == "" {
		profile.Name = template.Name
	}
	profileChanged := false

	if len(profile.ChannelTags) == 0 && len(profile.ChannelIDs) == 0 {
		profile.ChannelTags = append([]string{}, assignment.Tags...)
		profile.ChannelTagsMatchMode = objects.ChannelTagsMatchModeAny
		profileChanged = true
	}

	uniqueChannelIDs := lo.Uniq(channelIDs)
	if len(profile.ChannelIDs) > 0 {
		profile.ChannelIDs = lo.Uniq(append(append([]int{}, profile.ChannelIDs...), uniqueChannelIDs...))
		profileChanged = true
	}
	if profileChanged {
		template, err = client.APIKeyProfileTemplate.UpdateOneID(template.ID).SetProfile(profile).Save(ctx)
		if err != nil {
			return AdminAccessGroup{}, fmt.Errorf("failed to update access group assignment profile: %w", err)
		}
		assignment = accessGroupChannelAssignment(template)
	}

	channels, err := client.Channel.Query().Where(channel.IDIn(uniqueChannelIDs...)).All(ctx)
	if err != nil {
		return AdminAccessGroup{}, fmt.Errorf("failed to load channels: %w", err)
	}
	if len(channels) != len(uniqueChannelIDs) {
		return AdminAccessGroup{}, fmt.Errorf("one or more channels were not found")
	}

	if len(assignment.Tags) > 0 {
		for _, ch := range channels {
			tags := lo.Uniq(append(append([]string{}, ch.Tags...), assignment.Tags...))
			if _, err := client.Channel.UpdateOneID(ch.ID).SetTags(tags).Save(ctx); err != nil {
				return AdminAccessGroup{}, fmt.Errorf("failed to assign channel %d to access group: %w", ch.ID, err)
			}
		}
	}

	if s.ChannelService != nil {
		s.ChannelService.asyncReloadChannels()
	}

	return s.GetAdminAccessGroup(ctx, accessGroupID)
}

func (s *APIKeyService) ResolveAccessGroupProjectID(ctx context.Context, accessGroupID int) (int, error) {
	if accessGroupID <= 0 {
		return 0, fmt.Errorf("access group is required")
	}

	return authz.RunWithSystemBypass(ctx, "admin-access-group-project", func(bypassCtx context.Context) (int, error) {
		template, err := s.entFromContext(bypassCtx).APIKeyProfileTemplate.Get(bypassCtx, accessGroupID)
		if err != nil {
			return 0, fmt.Errorf("failed to get access group: %w", err)
		}

		return template.ProjectID, nil
	})
}

func (s *APIKeyService) registrationPolicy(ctx context.Context) (RegistrationConfig, error) {
	if s.SystemService == nil {
		return normalizeRegistrationConfig(s.Registration), nil
	}

	return s.SystemService.RegistrationConfig(ctx, s.Registration)
}

func (s *APIKeyService) requireSelfServiceEnabled(ctx context.Context) (RegistrationConfig, error) {
	policy, err := s.registrationPolicy(ctx)
	if err != nil {
		return RegistrationConfig{}, err
	}
	if !policy.SelfServiceEnabled {
		return RegistrationConfig{}, ErrSelfServiceDisabled
	}

	return policy, nil
}

func selfServicePresetAllowed(policy RegistrationConfig, name string) bool {
	if !policy.SelfServiceEnabled {
		return false
	}

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
		if key.Status == apikey.StatusArchived {
			return nil, fmt.Errorf("archived API keys cannot be managed through self-service")
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

	err := authz.RunWithSystemBypassVoid(ctx, "self-project-active", func(bypassCtx context.Context) error {
		exists, err := s.entFromContext(bypassCtx).Project.Query().
			Where(project.IDEQ(projectID), project.StatusEQ(project.StatusActive)).
			Exist(bypassCtx)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("project is not active")
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("project is not available to current user: %w", err)
	}

	if currentUser.IsOwner {
		return currentUser, nil
	}

	_, err = authz.RunWithSystemBypass(ctx, "self-project-membership", func(bypassCtx context.Context) (*ent.UserProject, error) {
		return s.entFromContext(bypassCtx).UserProject.Query().
			Where(
				userproject.UserIDEQ(currentUser.ID),
				userproject.ProjectIDEQ(projectID),
				userproject.HasProjectWith(project.StatusEQ(project.StatusActive)),
			).
			Only(bypassCtx)
	})
	if err != nil {
		return nil, fmt.Errorf("project is not available to current user: %w", err)
	}

	return currentUser, nil
}

func (s *APIKeyService) resolveSelfProjectID(ctx context.Context, projectID int) (int, error) {
	if projectID > 0 {
		return projectID, nil
	}

	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil {
		return 0, fmt.Errorf("user not found in context")
	}

	return authz.RunWithSystemBypass(ctx, "self-default-project", func(bypassCtx context.Context) (int, error) {
		membership, err := s.entFromContext(bypassCtx).UserProject.Query().
			Where(
				userproject.UserIDEQ(currentUser.ID),
				userproject.HasProjectWith(project.StatusEQ(project.StatusActive)),
			).
			Order(ent.Asc(userproject.FieldProjectID)).
			First(bypassCtx)
		if err == nil {
			return membership.ProjectID, nil
		}
		if !ent.IsNotFound(err) {
			return 0, fmt.Errorf("failed to resolve default workspace: %w", err)
		}

		if currentUser.IsOwner {
			proj, err := s.entFromContext(bypassCtx).Project.Query().
				Where(project.StatusEQ(project.StatusActive)).
				Order(ent.Asc(project.FieldID)).
				First(bypassCtx)
			if err != nil {
				if ent.IsNotFound(err) {
					return 0, fmt.Errorf("default workspace is not available")
				}
				return 0, fmt.Errorf("failed to resolve owner default workspace: %w", err)
			}
			return proj.ID, nil
		}

		return 0, fmt.Errorf("default workspace is not available")
	})
}

func summarizeMyAPIKey(key *ent.APIKey, revealedKey string) MyAPIKeySummary {
	summary := MyAPIKeySummary{
		ID:        key.ID,
		ProjectID: key.ProjectID,
		Name:      key.Name,
		Status:    key.Status.String(),
		Type:      key.Type.String(),
		CreatedAt: key.CreatedAt,
		UpdatedAt: key.UpdatedAt,
		Key:       revealedKey,
	}
	if key.Profiles != nil {
		summary.ActiveProfile = key.Profiles.ActiveProfile
		summary.QuotaSummary = quotaSummaryForActiveProfile(key.Profiles)
	}
	return summary
}

func summarizeSelfAccessGroup(tpl *ent.APIKeyProfileTemplate) SelfAccessGroup {
	return SelfAccessGroup{
		ID:          tpl.ID,
		ProjectID:   tpl.ProjectID,
		Name:        tpl.Name,
		Description: tpl.Description,
		Enabled:     true,
		Profiles:    []SelfAccessGroupProfile{summarizeSelfAccessGroupProfile(tpl)},
	}
}

func summarizeSelfAccessGroupProfile(tpl *ent.APIKeyProfileTemplate) SelfAccessGroupProfile {
	profile := SelfAccessGroupProfile{
		ID:   tpl.ID,
		Name: tpl.Name,
	}
	if tpl.Profile == nil {
		return profile
	}
	if strings.TrimSpace(tpl.Profile.Name) != "" {
		profile.Name = tpl.Profile.Name
	}
	profile.ModelCount = len(tpl.Profile.ModelIDs)
	if len(tpl.Profile.ModelIDs) > 0 {
		previewSize := min(len(tpl.Profile.ModelIDs), 5)
		profile.ModelPreview = append([]string{}, tpl.Profile.ModelIDs[:previewSize]...)
	}
	profile.QuotaSummary = quotaSummary(tpl.Profile.Quota)

	return profile
}

func (s *APIKeyService) summarizeAdminAccessGroup(ctx context.Context, policy RegistrationConfig, tpl *ent.APIKeyProfileTemplate) (AdminAccessGroup, error) {
	assignment := accessGroupChannelAssignment(tpl)
	channelCount, err := s.countAccessGroupChannels(ctx, assignment)
	if err != nil {
		return AdminAccessGroup{}, err
	}
	assignment.ChannelCount = channelCount

	return AdminAccessGroup{
		ID:                 tpl.ID,
		ProjectID:          tpl.ProjectID,
		Name:               tpl.Name,
		Description:        tpl.Description,
		Enabled:            true,
		SelfServiceVisible: selfServicePresetAllowed(policy, tpl.Name),
		Profiles:           []SelfAccessGroupProfile{summarizeSelfAccessGroupProfile(tpl)},
		ChannelAssignment:  assignment,
	}, nil
}

func (s *APIKeyService) countAccessGroupChannels(ctx context.Context, assignment AccessGroupChannelAssignment) (int, error) {
	if (len(assignment.Tags) == 0 && len(assignment.ChannelIDs) == 0) || !assignment.Assignable {
		return 0, nil
	}

	channels, err := s.entFromContext(ctx).Channel.Query().
		Where(channel.StatusNEQ(channel.StatusArchived)).
		All(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to count access group channels: %w", err)
	}

	count := 0
	channelIDSet := lo.SliceToMap(assignment.ChannelIDs, func(id int) (int, struct{}) {
		return id, struct{}{}
	})
	for _, ch := range channels {
		if len(channelIDSet) > 0 {
			if _, ok := channelIDSet[ch.ID]; !ok {
				continue
			}
		}
		if len(assignment.Tags) == 0 || objects.MatchChannelTags(assignment.Tags, objects.ChannelTagsMatchMode(assignment.Mode), ch.Tags) {
			count++
		}
	}

	return count, nil
}

func accessGroupChannelAssignment(tpl *ent.APIKeyProfileTemplate) AccessGroupChannelAssignment {
	assignment := AccessGroupChannelAssignment{
		Mode:       string(objects.ChannelTagsMatchModeAny),
		Assignable: true,
	}

	if tpl == nil {
		assignment.Assignable = false
		assignment.Reason = "access group is missing"
		return assignment
	}

	if tpl.Profile == nil || (len(tpl.Profile.ChannelTags) == 0 && len(tpl.Profile.ChannelIDs) == 0) {
		assignment.Tags = []string{generatedAccessGroupTag(tpl.ID)}
		assignment.Reason = "compatibility assignment uses a generated channel tag"
		return assignment
	}

	if len(tpl.Profile.ChannelIDs) > 0 {
		assignment.ChannelIDs = append([]int{}, tpl.Profile.ChannelIDs...)
		if len(tpl.Profile.ChannelTags) == 0 {
			assignment.Mode = "channel_ids"
			assignment.Reason = "compatibility assignment updates the access group's channel ID allow-list"
			return assignment
		}
	}

	mode := tpl.Profile.ChannelTagsMatchMode.OrDefault()
	assignment.Mode = string(mode)
	assignment.Tags = normalizeStringList(tpl.Profile.ChannelTags)
	if mode == objects.ChannelTagsMatchModeNone {
		assignment.Assignable = false
		assignment.Reason = "access group excludes channels by tag and cannot be assigned directly"
	}

	return assignment
}

func generatedAccessGroupTag(id int) string {
	return fmt.Sprintf("access-group:%d", id)
}

func summarizeMyRoutingPreset(tpl *ent.APIKeyProfileTemplate) MyRoutingPreset {
	summary := MyRoutingPreset{
		ID:          tpl.ID,
		Name:        tpl.Name,
		Description: tpl.Description,
		Enabled:     true,
	}
	if tpl.Profile == nil {
		return summary
	}

	summary.ProfileLabel = tpl.Profile.Name
	summary.ModelCount = len(tpl.Profile.ModelIDs)
	if len(tpl.Profile.ModelIDs) > 0 {
		previewSize := min(len(tpl.Profile.ModelIDs), 5)
		summary.ModelPreview = append([]string{}, tpl.Profile.ModelIDs[:previewSize]...)
	}
	summary.QuotaSummary = quotaSummary(tpl.Profile.Quota)

	return summary
}

func quotaSummaryForActiveProfile(profiles *objects.APIKeyProfiles) *MyQuotaSummary {
	if profiles == nil {
		return nil
	}

	for _, profile := range profiles.Profiles {
		if profile.Name == profiles.ActiveProfile {
			return quotaSummary(profile.Quota)
		}
	}

	return nil
}

func quotaSummary(quota *objects.APIKeyQuota) *MyQuotaSummary {
	if quota == nil {
		return nil
	}

	summary := &MyQuotaSummary{
		Requests:    quota.Requests,
		TotalTokens: quota.TotalTokens,
		Period:      string(quota.Period.Type),
	}
	if quota.Cost != nil {
		cost := quota.Cost.String()
		summary.Cost = &cost
	}

	return summary
}
