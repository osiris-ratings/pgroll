// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

// These tests lock in that every non-idempotent DDL action used on a `complete`
// path can be re-run safely. `complete` is not transactional and flips a
// migration's `done` flag only after all of its DDL has been applied, so an
// interruption between the DDL committing and the flag flipping leaves a
// migration whose physical schema is fully migrated but is still recorded as
// in-progress. Re-running `complete` then replays these actions against the
// already-migrated schema; they must no-op rather than error.

func TestRenameColumnActionIsIdempotent(t *testing.T) {
	t.Parallel()

	testutils.WithConnectionToContainer(t, func(conn *sql.DB, _ string) {
		ctx := context.Background()
		rdb := &db.RDB{DB: conn}

		_, err := conn.ExecContext(ctx, `CREATE TABLE items (id int, a text)`)
		require.NoError(t, err)

		action := migrations.NewRenameColumnAction(rdb, "items", "a", "b")

		// First run performs the rename.
		require.NoError(t, action.Execute(ctx))
		// Second run is a no-op: the source column is gone and the target
		// already exists (the interrupted-complete case).
		require.NoError(t, action.Execute(ctx))

		assert.Equal(t, 1, countColumns(t, conn, "items", "b"))
		assert.Equal(t, 0, countColumns(t, conn, "items", "a"))
	})
}

func TestRenameColumnActionErrorsWhenNeitherColumnExists(t *testing.T) {
	t.Parallel()

	testutils.WithConnectionToContainer(t, func(conn *sql.DB, _ string) {
		ctx := context.Background()
		rdb := &db.RDB{DB: conn}

		_, err := conn.ExecContext(ctx, `CREATE TABLE items (id int)`)
		require.NoError(t, err)

		// The table exists but neither the source nor target column does: this
		// is a genuine inconsistency, not an already-applied rename. The action
		// falls through to the rename and PostgreSQL surfaces the error rather
		// than the change silently no-op'ing.
		action := migrations.NewRenameColumnAction(rdb, "items", "missing_from", "missing_to")
		err = action.Execute(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "does not exist")
	})
}

func TestRenameColumnActionNoOpsOnMissingTable(t *testing.T) {
	t.Parallel()

	testutils.WithConnectionToContainer(t, func(conn *sql.DB, _ string) {
		ctx := context.Background()
		rdb := &db.RDB{DB: conn}

		// No table at all: preserve the existing ALTER TABLE IF EXISTS no-op.
		action := migrations.NewRenameColumnAction(rdb, "does_not_exist", "a", "b")
		require.NoError(t, action.Execute(ctx))
	})
}

func TestRenameTableActionIsIdempotent(t *testing.T) {
	t.Parallel()

	testutils.WithConnectionToContainer(t, func(conn *sql.DB, _ string) {
		ctx := context.Background()
		rdb := &db.RDB{DB: conn}

		_, err := conn.ExecContext(ctx, `CREATE TABLE t1 (id int)`)
		require.NoError(t, err)

		action := migrations.NewRenameTableAction(rdb, "t1", "t2")
		require.NoError(t, action.Execute(ctx))
		// t1 is gone, t2 exists: re-run must no-op.
		require.NoError(t, action.Execute(ctx))

		assert.True(t, relExists(t, conn, "t2"))
		assert.False(t, relExists(t, conn, "t1"))
	})
}

func TestRenameConstraintActionIsIdempotent(t *testing.T) {
	t.Parallel()

	testutils.WithConnectionToContainer(t, func(conn *sql.DB, _ string) {
		ctx := context.Background()
		rdb := &db.RDB{DB: conn}

		_, err := conn.ExecContext(ctx, `CREATE TABLE c1 (id int, CONSTRAINT chk_old CHECK (id > 0))`)
		require.NoError(t, err)

		action := migrations.NewRenameConstraintAction(rdb, "c1", "chk_old", "chk_new")
		require.NoError(t, action.Execute(ctx))
		// chk_old is gone, chk_new exists: re-run must no-op.
		require.NoError(t, action.Execute(ctx))

		assert.True(t, constraintExists(t, conn, "c1", "chk_new"))
		assert.False(t, constraintExists(t, conn, "c1", "chk_old"))
	})
}

func TestAddConstraintUsingUniqueIndexActionIsIdempotent(t *testing.T) {
	t.Parallel()

	testutils.WithConnectionToContainer(t, func(conn *sql.DB, _ string) {
		ctx := context.Background()
		rdb := &db.RDB{DB: conn}

		_, err := conn.ExecContext(ctx, `CREATE TABLE u1 (id int)`)
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, `CREATE UNIQUE INDEX u1_idx ON u1 (id)`)
		require.NoError(t, err)

		action := migrations.NewAddConstraintUsingUniqueIndex(rdb, "u1", "u1_uniq", "u1_idx")
		require.NoError(t, action.Execute(ctx))
		// Constraint already present: re-run must no-op.
		require.NoError(t, action.Execute(ctx))

		assert.True(t, constraintExists(t, conn, "u1", "u1_uniq"))
	})
}

func TestAddPrimaryKeyActionIsIdempotent(t *testing.T) {
	t.Parallel()

	testutils.WithConnectionToContainer(t, func(conn *sql.DB, _ string) {
		ctx := context.Background()
		rdb := &db.RDB{DB: conn}

		_, err := conn.ExecContext(ctx, `CREATE TABLE p1 (id int NOT NULL)`)
		require.NoError(t, err)
		_, err = conn.ExecContext(ctx, `CREATE UNIQUE INDEX p1_idx ON p1 (id)`)
		require.NoError(t, err)

		action := migrations.NewAddPrimaryKeyAction(rdb, "p1", "p1_idx")
		require.NoError(t, action.Execute(ctx))
		// Primary key already present: re-run must no-op.
		require.NoError(t, action.Execute(ctx))
	})
}

// TestCompleteIsReRunnableAfterInterruptedRename reproduces the production
// incident: an `add_column` migration whose `complete` applied the temp-column
// rename but was interrupted before flipping `done`. Re-running `complete` must
// recover rather than fail on the already-applied rename.
func TestCompleteIsReRunnableAfterInterruptedRename(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, conn *sql.DB) {
		ctx := context.Background()
		cfg := backfill.NewConfig()

		createTable := &migrations.Migration{
			Name:          "01_create_table",
			VersionSchema: "create_table",
			Operations: migrations.Operations{
				&migrations.OpCreateTable{
					Name: "items",
					Columns: []migrations.Column{
						{Name: "id", Type: "serial", Pk: true},
					},
				},
			},
		}
		require.NoError(t, mig.Start(ctx, createTable, cfg))
		require.NoError(t, mig.Complete(ctx))

		addColumn := &migrations.Migration{
			Name:          "02_add_source",
			VersionSchema: "add_source",
			Operations: migrations.Operations{
				&migrations.OpAddColumn{
					Table: "items",
					Column: migrations.Column{
						Name:     "source",
						Type:     "varchar(32)",
						Nullable: true,
					},
				},
			},
		}
		require.NoError(t, mig.Start(ctx, addColumn, cfg))

		// Simulate the interrupted complete: its first action — renaming the
		// physical temp column to the final name — committed, but the process
		// died before `done` was flipped. The temp name is derived exactly as
		// pgroll derives it.
		scope := migrations.MigrationScopeFor(addColumn.Name)
		temp := migrations.TemporaryName(scope, "source")
		_, err := conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE public.items RENAME COLUMN %s TO %s",
			pq.QuoteIdentifier(temp), pq.QuoteIdentifier("source")))
		require.NoError(t, err)

		// Re-running complete must succeed despite the rename already being done.
		require.NoError(t, mig.Complete(ctx))

		assert.Equal(t, 1, countColumns(t, conn, "items", "source"))
	})
}

func countColumns(t *testing.T, conn *sql.DB, table, column string) int {
	t.Helper()
	var n int
	require.NoError(t, conn.QueryRow(
		`SELECT count(*) FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`,
		table, column,
	).Scan(&n))
	return n
}

func relExists(t *testing.T, conn *sql.DB, rel string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, conn.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, rel).Scan(&exists))
	return exists
}

func constraintExists(t *testing.T, conn *sql.DB, table, constraint string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, conn.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid = to_regclass($1) AND conname = $2)`,
		table, constraint,
	).Scan(&exists))
	return exists
}
