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

// APIKeyProfileTemplateRevision is an immutable routing-policy snapshot.
type APIKeyProfileTemplateRevision struct{ ent.Schema }

func (APIKeyProfileTemplateRevision) Mixin() []ent.Mixin { return []ent.Mixin{TimeMixin{}} }

func (APIKeyProfileTemplateRevision) Fields() []ent.Field {
	return []ent.Field{
		field.Int("project_id").Immutable(),
		field.Int("template_id").Immutable(),
		field.Int64("revision").Immutable().Positive(),
		field.String("name").Immutable(),
		field.String("description").Default("").Immutable(),
		field.JSON("profile", &objects.APIKeyProfile{}).Immutable(),
		field.Int("created_by_user_id").Optional().Nillable().Immutable(),
	}
}

func (APIKeyProfileTemplateRevision) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).Ref("api_key_profile_template_revisions").Field("project_id").Required().Immutable().Unique(),
		edge.From("template", APIKeyProfileTemplate.Type).Ref("revisions").Field("template_id").Required().Immutable().Unique(),
	}
}

func (APIKeyProfileTemplateRevision) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("template_id", "revision").Unique(),
		index.Fields("project_id", "template_id"),
	}
}

func (APIKeyProfileTemplateRevision) Annotations() []schema.Annotation {
	return []schema.Annotation{entgql.Skip(entgql.SkipType)}
}

func (APIKeyProfileTemplateRevision) Policy() ent.Policy {
	return scopes.Policy{
		Query: scopes.QueryPolicy{
			scopes.UserProjectScopeReadRule(scopes.ScopeReadAPIKeys),
			scopes.APIKeyProjectScopeReadRule(scopes.ScopeReadAPIKeys),
			scopes.OwnerRule(),
		},
		Mutation: scopes.MutationPolicy{scopes.OwnerRule()},
	}
}
