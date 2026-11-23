// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

const baseUV = `CREATE TABLE u (id integer PRIMARY KEY, v integer);
	INSERT INTO u VALUES (1, 5)`

// checkV adds a CHECK to u.v: it waits for the train's contraction, with an
// `up` trigger the previous revision's writes cross and a `down` trigger the
// new revision's cross.
func checkV(name string) *migrations.Migration {
	return &migrations.Migration{
		Name: name,
		Operations: migrations.Operations{
			&migrations.OpAlterColumn{
				Table: "u", Column: "v",
				Check: &migrations.CheckConstraint{Name: "v_pos", Constraint: "v > 0"},
				Up:    "CASE WHEN v > 0 THEN v ELSE 1 END", Down: "v",
			},
		},
	}
}

func startBase(t *testing.T, mig *roll.Roll, up string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, mig.Start(ctx, baseMigration(up), backfill.NewConfig()))
	require.NoError(t, mig.Complete(ctx))
}

// Reverting a train before it contracts removes its triggers and the
// function they follow.
func TestTrainRevertRemovesItsTriggers(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		startBase(t, mig, baseUV)
		applyExpandOnlyTrain(t, mig, []*migrations.Migration{checkV("01_check_v"), laterIndex("u", "id")})
		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO u (id, v) VALUES (2, -3)`))

		_, err := mig.Revert(ctx)
		require.NoError(t, err)

		require.Zero(t, pgrollLeftovers(t, db))
		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO u (id, v) VALUES (3, -4)`))
		require.Equal(t, 3, *intPtr(db, t, `SELECT count(*) FROM public.u`))
	})
}

// A train's trigger functions call a function in the migrated schema. A
// trigger function needs no EXECUTE privilege from the role whose write
// fires it, but a function it calls does, also where functions are not
// executable by PUBLIC by default.
func TestTrainTargetIsExecutableByTheApplicationRole(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		_, err := db.ExecContext(ctx, `
			DO $$ BEGIN
			  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'app_writer') THEN
			    CREATE ROLE app_writer NOLOGIN;
			  END IF;
			END $$;
			ALTER DEFAULT PRIVILEGES REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;
			ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO app_writer;
			GRANT USAGE ON SCHEMA public TO app_writer;`)
		require.NoError(t, err)
		startBase(t, mig, baseUV)

		applyExpandOnlyTrain(t, mig, []*migrations.Migration{checkV("09_later")})

		conn, err := db.Conn(ctx)
		require.NoError(t, err)
		defer conn.Close()
		_, err = conn.ExecContext(ctx, "SET search_path TO "+previousSchema+"; "+
			"GRANT USAGE ON SCHEMA "+previousSchema+" TO app_writer; "+
			"GRANT ALL ON ALL TABLES IN SCHEMA "+previousSchema+" TO app_writer; "+
			"GRANT ALL ON ALL TABLES IN SCHEMA public TO app_writer; "+
			"SET ROLE app_writer")
		require.NoError(t, err)
		defer conn.ExecContext(ctx, "RESET ROLE") //nolint:errcheck
		_, err = conn.ExecContext(ctx, `INSERT INTO u (id, v) VALUES (3, 3)`)
		require.NoError(t, err, "the application role writes through the train's triggers")
	})
}

// A train stops part-way and the next migration is started the way
// `pgroll start <file>` starts it: no train target. The new revision pins
// that migration's schema, so the triggers the train left behind follow it.
func TestTrainPlainStartOnAPartialTrainRekeysTheTriggers(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		startBase(t, mig, baseUV)

		abandoned := laterIndex("u", "id") // 09_later, never applied
		applyMidTrain(t, mig, checkV("01_check_v"), abandoned)

		reopened, err := mig.ReopenedTrainLeaf(ctx)
		require.NoError(t, err)
		require.Empty(t, reopened, "the guard `pgroll start` runs allows this state")

		require.NoError(t, mig.Start(ctx, rawSQL("05_by_hand", `COMMENT ON TABLE u IS 'x'`, `SELECT 1`), backfill.NewConfig()))

		const started = "public_05_by_hand"
		require.NoError(t, execAs(t, db, started, `INSERT INTO u (id, v) VALUES (3, 7)`))
		assert.Equal(t, 7, *intPtr(db, t, `SELECT v FROM public_00_base.u WHERE id = 3`),
			"the previous revision reads what the new one wrote")
		require.Error(t, execAs(t, db, started, `INSERT INTO u (id, v) VALUES (4, -1)`),
			"the new revision's write must meet the new CHECK")
		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO u (id, v) VALUES (5, -3)`),
			"the previous revision is not held to it")
	})
}

// A train stops part-way and its leaf is materialized, the recovery for a
// batched migrate interrupted before its last Start projected a version
// schema. The fleet pins the materialized schema, so the triggers follow it.
func TestTrainMaterializedLeafOfAPartialTrainRekeysTheTriggers(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		startBase(t, mig, baseUV)

		final := laterIndex("u", "id") // 09_later, never applied
		applyMidTrain(t, mig, checkV("01_check_v"), final)
		applyMidTrain(t, mig, rawSQL("02_comment", `COMMENT ON TABLE u IS 'x'`, `SELECT 1`), final)

		// `pgroll materialize`, as cmd/materialize.go does with nothing active.
		sc, err := mig.SchemaWithDeferred(ctx)
		require.NoError(t, err)
		require.NoError(t, mig.Materialize(ctx, "02_comment", sc))

		reopened, err := mig.ReopenedTrainLeaf(ctx)
		require.NoError(t, err)
		require.Equal(t, "02_comment", reopened)

		const leaf = "public_02_comment"
		require.NoError(t, execAs(t, db, leaf, `INSERT INTO u (id, v) VALUES (3, 7)`))
		assert.Equal(t, 7, *intPtr(db, t, `SELECT v FROM public_00_base.u WHERE id = 3`),
			"the previous revision reads what a pod on the materialized leaf wrote")
		require.Error(t, execAs(t, db, leaf, `INSERT INTO u (id, v) VALUES (4, -1)`),
			"a pod on the materialized leaf is held to the new CHECK")
	})
}

// A train with no trigger leaves nothing of pgroll's in the migrated schema.
func TestTrainWithoutTriggersInstallsNoTargetFunction(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		startBase(t, mig, baseUV)
		applyExpandOnlyTrain(t, mig, []*migrations.Migration{rawSQL("09_later", `COMMENT ON TABLE u IS 'x'`, `SELECT 1`)})
		require.Zero(t, pgrollLeftovers(t, db))
	})
}

// A contraction that died between marking its last migration done and
// sealing it leaves the row done and unsealed, with the version schema the
// fleet runs on and no trigger. The next deployment may start.
func TestTrainGuardAllowsAContractionThatStoppedBeforeSealing(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		startBase(t, mig, baseUV)
		_, err := db.ExecContext(ctx, `UPDATE pgroll.migrations SET sealed = false WHERE name = '00_base'`)
		require.NoError(t, err)

		leaf, err := mig.ReopenedTrainLeaf(ctx)
		require.NoError(t, err)
		require.Empty(t, leaf)
	})
}

// A rename_column, then the drop of that column, in one train. The rename
// waits for the contraction, so the drop's `down` trigger writes the column
// under the name it still has.
func TestTrainRenameThenDropOfAColumn(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		startBase(t, mig, `CREATE TABLE u (id integer PRIMARY KEY, x integer, c integer NOT NULL);
			INSERT INTO u VALUES (1, 10, 1)`)
		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			{Name: "01_rename", Operations: migrations.Operations{&migrations.OpRenameColumn{Table: "u", From: "c", To: "d"}}},
			{Name: "02_drop", Operations: migrations.Operations{&migrations.OpDropColumn{Table: "u", Column: "d", Down: "x + 100"}}},
			laterIndex("u", "x"),
		})

		require.NoError(t, execAs(t, db, targetSchema, `INSERT INTO u (id, x) VALUES (2, 20)`),
			"the new revision writes without the dropped column")
		require.Equal(t, 120, *intPtr(db, t, `SELECT c FROM public_00_base.u WHERE id = 2`),
			"the previous revision reads the value `down` gave it")
		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO u (id, x, c) VALUES (3, 30, 3)`))

		require.NoError(t, mig.Complete(ctx))
		require.Zero(t, pgrollLeftovers(t, db))
	})
}

// With version schemas disabled nothing follows a train target.
func TestTrainWithVersionSchemasDisabledInstallsNoTargetFunction(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
		[]roll.Option{roll.WithVersionSchema(false)},
		func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			startBase(t, mig, baseUV)
			applyExpandOnlyTrain(t, mig, []*migrations.Migration{checkV("01_check_v"), laterIndex("u", "id")})

			var functions int
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT count(*) FROM pg_proc WHERE proname = $1`, backfill.TrainTargetFunction).Scan(&functions))
			require.Zero(t, functions)
			require.NoError(t, mig.Complete(ctx))
			require.Zero(t, pgrollLeftovers(t, db))
		})
}

// An expand-only sealed revert is a train too: while the fleet repins from
// the current schema to the restored one, both revisions keep writing, and
// the inverse migrations' triggers tell them apart by the restored schema.
func TestExpandOnlySealedRevertServesBothRevisions(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		startBase(t, mig, baseUV)

		later := rawSQL("03_comment", `COMMENT ON TABLE u IS 'later'`, `COMMENT ON TABLE u IS NULL`)
		applyExpandOnlyTrain(t, mig, []*migrations.Migration{laterIndexNamed("01_index", "u", "id"), checkV("02_check_v"), later})
		require.NoError(t, mig.Complete(ctx))

		plan, err := mig.RevertSealed(ctx, "00_base", nil, roll.WithExpandOnly())
		require.NoError(t, err)
		require.NotNil(t, plan)

		const current, restored = "public_03_comment", "public_00_base"

		require.NoError(t, execAs(t, db, current, `INSERT INTO u (id, v) VALUES (2, 7)`))
		require.Error(t, execAs(t, db, current, `INSERT INTO u (id, v) VALUES (3, -1)`),
			"the current revision is still held to its CHECK")

		require.NoError(t, execAs(t, db, restored, `INSERT INTO u (id, v) VALUES (4, -1)`))
		require.Equal(t, -1, *intPtr(db, t, `SELECT v FROM public_00_base.u WHERE id = 4`),
			"the restored revision keeps what it wrote")
		require.Equal(t, 1, *intPtr(db, t, `SELECT v FROM public_03_comment.u WHERE id = 4`),
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
