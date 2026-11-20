// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

// applyExpandOnlyTrain applies a train the way `pgroll migrate` does without
// --complete: every migration but the last is started without a version
// schema and completed inline or deferred, and the last is left active.
func applyExpandOnlyTrain(t *testing.T, mig *roll.Roll, train []*migrations.Migration) {
	t.Helper()
	ctx := context.Background()

	final := train[len(train)-1]
	target := roll.WithTrainTarget(final)
	for _, m := range train[:len(train)-1] {
		require.NoError(t, mig.Start(ctx, m, backfill.NewConfig(), roll.WithoutVersionSchema(), target))
		if m.CompleteMustBeDeferred() {
			require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))
		} else {
			require.NoError(t, mig.Complete(ctx, roll.WithSkipSchemaDrop(), roll.WithTriggersKeptFor(final)))
		}
	}
	require.NoError(t, mig.Start(ctx, final, backfill.NewConfig()))
}

// execAs runs one statement as a pod pinned to the given version schema.
func execAs(t *testing.T, db *sql.DB, versionSchema, stmt string) error {
	t.Helper()
	ctx := context.Background()

	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()

	_, err = conn.ExecContext(ctx, "SET search_path TO "+versionSchema)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, stmt)
	return err
}

func baseMigration(up string) *migrations.Migration {
	return &migrations.Migration{
		Name:       "00_base",
		Operations: migrations.Operations{&migrations.OpRawSQL{Up: up, Down: "SELECT 1"}},
	}
}

func laterIndex(table, column string) *migrations.Migration {
	return &migrations.Migration{
		Name: "09_later",
		Operations: migrations.Operations{
			&migrations.OpCreateIndex{
				Name:    "ix_" + table + "_" + column,
				Table:   table,
				Columns: []migrations.IndexField{{Column: column}},
			},
		},
	}
}

const (
	previousSchema = "public_00_base"
	targetSchema   = "public_09_later"
)

func intPtr(db *sql.DB, t *testing.T, query string) *int {
	t.Helper()
	var v sql.NullInt64
	require.NoError(t, db.QueryRowContext(context.Background(), query).Scan(&v))
	if !v.Valid {
		return nil
	}
	i := int(v.Int64)
	return &i
}

// A drop_column that is not the last migration of its train still fills the
// dropped column for the new revision's writes.
func TestTrainNonFinalDropColumnRunsDownForNewRevision(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		require.NoError(t, mig.Start(ctx, baseMigration(
			`CREATE TABLE u (id integer PRIMARY KEY, x integer, gone integer NOT NULL);
			 INSERT INTO u VALUES (1, 1, 1)`,
		), backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			{
				Name: "01_drop_gone",
				Operations: migrations.Operations{
					&migrations.OpDropColumn{Table: "u", Column: "gone", Down: "0"},
				},
			},
			laterIndex("u", "x"),
		})

		require.NoError(t, execAs(t, db, targetSchema, `INSERT INTO u (id, x) VALUES (2, 2)`),
			"the new revision does not write the dropped column; down must fill it")
		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO u (id, x, gone) VALUES (3, 3, 33)`))
		require.Equal(t, 0, *intPtr(db, t, `SELECT gone FROM public.u WHERE id = 2`))
		require.Equal(t, 33, *intPtr(db, t, `SELECT gone FROM public.u WHERE id = 3`))

		require.NoError(t, mig.Complete(ctx))
		require.Equal(t, 3, *intPtr(db, t, `SELECT count(*) FROM public.u`))
	})
}

// An alter_column that is not the last migration of its train leaves the new
// revision's writes alone and holds them to the new CHECK.
func TestTrainNonFinalAlterColumnKeepsNewRevisionWrites(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		require.NoError(t, mig.Start(ctx, baseMigration(
			`CREATE TABLE u (id integer PRIMARY KEY, v integer);
			 INSERT INTO u VALUES (1, 5)`,
		), backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			{
				Name: "01_check_v",
				Operations: migrations.Operations{
					&migrations.OpAlterColumn{
						Table:  "u",
						Column: "v",
						Check:  &migrations.CheckConstraint{Name: "v_pos", Constraint: "v > 0"},
						Up:     "CASE WHEN v > 0 THEN v ELSE 1 END",
						Down:   "v",
					},
				},
			},
			laterIndex("u", "id"),
		})

		require.Error(t, execAs(t, db, targetSchema, `INSERT INTO u (id, v) VALUES (10, -1)`),
			"the new revision's write must meet the new CHECK")
		require.NoError(t, execAs(t, db, targetSchema, `INSERT INTO u (id, v) VALUES (11, 7)`))
		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO u (id, v) VALUES (12, -3)`),
			"the previous revision is not held to the new CHECK")

		require.NoError(t, mig.Complete(ctx))
		require.Equal(t, 7, *intPtr(db, t, `SELECT v FROM public.u WHERE id = 11`))
		require.Equal(t, 1, *intPtr(db, t, `SELECT v FROM public.u WHERE id = 12`))
	})
}

// An add_column with `up` that is not the last migration of its train keeps
// filling the column for the previous revision's writes until the train
// contracts.
func TestTrainNonFinalAddColumnKeepsBackfillingPreviousRevision(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		require.NoError(t, mig.Start(ctx, baseMigration(
			`CREATE TABLE w (id integer PRIMARY KEY, x integer);
			 INSERT INTO w VALUES (1, 10)`,
		), backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			{
				Name: "01_add_a",
				Operations: migrations.Operations{
					&migrations.OpAddColumn{
						Table: "w",
						Up:    "x + 1",
						Column: migrations.Column{
							Name:     "a",
							Type:     "integer",
							Nullable: true,
							Check:    &migrations.CheckConstraint{Name: "a_set", Constraint: "a IS NOT NULL"},
						},
					},
				},
			},
			laterIndex("w", "a"),
		})

		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO w (id, x) VALUES (2, 20)`),
			"the previous revision cannot write the new column; up must fill it")
		require.NoError(t, execAs(t, db, targetSchema, `INSERT INTO w (id, x, a) VALUES (3, 30, 99)`))
		require.Equal(t, 21, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 2`))
		require.Equal(t, 99, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 3`),
			"the new revision's own value is kept")

		require.NoError(t, mig.Complete(ctx))
		require.Equal(t, 11, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 1`))
		require.Equal(t, 21, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 2`))

		var leftovers int
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT (SELECT count(*) FROM pg_proc WHERE proname LIKE '\_pgroll\_%')
			     + (SELECT count(*) FROM information_schema.columns
			        WHERE table_schema = 'public' AND column_name LIKE '\_pgroll\_%')`).Scan(&leftovers))
		require.Zero(t, leftovers, "the train's contraction removes every trigger and marker column")
	})
}
