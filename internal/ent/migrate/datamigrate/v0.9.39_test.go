package datamigrate_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/enttest"
	"github.com/looplj/axonhub/internal/ent/migrate/datamigrate"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/server/biz"
)

func TestV0_9_39MigratesExposureAndProvenanceIdempotently(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:diagnostics-migration?mode=memory&_fk=1")
	defer client.Close()
	ctx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	p1 := client.Project.Create().SetName("p1").SaveX(ctx)
	p2 := client.Project.Create().SetName("p2").SaveX(ctx)
	g1 := client.APIKeyProfileTemplate.Create().SetProject(p1).SetName("Shared").SetProfile(&objects.APIKeyProfile{Name: "Shared"}).SaveX(ctx)
	g2 := client.APIKeyProfileTemplate.Create().SetProject(p2).SetName("shared").SetProfile(&objects.APIKeyProfile{Name: "shared"}).SaveX(ctx)
	g3 := client.APIKeyProfileTemplate.Create().SetProject(p2).SetName("future-hidden").SetProfile(&objects.APIKeyProfile{Name: "future-hidden"}).SaveX(ctx)
	u := client.User.Create().SetEmail("u@example.com").SetPassword("x").SaveX(ctx)
	legacy := client.APIKey.Create().SetProject(p1).SetUser(u).SetName("legacy").SetKey("legacy-key").SetType(apikey.TypeUser).SaveX(ctx)
	admin := client.APIKey.Create().SetProject(p1).SetUser(u).SetName("admin").SetKey("admin-key").SetType(apikey.TypeServiceAccount).SaveX(ctx)
	policy := biz.RegistrationConfig{SelfServiceEnabled: true, SelfServicePresetNames: []string{"SHARED"}}
	raw, _ := json.Marshal(policy)
	client.System.Create().SetKey(biz.SystemKeyRegistrationPolicy).SetValue(string(raw)).SaveX(ctx)

	m := datamigrate.NewV0_9_39()
	require.NoError(t, m.Migrate(ctx, client))
	require.NoError(t, m.Migrate(ctx, client))
	require.True(t, client.APIKeyProfileTemplate.GetX(ctx, g1.ID).SelfServiceVisible)
	require.True(t, client.APIKeyProfileTemplate.GetX(ctx, g2.ID).SelfServiceVisible)
	require.False(t, client.APIKeyProfileTemplate.GetX(ctx, g3.ID).SelfServiceVisible)
	require.Equal(t, apikey.ProvisioningSourceLegacyUnknown, client.APIKey.GetX(ctx, legacy.ID).ProvisioningSource)
	require.Equal(t, apikey.ProvisioningSourceAdmin, client.APIKey.GetX(ctx, admin.ID).ProvisioningSource)
	require.Equal(t, 3, client.APIKeyProfileTemplateRevision.Query().CountX(ctx))
	stored, err := biz.NewSystemService(biz.SystemServiceParams{}).RegistrationConfig(ctx, biz.RegistrationConfig{})
	require.NoError(t, err)
	require.Equal(t, []string{"SHARED"}, stored.SelfServicePresetNames)
}

func TestV0_9_39WildcardOnlyExposesGroupsPresentAtMigration(t *testing.T) {
	client := enttest.NewEntClient(t, "sqlite3", "file:diagnostics-migration-wildcard?mode=memory&_fk=1")
	defer client.Close()
	ctx := ent.NewContext(authz.WithTestBypass(context.Background()), client)
	p := client.Project.Create().SetName("p").SaveX(ctx)
	existing := client.APIKeyProfileTemplate.Create().SetProject(p).SetName("existing").SetProfile(&objects.APIKeyProfile{Name: "existing"}).SaveX(ctx)
	policy := biz.RegistrationConfig{SelfServiceEnabled: true, SelfServicePresetNames: []string{"*"}}
	raw, _ := json.Marshal(policy)
	client.System.Create().SetKey(biz.SystemKeyRegistrationPolicy).SetValue(string(raw)).SaveX(ctx)

	m := datamigrate.NewV0_9_39()
	require.NoError(t, m.Migrate(ctx, client))
	require.True(t, client.APIKeyProfileTemplate.GetX(ctx, existing.ID).SelfServiceVisible)
	future := client.APIKeyProfileTemplate.Create().SetProject(p).SetName("future").SetProfile(&objects.APIKeyProfile{Name: "future"}).SaveX(ctx)
	require.NoError(t, m.Migrate(ctx, client))
	require.True(t, client.APIKeyProfileTemplate.GetX(ctx, existing.ID).SelfServiceVisible)
	require.False(t, client.APIKeyProfileTemplate.GetX(ctx, future.ID).SelfServiceVisible)
}
