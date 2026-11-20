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
				require.Equal(t, 21, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 2`))
				require.NoError(t, mig.Complete(ctx))
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

// A table renamed later in the train still has its kept trigger retired.
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

// A contraction refused because the train never finished leaves the kept
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
		require.NoError(t, mig.Complete(ctx, roll.WithSkipSchemaDrop(), roll.WithTriggersKeptFor(final)))
		require.NoError(t, mig.Start(ctx, deferred, backfill.NewConfig(),
			roll.WithoutVersionSchema(), roll.WithTrainTarget(final)))
		require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))

		_, _, err := mig.FinishContraction(ctx)
		require.Error(t, err)

		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO w (id, x) VALUES (2, 20)`))
		require.Equal(t, 21, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 2`))
	})
}
