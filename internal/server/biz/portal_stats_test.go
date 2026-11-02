package biz

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
)

type portalStatsFixture struct {
	client *ent.Client
	ctx    context.Context
	portal *PortalService

	a, b, noKeys   *ent.User
	groupA, groupB *ent.Project
}

func newPortalStatsFixture(t *testing.T) *portalStatsFixture {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:portal-stats?mode=memory&_fk=0")
	ctx := authz.WithTestBypass(ent.NewContext(context.Background(), client))

	makeUser := func(email string) *ent.User {
		return client.User.Create().
			SetEmail(email).
			SetPassword("password").
			SetStatus(user.StatusActivated).
			SetAccessMode(user.AccessModePortal).
			SaveX(ctx)
	}
	userA := makeUser("portal-a@example.com")
	userB := makeUser("portal-b@example.com")
	noKeys := makeUser("portal-no-keys@example.com")

	groupA := client.Project.Create().SetName("Portal group A").SetStatus(project.StatusActive).SaveX(ctx)
	groupB := client.Project.Create().SetName("Portal group B").SetStatus(project.StatusActive).SaveX(ctx)
	client.UserProject.Create().SetUserID(userA.ID).SetProjectID(groupA.ID).SaveX(ctx)
	client.UserProject.Create().SetUserID(userB.ID).SetProjectID(groupA.ID).SaveX(ctx)
	client.UserProject.Create().SetUserID(userA.ID).SetProjectID(groupB.ID).SaveX(ctx)

	systemService := NewSystemService(SystemServiceParams{Ent: client})
	quotaService := NewQuotaService(client, systemService)
	portalService := NewPortalService(PortalServiceParams{
		Ent:    client,
		Quota:  quotaService,
		System: systemService,
	})

	return &portalStatsFixture{
		client: client,
		ctx:    ctx,
		portal: portalService,
		a:      userA,
		b:      userB,
		noKeys: noKeys,
		groupA: groupA,
		groupB: groupB,
	}
}

func (f *portalStatsFixture) userContext(u *ent.User) context.Context {
	return contexts.WithUser(f.ctx, u)
}

func createPortalStatsKey(t *testing.T, f *portalStatsFixture, owner *ent.User, group *ent.Project, name string, keyType apikey.Type, portalManaged bool, status apikey.Status, profiles *objects.APIKeyProfiles) *ent.APIKey {
	t.Helper()
	if profiles == nil {
		profiles = &objects.APIKeyProfiles{}
	}

	return f.client.APIKey.Create().
		SetKey("sk-" + name).
		SetName(name).
		SetType(keyType).
		SetPortalManaged(portalManaged).
		SetStatus(status).
		SetScopes([]string{"read_channels", "write_requests"}).
		SetProfiles(profiles).
		SetUserID(owner.ID).
		SetProjectID(group.ID).
		SaveX(f.ctx)
}

func createPortalStatsRequest(t *testing.T, f *portalStatsFixture, key *ent.APIKey, modelID string, status request.Status, createdAt time.Time) *ent.Request {
	t.Helper()

	return f.client.Request.Create().
		SetAPIKeyID(key.ID).
		SetProjectID(key.ProjectID).
		SetModelID(modelID).
		SetFormat("openai/chat_completions").
		SetStatus(status).
		SetRequestBody(objects.JSONRawMessage([]byte(`{}`))).
		SetCreatedAt(createdAt).
		SaveX(f.ctx)
}

func createPortalStatsUsage(t *testing.T, f *portalStatsFixture, key *ent.APIKey, requestID int, modelID string, promptTokens, completionTokens, cachedTokens, totalTokens int64, createdAt time.Time, cost *float64) *ent.UsageLog {
	t.Helper()

	create := f.client.UsageLog.Create().
		SetRequestID(requestID).
		SetAPIKeyID(key.ID).
		SetProjectID(key.ProjectID).
		SetModelID(modelID).
		SetPromptTokens(promptTokens).
		SetCompletionTokens(completionTokens).
		SetPromptCachedTokens(cachedTokens).
		SetTotalTokens(totalTokens).
		SetCreatedAt(createdAt)
	if cost != nil {
		create.SetTotalCost(*cost)
	}
	return create.SaveX(f.ctx)
}

func dashboardModelsByID(dashboard *objects.PortalDashboard) map[string]*objects.PortalModelUsage {
	models := make(map[string]*objects.PortalModelUsage, len(dashboard.Models))
	for _, model := range dashboard.Models {
		models[model.ModelID] = model
	}
	return models
}

func TestPortalStatsDashboard_PersonalOwnershipAndProjectFilters(t *testing.T) {
	f := newPortalStatsFixture(t)
	defer f.client.Close()

	now := time.Now().UTC()
	profiles := &objects.APIKeyProfiles{ActiveProfile: "personal", Profiles: []objects.APIKeyProfile{{Name: "personal"}}}
	active := createPortalStatsKey(t, f, f.a, f.groupA, "a-active", apikey.TypePersonal, true, apikey.StatusEnabled, profiles)
	archived := createPortalStatsKey(t, f, f.a, f.groupA, "a-archived", apikey.TypePersonal, true, apikey.StatusArchived, profiles)
	softDeleted := createPortalStatsKey(t, f, f.a, f.groupA, "a-soft-deleted", apikey.TypePersonal, true, apikey.StatusEnabled, profiles)
	crossGroup := createPortalStatsKey(t, f, f.a, f.groupB, "a-cross-group", apikey.TypePersonal, true, apikey.StatusEnabled, profiles)
	bKey := createPortalStatsKey(t, f, f.b, f.groupA, "b-same-group", apikey.TypePersonal, true, apikey.StatusEnabled, profiles)

	legacyPersonal := createPortalStatsKey(t, f, f.a, f.groupA, "a-legacy-personal", apikey.TypePersonal, false, apikey.StatusEnabled, profiles)
	legacyUser := createPortalStatsKey(t, f, f.a, f.groupA, "a-legacy-user", apikey.TypeUser, false, apikey.StatusEnabled, profiles)
	legacyService := createPortalStatsKey(t, f, f.a, f.groupA, "a-legacy-service", apikey.TypeServiceAccount, false, apikey.StatusEnabled, profiles)

	activeRequest := createPortalStatsRequest(t, f, active, "a-active", request.StatusCompleted, now)
	archivedRequest := createPortalStatsRequest(t, f, archived, "a-archived", request.StatusFailed, now)
	crossGroupRequest := createPortalStatsRequest(t, f, crossGroup, "a-cross-group", request.StatusCompleted, now)
	createPortalStatsRequest(t, f, bKey, "b-only", request.StatusCompleted, now)

	createPortalStatsUsage(t, f, active, activeRequest.ID, "a-active", 2, 3, 1, 5, now, nil)
	// No Request row is created for this ID: UsageLog metrics must survive Request GC.
	createPortalStatsUsage(t, f, active, 900001, "a-active", 3, 4, 2, 7, now, nil)
	createPortalStatsUsage(t, f, archived, archivedRequest.ID, "a-archived", 1, 2, 0, 3, now, nil)
	createPortalStatsUsage(t, f, softDeleted, 900002, "a-soft-deleted", 1, 1, 0, 2, now, nil)
	createPortalStatsUsage(t, f, crossGroup, crossGroupRequest.ID, "a-cross-group", 50, 50, 0, 100, now, nil)
	createPortalStatsUsage(t, f, bKey, 900003, "b-only", 100, 100, 0, 200, now, nil)
	createPortalStatsUsage(t, f, legacyPersonal, 900004, "legacy-personal", 100, 100, 0, 200, now, nil)
	createPortalStatsUsage(t, f, legacyUser, 900005, "legacy-user", 100, 100, 0, 200, now, nil)
	createPortalStatsUsage(t, f, legacyService, 900006, "legacy-service", 100, 100, 0, 200, now, nil)

	require.NoError(t, f.client.APIKey.DeleteOneID(softDeleted.ID).Exec(f.ctx))

	dashboard, err := f.portal.Dashboard(f.userContext(f.a), nil, "all", nil)
	require.NoError(t, err)
	require.Equal(t, 3, dashboard.Requests)
	require.Equal(t, 2, dashboard.SuccessfulRequests)
	require.Equal(t, 5, dashboard.UsageCount)
	require.Equal(t, int64(57), dashboard.InputTokens)
	require.Equal(t, int64(60), dashboard.OutputTokens)
	require.Equal(t, int64(3), dashboard.CachedTokens)
	require.Equal(t, int64(117), dashboard.TotalTokens)
	require.Nil(t, dashboard.Cost)
	require.Equal(t, 0, dashboard.PricedCount)

	models := dashboardModelsByID(dashboard)
	require.ElementsMatch(t, []string{"a-active", "a-archived", "a-soft-deleted", "a-cross-group"}, mapKeys(models))
	require.Equal(t, 2, models["a-active"].UsageCount)
	require.Equal(t, int64(12), models["a-active"].TotalTokens)
	require.Equal(t, 1, models["a-archived"].UsageCount)
	require.Equal(t, int64(3), models["a-archived"].TotalTokens)
	require.Equal(t, 1, models["a-soft-deleted"].UsageCount)
	require.Equal(t, int64(2), models["a-soft-deleted"].TotalTokens)
	require.Equal(t, 1, models["a-cross-group"].UsageCount)
	require.NotContains(t, models, "b-only")
	require.NotContains(t, models, "legacy-personal")
	require.NotContains(t, models, "legacy-user")
	require.NotContains(t, models, "legacy-service")

	projectDashboard, err := f.portal.Dashboard(f.userContext(f.a), &f.groupA.ID, "all", nil)
	require.NoError(t, err)
	require.Equal(t, 2, projectDashboard.Requests)
	require.Equal(t, 1, projectDashboard.SuccessfulRequests)
	require.Equal(t, 4, projectDashboard.UsageCount)
	require.Equal(t, int64(17), projectDashboard.TotalTokens)
	projectModels := dashboardModelsByID(projectDashboard)
	require.NotContains(t, projectModels, "a-cross-group")
	require.Contains(t, projectModels, "a-soft-deleted")

	crossProjectDashboard, err := f.portal.Dashboard(f.userContext(f.a), &f.groupB.ID, "all", nil)
	require.NoError(t, err)
	require.Equal(t, 1, crossProjectDashboard.Requests)
	require.Equal(t, 1, crossProjectDashboard.SuccessfulRequests)
	require.Equal(t, 1, crossProjectDashboard.UsageCount)
	require.Equal(t, int64(100), crossProjectDashboard.TotalTokens)

	_, err = f.portal.Dashboard(f.userContext(f.a), &f.groupA.ID, "all", &bKey.ID)
	require.EqualError(t, err, "key unavailable")

	bDashboard, err := f.portal.Dashboard(f.userContext(f.b), nil, "all", nil)
	require.NoError(t, err)
	require.Equal(t, 1, bDashboard.Requests)
	require.Equal(t, 1, bDashboard.UsageCount)
	require.ElementsMatch(t, []string{"b-only"}, mapKeys(dashboardModelsByID(bDashboard)))

	noKeyDashboard, err := f.portal.Dashboard(f.userContext(f.noKeys), nil, "all", nil)
	require.NoError(t, err)
	require.Equal(t, 0, noKeyDashboard.Requests)
	require.Equal(t, 0, noKeyDashboard.UsageCount)
	require.Empty(t, noKeyDashboard.Models)
}

func TestPortalStatsDashboard_RejectsInvalidPeriod(t *testing.T) {
	f := newPortalStatsFixture(t)
	defer f.client.Close()

	_, err := f.portal.Dashboard(f.userContext(f.a), nil, "quarter", nil)
	require.EqualError(t, err, "invalid statistics period")
}

func TestPortalStatsDashboard_CostNullAndZeroRemainDistinct(t *testing.T) {
	f := newPortalStatsFixture(t)
	defer f.client.Close()

	now := time.Now().UTC()
	profiles := &objects.APIKeyProfiles{ActiveProfile: "personal", Profiles: []objects.APIKeyProfile{{Name: "personal"}}}
	nullKey := createPortalStatsKey(t, f, f.a, f.groupA, "null-cost", apikey.TypePersonal, true, apikey.StatusEnabled, profiles)
	zeroKey := createPortalStatsKey(t, f, f.a, f.groupA, "zero-cost", apikey.TypePersonal, true, apikey.StatusEnabled, profiles)
	nullRequest := createPortalStatsRequest(t, f, nullKey, "null-cost", request.StatusCompleted, now)
	zeroRequest := createPortalStatsRequest(t, f, zeroKey, "zero-cost", request.StatusCompleted, now)
	createPortalStatsUsage(t, f, nullKey, nullRequest.ID, "null-cost", 1, 1, 0, 2, now, nil)
	zeroCost := 0.0
	createPortalStatsUsage(t, f, zeroKey, zeroRequest.ID, "zero-cost", 1, 1, 0, 2, now, &zeroCost)

	nullDashboard, err := f.portal.Dashboard(f.userContext(f.a), nil, "all", &nullKey.ID)
	require.NoError(t, err)
	require.Nil(t, nullDashboard.Cost)
	require.Equal(t, 0, nullDashboard.PricedCount)
	require.Nil(t, dashboardModelsByID(nullDashboard)["null-cost"].Cost)
	require.Equal(t, 0, dashboardModelsByID(nullDashboard)["null-cost"].PricedCount)

	zeroDashboard, err := f.portal.Dashboard(f.userContext(f.a), nil, "all", &zeroKey.ID)
	require.NoError(t, err)
	require.NotNil(t, zeroDashboard.Cost)
	require.Equal(t, float64(0), *zeroDashboard.Cost)
	require.Equal(t, 1, zeroDashboard.PricedCount)
	require.NotNil(t, dashboardModelsByID(zeroDashboard)["zero-cost"].Cost)
	require.Equal(t, float64(0), *dashboardModelsByID(zeroDashboard)["zero-cost"].Cost)
	require.Equal(t, 1, dashboardModelsByID(zeroDashboard)["zero-cost"].PricedCount)
}

func TestPortalStatsKeyDTO_QuotaZeroAndUnset(t *testing.T) {
	f := newPortalStatsFixture(t)
	defer f.client.Close()

	zeroRequests := int64(0)
	key := createPortalStatsKey(t, f, f.a, f.groupA, "quota-zero", apikey.TypePersonal, true, apikey.StatusEnabled, &objects.APIKeyProfiles{
		ActiveProfile: "zero",
		Profiles: []objects.APIKeyProfile{
			{Name: "unset", ModelIDs: []string{"gpt-4"}},
			{
				Name:     "zero",
				ModelIDs: []string{"gpt-4"},
				Quota: &objects.APIKeyQuota{
					Requests: &zeroRequests,
					Period: objects.APIKeyQuotaPeriod{
						Type: objects.APIKeyQuotaPeriodTypePastDuration,
						PastDuration: &objects.APIKeyQuotaPastDuration{
							Value: 5,
							Unit:  objects.APIKeyQuotaPastDurationUnitMinute,
						},
					},
				},
			},
		},
	})

	dto, err := f.portal.keyDTO(f.userContext(f.a), key)
	require.NoError(t, err)
	require.Len(t, dto.Quotas, 1)
	require.Equal(t, "zero", dto.Quotas[0].ProfileName)
	require.True(t, dto.Quotas[0].Active)
	require.NotNil(t, dto.Quotas[0].Requests)
	require.Equal(t, int64(0), *dto.Quotas[0].Requests)
	require.Equal(t, int64(0), dto.Quotas[0].UsedRequests)
	require.NotNil(t, dto.Quotas[0].Start)
	require.NotNil(t, dto.Quotas[0].End)
}

func mapKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
