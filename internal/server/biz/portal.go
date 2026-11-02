package biz

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/samber/lo"
	"go.uber.org/fx"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/schema/schematype"
	"github.com/looplj/axonhub/internal/ent/userproject"
	"github.com/looplj/axonhub/internal/objects"
)

type PortalServiceParams struct {
	fx.In
	Ent    *ent.Client
	Keys   *APIKeyService
	Models *ModelService
	Quota  *QuotaService
	System *SystemService
	Auth   *AuthService
	Users  *UserService
}
type PortalService struct {
	*AbstractService
	keys   *APIKeyService
	models *ModelService
	quota  *QuotaService
	system *SystemService
	auth   *AuthService
	users  *UserService
}

func NewPortalService(p PortalServiceParams) *PortalService {
	return &PortalService{AbstractService: &AbstractService{db: p.Ent}, keys: p.Keys, models: p.Models, quota: p.Quota, system: p.System, auth: p.Auth, users: p.Users}
}
func (s *PortalService) Me(ctx context.Context) (*objects.PortalIdentity, error) {
	u, err := portalCurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	return &objects.PortalIdentity{ID: u.ID, Email: u.Email, FirstName: u.FirstName, LastName: u.LastName, PreferLanguage: u.PreferLanguage, AccessMode: string(u.AccessMode), Avatar: u.Avatar, HasPassword: u.Password != OIDC_ONLY_PLACEHOLDER}, nil
}
func (s *PortalService) Groups(ctx context.Context) ([]*objects.PortalGroup, error) {
	u, err := portalCurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	ctx = authz.WithSystemBypass(ctx, "portal-own-groups")
	client := s.entFromContext(ctx)
	groups, err := client.Project.Query().Where(project.StatusEQ(project.StatusActive), project.HasProjectUsersWith(userproject.UserIDEQ(u.ID))).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*objects.PortalGroup, 0, len(groups))
	seen := map[int]bool{}
	for _, g := range groups {
		seen[g.ID] = true
		available, err := s.Models(ctx, g.ID, nil, nil, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, &objects.PortalGroup{ID: g.ID, Name: g.Name, Member: true, Callable: len(available) > 0})
	}
	// Retained history remains discoverable after membership removal/archival.
	keys, err := s.ownKeys(ctx, nil).All(schematype.SkipSoftDelete(ctx))
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if seen[k.ProjectID] {
			continue
		}
		seen[k.ProjectID] = true
		g, err := client.Project.Get(schematype.SkipSoftDelete(ctx), k.ProjectID)
		if err != nil {
			if ent.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		out = append(out, &objects.PortalGroup{ID: g.ID, Name: g.Name, Member: false, Callable: false})
	}
	return out, nil
}
func (s *PortalService) group(ctx context.Context, id int) (*ent.Project, error) {
	u, err := portalCurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	ctx = authz.WithSystemBypass(ctx, "portal-current-member-group")
	return s.entFromContext(ctx).Project.Query().Where(project.IDEQ(id), project.StatusEQ(project.StatusActive), project.HasProjectUsersWith(userproject.UserIDEQ(u.ID))).Only(ctx)
}
func (s *PortalService) ownKeys(ctx context.Context, projectID *int) *ent.APIKeyQuery {
	u, _ := contexts.GetUser(ctx)
	q := s.entFromContext(ctx).APIKey.Query().Where(apikey.UserIDEQ(u.ID), apikey.TypeEQ(apikey.TypePersonal), apikey.PortalManagedEQ(true))
	if projectID != nil {
		q.Where(apikey.ProjectIDEQ(*projectID))
	}
	return q
}
func (s *PortalService) ownKey(ctx context.Context, id int, history bool) (*ent.APIKey, error) {
	if _, err := portalCurrentUser(ctx); err != nil {
		return nil, err
	}
	if history {
		ctx = schematype.SkipSoftDelete(ctx)
	}
	ctx = authz.WithSystemBypass(ctx, "portal-own-key")
	return s.ownKeys(ctx, nil).Where(apikey.IDEQ(id)).Only(ctx)
}
func (s *PortalService) keyDTO(ctx context.Context, k *ent.APIKey) (*objects.PortalKey, error) {
	models := []string{}
	if p := k.GetActiveProfile(); p != nil {
		models = append(models, p.ModelIDs...)
	}
	ips := append([]string{}, k.AllowedIps...)
	masked := "••••"
	if len(k.Key) > 8 {
		masked = k.Key[:3] + "••••" + k.Key[len(k.Key)-4:]
	}
	dto := &objects.PortalKey{ID: k.ID, ProjectID: k.ProjectID, Name: k.Name, Status: string(k.Status), ModelIDs: models, AllowedIPs: ips, CreatedAt: k.CreatedAt, MaskedKey: masked, Quotas: []*objects.PortalQuota{}}
	quotas, err := s.quota.ProfileQuotaUsages(ctx, k)
	if err != nil {
		return nil, err
	}
	for _, q := range quotas {
		row := &objects.PortalQuota{ProfileName: q.ProfileName, Active: k.Profiles.ActiveProfile == q.ProfileName, Requests: q.Quota.Requests, TotalTokens: q.Quota.TotalTokens, UsedRequests: q.Usage.RequestCount, UsedTokens: q.Usage.TotalTokens, UsedCost: q.Usage.TotalCost.String(), Start: q.Window.Start, End: q.Window.End}
		if q.Quota.Cost != nil {
			row.Cost = lo.ToPtr(q.Quota.Cost.String())
		}
		dto.Quotas = append(dto.Quotas, row)
	}
	return dto, nil
}
func (s *PortalService) Keys(ctx context.Context, projectID *int, first int, after *int) (*objects.PortalKeyPage, error) {
	if _, err := portalCurrentUser(ctx); err != nil {
		return nil, err
	}
	if first < 1 || first > 100 || (after != nil && *after < 1) {
		return nil, fmt.Errorf("invalid page bounds")
	}
	ctx = authz.WithSystemBypass(ctx, "portal-own-key-page")
	q := s.ownKeys(ctx, projectID)
	count, err := q.Clone().Count(ctx)
	if err != nil {
		return nil, err
	}
	if after != nil {
		q.Where(apikey.IDGT(*after))
	}
	rows, err := q.Order(ent.Asc(apikey.FieldID)).Limit(first + 1).All(ctx)
	if err != nil {
		return nil, err
	}
	page := &objects.PortalKeyPage{TotalCount: count, Items: []*objects.PortalKey{}}
	if len(rows) > first {
		rows = rows[:first]
		page.NextCursor = lo.ToPtr(rows[len(rows)-1].ID)
	}
	for _, k := range rows {
		dto, err := s.keyDTO(ctx, k)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, dto)
	}
	return page, nil
}
func validatePortalKeyInput(input objects.PortalKeyInput, g *ent.Project) ([]string, error) {
	if strings.TrimSpace(input.Name) == "" || len(input.Name) > 128 {
		return nil, fmt.Errorf("key name must contain 1–128 characters")
	}
	if len(input.ModelIDs) > 500 {
		return nil, fmt.Errorf("too many model restrictions")
	}
	p := g.GetActiveProfile()
	for _, id := range input.ModelIDs {
		if p == nil || p.ModelIDs == nil || !slices.Contains(*p.ModelIDs, id) {
			return nil, fmt.Errorf("model is outside current group authorization")
		}
	}
	return normalizeAllowedIPs(input.AllowedIPs)
}
func (s *PortalService) CreateKey(ctx context.Context, projectID int, input objects.PortalKeyInput) (*objects.PortalKey, error) {
	u, err := portalCurrentUser(ctx)
	if err != nil {
		return nil, err
	}
	if u.IsOwner {
		return nil, fmt.Errorf("owner cannot create portal-managed personal credentials")
	}
	if s.auth.AllowNoAuth {
		return nil, fmt.Errorf("disable anonymous API access before creating portal keys")
	}
	g, err := s.group(ctx, projectID)
	if err != nil {
		return nil, fmt.Errorf("group unavailable")
	}
	if err := ValidatePortalGroup(g); err != nil {
		return nil, err
	}
	ips, err := validatePortalKeyInput(input, g)
	if err != nil {
		return nil, err
	}
	available, err := s.Models(ctx, projectID, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	if !lo.SomeBy(available, func(m *objects.PortalModel) bool {
		return len(input.ModelIDs) == 0 || slices.Contains(input.ModelIDs, m.ID)
	}) {
		return nil, fmt.Errorf("group and key restrictions have no available model")
	}
	secret, err := GenerateAPIKey(s.keys.keyPrefix)
	if err != nil {
		return nil, err
	}
	var key *ent.APIKey
	ctx = authz.WithSystemBypass(ctx, "portal-create-personal-key")
	err = s.RunInTransaction(ctx, func(ctx context.Context) error {
		if err := s.keys.lockProjectForAPIKeyName(ctx, projectID); err != nil {
			return err
		}
		client := s.entFromContext(ctx)
		exists, err := client.APIKey.Query().Where(apikey.ProjectIDEQ(projectID), apikey.NameEQ(strings.TrimSpace(input.Name))).Exist(ctx)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("key name already exists in this group")
		}
		key, err = client.APIKey.Create().SetUserID(u.ID).SetProjectID(projectID).SetName(strings.TrimSpace(input.Name)).SetKey(secret).SetType(apikey.TypePersonal).SetPortalManaged(true).SetScopes([]string{"read_channels", "write_requests"}).SetAllowedIps(ips).SetProfiles(&objects.APIKeyProfiles{ActiveProfile: "personal", Profiles: []objects.APIKeyProfile{{Name: "personal", ModelIDs: input.ModelIDs}}}).Save(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s.keyDTO(ctx, key)
}
func (s *PortalService) UpdateKey(ctx context.Context, id int, input objects.PortalKeyInput) (*objects.PortalKey, error) {
	k, err := s.ownKey(ctx, id, false)
	if err != nil {
		return nil, fmt.Errorf("key unavailable")
	}
	g, err := s.group(ctx, k.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("group unavailable")
	}
	ips, err := validatePortalKeyInput(input, g)
	if err != nil {
		return nil, err
	}
	ctx = authz.WithSystemBypass(ctx, "portal-update-own-key")
	err = s.RunInTransaction(ctx, func(ctx context.Context) error {
		if err := s.keys.lockProjectForAPIKeyName(ctx, k.ProjectID); err != nil {
			return err
		}
		client := s.entFromContext(ctx)
		exists, err := client.APIKey.Query().Where(apikey.ProjectIDEQ(k.ProjectID), apikey.NameEQ(strings.TrimSpace(input.Name)), apikey.IDNEQ(id)).Exist(ctx)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("key name already exists in this group")
		}
		// Only the active profile's model restriction changes. Admin quota, mapping,
		// channels, other profiles and quota windows retain their original identities.
		profiles := k.Profiles.Clone()
		if profiles == nil || k.GetActiveProfile() == nil {
			profiles = &objects.APIKeyProfiles{ActiveProfile: "personal", Profiles: []objects.APIKeyProfile{{Name: "personal"}}}
		}
		for i := range profiles.Profiles {
			if profiles.Profiles[i].Name == profiles.ActiveProfile {
				profiles.Profiles[i].ModelIDs = append([]string{}, input.ModelIDs...)
			}
		}
		k, err = client.APIKey.UpdateOneID(id).SetName(strings.TrimSpace(input.Name)).SetAllowedIps(ips).SetProfiles(profiles).Save(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.keys.invalidateAPIKeyCaches(ctx, k.Key)
	return s.keyDTO(ctx, k)
}
func (s *PortalService) SetKeyStatus(ctx context.Context, id int, status string) (*objects.PortalKey, error) {
	k, err := s.ownKey(ctx, id, true)
	if err != nil {
		return nil, fmt.Errorf("key unavailable")
	}
	if status != "enabled" && status != "disabled" && status != "archived" {
		return nil, fmt.Errorf("invalid key status")
	}
	if status == "enabled" {
		if k.DeletedAt != 0 {
			return nil, fmt.Errorf("deleted key cannot be enabled")
		}
		g, err := s.group(ctx, k.ProjectID)
		if err != nil {
			return nil, fmt.Errorf("group unavailable")
		}
		if err := ValidatePortalGroup(g); err != nil {
			return nil, err
		}
	}
	ctx = authz.WithSystemBypass(schematype.SkipSoftDelete(ctx), "portal-key-status")
	k, err = s.entFromContext(ctx).APIKey.UpdateOneID(id).SetStatus(apikey.Status(status)).Save(ctx)
	if err != nil {
		return nil, err
	}
	s.keys.invalidateAPIKeyCaches(ctx, k.Key)
	return s.keyDTO(ctx, k)
}
func (s *PortalService) RevealKey(ctx context.Context, id int) (string, error) {
	k, err := s.ownKey(ctx, id, false)
	if err != nil {
		return "", fmt.Errorf("key unavailable")
	}
	if k.Status != apikey.StatusEnabled {
		return "", fmt.Errorf("key is not enabled")
	}
	g, err := s.group(ctx, k.ProjectID)
	if err != nil {
		return "", fmt.Errorf("group unavailable")
	}
	if err := ValidatePortalGroup(g); err != nil {
		return "", err
	}
	return k.Key, nil
}
func (s *PortalService) RotateKey(ctx context.Context, id int) (*objects.PortalKey, error) {
	k, err := s.ownKey(ctx, id, false)
	if err != nil {
		return nil, fmt.Errorf("key unavailable")
	}
	if _, err := s.group(ctx, k.ProjectID); err != nil {
		return nil, fmt.Errorf("group unavailable")
	}
	ctx = authz.WithSystemBypass(ctx, "portal-rotate-own-key")
	k, err = s.keys.RotateAPIKey(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.keyDTO(ctx, k)
}
