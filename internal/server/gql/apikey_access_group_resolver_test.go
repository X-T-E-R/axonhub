package gql

import (
	"context"
	"fmt"
	"testing"

	"github.com/99designs/gqlgen/client"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/project"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/pkg/xcache"
	"github.com/looplj/axonhub/internal/scopes"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestAPIKeyAccessGroupIDAcrossAdminGraphQLResponses(t *testing.T) {
	db := enttest.NewEntClient(t, "sqlite3", "file:gql-api-key-access-group?mode=memory&_fk=1")
	defer db.Close()
	setupCtx := ent.NewContext(authz.WithTestBypass(context.Background()), db)
	owner := db.User.Create().
		SetEmail("gql-access-group-owner@example.com").
		SetPassword("x").
		SetStatus(user.StatusActivated).
		SetIsOwner(true).
		SaveX(setupCtx)
	projectRow := db.Project.Create().SetName("gql-access-group-project").SetStatus(project.StatusActive).SaveX(setupCtx)
	group := db.APIKeyProfileTemplate.Create().
		SetName("linked-group").
		SetProject(projectRow).
		SetProfile(&objects.APIKeyProfile{Name: "linked-group"}).
		SaveX(setupCtx)
	linked := db.APIKey.Create().
		SetName("linked-key").
		SetKey("linked-key-secret").
		SetUser(owner).
		SetProject(projectRow).
		SetType(apikey.TypeServiceAccount).
		SetProvisioningSource(apikey.ProvisioningSourceAdmin).
		SetProfileMode(apikey.ProfileModeAccessGroup).
		SetAccessGroupID(group.ID).
		SetAccessGroupRevision(group.Revision).
		SaveX(setupCtx)
	db.APIKey.Create().
		SetName("snapshot-key").
		SetKey("snapshot-key-secret").
		SetUser(owner).
		SetProject(projectRow).
		SetType(apikey.TypeServiceAccount).
		SetProvisioningSource(apikey.ProvisioningSourceAdmin).
		SetProfileMode(apikey.ProfileModeSnapshot).
		SaveX(setupCtx)

	cacheConfig := xcache.Config{Mode: xcache.ModeMemory}
	projectService := biz.NewProjectService(biz.ProjectServiceParams{CacheConfig: cacheConfig, Ent: db})
	apiKeyService := biz.NewAPIKeyService(biz.APIKeyServiceParams{
		CacheConfig:    cacheConfig,
		Ent:            db,
		ProjectService: projectService,
		Registration:   biz.RegistrationConfig{SelfServiceEnabled: true},
		KeyPrefix:      "ah-test",
	})
	defer apiKeyService.Stop()
	handler := NewGraphqlHandlers(Dependencies{Ent: db, APIKeyService: apiKeyService})
	ownerCtx := authz.NewUserContext(context.Background(), owner.ID)
	ownerCtx = contexts.WithUser(ownerCtx, owner)
	ownerCtx = contexts.WithProjectID(ownerCtx, projectRow.ID)
	graphqlClient := client.New(handler.Graphql, func(request *client.Request) {
		request.HTTP = request.HTTP.WithContext(ownerCtx)
	})

	type apiKeyFields struct {
		ID            string
		Name          string
		AccessGroupID *string
	}
	var listResponse struct {
		APIKeys struct {
			Edges []struct {
				Node apiKeyFields
			}
		}
	}
	require.NoError(t, graphqlClient.Post(`query { apiKeys(first: 20) { edges { node { id name accessGroupID } } } }`, &listResponse))
	listed := map[string]*string{}
	for _, edge := range listResponse.APIKeys.Edges {
		listed[edge.Node.Name] = edge.Node.AccessGroupID
	}
	require.Nil(t, listed["snapshot-key"])
	require.NotNil(t, listed["linked-key"])
	require.Equal(t, guid(ent.TypeAPIKeyProfileTemplate, group.ID), *listed["linked-key"])

	var detailResponse struct{ Node apiKeyFields }
	require.NoError(t, graphqlClient.Post(`query Detail($id: ID!) { node(id: $id) { ... on APIKey { id name accessGroupID } } }`, &detailResponse,
		client.Var("id", guid(ent.TypeAPIKey, linked.ID))))
	require.Equal(t, guid(ent.TypeAPIKeyProfileTemplate, group.ID), *detailResponse.Node.AccessGroupID)

	var createResponse struct{ CreateAPIKey apiKeyFields }
	require.NoError(t, graphqlClient.Post(`mutation Create($input: CreateAPIKeyInput!) { createAPIKey(input: $input) { id name accessGroupID } }`, &createResponse,
		client.Var("input", map[string]any{"name": "created-snapshot", "type": "service_account", "projectID": guid(ent.TypeProject, projectRow.ID)})))
	require.Nil(t, createResponse.CreateAPIKey.AccessGroupID)

	var updateResponse struct{ UpdateAPIKey apiKeyFields }
	require.NoError(t, graphqlClient.Post(`mutation Update($id: ID!, $input: UpdateAPIKeyInput!) { updateAPIKey(id: $id, input: $input) { id name accessGroupID } }`, &updateResponse,
		client.Var("id", guid(ent.TypeAPIKey, linked.ID)), client.Var("input", map[string]any{"name": "linked-key-updated"})))
	require.Equal(t, guid(ent.TypeAPIKeyProfileTemplate, group.ID), *updateResponse.UpdateAPIKey.AccessGroupID)

	var rotateResponse struct{ RotateAPIKey apiKeyFields }
	require.NoError(t, graphqlClient.Post(`mutation Rotate($id: ID!) { rotateAPIKey(id: $id) { id name accessGroupID } }`, &rotateResponse,
		client.Var("id", guid(ent.TypeAPIKey, linked.ID))))
	require.Equal(t, guid(ent.TypeAPIKeyProfileTemplate, group.ID), *rotateResponse.RotateAPIKey.AccessGroupID)
}

func TestAPIKeyAccessGroupIDListKeepsProjectPrivacy(t *testing.T) {
	db := enttest.NewEntClient(t, "sqlite3", "file:gql-api-key-access-group-privacy?mode=memory&_fk=1")
	defer db.Close()
	setupCtx := ent.NewContext(authz.WithTestBypass(context.Background()), db)
	owner := db.User.Create().SetEmail("gql-project-privacy-owner@example.com").SetPassword("x").SetStatus(user.StatusActivated).SetIsOwner(true).SaveX(setupCtx)
	visibleProject := db.Project.Create().SetName("visible-project").SetStatus(project.StatusActive).SaveX(setupCtx)
	hiddenProject := db.Project.Create().SetName("hidden-project").SetStatus(project.StatusActive).SaveX(setupCtx)
	visibleGroup := db.APIKeyProfileTemplate.Create().SetName("visible-group").SetProject(visibleProject).SetProfile(&objects.APIKeyProfile{Name: "visible-group"}).SaveX(setupCtx)
	hiddenGroup := db.APIKeyProfileTemplate.Create().SetName("hidden-group").SetProject(hiddenProject).SetProfile(&objects.APIKeyProfile{Name: "hidden-group"}).SaveX(setupCtx)
	reader := db.APIKey.Create().SetName("reader").SetKey("reader-secret").SetUser(owner).SetProject(visibleProject).SetType(apikey.TypeServiceAccount).SetScopes([]string{string(scopes.ScopeReadAPIKeys)}).SaveX(setupCtx)
	db.APIKey.Create().SetName("visible-linked").SetKey("visible-linked-secret").SetUser(owner).SetProject(visibleProject).SetType(apikey.TypeServiceAccount).SetProfileMode(apikey.ProfileModeAccessGroup).SetAccessGroupID(visibleGroup.ID).SetAccessGroupRevision(visibleGroup.Revision).SaveX(setupCtx)
	db.APIKey.Create().SetName("hidden-linked").SetKey("hidden-linked-secret").SetUser(owner).SetProject(hiddenProject).SetType(apikey.TypeServiceAccount).SetProfileMode(apikey.ProfileModeAccessGroup).SetAccessGroupID(hiddenGroup.ID).SetAccessGroupRevision(hiddenGroup.Revision).SaveX(setupCtx)

	handler := NewGraphqlHandlers(Dependencies{Ent: db})
	requestCtx := authz.NewAPIKeyContext(context.Background(), reader.ID, visibleProject.ID)
	requestCtx = contexts.WithAPIKey(requestCtx, reader)
	requestCtx = contexts.WithProjectID(requestCtx, visibleProject.ID)
	graphqlClient := client.New(handler.Graphql, func(request *client.Request) {
		request.HTTP = request.HTTP.WithContext(requestCtx)
	})
	var response struct {
		APIKeys struct {
			Edges []struct {
				Node struct {
					Name          string
					AccessGroupID *string
				}
			}
		}
	}
	require.NoError(t, graphqlClient.Post(`query { apiKeys(first: 20) { edges { node { name accessGroupID } } } }`, &response))
	names := map[string]*string{}
	for _, edge := range response.APIKeys.Edges {
		names[edge.Node.Name] = edge.Node.AccessGroupID
	}
	require.Contains(t, names, "visible-linked")
	require.Equal(t, guid(ent.TypeAPIKeyProfileTemplate, visibleGroup.ID), *names["visible-linked"])
	require.NotContains(t, names, "hidden-linked")
}

func guid(entityType string, id int) string {
	return fmt.Sprintf("gid://axonhub/%s/%d", entityType, id)
}
