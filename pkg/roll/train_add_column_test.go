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

func baseW(t *testing.T, mig *roll.Roll) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, mig.Start(ctx, baseMigration(
		`CREATE TABLE w (id integer PRIMARY KEY, x integer);
		 INSERT INTO w VALUES (1, 10)`,
	), backfill.NewConfig()))
	require.NoError(t, mig.Complete(ctx))
}

// A mid-train migration that adds two columns to one table, one with `up`.
func TestTrainAddColumnWithASiblingAddColumn(t *testing.T) {
	t.Parallel()

	for name, ops := range map[string]migrations.Operations{
		"up first": {
			&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
			&migrations.OpAddColumn{Table: "w", Column: migrations.Column{Name: "b", Type: "integer", Nullable: true}},
		},
		"up second": {
			&migrations.OpAddColumn{Table: "w", Column: migrations.Column{Name: "b", Type: "integer", Nullable: true}},
			&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
		},
		"both up": {
			&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
			&migrations.OpAddColumn{Table: "w", Up: "x + 2", Column: migrations.Column{Name: "b", Type: "integer", Nullable: true}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				baseW(t, mig)
				applyExpandOnlyTrain(t, mig, []*migrations.Migration{
					{Name: "01_add_two", Operations: ops},
					laterIndex("w", "a"),
				})
				require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO w (id, x) VALUES (2, 20)`))
				require.NoError(t, execAs(t, db, targetSchema, `INSERT INTO w (id, x, a, b) VALUES (3, 30, 99, 98)`))
				require.Equal(t, 21, *intPtr(db, t, `SELECT a FROM public_09_later.w WHERE id = 2`))
				require.NoError(t, mig.Complete(ctx))
				require.Equal(t, 21, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 2`))
				require.Equal(t, 99, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 3`))
				require.Zero(t, pgrollLeftovers(t, db))
			})
		})
	}
}

// The new revision keeps writing while the train contracts.
func TestTrainWritesDuringContraction(t *testing.T) {
	t.Parallel()

	var conn *sql.DB
	var hookErrs []error
	hooks := roll.MigrationHooks{BeforeCompleteDDL: func(*roll.Roll) error {
		if conn == nil {
			return nil
		}
		for _, schema := range []string{targetSchema} {
			c, err := conn.Conn(context.Background())
			if err != nil {
				return err
			}
			_, err = c.ExecContext(context.Background(), "SET search_path TO "+schema)
			if err == nil {
				_, err = c.ExecContext(context.Background(),
					`INSERT INTO w (id, x) VALUES ((SELECT max(id) + 1 FROM public.w), 50)`)
			}
			c.Close()
			hookErrs = append(hookErrs, err)
		}
		return nil
	}}
	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
		[]roll.Option{roll.WithLockTimeoutMs(500), roll.WithMigrationHooks(hooks)},
		func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			baseW(t, mig)
			applyExpandOnlyTrain(t, mig, []*migrations.Migration{
				addColumn("01_add_a1", "a1", "x + 1"),
				addColumn("02_add_a2", "a2", "x + 2"),
				addColumn("09_later", "a3", "x + 3"),
			})
			conn = db
			require.NoError(t, mig.Complete(ctx))
			require.Len(t, hookErrs, 1)
			require.NoError(t, hookErrs[0], "new revision's write during contraction")
		})
}

// A table renamed later in the train still has the column's trigger removed.
func TestTrainAddColumnThenRenameTable(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		baseW(t, mig)
		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			addColumn("01_add_a", "a", "x + 1"),
			{Name: "02_rename", Operations: migrations.Operations{&migrations.OpRenameTable{From: "w", To: "w2"}}},
			rawSQL("09_later", `SELECT 1`, `SELECT 1`),
		})
		require.NoError(t, mig.Complete(ctx))
		require.Zero(t, pgrollLeftovers(t, db))
		require.NoError(t, execAs(t, db, "public", `INSERT INTO w2 (id, x, a) VALUES (5, 50, 1)`))
	})
}

// A contraction refused because the train never finished leaves the
// triggers in place: the previous revision is still the one running.
func TestTrainRefusedContractionKeepsTheTriggers(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		baseW(t, mig)

		final := rawSQL("09_later", `SELECT 1`, `SELECT 1`)
		deferred := &migrations.Migration{
			Name: "02_check_x",
			Operations: migrations.Operations{&migrations.OpAlterColumn{
				Table: "w", Column: "x",
				Check: &migrations.CheckConstraint{Name: "x_pos", Constraint: "x > 0"},
				Up:    "x", Down: "x",
			}},
		}
		require.NoError(t, mig.Start(ctx, addColumn("01_add_a", "a", "x + 1"), backfill.NewConfig(),
			roll.WithoutVersionSchema(), roll.WithTrainTarget(final)))
		require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))
		require.NoError(t, mig.Start(ctx, deferred, backfill.NewConfig(),
			roll.WithoutVersionSchema(), roll.WithTrainTarget(final)))
		require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))

		_, _, err := mig.FinishContraction(ctx)
		require.Error(t, err)

		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO w (id, x) VALUES (2, 20)`))
		require.Equal(t, 21, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 2`))
	})
}

// `up` runs on every write of the previous revision until the train
// contracts, wherever its add_column sits in the train. An `up` that does
// not read the row overwrites what the new revision stored; one that reads
// its own column keeps it.
func TestTrainAddColumnUpOnPreviousRevisionUpdates(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		up        string
		afterward int
	}{
		"a constant up overwrites the new revision's value": {up: "7", afterward: 7},
		"an up that reads its own column keeps it":          {up: "COALESCE(a, 7)", afterward: 99},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			for position, train := range map[string][]*migrations.Migration{
				"mid-train":     {addColumn("01_add_a", "a", tc.up), laterIndex("w", "x")},
				"last of train": {addColumn("09_later", "a", tc.up)},
			} {
				t.Run(position, func(t *testing.T) {
					t.Parallel()
					testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
						baseW(t, mig)
						applyExpandOnlyTrain(t, mig, train)

						require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO w (id, x) VALUES (2, 20)`))
						require.Equal(t, 7, *intPtr(db, t, `SELECT a FROM public_09_later.w WHERE id = 2`))

						require.NoError(t, execAs(t, db, targetSchema, `INSERT INTO w (id, x, a) VALUES (3, 30, 99)`))
						require.NoError(t, execAs(t, db, previousSchema, `UPDATE w SET x = 31 WHERE id = 3`))
						require.Equal(t, tc.afterward, *intPtr(db, t, `SELECT a FROM public_09_later.w WHERE id = 3`))
					})
				})
			}
		})
	}
}

// An expand-only sealed revert is a train too: while the fleet repins from
// the current schema to the restored one, both revisions keep writing.
func TestExpandOnlySealedRevertServesBothRevisions(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		require.NoError(t, mig.Start(ctx, baseMigration(
			`CREATE TABLE u (id integer PRIMARY KEY, v integer, email text);
			 INSERT INTO u VALUES (1, 5, 'a@example.com')`,
		), backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			laterIndexNamed("01_index", "u", "id"),
			{
				Name: "02_check_v",
				Operations: migrations.Operations{
					&migrations.OpAlterColumn{
						Table: "u", Column: "v",
						Check: &migrations.CheckConstraint{Name: "v_pos", Constraint: "v > 0"},
						Up:    "CASE WHEN v > 0 THEN v ELSE 1 END", Down: "v",
					},
				},
			},
			{
				Name: "03_drop_email",
				Operations: migrations.Operations{
					&migrations.OpDropColumn{Table: "u", Column: "email", Down: "'x@example.com'"},
				},
			},
		})
		require.NoError(t, mig.Complete(ctx))

		plan, err := mig.RevertSealed(ctx, "00_base", nil, roll.WithExpandOnly())
		require.NoError(t, err)
		require.NotNil(t, plan)

		const current, restored = "public_03_drop_email", "public_00_base"

		require.NoError(t, execAs(t, db, current, `INSERT INTO u (id, v) VALUES (2, 7)`))
		var email sql.NullString
		require.NoError(t, db.QueryRowContext(ctx, `SELECT email FROM public_00_base.u WHERE id = 2`).Scan(&email))
		require.Equal(t, "x@example.com", email.String,
			"the restored revision reads a row the current one wrote")
		require.Error(t, execAs(t, db, current, `INSERT INTO u (id, v) VALUES (3, -1)`),
			"the current revision is still held to its CHECK")

		require.NoError(t, execAs(t, db, restored, `INSERT INTO u (id, v, email) VALUES (4, -1, 'b@example.com')`))
		require.Equal(t, -1, *intPtr(db, t, `SELECT v FROM public_00_base.u WHERE id = 4`),
			"the restored revision keeps what it wrote")
		require.Equal(t, 1, *intPtr(db, t, `SELECT v FROM public_03_drop_email.u WHERE id = 4`),
			"the current revision reads it through the CHECK's up")

		require.NoError(t, mig.Complete(ctx))
		finished, err := mig.FinishPendingSealedRevert(ctx)
		require.NoError(t, err)
		require.NotNil(t, finished)
		require.Zero(t, pgrollLeftovers(t, db))
		require.Equal(t, -1, *intPtr(db, t, `SELECT v FROM public.u WHERE id = 4`))
	})
}

func laterIndexNamed(name, table, column string) *migrations.Migration {
	m := laterIndex(table, column)
	m.Name = name
	return m
}
