package biz

import (
	"context"
	stdsql "database/sql"
	"encoding/json"
	"errors"
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
	ID                  int             `json:"id"`
	ProjectID           int             `json:"projectId"`
	Name                string          `json:"name"`
	Status              string          `json:"status"`
	Type                string          `json:"type"`
	CreatedAt           time.Time       `json:"createdAt"`
	UpdatedAt           time.Time       `json:"updatedAt"`
	ActiveProfile       string          `json:"activeProfile"`
	QuotaSummary        *MyQuotaSummary `json:"quotaSummary,omitempty"`
	Key                 string          `json:"key,omitempty"`
	ProvisioningSource  string          `json:"provisioningSource"`
	ProfileMode         string          `json:"profileMode"`
	AccessGroupID       *int            `json:"accessGroupId,omitempty"`
	AccessGroupRevision *int64          `json:"accessGroupRevision,omitempty"`
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
	ModelIDs     []string        `json:"modelIds,omitempty"`
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

type AdminAccessGroupInput struct {
	ProjectID            int
	Name                 *string
	Description          *string
	SelfServiceVisible   *bool
	ModelIDs             *[]string
	ChannelIDs           *[]int
	ChannelTags          *[]string
	ChannelTagsMatchMode *string
	LoadBalanceStrategy  *string
	ClearLoadBalance     bool
}

type MyUpdateAPIKeyInput struct {
	Name *string
}

type LegacyAPIKeyClassificationInput struct {
	Mode          string
	AccessGroupID *int
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

type MyRequestDetail struct {
	Request               *ent.Request                      `json:"request"`
	Executions            []*ent.RequestExecution           `json:"executions"`
	Usage                 []*ent.UsageLog                   `json:"usage"`
	Trace                 *ent.Trace                        `json:"trace,omitempty"`
	Thread                *ent.Thread                       `json:"thread,omitempty"`
	EvidenceAvailability  MyEvidenceAvailability            `json:"evidenceAvailability"`
	ExecutionAvailability []MyExecutionEvidenceAvailability `json:"executionAvailability"`
}

type MyEvidenceState struct {
	State  string `json:"state"`
	Source string `json:"source"`
}

type MyEvidenceAvailability struct {
	RequestHeaders MyEvidenceState `json:"requestHeaders"`
	RequestBody    MyEvidenceState `json:"requestBody"`
	ResponseBody   MyEvidenceState `json:"responseBody"`
	ResponseChunks MyEvidenceState `json:"responseChunks"`
}

type MyExecutionEvidenceAvailability struct {
	ExecutionID int                    `json:"executionId"`
	Evidence    MyEvidenceAvailability `json:"evidence"`
}

type MyAPIKeyReveal struct {
	Key        string    `json:"key"`
	RevealedAt time.Time `json:"revealedAt"`
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
				apikey.Or(
					apikey.TypeEQ(apikey.TypePersonal),
					apikey.And(apikey.TypeEQ(apikey.TypeUser), apikey.ProvisioningSourceEQ(apikey.ProvisioningSourceLegacyUnknown)),
				),
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
		templates, err := s.entFromContext(bypassCtx).APIKeyProfileTemplate.Query().
			Where(apikeyprofiletemplate.ProjectIDEQ(resolvedProjectID)).
			Order(ent.Asc(apikeyprofiletemplate.FieldName)).
			All(bypassCtx)
		if err != nil {
			return nil, fmt.Errorf("failed to list access groups: %w", err)
		}
		templates = lo.Filter(templates, func(tpl *ent.APIKeyProfileTemplate, _ int) bool {
			return s.accessGroupSelfServiceVisible(bypassCtx, tpl)
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
		generatedKey, err := GenerateAPIKey(s.keyPrefix)
		if err != nil {
			return MyAPIKeySummary{}, fmt.Errorf("failed to generate API key: %w", err)
		}

		var key *ent.APIKey
		err = s.RunInTransaction(bypassCtx, func(txCtx context.Context) error {
			client := s.entFromContext(txCtx)
			template, lockErr := lockAccessGroup(txCtx, client, input.PresetID)
			if lockErr != nil || template.ProjectID != resolvedProjectID {
				return fmt.Errorf("routing preset not found or not available")
			}
			if !template.SelfServiceVisible {
				return fmt.Errorf("routing preset is not available for self-service")
			}

			profiles, profileErr := materializeAccessGroupProfile(template.Name, template.Profile)
			if profileErr != nil {
				return profileErr
			}
			if lockErr = s.lockProjectForAPIKeyName(txCtx, resolvedProjectID); lockErr != nil {
				return lockErr
			}
			exists, existsErr := client.APIKey.Query().
				Where(apikey.ProjectIDEQ(resolvedProjectID), apikey.NameEQ(name)).
				Exist(txCtx)
			if existsErr != nil {
				return fmt.Errorf("failed to check API key name uniqueness: %w", existsErr)
			}
			if exists {
				return fmt.Errorf("API key name already exists")
			}

			key, lockErr = client.APIKey.Create().
				SetName(name).
				SetKey(generatedKey).
				SetUserID(currentUser.ID).
				SetProjectID(resolvedProjectID).
				SetType(apikey.TypePersonal).
				SetStatus(apikey.StatusEnabled).
				SetProvisioningSource(apikey.ProvisioningSourceSelfService).
				SetProfileMode(apikey.ProfileModeAccessGroup).
				SetAccessGroupID(template.ID).
				SetAccessGroupRevision(template.Revision).
				SetScopes([]string{string(scopes.ScopeReadChannels), string(scopes.ScopeWriteRequests)}).
				SetProfiles(profiles).
				Save(txCtx)
			if lockErr != nil {
				return fmt.Errorf("failed to create API key: %w", lockErr)
			}
			return nil
		})
		if err != nil {
			return MyAPIKeySummary{}, err
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

func (s *APIKeyService) RevealMyAPIKey(ctx context.Context, id int) (MyAPIKeyReveal, error) {
	if _, err := s.requireSelfServiceEnabled(ctx); err != nil {
		return MyAPIKeyReveal{}, err
	}
	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil {
		return MyAPIKeyReveal{}, ErrNotFoundOrNotAuthorized
	}
	return authz.RunWithSystemBypass(ctx, "self-api-key-reveal", func(bypassCtx context.Context) (MyAPIKeyReveal, error) {
		key, err := s.entFromContext(bypassCtx).APIKey.Query().Where(
			apikey.IDEQ(id),
			apikey.UserIDEQ(currentUser.ID),
			apikey.Or(
				apikey.TypeEQ(apikey.TypePersonal),
				apikey.And(apikey.TypeEQ(apikey.TypeUser), apikey.ProvisioningSourceEQ(apikey.ProvisioningSourceLegacyUnknown)),
			),
		).Only(bypassCtx)
		if err != nil {
			return MyAPIKeyReveal{}, ErrNotFoundOrNotAuthorized
		}
		if _, err := s.requireProjectMember(ctx, key.ProjectID); err != nil {
			return MyAPIKeyReveal{}, ErrNotFoundOrNotAuthorized
		}
		if key.Status == apikey.StatusArchived {
			return MyAPIKeyReveal{}, ErrAPIKeyArchived
		}
		return MyAPIKeyReveal{Key: key.Key, RevealedAt: time.Now().UTC()}, nil
	})
}

func (s *APIKeyService) ClassifyMyLegacyAPIKey(ctx context.Context, id int, input LegacyAPIKeyClassificationInput) (MyAPIKeySummary, error) {
	if _, err := s.requireSelfServiceEnabled(ctx); err != nil {
		return MyAPIKeySummary{}, err
	}
	return s.classifyLegacyAPIKey(ctx, id, input, true)
}

func (s *APIKeyService) ClassifyLegacyAPIKey(ctx context.Context, id int, input LegacyAPIKeyClassificationInput) (MyAPIKeySummary, error) {
	if !authz.HasScope(ctx, scopes.ScopeWriteAPIKeys) {
		return MyAPIKeySummary{}, ErrNotFoundOrNotAuthorized
	}
	return s.classifyLegacyAPIKey(ctx, id, input, false)
}

func (s *APIKeyService) classifyLegacyAPIKey(ctx context.Context, id int, input LegacyAPIKeyClassificationInput, selfOnly bool) (MyAPIKeySummary, error) {
	actor, ok := contexts.GetUser(ctx)
	if !ok || actor == nil {
		return MyAPIKeySummary{}, ErrNotFoundOrNotAuthorized
	}
	var result *ent.APIKey
	err := authz.RunWithSystemBypassVoid(ctx, "legacy-api-key-classification", func(bypassCtx context.Context) error {
		return s.RunInTransaction(bypassCtx, func(txCtx context.Context) error {
			client := s.entFromContext(txCtx)
			var group *ent.APIKeyProfileTemplate
			if input.Mode == "personal_access_group" {
				if input.AccessGroupID == nil {
					return fmt.Errorf("access group is required")
				}
				var groupErr error
				group, groupErr = lockAccessGroup(txCtx, client, *input.AccessGroupID)
				if groupErr != nil {
					return ErrNotFoundOrNotAuthorized
				}
			}

			key, err := client.APIKey.Get(txCtx, id)
			if err != nil {
				return ErrNotFoundOrNotAuthorized
			}
			projectID, hasProject := contexts.GetProjectID(ctx)
			if hasProject && projectID != key.ProjectID {
				return ErrNotFoundOrNotAuthorized
			}
			creator := key.UserID == actor.ID
			admin := authz.HasScope(ctx, scopes.ScopeWriteAPIKeys)
			if (selfOnly && !creator) || (!selfOnly && !admin) {
				return ErrNotFoundOrNotAuthorized
			}
			if key.ProvisioningSource != apikey.ProvisioningSourceLegacyUnknown {
				result = key
				return nil
			}
			update := client.APIKey.UpdateOneID(key.ID).SetClassificationAt(time.Now().UTC()).SetClassificationByUserID(actor.ID)
			switch input.Mode {
			case "admin":
				if selfOnly || !admin {
					return ErrNotFoundOrNotAuthorized
				}
				update.SetProvisioningSource(apikey.ProvisioningSourceAdmin).SetProfileMode(apikey.ProfileModeSnapshot).ClearAccessGroupID().ClearAccessGroupRevision()
			case "personal_snapshot":
				update.SetType(apikey.TypePersonal).SetProvisioningSource(apikey.ProvisioningSourceSelfService).SetProfileMode(apikey.ProfileModeSnapshot).ClearAccessGroupID().ClearAccessGroupRevision()
			case "personal_access_group":
				if group.ProjectID != key.ProjectID {
					return ErrNotFoundOrNotAuthorized
				}
				if selfOnly && !group.SelfServiceVisible {
					return ErrNotFoundOrNotAuthorized
				}
				profiles, profileErr := materializeAccessGroupProfile(group.Name, group.Profile)
				if profileErr != nil {
					return profileErr
				}
				update.SetType(apikey.TypePersonal).SetProvisioningSource(apikey.ProvisioningSourceSelfService).SetProfileMode(apikey.ProfileModeAccessGroup).SetAccessGroupID(group.ID).SetAccessGroupRevision(group.Revision).SetProfiles(profiles)
			default:
				return fmt.Errorf("invalid classification mode")
			}
			result, err = update.Save(txCtx)
			if err == nil {
				s.invalidateAPIKeyCaches(txCtx, key.Key)
			}
			return err
		})
	})
	if err != nil {
		return MyAPIKeySummary{}, err
	}
	return summarizeMyAPIKey(result, ""), nil
}

func (s *APIKeyService) DetachAPIKeyAccessGroup(ctx context.Context, id int) (MyAPIKeySummary, error) {
	if !authz.HasScope(ctx, scopes.ScopeWriteAPIKeys) {
		return MyAPIKeySummary{}, ErrNotFoundOrNotAuthorized
	}
	var result *ent.APIKey
	err := s.RunInTransaction(ctx, func(txCtx context.Context) error {
		client := s.entFromContext(txCtx)
		key, err := client.APIKey.Get(txCtx, id)
		if err != nil {
			return err
		}
		if key.ProfileMode == apikey.ProfileModeAccessGroup && key.AccessGroupID != nil {
			if _, err := lockAccessGroup(txCtx, client, *key.AccessGroupID); err != nil {
				return err
			}
		}
		result, err = client.APIKey.UpdateOneID(id).SetProfileMode(apikey.ProfileModeSnapshot).ClearAccessGroupID().ClearAccessGroupRevision().Save(txCtx)
		if err == nil {
			s.invalidateAPIKeyCaches(txCtx, key.Key)
		}
		return err
	})
	if err != nil {
		return MyAPIKeySummary{}, err
	}
	return summarizeMyAPIKey(result, ""), nil
}

func (s *APIKeyService) GetMyRequestDetail(ctx context.Context, id int) (MyRequestDetail, error) {
	policy, err := s.requireSelfServiceEnabled(ctx)
	if err != nil {
		if errors.Is(err, ErrSelfServiceDisabled) {
			return MyRequestDetail{}, ErrSelfServiceRequestDetailsDisabled
		}
		return MyRequestDetail{}, err
	}
	if !policy.AllowRequestDetails {
		return MyRequestDetail{}, ErrSelfServiceRequestDetailsDisabled
	}
	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil {
		return MyRequestDetail{}, ErrNotFoundOrNotAuthorized
	}

	return authz.RunWithSystemBypass(ctx, "self-request-detail", func(bypassCtx context.Context) (MyRequestDetail, error) {
		client := s.entFromContext(bypassCtx)
		req, err := client.Request.Query().Where(
			request.IDEQ(id),
			request.HasAPIKeyWith(apikey.UserIDEQ(currentUser.ID)),
		).Only(bypassCtx)
		if err != nil {
			return MyRequestDetail{}, ErrNotFoundOrNotAuthorized
		}
		if _, err := s.requireProjectMember(ctx, req.ProjectID); err != nil {
			return MyRequestDetail{}, ErrNotFoundOrNotAuthorized
		}
		execs, err := req.QueryExecutions().Order(ent.Asc("created_at"), ent.Asc("id")).All(bypassCtx)
		if err != nil {
			return MyRequestDetail{}, fmt.Errorf("failed to load request executions: %w", err)
		}
		usage, err := req.QueryUsageLogs().Order(ent.Asc("created_at"), ent.Asc("id")).All(bypassCtx)
		if err != nil {
			return MyRequestDetail{}, fmt.Errorf("failed to load request usage: %w", err)
		}
		detail := MyRequestDetail{Request: req, Executions: execs, Usage: usage}
		detail.EvidenceAvailability = s.hydrateRequestEvidence(bypassCtx, req)
		detail.ExecutionAvailability = make([]MyExecutionEvidenceAvailability, 0, len(execs))
		for _, execution := range execs {
			detail.ExecutionAvailability = append(detail.ExecutionAvailability, MyExecutionEvidenceAvailability{ExecutionID: execution.ID, Evidence: s.hydrateExecutionEvidence(bypassCtx, execution)})
		}
		if req.TraceID != 0 {
			trace, traceErr := client.Trace.Get(bypassCtx, req.TraceID)
			if traceErr == nil {
				detail.Trace = trace
				if trace.ThreadID != 0 {
					detail.Thread, _ = client.Thread.Get(bypassCtx, trace.ThreadID)
				}
			}
		}
		return detail, nil
	})
}

func (s *APIKeyService) hydrateRequestEvidence(ctx context.Context, row *ent.Request) MyEvidenceAvailability {
	availability := MyEvidenceAvailability{RequestHeaders: evidenceStateForHeaders(row.RequestHeaders)}
	requestBody, requestBodyState := s.resolveSelfEvidence(ctx, row.DataStorageID, row.RequestBody, dispositionField(row.EvidenceDisposition, "requestBody"))
	responseBody, responseBodyState := s.resolveSelfEvidence(ctx, row.DataStorageID, row.ResponseBody, dispositionField(row.EvidenceDisposition, "responseBody"))
	chunkBytes, responseChunksState := s.resolveSelfEvidence(ctx, row.DataStorageID, marshalEvidenceChunks(row.ResponseChunks), dispositionField(row.EvidenceDisposition, "responseChunks"))
	availability.RequestBody, availability.ResponseBody, availability.ResponseChunks = requestBodyState, responseBodyState, responseChunksState
	if availability.RequestBody.State == "available" {
		row.RequestBody = requestBody
	}
	if availability.ResponseBody.State == "available" {
		row.ResponseBody = responseBody
	}
	if availability.ResponseChunks.State == "available" {
		_ = json.Unmarshal(chunkBytes, &row.ResponseChunks)
	}
	return availability
}

func (s *APIKeyService) hydrateExecutionEvidence(ctx context.Context, row *ent.RequestExecution) MyEvidenceAvailability {
	availability := MyEvidenceAvailability{RequestHeaders: evidenceStateForHeaders(row.RequestHeaders)}
	requestBody, requestBodyState := s.resolveSelfEvidence(ctx, row.DataStorageID, row.RequestBody, dispositionField(row.EvidenceDisposition, "requestBody"))
	responseBody, responseBodyState := s.resolveSelfEvidence(ctx, row.DataStorageID, row.ResponseBody, dispositionField(row.EvidenceDisposition, "responseBody"))
	chunkBytes, responseChunksState := s.resolveSelfEvidence(ctx, row.DataStorageID, marshalEvidenceChunks(row.ResponseChunks), dispositionField(row.EvidenceDisposition, "responseChunks"))
	availability.RequestBody, availability.ResponseBody, availability.ResponseChunks = requestBodyState, responseBodyState, responseChunksState
	if availability.RequestBody.State == "available" {
		row.RequestBody = requestBody
	}
	if availability.ResponseBody.State == "available" {
		row.ResponseBody = responseBody
	}
	if availability.ResponseChunks.State == "available" {
		_ = json.Unmarshal(chunkBytes, &row.ResponseChunks)
	}
	return availability
}

func (s *APIKeyService) resolveSelfEvidence(ctx context.Context, storageID int, raw objects.JSONRawMessage, disposition *objects.Disposition) (objects.JSONRawMessage, MyEvidenceState) {
	if disposition == nil {
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "" || trimmed == "null" || trimmed == "{}" || trimmed == "[]" {
			return raw, MyEvidenceState{State: "legacyUnknown", Source: "unknown"}
		}
		return raw, MyEvidenceState{State: "available", Source: "database"}
	}
	if disposition.Intent == "omit" || disposition.Outcome == "omitted" {
		if disposition.Intent == "notApplicable" {
			return nil, MyEvidenceState{State: "notApplicable", Source: "none"}
		}
		return nil, MyEvidenceState{State: "notPersisted", Source: "none"}
	}
	if disposition.Location == "database" && disposition.Outcome == "stored" {
		if len(raw) == 0 || string(raw) == "null" {
			return nil, MyEvidenceState{State: "storageUnavailable", Source: "database"}
		}
		return raw, MyEvidenceState{State: "available", Source: "database"}
	}
	if disposition.Location != "external" || disposition.Outcome != "stored" || disposition.StorageKey == nil || s.DataStorageService == nil {
		return nil, MyEvidenceState{State: "storageUnavailable", Source: disposition.Location}
	}
	if disposition.StorageID != nil {
		storageID = *disposition.StorageID
	}
	if storageID <= 0 {
		return nil, MyEvidenceState{State: "storageUnavailable", Source: "external"}
	}
	storage, err := s.DataStorageService.GetDataStorageByID(ctx, storageID)
	if err != nil {
		return nil, MyEvidenceState{State: "storageUnavailable", Source: "external"}
	}
	readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	loaded, err := s.DataStorageService.LoadData(readCtx, storage, *disposition.StorageKey)
	if err != nil {
		return nil, MyEvidenceState{State: "storageUnavailable", Source: "external"}
	}
	return loaded, MyEvidenceState{State: "available", Source: "external"}
}

func dispositionField(disposition *objects.EvidenceDisposition, field string) *objects.Disposition {
	if disposition == nil {
		return nil
	}
	switch field {
	case "requestBody":
		return &disposition.RequestBody
	case "responseBody":
		return &disposition.ResponseBody
	default:
		return &disposition.ResponseChunks
	}
}

func marshalEvidenceChunks(chunks []objects.JSONRawMessage) objects.JSONRawMessage {
	raw, _ := json.Marshal(chunks)
	return raw
}

func evidenceStateForHeaders(raw objects.JSONRawMessage) MyEvidenceState {
	if len(raw) == 0 || string(raw) == "null" {
		return MyEvidenceState{State: "legacyUnknown", Source: "unknown"}
	}
	return MyEvidenceState{State: "available", Source: "database"}
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
			return s.accessGroupSelfServiceVisible(bypassCtx, tpl)
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
				request.HasAPIKeyWith(apikey.UserIDEQ(currentUser.ID)),
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
				usagelog.HasRequestWith(request.HasAPIKeyWith(apikey.UserIDEQ(currentUser.ID))),
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
		groups := make([]AdminAccessGroup, 0, len(templates))
		for _, tpl := range templates {
			group, err := s.summarizeAdminAccessGroup(bypassCtx, tpl)
			if err != nil {
				return nil, err
			}
			groups = append(groups, group)
		}

		return groups, nil
	})
}

func (s *APIKeyService) CreateAdminAccessGroup(ctx context.Context, input AdminAccessGroupInput) (AdminAccessGroup, error) {
	if input.ProjectID <= 0 {
		return AdminAccessGroup{}, fmt.Errorf("project is required")
	}
	if input.Name == nil {
		return AdminAccessGroup{}, fmt.Errorf("access group name is required")
	}

	name := strings.TrimSpace(*input.Name)
	if name == "" {
		return AdminAccessGroup{}, fmt.Errorf("access group name is required")
	}

	profile, err := buildAccessGroupProfile(input, nil, name)
	if err != nil {
		return AdminAccessGroup{}, err
	}

	description := ""
	if input.Description != nil {
		description = strings.TrimSpace(*input.Description)
	}

	visible := input.SelfServiceVisible != nil && *input.SelfServiceVisible
	var created *ent.APIKeyProfileTemplate
	err = s.RunInTransaction(ctx, func(txCtx context.Context) error {
		var saveErr error
		created, saveErr = s.entFromContext(txCtx).APIKeyProfileTemplate.Create().
			SetName(name).
			SetDescription(description).
			SetProjectID(input.ProjectID).
			SetProfile(profile).
			SetRevision(1).
			SetSelfServiceVisible(visible).
			Save(txCtx)
		if saveErr != nil {
			return saveErr
		}
		return createAccessGroupRevision(txCtx, s.entFromContext(txCtx), created)
	})
	if err != nil {
		return AdminAccessGroup{}, fmt.Errorf("failed to create access group: %w", err)
	}
	return s.GetAdminAccessGroup(ctx, created.ID)
}

func (s *APIKeyService) GetAdminAccessGroup(ctx context.Context, id int) (AdminAccessGroup, error) {
	template, err := s.entFromContext(ctx).APIKeyProfileTemplate.Get(ctx, id)
	if err != nil {
		return AdminAccessGroup{}, fmt.Errorf("failed to get access group: %w", err)
	}

	return authz.RunWithSystemBypass(ctx, "admin-access-group-summarize", func(bypassCtx context.Context) (AdminAccessGroup, error) {
		return s.summarizeAdminAccessGroup(bypassCtx, template)
	})
}

func (s *APIKeyService) UpdateAdminAccessGroup(ctx context.Context, id int, input AdminAccessGroupInput) (AdminAccessGroup, error) {
	var updated *ent.APIKeyProfileTemplate
	var affected []*ent.APIKey
	err := s.RunInTransaction(ctx, func(txCtx context.Context) error {
		var updateErr error
		updated, affected, updateErr = updateAccessGroupPolicy(txCtx, s.entFromContext(txCtx), id, func(group *ent.APIKeyProfileTemplate) (accessGroupPolicyPatch, error) {
			newName := group.Name
			if input.Name != nil {
				newName = strings.TrimSpace(*input.Name)
				if newName == "" {
					return accessGroupPolicyPatch{}, fmt.Errorf("access group name is required")
				}
			}
			profile, profileErr := buildAccessGroupProfile(input, group.Profile, newName)
			if profileErr != nil {
				return accessGroupPolicyPatch{}, profileErr
			}
			if input.Name != nil && (strings.TrimSpace(profile.Name) == "" || strings.EqualFold(strings.TrimSpace(profile.Name), group.Name)) {
				profile.Name = newName
			}
			return accessGroupPolicyPatch{Name: input.Name, Description: input.Description, Profile: profile, Visibility: input.SelfServiceVisible}, nil
		})
		return updateErr
	})
	if err != nil {
		return AdminAccessGroup{}, err
	}
	for _, key := range affected {
		s.invalidateAPIKeyCaches(ctx, key.Key)
	}
	return s.GetAdminAccessGroup(ctx, updated.ID)
}

func (s *APIKeyService) AddChannelsToAccessGroup(ctx context.Context, accessGroupID int, channelIDs []int) (AdminAccessGroup, error) {
	if accessGroupID <= 0 {
		return AdminAccessGroup{}, fmt.Errorf("access group is required")
	}

	uniqueChannelIDs := lo.Uniq(channelIDs)
	var affected []*ent.APIKey
	err := s.RunInTransaction(ctx, func(txCtx context.Context) error {
		var updateErr error
		client := s.entFromContext(txCtx)
		_, affected, updateErr = updateAccessGroupPolicy(txCtx, client, accessGroupID, func(group *ent.APIKeyProfileTemplate) (accessGroupPolicyPatch, error) {
			if len(uniqueChannelIDs) > 0 {
				channels, channelErr := client.Channel.Query().Where(channel.IDIn(uniqueChannelIDs...)).All(txCtx)
				if channelErr != nil {
					return accessGroupPolicyPatch{}, fmt.Errorf("failed to load channels: %w", channelErr)
				}
				if len(channels) != len(uniqueChannelIDs) {
					return accessGroupPolicyPatch{}, fmt.Errorf("one or more channels were not found")
				}
			}
			profile := group.Profile.Clone()
			if profile == nil {
				profile = &objects.APIKeyProfile{Name: group.Name}
			}
			if strings.TrimSpace(profile.Name) == "" {
				profile.Name = group.Name
			}
			profile.ChannelIDs = append([]int{}, uniqueChannelIDs...)
			profile.ChannelTags = []string{}
			profile.ChannelTagsMatchMode = objects.ChannelTagsMatchModeAny
			return accessGroupPolicyPatch{Profile: profile}, nil
		})
		return updateErr
	})
	if err != nil {
		return AdminAccessGroup{}, err
	}
	for _, key := range affected {
		s.invalidateAPIKeyCaches(ctx, key.Key)
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

func (s *APIKeyService) accessGroupSelfServiceVisible(ctx context.Context, tpl *ent.APIKeyProfileTemplate) bool {
	_ = ctx
	return tpl != nil && tpl.SelfServiceVisible
}

func (s *APIKeyService) getOwnedUserAPIKey(ctx context.Context, id int) (*ent.APIKey, error) {
	currentUser, ok := contexts.GetUser(ctx)
	if !ok || currentUser == nil {
		return nil, fmt.Errorf("user not found in context")
	}

	return authz.RunWithSystemBypass(ctx, "self-api-key-get", func(bypassCtx context.Context) (*ent.APIKey, error) {
		key, err := s.entFromContext(bypassCtx).APIKey.Query().
			Where(
				apikey.IDEQ(id),
				apikey.UserIDEQ(currentUser.ID),
				apikey.Or(
					apikey.TypeEQ(apikey.TypePersonal),
					apikey.And(apikey.TypeEQ(apikey.TypeUser), apikey.ProvisioningSourceEQ(apikey.ProvisioningSourceLegacyUnknown)),
				),
			).
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
		ID:                  key.ID,
		ProjectID:           key.ProjectID,
		Name:                key.Name,
		Status:              key.Status.String(),
		Type:                key.Type.String(),
		CreatedAt:           key.CreatedAt,
		UpdatedAt:           key.UpdatedAt,
		Key:                 revealedKey,
		ProvisioningSource:  key.ProvisioningSource.String(),
		ProfileMode:         key.ProfileMode.String(),
		AccessGroupID:       key.AccessGroupID,
		AccessGroupRevision: key.AccessGroupRevision,
	}
	if key.Profiles != nil {
		summary.ActiveProfile = key.Profiles.ActiveProfile
		summary.QuotaSummary = quotaSummaryForActiveProfile(key.Profiles)
	}
	return summary
}

func buildAccessGroupProfile(input AdminAccessGroupInput, existing *objects.APIKeyProfile, defaultName string) (*objects.APIKeyProfile, error) {
	profile := existing.Clone()
	if profile == nil {
		profile = &objects.APIKeyProfile{}
	}
	if strings.TrimSpace(profile.Name) == "" {
		profile.Name = defaultName
	}

	if input.ModelIDs != nil {
		profile.ModelIDs = normalizeStringList(*input.ModelIDs)
	}
	if input.ChannelIDs != nil {
		profile.ChannelIDs = lo.Uniq(append([]int{}, (*input.ChannelIDs)...))
	}
	if input.ChannelTags != nil {
		profile.ChannelTags = normalizeStringList(*input.ChannelTags)
	}
	if input.ChannelTagsMatchMode != nil {
		mode := objects.ChannelTagsMatchMode(strings.TrimSpace(*input.ChannelTagsMatchMode))
		if !mode.IsValid() {
			return nil, fmt.Errorf("channelTagsMatchMode is invalid")
		}
		profile.ChannelTagsMatchMode = mode
	}
	if input.ClearLoadBalance {
		profile.LoadBalanceStrategy = nil
	} else if input.LoadBalanceStrategy != nil {
		strategy := strings.TrimSpace(*input.LoadBalanceStrategy)
		if strategy == "" {
			profile.LoadBalanceStrategy = nil
		} else {
			profile.LoadBalanceStrategy = lo.ToPtr(strategy)
		}
	}

	profiles := []objects.APIKeyProfile{*profile}
	if err := validateProfileNames(profiles); err != nil {
		return nil, err
	}
	if err := validateProfileFilters(profiles); err != nil {
		return nil, err
	}
	if err := validateProfileQuota(profiles); err != nil {
		return nil, err
	}

	return profile, nil
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
		profile.ModelIDs = append([]string{}, tpl.Profile.ModelIDs...)
		previewSize := min(len(tpl.Profile.ModelIDs), 5)
		profile.ModelPreview = append([]string{}, tpl.Profile.ModelIDs[:previewSize]...)
	}
	profile.QuotaSummary = quotaSummary(tpl.Profile.Quota)

	return profile
}

func (s *APIKeyService) summarizeAdminAccessGroup(ctx context.Context, tpl *ent.APIKeyProfileTemplate) (AdminAccessGroup, error) {
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
		SelfServiceVisible: s.accessGroupSelfServiceVisible(ctx, tpl),
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
		assignment.Mode = "channel_ids"
		assignment.Reason = "no channels assigned"
		return assignment
	}

	if len(tpl.Profile.ChannelIDs) > 0 {
		assignment.ChannelIDs = append([]int{}, tpl.Profile.ChannelIDs...)
		if len(tpl.Profile.ChannelTags) == 0 {
			assignment.Mode = "channel_ids"
			assignment.Reason = "channels are selected directly in this access group"
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
