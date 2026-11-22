// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

// A mid-train add_column with a volatile default has to repeat the default
// as its `up`, so it cannot keep a stored value. It completes inline: the
// previous revision's update of another column leaves the value alone.
func TestTrainVolatileDefaultSurvivesAPreviousRevisionUpdate(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		baseW(t, mig)

		uuid := "gen_random_uuid()"
		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			{Name: "01_add_public_id", Operations: migrations.Operations{&migrations.OpAddColumn{
				Table: "w", Up: uuid,
				Column: migrations.Column{Name: "public_id", Type: "uuid", Nullable: false, Default: &uuid},
			}}},
			laterIndex("w", "x"),
		})

		read := func() string {
			var id string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT public_id FROM public_09_later.w WHERE id = 1`).Scan(&id))
			return id
		}
		handedOut := read() // a pod of the new revision returns this to a client

		require.NoError(t, execAs(t, db, previousSchema, `UPDATE w SET x = x + 1 WHERE id = 1`),
			"a pod of the previous revision updates an unrelated column of the same row")
		require.Equal(t, handedOut, read(), "the identifier the new revision handed out is still the row's")
	})
}

// The same column, when a later migration of the train copies the generated
// values and a still later one backfills the table from pgroll's session.
func TestTrainVolatileDefaultSurvivesALaterBackfill(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		baseW(t, mig)

		uuid := "gen_random_uuid()"
		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			{Name: "01_add_public_id", Operations: migrations.Operations{&migrations.OpAddColumn{
				Table: "w", Up: uuid,
				Column: migrations.Column{Name: "public_id", Type: "uuid", Nullable: false, Default: &uuid},
			}}},
			rawSQL("02_links", `CREATE TABLE links AS SELECT id AS w_id, public_id FROM w`, `DROP TABLE links`),
			addColumn("03_add_b", "b", "x + 2"),
			laterIndex("w", "x"),
		})

		require.Equal(t, 1, *intPtr(db, t, `
			SELECT count(*) FROM public.links l JOIN public.w ON w.id = l.w_id AND w.public_id = l.public_id`),
			"the identifiers copied earlier in the train still match their rows")
	})
}

// An add_column with `up`, then a drop_table of the same table later in the
// train. drop_table renames the table at Start, under the column's trigger.
func TestTrainAddColumnThenDropTable(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		baseW(t, mig)
		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			addColumn("01_add_a", "a", "x + 1"),
			{Name: "02_drop_w", Operations: migrations.Operations{&migrations.OpDropTable{Name: "w"}}},
			rawSQL("09_later", `SELECT 1`, `SELECT 1`),
		})
		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO w (id, x) VALUES (2, 20)`),
			"the previous revision still writes the table the train drops")
		require.NoError(t, mig.Complete(t.Context()))
		require.Zero(t, pgrollLeftovers(t, db))
	})
}

// The same, when a later migration of the train creates a table with the
// dropped one's name.
func TestTrainAddColumnThenDropAndRecreateTable(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		baseW(t, mig)
		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			addColumn("01_add_a", "a", "x + 1"),
			{Name: "02_drop_w", Operations: migrations.Operations{&migrations.OpDropTable{Name: "w"}}},
			rawSQL("03_new_w", `CREATE TABLE w (id integer PRIMARY KEY, y text)`, `DROP TABLE w`),
			rawSQL("09_later", `SELECT 1`, `SELECT 1`),
		})
		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO w (id, x) VALUES (2, 20)`))
		require.NoError(t, execAs(t, db, targetSchema, `INSERT INTO w (id, y) VALUES (1, 'new')`))
		require.NoError(t, mig.Complete(t.Context()))
		require.Zero(t, pgrollLeftovers(t, db))
		require.Equal(t, 1, *intPtr(db, t, `SELECT count(*) FROM public.w WHERE y = 'new'`))
	})
}

// A train stops part-way and its leaf is materialized, the recovery for a
// batched migrate interrupted before its last Start projected a version
// schema. The fleet pins the materialized schema, so the triggers follow it.
func TestTrainMaterializedLeafOfAPartialTrainRekeysTheTriggers(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		require.NoError(t, mig.Start(ctx, baseMigration(
			`CREATE TABLE w (id integer PRIMARY KEY, x integer, v integer);
			 INSERT INTO w VALUES (1, 10, 5)`,
		), backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		final := laterIndex("w", "id") // 09_later, never applied
		applyMidTrain(t, mig, addColumn("01_add_a", "a", "x + 1"), final)
		applyMidTrain(t, mig, rawSQL("02_comment", `COMMENT ON TABLE w IS 'x'`, `SELECT 1`), final)

		// `pgroll materialize`, as cmd/materialize.go does with nothing active.
		sc, err := mig.SchemaWithDeferred(ctx)
		require.NoError(t, err)
		require.NoError(t, mig.Materialize(ctx, "02_comment", sc))

		reopened, err := mig.ReopenedTrainLeaf(ctx)
		require.NoError(t, err)
		require.Equal(t, "02_comment", reopened)

		const leaf = "public_02_comment"
		require.NoError(t, execAs(t, db, leaf, `INSERT INTO w (id, x, v, a) VALUES (3, 30, 7, 99)`))
		assert.Equal(t, 99, *intPtr(db, t, `SELECT a FROM public_02_comment.w WHERE id = 3`),
			"a pod pinned to the materialized leaf keeps the value it wrote")
		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO w (id, x, v) VALUES (4, 40, 7)`))
		assert.Equal(t, 41, *intPtr(db, t, `SELECT a FROM public_02_comment.w WHERE id = 4`),
			"the previous revision's write is still filled")
	})
}

// With version schemas disabled no fleet rolls, so a mid-train add_column
// completes inline and the writer's own value survives.
func TestTrainWithVersionSchemasDisabledCompletesAddColumnInline(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
		[]roll.Option{roll.WithVersionSchema(false)},
		func(mig *roll.Roll, db *sql.DB) {
			baseW(t, mig)
			applyExpandOnlyTrain(t, mig, []*migrations.Migration{
				addColumn("01_add_a", "a", "x + 1"),
				laterIndex("w", "x"),
			})
			require.NoError(t, execAs(t, db, "public", `INSERT INTO w (id, x, a) VALUES (3, 30, 99)`))
			require.Equal(t, 99, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 3`),
				"the writer's own value for a column a mid-train migration added")
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

		require.NoError(t, mig.Start(ctx, baseMigration(
			`CREATE TABLE u (id integer PRIMARY KEY, v integer);
			 INSERT INTO u VALUES (1, 5)`,
		), backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		writeAs := func(versionSchema, stmt string) error {
			conn, err := db.Conn(ctx)
			require.NoError(t, err)
			defer conn.Close()
			_, err = conn.ExecContext(ctx, "SET search_path TO "+versionSchema)
			require.NoError(t, err)
			_, err = conn.ExecContext(ctx, "GRANT USAGE ON SCHEMA "+versionSchema+" TO app_writer; "+
				"GRANT ALL ON ALL TABLES IN SCHEMA "+versionSchema+" TO app_writer; "+
				"GRANT ALL ON ALL TABLES IN SCHEMA public TO app_writer")
			require.NoError(t, err)
			_, err = conn.ExecContext(ctx, "SET ROLE app_writer")
			require.NoError(t, err)
			defer conn.ExecContext(ctx, "RESET ROLE") //nolint:errcheck
			_, err = conn.ExecContext(ctx, stmt)
			return err
		}

		// One migration, started alone and with no train: the literal form.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "01_check_v",
			Operations: migrations.Operations{
				&migrations.OpAlterColumn{
					Table: "u", Column: "v",
					Check: &migrations.CheckConstraint{Name: "v_pos", Constraint: "v > 0"},
					Up:    "CASE WHEN v > 0 THEN v ELSE 1 END", Down: "v",
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, writeAs(previousSchema, `INSERT INTO u (id, v) VALUES (2, 3)`),
			"without a train the application role writes")
		require.NoError(t, mig.Rollback(ctx))

		// The same migration the way `pgroll migrate` starts it.
		last := &migrations.Migration{
			Name: "09_later",
			Operations: migrations.Operations{
				&migrations.OpAlterColumn{
					Table: "u", Column: "v",
					Check: &migrations.CheckConstraint{Name: "v_pos", Constraint: "v > 0"},
					Up:    "CASE WHEN v > 0 THEN v ELSE 1 END", Down: "v",
				},
			},
		}
		applyExpandOnlyTrain(t, mig, []*migrations.Migration{last})
		require.NoError(t, writeAs(previousSchema, `INSERT INTO u (id, v) VALUES (3, 3)`),
			"with a train the application role writes")
	})
}

// `pgroll complete` on a train that stopped after an add_column with `up`
// is refused, and the previous revision, the only one running, keeps its
// fill.
func TestTrainUnfinishedContractionKeepsFillingThePreviousRevision(t *testing.T) {
	t.Parallel()

	for name, nullable := range map[string]bool{"nullable column": true, "NOT NULL column": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				baseW(t, mig)

				final := rawSQL("09_later", `SELECT 1`, `SELECT 1`)
				first := &migrations.Migration{
					Name: "01_add_a",
					Operations: migrations.Operations{
						&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: nullable}},
					},
				}
				// The train stops after its first migration.
				applyMidTrain(t, mig, first, final)

				require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO w (id, x) VALUES (2, 20)`))
				require.Equal(t, 21, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 2`))

				// `pgroll complete` with nothing active.
				_, _, err := mig.FinishContraction(ctx)
				require.Error(t, err)

				// The previous revision is still the only one running.
				require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO w (id, x) VALUES (3, 30)`),
					"the previous revision's write after `pgroll complete` on the unfinished train")
				require.Equal(t, 31, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 3`))
			})
		})
	}
}

// A train stops part-way and the next migration is started the way
// `pgroll start <file>` starts it: no train target. The new revision pins
// that migration's schema, so the triggers the train left behind follow it.
func TestTrainPlainStartOnAPartialTrainRekeysTheTriggers(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		require.NoError(t, mig.Start(ctx, baseMigration(
			`CREATE TABLE w (id integer PRIMARY KEY, x integer, v integer);
			 INSERT INTO w VALUES (1, 10, 5)`,
		), backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		abandoned := laterIndex("w", "id") // 09_later, never applied
		applyMidTrain(t, mig, addColumn("01_add_a", "a", "x + 1"), abandoned)
		applyMidTrain(t, mig, &migrations.Migration{
			Name: "02_check_v",
			Operations: migrations.Operations{
				&migrations.OpAlterColumn{
					Table: "w", Column: "v",
					Check: &migrations.CheckConstraint{Name: "v_pos", Constraint: "v > 0"},
					Up:    "CASE WHEN v > 0 THEN v ELSE 1 END", Down: "v",
				},
			},
		}, abandoned)

		// The train stops here. The operator starts the next file by hand.
		reopened, err := mig.ReopenedTrainLeaf(ctx)
		require.NoError(t, err)
		require.Empty(t, reopened, "the guard `pgroll start` runs allows this state")

		require.NoError(t, mig.Start(ctx, rawSQL("05_by_hand", `COMMENT ON TABLE w IS 'x'`, `SELECT 1`), backfill.NewConfig()))

		const started = "public_05_by_hand"
		require.NoError(t, execAs(t, db, started, `INSERT INTO w (id, x, v, a) VALUES (3, 30, 7, 99)`))
		assert.Equal(t, 99, *intPtr(db, t, `SELECT a FROM public_05_by_hand.w WHERE id = 3`),
			"the new revision keeps its own value for the added column")
		require.Error(t, execAs(t, db, started, `INSERT INTO w (id, x, v, a) VALUES (4, 40, -1, 1)`),
			"the new revision's write must meet the new CHECK")
	})
}

// A pod of the previous revision is still inside a transaction, one that
// has read another table, when the deployment is contracted. Dropping its
// version schema fails, by design, so the operator can move the pod and
// re-run; the straggler's writes are still filled.
func TestTrainRefusedContractionWithAStraggler(t *testing.T) {
	t.Parallel()

	for name, viaFinish := range map[string]bool{"Complete": false, "FinishContraction": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
				[]roll.Option{roll.WithLockTimeoutMs(100), roll.WithLockRetryTimeout(300 * time.Millisecond)},
				func(mig *roll.Roll, db *sql.DB) {
					ctx := context.Background()
					require.NoError(t, mig.Start(ctx, baseMigration(
						`CREATE TABLE w (id integer PRIMARY KEY, x integer);
						 CREATE TABLE z (id integer PRIMARY KEY);
						 INSERT INTO w VALUES (1, 10)`,
					), backfill.NewConfig()))
					require.NoError(t, mig.Complete(ctx))
					applyExpandOnlyTrain(t, mig, []*migrations.Migration{
						{Name: "01_add_a", Operations: migrations.Operations{&migrations.OpAddColumn{
							Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: false},
						}}},
						{Name: "02_check_x", Operations: migrations.Operations{&migrations.OpAlterColumn{
							Table: "w", Column: "x",
							Check: &migrations.CheckConstraint{Name: "x_pos", Constraint: "x > 0"},
							Up:    "x", Down: "x",
						}}},
						rawSQL("09_later", `SELECT 1`, `SELECT 1`),
					})
					if viaFinish {
						// Nothing active: the leaf is the train's second migration.
						_, err := mig.Revert(ctx, roll.WithRevertTo("02_check_x"))
						require.NoError(t, err)
					}

					straggler, err := db.Conn(ctx)
					require.NoError(t, err)
					defer straggler.Close()
					_, err = straggler.ExecContext(ctx, `SET search_path TO public_00_base`)
					require.NoError(t, err)
					_, err = straggler.ExecContext(ctx, `BEGIN; SELECT count(*) FROM z`)
					require.NoError(t, err)

					if viaFinish {
						_, _, err = mig.FinishContraction(ctx)
					} else {
						err = mig.Complete(ctx)
					}
					require.ErrorContains(t, err, "public_00_base", "the contraction is refused while the straggler holds its schema")

					_, err = straggler.ExecContext(ctx, `INSERT INTO w (id, x) VALUES (2, 20)`)
					require.NoError(t, err, "the straggler's write after the refused contraction")
				})
		})
	}
}

// A migration adds two columns with `up` to one table and one to another.
// Its Complete renames the first column, then waits on the other table; the
// second column's function is still installed and must not read the first
// under its temporary name.
func TestTrainWritesWhileAnAddColumnCompletes(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
		[]roll.Option{roll.WithLockTimeoutMs(200)},
		func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			require.NoError(t, mig.Start(ctx, baseMigration(
				`CREATE TABLE w (id integer PRIMARY KEY, x integer);
				 CREATE TABLE z (id integer PRIMARY KEY);
				 INSERT INTO w VALUES (1, 10)`,
			), backfill.NewConfig()))
			require.NoError(t, mig.Complete(ctx))

			applyExpandOnlyTrain(t, mig, []*migrations.Migration{
				{
					Name: "01_add_two",
					Operations: migrations.Operations{
						&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
						// Completes between the two, and waits on z.
						&migrations.OpAddColumn{Table: "z", Column: migrations.Column{Name: "c", Type: "integer", Nullable: true}},
						&migrations.OpAddColumn{Table: "w", Up: "x + 2", Column: migrations.Column{Name: "b", Type: "integer", Nullable: true}},
					},
				},
				rawSQL("09_later", `SELECT 1`, `SELECT 1`),
			})

			blocker, err := db.Conn(ctx)
			require.NoError(t, err)
			defer blocker.Close()
			_, err = blocker.ExecContext(ctx, `BEGIN; SELECT count(*) FROM z`)
			require.NoError(t, err)

			done := make(chan error, 1)
			go func() { done <- mig.Complete(ctx) }()
			require.Eventually(t, func() bool {
				var renamed bool
				require.NoError(t, db.QueryRowContext(ctx, `
					SELECT EXISTS (SELECT 1 FROM information_schema.columns
					               WHERE table_schema = 'public' AND table_name = 'w' AND column_name = 'a')`).Scan(&renamed))
				return renamed
			}, 10*time.Second, 50*time.Millisecond)

			windowErr := execAs(t, db, targetSchema, `INSERT INTO w (id, x, a, b) VALUES (3, 30, 1, 2)`)

			_, err = blocker.ExecContext(ctx, `ROLLBACK`)
			require.NoError(t, err)
			require.NoError(t, <-done)
			require.NoError(t, windowErr, "the new revision's write while the migration's Complete is still running")
			require.Zero(t, pgrollLeftovers(t, db))
		})
}

// Trigger function names are `_pgroll_trigger_<table>_<column>_<scope>` and
// Postgres cuts an identifier at 63 bytes. Two columns of a long-named table
// that share their first characters still get a function each.
func TestTrainLongTableAndColumnNamesGetDistinctTriggerFunctions(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		const table = "portfolio_monitoring_subscription_items" // 39 bytes
		require.NoError(t, mig.Start(ctx, baseMigration(
			`CREATE TABLE `+table+` (id integer PRIMARY KEY, x integer);
			 INSERT INTO `+table+` VALUES (1, 10)`,
		), backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		add := func(name, column, up string) *migrations.Migration {
			return &migrations.Migration{
				Name: name,
				Operations: migrations.Operations{
					&migrations.OpAddColumn{Table: table, Up: up, Column: migrations.Column{Name: column, Type: "integer", Nullable: true}},
				},
			}
		}
		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			add("01_add_code", "previous_status_code", "x + 1"),
			add("02_add_text", "previous_status_text", "x + 2"),
			rawSQL("09_later", `SELECT 1`, `SELECT 1`),
		})

		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO `+table+` (id, x) VALUES (2, 20)`))
		code := intPtr(db, t, `SELECT previous_status_code FROM public_09_later.`+table+` WHERE id = 2`)
		text := intPtr(db, t, `SELECT previous_status_text FROM public_09_later.`+table+` WHERE id = 2`)
		if assert.NotNil(t, code, "the first column is filled for the previous revision") {
			assert.Equal(t, 21, *code)
		}
		if assert.NotNil(t, text, "the second column is filled for the previous revision") {
			assert.Equal(t, 22, *text)
		}

		require.NoError(t, mig.Complete(ctx))
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
		baseW(t, mig)
		_, err := db.ExecContext(ctx, `UPDATE pgroll.migrations SET sealed = false WHERE name = '00_base'`)
		require.NoError(t, err)

		leaf, err := mig.ReopenedTrainLeaf(ctx)
		require.NoError(t, err)
		require.Empty(t, leaf, "the next deployment may start")
	})
}

// The new revision writes while the train contracts. An earlier migration
// of the train drops a column, or renames a duplicate, when it drains; a
// later migration's trigger functions on the table are still installed.
func TestTrainWritesBetweenDrains(t *testing.T) {
	t.Parallel()

	for name, first := range map[string]*migrations.Migration{
		"an earlier drop_column": {
			Name: "01_first",
			Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "w", Column: "gone", Down: "0"},
			},
		},
		"an earlier alter_column": {
			Name: "01_first",
			Operations: migrations.Operations{
				&migrations.OpAlterColumn{
					Table: "w", Column: "gone",
					Check: &migrations.CheckConstraint{Name: "gone_pos", Constraint: "gone >= 0"},
					Up:    "gone", Down: "gone",
				},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
				[]roll.Option{roll.WithLockTimeoutMs(200)},
				func(mig *roll.Roll, db *sql.DB) {
					ctx := context.Background()
					require.NoError(t, mig.Start(ctx, baseMigration(
						`CREATE TABLE w (id integer PRIMARY KEY, x integer, gone integer);
						 CREATE TABLE z (id integer PRIMARY KEY, c integer);
						 INSERT INTO w VALUES (1, 10, 1)`,
					), backfill.NewConfig()))
					require.NoError(t, mig.Complete(ctx))

					applyExpandOnlyTrain(t, mig, []*migrations.Migration{
						first,
						{
							Name: "02_second",
							Operations: migrations.Operations{
								// Drains first, and waits on z.
								&migrations.OpAlterColumn{
									Table: "z", Column: "c",
									Check: &migrations.CheckConstraint{Name: "c_pos", Constraint: "c > 0"},
									Up:    "c", Down: "c",
								},
								&migrations.OpAlterColumn{
									Table: "w", Column: "x",
									Check: &migrations.CheckConstraint{Name: "x_pos", Constraint: "x > 0"},
									Up:    "x", Down: "x",
								},
							},
						},
						rawSQL("09_later", `SELECT 1`, `SELECT 1`),
					})

					require.NoError(t, execAs(t, db, targetSchema, `INSERT INTO w (id, x) VALUES (2, 20)`),
						"the new revision writes before the contraction")

					// A session has read z in a transaction it has not ended.
					blocker, err := db.Conn(ctx)
					require.NoError(t, err)
					defer blocker.Close()
					_, err = blocker.ExecContext(ctx, `BEGIN; SELECT count(*) FROM z`)
					require.NoError(t, err)

					done := make(chan error, 1)
					go func() { done <- mig.Complete(ctx) }()

					// Wait for the first migration's drain.
					require.Eventually(t, func() bool {
						var drained bool
						require.NoError(t, db.QueryRowContext(ctx, `
							SELECT NOT complete_deferred FROM pgroll.migrations WHERE name = '01_first'`).Scan(&drained))
						return drained
					}, 10*time.Second, 50*time.Millisecond)

					windowErr := execAs(t, db, targetSchema, `INSERT INTO w (id, x) VALUES (3, 30)`)

					_, err = blocker.ExecContext(ctx, `ROLLBACK`)
					require.NoError(t, err)
					require.NoError(t, <-done)
					require.NoError(t, execAs(t, db, targetSchema, `INSERT INTO w (id, x) VALUES (4, 40)`))
					require.NoError(t, windowErr, "the new revision's write between two drains")
				})
		})
	}
}

// A train with no trigger leaves nothing of pgroll's in the migrated schema.
func TestTrainWithoutTriggersInstallsNoTargetFunction(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		baseW(t, mig)
		applyExpandOnlyTrain(t, mig, []*migrations.Migration{rawSQL("09_later", `COMMENT ON TABLE w IS 'x'`, `SELECT 1`)})
		require.Zero(t, pgrollLeftovers(t, db), "nothing of pgroll's in the schema while the deployment waits for its contraction")
	})
}

// A data migration later in the train writes the column an earlier
// migration added with `up`. pgroll's session is not pinned to a version
// schema, and it can name the column, so the values it writes stay; a row
// it inserts without one is filled.
func TestTrainLaterDataMigrationWritesAnAddedColumn(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		baseW(t, mig)
		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			addColumn("01_add_a", "a", "x + 1"),
			rawSQL("02_populate", `
				UPDATE w SET a = 500 WHERE id = 1;
				INSERT INTO w (id, x, a) VALUES (7, 70, 700);
				INSERT INTO w (id, x) VALUES (8, 80)`, `SELECT 1`),
			addColumn("03_add_b", "b", "x + 2"),
			laterIndex("w", "x"),
		})

		assert.Equal(t, 500, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 1`),
			"the value the data migration's UPDATE set, after a later backfill of the table")
		assert.Equal(t, 700, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 7`),
			"the value the data migration's INSERT set")
		assert.Equal(t, 81, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 8`),
			"the row the data migration inserted without a value")

		require.NoError(t, execAs(t, db, previousSchema, `UPDATE w SET x = 11 WHERE id = 1`))
		assert.Equal(t, 12, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 1`),
			"the previous revision cannot name the column, so its update recomputes it")

		require.NoError(t, mig.Complete(t.Context()))
		assert.Equal(t, 700, *intPtr(db, t, `SELECT a FROM public.w WHERE id = 7`), "after contraction")
		require.Zero(t, pgrollLeftovers(t, db))
	})
}

// A column with a constant default and an `up` derived from the row: the
// backfill reaches rows that already hold the default.
func TestTrainAddColumnWithDefaultAndDerivedUpIsBackfilled(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		baseW(t, mig)
		no := "false"
		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			{Name: "01_add_flag", Operations: migrations.Operations{&migrations.OpAddColumn{
				Table: "w", Up: "x IS NOT NULL",
				Column: migrations.Column{Name: "has_x", Type: "boolean", Nullable: false, Default: &no},
			}}},
			laterIndex("w", "x"),
		})

		var filled bool
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT has_x FROM public.w WHERE id = 1`).Scan(&filled))
		assert.True(t, filled, "the row that existed before the migration")

		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO w (id, x) VALUES (2, 20)`))
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT has_x FROM public.w WHERE id = 2`).Scan(&filled))
		assert.True(t, filled, "a row the previous revision inserts")
	})
}

// One migration adds two columns with `up` and puts a foreign key on them,
// which duplicates them under the names they would have started with.
func TestAddColumnsWithUpAndAConstraintOnThemInOneMigration(t *testing.T) {
	t.Parallel()

	rekey := func(name string) *migrations.Migration {
		return &migrations.Migration{Name: name, Operations: migrations.Operations{
			&migrations.OpAddColumn{Table: "h", Up: "(SELECT p.id FROM p WHERE p.acct = owner)", Column: migrations.Column{Name: "p_id", Type: "integer", Nullable: false}},
			&migrations.OpAddColumn{Table: "h", Up: "(SELECT p.lane FROM p WHERE p.acct = owner)", Column: migrations.Column{Name: "lane", Type: "integer", Nullable: false}},
			&migrations.OpCreateConstraint{
				Name: "fk_h_p_lane", Table: "h", Type: "foreign_key", Columns: []string{"p_id", "lane"},
				References: &migrations.TableForeignKeyReference{Table: "p", Columns: []string{"id", "lane"}},
				Up:         map[string]string{"p_id": "p_id", "lane": "lane"},
				Down:       map[string]string{"p_id": "p_id", "lane": "lane"},
			},
			&migrations.OpDropColumn{Table: "h", Column: "owner", Down: "(SELECT p.acct FROM p WHERE p.id = p_id)"},
		}}
	}
	const base = `CREATE TABLE p (id integer PRIMARY KEY, lane integer, acct integer UNIQUE, UNIQUE (id, lane));
		CREATE TABLE h (id integer PRIMARY KEY, owner integer NOT NULL);
		INSERT INTO p VALUES (1, 100, 7);
		INSERT INTO h VALUES (1, 7)`
	check := func(t *testing.T, db *sql.DB) {
		t.Helper()
		require.Equal(t, 100, *intPtr(db, t, `SELECT lane FROM public.h WHERE id = 1`))
		require.Zero(t, pgrollLeftovers(t, db))
		var notNull bool
		require.NoError(t, db.QueryRowContext(context.Background(), `
			SELECT bool_and(attnotnull) FROM pg_attribute
			WHERE attrelid = 'public.h'::regclass AND attname IN ('p_id', 'lane')`).Scan(&notNull))
		require.True(t, notNull, "both columns end NOT NULL")
	}

	t.Run("on its own", func(t *testing.T) {
		t.Parallel()
		testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			require.NoError(t, mig.Start(ctx, baseMigration(base), backfill.NewConfig()))
			require.NoError(t, mig.Complete(ctx))
			require.NoError(t, mig.Start(ctx, rekey("09_later"), backfill.NewConfig()))
			require.NoError(t, mig.Complete(ctx))
			check(t, db)
		})
	})
	t.Run("mid-train", func(t *testing.T) {
		t.Parallel()
		testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			require.NoError(t, mig.Start(ctx, baseMigration(base), backfill.NewConfig()))
			require.NoError(t, mig.Complete(ctx))
			applyExpandOnlyTrain(t, mig, []*migrations.Migration{rekey("01_rekey"), rawSQL("09_later", `SELECT 1`, `SELECT 1`)})
			require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO p (id, lane, acct) VALUES (2, 200, 8)`))
			require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO h (id, owner) VALUES (2, 8)`))
			require.NoError(t, execAs(t, db, targetSchema, `INSERT INTO h (id, p_id, lane) VALUES (3, 1, 100)`))
			require.NoError(t, mig.Complete(ctx))
			require.Equal(t, 200, *intPtr(db, t, `SELECT lane FROM public.h WHERE id = 2`))
			check(t, db)
		})
	})
}

// A migration adds one column with `up` and others without. It waits for
// the train's contraction as a whole, so a later migration names any of its
// columns, and a generated default is not generated twice.
func TestTrainLaterSQLNamesEveryColumnOfAWaitingMigration(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		baseW(t, mig)
		yes, uuid := "true", "gen_random_uuid()"
		applyExpandOnlyTrain(t, mig, []*migrations.Migration{
			{Name: "01_add_three", Operations: migrations.Operations{
				&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: false}},
				&migrations.OpAddColumn{Table: "w", Column: migrations.Column{Name: "speaks", Type: "boolean", Nullable: false, Default: &yes}},
				&migrations.OpAddColumn{Table: "w", Up: uuid, Column: migrations.Column{Name: "public_id", Type: "uuid", Nullable: false, Default: &uuid}},
			}},
			rawSQL("02_backfill", `UPDATE w SET speaks = false WHERE x > 5`, `SELECT 1`),
			{Name: "03_index", Operations: migrations.Operations{&migrations.OpCreateIndex{
				Name: "ix_w_speaking", Table: "w", Columns: []migrations.IndexField{{Column: "a"}}, Predicate: "speaks",
			}}},
			laterIndex("w", "x"),
		})

		read := func(id int) string {
			var v string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT public_id FROM public_09_later.w WHERE id = $1`, id).Scan(&v))
			return v
		}
		handedOut := read(1)
		require.NoError(t, execAs(t, db, previousSchema, `UPDATE w SET x = x + 1 WHERE id = 1`))
		require.Equal(t, handedOut, read(1), "the generated value survives the previous revision's update")
		require.Equal(t, 12, *intPtr(db, t, `SELECT a FROM public_09_later.w WHERE id = 1`))

		require.NoError(t, execAs(t, db, previousSchema, `INSERT INTO w (id, x) VALUES (2, 20)`))
		require.NotEmpty(t, read(2), "the previous revision's insert gets a generated value")

		require.NoError(t, mig.Complete(ctx))
		require.Equal(t, handedOut, read(1))
		require.Zero(t, pgrollLeftovers(t, db))
	})
}
