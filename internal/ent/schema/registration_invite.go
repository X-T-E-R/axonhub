package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"

	"github.com/looplj/axonhub/internal/scopes"
)

// RegistrationInvite is a finite-use admission to one portal group. Secrets are
// never exposed through the generic Ent GraphQL node or mutation surface.
type RegistrationInvite struct{ ent.Schema }

func (RegistrationInvite) Mixin() []ent.Mixin { return []ent.Mixin{TimeMixin{}} }
func (RegistrationInvite) Fields() []ent.Field {
	return []ent.Field{
		field.String("digest").Unique().Sensitive().Immutable().MinLen(64).MaxLen(64),
		field.Int("project_id").Immutable(),
		field.Int("created_by_id").Immutable(),
		field.Int("max_uses").Positive().Immutable(),
		field.Int("used_uses").Default(0).NonNegative(),
		field.Time("expires_at").Optional().Nillable().Immutable(),
		field.Bool("revoked").Default(false),
	}
}
func (RegistrationInvite) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("project", Project.Type).Field("project_id").Required().Unique().Immutable().Annotations(entsql.OnDelete(entsql.Cascade)),
	}
}
func (RegistrationInvite) Annotations() []schema.Annotation {
	return []schema.Annotation{entgql.Skip()}
}
func (RegistrationInvite) Policy() ent.Policy {
	return scopes.Policy{
		Query:    scopes.QueryPolicy{scopes.OwnerRule(), scopes.UserReadScopeRule(scopes.ScopeWriteUsers)},
		Mutation: scopes.MutationPolicy{scopes.OwnerRule(), scopes.UserWriteScopeRule(scopes.ScopeWriteUsers)},
	}
}
