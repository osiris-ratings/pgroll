// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

// rawMig builds a RawMigration whose Operations is the JSON encoding of the
// given typed operation slice — what `pgroll stamp` sees after parsing a file
// off disk.
func rawMig(t *testing.T, name string, ops migrations.Operations) *migrations.RawMigration {
	t.Helper()
	body, err := json.Marshal(ops)
	require.NoError(t, err)
	return &migrations.RawMigration{Name: name, Operations: body}
}

// execNoInferred runs DDL on a pinned connection with pgroll's
// no_inferred_migrations GUC set, so the inferred-migration event trigger
// doesn't insert phantom rows that would confuse stamp test assertions.
// Mirrors the GUC pgroll's own state connection sets in state.New.
func execNoInferred(t *testing.T, ctx context.Context, db *sql.DB, ddls ...string) {
	t.Helper()
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(ctx, "SET pgroll.no_inferred_migrations TO 'TRUE'")
	require.NoError(t, err)
	for _, ddl := range ddls {
		_, err = conn.ExecContext(ctx, ddl)
		require.NoError(t, err)
	}
}

func TestStampRecordsUnstampedMigrationsWithParentChain(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		raws := []*migrations.RawMigration{
			rawMig(t, "01_create_widgets", migrations.Operations{createTableOp("widgets")}),
			rawMig(t, "02_create_gadgets", migrations.Operations{createTableOp("gadgets")}),
			rawMig(t, "03_create_doodads", migrations.Operations{createTableOp("doodads")}),
		}
		// Live tables already exist (simulating a SQL dump load).
		execNoInferred(
			t, ctx, db,
			"CREATE TABLE widgets(id integer PRIMARY KEY, name varchar(255))",
			"CREATE TABLE gadgets(id integer PRIMARY KEY, name varchar(255))",
			"CREATE TABLE doodads(id integer PRIMARY KEY, name varchar(255))",
		)

		stamped, err := m.Stamp(ctx, raws, roll.MigrationTypePgroll)
		require.NoError(t, err)
		require.Equal(t, []string{"01_create_widgets", "02_create_gadgets", "03_create_doodads"}, stamped)

		type row struct {
			name        string
			parent      *string
			migType     string
			done        bool
			emptySchema bool
		}
		query := `SELECT name, parent, migration_type, done, resulting_schema = '{}'::jsonb
		          FROM pgroll.migrations WHERE schema = $1 ORDER BY created_at`
		rows, err := db.QueryContext(ctx, query, cSchema)
		require.NoError(t, err)
		defer rows.Close()
		var got []row
		for rows.Next() {
			var r row
			require.NoError(t, rows.Scan(&r.name, &r.parent, &r.migType, &r.done, &r.emptySchema))
			got = append(got, r)
		}
		require.Len(t, got, 3)

		// First row: parent NULL (no prior history), middle two: chained off prior name.
		assert.Equal(t, "01_create_widgets", got[0].name)
		assert.Nil(t, got[0].parent)
		assert.Equal(t, "02_create_gadgets", got[1].name)
		require.NotNil(t, got[1].parent)
		assert.Equal(t, "01_create_widgets", *got[1].parent)
		assert.Equal(t, "03_create_doodads", got[2].name)
		require.NotNil(t, got[2].parent)
		assert.Equal(t, "02_create_gadgets", *got[2].parent)

		for _, r := range got {
			assert.True(t, r.done, "row %q should be done", r.name)
			assert.Equal(t, "pgroll", r.migType, "row %q migration_type", r.name)
		}

		// Only the leaf row carries a live schema; intermediates are '{}'.
		assert.True(t, got[0].emptySchema, "row 0 resulting_schema should be empty")
		assert.True(t, got[1].emptySchema, "row 1 resulting_schema should be empty")
		assert.False(t, got[2].emptySchema, "leaf resulting_schema should be live")
	})
}

func TestStampIsIdempotent(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		raws := []*migrations.RawMigration{
			rawMig(t, "01_a", migrations.Operations{createTableOp("a")}),
			rawMig(t, "02_b", migrations.Operations{createTableOp("b")}),
		}
		execNoInferred(
			t, ctx, db,
			"CREATE TABLE a(id integer PRIMARY KEY, name varchar(255))",
			"CREATE TABLE b(id integer PRIMARY KEY, name varchar(255))",
		)

		first, err := m.Stamp(ctx, raws, roll.MigrationTypePgroll)
		require.NoError(t, err)
		require.Len(t, first, 2)

		second, err := m.Stamp(ctx, raws, roll.MigrationTypePgroll)
		require.NoError(t, err)
		assert.Empty(t, second, "second stamp should produce no new rows")

		var count int
		require.NoError(t, db.QueryRowContext(ctx,
			"SELECT count(*) FROM pgroll.migrations WHERE schema = $1", cSchema).Scan(&count))
		assert.Equal(t, 2, count, "row count should be unchanged after second stamp")
	})
}

func TestStampPartialAppendsRespectsExistingChain(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// First, normal pgroll path: start + complete one migration.
		require.NoError(t, m.Start(ctx, &migrations.Migration{
			Name:       "01_seed",
			Operations: migrations.Operations{createTableOp("seed_table")},
		}, backfill.NewConfig()))
		require.NoError(t, m.Complete(ctx))

		// Now stamp a chain whose first entry is the already-applied one.
		raws := []*migrations.RawMigration{
			rawMig(t, "01_seed", migrations.Operations{createTableOp("seed_table")}),
			rawMig(t, "02_more", migrations.Operations{createTableOp("more")}),
		}
		execNoInferred(
			t, ctx, db,
			"CREATE TABLE more(id integer PRIMARY KEY, name varchar(255))",
		)

		stamped, err := m.Stamp(ctx, raws, roll.MigrationTypePgroll)
		require.NoError(t, err)
		assert.Equal(t, []string{"02_more"}, stamped, "only the unstamped entry should be inserted")

		var parent *string
		require.NoError(t, db.QueryRowContext(ctx,
			"SELECT parent FROM pgroll.migrations WHERE schema = $1 AND name = $2",
			cSchema, "02_more").Scan(&parent))
		require.NotNil(t, parent)
		assert.Equal(t, "01_seed", *parent, "stamped row should chain off the live leaf")
	})
}

func TestStampRefusesActiveMigration(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
		ctx := context.Background()

		require.NoError(t, m.Start(ctx, &migrations.Migration{
			Name:       "01_in_flight",
			Operations: migrations.Operations{createTableOp("inflight")},
		}, backfill.NewConfig()))
		// No Complete — leaves an active migration period.

		raws := []*migrations.RawMigration{
			rawMig(t, "02_later", migrations.Operations{createTableOp("later")}),
		}
		_, err := m.Stamp(ctx, raws, roll.MigrationTypePgroll)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "rollback")
	})
}

func TestStampPreservesMigrationBody(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		ops := migrations.Operations{createTableOp("widgets")}
		raws := []*migrations.RawMigration{rawMig(t, "01_widgets", ops)}
		execNoInferred(
			t, ctx, db,
			"CREATE TABLE widgets(id integer PRIMARY KEY, name varchar(255))",
		)

		_, err := m.Stamp(ctx, raws, roll.MigrationTypePgroll)
		require.NoError(t, err)

		var stored []byte
		require.NoError(t, db.QueryRowContext(ctx,
			"SELECT migration FROM pgroll.migrations WHERE schema = $1 AND name = $2",
			cSchema, "01_widgets").Scan(&stored))
		// Body should NOT be empty `{}` — it should contain the operation we passed in.
		assert.Contains(t, string(stored), "create_table",
			"stored migration body should contain the operation, not be the '{}' placeholder")
	})
}

func TestStampMigrationTypeFlag(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		raws := []*migrations.RawMigration{
			rawMig(t, "01_baseline_marker", migrations.Operations{createTableOp("a")}),
		}
		execNoInferred(
			t, ctx, db,
			"CREATE TABLE a(id integer PRIMARY KEY, name varchar(255))",
		)

		_, err := m.Stamp(ctx, raws, roll.MigrationTypeBaseline)
		require.NoError(t, err)

		var migType string
		require.NoError(t, db.QueryRowContext(ctx,
			"SELECT migration_type FROM pgroll.migrations WHERE schema = $1 AND name = $2",
			cSchema, "01_baseline_marker").Scan(&migType))
		assert.Equal(t, "baseline", migType)
	})
}

func TestStampThenMaterializeYieldsQueryableViews(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		const leafName = "01_widgets"

		execNoInferred(
			t, ctx, db,
			"CREATE TABLE widgets(id integer PRIMARY KEY, name varchar(255))",
		)

		raws := []*migrations.RawMigration{
			rawMig(t, leafName, migrations.Operations{createTableOp("widgets")}),
		}
		_, err := m.Stamp(ctx, raws, roll.MigrationTypePgroll)
		require.NoError(t, err)

		// Mirror what `pgroll stamp --materialize` does at the cmd layer.
		sc, err := m.State().ReadSchema(ctx, m.Schema())
		require.NoError(t, err)
		require.NoError(t, m.Materialize(ctx, leafName, sc))

		versionSchema := roll.VersionedSchemaName(cSchema, leafName)
		require.True(t, schemaExists(t, db, versionSchema))
		// versionSchema is computed by VersionedSchemaName from the test's
		// own constants; this is a test-only sanity query.
		//nolint:gosec // G202: schema name comes from test-controlled input.
		_, err = db.ExecContext(ctx, "SELECT id, name FROM \""+versionSchema+"\".widgets")
		require.NoError(t, err, "stamped + materialized leaf should be queryable through the version schema")
	})
}

func TestStampEmptyInputIsNoop(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		stamped, err := m.Stamp(ctx, nil, roll.MigrationTypePgroll)
		require.NoError(t, err)
		assert.Empty(t, stamped)

		var count int
		require.NoError(t, db.QueryRowContext(ctx,
			"SELECT count(*) FROM pgroll.migrations WHERE schema = $1", cSchema).Scan(&count))
		assert.Equal(t, 0, count, "empty stamp must not insert any rows")
	})
}
