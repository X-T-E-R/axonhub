package biz

import (
	"testing"

	"github.com/google/uuid"
	"github.com/samber/lo"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/contexts"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/model"
	"github.com/looplj/axonhub/internal/ent/user"
	"github.com/looplj/axonhub/internal/objects"
)

func TestPortalMappedCatalogFormatsPricesAndFallback(t *testing.T) {
	db := enttest.NewEntClient(t, "sqlite3", "file:catalog-metadata-"+uuid.NewString()+"?mode=memory&_fk=1")
	defer db.Close()
	ctx := ent.NewContext(authz.WithTestBypass(t.Context()), db)
	u := db.User.Create().SetEmail("catalog-metadata@example.test").SetPassword("synthetic").SetAccessMode(user.AccessModePortal).SaveX(ctx)
	makeChannel := func(name string, kind channel.Type) *Channel {
		ch := db.Channel.Create().SetName(name).SetType(kind).SetBaseURL("https://unused.example.test").SetCredentials(objects.ChannelCredentials{APIKey: "synthetic"}).
			SetSupportedModels([]string{"target", "forbidden"}).SetDefaultTestModel("target").SaveX(ctx)
		return &Channel{Channel: ch}
	}
	a, b := makeChannel("responses", channel.TypeOpenaiResponses), makeChannel("chat", channel.TypeGeminiOpenai)
	price := func(input, output int64) *ent.ChannelModelPrice {
		return &ent.ChannelModelPrice{Price: objects.ModelPrice{Items: []objects.ModelPriceItem{
			{ItemCode: objects.PriceItemCodeUsage, Pricing: objects.Pricing{Mode: objects.PricingModeUsagePerUnit, UsagePerUnit: lo.ToPtr(decimal.NewFromInt(input))}},
			{ItemCode: objects.PriceItemCodeCompletion, Pricing: objects.Pricing{Mode: objects.PricingModeUsagePerUnit, UsagePerUnit: lo.ToPtr(decimal.NewFromInt(output))}},
		}}}
	}
	a.cachedModelPrices = map[string]*ent.ChannelModelPrice{"target": price(1, 2), "forbidden": price(42, 42)}
	b.cachedModelPrices = map[string]*ent.ChannelModelPrice{"target": price(1, 2), "forbidden": nil}
	channels := NewChannelServiceForTest(db)
	defer channels.Stop()
	channels.SetEnabledChannelsForTest([]*Channel{a, b})
	ids := []string{"alias", "target"}
	profile := func(channelIDs []int) *objects.ProjectProfiles {
		return &objects.ProjectProfiles{ActiveProfile: "p", Profiles: []objects.ProjectProfile{{Name: "p", ModelIDs: &ids, ChannelIDs: channelIDs}}}
	}
	g := db.Project.Create().SetName("Metadata group").SetProfiles(profile([]int{a.ID, b.ID})).SaveX(ctx)
	db.UserProject.Create().SetUserID(u.ID).SetProjectID(g.ID).SaveX(ctx)
	key := db.APIKey.Create().SetName("Mapped key").SetKey("ah-mapped-metadata").SetType(apikey.TypePersonal).SetUserID(u.ID).SetProjectID(g.ID).SetPortalManaged(true).
		SetProfiles(&objects.APIKeyProfiles{ActiveProfile: "p", Profiles: []objects.APIKeyProfile{{Name: "p", ModelIDs: []string{"alias"}, ModelMappings: []objects.ModelMapping{{From: "alias", To: "target"}}}}}).SaveX(ctx)
	// The alias need not have a Model row. Its finite key/group authorization
	// makes it discoverable; capabilities and eligible routes come from target.
	target := db.Model.Create().SetModelID("target").SetName("Target").SetType(model.TypeChat).SetStatus(model.StatusEnabled).SetDeveloper("synthetic").SetIcon("OpenAI").SetGroup("catalog").
		SetModelCard(&objects.ModelCard{Vision: true, Limit: objects.ModelCardLimit{Context: 1024}}).
		SetSettings(&objects.ModelSettings{Associations: []*objects.ModelAssociation{
			{Type: "channel_regex", ChannelRegex: &objects.ChannelRegexAssociation{ChannelID: a.ID, Pattern: "^(target|forbidden)$"}},
			{Type: "channel_model", ChannelModel: &objects.ChannelModelAssociation{ChannelID: b.ID, ModelID: "target"}},
		}}).SaveX(ctx)
	system := NewSystemService(SystemServiceParams{Ent: db})
	settings := SystemModelSettings{QueryAllChannelModels: true, FallbackToChannelsOnModelNotFound: true}
	require.NoError(t, system.SetModelSettings(ctx, settings))
	models := NewModelService(ModelServiceParams{Ent: db, SystemService: system, ChannelService: channels})
	portal := NewPortalService(PortalServiceParams{Ent: db, Models: models})
	catalog := func() []*objects.PortalModel {
		t.Helper()
		current, err := RefreshAPIKeyAuthorization(ctx, db, key.Key)
		require.NoError(t, err)
		facades, err := models.ListEnabledModels(contexts.WithAPIKey(ctx, current))
		require.NoError(t, err)
		dtos, err := portal.Models(contexts.WithUser(ctx, u), g.ID, &key.ID, nil, nil)
		require.NoError(t, err)
		require.Equal(t, lo.Map(facades, func(m ModelFacade, _ int) string { return m.ID }), lo.Map(dtos, func(m *objects.PortalModel, _ int) string { return m.ID }))
		return dtos
	}
	dtos := catalog()
	require.Len(t, dtos, 1)
	require.Equal(t, "alias", dtos[0].ID)
	require.Equal(t, "chat", dtos[0].Type)
	require.True(t, dtos[0].Vision)
	require.EqualValues(t, 1024, *dtos[0].ContextLength)
	require.Equal(t, []string{"openai/chat_completions", "openai/responses"}, dtos[0].APIFormats)
	require.Equal(t, float64(1), *dtos[0].InputPrice, "a forbidden association entry must not poison price selection")
	require.Equal(t, float64(2), *dtos[0].OutputPrice)
	// Only currently authorized target candidates determine formats and prices.
	db.Project.UpdateOne(g).SetProfiles(profile([]int{a.ID})).SaveX(ctx)
	dtos = catalog()
	require.Len(t, dtos, 1)
	require.Equal(t, []string{"openai/responses"}, dtos[0].APIFormats)
	require.Equal(t, float64(1), *dtos[0].InputPrice)
	db.Project.UpdateOne(g).SetProfiles(profile([]int{a.ID, b.ID})).SaveX(ctx)
	b.cachedModelPrices["target"] = price(3, 4)
	dtos = catalog()
	require.Len(t, dtos, 1)
	require.Nil(t, dtos[0].InputPrice, "different eligible prices remain unknown, not the alias's invented price")
	require.Nil(t, dtos[0].OutputPrice)
	b.cachedModelPrices["target"] = nil
	dtos = catalog()
	require.Nil(t, dtos[0].InputPrice, "a missing eligible route price remains unknown")
	// Disabled/missing configured targets follow execution's channel fallback
	// setting. No independent alias route may keep a non-callable alias visible.
	db.Model.UpdateOne(target).SetStatus(model.StatusDisabled).SaveX(ctx)
	require.Len(t, catalog(), 1)
	settings.FallbackToChannelsOnModelNotFound = false
	require.NoError(t, system.SetModelSettings(ctx, settings))
	require.Empty(t, catalog())
	settings.FallbackToChannelsOnModelNotFound = true
	settings.QueryAllChannelModels = false
	require.NoError(t, system.SetModelSettings(ctx, settings))
	require.Empty(t, catalog(), "configured-only discovery must not invent an unconfigured alias")
	db.Model.Create().SetModelID("alias").SetName("Alias").SetType(model.TypeChat).SetStatus(model.StatusEnabled).SetDeveloper("synthetic").SetIcon("OpenAI").SetGroup("catalog").
		SetModelCard(&objects.ModelCard{}).SetSettings(&objects.ModelSettings{}).SaveX(ctx)
	require.Len(t, catalog(), 1, "configured alias can resolve a channel fallback target")
}
