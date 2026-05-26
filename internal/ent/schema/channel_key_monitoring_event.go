package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/internal/scopes"
)

// ChannelKeyMonitoringEvent stores secret-safe channel-key monitoring history.
type ChannelKeyMonitoringEvent struct {
	ent.Schema
}

func (ChannelKeyMonitoringEvent) Mixin() []ent.Mixin {
	return []ent.Mixin{
		TimeMixin{},
	}
}

func (ChannelKeyMonitoringEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("channel_id", "checked_at").StorageKey("monitoring_events_by_channel_checked"),
		index.Fields("key_id", "checked_at").StorageKey("monitoring_events_by_key_checked"),
		index.Fields("rule_id", "checked_at").StorageKey("monitoring_events_by_rule_checked"),
		index.Fields("success", "checked_at").StorageKey("monitoring_events_by_success_checked"),
		index.Fields("trigger", "checked_at").StorageKey("monitoring_events_by_trigger_checked"),
	}
}

func (ChannelKeyMonitoringEvent) Fields() []ent.Field {
	return []ent.Field{
		field.Int("channel_id").Immutable().Annotations(entgql.OrderField("CHANNEL_ID")),
		field.String("channel_name").Optional().Immutable(),
		field.String("key_id").Optional().Immutable().Annotations(entgql.OrderField("KEY_ID")),
		field.String("masked_key").Optional().Immutable(),
		field.String("rule_id").Optional().Immutable().Annotations(entgql.OrderField("RULE_ID")),
		field.String("rule_name").Optional().Immutable(),
		field.String("trigger").Default("scheduled").Immutable().Annotations(entgql.OrderField("TRIGGER")),
		field.String("source").Optional().Immutable(),
		field.Bool("success").Default(false).Immutable().Annotations(entgql.OrderField("SUCCESS")),
		field.Bool("skipped").Default(false).Immutable().Annotations(entgql.OrderField("SKIPPED")),
		field.Text("reason").Optional().Immutable(),
		field.Int("status_code").Optional().Nillable().Immutable(),
		field.JSON("balance", objects.JSONRawMessage{}).Optional().Immutable(),
		field.String("currency").Optional().Immutable(),
		field.Bool("available").Optional().Nillable().Immutable(),
		field.String("probe").Optional().Immutable(),
		field.String("matched_policy").Optional().Immutable(),
		field.String("action").Optional().Immutable(),
		field.Time("next_check_at").Optional().Nillable().Immutable(),
		field.Int("backoff_attempt").Optional().Immutable(),
		field.Time("checked_at").Immutable().Annotations(entgql.OrderField("CHECKED_AT")),
	}
}

func (ChannelKeyMonitoringEvent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("channel", Channel.Type).
			Ref("monitoring_events").
			Field("channel_id").
			Required().
			Immutable().
			Unique(),
	}
}

func (ChannelKeyMonitoringEvent) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.QueryField(),
		entgql.RelayConnection(),
	}
}

func (ChannelKeyMonitoringEvent) Policy() ent.Policy {
	return scopes.Policy{
		Query: scopes.QueryPolicy{
			scopes.APIKeyScopeQueryRule(scopes.ScopeReadChannels),
			scopes.OwnerRule(),
			scopes.UserReadScopeRule(scopes.ScopeReadChannels),
		},
	}
}
