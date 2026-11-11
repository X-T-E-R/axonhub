package schema

import (
	"time"

	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// CompactUsageBatch records idempotent ingestion receipts for sealed compact
// accounting batches uploaded by non-control data nodes.
type CompactUsageBatch struct{ ent.Schema }

func (CompactUsageBatch) Annotations() []schema.Annotation {
	return []schema.Annotation{entgql.Skip(entgql.SkipAll)}
}

func (CompactUsageBatch) Fields() []ent.Field {
	return []ent.Field{
		field.String("producer_uuid").MaxLen(64).Immutable(),
		field.String("node_id").MaxLen(128).Immutable(),
		field.Int64("seq_start").Immutable(),
		field.Int64("seq_end").Immutable(),
		field.Int("event_count").Immutable(),
		field.String("payload_digest").MaxLen(64).Immutable(),
		field.Int("schema_version").Default(1).Immutable(),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (CompactUsageBatch) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("producer_uuid", "seq_start", "seq_end").
			StorageKey("compact_usage_batches_by_producer_range").
			Unique(),
		index.Fields("producer_uuid", "seq_end").
			StorageKey("compact_usage_batches_by_producer_seq_end"),
		index.Fields("node_id", "created_at").
			StorageKey("compact_usage_batches_by_node_created_at"),
	}
}
