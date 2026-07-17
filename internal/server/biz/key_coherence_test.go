package biz

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/request"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
)

func TestSelfServicePersonalLifecycleAndAccessGroupPropagation(t *testing.T) {
	service, client := setupTestAPIKeyService(t, xcache.Config{Mode: xcache.ModeMemory})
	defer service.Stop()
	defer client.Close()
	setupCtx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	u := client.User.Create().SetEmail(uuid.NewString() + "@example.com").SetPassword("x").SetStatus(user.StatusActivated).SaveX(setupCtx)
	p := client.Project.Create().SetName(uuid.NewString()).SetStatus(project.StatusActive).SaveX(setupCtx)
	client.UserProject.Create().SetUser(u).SetProject(p).SaveX(setupCtx)
	group := client.APIKeyProfileTemplate.Create().SetProject(p).SetName("live").SetSelfServiceVisible(true).SetRevision(1).SetProfile(&objects.APIKeyProfile{Name: "live", ModelIDs: []string{"m1"}}).SaveX(setupCtx)
	ctx := ent.NewContext(authz.NewUserContext(context.Background(), u.ID), client)
	ctx = contexts.WithUser(ctx, u)
	ctx = contexts.WithProjectID(ctx, p.ID)
	created, err := service.CreateMyAPIKey(ctx, MyCreateAPIKeyInput{ProjectID: p.ID, Name: "mine", PresetID: group.ID})
	require.NoError(t, err)
	key := client.APIKey.GetX(setupCtx, created.ID)
	require.Equal(t, apikey.TypePersonal, key.Type)
	require.Equal(t, apikey.ProvisioningSourceSelfService, key.ProvisioningSource)
	require.Equal(t, apikey.ProfileModeAccessGroup, key.ProfileMode)
	require.Equal(t, group.ID, *key.AccessGroupID)
	require.Equal(t, int64(1), *key.AccessGroupRevision)
	revealed, err := service.RevealMyAPIKey(ctx, key.ID)
	require.NoError(t, err)
	require.Equal(t, key.Key, revealed.Key)

	models := []string{"m2"}
	_, err = service.UpdateAdminAccessGroup(setupCtx, group.ID, AdminAccessGroupInput{ModelIDs: &models})
	require.NoError(t, err)
	key = client.APIKey.GetX(setupCtx, key.ID)
	require.Equal(t, int64(2), *key.AccessGroupRevision)
	require.Equal(t, []string{"m2"}, key.Profiles.Profiles[0].ModelIDs)
	require.Equal(t, 1, client.APIKeyProfileTemplateRevision.Query().CountX(setupCtx))
	revision := client.APIKeyProfileTemplateRevision.Query().OnlyX(setupCtx)
	require.Equal(t, int64(2), revision.Revision)
	require.Equal(t, []string{"m2"}, revision.Profile.ModelIDs)
	late, err := service.CreateMyAPIKey(ctx, MyCreateAPIKeyInput{ProjectID: p.ID, Name: "late", PresetID: group.ID})
	require.NoError(t, err)
	lateKey := client.APIKey.GetX(setupCtx, late.ID)
	require.Equal(t, int64(2), *lateKey.AccessGroupRevision)
	require.Equal(t, []string{"m2"}, lateKey.Profiles.Profiles[0].ModelIDs)

	client.APIKey.UpdateOneID(key.ID).SetStatus(apikey.StatusArchived).SaveX(setupCtx)
	_, err = service.RevealMyAPIKey(ctx, key.ID)
	require.ErrorIs(t, err, ErrAPIKeyArchived)
}

func TestConcurrentAccessGroupDispatcherPreservesUnrelatedChanges(t *testing.T) {
	service, client := setupTestAPIKeyService(t, xcache.Config{Mode: xcache.ModeMemory})
	defer service.Stop()
	defer client.Close()
	setupCtx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	p := client.Project.Create().SetName(uuid.NewString()).SetStatus(project.StatusActive).SaveX(setupCtx)
	ch := client.Channel.Create().SetType(channel.TypeOpenai).SetName(uuid.NewString()).SetBaseURL("https://example.com/v1").SetCredentials(objects.ChannelCredentials{APIKey: "secret"}).SetSupportedModels([]string{"m"}).SetManualModels([]string{}).SetDefaultTestModel("m").SaveX(setupCtx)
	group := client.APIKeyProfileTemplate.Create().SetProject(p).SetName("concurrent").SetRevision(1).SetProfile(&objects.APIKeyProfile{Name: "concurrent"}).SaveX(setupCtx)

	for i := 0; i < 20; i++ {
		baseModels := []string{fmt.Sprintf("base-%d", i)}
		emptyChannels := []int{}
		_, err := service.UpdateAdminAccessGroup(setupCtx, group.ID, AdminAccessGroupInput{ModelIDs: &baseModels, ChannelIDs: &emptyChannels})
		require.NoError(t, err)

		models := []string{fmt.Sprintf("model-%d", i)}
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, updateErr := service.UpdateAdminAccessGroup(setupCtx, group.ID, AdminAccessGroupInput{ModelIDs: &models})
			errs <- updateErr
		}()
		go func() {
			defer wg.Done()
			<-start
			_, updateErr := service.AddChannelsToAccessGroup(setupCtx, group.ID, []int{ch.ID})
			errs <- updateErr
		}()
		close(start)
		wg.Wait()
		close(errs)
		for updateErr := range errs {
			require.NoError(t, updateErr)
		}
		head := client.APIKeyProfileTemplate.GetX(setupCtx, group.ID)
		require.Equal(t, models, head.Profile.ModelIDs)
		require.Equal(t, []int{ch.ID}, head.Profile.ChannelIDs)
	}
}

func TestConcurrentLegacyAttachAndMutationRemainCoherent(t *testing.T) {
	service, client := setupTestAPIKeyService(t, xcache.Config{Mode: xcache.ModeMemory})
	defer service.Stop()
	defer client.Close()
	setupCtx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	u := client.User.Create().SetEmail(uuid.NewString() + "@example.com").SetPassword("x").SetStatus(user.StatusActivated).SaveX(setupCtx)
	p := client.Project.Create().SetName(uuid.NewString()).SetStatus(project.StatusActive).SaveX(setupCtx)
	client.UserProject.Create().SetUser(u).SetProject(p).SaveX(setupCtx)
	group := client.APIKeyProfileTemplate.Create().SetProject(p).SetName("attach").SetSelfServiceVisible(true).SetRevision(1).SetProfile(&objects.APIKeyProfile{Name: "attach", ModelIDs: []string{"initial"}}).SaveX(setupCtx)
	ctx := ent.NewContext(authz.NewUserContext(context.Background(), u.ID), client)
	ctx = contexts.WithUser(ctx, u)
	ctx = contexts.WithProjectID(ctx, p.ID)

	for i := 0; i < 20; i++ {
		legacy := client.APIKey.Create().SetProject(p).SetUser(u).SetName(fmt.Sprintf("legacy-%d", i)).SetKey(fmt.Sprintf("legacy-secret-%d", i)).SetType(apikey.TypeUser).SetProfiles(&objects.APIKeyProfiles{}).SaveX(setupCtx)
		models := []string{fmt.Sprintf("head-%d", i)}
		start := make(chan struct{})
		errs := make(chan error, 2)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, classifyErr := service.ClassifyMyLegacyAPIKey(ctx, legacy.ID, LegacyAPIKeyClassificationInput{Mode: "personal_access_group", AccessGroupID: &group.ID})
			errs <- classifyErr
		}()
		go func() {
			defer wg.Done()
			<-start
			_, updateErr := service.UpdateAdminAccessGroup(setupCtx, group.ID, AdminAccessGroupInput{ModelIDs: &models})
			errs <- updateErr
		}()
		close(start)
		wg.Wait()
		close(errs)
		for operationErr := range errs {
			require.NoError(t, operationErr)
		}
		head := client.APIKeyProfileTemplate.GetX(setupCtx, group.ID)
		attached := client.APIKey.GetX(setupCtx, legacy.ID)
		require.Equal(t, apikey.ProfileModeAccessGroup, attached.ProfileMode)
		require.Equal(t, head.Revision, *attached.AccessGroupRevision)
		require.Equal(t, head.Profile.ModelIDs, attached.Profiles.Profiles[0].ModelIDs)
	}
}

func TestLegacyExposureNamesAreEvidenceOnlyAndSurviveRegistrationUpdates(t *testing.T) {
	service, client := setupTestAPIKeyService(t, xcache.Config{Mode: xcache.ModeMemory})
	defer service.Stop()
	defer client.Close()
	setupCtx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	p := client.Project.Create().SetName(uuid.NewString()).SetStatus(project.StatusActive).SaveX(setupCtx)
	u := client.User.Create().SetEmail(uuid.NewString() + "@example.com").SetPassword("x").SetStatus(user.StatusActivated).SaveX(setupCtx)
	client.UserProject.Create().SetUser(u).SetProject(p).SaveX(setupCtx)
	hidden := client.APIKeyProfileTemplate.Create().SetProject(p).SetName("fast").SetProfile(&objects.APIKeyProfile{Name: "fast"}).SaveX(setupCtx)
	ctx := ent.NewContext(authz.NewUserContext(context.Background(), u.ID), client)
	ctx = contexts.WithUser(ctx, u)
	ctx = contexts.WithProjectID(ctx, p.ID)

	groups, err := service.ListMyAccessGroups(ctx, p.ID)
	require.NoError(t, err)
	require.Empty(t, groups, "legacy name fallback must not expose a hidden group")
	_, err = service.CreateMyAPIKey(ctx, MyCreateAPIKeyInput{ProjectID: p.ID, Name: "blocked", PresetID: hidden.ID})
	require.ErrorContains(t, err, "not available for self-service")

	systems := NewSystemService(SystemServiceParams{})
	require.NoError(t, systems.SetRegistrationConfig(setupCtx, RegistrationConfig{SelfServiceEnabled: true, SelfServicePresetNames: []string{"legacy-name"}}))
	require.NoError(t, systems.SetRegistrationConfig(setupCtx, RegistrationConfig{SelfServiceEnabled: false, SelfServicePresetNames: []string{}}))
	stored, err := systems.RegistrationConfig(setupCtx, RegistrationConfig{})
	require.NoError(t, err)
	require.Equal(t, []string{"legacy-name"}, stored.SelfServicePresetNames)
}

func TestLegacyClassificationAndOwnerScopedRequestDetail(t *testing.T) {
	service, client := setupTestAPIKeyService(t, xcache.Config{Mode: xcache.ModeMemory})
	defer service.Stop()
	defer client.Close()
	service.Registration.AllowRequestDetails = true
	setupCtx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	u := client.User.Create().SetEmail(uuid.NewString() + "@example.com").SetPassword("x").SetStatus(user.StatusActivated).SaveX(setupCtx)
	other := client.User.Create().SetEmail(uuid.NewString() + "@example.com").SetPassword("x").SetStatus(user.StatusActivated).SaveX(setupCtx)
	p := client.Project.Create().SetName(uuid.NewString()).SetStatus(project.StatusActive).SaveX(setupCtx)
	client.UserProject.Create().SetUser(u).SetProject(p).SaveX(setupCtx)
	client.UserProject.Create().SetUser(other).SetProject(p).SaveX(setupCtx)
	legacy := client.APIKey.Create().SetProject(p).SetUser(u).SetName("legacy").SetKey("legacy-secret").SetType(apikey.TypeUser).SetProfiles(&objects.APIKeyProfiles{}).SaveX(setupCtx)
	ctx := ent.NewContext(authz.NewUserContext(context.Background(), u.ID), client)
	ctx = contexts.WithUser(ctx, u)
	ctx = contexts.WithProjectID(ctx, p.ID)
	classified, err := service.ClassifyMyLegacyAPIKey(ctx, legacy.ID, LegacyAPIKeyClassificationInput{Mode: "personal_snapshot"})
	require.NoError(t, err)
	require.Equal(t, apikey.TypePersonal.String(), classified.Type)
	classified, err = service.ClassifyMyLegacyAPIKey(ctx, legacy.ID, LegacyAPIKeyClassificationInput{Mode: "personal_snapshot"})
	require.NoError(t, err)
	require.Equal(t, apikey.ProvisioningSourceSelfService.String(), classified.ProvisioningSource)
	own := client.Request.Create().SetProject(p).SetAPIKeyID(legacy.ID).SetModelID("m").SetRequestBody([]byte(`{"secret":"preserved"}`)).SetStatus(request.StatusCompleted).SaveX(setupCtx)
	otherKey := client.APIKey.Create().SetProject(p).SetUser(other).SetName("other").SetKey("other-secret").SetType(apikey.TypePersonal).SaveX(setupCtx)
	foreign := client.Request.Create().SetProject(p).SetAPIKeyID(otherKey.ID).SetModelID("m").SetRequestBody([]byte(`{}`)).SetStatus(request.StatusCompleted).SaveX(setupCtx)
	detail, err := service.GetMyRequestDetail(ctx, own.ID)
	require.NoError(t, err)
	require.Equal(t, own.ID, detail.Request.ID)
	require.Equal(t, "available", detail.EvidenceAvailability.RequestBody.State)
	require.Equal(t, "legacyUnknown", detail.EvidenceAvailability.ResponseBody.State)
	listed, err := service.ListMyRequests(ctx, p.ID, 50)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, own.ID, listed[0].ID)
	_, err = service.GetMyRequestDetail(ctx, foreign.ID)
	require.ErrorIs(t, err, ErrNotFoundOrNotAuthorized)
	service.Registration.AllowRequestDetails = false
	_, err = service.GetMyRequestDetail(ctx, own.ID)
	require.ErrorIs(t, err, ErrSelfServiceRequestDetailsDisabled)
}
