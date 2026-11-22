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

func pgrollLeftovers(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRowContext(context.Background(), `
		SELECT (SELECT count(*) FROM pg_proc WHERE proname LIKE '\_pgroll\_%')
		     + (SELECT count(*) FROM information_schema.columns
		        WHERE table_schema = 'public' AND column_name LIKE '\_pgroll\_%')`).Scan(&n))
	return n
}

func addColumn(name, column, up string) *migrations.Migration {
	return &migrations.Migration{
		Name: name,
		Operations: migrations.Operations{
			&migrations.OpAddColumn{
				Table:  "w",
				Up:     up,
				Column: migrations.Column{Name: column, Type: "integer", Nullable: true},
			},
		},
	}
}

func rawSQL(name, up, down string) *migrations.Migration {
	return &migrations.Migration{
		Name:       name,
		Operations: migrations.Operations{&migrations.OpRawSQL{Up: up, Down: down}},
	}
}

// Reverting a train before it contracts removes its triggers.
func TestTrainRevertRemovesItsTriggers(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		require.NoError(t, mig.Start(ctx, baseMigration(
			`CREATE TABLE w (id integer PRIMARY KEY, x integer);
			 INSERT INTO w VALUES (1, 10)`,
		), backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			addColumn("01_add_a", "a", "x + 1"),
			laterIndex("w", "a"),
		})
		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO w (id, x) VALUES (2, 20)`))

		_, err := mig.Revert(ctx)
		require.NoError(t, err)

		require.Zero(t, pgrollLeftovers(t, db))
		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO w (id, x) VALUES (3, 30)`))
		require.Equal(t, 3, *intPtr(db, t, `SELECT count(*) FROM public.w`))
	})
}

// The docs' train A: an add_column, then a raw CHECK naming the new column,
// then its validation.
func TestTrainAddColumnThenRawCheckOnIt(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		require.NoError(t, mig.Start(ctx, baseMigration(
			`CREATE TABLE w (id integer PRIMARY KEY, x integer);
			 INSERT INTO w VALUES (1, 10)`,
		), backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			addColumn("01_add_a", "a", "x + 1"),
			rawSQL("02_check", `ALTER TABLE w ADD CONSTRAINT c CHECK (a IS NOT NULL) NOT VALID`,
				`ALTER TABLE w DROP CONSTRAINT IF EXISTS c`),
			rawSQL("09_later", `ALTER TABLE w VALIDATE CONSTRAINT c`, `SELECT 1`),
		})

		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO w (id, x) VALUES (2, 20)`))
		require.Equal(t, 21, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 2`))

		require.NoError(t, mig.Complete(ctx))
		require.Zero(t, pgrollLeftovers(t, db))
	})
}

// Three add_columns in one train: the previous revision's write fills all
// three.
func TestTrainThreeAddColumnsAllBackfilled(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		require.NoError(t, mig.Start(ctx, baseMigration(
			`CREATE TABLE w (id integer PRIMARY KEY, x integer);
			 INSERT INTO w VALUES (1, 10)`,
		), backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			addColumn("01_add_a1", "a1", "x + 1"),
			addColumn("02_add_a2", "a2", "x + 2"),
			addColumn("09_later", "a3", "x + 3"),
		})

		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO w (id, x) VALUES (2, 20)`))
		require.NoError(t, mig.Complete(ctx))

		require.Equal(t, 21, *intPtr(db, t, `SELECT a1 FROM public.w WHERE id = 2`))
		require.Equal(t, 22, *intPtr(db, t, `SELECT a2 FROM public.w WHERE id = 2`))
		require.Equal(t, 23, *intPtr(db, t, `SELECT a3 FROM public.w WHERE id = 2`))
		require.Zero(t, pgrollLeftovers(t, db))
	})
}

// A native nullability relax and the drop of the same column in one train:
// the drop's NULL reaches the original column through the relax's `down`,
// so the new revision's write lands only when that `down` yields a value.
func TestTrainNativeRelaxThenDrop(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		relaxDown string
		accepted  bool
	}{
		"the relax's down fills the column":    {relaxDown: "COALESCE(gone, 0)", accepted: true},
		"the relax's down passes NULL through": {relaxDown: "gone", accepted: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testTrainNativeRelaxThenDrop(t, tc.relaxDown, tc.accepted)
		})
	}
}

func testTrainNativeRelaxThenDrop(t *testing.T, relaxDown string, accepted bool) {
	t.Helper()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		require.NoError(t, mig.Start(ctx, baseMigration(
			`CREATE TABLE u (id integer PRIMARY KEY, gone integer NOT NULL);
			 INSERT INTO u VALUES (1, 1)`,
		), backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		nullable := true
		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			{
				Name: "01_relax",
				Operations: migrations.Operations{
					&migrations.OpAlterColumn{
						Table: "u", Column: "gone", Nullable: &nullable,
						Up: "gone", Down: relaxDown,
					},
				},
			},
			{
				Name: "09_later",
				Operations: migrations.Operations{
					&migrations.OpDropColumn{Table: "u", Column: "gone", Down: "NULL"},
				},
			},
		})

		err := execAs(t, db, targetSchema, `INSERT INTO u (id) VALUES (2)`)
		if !accepted {
			require.ErrorContains(t, err, "not-null")
			return
		}
		require.NoError(t, err)
		require.Equal(t, 0, *intPtr(db, t, `SELECT gone FROM public_00_base.u WHERE id = 2`))
		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO u (id, gone) VALUES (3, 3)`))
		require.NoError(t, mig.Complete(ctx))
		require.Equal(t, 3, *intPtr(db, t, `SELECT count(*) FROM public.u`))
		require.Zero(t, pgrollLeftovers(t, db))
	})
}

// Two alter_columns on the same column in one train.
func TestTrainTwoAlterColumnsOnOneColumn(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		require.NoError(t, mig.Start(ctx, baseMigration(
			`CREATE TABLE u (id integer PRIMARY KEY, v integer);
			 INSERT INTO u VALUES (1, 5)`,
		), backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		bigint := "bigint"
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
			{
				Name: "02_widen_v",
				Operations: migrations.Operations{
					&migrations.OpAlterColumn{Table: "u", Column: "v", Type: &bigint, Up: "v", Down: "v"},
				},
			},
			laterIndex("u", "id"),
		})

		require.Error(t, execAs(t, db, targetSchema, `INSERT INTO u (id, v) VALUES (10, -1)`))
		require.NoError(t, execAs(t, db, targetSchema, `INSERT INTO u (id, v) VALUES (11, 7)`))
		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO u (id, v) VALUES (12, -3)`))
		require.Equal(t, 7, *intPtr(db, t, `SELECT v FROM public_00_base.u WHERE id = 11`),
			"the previous revision reads the new revision's value")

		require.NoError(t, mig.Complete(ctx))
		require.Equal(t, 7, *intPtr(db, t, `SELECT v FROM public.u WHERE id = 11`))
		require.Equal(t, 1, *intPtr(db, t, `SELECT v FROM public.u WHERE id = 12`))
		require.Zero(t, pgrollLeftovers(t, db))
	})
}
