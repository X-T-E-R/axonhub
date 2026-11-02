package ent_test

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	entschema "entgo.io/ent/dialect/sql/schema"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/looplj/axonhub/internal/authz"
	"github.com/looplj/axonhub/internal/ent"
	"github.com/looplj/axonhub/internal/ent/apikey"
	"github.com/looplj/axonhub/internal/ent/migrate"
	"github.com/looplj/axonhub/internal/ent/migrate/schemahook"
	_ "github.com/looplj/axonhub/internal/ent/runtime"
	"github.com/looplj/axonhub/internal/ent/user"
	_ "github.com/looplj/axonhub/internal/pkg/sqlite"
)

func TestPortalMigrationFromLegacySchema(t *testing.T) {
	client, db := newPortalMigrationClient(t)
	ctx := authz.WithTestBypass(context.Background())

	legacyTables, err := entschema.CopyTables(migrate.Tables)
	require.NoError(t, err)
	removeSchemaColumn(t, legacyTables, user.Table, user.FieldAccessMode)
	removeSchemaColumn(t, legacyTables, apikey.Table, apikey.FieldPortalManaged)
	require.NoError(t, createPortalSchema(ctx, client, legacyTables))

	hasColumn, err := sqliteColumnExists(ctx, db, user.Table, user.FieldAccessMode)
	require.NoError(t, err)
	require.False(t, hasColumn)
	hasColumn, err = sqliteColumnExists(ctx, db, apikey.Table, apikey.FieldPortalManaged)
	require.NoError(t, err)
	require.False(t, hasColumn)

	const (
		legacyProjectID = 201
		legacyUserID    = 202
		legacyKeyID     = 203
	)
	_, err = db.ExecContext(ctx,
		"INSERT INTO projects (id, name, description, status) VALUES (?, ?, ?, ?)",
		legacyProjectID, "Legacy project", "created before portal", "active")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		"INSERT INTO users (id, email, password) VALUES (?, ?, ?)",
		legacyUserID, "legacy@example.com", "legacy-password")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		"INSERT INTO api_keys (id, user_id, project_id, key, name) VALUES (?, ?, ?, ?, ?)",
		legacyKeyID, legacyUserID, legacyProjectID, "legacy-api-key", "Legacy key")
	require.NoError(t, err)

	currentTables, err := entschema.CopyTables(migrate.Tables)
	require.NoError(t, err)
	require.NoError(t, createPortalSchema(ctx, client, currentTables))

	hasColumn, err = sqliteColumnExists(ctx, db, user.Table, user.FieldAccessMode)
	require.NoError(t, err)
	require.True(t, hasColumn)
	hasColumn, err = sqliteColumnExists(ctx, db, apikey.Table, apikey.FieldPortalManaged)
	require.NoError(t, err)
	require.True(t, hasColumn)

	migratedProject, err := client.Project.Get(ctx, legacyProjectID)
	require.NoError(t, err)
	assert.Equal(t, legacyProjectID, migratedProject.ID)
	assert.Equal(t, "Legacy project", migratedProject.Name)

	migratedUser, err := client.User.Get(ctx, legacyUserID)
	require.NoError(t, err)
	assert.Equal(t, user.AccessModeLegacy, migratedUser.AccessMode)
	assert.Equal(t, legacyUserID, migratedUser.ID)

	migratedKey, err := client.APIKey.Get(ctx, legacyKeyID)
	require.NoError(t, err)
	assert.False(t, migratedKey.PortalManaged)
	assert.Equal(t, legacyKeyID, migratedKey.ID)
	assert.Equal(t, "legacy-api-key", migratedKey.Key)
	assert.Equal(t, legacyProjectID, migratedKey.ProjectID)
	assert.Equal(t, legacyUserID, migratedKey.UserID)

	updatedUser, err := client.User.UpdateOneID(legacyUserID).
		SetAccessMode(user.AccessModePortal).
		Save(ctx)
	require.NoError(t, err)
	assert.Equal(t, user.AccessModePortal, updatedUser.AccessMode)

	newKey, err := client.APIKey.Create().
		SetUserID(legacyUserID).
		SetProjectID(legacyProjectID).
		SetKey("portal-api-key").
		SetName("Portal key").
		SetPortalManaged(true).
		Save(ctx)
	require.NoError(t, err)
	assert.True(t, newKey.PortalManaged)
	assert.Equal(t, legacyProjectID, newKey.ProjectID)
	assert.Equal(t, "portal-api-key", newKey.Key)
}

func TestPortalMigrationSchemaDefaults(t *testing.T) {
	client, db := newPortalMigrationClient(t)
	ctx := authz.WithTestBypass(context.Background())

	currentTables, err := entschema.CopyTables(migrate.Tables)
	require.NoError(t, err)
	require.NoError(t, createPortalSchema(ctx, client, currentTables))

	const (
		projectID = 301
		userID    = 302
		keyID     = 303
	)
	_, err = db.ExecContext(ctx,
		"INSERT INTO projects (id, name, description, status) VALUES (?, ?, ?, ?)",
		projectID, "New project", "created with current schema", "active")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		"INSERT INTO users (id, email, password) VALUES (?, ?, ?)",
		userID, "new@example.com", "new-password")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		"INSERT INTO api_keys (id, user_id, project_id, key, name) VALUES (?, ?, ?, ?, ?)",
		keyID, userID, projectID, "new-api-key", "New key")
	require.NoError(t, err)

	createdUser, err := client.User.Get(ctx, userID)
	require.NoError(t, err)
	assert.Equal(t, user.AccessModeLegacy, createdUser.AccessMode)

	createdKey, err := client.APIKey.Get(ctx, keyID)
	require.NoError(t, err)
	assert.False(t, createdKey.PortalManaged)
	assert.Equal(t, projectID, createdKey.ProjectID)
}

func newPortalMigrationClient(t *testing.T) (*ent.Client, *sql.DB) {
	t.Helper()

	dbPath := filepath.ToSlash(filepath.Join(t.TempDir(), "portal-migration.sqlite"))
	dsn := "file:" + dbPath + "?_fk=0"
	db, err := sql.Open("sqlite3", dsn)
	require.NoError(t, err)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	client := ent.NewClient(ent.Driver(entsql.OpenDB(dialect.SQLite, db)))
	t.Cleanup(func() {
		_ = client.Close()
	})
	return client, db
}

func createPortalSchema(ctx context.Context, client *ent.Client, tables []*entschema.Table) error {
	return migrate.Create(ctx, client.Schema, tables, portalMigrationOptions()...)
}

func portalMigrationOptions() []entschema.MigrateOption {
	return []entschema.MigrateOption{
		migrate.WithGlobalUniqueID(false),
		migrate.WithForeignKeys(false),
		migrate.WithDropIndex(true),
		migrate.WithDropColumn(true),
		entschema.WithHooks(schemahook.V0_3_0),
	}
}

func removeSchemaColumn(t *testing.T, tables []*entschema.Table, tableName, columnName string) {
	t.Helper()

	for _, table := range tables {
		if table.Name != tableName {
			continue
		}

		columns := table.Columns[:0]
		for _, column := range table.Columns {
			if column.Name != columnName {
				columns = append(columns, column)
			}
		}
		table.Columns = columns

		indexes := table.Indexes[:0]
		for _, index := range table.Indexes {
			containsRemovedColumn := false
			for _, column := range index.Columns {
				if column.Name == columnName {
					containsRemovedColumn = true
					break
				}
			}
			if !containsRemovedColumn {
				indexes = append(indexes, index)
			}
		}
		table.Indexes = indexes
		return
	}

	t.Fatalf("table %q not found in copied schema", tableName)
}

func sqliteColumnExists(ctx context.Context, db *sql.DB, tableName, columnName string) (bool, error) {
	rows, err := db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info('%s')", tableName))
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid          int
			name         string
			columnType   string
			notNull      int
			defaultValue any
			primaryKey   int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == columnName {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}
