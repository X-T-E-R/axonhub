package db

import (
	"context"
	"testing"

	"entgo.io/ent"
	"entgo.io/ent/dialect"
	"github.com/stretchr/testify/require"
)

func TestRouterDriverPrimaryAndReplicaRouting(t *testing.T) {
	t.Parallel()

	master := &recordingDriver{dialect: "postgres"}
	replica := &recordingDriver{dialect: "postgres"}
	router := newRouterDriver(master, replica)

	require.Same(t, master, router.PrimaryDriver())
	require.NoError(t, router.Exec(context.Background(), "VACUUM", nil, nil))
	require.Equal(t, 1, master.execCount)
	require.Zero(t, replica.execCount, "maintenance must never execute on the replica")

	queryCtx := ent.NewQueryContext(context.Background(), &ent.QueryContext{})
	require.NoError(t, router.Query(queryCtx, "SELECT 1", nil, nil))
	require.Equal(t, 1, replica.queryCount)
	require.Zero(t, master.queryCount)

	require.NoError(t, router.Query(context.Background(), "SELECT 1", nil, nil))
	require.Equal(t, 1, master.queryCount, "raw queries must use the primary")

	require.NoError(t, router.Query(WithPrimary(queryCtx), "SELECT 1", nil, nil))
	require.Equal(t, 2, master.queryCount, "safety-sensitive Ent reads must be forceable to primary")
	require.Equal(t, 1, replica.queryCount)
}

type recordingDriver struct {
	dialect    string
	execCount  int
	queryCount int
}

func (d *recordingDriver) Dialect() string { return d.dialect }

func (d *recordingDriver) Exec(context.Context, string, any, any) error {
	d.execCount++
	return nil
}

func (d *recordingDriver) Query(context.Context, string, any, any) error {
	d.queryCount++
	return nil
}

func (d *recordingDriver) Tx(context.Context) (dialect.Tx, error) { return d, nil }
func (d *recordingDriver) Commit() error                          { return nil }
func (d *recordingDriver) Rollback() error                        { return nil }
func (d *recordingDriver) Close() error                           { return nil }

var _ dialect.Driver = (*recordingDriver)(nil)
var _ dialect.Tx = (*recordingDriver)(nil)
