package biz

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/model"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/cluster"
)

func TestSelectStaticRouteUsesLowestPriorityAndSortedOwner(t *testing.T) {
	owner, owners, status := selectStaticRoute([]routeLink{{Owner: "lag", Priority: 0}, {Owner: "gcp", Priority: 0}})
	require.Equal(t, "ready", status)
	require.Equal(t, "gcp", owner)
	require.Equal(t, []string{"gcp"}, owners)

	owner, owners, status = selectStaticRoute([]routeLink{{Owner: "lag", Priority: 10}, {Owner: "gcp", Priority: 0}})
	require.Equal(t, "gcp", owner)
	require.Equal(t, "ready", status)
	require.Equal(t, []string{"gcp"}, owners)

	owner, owners, status = selectStaticRoute([]routeLink{{Owner: "lag", Priority: 10, Conditional: true}, {Owner: "gcp", Priority: 0, Conditional: true}})
	require.Equal(t, "ready", status)
	require.Equal(t, "gcp", owner)
	require.Equal(t, []string{"gcp"}, owners)

	owner, owners, status = selectStaticRoute([]routeLink{{Owner: "lag", Priority: 5, Conditional: true}, {Owner: "gcp", Priority: 5, Conditional: true}})
	require.Equal(t, "ready", status)
	require.Equal(t, "gcp", owner)
	require.Equal(t, []string{"gcp"}, owners)
}

func TestStaticManifestConvergesToSingleWinnerAndReportsNotices(t *testing.T) {
	db := enttest.NewEntClient(t, "sqlite3", "file:static-route-"+uuid.NewString()+"?mode=memory&_fk=1")
	defer db.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), db)
	makeChannel := func(name string, models ...string) *Channel {
		if len(models) == 0 {
			models = []string{"m"}
		}
		row := db.Channel.Create().SetName(name).SetType(channel.TypeOpenai).SetStatus(channel.StatusEnabled).SetBaseURL("https://unused.example.test").
			SetCredentials(objects.ChannelCredentials{APIKey: "synthetic"}).SetSupportedModels(models).SetDefaultTestModel(models[0]).SaveX(ctx)
		return &Channel{Channel: row}
	}
	gcp := makeChannel("gcp-channel", "m", "unconfigured-shared", "when-model")
	lag := makeChannel("lag-channel", "m", "unconfigured-shared", "when-model")
	channels := NewChannelServiceForTest(db)
	defer channels.Stop()
	channels.SetEnabledChannelsForTest([]*Channel{gcp, lag})
	channels.node = cluster.Config{Enabled: true, NodeID: "control", ControlNode: "control",
		Nodes:    map[string]string{"control": "http://127.0.0.1:1", "gcp": "http://127.0.0.1:2", "lag": "http://127.0.0.1:3"},
		Channels: map[int]cluster.ChannelBinding{gcp.ID: {Owner: "gcp", Identity: "gcp"}, lag.ID: {Owner: "lag", Identity: "lag"}},
	}.WithRuntime()
	db.Model.Create().SetModelID("m").SetName("M").SetType(model.TypeChat).SetStatus(model.StatusEnabled).SetDeveloper("synthetic").SetIcon("OpenAI").SetGroup("catalog").
		SetModelCard(&objects.ModelCard{}).SetSettings(&objects.ModelSettings{Associations: []*objects.ModelAssociation{
			{Type: "channel_model", Priority: 10, ChannelModel: &objects.ChannelModelAssociation{ChannelID: lag.ID, ModelID: "m"}},
			{Type: "channel_model", Priority: 0, ChannelModel: &objects.ChannelModelAssociation{ChannelID: gcp.ID, ModelID: "m"}},
		}}).SaveX(ctx)
	db.Model.Create().SetModelID("when-model").SetName("WhenModel").SetType(model.TypeChat).SetStatus(model.StatusEnabled).SetDeveloper("synthetic").SetIcon("OpenAI").SetGroup("catalog").
		SetModelCard(&objects.ModelCard{}).SetSettings(&objects.ModelSettings{Associations: []*objects.ModelAssociation{
			{Type: "channel_model", Priority: 10, When: &objects.ModelAssociationWhen{Enabled: true}, ChannelModel: &objects.ChannelModelAssociation{ChannelID: gcp.ID, ModelID: "when-model"}},
			{Type: "channel_model", Priority: 2, When: &objects.ModelAssociationWhen{Enabled: true}, ChannelModel: &objects.ChannelModelAssociation{ChannelID: lag.ID, ModelID: "when-model"}},
		}}).SaveX(ctx)
	group := db.Project.Create().SetName("static route").SaveX(ctx)
	open := db.APIKey.Create().SetName("open").SetKey("ah-static-open").SetType(apikey.TypeUser).SetProjectID(group.ID).
		SetProfiles(&objects.APIKeyProfiles{ActiveProfile: "open-p", Profiles: []objects.APIKeyProfile{{Name: "open-p", ModelMappings: []objects.ModelMapping{{From: "gpt-5.5", To: "m"}}}}}).SaveX(ctx)
	restricted := db.APIKey.Create().SetName("restricted").SetKey("ah-static-restricted").SetType(apikey.TypeUser).SetProjectID(group.ID).
		SetProfiles(&objects.APIKeyProfiles{ActiveProfile: "p", Profiles: []objects.APIKeyProfile{{Name: "p", ChannelIDs: []int{lag.ID}, ModelIDs: []string{"m", "public-alias"}, ModelMappings: []objects.ModelMapping{{From: "public-alias", To: "m"}}}}}).SaveX(ctx)
	models := NewModelService(ModelServiceParams{Ent: db, SystemService: NewSystemService(SystemServiceParams{Ent: db}), ChannelService: channels})
	load := func(row *ent.APIKey) *ent.APIKey {
		t.Helper()
		return db.APIKey.Query().Where(apikey.ID(row.ID)).WithProject().OnlyX(ctx)
	}
	manifest, err := models.BuildExecutionManifest(ctx)
	require.NoError(t, err)
	require.Contains(t, manifest.Models, ModelExecutionRoute{Model: "m", Owner: "gcp", Owners: []string{"gcp"}, Status: "ready"})
	require.Contains(t, manifest.Models, ModelExecutionRoute{Model: "unconfigured-shared", Owner: "gcp", Owners: []string{"gcp"}, Status: "ready"})
	require.Contains(t, manifest.Models, ModelExecutionRoute{Model: "when-model", Owner: "lag", Owners: []string{"lag"}, Status: "ready"})
	require.Contains(t, manifest.Models, ModelExecutionRoute{Model: "public-alias", Owner: "lag", Owners: []string{"lag"}, Status: "ready"})
	require.Contains(t, manifest.Models, ModelExecutionRoute{Model: "gpt-5.5", Owner: "gcp", Owners: []string{"gcp"}, Status: "ready"})

	openRoute, err := models.ExecutionRouteForKey(ctx, load(open), "m")
	require.NoError(t, err)
	require.Equal(t, ModelExecutionRoute{Model: "m", Owner: "gcp", Owners: []string{"gcp"}, Status: "ready"}, openRoute)
	restrictedRoute, err := models.ExecutionRouteForKey(ctx, load(restricted), "m")
	require.NoError(t, err)
	require.Equal(t, ModelExecutionRoute{Model: "m", Owner: "lag", Owners: []string{"lag"}, Status: "ready"}, restrictedRoute)

	notices, err := models.ModelMachineNotices(ctx, ModelRouteQuery{Focus: map[int]bool{gcp.ID: true, lag.ID: true}})
	require.NoError(t, err)
	require.Equal(t, []ModelMachineNotice{
		{Model: "gpt-5.5", Machines: []string{"gcp", "lag"}, Winner: "gcp"},
		{Model: "m", Machines: []string{"gcp", "lag"}, Winner: "gcp"},
		{Model: "unconfigured-shared", Machines: []string{"gcp", "lag"}, Winner: "gcp"},
		{Model: "when-model", Machines: []string{"gcp", "lag"}, Winner: "lag"},
	}, notices)

	notices, err = models.ModelMachineNotices(ctx, ModelRouteQuery{
		UseDraft:   true,
		Structural: true,
		DraftModel: "synthetic",
		Draft: []*objects.ModelAssociation{
			{Type: "channel_model", Priority: 5, ChannelModel: &objects.ChannelModelAssociation{ChannelID: lag.ID}},
		},
	})
	require.NoError(t, err)
	require.Equal(t, []ModelMachineNotice{
		{Model: "m", Machines: []string{"gcp", "lag"}, Winner: "gcp"},
		{Model: "when-model", Machines: []string{"gcp", "lag"}, Winner: "lag"},
	}, notices)

	notices, err = models.ModelMachineNotices(ctx, ModelRouteQuery{OwnerOverride: map[int]string{gcp.ID: "lag"}, Focus: map[int]bool{gcp.ID: true}})
	require.NoError(t, err)
	require.Empty(t, notices)
}
