package schema

import (
	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/looplj/axonhub/internal/objects"
	"github.com/looplj/axonhub/llm"
	"time"
)

// PendingUsage is durable, compact accounting, never diagnostic payload. It has
// no cascading parent edge: losing an asynchronous parent must not erase usage.
type PendingUsage struct{ ent.Schema }

func (PendingUsage) Annotations() []schema.Annotation {
	return []schema.Annotation{entgql.Skip(entgql.SkipAll)}
}
func (PendingUsage) Fields() []ent.Field {
	return []ent.Field{
		field.String("id").MaxLen(36).Immutable(),
		field.String("node_id"),
		field.Int("api_key_id").Immutable(),
		field.Int("project_id").Immutable(),
		field.Int("channel_id").Immutable(),
		field.String("model_id").Immutable(),
		field.String("source").Immutable(),
		field.String("format").Immutable(),
		field.Time("captured_at").Immutable(),
		field.Time("request_created_at").Immutable(),
		field.Int("request_id").Default(0),
		field.String("request_observation_id").Immutable(),
		field.JSON("usage", llm.Usage{}).Immutable(),
		field.Int64("total_tokens").Immutable(),
		field.Float("total_cost").Optional().Nillable().Immutable(),
		field.JSON("cost_items", []objects.CostItem{}).Optional().Immutable(),
		field.String("price_reference_id").Immutable(),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}
func (PendingUsage) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("api_key_id", "captured_at"), index.Fields("node_id", "created_at"),
	}
}
