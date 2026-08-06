package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ObservabilityPayload stores content-addressed, variable-size diagnostic
// evidence outside the Request and RequestExecution skeleton relations. The
// request_id is the deduplication boundary: payloads are never shared between
// unrelated requests, even when their bytes happen to match.
type ObservabilityPayload struct {
	ent.Schema
}

func (ObservabilityPayload) Annotations() []schema.Annotation {
	return []schema.Annotation{entgql.Skip(entgql.SkipAll)}
}

func (ObservabilityPayload) Mixin() []ent.Mixin { return []ent.Mixin{TimeMixin{}} }

func (ObservabilityPayload) Fields() []ent.Field {
	return []ent.Field{
		field.Int("request_id").Immutable(),
		field.Enum("kind").Values("request_body").Immutable(),
		field.String("sha256").MaxLen(64).Immutable(),
		field.Int64("byte_length").NonNegative().Immutable(),
		// ChargedBytes deliberately includes fixed row/index overhead in
		// addition to the exact payload length.
		field.Int64("charged_bytes").NonNegative().Immutable(),
		field.Bytes("data").Immutable(),
	}
}

func (ObservabilityPayload) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("request_id", "kind", "sha256", "byte_length").
			StorageKey("observability_payloads_by_request_hash_length"),
		index.Fields("created_at", "id").
			StorageKey("observability_payloads_by_created_at_id"),
	}
}

func (ObservabilityPayload) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("request", Request.Type).
			Field("request_id").
			Required().
			Immutable().
			Unique().
			Annotations(entsql.OnDelete(entsql.Cascade)),
		edge.From("request_body_requests", Request.Type).Ref("request_body_payload"),
		edge.From("request_body_executions", RequestExecution.Type).Ref("request_body_payload"),
	}
}
