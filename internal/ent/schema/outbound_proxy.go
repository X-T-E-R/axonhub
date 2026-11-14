package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"

	"github.com/looplj/axonhub/internal/scopes"
)

// OutboundProxy stores a managed outbound proxy endpoint. Proxy credentials are
// kept in proxy_url for use by the egress runtime, but service DTOs never return
// that field verbatim.
type OutboundProxy struct {
	ent.Schema
}

func (OutboundProxy) Mixin() []ent.Mixin {
	return []ent.Mixin{TimeMixin{}}
}

func (OutboundProxy) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty().MaxLen(200),
		field.Enum("kind").Values("static", "dynamic_template").Default("static"),
		field.String("proxy_url").NotEmpty().Sensitive(),
		field.String("pool_name").Optional().Default(""),
		field.Enum("status").Values("enabled", "disabled").Default("enabled"),
		field.Int("weight").Default(100),
		field.Int("max_concurrent").Optional().Nillable(),
		field.Int("max_credentials").Optional().Nillable(),
		field.Int("backup_proxy_id").Optional().Nillable(),
		field.Time("expires_at").Optional().Nillable(),
		field.String("remark").Optional().Default(""),
		field.Time("last_tested_at").Optional().Nillable(),
		field.Bool("last_test_success").Optional().Nillable(),
		field.String("last_exit_ip").Optional().Default(""),
		field.String("last_country").Optional().Default(""),
		field.Int("last_latency_ms").Optional().Nillable(),
		field.String("last_test_error").Optional().Default(""),
	}
}

func (OutboundProxy) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("pool_name", "status"),
	}
}

func (OutboundProxy) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("backup_proxy", OutboundProxy.Type).
			Field("backup_proxy_id").
			Unique().
			Annotations(
				entsql.OnDelete(entsql.Restrict),
				entgql.Skip(entgql.SkipAll),
			),
		edge.From("backup_for", OutboundProxy.Type).
			Ref("backup_proxy").
			Annotations(entgql.Skip(entgql.SkipAll)),
	}
}

// Outbound proxies are managed by the backend service instead of Ent-generated
// GraphQL nodes/mutations. Keep the schema in Ent for migrations and privacy,
// while the parent GraphQL layer exposes its own redacted read view.
func (OutboundProxy) Annotations() []schema.Annotation {
	return []schema.Annotation{
		entgql.Skip(entgql.SkipAll),
	}
}

func (OutboundProxy) Policy() ent.Policy {
	return scopes.Policy{
		Query: scopes.QueryPolicy{
			scopes.APIKeyScopeQueryRule(scopes.ScopeReadChannels),
			scopes.OwnerRule(),
			scopes.UserReadScopeRule(scopes.ScopeReadChannels),
		},
		Mutation: scopes.MutationPolicy{
			scopes.OwnerRule(),
			scopes.UserWriteScopeRule(scopes.ScopeWriteChannels),
		},
	}
}
