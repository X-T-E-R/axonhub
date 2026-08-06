package schema

import (
	"time"

	"entgo.io/contrib/entgql"
	"entgo.io/ent"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
)

// ManagedObservabilityState is a singleton reconciliation record. Admission
// and GC lock this row so pressure hysteresis remains authoritative across
// application instances sharing a database.
type ManagedObservabilityState struct {
	ent.Schema
}

func (ManagedObservabilityState) Annotations() []schema.Annotation {
	return []schema.Annotation{entgql.Skip(entgql.SkipAll)}
}

func (ManagedObservabilityState) Fields() []ent.Field {
	return []ent.Field{
		field.Int("id").Default(1).Immutable(),
		field.Int64("charged_bytes").Default(0).NonNegative(),
		field.Bool("under_pressure").Default(false),
		field.String("last_error").Optional().Default(""),
		field.Time("updated_at").Default(func() time.Time { return time.Now().UTC() }).
			UpdateDefault(func() time.Time { return time.Now().UTC() }),
	}
}
