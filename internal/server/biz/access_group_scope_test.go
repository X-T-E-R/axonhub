package biz

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
)

func TestAccessGroupServicesRequireChannelScopeForFullPolicy(t *testing.T) {
	apiKeys, client := setupTestAPIKeyService(t, xcache.Config{Mode: xcache.ModeMemory})
	defer apiKeys.Stop()
	defer client.Close()

	setupCtx := ent.NewContext(context.Background(), client)
	setupCtx = authz.WithTestBypass(setupCtx)

	testProject, err := client.Project.Create().
		SetName(fmt.Sprintf("access-group-scopes-%d", time.Now().UnixNano())).
		SetStatus(project.StatusActive).
		Save(setupCtx)
	require.NoError(t, err)
	template, err := client.APIKeyProfileTemplate.Create().
		SetName("scoped-group").
		SetProject(testProject).
		SetProfile(&objects.APIKeyProfile{Name: "scoped-group", ModelIDs: []string{"gpt-4.1"}, ChannelIDs: []int{42}}).
		Save(setupCtx)
	require.NoError(t, err)

	admin, err := client.User.Create().
		SetEmail(fmt.Sprintf("access-group-scopes-%d@example.com", time.Now().UnixNano())).
		SetPassword("password").
		SetStatus(user.StatusActivated).
		Save(setupCtx)
	require.NoError(t, err)
	_, err = client.UserProject.Create().
		SetUserID(admin.ID).
		SetProjectID(testProject.ID).
		SetScopes([]string{string(scopes.ScopeReadAPIKeys), string(scopes.ScopeWriteAPIKeys)}).
		Save(setupCtx)
	require.NoError(t, err)
	admin, err = client.User.Query().
		Where(user.IDEQ(admin.ID)).
		WithProjectUsers().
		WithRoles().
		Only(setupCtx)
	require.NoError(t, err)

	ctx := ent.NewContext(context.Background(), client)
	ctx = authz.NewUserContext(ctx, admin.ID)
	ctx = contexts.WithUser(ctx, admin)
	ctx = contexts.WithProjectID(ctx, testProject.ID)

	_, err = apiKeys.ListAdminAccessGroups(ctx, testProject.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), string(scopes.ScopeReadChannels))

	description := "must not be applied"
	profiles := NewAPIKeyProfileTemplateService(APIKeyProfileTemplateServiceParams{Ent: client, APIKeys: apiKeys})
	_, err = profiles.UpdateTemplate(
		ctx,
		template.ID,
		ent.UpdateAPIKeyProfileTemplateInput{Description: &description},
		template.Profile,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), string(scopes.ScopeReadChannels))

	unchanged, err := client.APIKeyProfileTemplate.Get(setupCtx, template.ID)
	require.NoError(t, err)
	require.Empty(t, unchanged.Description)

	admin.Edges.ProjectUsers[0].Scopes = append(admin.Edges.ProjectUsers[0].Scopes, string(scopes.ScopeReadChannels))
	description = "applied with both mutation scopes"
	updated, err := profiles.UpdateTemplate(
		ctx,
		template.ID,
		ent.UpdateAPIKeyProfileTemplateInput{Description: &description},
		template.Profile,
	)
	require.NoError(t, err)
	require.Equal(t, description, updated.Description)

	groups, err := apiKeys.ListAdminAccessGroups(ctx, testProject.ID)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	require.Equal(t, []string{"gpt-4.1"}, groups[0].Profiles[0].ModelIDs)
	require.Equal(t, []int{42}, groups[0].ChannelAssignment.ChannelIDs)
}
