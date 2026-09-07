package schema

import (
	"context"
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
		field.Int64("ledger_revision").Default(0).NonNegative(),
		field.Bool("under_pressure").Default(false),
		field.String("last_error").Optional().Default(""),
		field.Time("updated_at").Default(func() time.Time { return time.Now().UTC() }).
			UpdateDefault(func() time.Time { return time.Now().UTC() }),
	}
}

// Hooks gives charged-byte updates an atomic, clock-independent version.
// Charged-byte changes use Update/UpdateOne; create-on-conflict paths are
// restricted to initialization or pressure/error metadata, never charge deltas.
func (ManagedObservabilityState) Hooks() []ent.Hook {
	return []ent.Hook{func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, mutation ent.Mutation) (ent.Value, error) {
			_, set := mutation.Field("charged_bytes")
			_, added := mutation.AddedField("charged_bytes")
			if mutation.Op().Is(ent.OpUpdate|ent.OpUpdateOne) && (set || added) {
				if err := mutation.AddField("ledger_revision", int64(1)); err != nil {
					return nil, err
				}
			}
			return next.Mutate(ctx, mutation)
		})
	}}
}
