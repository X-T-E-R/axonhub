package db

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/require"
)

func TestEnsureSQLiteDSN(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		dialect    string
		dsn        string
		disableWAL bool
		want       string
	}{
		{
			name:    "postgres unchanged",
			dialect: "postgres",
			dsn:     "postgres://localhost/axonhub",
			want:    "postgres://localhost/axonhub",
		},
		{
			name:    "sqlite adds wal and busy timeout",
			dialect: "sqlite3",
			dsn:     "file:axonhub.db?cache=shared&_fk=1",
			want:    "file:axonhub.db?cache=shared&_fk=1&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)",
		},
		{
			name:    "sqlite without query params",
			dialect: "sqlite3",
			dsn:     "file:axonhub.db",
			want:    "file:axonhub.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)",
		},
		{
			name:       "wal disabled still adds busy timeout",
			dialect:    "sqlite3",
			dsn:        "file:axonhub.db",
			disableWAL: true,
			want:       "file:axonhub.db?_pragma=busy_timeout(5000)",
		},
		{
			name:    "existing wal preserved",
			dialect: "sqlite3",
			dsn:     "file:axonhub.db?_pragma=journal_mode(DELETE)",
			want:    "file:axonhub.db?_pragma=journal_mode(DELETE)&_pragma=busy_timeout(5000)",
		},
		{
			name:    "existing busy timeout preserved",
			dialect: "sqlite3",
			dsn:     "file:axonhub.db?_pragma=busy_timeout(10000)",
			want:    "file:axonhub.db?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)",
		},
		{
			name:    "both pragmas preserved",
			dialect: "sqlite3",
			dsn:     "file:axonhub.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)",
			want:    "file:axonhub.db?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ensureSQLiteDSN(tt.dialect, tt.dsn, tt.disableWAL)
			if got != tt.want {
				t.Fatalf("ensureSQLiteDSN() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewEntClientUpgradeAddsRequestExecutionCleanupIndex(t *testing.T) {
	t.Parallel()

	dbPath := filepath.ToSlash(filepath.Join(t.TempDir(), "upgrade-index.db"))
	cfg := Config{
		Dialect:              "sqlite3",
		DSN:                  "file:" + dbPath + "?_fk=0",
		DisableSQLiteAutoWAL: true,
	}

	oldClient := NewEntClient(cfg)
	oldDriver, ok := oldClient.Driver().(*entsql.Driver)
	require.True(t, ok)
	_, err := oldDriver.ExecContext(context.Background(), "DROP INDEX request_executions_by_created_at")
	require.NoError(t, err)
	require.NoError(t, oldClient.Close())

	upgradedClient := NewEntClient(cfg)
	t.Cleanup(func() { _ = upgradedClient.Close() })
	upgradedDriver, ok := upgradedClient.Driver().(*entsql.Driver)
	require.True(t, ok)
	rows, err := upgradedDriver.DB().QueryContext(context.Background(), "PRAGMA index_list('request_executions')")
	require.NoError(t, err)
	defer rows.Close()

	found := false
	for rows.Next() {
		var seq int
		var name string
		var unique int
		var origin string
		var partial int
		require.NoError(t, rows.Scan(&seq, &name, &unique, &origin, &partial))
		if strings.EqualFold(name, "request_executions_by_created_at") {
			found = true
		}
	}
	require.NoError(t, rows.Err())
	require.True(t, found, "startup migration must add the cleanup predicate index to an upgraded schema")
}

func TestNewEntClientUpgradeAddsAPIKeyAllowedIPsColumn(t *testing.T) {
	t.Parallel()

	dbPath := filepath.ToSlash(filepath.Join(t.TempDir(), "upgrade-api-key-allowed-ips.db"))
	cfg := Config{
		Dialect:              "sqlite3",
		DSN:                  "file:" + dbPath + "?_fk=0",
		DisableSQLiteAutoWAL: true,
	}

	oldClient := NewEntClient(cfg)
	oldDriver, ok := oldClient.Driver().(*entsql.Driver)
	require.True(t, ok)
	_, err := oldDriver.ExecContext(context.Background(), "ALTER TABLE api_keys DROP COLUMN allowed_ips")
	require.NoError(t, err)
	require.NoError(t, oldClient.Close())

	upgradedClient := NewEntClient(cfg)
	t.Cleanup(func() { _ = upgradedClient.Close() })
	upgradedDriver, ok := upgradedClient.Driver().(*entsql.Driver)
	require.True(t, ok)
	rows, err := upgradedDriver.DB().QueryContext(context.Background(), "PRAGMA table_info('api_keys')")
	require.NoError(t, err)
	defer rows.Close()

	found := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		require.NoError(t, rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey))
		if strings.EqualFold(name, "allowed_ips") {
			found = true
		}
	}
	require.NoError(t, rows.Err())
	require.True(t, found, "startup auto-migration must add api_keys.allowed_ips to an upgraded schema")
}
