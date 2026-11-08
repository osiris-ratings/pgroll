// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

// Contraction keeps only the live version schema and drops every other, so it
// must never run without it: these tests hold it to refusing, with every
// version schema left as it was, and to `pgroll materialize` putting the live
// one back as its Start projected it.

func versionSchemas(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT nspname FROM pg_namespace WHERE nspname LIKE $1 ORDER BY 1`, cSchema+`\_%`)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		require.NoError(t, rows.Scan(&s))
		out = append(out, s)
	}
	require.NoError(t, rows.Err())
	return out
}

// TestContractionRefusesWhenLiveSchemaMissing: the live version schema is
// gone (an older binary dropped it, or an operator did). Contracting would
// drop every other version schema and leave apps none, so it refuses, on
// both paths, and the queued contraction resuming does not excuse it.
func TestContractionRefusesWhenLiveSchemaMissing(t *testing.T) {
	t.Parallel()

	for _, p := range contractionPaths {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				createBase(t, mig, db)
				p.apply(t, mig, []*migrations.Migration{
					{Name: "01_drop_value", Operations: migrations.Operations{
						&migrations.OpDropColumn{Table: "t", Column: "value", Down: "''"},
					}},
					leafCreatingEvents("02_leaf"),
				})
				// An interrupted contraction: the deployment is sealed.
				_, err := mig.State().MarkSealed(ctx, cSchema)
				require.NoError(t, err)
				_, err = db.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA %s CASCADE", roll.VersionedSchemaName(cSchema, "02_leaf")))
				require.NoError(t, err)

				require.ErrorContains(t, pgrollComplete(ctx, mig), "pgroll materialize")
				assert.Equal(t, []string{roll.VersionedSchemaName(cSchema, "00_base")}, versionSchemas(t, db),
					"no version schema is created or dropped")
				assert.True(t, columnExists(t, db, cSchema, "t", "value"), "nothing is contracted")
			})
		})
	}
}

// TestContractionKeepsAppsSchemaUnderBaselineLeaf: a baseline row is sealed
// and never had a version schema. Contracting with it as the leaf must not
// build one and then drop the schema apps are pinned to.
func TestContractionKeepsAppsSchemaUnderBaselineLeaf(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		createBase(t, mig, db)
		apps := roll.VersionedSchemaName(cSchema, "00_base")
		require.NoError(t, mig.CreateBaseline(ctx, "01_baseline"))

		require.NoError(t, pgrollComplete(ctx, mig))

		assert.Equal(t, []string{apps}, versionSchemas(t, db),
			"the schema apps are pinned to must survive, and no schema is built for the baseline")
	})
}

// TestContractionKeepsAppsSchemaForUnfinishedBatch: a `pgroll migrate` died
// after an inline intermediate and before the leaf started. The first
// `pgroll complete` seals the intermediate; the second must not then treat it
// as a contracted leaf, build its schema and drop the apps' schema.
func TestContractionKeepsAppsSchemaForUnfinishedBatch(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		createBase(t, mig, db)
		apps := roll.VersionedSchemaName(cSchema, "00_base")
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name:       "01_add_table",
			Operations: migrations.Operations{createTableOp("added")},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithSkipSchemaDrop()))

		for range 2 {
			require.NoError(t, pgrollComplete(ctx, mig))
		}

		assert.Equal(t, []string{apps}, versionSchemas(t, db),
			"the schema apps are pinned to must survive, and no schema is built for the intermediate")
	})
}

// TestCompleteRefusesActiveIntermediate: a `pgroll migrate` died with an
// intermediate started, without a version schema, and still active.
// Completing it would keep its schema, which does not exist, and drop the
// one apps are pinned to.
func TestCompleteRefusesActiveIntermediate(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		createBase(t, mig, db)
		apps := roll.VersionedSchemaName(cSchema, "00_base")
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name:       "01_add_table",
			Operations: migrations.Operations{createTableOp("added")},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))

		require.ErrorContains(t, pgrollComplete(ctx, mig), "re-running `pgroll migrate`")
		assert.Equal(t, []string{apps}, versionSchemas(t, db), "the schema apps are pinned to must survive")
	})
}

// TestMaterializeReplaysQueuedContraction: with nothing active and
// contraction queued, `pgroll materialize` projects the queued Starts, as the
// leaf's Start did: a column a queued duplicator rewrites reads its
// duplicate. Projected from the bare tables, the view would read the
// original, which the contraction drops.
func TestMaterializeReplaysQueuedContraction(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		createBase(t, mig, db)
		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{Name: "01_value_to_text", Operations: migrations.Operations{
				&migrations.OpAlterColumn{Table: "t", Column: "value", Type: ptr("text"), Up: "value", Down: "value"},
			}},
			leafCreatingEvents("02_leaf"),
		})
		live := roll.VersionedSchemaName(cSchema, "02_leaf")
		_, err := db.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA %s CASCADE", live))
		require.NoError(t, err)

		sc, err := mig.ReadSchemaWithDeferred(ctx)
		require.NoError(t, err)
		require.NoError(t, mig.Materialize(ctx, "02_leaf", sc))

		var def string
		require.NoError(t, db.QueryRow(fmt.Sprintf("SELECT pg_get_viewdef('%s.t'::regclass)", live)).Scan(&def))
		assert.Contains(t, def, migrations.TemporaryName(migrations.MigrationScopeFor("01_value_to_text"), "value"),
			"the live view must read the queued duplicate")

		_, _, err = mig.FinishContraction(ctx)
		require.NoError(t, err)
		assertLiveSchemaServes(t, db, "02_leaf", "t", "hot", "events")
		assert.Equal(t, "text", columnType(t, db, "t", "value"))
	})
}

// TestTailLocksTableBeforeSequence: an app transaction holds a row lock on s
// and then calls nextval on s's sequence while the duplicator tail runs. The
// tail must wait for the table first, holding nothing on the sequence, or the
// two deadlock once lock_timeout exceeds deadlock_timeout.
func TestTailLocksTableBeforeSequence(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, cSchema,
		[]roll.Option{roll.WithLockTimeoutMs(10000)},
		func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			createBase(t, mig, db)
			_, err := db.Exec(`CREATE TABLE s (id serial PRIMARY KEY, v text); INSERT INTO s (v) VALUES ('a')`)
			require.NoError(t, err)
			applyAdditiveTrain(t, mig, []*migrations.Migration{
				{Name: "01_s_id_bigint", Operations: migrations.Operations{
					&migrations.OpAlterColumn{Table: "s", Column: "id", Type: ptr("bigint"), Up: "id", Down: "id"},
				}},
				leafCreatingEvents("02_leaf"),
			})

			app, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = app.Rollback() }()
			_, err = app.ExecContext(ctx, "UPDATE s SET v = 'b' WHERE id = 1")
			require.NoError(t, err)

			done := make(chan error, 1)
			go func() { done <- mig.Complete(ctx) }()

			// Wait until the tail is waiting for the table the app holds.
			require.Eventually(t, func() bool {
				var waiting bool
				require.NoError(t, db.QueryRow(`SELECT EXISTS (
					SELECT 1 FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
					WHERE NOT l.granted AND a.application_name = 'pgroll'
					  AND l.relation = 's'::regclass)`).Scan(&waiting))
				return waiting
			}, 20*time.Second, 50*time.Millisecond)

			_, err = app.ExecContext(ctx, "SELECT nextval(pg_get_serial_sequence('s', 'id'))")
			require.NoError(t, err, "the app must not deadlock with the tail")
			require.NoError(t, app.Commit())
			require.NoError(t, <-done)
		})
}

// transactionControlCases are onComplete statements that end the transaction
// contraction runs them in, as an independent review of #68 reproduced:
// contracting the first three times applied the UPDATE three times and never
// finished; the second left the live view dropped and every retry failing.
var transactionControlCases = map[string]string{
	"update":  "BEGIN; UPDATE t SET value = value || 'x'; COMMIT;",
	"cascade": "BEGIN; ALTER TABLE t DROP COLUMN value CASCADE; COMMIT;",
}

// TestOnCompleteRawSQLWithTransactionControlIsInvalid: such a statement fails
// validation, so the train fails before anything is applied.
func TestOnCompleteRawSQLWithTransactionControlIsInvalid(t *testing.T) {
	t.Parallel()

	for name, stmt := range transactionControlCases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				createBase(t, mig, db)

				err := mig.Start(ctx, rawOnComplete("01_raw", stmt), backfill.NewConfig())
				require.ErrorContains(t, err, "must not control transactions")

				active, err := mig.State().IsActiveMigrationPeriod(ctx, cSchema)
				require.NoError(t, err)
				assert.False(t, active, "nothing was started")
			})
		})
	}
}

// TestContractionRefusesRawSQLWithTransactionControl: such a statement that
// is already queued (applied by an older pgroll) is refused before it runs,
// every time, with the data and the live view as they were.
func TestContractionRefusesRawSQLWithTransactionControl(t *testing.T) {
	t.Parallel()

	for name, stmt := range transactionControlCases {
		for _, p := range contractionPaths {
			t.Run(name+"/"+p.name, func(t *testing.T) {
				t.Parallel()
				testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, cSchema,
					[]roll.Option{roll.WithSkipValidation(true)},
					func(mig *roll.Roll, db *sql.DB) {
						ctx := context.Background()
						createBase(t, mig, db)
						p.apply(t, mig, []*migrations.Migration{rawOnComplete("01_raw", stmt), leafCreatingEvents("02_leaf")})

						for range 3 {
							require.ErrorContains(t, pgrollComplete(ctx, mig), "must not control transactions")
						}
						var value string
						require.NoError(t, db.QueryRow("SELECT value FROM t WHERE id = 1").Scan(&value))
						assert.Equal(t, "a", value, "the statement must not have run")
						assertLiveSchemaServes(t, db, "02_leaf", "t", "hot", "events")
						assert.True(t, viewHasColumn(t, db, roll.VersionedSchemaName(cSchema, "02_leaf"), "t", "value"))
					})
			})
		}
	}
}

// TestContractionRefusesWhenLiveViewMissing: a live view is gone before
// `pgroll complete` runs. Contracting would finish with apps still unable to
// read that table, so it refuses, naming the view. Tables created after the
// live schema (a partition pg_partman adds, a table added by hand) were never
// projected and are not missing. Once materialize creates the view, it
// contracts.
func TestContractionRefusesWhenLiveViewMissing(t *testing.T) {
	t.Parallel()

	for _, p := range contractionPaths {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				createBase(t, mig, db)
				_, err := db.ExecContext(ctx, `CREATE TABLE pt (id integer, at date) PARTITION BY RANGE (at);
					CREATE TABLE pt_2026 PARTITION OF pt FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`)
				require.NoError(t, err)
				p.apply(t, mig, []*migrations.Migration{
					{Name: "01_drop_value", Operations: migrations.Operations{
						&migrations.OpDropColumn{Table: "t", Column: "value", Down: "''"},
					}},
					leafCreatingEvents("02_leaf"),
				})
				live := roll.VersionedSchemaName(cSchema, "02_leaf")
				_, err = db.ExecContext(ctx, fmt.Sprintf(`
					CREATE TABLE pt_2027 PARTITION OF pt FOR VALUES FROM ('2027-01-01') TO ('2028-01-01');
					CREATE TABLE added_later (id integer);
					DROP VIEW %s.hot`, live))
				require.NoError(t, err)

				err = pgrollComplete(ctx, mig)
				require.ErrorContains(t, err, "missing view "+liveView("02_leaf", "hot")+",")
				assert.NotContains(t, err.Error(), "pt_2027")
				assert.NotContains(t, err.Error(), "added_later")
				assert.Equal(t, []string{roll.VersionedSchemaName(cSchema, "00_base"), live}, versionSchemas(t, db),
					"no version schema is dropped")
				assert.True(t, columnExists(t, db, cSchema, "t", "value"), "nothing is contracted")

				sc, err := mig.ReadSchemaWithDeferred(ctx)
				require.NoError(t, err)
				require.NoError(t, mig.Materialize(ctx, "02_leaf", sc))
				require.NoError(t, pgrollComplete(ctx, mig))
				assertLiveSchemaServes(t, db, "02_leaf", "t", "hot", "events")
			})
		})
	}
}

// failMatching fails every statement run on pgroll's DDL connection whose
// text contains match.
type failMatching struct {
	db.DB
	match string
}

func (f *failMatching) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	if strings.Contains(query, f.match) {
		return nil, fmt.Errorf("injected failure: %s", query)
	}
	return f.DB.ExecContext(ctx, query, args...)
}

// TestTailRetryDoesNotLockTable: the duplicator tail committed and the
// contraction then failed. Its retry has nothing left to do to the table, so
// it must not wait for an ACCESS EXCLUSIVE lock on it for the tail.
func TestTailRetryDoesNotLockTable(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, cSchema,
		[]roll.Option{roll.WithLockTimeoutMs(10000)},
		func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			createBase(t, mig, db)
			applyAdditiveTrain(t, mig, []*migrations.Migration{
				{Name: "01_value_to_text", Operations: migrations.Operations{
					&migrations.OpAlterColumn{Table: "t", Column: "value", Type: ptr("text"), Up: "value", Down: "value"},
				}},
				leafCreatingEvents("02_leaf"),
			})

			// The tail commits; dropping the backfill marker after it fails.
			fc := &failMatching{match: "_pgroll_needs_backfill"}
			fc.DB = roll.SwapConnForTest(mig, fc)
			require.Error(t, mig.Complete(ctx))
			roll.SwapConnForTest(mig, fc.DB)
			require.Equal(t, "text", columnType(t, db, "t", "value"), "the tail committed")

			app, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			defer func() { _ = app.Rollback() }()
			_, err = app.ExecContext(ctx, "SELECT 1 FROM t LIMIT 1")
			require.NoError(t, err)

			done := make(chan error, 1)
			go func() { done <- mig.Complete(ctx) }()

			var waiting string
			require.Eventually(t, func() bool {
				return db.QueryRow(`SELECT a.query FROM pg_locks l JOIN pg_stat_activity a ON a.pid = l.pid
					WHERE NOT l.granted AND a.application_name = 'pgroll' AND l.relation = 't'::regclass`).
					Scan(&waiting) == nil
			}, 20*time.Second, 50*time.Millisecond)
			assert.NotContains(t, waiting, "LOCK TABLE", "the retry's tail must not lock the table")

			require.NoError(t, app.Commit())
			require.NoError(t, <-done)
			assertLiveSchemaServes(t, db, "02_leaf", "t", "hot", "events")
		})
}
