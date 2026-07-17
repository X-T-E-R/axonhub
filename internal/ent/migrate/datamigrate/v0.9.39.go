package datamigrate

import (
	"context"
	"strings"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/system"
	"github.com/looplj/axonhub/internal/server/biz"
)

type V0_9_39 struct{}

const v0_9_39VisibilityMarker = "migration.v0.9.39.access_group_visibility"

func NewV0_9_39() DataMigrator   { return &V0_9_39{} }
func (*V0_9_39) Version() string { return "v0.9.39" }

// Migrate is deliberately idempotent. It preserves every ambiguous user key
// as legacy_unknown and migrates the old global exposure names only onto groups
// that exist at migration time; future groups stay hidden by default.
func (*V0_9_39) Migrate(ctx context.Context, client *ent.Client) error {
	ctx = authz.WithSystemBypass(ctx, "database-migrate-v0.9.39")
	tx, err := client.Tx(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	txClient := tx.Client()
	txCtx := ent.NewTxContext(ctx, tx)
	txCtx = ent.NewContext(txCtx, txClient)

	if _, err := txClient.APIKey.Update().Where(apikey.TypeNEQ(apikey.TypeUser), apikey.ProvisioningSourceEQ(apikey.ProvisioningSourceLegacyUnknown)).
		SetProvisioningSource(apikey.ProvisioningSourceAdmin).SetProfileMode(apikey.ProfileModeSnapshot).ClearAccessGroupID().ClearAccessGroupRevision().Save(txCtx); err != nil {
		return err
	}
	if _, err := txClient.APIKey.Update().Where(apikey.TypeEQ(apikey.TypeUser), apikey.ProvisioningSourceEQ(apikey.ProvisioningSourceLegacyUnknown)).
		SetProfileMode(apikey.ProfileModeSnapshot).ClearAccessGroupID().ClearAccessGroupRevision().Save(txCtx); err != nil {
		return err
	}

	policy, err := biz.NewSystemService(biz.SystemServiceParams{}).RegistrationConfig(txCtx, biz.RegistrationConfig{})
	if err != nil {
		return err
	}
	allowed := map[string]struct{}{}
	wildcard := false
	for _, name := range policy.SelfServicePresetNames {
		n := strings.ToLower(strings.TrimSpace(name))
		if n == "*" {
			wildcard = true
		} else if n != "" {
			allowed[n] = struct{}{}
		}
	}
	groups, err := txClient.APIKeyProfileTemplate.Query().All(txCtx)
	if err != nil {
		return err
	}
	// A durable marker lets an idempotent rerun retain the legacy name/wildcard
	// evidence without applying it to groups created after the first migration.
	visibilityMigrated, err := txClient.System.Query().Where(system.KeyEQ(v0_9_39VisibilityMarker)).Exist(txCtx)
	if err != nil {
		return err
	}
	for _, group := range groups {
		_, named := allowed[strings.ToLower(strings.TrimSpace(group.Name))]
		visible := policy.SelfServiceEnabled && (wildcard || named)
		update := txClient.APIKeyProfileTemplate.UpdateOneID(group.ID).SetRevision(max(group.Revision, 1))
		if !visibilityMigrated && len(policy.SelfServicePresetNames) > 0 {
			update.SetSelfServiceVisible(visible)
		}
		group, err = update.Save(txCtx)
		if err != nil {
			return err
		}
		// Upsert is avoided for cross-database portability; a duplicate means an
		// earlier run already recorded the immutable snapshot.
		_, err = txClient.APIKeyProfileTemplateRevision.Create().SetProjectID(group.ProjectID).SetTemplateID(group.ID).
			SetRevision(group.Revision).SetName(group.Name).SetDescription(group.Description).SetProfile(group.Profile).Save(txCtx)
		if err != nil && !ent.IsConstraintError(err) {
			return err
		}
	}
	if !visibilityMigrated {
		if _, err := txClient.System.Create().SetKey(v0_9_39VisibilityMarker).SetValue("complete").Save(txCtx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}
