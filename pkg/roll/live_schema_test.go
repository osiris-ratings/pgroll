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

// These tests hold contraction to one invariant: the live version schema,
// the one apps are pinned to, is never left missing views — not when a
// drain fails, not when it is cancelled, and not when an earlier binary left
// it dropped. All but the fallback cases fail on the drop-then-drain
// contraction this replaced. live_schema_invariant_test.go walks every
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

// TestContractionKeepsLiveSchemaWhenFallbackCancelled: a raw statement live
// views block takes the fallback (one transaction that drops the blocking
// views, runs it and recreates them), and that transaction is cancelled. It
// rolls back, so the live views are as they were.
func TestContractionKeepsLiveSchemaWhenFallbackCancelled(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		createBase(t, mig, db)

		applyAdditiveTrain(t, mig, []*migrations.Migration{
			rawOnComplete("01_drop_then_sleep", "ALTER TABLE t DROP COLUMN value; SELECT pg_sleep(30)"),
			leafCreatingEvents("02_leaf"),
		})

		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		require.Error(t, mig.Complete(ctx), "the cancelled drain must fail")
		assertLiveSchemaServes(t, db, "02_leaf", "t", "hot", "events")
		// The cancel rolled the statement back, so the view still has value.
		assert.True(t, viewHasColumn(t, db, roll.VersionedSchemaName(cSchema, "02_leaf"), "t", "value"))
	})
}

// TestContractionFallsBackWhenLiveViewBlocks: raw SQL that a live view
// depends on (2BP01, 0A000) runs in one transaction that drops the blocking
// views and recreates them from what the statement leaves.
func TestContractionFallsBackWhenLiveViewBlocks(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		sql        string
		valueAfter bool
	}{
		"drop_projected_column":   {sql: "ALTER TABLE t DROP COLUMN value", valueAfter: false},
		"alter_type_of_projected": {sql: "ALTER TABLE t ALTER COLUMN value TYPE text", valueAfter: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				createBase(t, mig, db)

				applyAdditiveTrain(t, mig, []*migrations.Migration{
					rawOnComplete("01_raw", tc.sql),
					leafCreatingEvents("02_leaf"),
				})

				require.NoError(t, mig.Complete(ctx))
				assertLiveSchemaServes(t, db, "02_leaf", "t", "hot", "events")
				assert.Equal(t, tc.valueAfter,
					viewHasColumn(t, db, roll.VersionedSchemaName(cSchema, "02_leaf"), "t", "value"))
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

// TestContractionRestoresLiveSchemaLeftDropped: an older binary's
// contraction dropped the live schema, drained the raw SQL and stopped. What
// is left to drain is typed, so nothing in it asks for a rebuild; the
// contraction must still put the live schema back, in each shape it can be
// resumed from.
func TestContractionRestoresLiveSchemaLeftDropped(t *testing.T) {
	t.Parallel()

	train := func() []*migrations.Migration {
		return []*migrations.Migration{
			rawOnComplete("01_raw", "SELECT 1"),
			{Name: "02_drop_value", Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "t", Column: "value", Down: "''"},
			}},
			leafCreatingEvents("03_leaf"),
		}
	}
	live := roll.VersionedSchemaName(cSchema, "03_leaf")

	// leaveDropped reproduces what the old contraction left: the deployment
	// sealed, the raw SQL drained, the live schema gone.
	leaveDropped := func(t *testing.T, mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		_, err := mig.State().MarkSealed(ctx, cSchema)
		require.NoError(t, err)
		require.NoError(t, mig.State().ClearCompleteDeferred(ctx, cSchema, "01_raw"))
		_, err = db.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA %s CASCADE", live))
		require.NoError(t, err)
	}

	t.Run("active_leaf", func(t *testing.T) {
		t.Parallel()
		testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
			createBase(t, mig, db)
			applyAdditiveTrain(t, mig, train())
			leaveDropped(t, mig, db)

			require.NoError(t, mig.Complete(context.Background()))
			assertLiveSchemaServes(t, db, "03_leaf", "t", "hot", "events")
		})
	})

	t.Run("queued_leaf", func(t *testing.T) {
		t.Parallel()
		testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
			createBase(t, mig, db)
			applyDelayedContractionTrain(t, mig, train())
			leaveDropped(t, mig, db)

			_, _, err := mig.FinishContraction(context.Background())
			require.NoError(t, err)
			assertLiveSchemaServes(t, db, "03_leaf", "t", "hot", "events")
		})
	})

	t.Run("contracted_leaf", func(t *testing.T) {
		t.Parallel()
		testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			createBase(t, mig, db)
			applyAdditiveTrain(t, mig, train())
			require.NoError(t, mig.Complete(ctx))
			// An older binary's retry finished the contraction without
			// rebuilding the schema it had dropped.
			_, err := db.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA %s CASCADE", live))
			require.NoError(t, err)

			_, _, err = mig.FinishContraction(ctx)
			require.NoError(t, err)
			assertLiveSchemaServes(t, db, "03_leaf", "t", "hot", "events")
		})
	})
}

// TestRestoreActiveVersionSchema is the recovery `pgroll materialize` runs
// while a migration is in progress: the active migration's version schema
// is re-projected as its Start projected it.
func TestRestoreActiveVersionSchema(t *testing.T) {
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
// sealed, the original `id` dropped (and the primary key with it), the
// duplicate not yet renamed, and, as that pgroll dropped it before draining,
// the live schema gone. Returns the live schema.
func orphanPrimaryKeyDuplicate(t *testing.T, mig *roll.Roll, db *sql.DB) string {
	t.Helper()
	ctx := context.Background()
	createBase(t, mig, db)
	applyAdditiveTrain(t, mig, []*migrations.Migration{
		{Name: "01_id_to_bigint", Operations: migrations.Operations{
			&migrations.OpAlterColumn{Table: "t", Column: "id", Type: ptr("bigint"), Up: "id", Down: "id"},
		}},
		leafCreatingEvents("02_leaf"),
	})
	live := roll.VersionedSchemaName(cSchema, "02_leaf")

	_, err := mig.State().MarkSealed(ctx, cSchema)
	require.NoError(t, err)
	// That pgroll dropped the previous deployment's schema and the live one
	// before draining.
	for _, version := range []string{"00_base", "02_leaf"} {
		_, err = db.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA %s CASCADE", roll.VersionedSchemaName(cSchema, version)))
		require.NoError(t, err)
	}
	_, err = db.ExecContext(ctx, "ALTER TABLE t DROP COLUMN id")
	require.NoError(t, err)
	return live
}

// TestResumedDuplicatorKeepsPrimaryKey: the resume takes the primary key from
// pgroll's schema snapshot. The catalog lost it with the dropped column, and
// resuming from the catalog finished with the table's primary key gone.
func TestResumedDuplicatorKeepsPrimaryKey(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		orphanPrimaryKeyDuplicate(t, mig, db)

		require.NoError(t, pgrollComplete(context.Background(), mig))

		var name, columns string
		require.NoError(t, db.QueryRow(`
			SELECT c.conname, string_agg(a.attname, ',' ORDER BY a.attnum)
			FROM pg_constraint c
			JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
			WHERE c.conrelid = 'public.t'::regclass AND c.contype = 'p'
			GROUP BY c.conname`).Scan(&name, &columns), "t must still have a primary key")
		assert.Equal(t, "t_pkey", name)
		assert.Equal(t, "id", columns)
		assert.Equal(t, "bigint", columnType(t, db, "t", "id"))
	})
}

// TestRestoredLiveViewExposesOrphanedDuplicate: with the original dropped
// and the duplicate not yet renamed, the live view is rebuilt from the
// snapshot's column mapping, so it exposes `id` with the duplicate's data.
// Projecting from the catalog alone dropped `id` from the view.
func TestRestoredLiveViewExposesOrphanedDuplicate(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		live := orphanPrimaryKeyDuplicate(t, mig, db)

		_, _, err := mig.RestoreActiveVersionSchema(context.Background())
		require.NoError(t, err)

		require.True(t, viewHasColumn(t, db, live, "t", "id"), "the live view must expose the user's column")
		var id int64
		var value string
		require.NoError(t, db.QueryRow(fmt.Sprintf("SELECT id, value FROM %s.t", live)).Scan(&id, &value))
		assert.Equal(t, int64(1), id, "with the duplicate's data")
		assert.Equal(t, "a", value)
	})
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
