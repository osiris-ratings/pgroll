// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
	"github.com/xataio/pgroll/pkg/schema"
)

func TestMaterializeCreatesVersionSchemaAndViews(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		const version = "01_create_table"
		versionSchema := roll.VersionedSchemaName(cSchema, version)

		require.NoError(t, m.Start(ctx, &migrations.Migration{
			Name:       version,
			Operations: migrations.Operations{createTableOp("widgets")},
		}, backfill.NewConfig()))
		require.NoError(t, m.Complete(ctx))

		// Reproduce the stuck state: live tables still exist, but the
		// version schema apps connect to is gone.
		_, err := db.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA %q CASCADE", versionSchema))
		require.NoError(t, err)
		require.False(t, schemaExists(t, db, versionSchema))

		sc, err := m.State().ReadSchema(ctx, m.Schema())
		require.NoError(t, err)

		require.NoError(t, m.Materialize(ctx, version, sc))

		require.True(t, schemaExists(t, db, versionSchema))

		// The view inside the version schema should be queryable, proving
		// it was projected over the live table.
		_, err = db.ExecContext(ctx, fmt.Sprintf("SELECT id, name FROM %q.widgets", versionSchema))
		require.NoError(t, err)
	})
}

func TestMaterializeIsIdempotent(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		const version = "01_create_table"
		versionSchema := roll.VersionedSchemaName(cSchema, version)

		require.NoError(t, m.Start(ctx, &migrations.Migration{
			Name:       version,
			Operations: migrations.Operations{createTableOp("widgets")},
		}, backfill.NewConfig()))
		require.NoError(t, m.Complete(ctx))

		_, err := db.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA %q CASCADE", versionSchema))
		require.NoError(t, err)

		sc, err := m.State().ReadSchema(ctx, m.Schema())
		require.NoError(t, err)

		require.NoError(t, m.Materialize(ctx, version, sc))
		require.NoError(t, m.Materialize(ctx, version, sc))

		require.True(t, schemaExists(t, db, versionSchema))
		_, err = db.ExecContext(ctx, fmt.Sprintf("SELECT id, name FROM %q.widgets", versionSchema))
		require.NoError(t, err)
	})
}

func TestMaterializeRejectsOverlongVersionName(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
		ctx := context.Background()

		longName := strings.Repeat("x", migrations.MaxIdentifierLength-len(cSchema))
		require.Greater(t, len(cSchema)+1+len(longName), migrations.MaxIdentifierLength)

		err := m.Materialize(ctx, longName, schema.New())

		var tooLong migrations.VersionSchemaNameTooLongError
		require.ErrorAs(t, err, &tooLong)
		assert.Equal(t, cSchema, tooLong.Schema)
		assert.Equal(t, longName, tooLong.VersionName)
	})
}

func TestMaterializeRejectsEmptyVersion(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
		ctx := context.Background()
		err := m.Materialize(ctx, "", schema.New())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "version name must not be empty")
	})
}

func TestMaterializeSkipsDeletedAndSoftDeletedTables(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		const version = "01_skip_deleted"
		versionSchema := roll.VersionedSchemaName(cSchema, version)

		// Set up two live tables we can introspect.
		_, err := db.ExecContext(ctx, `CREATE TABLE keep_me(id integer PRIMARY KEY)`)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `CREATE TABLE drop_me(id integer PRIMARY KEY)`)
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, `CREATE TABLE _pgroll_del_softie(id integer PRIMARY KEY)`)
		require.NoError(t, err)

		sc, err := m.State().ReadSchema(ctx, m.Schema())
		require.NoError(t, err)

		// Mark drop_me as virtually deleted so ensureViews skips it.
		require.Contains(t, sc.Tables, "drop_me")
		sc.Tables["drop_me"].Deleted = true

		require.NoError(t, m.Materialize(ctx, version, sc))

		require.True(t, schemaExists(t, db, versionSchema))
		assert.True(t, viewExists(t, db, versionSchema, "keep_me"))
		assert.False(t, viewExists(t, db, versionSchema, "drop_me"))
		assert.False(t, viewExists(t, db, versionSchema, "_pgroll_del_softie"))
	})
}

func TestMaterializeDoesNotInsertMigrationsRow(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		const version = "01_just_views"

		_, err := db.ExecContext(ctx, `CREATE TABLE widgets(id integer PRIMARY KEY)`)
		require.NoError(t, err)

		sc, err := m.State().ReadSchema(ctx, m.Schema())
		require.NoError(t, err)

		require.NoError(t, m.Materialize(ctx, version, sc))

		var count int
		require.NoError(t, db.QueryRowContext(ctx,
			"SELECT count(*) FROM pgroll.migrations WHERE schema = $1 AND name = $2",
			cSchema, version).Scan(&count))
		assert.Equal(t, 0, count, "materialize must not record a migrations row")
	})
}

func viewExists(t *testing.T, db *sql.DB, schemaName, viewName string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM pg_catalog.pg_views
			WHERE schemaname = $1 AND viewname = $2
		)`, schemaName, viewName).Scan(&exists)
	require.NoError(t, err)
	return exists
}
