// SPDX-License-Identifier: Apache-2.0

package roll_test

// Train shapes around an add_column with `up` that is not the last migration
// of its train: later operations on the column and its table, writes from
// both revisions, reverts, and the contraction.
//
// Every train is applied the way `pgroll migrate` applies it without
// --complete.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

const (
	dcPrev = "public_00_base"
	dcNew  = "public_09_last"
)

// dcSafely turns a panic inside pgroll into an error, so one nil dereference
// does not take the whole test binary down.
func dcSafely(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("PANIC: %v", r)
		}
	}()
	return fn()
}

func dcBase(t *testing.T, mig *roll.Roll, up string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, mig.Start(ctx, &migrations.Migration{
		Name:       "00_base",
		Operations: migrations.Operations{&migrations.OpRawSQL{Up: up, Down: "SELECT 1"}},
	}, backfill.NewConfig()))
	require.NoError(t, mig.Complete(ctx))
}

// dcTrainErr applies a train and returns the step that failed, if any.
func dcTrainErr(mig *roll.Roll, train []*migrations.Migration) error {
	ctx := context.Background()
	final := train[len(train)-1]
	for _, m := range train[:len(train)-1] {
		if err := dcSafely(func() error { return mig.Start(ctx, m, backfill.NewConfig(), dcMidOpts(mig, final)...) }); err != nil {
			return fmt.Errorf("start %s: %w", m.Name, err)
		}
		opt := roll.WithSkipSchemaDrop()
		if roll.DefersCompleteInTrain(m, mig.UseVersionSchema()) {
			opt = roll.WithDeferComplete()
		}
		if err := dcSafely(func() error { return mig.Complete(ctx, opt) }); err != nil {
			return fmt.Errorf("complete %s: %w", m.Name, err)
		}
	}
	if err := dcSafely(func() error { return mig.Start(ctx, final, backfill.NewConfig(), dcLeafOpts(mig, final)...) }); err != nil {
		return fmt.Errorf("start %s: %w", final.Name, err)
	}
	return nil
}

func dcTrain(t *testing.T, mig *roll.Roll, train []*migrations.Migration) {
	t.Helper()
	require.NoError(t, dcTrainErr(mig, train))
}

func dcExecAs(t *testing.T, db *sql.DB, versionSchema, stmt string) error {
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

func dcInt(t *testing.T, db *sql.DB, query string) *int {
	t.Helper()
	var v sql.NullInt64
	require.NoError(t, db.QueryRowContext(context.Background(), query).Scan(&v))
	if !v.Valid {
		return nil
	}
	i := int(v.Int64)
	return &i
}

// dcWant asserts that query returns want; a NULL fails instead of panicking.
func dcWant(t *testing.T, db *sql.DB, want int, query string, msg ...any) {
	t.Helper()
	got := dcInt(t, db, query)
	if assert.NotNil(t, got, msg...) {
		assert.Equal(t, want, *got, msg...)
	}
}

func dcAdd(name, table, column, up string) *migrations.Migration {
	return &migrations.Migration{
		Name: name,
		Operations: migrations.Operations{
			&migrations.OpAddColumn{Table: table, Up: up, Column: migrations.Column{Name: column, Type: "integer", Nullable: true}},
		},
	}
}

func dcLast() *migrations.Migration {
	return &migrations.Migration{
		Name:       "09_last",
		Operations: migrations.Operations{&migrations.OpRawSQL{Up: "SELECT 1", Down: "SELECT 1"}},
	}
}

func dcIndex(name, table, column string) *migrations.Migration {
	return &migrations.Migration{
		Name: name,
		Operations: migrations.Operations{
			&migrations.OpCreateIndex{Name: "ix_" + table + "_" + column, Table: table, Columns: []migrations.IndexField{{Column: column}}},
		},
	}
}

const dcW = `CREATE TABLE w (id integer PRIMARY KEY, x integer);
	INSERT INTO w VALUES (1, 10)`

// dcLeftovers counts pgroll's own columns, functions and constraints left in
// the public schema.
func dcLeftovers(t *testing.T, db *sql.DB) int {
	t.Helper()
	return *dcInt(t, db, `
		SELECT (SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND column_name LIKE '\_pgroll\_%')
		     + (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'public' AND p.proname LIKE '\_pgroll\_%')
		     + (SELECT count(*) FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace WHERE n.nspname = 'public' AND c.conname LIKE '\_pgroll\_%')
		     + (SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname LIKE '\_pgroll\_%')`)
}

// ---------------------------------------------------------------------------
// 1. The most common train: add a column, then index it. Reverting it.
// ---------------------------------------------------------------------------

// `pgroll revert` of [add_column a (up), create_index on a].
func TestTrainShapeRevertTrainWithIndexOnNewColumn(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		dcBase(t, mig, dcW)
		last := dcIndex("09_last", "w", "a")
		dcTrain(t, mig, []*migrations.Migration{dcAdd("01_add_a", "w", "a", "x + 1"), last})

		require.NoError(t, dcExecAs(t, db, dcPrev, `INSERT INTO w (id, x) VALUES (2, 20)`))
		require.NoError(t, dcExecAs(t, db, dcNew, `INSERT INTO w (id, x, a) VALUES (3, 30, 99)`))

		err := dcSafely(func() error { _, err := mig.Revert(ctx); return err })
		require.NoError(t, err, "pgroll revert of the train")
		require.Zero(t, dcLeftovers(t, db))
		require.NoError(t, dcExecAs(t, db, dcPrev, `INSERT INTO w (id, x) VALUES (4, 40)`))
	})
}

// `pgroll rollback` of the active last migration, when it names the column an
// earlier migration of the train added.
func TestTrainShapeRollbackLeafNamingNewColumn(t *testing.T) {
	t.Parallel()

	leaves := map[string]*migrations.Migration{
		"create_index": dcIndex("09_last", "w", "a"),
		"alter_column": {Name: "09_last", Operations: migrations.Operations{&migrations.OpAlterColumn{
			Table: "w", Column: "a",
			Check: &migrations.CheckConstraint{Name: "a_pos", Constraint: "a > 0"},
			Up:    "a", Down: "a",
		}}},
		"rename_column": {Name: "09_last", Operations: migrations.Operations{&migrations.OpRenameColumn{Table: "w", From: "a", To: "b"}}},
		"drop_column":   {Name: "09_last", Operations: migrations.Operations{&migrations.OpDropColumn{Table: "w", Column: "a", Down: "x + 1"}}},
	}
	for name, leaf := range leaves {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				dcBase(t, mig, dcW)
				dcTrain(t, mig, []*migrations.Migration{dcAdd("01_add_a", "w", "a", "x + 1"), leaf})

				require.NoError(t, dcSafely(func() error { return mig.Rollback(ctx) }), "pgroll rollback of the last migration")
				require.NoError(t, dcExecAs(t, db, dcPrev, `INSERT INTO w (id, x) VALUES (2, 20)`))
			})
		})
	}
}

// A later migration's Start fails for an ordinary reason (duplicate values
// under a unique index). pgroll rolls the failed Start back by itself.
func TestTrainShapeFailedStartNamingNewColumnRollsBack(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		dcBase(t, mig, `CREATE TABLE w (id integer PRIMARY KEY, x integer);
			INSERT INTO w VALUES (1, 10), (2, 10)`)

		err := dcTrainErr(mig, []*migrations.Migration{
			dcAdd("01_add_a", "w", "a", "x + 1"),
			{Name: "02_unique_a", Operations: migrations.Operations{&migrations.OpCreateIndex{
				Name: "ux_w_a", Table: "w", Unique: true, Columns: []migrations.IndexField{{Column: "a"}},
			}}},
			dcLast(),
		})
		require.Error(t, err, "two rows share a value, so the unique index cannot build")
		t.Logf("train error: %v", err)
		assert.NotContains(t, err.Error(), "PANIC", "the failed Start is rolled back, not crashed out of")

		active, aerr := mig.State().IsActiveMigrationPeriod(ctx, "public")
		require.NoError(t, aerr)
		assert.False(t, active, "nothing is left in progress after the failed Start")
	})
}

// ---------------------------------------------------------------------------
// 2. create_constraint, which stays inline, on the column a deferred
//    add_column added.
// ---------------------------------------------------------------------------

func TestTrainShapeCreateConstraintOnNewColumn(t *testing.T) {
	t.Parallel()

	check := "a >= 0"
	constraints := map[string]*migrations.OpCreateConstraint{
		"unique": {
			Name: "w_a_key", Table: "w", Type: migrations.OpCreateConstraintTypeUnique, Columns: []string{"a"},
			Up: migrations.MultiColumnUpSQL{"a": "a"}, Down: migrations.MultiColumnDownSQL{"a": "a"},
		},
		"check": {
			Name: "w_a_nonneg", Table: "w", Type: migrations.OpCreateConstraintTypeCheck, Columns: []string{"a"}, Check: &check,
			Up: migrations.MultiColumnUpSQL{"a": "a"}, Down: migrations.MultiColumnDownSQL{"a": "a"},
		},
	}
	for name, constraint := range constraints {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				dcBase(t, mig, dcW)
				dcTrain(t, mig, []*migrations.Migration{
					dcAdd("01_add_a", "w", "a", "x + 1"),
					{Name: "02_constrain_a", Operations: migrations.Operations{constraint}},
					dcLast(),
				})

				physical := *dcInt(t, db, `SELECT count(*) FROM information_schema.columns
					WHERE table_schema = 'public' AND table_name = 'w' AND (column_name = 'a' OR column_name LIKE '\_pgroll\_new\_a\_%')`)
				assert.Equal(t, 1, physical, "one physical column stands for w.a while the train is expanded")

				require.NoError(t, dcExecAs(t, db, dcNew, `INSERT INTO w (id, x, a) VALUES (3, 30, 99)`),
					"the new revision writes the column")
				dcWant(t, db, 99, `SELECT a FROM public_09_last.w WHERE id = 3`,
					"the new revision reads back what it wrote")
				require.NoError(t, dcExecAs(t, db, dcPrev, `INSERT INTO w (id, x) VALUES (2, 20)`),
					"the previous revision writes the table")

				require.NoError(t, dcSafely(func() error { return mig.Complete(ctx) }), "the contraction")
				got := dcInt(t, db, `SELECT a FROM public.w WHERE id = 3`)
				if assert.NotNil(t, got, "the new revision's value survives the contraction") {
					assert.Equal(t, 99, *got)
				}
				dcWant(t, db, 1, `SELECT count(*) FROM pg_constraint WHERE conname = '`+constraint.Name+`'`,
					"the constraint is on the table after the contraction")
				assert.Zero(t, dcLeftovers(t, db))
			})
		})
	}
}

// ---------------------------------------------------------------------------
// 3. Two add_columns with `up` on one table, and the contraction.
// ---------------------------------------------------------------------------

// The second migration's trigger function declares the first one's column
// under its temporary name. When the first drains, that name is gone, and
// the table cannot be written until the second drains. Held open here with a
// reader on another table of the second migration.
func TestTrainShapeTwoAddColumnsWritesBetweenDrains(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
		[]roll.Option{roll.WithLockTimeoutMs(200)},
		func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			dcBase(t, mig, `CREATE TABLE w (id integer PRIMARY KEY, x integer);
				CREATE TABLE z (id integer PRIMARY KEY);
				INSERT INTO w VALUES (1, 10)`)
			dcTrain(t, mig, []*migrations.Migration{
				dcAdd("01_add_a", "w", "a", "x + 1"),
				{Name: "02_add_b", Operations: migrations.Operations{
					&migrations.OpAddColumn{Table: "z", Up: "1", Column: migrations.Column{Name: "c", Type: "integer", Nullable: true}},
					&migrations.OpAddColumn{Table: "w", Up: "x + 2", Column: migrations.Column{Name: "b", Type: "integer", Nullable: true}},
				}},
				dcLast(),
			})
			require.NoError(t, dcExecAs(t, db, dcNew, `INSERT INTO w (id, x, a, b) VALUES (2, 20, 1, 2)`),
				"the new revision writes before the contraction")

			blocker, err := db.Conn(ctx)
			require.NoError(t, err)
			defer blocker.Close()
			_, err = blocker.ExecContext(ctx, `BEGIN; SELECT count(*) FROM z`)
			require.NoError(t, err)

			done := make(chan error, 1)
			go func() { done <- dcSafely(func() error { return mig.Complete(ctx) }) }()

			deadline := time.Now().Add(15 * time.Second)
			for time.Now().Before(deadline) {
				var renamed bool
				require.NoError(t, db.QueryRowContext(ctx, `
					SELECT EXISTS (SELECT 1 FROM information_schema.columns
					               WHERE table_schema = 'public' AND table_name = 'w' AND column_name = 'a')`).Scan(&renamed))
				if renamed {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			time.Sleep(200 * time.Millisecond)
			windowErr := dcExecAs(t, db, dcNew, `INSERT INTO w (id, x, a, b) VALUES (3, 30, 1, 2)`)

			_, err = blocker.ExecContext(ctx, `ROLLBACK`)
			require.NoError(t, err)
			require.NoError(t, <-done)
			require.NoError(t, windowErr, "the new revision's write while the train contracts")
		})
}

// The same window with nothing holding it open: two NOT NULL columns on a
// table with rows to validate, and the fleet writing while `pgroll complete`
// runs.
func TestTrainShapeTwoAddColumnsWritesDuringContraction(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		dcBase(t, mig, `CREATE TABLE w (id integer PRIMARY KEY, x integer);
			INSERT INTO w SELECT g, g FROM generate_series(1, 300000) g`)
		notNull := func(name, column, up string) *migrations.Migration {
			return &migrations.Migration{Name: name, Operations: migrations.Operations{
				&migrations.OpAddColumn{Table: "w", Up: up, Column: migrations.Column{Name: column, Type: "integer", Nullable: false}},
			}}
		}
		dcTrain(t, mig, []*migrations.Migration{
			notNull("01_add_a", "a", "x + 1"),
			notNull("02_add_b", "b", "x + 2"),
			dcLast(),
		})

		var failed, written atomic.Int64
		var firstErr atomic.Value
		stop := make(chan struct{})
		var wg sync.WaitGroup
		for worker := range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := db.Conn(ctx)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, err := conn.ExecContext(ctx, "SET search_path TO "+dcNew); err != nil {
					return
				}
				for i := 0; ; i++ {
					select {
					case <-stop:
						return
					default:
					}
					id := 1_000_000 + worker*1_000_000 + i
					if _, err := conn.ExecContext(ctx, `INSERT INTO w (id, x, a, b) VALUES ($1, 1, 2, 3)`, id); err != nil {
						failed.Add(1)
						firstErr.CompareAndSwap(nil, err.Error())
					} else {
						written.Add(1)
					}
				}
			}()
		}
		time.Sleep(300 * time.Millisecond)
		start := time.Now()
		completeErr := dcSafely(func() error { return mig.Complete(ctx) })
		took := time.Since(start)
		time.Sleep(100 * time.Millisecond)
		close(stop)
		wg.Wait()

		require.NoError(t, completeErr)
		t.Logf("contraction took %s; %d writes succeeded, %d failed; first error: %v",
			took, written.Load(), failed.Load(), firstErr.Load())
		require.Zero(t, failed.Load(), "writes of the new revision that failed while the train contracted")
	})
}

// ---------------------------------------------------------------------------
// 4. Later native operations on the new column or its table.
// ---------------------------------------------------------------------------

func TestTrainShapeLaterNativeOps(t *testing.T) {
	t.Parallel()

	bigint := "bigint"
	cases := map[string]struct {
		later     []*migrations.Migration
		newInsert string
		// finalCheck is a query that must return 1 after the contraction.
		finalCheck string
		skipPrev   bool
	}{
		"create_index mid-train": {
			later:      []*migrations.Migration{dcIndex("02_index_a", "w", "a")},
			newInsert:  `INSERT INTO w (id, x, a) VALUES (3, 30, 99)`,
			finalCheck: `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'ix_w_a' AND indexdef LIKE '%(a)%'`,
		},
		"create_index with a predicate on the column": {
			later: []*migrations.Migration{{Name: "02_index_a", Operations: migrations.Operations{&migrations.OpCreateIndex{
				Name: "ix_w_x_where_a", Table: "w", Columns: []migrations.IndexField{{Column: "x"}}, Predicate: "a IS NOT NULL",
			}}}},
			newInsert:  `INSERT INTO w (id, x, a) VALUES (3, 30, 99)`,
			finalCheck: `SELECT count(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'ix_w_x_where_a'`,
		},
		"alter_column type": {
			later: []*migrations.Migration{{Name: "02_widen_a", Operations: migrations.Operations{&migrations.OpAlterColumn{
				Table: "w", Column: "a", Type: &bigint, Up: "a", Down: "a",
			}}}},
			newInsert:  `INSERT INTO w (id, x, a) VALUES (3, 30, 99)`,
			finalCheck: `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'w' AND column_name = 'a' AND data_type = 'bigint'`,
		},
		"alter_column check": {
			later: []*migrations.Migration{{Name: "02_check_a", Operations: migrations.Operations{&migrations.OpAlterColumn{
				Table: "w", Column: "a", Check: &migrations.CheckConstraint{Name: "a_pos", Constraint: "a > 0"}, Up: "a", Down: "a",
			}}}},
			newInsert:  `INSERT INTO w (id, x, a) VALUES (3, 30, 99)`,
			finalCheck: `SELECT count(*) FROM pg_constraint WHERE conname = 'a_pos' AND convalidated`,
		},
		"rename_column": {
			later: []*migrations.Migration{{Name: "02_rename_a", Operations: migrations.Operations{
				&migrations.OpRenameColumn{Table: "w", From: "a", To: "b"},
			}}},
			newInsert:  `INSERT INTO w (id, x, b) VALUES (3, 30, 99)`,
			finalCheck: `SELECT count(*) FROM public.w WHERE id = 3 AND b = 99`,
		},
		"drop_column of the new column": {
			later: []*migrations.Migration{{Name: "02_drop_a", Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "w", Column: "a", Down: "x + 1"},
			}}},
			newInsert:  `INSERT INTO w (id, x) VALUES (3, 30)`,
			finalCheck: `SELECT 1 - count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'w' AND column_name = 'a'`,
		},
		"rename_table": {
			later: []*migrations.Migration{{Name: "02_rename_w", Operations: migrations.Operations{
				&migrations.OpRenameTable{From: "w", To: "w2"},
			}}},
			newInsert:  `INSERT INTO w2 (id, x, a) VALUES (3, 30, 99)`,
			finalCheck: `SELECT count(*) FROM public.w2 WHERE id = 3 AND a = 99`,
		},
		"drop_table": {
			later: []*migrations.Migration{{Name: "02_drop_w", Operations: migrations.Operations{
				&migrations.OpDropTable{Name: "w"},
			}}},
			newInsert:  `SELECT 1`,
			finalCheck: `SELECT 1 - count(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'w'`,
		},
		"add_column whose check names the new column": {
			later: []*migrations.Migration{{Name: "02_add_b", Operations: migrations.Operations{&migrations.OpAddColumn{
				Table: "w", Up: "COALESCE(b, a + 1)",
				Column: migrations.Column{
					Name: "b", Type: "integer", Nullable: true,
					Check: &migrations.CheckConstraint{Name: "b_gt_a", Constraint: "b > a"},
				},
			}}}},
			newInsert:  `INSERT INTO w (id, x, a, b) VALUES (3, 30, 99, 100)`,
			finalCheck: `SELECT count(*) FROM pg_constraint WHERE conname = 'b_gt_a' AND convalidated`,
		},
		"add_column whose up reads the new column": {
			later:      []*migrations.Migration{dcAdd("02_add_b", "w", "b", "a + 1")},
			newInsert:  `INSERT INTO w (id, x, a, b) VALUES (3, 30, 99, 100)`,
			finalCheck: `SELECT count(*) FROM public.w WHERE id = 1 AND a = 11 AND b = 12`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				dcBase(t, mig, dcW)
				train := append([]*migrations.Migration{dcAdd("01_add_a", "w", "a", "x + 1")}, tc.later...)
				train = append(train, dcLast())
				require.NoError(t, dcTrainErr(mig, train), "applying the train")

				assert.NoError(t, dcExecAs(t, db, dcPrev, `INSERT INTO w (id, x) VALUES (2, 20)`),
					"the previous revision writes the table")
				assert.NoError(t, dcExecAs(t, db, dcNew, tc.newInsert), "the new revision writes the table")

				require.NoError(t, dcSafely(func() error { return mig.Complete(ctx) }), "the contraction")
				dcWant(t, db, 1, tc.finalCheck, "the schema the train was meant to end in")
				assert.Zero(t, dcLeftovers(t, db), "nothing of pgroll's is left")
			})
		})
	}
}

// ---------------------------------------------------------------------------
// 5. Column shapes, mid-train.
// ---------------------------------------------------------------------------

func TestTrainShapeColumnShapes(t *testing.T) {
	t.Parallel()

	zero := "0"
	cases := map[string]migrations.Column{
		"nullable":                   {Name: "a", Type: "integer", Nullable: true},
		"not null, no default":       {Name: "a", Type: "integer", Nullable: false},
		"not null, constant default": {Name: "a", Type: "integer", Nullable: false, Default: &zero},
		"check":                      {Name: "a", Type: "integer", Nullable: true, Check: &migrations.CheckConstraint{Name: "a_pos", Constraint: "a > 0"}},
		"unique":                     {Name: "a", Type: "integer", Nullable: true, Unique: true},
		"references":                 {Name: "a", Type: "integer", Nullable: true, References: &migrations.ForeignKeyReference{Name: "w_a_fk", Table: "p", Column: "id"}},
	}
	for name, column := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				dcBase(t, mig, `CREATE TABLE p (id integer PRIMARY KEY);
					INSERT INTO p SELECT g FROM generate_series(1, 200) g;
					CREATE TABLE w (id integer PRIMARY KEY, x integer);
					INSERT INTO w VALUES (1, 10)`)
				require.NoError(t, dcTrainErr(mig, []*migrations.Migration{
					{Name: "01_add_a", Operations: migrations.Operations{&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: column}}},
					dcIndex("02_index_x", "w", "x"),
					dcLast(),
				}))

				require.NoError(t, dcExecAs(t, db, dcPrev, `INSERT INTO w (id, x) VALUES (2, 20)`))
				require.NoError(t, dcExecAs(t, db, dcNew, `INSERT INTO w (id, x, a) VALUES (3, 30, 99)`))
				require.NoError(t, dcExecAs(t, db, dcPrev, `UPDATE w SET x = x WHERE id = 3`))

				require.NoError(t, dcSafely(func() error { return mig.Complete(ctx) }))
				dcWant(t, db, 21, `SELECT a FROM public.w WHERE id = 2`)
				assert.Zero(t, dcLeftovers(t, db))
			})
		})
	}
}

// A volatile default forces `up` to equal it (UpSQLMustBeColumnDefaultError),
// so the author cannot write an `up` that keeps the stored value.
func TestTrainShapeVolatileDefaultMidTrain(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		dcBase(t, mig, dcW)
		uuid := "gen_random_uuid()"
		dcTrain(t, mig, []*migrations.Migration{
			{Name: "01_add_public_id", Operations: migrations.Operations{&migrations.OpAddColumn{
				Table: "w", Up: uuid, Column: migrations.Column{Name: "public_id", Type: "uuid", Nullable: false, Default: &uuid},
			}}},
			dcLast(),
		})
		read := func() string {
			var id string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT public_id FROM public_09_last.w WHERE id = 1`).Scan(&id))
			return id
		}
		handedOut := read()
		require.NoError(t, dcExecAs(t, db, dcPrev, `UPDATE w SET x = x + 1 WHERE id = 1`))
		require.Equal(t, handedOut, read(), "the identifier survives a previous-revision update of another column")
	})
}

// ---------------------------------------------------------------------------
// 6. Mixed migrations.
// ---------------------------------------------------------------------------

func TestTrainShapeMixedMigrations(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		mixed      *migrations.Migration
		later      []*migrations.Migration
		prevInsert string
		newInsert  string
	}{
		"add_column and an index on it": {
			mixed: &migrations.Migration{Name: "01_mixed", Operations: migrations.Operations{
				&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
				&migrations.OpCreateIndex{Name: "ix_w_a", Table: "w", Columns: []migrations.IndexField{{Column: "a"}}},
			}},
			prevInsert: `INSERT INTO w (id, x) VALUES (2, 20)`,
			newInsert:  `INSERT INTO w (id, x, a) VALUES (3, 30, 99)`,
		},
		"create_table and an add_column on it": {
			mixed: &migrations.Migration{Name: "01_mixed", Operations: migrations.Operations{
				&migrations.OpCreateTable{Name: "n", Columns: []migrations.Column{
					{Name: "id", Type: "integer", Pk: true},
					{Name: "x", Type: "integer", Nullable: true},
				}},
				&migrations.OpAddColumn{Table: "n", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
			}},
			// The table is new: only the new revision knows it. A later
			// raw statement of the train seeds it.
			later: []*migrations.Migration{{Name: "02_seed", Operations: migrations.Operations{
				&migrations.OpRawSQL{Up: `INSERT INTO n (id, x) VALUES (1, 10)`, Down: `SELECT 1`},
			}}},
			prevInsert: `SELECT 1`,
			newInsert:  `INSERT INTO n (id, x, a) VALUES (3, 30, 99)`,
		},
		"create_table, then seeding the column an add_column gave it": {
			mixed: &migrations.Migration{Name: "01_mixed", Operations: migrations.Operations{
				&migrations.OpCreateTable{Name: "n", Columns: []migrations.Column{
					{Name: "id", Type: "integer", Pk: true},
					{Name: "x", Type: "integer", Nullable: true},
				}},
				&migrations.OpAddColumn{Table: "n", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
			}},
			later: []*migrations.Migration{{Name: "02_seed", Operations: migrations.Operations{
				&migrations.OpRawSQL{Up: `INSERT INTO n (id, x, a) VALUES (1, 10, 5)`, Down: `SELECT 1`},
			}}},
			prevInsert: `SELECT 1`,
			newInsert:  `INSERT INTO n (id, x, a) VALUES (3, 30, 99)`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				dcBase(t, mig, dcW)
				train := append([]*migrations.Migration{tc.mixed}, tc.later...)
				train = append(train, dcLast())
				require.NoError(t, dcTrainErr(mig, train), "applying the train")
				assert.NoError(t, dcExecAs(t, db, dcPrev, tc.prevInsert))
				assert.NoError(t, dcExecAs(t, db, dcNew, tc.newInsert))
				require.NoError(t, dcSafely(func() error { return mig.Complete(ctx) }), "the contraction")
				assert.Zero(t, dcLeftovers(t, db))
			})
		})
	}
}

// ---------------------------------------------------------------------------
// 7. Reverts.
// ---------------------------------------------------------------------------

// A bounded revert that leaves the deferred add_column as the leaf, then the
// contraction of what is left.
func TestTrainShapeBoundedRevertOntoDeferredAddColumn(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		dcBase(t, mig, dcW)
		dcTrain(t, mig, []*migrations.Migration{
			dcAdd("01_add_a", "w", "a", "x + 1"),
			{Name: "02_add_c", Operations: migrations.Operations{
				&migrations.OpAddColumn{Table: "w", Column: migrations.Column{Name: "c", Type: "integer", Nullable: true}},
			}},
			dcIndex("03_index_c", "w", "c"),
			dcLast(),
		})

		err := dcSafely(func() error { _, err := mig.Revert(ctx, roll.WithRevertTo("01_add_a")); return err })
		require.NoError(t, err, "bounded revert to the deferred add_column")

		const leaf = "public_01_add_a"
		require.NoError(t, dcExecAs(t, db, leaf, `INSERT INTO w (id, x, a) VALUES (3, 30, 99)`))
		require.NoError(t, dcExecAs(t, db, dcPrev, `INSERT INTO w (id, x) VALUES (2, 20)`))
		dcWant(t, db, 99, `SELECT a FROM public_01_add_a.w WHERE id = 3`)
		dcWant(t, db, 21, `SELECT a FROM public_01_add_a.w WHERE id = 2`)

		err = dcSafely(func() error { _, _, err := mig.FinishContraction(ctx); return err })
		require.NoError(t, err, "pgroll complete after the bounded revert")
		dcWant(t, db, 99, `SELECT a FROM public.w WHERE id = 3`)
		assert.Zero(t, dcLeftovers(t, db))
	})
}

// A bounded revert that keeps a later migration naming the new column.
func TestTrainShapeBoundedRevertKeepingIndexOnNewColumn(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		dcBase(t, mig, dcW)
		dcTrain(t, mig, []*migrations.Migration{
			dcAdd("01_add_a", "w", "a", "x + 1"),
			dcIndex("02_index_a", "w", "a"),
			dcAdd("03_add_b", "w", "b", "x + 2"),
			dcLast(),
		})
		err := dcSafely(func() error { _, err := mig.Revert(ctx, roll.WithRevertTo("02_index_a")); return err })
		require.NoError(t, err, "bounded revert to the index")
		require.NoError(t, dcExecAs(t, db, "public_02_index_a", `INSERT INTO w (id, x, a) VALUES (3, 30, 99)`))
		require.NoError(t, dcExecAs(t, db, dcPrev, `INSERT INTO w (id, x) VALUES (2, 20)`))

		err = dcSafely(func() error { _, err := mig.Revert(ctx); return err })
		require.NoError(t, err, "then the rest of the train")
		assert.Zero(t, dcLeftovers(t, db))
	})
}

// A sealed revert's inverse of a drop_column is an add_column with `up`, so
// the inverse train defers it too. Here the next inverse works on that
// column.
func TestTrainShapeSealedRevertOfDropColumn(t *testing.T) {
	t.Parallel()

	for name, expandOnly := range map[string]bool{"one shot": false, "expand only": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				dcBase(t, mig, `CREATE TABLE u (id integer PRIMARY KEY, v integer, email text);
					INSERT INTO u VALUES (1, 5, 'a@example.com')`)
				bigint := "bigint"
				dcTrain(t, mig, []*migrations.Migration{
					{Name: "01_widen_v", Operations: migrations.Operations{&migrations.OpAlterColumn{
						Table: "u", Column: "v", Type: &bigint, Up: "v", Down: "v",
					}}},
					{Name: "02_drop_v", Operations: migrations.Operations{&migrations.OpDropColumn{Table: "u", Column: "v", Down: "7"}}},
					dcLast(),
				})
				require.NoError(t, mig.Complete(ctx))

				var opts []roll.SealedRevertOption
				if expandOnly {
					opts = append(opts, roll.WithExpandOnly())
				}
				err := dcSafely(func() error { _, err := mig.RevertSealed(ctx, "00_base", nil, opts...); return err })
				require.NoError(t, err, "sealed revert")
				if expandOnly {
					require.NoError(t, dcExecAs(t, db, dcNew, `INSERT INTO u (id, email) VALUES (2, 'b@example.com')`),
						"the current revision writes while the fleet repins")
					require.NoError(t, dcExecAs(t, db, dcPrev, `INSERT INTO u (id, v, email) VALUES (3, 9, 'c@example.com')`),
						"the restored revision writes")
					require.NoError(t, dcSafely(func() error { return mig.Complete(ctx) }))
					_, err := mig.FinishPendingSealedRevert(ctx)
					require.NoError(t, err)
				}
				dcWant(t, db, 1, `SELECT count(*) FROM information_schema.columns
					WHERE table_schema = 'public' AND table_name = 'u' AND column_name = 'v' AND data_type = 'integer'`)
				assert.Zero(t, dcLeftovers(t, db))
			})
		})
	}
}

// ---------------------------------------------------------------------------
// 8. The known cost: raw SQL later in the train that names the column.
// ---------------------------------------------------------------------------

func TestTrainShapeRawSQLNamingNewColumn(t *testing.T) {
	t.Parallel()

	statements := map[string]string{
		"data fix":          `UPDATE w SET a = 500 WHERE id = 1`,
		"expression index":  `CREATE INDEX ix_w_a_expr ON w ((a + 1))`,
		"NOT VALID check":   `ALTER TABLE w ADD CONSTRAINT a_pos CHECK (a > 0) NOT VALID`,
		"comment on column": `COMMENT ON COLUMN w.a IS 'x'`,
	}
	for name, stmt := range statements {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				dcBase(t, mig, dcW)
				err := dcTrainErr(mig, []*migrations.Migration{
					dcAdd("01_add_a", "w", "a", "x + 1"),
					{Name: "02_raw", Operations: migrations.Operations{&migrations.OpRawSQL{Up: stmt, Down: "SELECT 1"}}},
					dcLast(),
				})
				if err != nil {
					t.Logf("train error: %v", err)
					active, aerr := mig.State().IsActiveMigrationPeriod(ctx, "public")
					require.NoError(t, aerr)
					t.Logf("a migration is left in progress: %v", active)
				}
				require.NoError(t, err, "raw SQL naming a column an earlier migration of the train added")
			})
		})
	}
}

// ---------------------------------------------------------------------------
// 9. Upgrading with a window open.
// ---------------------------------------------------------------------------

// The previous build completed a mid-train add_column inline. Its row is
// done, not deferred, unsealed. RevertTargets reads CompleteMustBeDeferred to
// tell a healthy inline row from a contraction a crash interrupted.
func TestTrainShapeRevertOfATrainThePreviousBuildApplied(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		dcBase(t, mig, dcW)

		// As the previous build's `pgroll migrate` did it.
		require.NoError(t, mig.Start(ctx, dcAdd("01_add_a", "w", "a", "x + 1"), backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithSkipSchemaDrop()))
		require.NoError(t, mig.Start(ctx, dcLast(), backfill.NewConfig()))

		err := dcSafely(func() error { _, err := mig.Revert(ctx); return err })
		require.NoError(t, err, "pgroll revert, after the upgrade, of a train the previous build expanded")
	})
}

// ---------------------------------------------------------------------------
// 10. Comparisons: what the base build already does with a deferred shape.
// ---------------------------------------------------------------------------

// A migration that mixes an add_column with `up` and a drop_column is
// deferred on the base build too. Reverting a train that then indexes the
// new column is the same path as test 1.
func TestTrainShapeMixedAddAndDropThenIndexRevert(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		dcBase(t, mig, `CREATE TABLE w (id integer PRIMARY KEY, x integer, y integer);
			INSERT INTO w VALUES (1, 10, 1)`)
		dcTrain(t, mig, []*migrations.Migration{
			{Name: "01_rekey", Operations: migrations.Operations{
				&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
				&migrations.OpDropColumn{Table: "w", Column: "y", Down: "0"},
			}},
			dcIndex("09_last", "w", "a"),
		})
		err := dcSafely(func() error { _, err := mig.Revert(ctx); return err })
		require.NoError(t, err, "pgroll revert of the train")
	})
}

// A deferred alter_column, then a drop_table of its table.
func TestTrainShapeAlterColumnThenDropTable(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		dcBase(t, mig, dcW)
		dcTrain(t, mig, []*migrations.Migration{
			{Name: "01_check_x", Operations: migrations.Operations{&migrations.OpAlterColumn{
				Table: "w", Column: "x", Check: &migrations.CheckConstraint{Name: "x_pos", Constraint: "x > 0"}, Up: "x", Down: "x",
			}}},
			{Name: "02_drop_w", Operations: migrations.Operations{&migrations.OpDropTable{Name: "w"}}},
			dcLast(),
		})
		assert.NoError(t, dcExecAs(t, db, dcPrev, `INSERT INTO w (id, x) VALUES (2, 20)`),
			"the previous revision writes the table the train drops")
		require.NoError(t, dcSafely(func() error { return mig.Complete(ctx) }), "the contraction")
	})
}

// An add_column with a CHECK and no `up` has no trigger to lose. Five of the
// downstream's twenty requires-leaf migrations are this shape.
func TestTrainShapeAddColumnCheckWithoutUpMidTrain(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		dcBase(t, mig, dcW)
		dcTrain(t, mig, []*migrations.Migration{
			{Name: "01_add_c", Operations: migrations.Operations{&migrations.OpAddColumn{
				Table: "w",
				Column: migrations.Column{
					Name: "c", Type: "integer", Nullable: true,
					Check: &migrations.CheckConstraint{Name: "w_one_owner", Constraint: "num_nonnulls(x, c) = 1"},
				},
			}}},
			dcIndex("02_index_c", "w", "c"),
			dcLast(),
		})
		require.NoError(t, dcExecAs(t, db, dcPrev, `INSERT INTO w (id, x) VALUES (2, 20)`),
			"the previous revision's write meets the CHECK without any trigger")
		require.NoError(t, dcExecAs(t, db, dcNew, `INSERT INTO w (id, c) VALUES (3, 5)`))
		require.Error(t, dcExecAs(t, db, dcNew, `INSERT INTO w (id, x, c) VALUES (4, 1, 5)`), "the CHECK holds")
		require.Error(t, dcExecAs(t, db, dcPrev, `INSERT INTO w (id) VALUES (5)`), "for the previous revision too")
		require.NoError(t, dcSafely(func() error { return mig.Complete(ctx) }))
		assert.Zero(t, dcLeftovers(t, db))
	})
}

// ---------------------------------------------------------------------------
// 11. The prior review's findings, ported to the proposal's lifecycle (no
//     kept triggers: the add_column is deferred instead).
// ---------------------------------------------------------------------------

// Prior finding 5: `pgroll complete` on a train that stopped after its first
// migration.
func TestTrainShapePortedContractionOfAnUnfinishedTrain(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		dcBase(t, mig, dcW)
		final := dcLast()
		first := &migrations.Migration{Name: "01_add_a", Operations: migrations.Operations{
			&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: false}},
		}}
		require.NoError(t, mig.Start(ctx, first, backfill.NewConfig(), dcMidOpts(mig, final)...))
		opt := roll.WithSkipSchemaDrop()
		if roll.DefersCompleteInTrain(first, mig.UseVersionSchema()) {
			opt = roll.WithDeferComplete()
		}
		require.NoError(t, mig.Complete(ctx, opt))

		_, _, err := mig.FinishContraction(ctx)
		t.Logf("FinishContraction on the unfinished train: %v", err)
		require.NoError(t, dcExecAs(t, db, dcPrev, `INSERT INTO w (id, x) VALUES (3, 30)`),
			"the previous revision's write after `pgroll complete` on the unfinished train")
	})
}

// Prior finding 7: a contraction refused because a straggler still holds the
// previous schema must leave the straggler able to write.
func TestTrainShapePortedRefusedContractionWithAStraggler(t *testing.T) {
	t.Parallel()
	for name, viaFinish := range map[string]bool{"Complete": false, "FinishContraction": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
				[]roll.Option{roll.WithLockTimeoutMs(100), roll.WithLockRetryTimeout(300 * time.Millisecond)},
				func(mig *roll.Roll, db *sql.DB) {
					ctx := context.Background()
					dcBase(t, mig, `CREATE TABLE w (id integer PRIMARY KEY, x integer);
						CREATE TABLE z (id integer PRIMARY KEY);
						INSERT INTO w VALUES (1, 10)`)
					dcTrain(t, mig, []*migrations.Migration{
						{Name: "01_add_a", Operations: migrations.Operations{&migrations.OpAddColumn{
							Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: false},
						}}},
						dcIndex("02_index_x", "w", "x"),
						dcLast(),
					})
					if viaFinish {
						_, err := mig.Revert(ctx, roll.WithRevertTo("02_index_x"))
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
					require.ErrorContains(t, err, "public_00_base")
					_, err = straggler.ExecContext(ctx, `INSERT INTO w (id, x) VALUES (2, 20)`)
					require.NoError(t, err, "the straggler's write after the refused contraction")
				})
		})
	}
}

// Prior finding 8: version schemas disabled.
func TestTrainShapePortedVersionSchemasDisabled(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
		[]roll.Option{roll.WithVersionSchema(false)},
		func(mig *roll.Roll, db *sql.DB) {
			dcBase(t, mig, dcW)
			dcTrain(t, mig, []*migrations.Migration{dcAdd("01_add_a", "w", "a", "x + 1"), dcIndex("09_last", "w", "x")})
			require.NoError(t, dcExecAs(t, db, "public", `INSERT INTO w (id, x, a) VALUES (3, 30, 99)`))
			dcWant(t, db, 99, `SELECT a FROM public.w WHERE id = 3`, "the writer's own value for a column a mid-train migration added")
		})
}

// Prior finding 9: trigger function names cut at 63 bytes.
func TestTrainShapePortedTruncatedFunctionNames(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		const table = "portfolio_monitoring_subscription_items"
		dcBase(t, mig, `CREATE TABLE `+table+` (id integer PRIMARY KEY, x integer);
			INSERT INTO `+table+` VALUES (1, 10)`)
		dcTrain(t, mig, []*migrations.Migration{
			dcAdd("01_add_code", table, "previous_status_code", "x + 1"),
			dcAdd("02_add_text", table, "previous_status_text", "x + 2"),
			dcLast(),
		})
		require.NoError(t, dcExecAs(t, db, dcPrev, `INSERT INTO `+table+` (id, x) VALUES (2, 20)`))
		require.NoError(t, dcSafely(func() error { return mig.Complete(ctx) }))
		dcWant(t, db, 21, `SELECT previous_status_code FROM public.`+table+` WHERE id = 2`, "the first column is filled for the previous revision")
		dcWant(t, db, 22, `SELECT previous_status_text FROM public.`+table+` WHERE id = 2`, "the second column is filled for the previous revision")
	})
}

// ---------------------------------------------------------------------------
// 12. pgroll's own examples, the way `make examples` runs them: one
//     `pgroll migrate examples --complete` train.
// ---------------------------------------------------------------------------

func TestTrainShapeExamplesChainAsOneTrain(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		dir := os.DirFS("../../examples")
		files, err := migrations.CollectFilesFromDir(dir)
		require.NoError(t, err)
		train := make([]*migrations.Migration, 0, len(files))
		for _, f := range files {
			m, err := migrations.ReadMigration(dir, f)
			require.NoError(t, err, f)
			train = append(train, m)
		}
		require.NotEmpty(t, train)

		for i, m := range train {
			last := i == len(train)-1
			var startOpts []roll.StartOption
			if !last {
				startOpts = append(startOpts, roll.WithoutVersionSchema())
			}
			err := dcSafely(func() error { return mig.Start(ctx, m, backfill.NewConfig(), startOpts...) })
			require.NoError(t, err, "start of %s (%d of %d)", m.Name, i+1, len(train))
			var completeOpts []roll.CompleteOption
			switch {
			case last:
			case roll.DefersCompleteInTrain(m, mig.UseVersionSchema()):
				completeOpts = append(completeOpts, roll.WithDeferComplete())
			default:
				completeOpts = append(completeOpts, roll.WithSkipSchemaDrop())
			}
			err = dcSafely(func() error { return mig.Complete(ctx, completeOpts...) })
			require.NoError(t, err, "complete of %s (%d of %d)", m.Name, i+1, len(train))
		}
	})
}

// dcMidOpts and dcLeafOpts are the Start options of a train's earlier
// migrations and of its last one.
func dcMidOpts(mig *roll.Roll, final *migrations.Migration) []roll.StartOption {
	return append([]roll.StartOption{roll.WithoutVersionSchema()}, dcLeafOpts(mig, final)...)
}

func dcLeafOpts(mig *roll.Roll, final *migrations.Migration) []roll.StartOption {
	if !mig.UseVersionSchema() {
		return nil
	}
	return []roll.StartOption{roll.WithTrainTarget(final)}
}
