package gql

import (
	"context"
	"fmt"
	"testing"
	"time"

	gqlclient "github.com/99designs/gqlgen/client"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
)

func setupTestAPIKeyProfileTemplateResolvers(t *testing.T) (*mutationResolver, *queryResolver, context.Context, *ent.Client) {
	t.Helper()

	client := enttest.NewEntClient(t, "sqlite3", "file:ent?mode=memory&_fk=1")
	svc := biz.NewAPIKeyProfileTemplateService(biz.APIKeyProfileTemplateServiceParams{
		Ent: client,
	})

	ctx := context.Background()
	ctx = ent.NewContext(ctx, client)
	ctx = authz.WithTestBypass(ctx)

	resolver := &Resolver{
		client:                       client,
		apiKeyProfileTemplateService: svc,
	}

	return &mutationResolver{resolver}, &queryResolver{resolver}, ctx, client
}

func createTestUserWithScopes(t *testing.T, ctx context.Context, client *ent.Client) *ent.User {
	t.Helper()

	hashedPassword, err := biz.HashPassword("test-password")
	require.NoError(t, err)

	testUser, err := client.User.Create().
		SetEmail(fmt.Sprintf("test-%d@example.com", time.Now().UnixNano())).
		SetPassword(hashedPassword).
		SetFirstName("Test").
		SetLastName("User").
		SetStatus(user.StatusActivated).
		SetIsOwner(true).
		Save(ctx)
	require.NoError(t, err)

	return testUser
}

func createTestProject(t *testing.T, ctx context.Context, client *ent.Client) *ent.Project {
	t.Helper()

	testProject, err := client.Project.Create().
		SetName(fmt.Sprintf("test-project-%d", time.Now().UnixNano())).
		SetDescription("test project").
		SetStatus(project.StatusActive).
		Save(ctx)
	require.NoError(t, err)

	return testProject
}

func TestApiKeyProfileTemplate_CreateTemplate(t *testing.T) {
	mutationResolver, _, ctx, client := setupTestAPIKeyProfileTemplateResolvers(t)
	defer client.Close()

	testUser := createTestUserWithScopes(t, ctx, client)
	ctx = contexts.WithUser(ctx, testUser)

	testProject := createTestProject(t, ctx, client)

	input := ent.CreateAPIKeyProfileTemplateInput{
		Name:        "my-template",
		Description: ptrStr("A test template"),
		ProjectID:   testProject.ID,
	}

	profile := objects.APIKeyProfile{
		Name: "test-profile",
		ModelMappings: []objects.ModelMapping{
			{From: "gpt-4", To: "gpt-4-turbo"},
		},
	}

	template, err := mutationResolver.CreateAPIKeyProfileTemplate(ctx, input, profile)
	require.NoError(t, err)
	require.NotNil(t, template)
	require.Equal(t, "my-template", template.Name)
	require.Equal(t, testProject.ID, template.ProjectID)
	require.NotNil(t, template.Profile)
	require.Equal(t, "my-template", template.Profile.Name)
}

func TestApiKeyProfileTemplate_UpdateTemplate(t *testing.T) {
	mutationResolver, _, ctx, client := setupTestAPIKeyProfileTemplateResolvers(t)
	defer client.Close()

	testUser := createTestUserWithScopes(t, ctx, client)
	ctx = contexts.WithUser(ctx, testUser)

	testProject := createTestProject(t, ctx, client)

	template, err := client.APIKeyProfileTemplate.Create().
		SetName("original-name").
		SetDescription("original desc").
		SetProject(testProject).
		SetProfile(&objects.APIKeyProfile{Name: "original-profile"}).
		Save(ctx)
	require.NoError(t, err)

	input := ent.UpdateAPIKeyProfileTemplateInput{
		Name:        ptrStr("updated-name"),
		Description: ptrStr("updated desc"),
	}

	updatedProfile := &objects.APIKeyProfile{
		Name: "updated-profile",
		ModelMappings: []objects.ModelMapping{
			{From: "claude-3", To: "claude-3-opus"},
		},
	}

	result, err := mutationResolver.UpdateAPIKeyProfileTemplate(ctx, objects.GUID{Type: ent.TypeAPIKeyProfileTemplate, ID: template.ID}, input, updatedProfile)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "updated-name", result.Name)
	require.Equal(t, "updated desc", result.Description)
	require.Equal(t, "updated-name", result.Profile.Name)
}

func TestApiKeyProfileTemplate_DeleteTemplate(t *testing.T) {
	mutationResolver, _, ctx, client := setupTestAPIKeyProfileTemplateResolvers(t)
	defer client.Close()

	testUser := createTestUserWithScopes(t, ctx, client)
	ctx = contexts.WithUser(ctx, testUser)

	testProject := createTestProject(t, ctx, client)

	template, err := client.APIKeyProfileTemplate.Create().
		SetName("to-delete").
		SetDescription("delete me").
		SetProject(testProject).
		SetProfile(&objects.APIKeyProfile{Name: "delete-profile"}).
		Save(ctx)
	require.NoError(t, err)

	result, err := mutationResolver.DeleteAPIKeyProfileTemplate(ctx, objects.GUID{Type: ent.TypeAPIKeyProfileTemplate, ID: template.ID})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "to-delete", result.Name)

	_, err = client.APIKeyProfileTemplate.Get(ctx, template.ID)
	require.Error(t, err)
}

func TestApiKeyProfileTemplate_LoadTemplate(t *testing.T) {
	mutationResolver, _, ctx, client := setupTestAPIKeyProfileTemplateResolvers(t)
	defer client.Close()

	testUser := createTestUserWithScopes(t, ctx, client)
	ctx = contexts.WithUser(ctx, testUser)

	testProject := createTestProject(t, ctx, client)

	existingProfiles := &objects.APIKeyProfiles{
		ActiveProfile: "Default",
		Profiles: []objects.APIKeyProfile{
			{Name: "Default"},
		},
	}

	apiKey, err := client.APIKey.Create().
		SetName("test-api-key").
		SetKey(fmt.Sprintf("ah-test-%d", time.Now().UnixNano())).
		SetUserID(testUser.ID).
		SetProjectID(testProject.ID).
		SetType(apikey.TypeUser).
		SetProfiles(existingProfiles).
		Save(ctx)
	require.NoError(t, err)

	templateProfile := &objects.APIKeyProfile{
		ModelMappings: []objects.ModelMapping{
			{From: "gpt-4", To: "gpt-4-turbo"},
		},
	}

	template, err := client.APIKeyProfileTemplate.Create().
		SetName("load-template").
		SetDescription("load me").
		SetProject(testProject).
		SetProfile(templateProfile).
		Save(ctx)
	require.NoError(t, err)

	input := LoadAPIKeyProfileTemplateInput{
		TemplateID: objects.GUID{Type: ent.TypeAPIKeyProfileTemplate, ID: template.ID},
		APIKeyID:   objects.GUID{Type: ent.TypeAPIKey, ID: apiKey.ID},
	}

	result, err := mutationResolver.LoadAPIKeyProfileTemplate(ctx, input)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.NotNil(t, result.Profiles)
	require.Len(t, result.Profiles.Profiles, 2)
	require.Equal(t, "Default", result.Profiles.Profiles[0].Name)
	require.Equal(t, "load-template", result.Profiles.Profiles[1].Name)
}

func TestApiKeyProfileTemplate_QueryTemplates(t *testing.T) {
	_, queryResolver, ctx, client := setupTestAPIKeyProfileTemplateResolvers(t)
	defer client.Close()

	testUser := createTestUserWithScopes(t, ctx, client)
	ctx = contexts.WithUser(ctx, testUser)

	testProject := createTestProject(t, ctx, client)

	for i := range 3 {
		_, err := client.APIKeyProfileTemplate.Create().
			SetName(fmt.Sprintf("template-%d", i)).
			SetDescription(fmt.Sprintf("template %d", i)).
			SetProject(testProject).
			SetProfile(&objects.APIKeyProfile{Name: fmt.Sprintf("profile-%d", i)}).
			Save(ctx)
		require.NoError(t, err)
	}

	where := &ent.APIKeyProfileTemplateWhereInput{
		HasProject: ptrBool(true),
		HasProjectWith: []*ent.ProjectWhereInput{
			{ID: &testProject.ID},
		},
	}

	first := 10
	conn, err := queryResolver.APIKeyProfileTemplates(ctx, nil, &first, nil, nil, nil, where)
	require.NoError(t, err)
	require.NotNil(t, conn)
	require.Equal(t, 3, conn.TotalCount)
}

func TestAPIKeyProfileTemplateResolvers_RequireReadChannelsForPolicy(t *testing.T) {
	mutationResolver, queryResolver, setupCtx, client := setupTestAPIKeyProfileTemplateResolvers(t)
	defer client.Close()

	testProject := createTestProject(t, setupCtx, client)
	template, err := client.APIKeyProfileTemplate.Create().
		SetName("scoped-template").
		SetProject(testProject).
		SetProfile(&objects.APIKeyProfile{Name: "scoped-template", ModelIDs: []string{"gpt-4.1"}, ChannelIDs: []int{42}}).
		Save(setupCtx)
	require.NoError(t, err)

	adminUser, err := client.User.Create().
		SetEmail(fmt.Sprintf("scoped-template-%d@example.com", time.Now().UnixNano())).
		SetPassword("password").
		SetStatus(user.StatusActivated).
		Save(setupCtx)
	require.NoError(t, err)
	_, err = client.UserProject.Create().
		SetUserID(adminUser.ID).
		SetProjectID(testProject.ID).
		SetScopes([]string{string(scopes.ScopeReadAPIKeys), string(scopes.ScopeWriteAPIKeys)}).
		Save(setupCtx)
	require.NoError(t, err)
	adminUser, err = client.User.Query().
		Where(user.IDEQ(adminUser.ID)).
		WithProjectUsers().
		WithRoles().
		Only(setupCtx)
	require.NoError(t, err)

	ctx := ent.NewContext(context.Background(), client)
	ctx = authz.NewUserContext(ctx, adminUser.ID)
	ctx = contexts.WithUser(ctx, adminUser)
	ctx = contexts.WithProjectID(ctx, testProject.ID)

	first := 10
	conn, err := queryResolver.APIKeyProfileTemplates(ctx, nil, &first, nil, nil, nil, &ent.APIKeyProfileTemplateWhereInput{})
	require.NoError(t, err, "read_api_keys still permits explicit metadata projection")
	require.Len(t, conn.Edges, 1)

	profileResolver := &aPIKeyProfileTemplateResolver{queryResolver.Resolver}
	_, err = profileResolver.Profile(ctx, conn.Edges[0].Node)
	require.Error(t, err)
	require.Contains(t, err.Error(), string(scopes.ScopeReadChannels))

	newDescription := "must not be applied"
	_, err = mutationResolver.UpdateAPIKeyProfileTemplate(
		ctx,
		objects.GUID{Type: ent.TypeAPIKeyProfileTemplate, ID: template.ID},
		ent.UpdateAPIKeyProfileTemplateInput{Description: &newDescription},
		template.Profile,
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), string(scopes.ScopeReadChannels))

	unchanged, err := client.APIKeyProfileTemplate.Get(setupCtx, template.ID)
	require.NoError(t, err)
	require.Empty(t, unchanged.Description)

	adminUser.Edges.ProjectUsers[0].Scopes = append(adminUser.Edges.ProjectUsers[0].Scopes, string(scopes.ScopeReadChannels))
	profile, err := profileResolver.Profile(ctx, conn.Edges[0].Node)
	require.NoError(t, err)
	require.Equal(t, []string{"gpt-4.1"}, profile.ModelIDs)
	require.Equal(t, []int{42}, profile.ChannelIDs)

	newDescription = "applied with both scopes"
	updated, err := mutationResolver.UpdateAPIKeyProfileTemplate(
		ctx,
		objects.GUID{Type: ent.TypeAPIKeyProfileTemplate, ID: template.ID},
		ent.UpdateAPIKeyProfileTemplateInput{Description: &newDescription},
		template.Profile,
	)
	require.NoError(t, err)
	require.Equal(t, newDescription, updated.Description)
}

func TestAPIKeyProfileTemplateGraphQL_ProjectsMetadataButGuardsProfile(t *testing.T) {
	db := enttest.NewEntClient(t, "sqlite3", "file:gql-access-group-scopes?mode=memory&_fk=1")
	defer db.Close()
	setupCtx := ent.NewContext(authz.WithTestBypass(context.Background()), db)

	testProject := createTestProject(t, setupCtx, db)
	db.APIKeyProfileTemplate.Create().
		SetName("graphql-scoped-template").
		SetProject(testProject).
		SetProfile(&objects.APIKeyProfile{Name: "graphql-scoped-template", ModelIDs: []string{"gpt-4.1"}, ChannelIDs: []int{42}}).
		SaveX(setupCtx)
	reader := db.User.Create().
		SetEmail("graphql-scoped-template@example.com").
		SetPassword("password").
		SetStatus(user.StatusActivated).
		SaveX(setupCtx)
	db.UserProject.Create().
		SetUserID(reader.ID).
		SetProjectID(testProject.ID).
		SetScopes([]string{string(scopes.ScopeReadAPIKeys)}).
		SaveX(setupCtx)
	reader = db.User.Query().Where(user.IDEQ(reader.ID)).WithProjectUsers().WithRoles().OnlyX(setupCtx)

	requestCtx := authz.NewUserContext(context.Background(), reader.ID)
	requestCtx = contexts.WithUser(requestCtx, reader)
	requestCtx = contexts.WithProjectID(requestCtx, testProject.ID)
	handler := NewGraphqlHandlers(Dependencies{Ent: db})
	graphqlClient := gqlclient.New(handler.Graphql, func(request *gqlclient.Request) {
		request.HTTP = request.HTTP.WithContext(requestCtx)
	})

	var metadata struct {
		APIKeyProfileTemplates struct {
			Edges []struct {
				Node struct{ Name string }
			}
		}
	}
	require.NoError(t, graphqlClient.Post(`query { apiKeyProfileTemplates(first: 10) { edges { node { name } } } }`, &metadata))
	require.Len(t, metadata.APIKeyProfileTemplates.Edges, 1)
	require.Equal(t, "graphql-scoped-template", metadata.APIKeyProfileTemplates.Edges[0].Node.Name)

	var fullPolicy struct {
		APIKeyProfileTemplates struct {
			Edges []struct {
				Node struct {
					Profile *struct {
						ModelIDs   []string
						ChannelIDs []int
					}
				}
			}
		}
	}
	err := graphqlClient.Post(`query { apiKeyProfileTemplates(first: 10) { edges { node { profile { modelIDs channelIDs } } } } }`, &fullPolicy)
	require.Error(t, err)
	require.Contains(t, err.Error(), string(scopes.ScopeReadChannels))

	reader.Edges.ProjectUsers[0].Scopes = append(reader.Edges.ProjectUsers[0].Scopes, string(scopes.ScopeReadChannels))
	require.NoError(t, graphqlClient.Post(`query { apiKeyProfileTemplates(first: 10) { edges { node { profile { modelIDs channelIDs } } } } }`, &fullPolicy))
	require.Equal(t, []string{"gpt-4.1"}, fullPolicy.APIKeyProfileTemplates.Edges[0].Node.Profile.ModelIDs)
	require.Equal(t, []int{42}, fullPolicy.APIKeyProfileTemplates.Edges[0].Node.Profile.ChannelIDs)
}

func ptrStr(s string) *string {
	return &s
}

func ptrBool(b bool) *bool {
	return &b
}
