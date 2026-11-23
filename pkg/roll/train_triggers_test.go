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
	for _, m := range train[:len(train)-1] {
		applyMidTrain(t, mig, m, final)
	}
	require.NoError(t, mig.Start(ctx, final, backfill.NewConfig(), trainStartOptions(mig, final)...))
}

// applyMidTrain applies one earlier migration of a train whose last
// migration is final.
func applyMidTrain(t *testing.T, mig *roll.Roll, m, final *migrations.Migration) {
	t.Helper()
	ctx := context.Background()

	opts := append([]roll.StartOption{roll.WithoutVersionSchema()}, trainStartOptions(mig, final)...)
	require.NoError(t, mig.Start(ctx, m, backfill.NewConfig(), opts...))
	if m.CompleteMustBeDeferred() {
		require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))
	} else {
		require.NoError(t, mig.Complete(ctx, roll.WithSkipSchemaDrop()))
	}
}

func trainStartOptions(mig *roll.Roll, final *migrations.Migration) []roll.StartOption {
	if !mig.UseVersionSchema() {
		return nil
	}
	return []roll.StartOption{roll.WithTrainTarget(final)}
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

// A train that stopped part-way and resumes with a different last migration
// re-keys the triggers its first attempt installed.
func TestTrainResumedWithADifferentLastMigration(t *testing.T) {
	t.Parallel()

	for name, rest := range map[string][]*migrations.Migration{
		"more migrations follow": {
			rawSQL("02_comment", `COMMENT ON TABLE u IS 'resumed'`, `SELECT 1`),
			rawSQL("10_resumed", `COMMENT ON COLUMN u.id IS 'resumed'`, `SELECT 1`),
		},
		"only the last one follows": {
			rawSQL("10_resumed", `COMMENT ON COLUMN u.id IS 'resumed'`, `SELECT 1`),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				require.NoError(t, mig.Start(ctx, baseMigration(
					`CREATE TABLE u (id integer PRIMARY KEY, v integer);
					 INSERT INTO u VALUES (1, 5)`,
				), backfill.NewConfig()))
				require.NoError(t, mig.Complete(ctx))

				// The first attempt stops after its first migration.
				abandoned := roll.WithTrainTarget(laterIndex("u", "id"))
				require.NoError(t, mig.Start(ctx, &migrations.Migration{
					Name: "01_check_v",
					Operations: migrations.Operations{
						&migrations.OpAlterColumn{
							Table: "u", Column: "v",
							Check: &migrations.CheckConstraint{Name: "v_pos", Constraint: "v > 0"},
							Up:    "CASE WHEN v > 0 THEN v ELSE 1 END", Down: "v",
						},
					},
				}, backfill.NewConfig(), roll.WithoutVersionSchema(), abandoned))
				require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))

				reopened, err := mig.ReopenedTrainLeaf(ctx)
				require.NoError(t, err)
				require.Empty(t, reopened, "a train that stopped part-way may resume")

				applyExpandOnlyTrain(t, mig, rest)

				const resumed = "public_10_resumed"
				require.Error(t, execAs(t, db, resumed, `INSERT INTO u (id, v) VALUES (10, -1)`),
					"the new revision's write must meet the new CHECK")
				require.NoError(t, execAs(t, db, resumed, `INSERT INTO u (id, v) VALUES (11, 7)`))
				require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO u (id, v) VALUES (12, -3)`))
				require.Equal(t, 7, *intPtr(db, t, `SELECT v FROM public_00_base.u WHERE id = 11`))

				require.NoError(t, mig.Complete(ctx))
				require.Equal(t, 7, *intPtr(db, t, `SELECT v FROM public.u WHERE id = 11`))
				require.Equal(t, 1, *intPtr(db, t, `SELECT v FROM public.u WHERE id = 12`))
				require.Zero(t, pgrollLeftovers(t, db))
			})
		})
	}
}

// A revert bounded at an earlier migration of the train makes that migration
// the leaf the revisions pin; the triggers left behind follow it.
func TestTrainBoundedRevertRekeysTheRemainingTriggers(t *testing.T) {
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
						Table: "u", Column: "v",
						Check: &migrations.CheckConstraint{Name: "v_pos", Constraint: "v > 0"},
						Up:    "CASE WHEN v > 0 THEN v ELSE 1 END", Down: "v",
					},
				},
			},
			rawSQL("02_comment", `COMMENT ON TABLE u IS 'kept'`, `COMMENT ON TABLE u IS NULL`),
			laterIndex("u", "id"),
		})

		_, err := mig.Revert(ctx, roll.WithRevertTo("02_comment"))
		require.NoError(t, err)

		reopened, err := mig.ReopenedTrainLeaf(ctx)
		require.NoError(t, err)
		require.Equal(t, "02_comment", reopened)

		const leaf = "public_02_comment"
		require.Error(t, execAs(t, db, leaf, `INSERT INTO u (id, v) VALUES (10, -1)`),
			"the new revision's write must meet the new CHECK")
		require.NoError(t, execAs(t, db, leaf, `INSERT INTO u (id, v) VALUES (11, 7)`))
		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO u (id, v) VALUES (12, -3)`))
		require.Equal(t, 7, *intPtr(db, t, `SELECT v FROM public_00_base.u WHERE id = 11`))

		_, err = mig.Revert(ctx)
		require.NoError(t, err)
		require.Zero(t, pgrollLeftovers(t, db))
		require.Equal(t, -3, *intPtr(db, t, `SELECT v FROM public.u WHERE id = 12`))
	})
}
