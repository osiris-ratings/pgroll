// SPDX-License-Identifier: Apache-2.0

package roll_test

// Adversarial review of 2ea2671. Builds with the base build too (6b2e5ea):
// it uses only the r2* helpers, which train_cases_test.go holds here and
// review2_test.go holds in the base copy.

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
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

func r3Columns(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `
		SELECT column_name || CASE WHEN is_nullable = 'NO' THEN '!' ELSE '' END
		       || COALESCE('=' || column_default, '')
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 ORDER BY ordinal_position`, table)
	require.NoError(t, err)
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		cols = append(cols, c)
	}
	require.NoError(t, rows.Err())
	return strings.Join(cols, ", ")
}

func r3Str(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	var v sql.NullString
	require.NoError(t, db.QueryRowContext(context.Background(), query).Scan(&v))
	if !v.Valid {
		return "<NULL>"
	}
	return v.String
}

func r3TryStr(db *sql.DB, query string) string {
	var v sql.NullString
	if err := db.QueryRowContext(context.Background(), query).Scan(&v); err != nil {
		return r3Err(err)
	}
	if !v.Valid {
		return "<NULL>"
	}
	return v.String
}

func r3Opts(versionSchemas bool) []roll.Option {
	return []roll.Option{roll.WithLockTimeoutMs(500), roll.WithVersionSchema(versionSchemas)}
}

// ---------------------------------------------------------------------------
// 1. An add_column that completes inline after a deferred drop or rename of
//    the column that had its name.
// ---------------------------------------------------------------------------

// The earlier migration's drop or rename waits for the contraction, so the
// old column still has the name when the add_column completes inline.
func TestTrainMatrixInlineAddColumnAfterADeferredDropOrRenameOfItsName(t *testing.T) {
	t.Parallel()

	seven, volatile := "7", "(random() * 100)::integer"
	earlier := map[string]migrations.Operation{
		"drop":   &migrations.OpDropColumn{Table: "w", Column: "x", Down: "0"},
		"rename": &migrations.OpRenameColumn{Table: "w", From: "x", To: "y"},
	}
	adds := map[string]*migrations.OpAddColumn{
		"nullable, no up":      {Table: "w", Column: migrations.Column{Name: "x", Type: "text", Nullable: true}},
		"not null default 7":   {Table: "w", Column: migrations.Column{Name: "x", Type: "integer", Nullable: false, Default: &seven}},
		"up repeating default": {Table: "w", Up: "7", Column: migrations.Column{Name: "x", Type: "integer", Nullable: true, Default: &seven}},
		"up 5":                 {Table: "w", Up: "5", Column: migrations.Column{Name: "x", Type: "integer", Nullable: true}},
		"not null, up 5":       {Table: "w", Up: "5", Column: migrations.Column{Name: "x", Type: "integer", Nullable: false}},
		"volatile default":     {Table: "w", Up: volatile, Column: migrations.Column{Name: "x", Type: "integer", Nullable: true, Default: &volatile}},
	}
	for mode, versionSchemas := range map[string]bool{"rolling": true, "version schemas disabled": false} {
		for en, e := range earlier {
			for an, a := range adds {
				t.Run(mode+"/"+en+"/"+an, func(t *testing.T) {
					t.Parallel()
					testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public", r3Opts(versionSchemas), func(mig *roll.Roll, db *sql.DB) {
						ctx := context.Background()
						r2Base(t, mig, r2W)
						err := r2TrainErr(mig, []*migrations.Migration{
							{Name: "01_earlier", Operations: migrations.Operations{e}},
							{Name: "02_add", Operations: migrations.Operations{a}},
							r2Last(),
						})
						if err != nil {
							t.Logf("LOUD: the train is refused: %v", err)
							return
						}
						if versionSchemas {
							t.Logf("the new revision reads the added column before the contraction: err=%v",
								r2ExecAs(t, db, r2New, `SELECT x FROM w`))
							// The previous revision, still live, inserts a row
							// without a value for the old column.
							require.NoError(t, r2ExecAs(t, db, r2Prev, `INSERT INTO w (id) VALUES (50)`))
							assert.Equal(t, "<NULL>", r3Str(t, db, `SELECT x::text FROM public_00_base.w WHERE id = 50`),
								"the old column of a row the previous revision inserted without it")
						}
						if err := r2Safely(func() error { return mig.Complete(ctx) }); err != nil {
							t.Logf("LOUD: the contraction is refused: %v", err)
							return
						}
						cols := r3Columns(t, db, "w")
						t.Logf("columns after the contraction: %s", cols)
						assert.Zero(t, r2Leftovers(t, db, "public"), "pgroll artifacts left after a contraction that reported success")
						assert.Contains(t, ", "+cols, ", x", "the added column has its name")
						if en == "rename" {
							assert.NotContains(t, cols, "y!", "the renamed column did not take the added column's NOT NULL")
							assert.NotContains(t, cols, "y=", "the renamed column did not take the added column's default")
						}
					})
				})
			}
		}
	}
}

// Three deployments, each contracted: add z; drop x; add a new x. A one-shot
// sealed revert to the base applies the inverses as one train: drop the new
// x (waits), add the old x back (completes inline), drop z (last).
func TestTrainMatrixSealedRevertReAddsADroppedColumnWhoseNameWasReused(t *testing.T) {
	t.Parallel()
	for name, expandOnly := range map[string]bool{"one shot": false, "expand only": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				r2Base(t, mig, r2W)
				for _, m := range []*migrations.Migration{
					{Name: "01_add_z", Operations: migrations.Operations{&migrations.OpAddColumn{Table: "w", Column: migrations.Column{Name: "z", Type: "integer", Nullable: true}}}},
					{Name: "02_drop_x", Operations: migrations.Operations{&migrations.OpDropColumn{Table: "w", Column: "x", Down: "0"}}},
					{Name: "03_add_x", Operations: migrations.Operations{&migrations.OpAddColumn{Table: "w", Column: migrations.Column{Name: "x", Type: "text", Nullable: true}}}},
				} {
					require.NoError(t, mig.Start(ctx, m, backfill.NewConfig()))
					require.NoError(t, mig.Complete(ctx))
				}
				t.Logf("before the revert: %s", r3Columns(t, db, "w"))

				var opts []roll.SealedRevertOption
				if expandOnly {
					opts = append(opts, roll.WithExpandOnly())
				}
				err := r2Safely(func() error { _, err := mig.RevertSealed(ctx, "00_base", nil, opts...); return err })
				if err == nil && expandOnly {
					err = r2Safely(func() error { return mig.Complete(ctx) })
					if err == nil {
						_, err = mig.FinishPendingSealedRevert(ctx)
					}
				}
				if err != nil {
					t.Logf("LOUD: the sealed revert is refused: %v", err)
					return
				}
				cols := r3Columns(t, db, "w")
				t.Logf("after a sealed revert that reported success: %s", cols)
				assert.Equal(t, "id!, x", cols, "the table as the base migration left it")
				assert.Zero(t, r2Leftovers(t, db, "public"))
			})
		})
	}
}

// ---------------------------------------------------------------------------
// 2. A migration that creates a table and adds a filling column elsewhere
//    now waits for the contraction, so its create_table is replayed over the
//    physical schema by every later Start and Complete of the train.
// ---------------------------------------------------------------------------

func r3CreateTableAndAdd(name string) *migrations.Migration {
	return &migrations.Migration{Name: name, Operations: migrations.Operations{
		&migrations.OpCreateTable{Name: "t", Columns: []migrations.Column{{Name: "id", Type: "integer", Pk: true}}},
		&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
	}}
}

// A later migration of the train changes the created table.
func TestTrainMatrixLaterMigrationChangesATableAWaitingMigrationCreated(t *testing.T) {
	t.Parallel()

	later := map[string]*migrations.Migration{
		"add_column": {Name: "02_later", Operations: migrations.Operations{
			&migrations.OpAddColumn{Table: "t", Column: migrations.Column{Name: "b", Type: "integer", Nullable: true}},
		}},
		"raw sql add column": r2Raw("02_later", `ALTER TABLE t ADD COLUMN b integer`),
	}
	for name, l := range later {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				r2Base(t, mig, r2W)
				require.NoError(t, r2TrainErr(mig, []*migrations.Migration{r3CreateTableAndAdd("01_create"), l, r2Last()}),
					"the train, as `pgroll migrate` applies it")
				assert.NoError(t, r2ExecAs(t, db, r2New, `INSERT INTO t (id, b) VALUES (1, 2)`),
					"the new revision writes the column the later migration added")
				assert.NoError(t, r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x) VALUES (2, 20)`), "previous revision")
				require.NoError(t, r2Safely(func() error { return mig.Complete(ctx) }))
				t.Logf("t after the contraction: %s", r3Columns(t, db, "t"))
				assert.NoError(t, r2ExecAs(t, db, r2New, `INSERT INTO t (id, b) VALUES (2, 2)`),
					"the new revision writes the column after the contraction")
				assert.Zero(t, r2Leftovers(t, db, "public"))
			})
		})
	}
}

// The same train, reverted before the contraction instead.
func TestTrainMatrixRevertAfterAnInlineAddColumnKeptItsTemporaryName(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		r2Base(t, mig, r2W)
		err := r2TrainErr(mig, []*migrations.Migration{
			{Name: "01_earlier", Operations: migrations.Operations{&migrations.OpRenameColumn{Table: "w", From: "x", To: "y"}}},
			{Name: "02_add", Operations: migrations.Operations{
				&migrations.OpAddColumn{Table: "w", Column: migrations.Column{Name: "x", Type: "text", Nullable: true}},
			}},
			r2Last(),
		})
		if err != nil {
			t.Logf("LOUD: the train is refused: %v", err)
			return
		}
		_, err = mig.Revert(ctx)
		assert.NoError(t, err, "revert of the train")
		t.Logf("w after the revert: %s", r3Columns(t, db, "w"))
	})
}

// ---------------------------------------------------------------------------
// 3. A later migration of the train duplicates the column a waiting
//    add_column added: the column still carries what its Complete would
//    have turned into a NOT NULL attribute or a UNIQUE constraint.
// ---------------------------------------------------------------------------

func r3AlterAType() *migrations.Migration {
	bigint := "bigint"
	return &migrations.Migration{Name: "02_alter", Operations: migrations.Operations{
		&migrations.OpAlterColumn{Table: "w", Column: "a", Type: &bigint, Up: "a", Down: "a"},
	}}
}

// NOT NULL without a default: the later alter_column's Start fails half-way
// and is not rolled back.
func TestTrainMatrixAlterColumnOnAWaitingNotNullAddColumn(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		r2Base(t, mig, r2W)
		err := r2TrainErr(mig, []*migrations.Migration{
			{Name: "01_add", Operations: migrations.Operations{
				&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: false}},
			}},
			r3AlterAType(),
			r2Last(),
		})
		assert.NoError(t, err, "the train, as `pgroll migrate` applies it")
		assert.NoError(t, r2ExecAs(t, db, r2Prev, `UPDATE w SET x = 11 WHERE id = 1`),
			"the previous revision, live while `pgroll migrate` runs, updates a row")
		t.Logf("w after `pgroll migrate`: %s", r3Columns(t, db, "w"))
		t.Logf("in progress: %s", r3Str(t, db, `SELECT COALESCE(string_agg(name, ','), '(none)') FROM pgroll.migrations WHERE NOT done`))
		if err == nil {
			require.NoError(t, r2Safely(func() error { return mig.Complete(ctx) }))
			assert.Zero(t, r2Leftovers(t, db, "public"))
		}
	})
}

// UNIQUE: the contraction succeeds and the constraint is gone.
func TestTrainMatrixAlterColumnOnAWaitingUniqueAddColumn(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		r2Base(t, mig, r2W)
		r2Train(t, mig, []*migrations.Migration{
			{Name: "01_add", Operations: migrations.Operations{
				&migrations.OpAddColumn{Table: "w", Up: "x + 100", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true, Unique: true}},
			}},
			r3AlterAType(),
			r2Last(),
		})
		require.NoError(t, r2Safely(func() error { return mig.Complete(ctx) }))
		r2Want(t, db, 1, `SELECT count(*) FROM pg_constraint WHERE conrelid = 'public.w'::regclass AND contype = 'u'`,
			"the column's UNIQUE constraint after the contraction")
		t.Logf("indexes on w: %s", r3Str(t, db, `SELECT string_agg(indexdef, '; ' ORDER BY indexname) FROM pg_indexes WHERE schemaname = 'public' AND tablename = 'w'`))
		assert.Zero(t, r2Leftovers(t, db, "public"), "pgroll artifacts left after a contraction that reported success")

		// The leftover index name blocks every later unique column called `a`.
		err := r2Safely(func() error {
			return mig.Start(ctx, &migrations.Migration{Name: "10_next", Operations: migrations.Operations{
				&migrations.OpCreateTable{Name: "t", Columns: []migrations.Column{{Name: "id", Type: "integer", Pk: true}}},
			}}, backfill.NewConfig())
		})
		require.NoError(t, err)
		require.NoError(t, mig.Complete(ctx))
		err = r2Safely(func() error {
			return mig.Start(ctx, &migrations.Migration{Name: "11_next", Operations: migrations.Operations{
				&migrations.OpAddColumn{Table: "t", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true, Unique: true}},
			}}, backfill.NewConfig())
		})
		assert.NoError(t, err, "a later deployment adds a unique column `a` to another table")
	})
}

// The same shape in the emergency path: two contracted deployments (change a
// NOT NULL column's type; drop it) are reverted by inversion. The inverse
// train re-adds the column NOT NULL with `up`, then alters its type back.
func TestTrainMatrixSealedRevertReAddsANotNullColumnThenAltersIt(t *testing.T) {
	t.Parallel()
	for name, expandOnly := range map[string]bool{"one shot": false, "expand only": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				r2Base(t, mig, `CREATE TABLE w (id integer PRIMARY KEY, x integer NOT NULL, n integer);
					INSERT INTO w VALUES (1, 10, 0)`)
				bigint := "bigint"
				for _, m := range []*migrations.Migration{
					{Name: "01_alter_x", Operations: migrations.Operations{&migrations.OpAlterColumn{Table: "w", Column: "x", Type: &bigint, Up: "x", Down: "x"}}},
					{Name: "02_drop_x", Operations: migrations.Operations{&migrations.OpDropColumn{Table: "w", Column: "x", Down: "0"}}},
				} {
					require.NoError(t, mig.Start(ctx, m, backfill.NewConfig()))
					require.NoError(t, mig.Complete(ctx))
				}

				var opts []roll.SealedRevertOption
				if expandOnly {
					opts = append(opts, roll.WithExpandOnly())
				}
				err := r2Safely(func() error { _, err := mig.RevertSealed(ctx, "00_base", nil, opts...); return err })
				assert.NoError(t, err, "sealed revert")
				if expandOnly {
					// The base build fails the current revision's INSERT here (the
					// fault c7ae750 fixed); its UPDATE works on both.
					assert.NoError(t, r2ExecAs(t, db, "public_02_drop_x", `UPDATE w SET n = n + 1 WHERE id = 1`),
						"the current revision, live while the fleet repins, updates a row")
					t.Logf("the current revision inserts a row: %s", r3Err(r2ExecAs(t, db, "public_02_drop_x", `INSERT INTO w (id, n) VALUES (2, 0)`)))
				}
				t.Logf("w after the sealed revert: %s", r3Columns(t, db, "w"))
			})
		})
	}
}

// ---------------------------------------------------------------------------
// 4. What the longer-lived trigger does to writers the base build left alone.
// ---------------------------------------------------------------------------

// A later migration of the train rewrites the added column; the previous
// revision then updates another column of the row. `up` runs for that
// update, as it does for the last migration of a train: an `up` that reads
// its own column keeps what is stored, one that does not replaces it.
func TestTrainMatrixLaterDataMigrationAndAPreviousRevisionUpdate(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		up   string
		want int
	}{
		"up reads its own column": {up: "COALESCE(a, x + 1)", want: 1000},
		"up does not":             {up: "x + 1", want: 12},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				r2Base(t, mig, r2W)
				r2Train(t, mig, []*migrations.Migration{
					r2Add("01_add_a", "w", "a", tc.up),
					r2Raw("02_data", `UPDATE w SET a = 1000 WHERE id = 1`),
					r2Last(),
				})
				r2Want(t, db, 1000, `SELECT a FROM public_09_last.w WHERE id = 1`, "what the data migration wrote")
				require.NoError(t, r2ExecAs(t, db, r2Prev, `UPDATE w SET x = 11 WHERE id = 1`), "previous revision")
				r2Want(t, db, tc.want, `SELECT a FROM public_09_last.w WHERE id = 1`, "after a previous-revision update")
			})
		})
	}
}

// A job of the new revision that pins `public` inserts a row and names the
// column.
func TestTrainMatrixUnpinnedInsertNamingTheColumnMidTrain(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		r2Base(t, mig, r2W)
		r2Train(t, mig, []*migrations.Migration{r2Add("01_add_a", "w", "a", "x + 1"), r2Last()})
		require.NoError(t, r2ExecAs(t, db, "public", `INSERT INTO w (id, x, a) VALUES (2, 20, 99)`))
		r2Want(t, db, 99, `SELECT a FROM public.w WHERE id = 2`, "the value the job inserted")
		require.NoError(t, r2ExecAs(t, db, "public", `INSERT INTO w (id, x, a) VALUES (3, 30, 99) ON CONFLICT (id) DO UPDATE SET a = EXCLUDED.a`))
		require.NoError(t, r2ExecAs(t, db, "public", `INSERT INTO w (id, x, a) VALUES (3, 30, 77) ON CONFLICT (id) DO UPDATE SET a = EXCLUDED.a`))
		r2Want(t, db, 77, `SELECT a FROM public.w WHERE id = 3`, "the value the job upserted")
	})
}

// Writers race the batched backfill of a column that starts under its own
// name: no row ends unfilled, and each has what its last writer gave it.
func TestTrainMatrixWritersRaceTheBackfill(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		r2Base(t, mig, `CREATE TABLE w (id integer PRIMARY KEY, x integer);
			INSERT INTO w SELECT g, g FROM generate_series(1, 4000) g`)

		stop := make(chan struct{})
		done := make(chan error, 2)
		writer := func(path string, stmts func(i int) string) {
			conn, err := db.Conn(ctx)
			if err != nil {
				done <- err
				return
			}
			defer conn.Close()
			if _, err := conn.ExecContext(ctx, "SET search_path TO "+path); err != nil {
				done <- err
				return
			}
			for i := 0; ; i++ {
				select {
				case <-stop:
					done <- nil
					return
				default:
				}
				if _, err := conn.ExecContext(ctx, stmts(i)); err != nil {
					done <- fmt.Errorf("%s: %w", stmts(i), err)
					return
				}
			}
		}
		go writer(r2Prev, func(i int) string {
			if i%3 == 0 {
				return fmt.Sprintf(`INSERT INTO w (id, x) VALUES (%d, %d)`, 10000+i, i)
			}
			return fmt.Sprintf(`UPDATE w SET x = x + 100000 WHERE id = %d`, 1+(i*37)%4000)
		})
		go writer("public", func(i int) string {
			return fmt.Sprintf(`UPDATE w SET x = x + 100000 WHERE id = %d`, 1+(i*53)%4000)
		})

		err := mig.Start(ctx, r2Add("01_add_a", "w", "a", "x + 1"),
			backfill.NewConfig(backfill.WithBatchSize(50), backfill.WithBatchDelay(time.Millisecond)))
		close(stop)
		require.NoError(t, <-done)
		require.NoError(t, <-done)
		require.NoError(t, err)
		require.NoError(t, mig.Complete(ctx))
		r2Want(t, db, 0, `SELECT count(*) FROM public.w WHERE a IS DISTINCT FROM x + 1`, "rows whose a is not x + 1")
		t.Logf("rows: %s", r3Str(t, db, `SELECT count(*)::text FROM public.w`))
	})
}

// A fleet that names nothing but `x` writes a partitioned table while a
// train adds a NOT NULL column to it.
func TestTrainMatrixPartitionedTable(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		r2Base(t, mig, `CREATE TABLE w (id integer, x integer, PRIMARY KEY (id)) PARTITION BY RANGE (id);
			CREATE TABLE w_1 PARTITION OF w FOR VALUES FROM (0) TO (100);
			CREATE TABLE w_2 PARTITION OF w FOR VALUES FROM (100) TO (200);
			INSERT INTO w VALUES (1, 10), (101, 110)`)
		err := r2TrainErr(mig, []*migrations.Migration{
			{Name: "01_add", Operations: migrations.Operations{
				&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
			}},
			r2Last(),
		})
		out := "train=" + r3Err(err)
		out += " prevIns=" + r3Err(r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x) VALUES (2, 20), (102, 120)`))
		out += " prevMove=" + r3Err(r2ExecAs(t, db, r2Prev, `UPDATE w SET id = 150 WHERE id = 1`))
		out += " newIns=" + r3Err(r2ExecAs(t, db, r2New, `INSERT INTO w (id, x, a) VALUES (3, 30, 99)`))
		out += " complete=" + r3Err(r2Safely(func() error { return mig.Complete(ctx) }))
		t.Logf("SIG|partitioned|%s || %s rows[%s] leftovers[%d]", out, r3Columns(t, db, "w"),
			r3TryStr(db, `SELECT string_agg(t::text, ' ' ORDER BY id) FROM public.w t`), r2Leftovers(t, db, "public"))
	})
}

// One migration adds two columns; the reader's trigger function sorts
// before the filler's by name.
func TestTrainMatrixReaderSortsBeforeItsFiller(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		r2Base(t, mig, r2W)
		r2Train(t, mig, []*migrations.Migration{
			{Name: "01_add", Operations: migrations.Operations{
				&migrations.OpAddColumn{Table: "w", Up: "zz * 10", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
				&migrations.OpAddColumn{Table: "w", Up: "m + 1", Column: migrations.Column{Name: "zz", Type: "integer", Nullable: true}},
				&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "m", Type: "integer", Nullable: true}},
			}},
			r2Last(),
		})
		require.NoError(t, r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x) VALUES (2, 20)`))
		require.NoError(t, r2ExecAs(t, db, r2Prev, `UPDATE w SET x = 50 WHERE id = 1`))
		require.NoError(t, mig.Complete(ctx))
		assert.Equal(t, "(1,50,520,52,51) (2,20,220,22,21)", r3Str(t, db, `SELECT string_agg(t::text, ' ' ORDER BY id) FROM public.w t`))
	})
}

// A constraint beside the add_column keeps the column under its temporary
// name, and the migration waits for the contraction so the previous
// revision's writes stay filled. SQL later in the train cannot name the
// column, and says so before any pod rolls.
func TestTrainMatrixLaterSQLNamesAColumnAddedBesideAConstraint(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		r2Base(t, mig, r2W)
		err := r2TrainErr(mig, []*migrations.Migration{
			{Name: "01_add", Operations: migrations.Operations{
				&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
				&migrations.OpCreateConstraint{Name: "uq_a", Table: "w", Type: "unique", Columns: []string{"a"}, Up: map[string]string{"a": "a"}, Down: map[string]string{"a": "a"}},
			}},
			r2Raw("02_data", `UPDATE w SET a = a + 1000`),
			r2Last(),
		})
		require.ErrorContains(t, err, `column "a" does not exist`)
		require.ErrorContains(t, err, "changes rolled back")
	})
}

// ---------------------------------------------------------------------------
// Differential harness: every mode times every column shape. Each scenario
// logs one SIG line; the lines of the two builds are compared by hand.
// ---------------------------------------------------------------------------

const r3DiffBase = `CREATE TABLE p (id integer PRIMARY KEY);
	INSERT INTO p VALUES (1), (2);
	CREATE TABLE w (id integer PRIMARY KEY, x integer);
	INSERT INTO w VALUES (1, 10), (2, 20)`

type r3Shape struct {
	ops      func() migrations.Operations
	newValue string // what the new revision writes to the column
	rows     string // expression over w for the signature
}

func r3Shapes() map[string]r3Shape {
	zero, seven, three, volatile, comment := "0", "7", "3", "(random() * 100)::integer", "a comment"
	col := func(c migrations.Column) migrations.Column { c.Name, c.Type = "a", "integer"; return c }
	one := func(up string, c migrations.Column) func() migrations.Operations {
		return func() migrations.Operations {
			return migrations.Operations{&migrations.OpAddColumn{Table: "w", Up: up, Column: col(c)}}
		}
	}
	plain := "id || ':' || x || ':' || COALESCE(a::text, 'NULL')"
	return map[string]r3Shape{
		"01 nullable up const":       {one("5", migrations.Column{Nullable: true}), "99", plain},
		"02 nullable up derived":     {one("x + 1", migrations.Column{Nullable: true}), "99", plain},
		"03 notnull up derived":      {one("x + 1", migrations.Column{}), "99", plain},
		"04 notnull default up":      {one("x * 2", migrations.Column{Default: &zero}), "99", plain},
		"05 default eq up":           {one("7", migrations.Column{Nullable: true, Default: &seven}), "99", plain},
		"06 volatile default":        {one(volatile, migrations.Column{Nullable: true, Default: &volatile}), "99", "id || ':' || x || ':' || (a IS NOT NULL)::text || CASE WHEN a = 99 THEN '=99' ELSE '' END"},
		"07 unique up":               {one("x + 100", migrations.Column{Nullable: true, Unique: true}), "99", plain},
		"08 check up":                {one("x + 1", migrations.Column{Nullable: true, Check: &migrations.CheckConstraint{Name: "a_pos", Constraint: "a > 0"}}), "99", plain},
		"09 fk up":                   {one("1", migrations.Column{Nullable: true, References: &migrations.ForeignKeyReference{Name: "fk_a", Table: "p", Column: "id"}}), "2", plain},
		"10 no up":                   {one("", migrations.Column{Nullable: true}), "99", plain},
		"11 comment notnull self up": {one("COALESCE(a, x + 1)", migrations.Column{Comment: &comment}), "99", plain},
		"12 two adds": {func() migrations.Operations {
			return migrations.Operations{
				&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
				&migrations.OpAddColumn{Table: "w", Column: migrations.Column{Name: "b", Type: "integer", Nullable: true, Default: &three}},
			}
		}, "99", plain + " || ':' || COALESCE(b::text, 'NULL')"},
		"13 add and index": {func() migrations.Operations {
			return migrations.Operations{
				&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
				&migrations.OpCreateIndex{Name: "ix_w_a", Table: "w", Columns: []migrations.IndexField{{Column: "a"}}},
			}
		}, "99", plain},
		"14 add and create table": {func() migrations.Operations {
			return migrations.Operations{
				&migrations.OpCreateTable{Name: "t", Columns: []migrations.Column{{Name: "id", Type: "integer", Pk: true}}},
				&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
			}
		}, "99", plain},
		"15 second add reads first": {func() migrations.Operations {
			return migrations.Operations{
				&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
				&migrations.OpAddColumn{Table: "w", Up: "a * 10", Column: migrations.Column{Name: "b", Type: "integer", Nullable: true}},
			}
		}, "99", plain + " || ':' || COALESCE(b::text, 'NULL')"},
	}
}

// r3Sig describes table w and what pgroll left in the schema.
func r3Sig(t *testing.T, db *sql.DB, rows string) string {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`SELECT to_regclass('public.w') IS NOT NULL`).Scan(&exists))
	if !exists {
		return "w is gone"
	}
	var hasA bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'w' AND column_name = 'a')`).Scan(&hasA))
	var data string
	if hasA {
		data = r3Str(t, db, fmt.Sprintf(`SELECT string_agg(%s, ' ' ORDER BY id) FROM public.w`, rows))
	} else {
		data = "(no a) " + r3Str(t, db, `SELECT string_agg(id || ':' || COALESCE(x::text, 'NULL'), ' ' ORDER BY id) FROM public.w`)
	}
	return strings.Join([]string{
		"cols[" + r3Columns(t, db, "w") + "]",
		"cons[" + r3Str(t, db, `SELECT COALESCE(string_agg(conname || ' ' || pg_get_constraintdef(oid) || CASE WHEN convalidated THEN '' ELSE ' NOT VALID' END, '; ' ORDER BY conname), '') FROM pg_constraint WHERE conrelid = 'public.w'::regclass`) + "]",
		"idx[" + r3Str(t, db, `SELECT COALESCE(string_agg(indexname || CASE WHEN i.indisvalid THEN '' ELSE ' INVALID' END, '; ' ORDER BY indexname), '') FROM pg_indexes x JOIN pg_class c ON c.relname = x.indexname JOIN pg_index i ON i.indexrelid = c.oid WHERE x.schemaname = 'public' AND x.tablename = 'w'`) + "]",
		"comment[" + r3Str(t, db, `SELECT COALESCE(string_agg(col_description('public.w'::regclass, attnum), ','), '') FROM pg_attribute WHERE attrelid = 'public.w'::regclass AND attnum > 0 AND NOT attisdropped`) + "]",
		fmt.Sprintf("leftovers[%d]", r2Leftovers(t, db, "public")),
		"triggers[" + r3Str(t, db, `SELECT count(*)::text FROM pg_trigger WHERE tgrelid = 'public.w'::regclass AND NOT tgisinternal`) + "]",
		"rows[" + data + "]",
	}, " ")
}

// r3ErrLen cuts error text in a signature.
var r3ErrLen = 90

func r3Err(err error) string {
	if err == nil {
		return "ok"
	}
	msg := err.Error()
	if i := strings.Index(msg, " at column"); i > 0 {
		msg = msg[:i]
	}
	if len(msg) > r3ErrLen {
		msg = msg[:r3ErrLen]
	}
	return "ERR(" + msg + ")"
}

// r3Writes are the writes of both revisions and of a session on `public`
// whose code predates the column.
func r3Writes(t *testing.T, db *sql.DB, prev, next, newValue string) string {
	t.Helper()
	var out []string
	run := func(label, path, stmt string) {
		out = append(out, label+"="+r3Err(r2ExecAs(t, db, path, stmt)))
	}
	if prev != "" {
		run("prevIns", prev, `INSERT INTO w (id, x) VALUES (3, 30)`)
		run("prevUpd", prev, `UPDATE w SET x = 11 WHERE id = 1`)
	}
	if next != "" {
		run("newIns", next, fmt.Sprintf(`INSERT INTO w (id, x, a) VALUES (4, 40, %s)`, newValue))
		run("newUpd", next, `UPDATE w SET x = 21 WHERE id = 2`)
		run("newIns2", next, fmt.Sprintf(`INSERT INTO w (id, x, a) VALUES (6, 60, %s)`, newValue))
	}
	run("pubIns", "", `INSERT INTO w (id, x) VALUES (5, 50)`)
	run("pubUpsert", "public", `INSERT INTO w (id, x) VALUES (5, 51) ON CONFLICT (id) DO UPDATE SET x = EXCLUDED.x`)
	if prev != "" && next != "" {
		run("prevUpdNewRow", prev, `UPDATE w SET x = 41 WHERE id = 4`)
	}
	return strings.Join(out, ",")
}

// r3CompleteTrain applies a train the way `pgroll migrate --complete` does
// while version schemas are enabled, up to the last migration's Start.
func r3CompleteTrainErr(mig *roll.Roll, train []*migrations.Migration) error {
	ctx := context.Background()
	for _, m := range train[:len(train)-1] {
		if err := r2Safely(func() error { return mig.Start(ctx, m, backfill.NewConfig(), roll.WithoutVersionSchema()) }); err != nil {
			return fmt.Errorf("start %s: %w", m.Name, err)
		}
		opt := roll.WithSkipSchemaDrop()
		if m.CompleteMustBeDeferred() {
			opt = roll.WithDeferComplete()
		}
		if err := r2Safely(func() error { return mig.Complete(ctx, opt) }); err != nil {
			return fmt.Errorf("complete %s: %w", m.Name, err)
		}
	}
	final := train[len(train)-1]
	if err := r2Safely(func() error { return mig.Start(ctx, final, backfill.NewConfig()) }); err != nil {
		return fmt.Errorf("start %s: %w", final.Name, err)
	}
	return nil
}

func TestTrainMatrixDiff(t *testing.T) {
	skipUnlessTrainMatrix(t)
	t.Parallel()

	type mode struct {
		versionSchemas bool
		run            func(t *testing.T, mig *roll.Roll, db *sql.DB, sh r3Shape) string
	}
	ctx := context.Background()
	add := func(sh r3Shape) *migrations.Migration {
		return &migrations.Migration{Name: "01_add", Operations: sh.ops()}
	}
	step := func(label string, fn func() error) string { return label + "=" + r3Err(r2Safely(fn)) }
	modes := map[string]mode{
		"a single": {true, func(t *testing.T, mig *roll.Roll, db *sql.DB, sh r3Shape) string {
			out := step("start", func() error { return mig.Start(ctx, add(sh), backfill.NewConfig()) })
			out += " " + r3Writes(t, db, r2Prev, "public_01_add", sh.newValue)
			out += " mid{" + r3Str(t, db, fmt.Sprintf(`SELECT string_agg(%s, ' ' ORDER BY id) FROM public_01_add.w`, sh.rows)) + "}"
			return out + " " + step("complete", func() error { return mig.Complete(ctx) })
		}},
		"b single rollback": {true, func(t *testing.T, mig *roll.Roll, db *sql.DB, sh r3Shape) string {
			out := step("start", func() error { return mig.Start(ctx, add(sh), backfill.NewConfig()) })
			out += " " + r3Writes(t, db, r2Prev, "public_01_add", sh.newValue)
			return out + " " + step("rollback", func() error { return mig.Rollback(ctx) })
		}},
		"c single no version schemas": {false, func(t *testing.T, mig *roll.Roll, db *sql.DB, sh r3Shape) string {
			out := step("start", func() error { return mig.Start(ctx, add(sh), backfill.NewConfig()) })
			out += " " + r3Writes(t, db, "", "", sh.newValue)
			return out + " " + step("complete", func() error { return mig.Complete(ctx) })
		}},
		"d train --complete": {true, func(t *testing.T, mig *roll.Roll, db *sql.DB, sh r3Shape) string {
			out := step("train", func() error { return r3CompleteTrainErr(mig, []*migrations.Migration{add(sh), r2Last()}) })
			out += " " + r3Writes(t, db, "", "", sh.newValue)
			return out + " " + step("complete", func() error { return mig.Complete(ctx) })
		}},
		"e train no version schemas": {false, func(t *testing.T, mig *roll.Roll, db *sql.DB, sh r3Shape) string {
			out := step("train", func() error { return r2TrainErr(mig, []*migrations.Migration{add(sh), r2Last()}) })
			out += " " + r3Writes(t, db, "", "", sh.newValue)
			return out + " " + step("complete", func() error { return mig.Complete(ctx) })
		}},
		"f train rolling": {true, func(t *testing.T, mig *roll.Roll, db *sql.DB, sh r3Shape) string {
			out := step("train", func() error { return r2TrainErr(mig, []*migrations.Migration{add(sh), r2Last()}) })
			out += " " + r3Writes(t, db, r2Prev, r2New, sh.newValue)
			out += " mid{" + r3Str(t, db, fmt.Sprintf(`SELECT string_agg(%s, ' ' ORDER BY id) FROM public_09_last.w`, sh.rows)) + "}"
			return out + " " + step("complete", func() error { return mig.Complete(ctx) })
		}},
		"g train rolling revert": {true, func(t *testing.T, mig *roll.Roll, db *sql.DB, sh r3Shape) string {
			out := step("train", func() error { return r2TrainErr(mig, []*migrations.Migration{add(sh), r2Last()}) })
			out += " " + r3Writes(t, db, r2Prev, r2New, sh.newValue)
			return out + " " + step("revert", func() error { _, err := mig.Revert(ctx); return err })
		}},
		"h train rolling rollback revert": {true, func(t *testing.T, mig *roll.Roll, db *sql.DB, sh r3Shape) string {
			out := step("train", func() error { return r2TrainErr(mig, []*migrations.Migration{add(sh), r2Last()}) })
			out += " " + r3Writes(t, db, r2Prev, r2New, sh.newValue)
			out += " " + step("rollback", func() error { return mig.Rollback(ctx) })
			return out + " " + step("revert", func() error { _, err := mig.Revert(ctx); return err })
		}},
		"i sealed revert one shot": {true, func(t *testing.T, mig *roll.Roll, db *sql.DB, sh r3Shape) string {
			out := step("train", func() error { return r2TrainErr(mig, []*migrations.Migration{add(sh), r2Last()}) })
			out += " " + step("complete", func() error { return mig.Complete(ctx) })
			return out + " " + step("sealed", func() error { _, err := mig.RevertSealed(ctx, "00_base", nil); return err })
		}},
		"j sealed revert expand only": {true, func(t *testing.T, mig *roll.Roll, db *sql.DB, sh r3Shape) string {
			out := step("train", func() error { return r2TrainErr(mig, []*migrations.Migration{add(sh), r2Last()}) })
			out += " " + step("complete", func() error { return mig.Complete(ctx) })
			out += " " + step("sealed", func() error { _, err := mig.RevertSealed(ctx, "00_base", nil, roll.WithExpandOnly()); return err })
			out += " cur=" + r3Err(r2ExecAs(t, db, r2New, fmt.Sprintf(`INSERT INTO w (id, x, a) VALUES (4, 40, %s)`, sh.newValue)))
			out += " restored=" + r3Err(r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x) VALUES (3, 30)`))
			out += " " + step("complete2", func() error { return mig.Complete(ctx) })
			return out + " " + step("finish", func() error { _, err := mig.FinishPendingSealedRevert(ctx); return err })
		}},
		"k leaf of rolling train": {true, func(t *testing.T, mig *roll.Roll, db *sql.DB, sh r3Shape) string {
			leaf := add(sh)
			leaf.Name = "09_last"
			out := step("train", func() error { return r2TrainErr(mig, []*migrations.Migration{r2Raw("01_first", "SELECT 1"), leaf}) })
			out += " " + r3Writes(t, db, r2Prev, r2New, sh.newValue)
			return out + " " + step("complete", func() error { return mig.Complete(ctx) })
		}},
	}
	for mn, m := range modes {
		for sn, sh := range r3Shapes() {
			t.Run(mn+"/"+sn, func(t *testing.T) {
				t.Parallel()
				testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public", r3Opts(m.versionSchemas), func(mig *roll.Roll, db *sql.DB) {
					r2Base(t, mig, r3DiffBase)
					steps := m.run(t, mig, db, sh)
					t.Logf("SIG|%s|%s|%s || %s", mn, sn, steps, r3Sig(t, db, sh.rows))
				})
			})
		}
	}
}

// ---------------------------------------------------------------------------
// Differential harness for trains of several migrations.
// ---------------------------------------------------------------------------

type r3Train struct {
	migs   func() []*migrations.Migration // the leaf, 09_last, is appended
	table  string                         // the table's name after the train
	newIns string                         // the new revision's insert of row 4
	newUpd string                         // the new revision's update
}

func r3AddA(extra ...migrations.Operation) *migrations.Migration {
	add := &migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}}
	return &migrations.Migration{Name: "01_add", Operations: append(migrations.Operations{add}, extra...)}
}

func r3Trains() map[string]r3Train {
	one := func(name string, ops ...migrations.Operation) *migrations.Migration {
		return &migrations.Migration{Name: name, Operations: ops}
	}
	f, bigint, zero := false, "bigint", "0"
	std := func(migs func() []*migrations.Migration) r3Train {
		return r3Train{migs, "w", `INSERT INTO w (id, x, a) VALUES (4, 40, 99)`, `UPDATE w SET x = 21 WHERE id = 2`}
	}
	return map[string]r3Train{
		"01 add; raw update of a": std(func() []*migrations.Migration {
			return []*migrations.Migration{r3AddA(), r2Raw("02_data", `UPDATE w SET a = a * 2`)}
		}),
		"02 add; raw insert": std(func() []*migrations.Migration {
			return []*migrations.Migration{r3AddA(), r2Raw("02_data", `INSERT INTO w (id, x) VALUES (7, 70); INSERT INTO w (id, x, a) VALUES (8, 80, 5)`)}
		}),
		"03 add; alter a not null": std(func() []*migrations.Migration {
			return []*migrations.Migration{r3AddA(), one("02_alter", &migrations.OpAlterColumn{Table: "w", Column: "a", Nullable: &f, Up: "COALESCE(a, 0)", Down: "a"})}
		}),
		"04 add; rename a to b": {func() []*migrations.Migration {
			return []*migrations.Migration{r3AddA(), one("02_rename", &migrations.OpRenameColumn{Table: "w", From: "a", To: "b"})}
		}, "w", `INSERT INTO w (id, x, b) VALUES (4, 40, 99)`, `UPDATE w SET x = 21 WHERE id = 2`},
		"05 add; alter x type": std(func() []*migrations.Migration {
			return []*migrations.Migration{r3AddA(), one("02_alter", &migrations.OpAlterColumn{Table: "w", Column: "x", Type: &bigint, Up: "x", Down: "x"})}
		}),
		"06 add; drop x": {func() []*migrations.Migration {
			return []*migrations.Migration{r3AddA(), one("02_drop", &migrations.OpDropColumn{Table: "w", Column: "x", Down: "a - 1"})}
		}, "w", `INSERT INTO w (id, a) VALUES (4, 99)`, `UPDATE w SET a = 77 WHERE id = 2`},
		"07 add; rename table": {func() []*migrations.Migration {
			return []*migrations.Migration{r3AddA(), one("02_rename", &migrations.OpRenameTable{From: "w", To: "w2"})}
		}, "w2", `INSERT INTO w2 (id, x, a) VALUES (4, 40, 99)`, `UPDATE w2 SET x = 21 WHERE id = 2`},
		"08 add; add b reading a": std(func() []*migrations.Migration {
			return []*migrations.Migration{r3AddA(), one("02_add_b", &migrations.OpAddColumn{Table: "w", Up: "a * 10", Column: migrations.Column{Name: "b", Type: "integer", Nullable: true}})}
		}),
		"09 rename x to y; add a reading y": {func() []*migrations.Migration {
			return []*migrations.Migration{
				one("01_rename", &migrations.OpRenameColumn{Table: "w", From: "x", To: "y"}),
				one("02_add", &migrations.OpAddColumn{Table: "w", Up: "y + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}}),
			}
		}, "w", `INSERT INTO w (id, y, a) VALUES (4, 40, 99)`, `UPDATE w SET y = 21 WHERE id = 2`},
		"10 alter x; add a reading x": std(func() []*migrations.Migration {
			return []*migrations.Migration{
				one("01_alter", &migrations.OpAlterColumn{Table: "w", Column: "x", Type: &bigint, Up: "x", Down: "x"}),
				one("02_add", &migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "bigint", Nullable: true}}),
			}
		}),
		"11 add; raw check on a": std(func() []*migrations.Migration {
			return []*migrations.Migration{r3AddA(), r2Raw("02_check", `ALTER TABLE w ADD CONSTRAINT a_pos CHECK (a > 0) NOT VALID`)}
		}),
		"12 add notnull default; raw update": std(func() []*migrations.Migration {
			return []*migrations.Migration{
				one("01_add", &migrations.OpAddColumn{Table: "w", Up: "x * 2", Column: migrations.Column{Name: "a", Type: "integer", Default: &zero}}),
				r2Raw("02_data", `UPDATE w SET a = 5 WHERE id = 1; UPDATE w SET x = x WHERE id = 2`),
			}
		}),
		"13 add with unique constraint beside it; raw naming a": std(func() []*migrations.Migration {
			return []*migrations.Migration{
				r3AddA(&migrations.OpCreateConstraint{Name: "uq_a", Table: "w", Type: "unique", Columns: []string{"a"}, Up: map[string]string{"a": "a"}, Down: map[string]string{"a": "a"}}),
				r2Raw("02_data", `UPDATE w SET a = a + 1000`),
			}
		}),
		"14 add; create index on a; drop index": std(func() []*migrations.Migration {
			return []*migrations.Migration{
				r3AddA(),
				one("02_index", &migrations.OpCreateIndex{Name: "ix_a", Table: "w", Columns: []migrations.IndexField{{Column: "a"}}}),
				one("03_drop_index", &migrations.OpDropIndex{Name: "ix_a"}),
			}
		}),
		"15 add; add c no up; raw naming c": std(func() []*migrations.Migration {
			return []*migrations.Migration{
				r3AddA(),
				one("02_add_c", &migrations.OpAddColumn{Table: "w", Column: migrations.Column{Name: "c", Type: "integer", Nullable: true}}),
				r2Raw("03_data", `UPDATE w SET c = id`),
			}
		}),
		"16 add; drop a": {func() []*migrations.Migration {
			return []*migrations.Migration{r3AddA(), one("02_drop", &migrations.OpDropColumn{Table: "w", Column: "a", Down: "x + 1"})}
		}, "w", `INSERT INTO w (id, x) VALUES (4, 40)`, `UPDATE w SET x = 21 WHERE id = 2`},
		"18 add notnull; unique constraint on a": std(func() []*migrations.Migration {
			return []*migrations.Migration{
				one("01_add", &migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer"}}),
				one("02_uq", &migrations.OpCreateConstraint{Name: "uq_a", Table: "w", Type: "unique", Columns: []string{"a"}, Up: map[string]string{"a": "a"}, Down: map[string]string{"a": "a"}}),
			}
		}),
		"19 add unique check; check constraint on a": std(func() []*migrations.Migration {
			chk := "a < 100000"
			return []*migrations.Migration{
				one("01_add", &migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true, Unique: true, Check: &migrations.CheckConstraint{Name: "a_pos", Constraint: "a > 0"}}}),
				one("02_ck", &migrations.OpCreateConstraint{Name: "a_small", Table: "w", Type: "check", Columns: []string{"a"}, Check: &chk, Up: map[string]string{"a": "a"}, Down: map[string]string{"a": "a"}}),
			}
		}),
		"20 add default 0; raw set default 42": {func() []*migrations.Migration {
			return []*migrations.Migration{
				one("01_add", &migrations.OpAddColumn{Table: "w", Up: "x * 2", Column: migrations.Column{Name: "a", Type: "integer", Default: &zero}}),
				r2Raw("02_default", `ALTER TABLE w ALTER COLUMN a SET DEFAULT 42`),
			}
		}, "w", `INSERT INTO w (id, x) VALUES (4, 40)`, `UPDATE w SET x = 21 WHERE id = 2`},
		"21 add; raw type change of a": std(func() []*migrations.Migration {
			return []*migrations.Migration{r3AddA(), r2Raw("02_type", `ALTER TABLE w ALTER COLUMN a TYPE bigint`)}
		}),
		"22 add notnull; alter a type": std(func() []*migrations.Migration {
			return []*migrations.Migration{
				one("01_add", &migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer"}}),
				one("02_alter", &migrations.OpAlterColumn{Table: "w", Column: "a", Type: &bigint, Up: "a", Down: "a"}),
			}
		}),
		"23 add; add table and fk column to it": std(func() []*migrations.Migration {
			return []*migrations.Migration{
				one("01_add", &migrations.OpAddColumn{Table: "w", Up: "x + 100", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true, Unique: true}}),
				one("02_table", &migrations.OpCreateTable{Name: "t", Columns: []migrations.Column{
					{Name: "id", Type: "integer", Pk: true},
					{Name: "w_a", Type: "integer", Nullable: true, References: &migrations.ForeignKeyReference{Name: "fk_t_w", Table: "w", Column: "a"}},
				}}),
			}
		}),
		"24 add unique; alter a type": std(func() []*migrations.Migration {
			return []*migrations.Migration{
				one("01_add", &migrations.OpAddColumn{Table: "w", Up: "x + 100", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true, Unique: true}}),
				one("02_alter", &migrations.OpAlterColumn{Table: "w", Column: "a", Type: &bigint, Up: "a", Down: "a"}),
			}
		}),
		"25 add fk; alter a type": {func() []*migrations.Migration {
			return []*migrations.Migration{
				one("01_add", &migrations.OpAddColumn{Table: "w", Up: "1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true, References: &migrations.ForeignKeyReference{Name: "fk_a", Table: "p", Column: "id"}}}),
				one("02_alter", &migrations.OpAlterColumn{Table: "w", Column: "a", Type: &bigint, Up: "a", Down: "a"}),
			}
		}, "w", `INSERT INTO w (id, x, a) VALUES (4, 40, 2)`, `UPDATE w SET x = 21 WHERE id = 2`},
		"26 add check; alter a type": std(func() []*migrations.Migration {
			return []*migrations.Migration{
				one("01_add", &migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true, Check: &migrations.CheckConstraint{Name: "a_pos", Constraint: "a > 0"}}}),
				one("02_alter", &migrations.OpAlterColumn{Table: "w", Column: "a", Type: &bigint, Up: "a", Down: "a"}),
			}
		}),
		"27 add notnull default; alter a type": std(func() []*migrations.Migration {
			return []*migrations.Migration{
				one("01_add", &migrations.OpAddColumn{Table: "w", Up: "x * 2", Column: migrations.Column{Name: "a", Type: "integer", Default: &zero}}),
				one("02_alter", &migrations.OpAlterColumn{Table: "w", Column: "a", Type: &bigint, Up: "a", Down: "a"}),
			}
		}),
		"28 add comment default; alter x type": std(func() []*migrations.Migration {
			return []*migrations.Migration{
				one("01_add", &migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer"}}),
				one("02_alter", &migrations.OpAlterColumn{Table: "w", Column: "x", Type: &bigint, Up: "x", Down: "x"}),
			}
		}),
		"17 add; raw sql trigger of the user": std(func() []*migrations.Migration {
			return []*migrations.Migration{
				r3AddA(),
				r2Raw("02_trigger", `CREATE FUNCTION bump() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN NEW.a := COALESCE(NEW.a, 0) + 1000; RETURN NEW; END $$;
					CREATE TRIGGER "0_bump" BEFORE INSERT ON w FOR EACH ROW EXECUTE FUNCTION bump()`),
			}
		}),
	}
}

func r3TableSig(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`SELECT to_regclass('public.' || $1) IS NOT NULL`, table).Scan(&exists))
	if !exists {
		return table + " is gone"
	}
	q := func(query string) string { return r3Str(t, db, fmt.Sprintf(query, table)) }
	return strings.Join([]string{
		"cols[" + r3Columns(t, db, table) + "]",
		"cons[" + q(`SELECT COALESCE(string_agg(conname || ' ' || pg_get_constraintdef(oid) || CASE WHEN convalidated THEN '' ELSE ' NOT VALID' END, '; ' ORDER BY conname), '') FROM pg_constraint WHERE conrelid = 'public.%s'::regclass`) + "]",
		"idx[" + q(`SELECT COALESCE(string_agg(indexname, '; ' ORDER BY indexname), '') FROM pg_indexes x WHERE x.schemaname = 'public' AND x.tablename = '%s'`) + "]",
		fmt.Sprintf("leftovers[%d]", r2Leftovers(t, db, "public")),
		"triggers[" + q(`SELECT count(*)::text FROM pg_trigger WHERE tgrelid = 'public.%s'::regclass AND NOT tgisinternal AND tgname LIKE '\_pgroll%%'`) + "]",
		"rows[" + q(`SELECT string_agg(t::text, ' ' ORDER BY id) FROM public.%s t`) + "]",
	}, " ")
}

func TestTrainMatrixDiffTrains(t *testing.T) {
	skipUnlessTrainMatrix(t)
	t.Parallel()
	ctx := context.Background()
	step := func(label string, fn func() error) string { return label + "=" + r3Err(r2Safely(fn)) }
	ends := map[string]func(mig *roll.Roll, first string) string{
		"contract": func(mig *roll.Roll, _ string) string {
			return step("complete", func() error { return mig.Complete(ctx) })
		},
		"revert": func(mig *roll.Roll, _ string) string {
			return step("revert", func() error { _, err := mig.Revert(ctx); return err })
		},
		"bounded revert then contract": func(mig *roll.Roll, first string) string {
			out := step("revertTo", func() error { _, err := mig.Revert(ctx, roll.WithRevertTo(first)); return err })
			return out + " " + step("complete", func() error { _, _, err := mig.FinishContraction(ctx); return err })
		},
	}
	for tn, tr := range r3Trains() {
		for en, end := range ends {
			t.Run(tn+"/"+en, func(t *testing.T) {
				t.Parallel()
				testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
					r2Base(t, mig, r3DiffBase)
					train := append(tr.migs(), r2Last())
					out := step("train", func() error { return r2TrainErr(mig, train) })
					run := func(label, path, stmt string) { out += " " + label + "=" + r3Err(r2ExecAs(t, db, path, stmt)) }
					run("prevIns", r2Prev, `INSERT INTO w (id, x) VALUES (3, 30)`)
					run("prevUpd", r2Prev, `UPDATE w SET x = 11 WHERE id = 1`)
					run("newIns", r2New, tr.newIns)
					run("newUpd", r2New, tr.newUpd)
					run("prevUpdNewRow", r2Prev, `UPDATE w SET x = 41 WHERE id = 4`)
					run("prevUpsert", r2Prev, `INSERT INTO w (id, x) VALUES (3, 31) ON CONFLICT (id) DO UPDATE SET x = EXCLUDED.x`)
					out += " mid{" + r3TryStr(db, fmt.Sprintf(`SELECT string_agg(to_jsonb(t)::text, ' ' ORDER BY id) FROM public_09_last.%s t`, tr.table)) + "}"
					out += " " + end(mig, train[0].Name)
					table := tr.table
					if en == "revert" {
						table = "w"
					}
					t.Logf("SIG|%s|%s|%s || %s", tn, en, out, r3TableSig(t, db, table))
				})
			})
		}
	}
}
