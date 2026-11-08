// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

// These tests hold contraction to one invariant: it never drops or rebuilds
// the live version schema, the one apps are pinned to — not when a drain
// fails, not when it is cancelled — and it refuses, leaving the schema as it
// was, whatever would break that. live_schema_invariant_test.go walks every
// statement boundary and probes for read gaps.

// applyAdditiveTrain applies a train the way `pgroll migrate` does on a
// shared fleet: intermediates without a version schema, deferred or inline
// as their class requires, and the final migration started and left active,
// its version schema live, for a later `pgroll complete`.
func applyAdditiveTrain(t *testing.T, mig *roll.Roll, train []*migrations.Migration) {
	t.Helper()
	ctx := context.Background()

	for i, m := range train {
		if i == len(train)-1 {
			require.NoError(t, mig.Start(ctx, m, backfill.NewConfig()))
			continue
		}
		require.NoError(t, mig.Start(ctx, m, backfill.NewConfig(), roll.WithoutVersionSchema()))
		if m.CompleteMustBeDeferred() {
			require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))
		} else {
			require.NoError(t, mig.Complete(ctx, roll.WithSkipSchemaDrop()))
		}
	}
}

// createBase creates the previous deployment: `t (id, value)` with a row,
// and an unrelated table `hot`.
func createBase(t *testing.T, mig *roll.Roll, db *sql.DB) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, mig.Start(ctx, &migrations.Migration{
		Name: "00_base",
		Operations: migrations.Operations{
			&migrations.OpCreateTable{Name: "t", Columns: []migrations.Column{
				{Name: "id", Type: "integer", Pk: true},
				{Name: "value", Type: "varchar(255)", Nullable: true},
			}},
			createTableOp("hot"),
		},
	}, backfill.NewConfig()))
	require.NoError(t, mig.Complete(ctx))
	_, err := db.ExecContext(ctx, `INSERT INTO public.t (id, value) VALUES (1, 'a')`)
	require.NoError(t, err)
}

func rawOnComplete(name, sql string) *migrations.Migration {
	return &migrations.Migration{
		Name:       name,
		Operations: migrations.Operations{&migrations.OpRawSQL{Up: sql, OnComplete: true}},
	}
}

func leafCreatingEvents(name string) *migrations.Migration {
	return &migrations.Migration{
		Name:       name,
		Operations: migrations.Operations{createTableOp("events")},
	}
}

// assertLiveSchemaServes checks that apps pinned to the live schema can read
// every table it projects.
func assertLiveSchemaServes(t *testing.T, db *sql.DB, version string, tables ...string) {
	t.Helper()
	live := roll.VersionedSchemaName(cSchema, version)
	require.True(t, schemaExists(t, db, live), "live version schema %q must exist", live)
	for _, table := range tables {
		_, err := db.Exec(fmt.Sprintf("SELECT * FROM %s.%s", live, table))
		require.NoError(t, err, "live view %s.%s must be selectable", live, table)
	}
}

// TestContractionKeepsLiveSchemaWhenDrainFails: onComplete raw SQL drains,
// then a typed drain fails on a lock. The live views must stay selectable,
// and the retry must succeed with them in place.
func TestContractionKeepsLiveSchemaWhenDrainFails(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
		[]roll.Option{roll.WithLockTimeoutMs(50), roll.WithLockRetryTimeout(-1)},
		func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			createBase(t, mig, db)

			applyAdditiveTrain(t, mig, []*migrations.Migration{
				rawOnComplete("01_raw", "SELECT 1"),
				{Name: "02_drop_value", Operations: migrations.Operations{
					&migrations.OpDropColumn{Table: "t", Column: "value", Down: "''"},
				}},
				leafCreatingEvents("03_leaf"),
			})

			release := holdAccessShareOnView(t, db, "public.t")
			err := mig.Complete(ctx)
			require.Error(t, err, "the drop_column drain must fail while t is locked")
			assertLiveSchemaServes(t, db, "03_leaf", "t", "hot", "events")
			release()

			require.NoError(t, mig.Complete(ctx), "the retry must contract once the lock is gone")
			assertLiveSchemaServes(t, db, "03_leaf", "t", "hot", "events")
			assert.False(t, viewHasColumn(t, db, roll.VersionedSchemaName(cSchema, "03_leaf"), "t", "value"))
		})
}

// TestContractionKeepsLiveSchemaWhenCancelled is the incident: the operator
// interrupts `pgroll complete` while onComplete raw SQL is running. The live
// schema must still be there afterwards.
func TestContractionKeepsLiveSchemaWhenCancelled(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		createBase(t, mig, db)

		applyAdditiveTrain(t, mig, []*migrations.Migration{
			rawOnComplete("01_slow_raw", "SELECT pg_sleep(30)"),
			leafCreatingEvents("02_leaf"),
		})

		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		require.Error(t, mig.Complete(ctx), "the cancelled drain must fail")
		assertLiveSchemaServes(t, db, "02_leaf", "t", "hot", "events")
	})
}

// TestContractionRefusesRawSQLBlockedByLiveView: raw SQL that a live view
// depends on (2BP01, 0A000) fails the contraction with the view named. The
// view and the table stay as they were, and a retry fails the same way.
func TestContractionRefusesRawSQLBlockedByLiveView(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"drop_projected_column":   "ALTER TABLE t DROP COLUMN value",
		"alter_type_of_projected": "ALTER TABLE t ALTER COLUMN value TYPE text",
	}
	for name, stmt := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				createBase(t, mig, db)

				applyAdditiveTrain(t, mig, []*migrations.Migration{
					rawOnComplete("01_raw", stmt),
					leafCreatingEvents("02_leaf"),
				})

				for range 2 {
					require.ErrorContains(t, mig.Complete(ctx), "conflicts with view "+liveView("02_leaf", "t"))
				}
				assertLiveSchemaServes(t, db, "02_leaf", "t", "hot", "events")
				assert.True(t, viewHasColumn(t, db, roll.VersionedSchemaName(cSchema, "02_leaf"), "t", "value"))
				assert.Equal(t, "character varying", columnType(t, db, "t", "value"))
			})
		})
	}
}

// TestContractionRunsConstraintDrainInPlace is the incident's train: an
// onComplete ADD CONSTRAINT … NOT VALID, then an onComplete VALIDATE. Views
// never depend on constraints, so the drain must not touch the live schema:
// a backend holds a live view open with lock retries off, and any drop of
// the live schema would fail on it.
func TestContractionRunsConstraintDrainInPlace(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
		[]roll.Option{roll.WithLockTimeoutMs(50), roll.WithLockRetryTimeout(-1)},
		func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			createBase(t, mig, db)

			applyAdditiveTrain(t, mig, []*migrations.Migration{
				rawOnComplete("01_add_check",
					"ALTER TABLE t ADD CONSTRAINT t_value_present CHECK (value IS NOT NULL) NOT VALID"),
				rawOnComplete("02_validate_check", "ALTER TABLE t VALIDATE CONSTRAINT t_value_present"),
				leafCreatingEvents("03_leaf"),
			})

			live := roll.VersionedSchemaName(cSchema, "03_leaf")
			release := holdAccessShareOnView(t, db, live+".hot")
			defer release()

			require.NoError(t, mig.Complete(ctx), "the constraint drain must run with the live views in place")
			assertLiveSchemaServes(t, db, "03_leaf", "t", "hot", "events")

			var validated bool
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT convalidated FROM pg_constraint WHERE conname = 't_value_present'`).Scan(&validated))
			assert.True(t, validated, "the VALIDATE must have run")
		})
}

// TestRestoreActiveVersionSchema is the recovery `pgroll materialize` runs
// while a migration is in progress: what is missing from the active
// migration's version schema is created as its Start projected it. A view
// that exists is never rebuilt, and nothing is created when that Start
// cannot be replayed over the tables.
func TestRestoreActiveVersionSchema(t *testing.T) {
	t.Parallel()

	t.Run("creates what is missing, never rebuilds", func(t *testing.T) {
		t.Parallel()
		testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			createBase(t, mig, db)
			applyAdditiveTrain(t, mig, []*migrations.Migration{
				{Name: "01_add_label", Operations: migrations.Operations{
					&migrations.OpAddColumn{Table: "t", Column: migrations.Column{Name: "label", Type: "text", Nullable: true}},
				}},
			})
			live := roll.VersionedSchemaName(cSchema, "01_add_label")
			_, err := db.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA %s CASCADE", live))
			require.NoError(t, err)

			got, n, err := mig.RestoreActiveVersionSchema(ctx)
			require.NoError(t, err)
			assert.Equal(t, live, got)
			assert.Equal(t, 2, n, "views for t and hot")
			assertLiveSchemaServes(t, db, "01_add_label", "t", "hot")
			assert.True(t, viewHasColumn(t, db, live, "t", "label"),
				"the in-flight add_column must project under its real name, as Start projected it")

			_, n, err = mig.RestoreActiveVersionSchema(ctx)
			require.NoError(t, err)
			assert.Zero(t, n, "a complete live schema is left alone")

			// A view that exists is left as it is, even one that no longer
			// matches the projection: rebuilding it would re-lock what apps read.
			_, err = db.ExecContext(ctx, fmt.Sprintf("DROP VIEW %[1]s.t; CREATE VIEW %[1]s.t AS SELECT id FROM %[2]s.t",
				live, cSchema))
			require.NoError(t, err)
			_, n, err = mig.RestoreActiveVersionSchema(ctx)
			require.NoError(t, err)
			assert.Zero(t, n)
			assert.False(t, viewHasColumn(t, db, live, "t", "label"), "an existing view is never rebuilt")
		})
	})

	t.Run("creates nothing when the Start cannot be replayed", func(t *testing.T) {
		t.Parallel()
		testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			createBase(t, mig, db)
			applyAdditiveTrain(t, mig, []*migrations.Migration{
				{Name: "01_rename_value", Operations: migrations.Operations{
					&migrations.OpRenameColumn{Table: "t", From: "value", To: "label"},
				}},
			})
			live := roll.VersionedSchemaName(cSchema, "01_rename_value")
			_, err := db.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA %s CASCADE", live))
			require.NoError(t, err)
			// The rename ran physically, as a Complete cut short leaves it, so
			// the Start no longer replays: there is no `value` to rename.
			_, err = db.ExecContext(ctx, "ALTER TABLE t RENAME COLUMN value TO label")
			require.NoError(t, err)

			_, n, err := mig.RestoreActiveVersionSchema(ctx)
			require.Error(t, err, "a projection the Start cannot reproduce must not be guessed from the tables")
			assert.Zero(t, n)
			assert.False(t, schemaExists(t, db, live), "nothing is created")
		})
	})
}

func viewHasColumn(t *testing.T, db *sql.DB, schemaName, view, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = $2 AND column_name = $3
		)`, schemaName, view, column).Scan(&exists))
	return exists
}

// orphanPrimaryKeyDuplicate leaves the state an older pgroll's interrupted
// alter_column contraction left on a primary key column: the deployment
// sealed, the previous deployment's schema dropped, the original `id`
// dropped (and the primary key with it), and the duplicate not yet renamed.
func orphanPrimaryKeyDuplicate(t *testing.T, mig *roll.Roll, db *sql.DB, p contractionPath) {
	t.Helper()
	ctx := context.Background()
	createBase(t, mig, db)
	p.apply(t, mig, []*migrations.Migration{
		{Name: "01_id_to_bigint", Operations: migrations.Operations{
			&migrations.OpAlterColumn{Table: "t", Column: "id", Type: ptr("bigint"), Up: "id", Down: "id"},
		}},
		leafCreatingEvents("02_leaf"),
	})

	_, err := mig.State().MarkSealed(ctx, cSchema)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA %s CASCADE", roll.VersionedSchemaName(cSchema, "00_base")))
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, "ALTER TABLE t DROP COLUMN id")
	require.NoError(t, err)
}

// TestOrphanedDuplicateRefusesContraction: an older pgroll's contraction
// dropped a primary key column and stopped before renaming its duplicate into
// place. The primary key went with the column and the catalog cannot say what
// it was, so the contraction refuses, naming the table and column, rather than
// finish without it. The live schema is left as it was.
func TestOrphanedDuplicateRefusesContraction(t *testing.T) {
	t.Parallel()

	for _, p := range contractionPaths {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				orphanPrimaryKeyDuplicate(t, mig, db, p)

				err := pgrollComplete(context.Background(), mig)
				require.ErrorContains(t, err, `column "id" of table "t" is gone and its duplicate`)
				assert.Len(t, orphanedDuplicates(t, db), 1, "the duplicate is left for the operator")
				assertLiveSchemaServes(t, db, "02_leaf", "t", "hot", "events")
			})
		})
	}
}

// TestChainedDuplicatorsOnOneColumn: two duplicators on one column in a
// train (a CHECK added, then dropped), the examples' posts.user_id shape.
// The second must name its duplicate, triggers and schema entry after the
// logical column: its triggers write columns that exist, so a write through
// the live view does not fail, the live view reads the second duplicate,
// and contraction does not drop the column the live view reads.
//
// The written value is not asserted: an intermediate migration's triggers
// treat every write through the leaf's schema as an old-version write and
// recompute the duplicate from the original, a separate, pre-existing issue.
func TestChainedDuplicatorsOnOneColumn(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		createBase(t, mig, db)
		applyAdditiveTrain(t, mig, []*migrations.Migration{
			{Name: "01_add_check", Operations: migrations.Operations{
				&migrations.OpAlterColumn{
					Table: "t", Column: "value",
					Check: &migrations.CheckConstraint{Name: "t_value_short", Constraint: "length(value) < 100"},
					Up:    "value", Down: "value",
				},
			}},
			{Name: "02_drop_check", Operations: migrations.Operations{
				&migrations.OpDropConstraint{Table: "t", Name: "t_value_short", Up: "value", Down: "value"},
			}},
			leafCreatingEvents("03_leaf"),
		})
		live := roll.VersionedSchemaName(cSchema, "03_leaf")

		var def string
		require.NoError(t, db.QueryRow(fmt.Sprintf("SELECT pg_get_viewdef('%s.t'::regclass)", live)).Scan(&def))
		assert.Contains(t, def, migrations.TemporaryName(migrations.MigrationScopeFor("02_drop_check"), "value"),
			"the live view must read the last duplicate")

		_, err := db.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s.t (id, value) VALUES (2, 'b')", live))
		require.NoError(t, err, "a write through the live view must work while the train is expanded")

		require.NoError(t, mig.Complete(ctx))
		assertLiveSchemaServes(t, db, "03_leaf", "t", "hot", "events")
		var rows int
		require.NoError(t, db.QueryRow("SELECT count(*) FROM public.t WHERE id = 2").Scan(&rows))
		assert.Equal(t, 1, rows)
		assert.False(t, constraintExists(t, db, "t_value_short"))
		assert.Empty(t, orphanedDuplicates(t, db))
	})
}
