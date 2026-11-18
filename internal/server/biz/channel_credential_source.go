package biz

import (
	"context"
	"fmt"
	"strings"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/samber/lo"

	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/channel"
	"github.com/looplj/axonhub/internal/objects"
)

func credentialSourceID(settings *objects.ChannelSettings) int {
	if settings == nil {
		return 0
	}
	return max(0, lo.FromPtr(settings.CredentialSourceChannelID))
}

func credentialOwnerID(ch *ent.Channel) int {
	if id := credentialSourceID(ch.Settings); id != 0 {
		return id
	}
	return ch.ID
}

// Serialize changes to a reference and to its source against each other. SQLite
// serializes writes and rejects writes from an obsolete read snapshot.
func (svc *ChannelService) lockCredentialTopology(ctx context.Context, ids ...int) error {
	db := svc.entFromContext(ctx)
	if db.Driver().Dialect() == dialect.SQLite {
		return nil
	}
	_, err := db.Channel.Query().Where(channel.IDIn(ids...)).
		Order(ent.Asc(channel.FieldID)).
		Modify(func(s *entsql.Selector) { s.ForUpdate() }).
		IDs(ctx)
	return err
}

func credentialSourceChannel(ctx context.Context, db *ent.Client, id int) (*ent.Channel, error) {
	source, err := db.Channel.Query().Where(channel.IDEQ(id), channel.DeletedAtEQ(0), channel.StatusNEQ(channel.StatusArchived)).Only(ctx)
	if ent.IsNotFound(err) {
		return nil, fmt.Errorf("credential source channel #%d is missing, deleted, or archived", id)
	}
	if err != nil {
		return nil, fmt.Errorf("read credential source channel #%d: %w", id, err)
	}
	if credentialSourceID(source.Settings) != 0 {
		return nil, fmt.Errorf("credential source channel #%d %s delegates its credentials; select the original source channel instead", id, source.Name)
	}
	return source, nil
}

func (svc *ChannelService) validateCredentialSource(ctx context.Context, id int, settings *objects.ChannelSettings) error {
	sourceID := credentialSourceID(settings)
	if sourceID == 0 {
		return nil
	}
	if sourceID == id {
		return fmt.Errorf("channel #%d cannot reference itself as its credential source", id)
	}
	if err := svc.lockCredentialTopology(ctx, id, sourceID); err != nil {
		return err
	}
	if _, err := credentialSourceChannel(ctx, svc.entFromContext(ctx), sourceID); err != nil {
		return err
	}
	if id == 0 {
		return nil
	}
	if err := svc.requireNoCredentialDependents(ctx, id); err != nil {
		return err
	}
	return svc.validateCredentialPlacement(ctx, id, sourceID)
}

// Shared admission state lives on the execution machine. An unassigned
// channel can be placed with its pool later via the placement preview.
func (svc *ChannelService) validateCredentialPlacement(ctx context.Context, id, sourceID int) error {
	if !svc.node.Enabled {
		return nil
	}
	_, state, err := readChannelPlacement(ctx, svc.entFromContext(ctx))
	if err != nil {
		return err
	}
	node := svc.node.AtPlacement(state.PlacementSnapshot)
	owner := node.Owner(id)
	if owner == "" {
		return nil
	}
	rows, err := svc.entFromContext(ctx).Channel.Query().Where(channel.StatusNEQ(channel.StatusArchived)).
		Select(channel.FieldID, channel.FieldSettings).All(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if credentialOwnerID(row) != sourceID {
			continue
		}
		if peer := node.Owner(row.ID); peer != "" && peer != owner {
			return fmt.Errorf("credential source #%d executes on %q (via channel #%d); move channel #%d there before sharing credentials", sourceID, peer, row.ID, id)
		}
	}
	return nil
}

func (svc *ChannelService) requireNoCredentialDependents(ctx context.Context, ids ...int) error {
	rows, err := svc.entFromContext(ctx).Channel.Query().
		Where(channel.DeletedAtEQ(0), channel.StatusNEQ(channel.StatusArchived)).
		Order(ent.Asc(channel.FieldID)).
		Select(channel.FieldID, channel.FieldName, channel.FieldSettings).All(ctx)
	if err != nil {
		return fmt.Errorf("check credential source dependents: %w", err)
	}
	sources := lo.SliceToMap(ids, func(id int) (int, bool) { return id, true })
	var dependents []string
	for _, row := range rows {
		if id := credentialSourceID(row.Settings); sources[id] {
			dependents = append(dependents, fmt.Sprintf("#%d %s (source #%d)", row.ID, row.Name, id))
		}
	}
	if len(dependents) != 0 {
		return fmt.Errorf("credential source is in use: detach or reassign credentialSourceChannelId on %s first", strings.Join(dependents, ", "))
	}
	return nil
}

func (svc *ChannelService) mutableCredentialChannel(ctx context.Context, id int) (*ent.Channel, error) {
	ch, err := svc.entFromContext(ctx).Channel.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("failed to get channel: %w", err)
	}
	if sourceID := credentialSourceID(ch.Settings); sourceID != 0 {
		return nil, fmt.Errorf("channel #%d shares credentials: edit source channel #%d or clear credentialSourceChannelId first", ch.ID, sourceID)
	}
	return ch, nil
}

func resolveCredentialOwner(ctx context.Context, db *ent.Client, ch *ent.Channel) (*ent.Channel, error) {
	if id := credentialSourceID(ch.Settings); id != 0 {
		return credentialSourceChannel(ctx, db, id)
	}
	return ch, nil
}

// Only credential fields are projected onto the request channel. Never persist
// this copy or substitute the source channel's endpoint, routing or proxy.
func withEffectiveCredentials(ch, source *ent.Channel) *ent.Channel {
	if ch == source {
		return ch
	}
	resolved, settings := *ch, *ch.Settings
	resolved.Credentials, resolved.DisabledAPIKeys = source.Credentials, source.DisabledAPIKeys
	settings.CredentialPolicies = nil
	var health objects.ChannelKeyHealthCheck
	if settings.KeyHealthCheck != nil {
		health = *settings.KeyHealthCheck
		health.KeyMetadata, health.ArchivedKeys = nil, nil
	}
	if source.Settings != nil {
		settings.CredentialPolicies = source.Settings.CredentialPolicies
		if source.Settings.KeyHealthCheck != nil {
			health.KeyMetadata = source.Settings.KeyHealthCheck.KeyMetadata
			health.ArchivedKeys = source.Settings.KeyHealthCheck.ArchivedKeys
		}
	}
	settings.KeyHealthCheck = &health
	resolved.Settings = &settings
	return &resolved
}

func (svc *ChannelService) validateCredentialStatusChange(ctx context.Context, ids []int, status channel.Status) error {
	if err := svc.lockCredentialTopology(ctx, ids...); err != nil {
		return err
	}
	switch status {
	case channel.StatusArchived:
		return svc.requireNoCredentialDependents(ctx, ids...)
	case channel.StatusEnabled, channel.StatusDisabled:
		// Re-activating an archived dependent must revalidate its source. A
		// previously archived dependent did not protect its source from deletion.
		rows, err := svc.entFromContext(ctx).Channel.Query().Where(channel.IDIn(ids...), channel.StatusEQ(channel.StatusArchived)).All(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if err := svc.validateCredentialSource(ctx, row.ID, row.Settings); err != nil {
				return err
			}
		}
	}
	return nil
}

func (svc *ChannelService) buildChannelWithCredentialSource(ctx context.Context, row *ent.Channel, apiKeyOverride ...string) (*Channel, error) {
	source, err := resolveCredentialOwner(ctx, observationPrimaryClient(svc.entFromContext(ctx)), row)
	if err != nil {
		return nil, err
	}
	if len(apiKeyOverride) != 0 && !lo.Contains(source.Credentials.GetAllAPIKeys(), apiKeyOverride[0]) {
		return nil, fmt.Errorf("the provided API key is not configured for channel #%d", row.ID)
	}
	return svc.buildChannelWithOutbounds(withEffectiveCredentials(row, source), apiKeyOverride...)
}

// CredentialOwnerID identifies the pool used by this execution snapshot.
func (c *Channel) CredentialOwnerID() int {
	return credentialOwnerID(c.Channel)
}

// ValidateChannelCredentialSourceTopology checks the final graph after a batch restore.
func ValidateChannelCredentialSourceTopology(ctx context.Context, db *ent.Client) error {
	rows, err := db.Channel.Query().Where(channel.DeletedAtEQ(0), channel.StatusNEQ(channel.StatusArchived)).
		Select(channel.FieldID, channel.FieldName, channel.FieldSettings).All(ctx)
	if err != nil {
		return err
	}
	byID := lo.SliceToMap(rows, func(row *ent.Channel) (int, *ent.Channel) { return row.ID, row })
	for _, row := range rows {
		if sourceID := credentialSourceID(row.Settings); sourceID != 0 {
			source := byID[sourceID]
			if source == nil || credentialSourceID(source.Settings) != 0 {
				return fmt.Errorf("channel #%d %s must reference a non-archived credential owner, got #%d", row.ID, row.Name, sourceID)
			}
		}
	}
	return nil
}
