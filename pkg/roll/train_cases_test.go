// SPDX-License-Identifier: Apache-2.0

package roll_test

// Adversarial review of d547972. Every test builds with the base build too
// (6b2e5ea): review2_shim_test.go holds the build-specific train options.

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

const (
	r2Prev = "public_00_base"
	r2New  = "public_09_last"
	r2W    = `CREATE TABLE w (id integer PRIMARY KEY, x integer);
		INSERT INTO w VALUES (1, 10)`
)

func r2Safely(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("PANIC: %v", r)
		}
	}()
	return fn()
}

func r2Raw(name, up string) *migrations.Migration {
	return &migrations.Migration{
		Name:       name,
		Operations: migrations.Operations{&migrations.OpRawSQL{Up: up, Down: "SELECT 1"}},
	}
}

func r2Base(t *testing.T, mig *roll.Roll, up string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, mig.Start(ctx, r2Raw("00_base", up), backfill.NewConfig()))
	require.NoError(t, mig.Complete(ctx))
}

func r2Last() *migrations.Migration { return r2Raw("09_last", "SELECT 1") }

func r2Add(name, table, column, up string) *migrations.Migration {
	return &migrations.Migration{
		Name: name,
		Operations: migrations.Operations{
			&migrations.OpAddColumn{Table: table, Up: up, Column: migrations.Column{Name: column, Type: "integer", Nullable: true}},
		},
	}
}

// r2TrainErr applies a train the way `pgroll migrate` does without
// --complete, and returns the step that failed.
func r2TrainErr(mig *roll.Roll, train []*migrations.Migration) error {
	ctx := context.Background()
	final := train[len(train)-1]
	for _, m := range train[:len(train)-1] {
		if err := r2Safely(func() error { return mig.Start(ctx, m, backfill.NewConfig(), r2MidOpts(mig, final)...) }); err != nil {
			return fmt.Errorf("start %s: %w", m.Name, err)
		}
		opt := roll.WithSkipSchemaDrop()
		if r2Defers(mig, m) {
			opt = roll.WithDeferComplete()
		}
		if err := r2Safely(func() error { return mig.Complete(ctx, opt) }); err != nil {
			return fmt.Errorf("complete %s: %w", m.Name, err)
		}
	}
	if err := r2Safely(func() error { return mig.Start(ctx, final, backfill.NewConfig(), r2LeafOpts(mig, final)...) }); err != nil {
		return fmt.Errorf("start %s: %w", final.Name, err)
	}
	return nil
}

func r2Train(t *testing.T, mig *roll.Roll, train []*migrations.Migration) {
	t.Helper()
	require.NoError(t, r2TrainErr(mig, train))
}

// r2ExecAs runs stmt in a session whose search_path is set to searchPath. An
// empty searchPath leaves the session's default, `"$user", public`.
func r2ExecAs(t *testing.T, db *sql.DB, searchPath, stmt string) error {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	if searchPath != "" {
		_, err = conn.ExecContext(ctx, "SET search_path TO "+searchPath)
		require.NoError(t, err)
	} else {
		_, err = conn.ExecContext(ctx, "RESET search_path")
		require.NoError(t, err)
	}
	_, err = conn.ExecContext(ctx, stmt)
	return err
}

func r2Int(t *testing.T, db *sql.DB, query string) *int {
	t.Helper()
	var v sql.NullInt64
	require.NoError(t, db.QueryRowContext(context.Background(), query).Scan(&v))
	if !v.Valid {
		return nil
	}
	i := int(v.Int64)
	return &i
}

func r2Want(t *testing.T, db *sql.DB, want int, query string, msg ...any) bool {
	t.Helper()
	got := r2Int(t, db, query)
	if !assert.NotNil(t, got, msg...) {
		return false
	}
	return assert.Equal(t, want, *got, msg...)
}

func r2Leftovers(t *testing.T, db *sql.DB, schema string) int {
	t.Helper()
	return *r2Int(t, db, fmt.Sprintf(`
		SELECT (SELECT count(*) FROM information_schema.columns WHERE table_schema = '%[1]s' AND column_name LIKE '\_pgroll\_%%')
		     + (SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = '%[1]s' AND p.proname LIKE '\_pgroll\_%%')
		     + (SELECT count(*) FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace WHERE n.nspname = '%[1]s' AND c.conname LIKE '\_pgroll\_%%')
		     + (SELECT count(*) FROM pg_indexes WHERE schemaname = '%[1]s' AND indexname LIKE '\_pgroll\_%%')`, schema))
}

// ---------------------------------------------------------------------------
// Claim 4: sessions not pinned to a version schema.
//
// The ETL host pins `public` and contracts with --complete; Airflow's
// db_search_path defaults to `public`. Their code predates the column, so on
// the base build every one of their writes runs `up`.
// ---------------------------------------------------------------------------

// A derived `up` (the column follows another one) and a writer on `public`
// whose code does not know the column. One migration, started and completed:
// the writes land between the backfill reaching the row and the Complete.
func TestTrainCaseUnpinnedWriterAndDerivedUp(t *testing.T) {
	t.Parallel()

	for name, versionSchemas := range map[string]bool{"version schemas": true, "version schemas disabled": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts := []roll.Option{roll.WithLockTimeoutMs(500), roll.WithVersionSchema(versionSchemas)}
			testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public", opts, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				r2Base(t, mig, `CREATE TABLE w (id integer PRIMARY KEY, x integer);
					INSERT INTO w VALUES (1, 10), (2, 20)`)

				require.NoError(t, mig.Start(ctx, r2Add("01_add_a", "w", "a", "x + 1"), backfill.NewConfig()))

				require.NoError(t, r2ExecAs(t, db, "", `UPDATE w SET x = 50 WHERE id = 1`),
					"old code on `public` updates the column `up` reads")
				require.NoError(t, r2ExecAs(t, db, "public", `INSERT INTO w (id, x) VALUES (2, 70) ON CONFLICT (id) DO UPDATE SET x = EXCLUDED.x`),
					"old code on `public` upserts")
				require.NoError(t, r2ExecAs(t, db, "", `INSERT INTO w (id, x) VALUES (3, 30)`))

				require.NoError(t, mig.Complete(ctx))
				r2Want(t, db, 31, `SELECT a FROM public.w WHERE id = 3`, "insert")
				r2Want(t, db, 51, `SELECT a FROM public.w WHERE id = 1`, "update: a = x + 1 after the migration")
				r2Want(t, db, 71, `SELECT a FROM public.w WHERE id = 2`, "upsert: a = x + 1 after the migration")
			})
		})
	}
}

// The same writer when the column has a fast-path default that differs from
// `up`: its INSERT leaves the column to the default.
func TestTrainCaseUnpinnedInsertAndFastPathDefault(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		r2Base(t, mig, r2W)
		zero := "0"
		require.NoError(t, mig.Start(ctx, &migrations.Migration{Name: "01_add_b", Operations: migrations.Operations{
			&migrations.OpAddColumn{Table: "w", Up: "x * 2", Column: migrations.Column{Name: "b", Type: "integer", Nullable: false, Default: &zero}},
		}}, backfill.NewConfig()))
		require.NoError(t, r2ExecAs(t, db, "", `INSERT INTO w (id, x) VALUES (2, 20)`))
		require.NoError(t, mig.Complete(ctx))
		r2Want(t, db, 20, `SELECT b FROM public.w WHERE id = 1`, "backfilled")
		r2Want(t, db, 40, `SELECT b FROM public.w WHERE id = 2`, "inserted from `public` during the migration")
	})
}

// Mid-train: the new revision stores a value, then a session on `public`
// updates another column of the row. That session runs the previous
// revision's code as far as pgroll can tell, so `up` runs as it does for a
// pod pinned to the previous schema.
func TestTrainCaseUnpinnedUpdateOfANewRevisionRow(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		r2Base(t, mig, r2W)
		r2Train(t, mig, []*migrations.Migration{r2Add("01_add_a", "w", "a", "0"), r2Last()})

		require.NoError(t, r2ExecAs(t, db, r2New, `INSERT INTO w (id, x, a) VALUES (3, 30, 99)`))
		require.NoError(t, r2ExecAs(t, db, "", `UPDATE w SET x = 31 WHERE id = 3`),
			"a session on `public` touches the row")
		r2Want(t, db, 0, `SELECT a FROM public_09_last.w WHERE id = 3`, "the value `up` yields")
	})
}

// A session on `public` that names the column, on a row the new revision
// inserted: the trigger takes the row for one the backfill has not reached.
func TestTrainCaseUnpinnedWriteOfTheColumnOnANewRevisionRow(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		r2Base(t, mig, r2W)
		r2Train(t, mig, []*migrations.Migration{r2Add("01_add_a", "w", "a", "0"), r2Last()})

		require.NoError(t, r2ExecAs(t, db, r2New, `INSERT INTO w (id, x, a) VALUES (3, 30, 99)`))
		require.NoError(t, r2ExecAs(t, db, "", `UPDATE w SET a = 7 WHERE id = 3`),
			"a data fix from psql on `public`")
		r2Want(t, db, 7, `SELECT a FROM public_09_last.w WHERE id = 3`, "the value the session on `public` wrote")
	})
}

// ---------------------------------------------------------------------------
// Claim 3: the started name, at Start and in each replay.
// ---------------------------------------------------------------------------

// The name is re-added after an earlier operation of the train dropped or
// renamed the column that had it. The train is then reverted, or contracted.
func TestTrainCaseReAddedColumnName(t *testing.T) {
	t.Parallel()

	drop := &migrations.OpDropColumn{Table: "w", Column: "x", Down: "0"}
	rename := &migrations.OpRenameColumn{Table: "w", From: "x", To: "y"}
	add := func(up string) *migrations.OpAddColumn {
		return &migrations.OpAddColumn{Table: "w", Up: up, Column: migrations.Column{Name: "x", Type: "integer", Nullable: true}}
	}
	trains := map[string][]*migrations.Migration{
		"drop then add, two migrations": {
			{Name: "01_drop", Operations: migrations.Operations{drop}},
			{Name: "02_add", Operations: migrations.Operations{add("5")}},
			r2Last(),
		},
		"rename then add, two migrations": {
			{Name: "01_rename", Operations: migrations.Operations{rename}},
			{Name: "02_add", Operations: migrations.Operations{add("5")}},
			r2Last(),
		},
		"rename then add, one migration": {
			{Name: "01_swap", Operations: migrations.Operations{rename, add("5")}},
			r2Last(),
		},
	}
	for name, train := range trains {
		for _, end := range []string{"revert", "bounded revert", "rollback then revert", "contract"} {
			t.Run(name+"/"+end, func(t *testing.T) {
				t.Parallel()
				testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
					ctx := context.Background()
					r2Base(t, mig, r2W)
					r2Train(t, mig, train)
					require.NoError(t, r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x) VALUES (2, 20)`), "previous revision")
					require.NoError(t, r2ExecAs(t, db, r2New, `INSERT INTO w (id, x) VALUES (3, 30)`), "new revision")
					r2Want(t, db, 5, `SELECT x FROM public_09_last.w WHERE id = 2`, "the new column, filled for the previous revision")
					r2Want(t, db, 30, `SELECT x FROM public_09_last.w WHERE id = 3`)

					switch end {
					case "contract":
						require.NoError(t, r2Safely(func() error { return mig.Complete(ctx) }))
						r2Want(t, db, 5, `SELECT x FROM public.w WHERE id = 1`)
						r2Want(t, db, 30, `SELECT x FROM public.w WHERE id = 3`)
						assert.Zero(t, r2Leftovers(t, db, "public"))
						return
					case "revert":
						require.NoError(t, r2Safely(func() error { _, err := mig.Revert(ctx); return err }))
					case "bounded revert":
						first := train[0].Name
						require.NoError(t, r2Safely(func() error { _, err := mig.Revert(ctx, roll.WithRevertTo(first)); return err }))
						require.NoError(t, r2Safely(func() error { _, err := mig.Revert(ctx); return err }))
					case "rollback then revert":
						require.NoError(t, r2Safely(func() error { return mig.Rollback(ctx) }))
						require.NoError(t, r2Safely(func() error { _, err := mig.Revert(ctx); return err }))
					}
					r2Want(t, db, 10, `SELECT x FROM public.w WHERE id = 1`, "the original column and its value")
					r2Want(t, db, 20, `SELECT x FROM public.w WHERE id = 2`, "the previous revision's value")
					r2Want(t, db, 2, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'w'`,
						"the table has its two columns again")
					assert.Zero(t, r2Leftovers(t, db, "public"))
					require.NoError(t, r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x) VALUES (4, 40)`), "previous revision after the revert")
				})
			})
		}
	}
}

// ---------------------------------------------------------------------------
// Claim 5: trigger functions that outlive another migration's contraction.
// ---------------------------------------------------------------------------

// r2ContractWithAWindow runs the contraction while a session holds table z,
// which the migration named `blocked` waits for, and calls during once the
// migration named `drained` has drained.
func r2ContractWithAWindow(t *testing.T, mig *roll.Roll, db *sql.DB, schema, drained string, during func()) error {
	t.Helper()
	ctx := context.Background()
	blocker, err := db.Conn(ctx)
	require.NoError(t, err)
	defer blocker.Close()
	_, err = blocker.ExecContext(ctx, fmt.Sprintf(`BEGIN; SELECT count(*) FROM %s.z`, schema))
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- r2Safely(func() error { return mig.Complete(ctx) }) }()

	deadline := time.Now().Add(15 * time.Second)
	for {
		var isDrained bool
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT NOT complete_deferred FROM pgroll.migrations WHERE name = $1 AND schema = $2`, drained, schema).Scan(&isDrained))
		if isDrained {
			break
		}
		select {
		case err := <-done:
			_, _ = blocker.ExecContext(ctx, `ROLLBACK`)
			t.Fatalf("the contraction ended before %s drained: %v", drained, err)
		default:
		}
		if time.Now().After(deadline) {
			_, _ = blocker.ExecContext(ctx, `ROLLBACK`)
			<-done
			t.Fatalf("%s never drained", drained)
		}
		time.Sleep(50 * time.Millisecond)
	}
	during()
	_, err = blocker.ExecContext(ctx, `ROLLBACK`)
	require.NoError(t, err)
	return <-done
}

func r2BlockedOnZ(name string) *migrations.Migration {
	return &migrations.Migration{Name: name, Operations: migrations.Operations{
		&migrations.OpAlterColumn{
			Table: "z", Column: "c",
			Check: &migrations.CheckConstraint{Name: "c_pos", Constraint: "c > 0"},
			Up:    "c", Down: "c",
		},
	}}
}

// An earlier migration alters or renames a column; a later add_column's `up`
// reads it. The earlier one drains first and moves the column the
// add_column's trigger function reads, which the contraction has not
// reached.
func TestTrainCaseWritesBetweenDrainsWhenUpReadsAMovedColumn(t *testing.T) {
	t.Parallel()

	type shape struct {
		first *migrations.Migration
		up    string
		col   string
	}
	for name, s := range map[string]shape{
		"alter_column": {
			first: &migrations.Migration{Name: "01_first", Operations: migrations.Operations{&migrations.OpAlterColumn{
				Table: "w", Column: "x",
				Check: &migrations.CheckConstraint{Name: "x_pos", Constraint: "x > 0"},
				Up:    "x", Down: "x",
			}}},
			up: "x + 1", col: "x",
		},
		"rename_column": {
			first: &migrations.Migration{Name: "01_first", Operations: migrations.Operations{
				&migrations.OpRenameColumn{Table: "w", From: "x", To: "y"},
			}},
			up: "y + 1", col: "y",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
				[]roll.Option{roll.WithLockTimeoutMs(200)},
				func(mig *roll.Roll, db *sql.DB) {
					r2Base(t, mig, `CREATE TABLE w (id integer PRIMARY KEY, x integer);
						CREATE TABLE z (id integer PRIMARY KEY, c integer);
						INSERT INTO w VALUES (1, 10)`)
					r2Train(t, mig, []*migrations.Migration{
						s.first,
						r2BlockedOnZ("02_blocked"),
						r2Add("03_add_a", "w", "a", s.up),
						r2Last(),
					})
					insert := func(id int) error {
						return r2ExecAs(t, db, r2New, fmt.Sprintf(`INSERT INTO w (id, %s, a) VALUES (%d, 20, 7)`, s.col, id))
					}
					require.NoError(t, insert(2), "before the contraction")

					var windowErr error
					err := r2ContractWithAWindow(t, mig, db, "public", "01_first", func() { windowErr = insert(3) })
					require.NoError(t, err, "the contraction")
					assert.NoError(t, windowErr, "the new revision's write while the contraction waits between two drains")
				})
		})
	}
}

// A table is renamed by an earlier migration; a later add_column's trigger
// function declares its variables by the table's old name. The rename drains
// first.
func TestTrainCaseWritesBetweenDrainsAfterATableRename(t *testing.T) {
	t.Parallel()

	type shape struct{ schema, from, to string }
	for name, s := range map[string]shape{
		"plain":           {"public", "w", "w2"},
		"quoted names":    {"public", `Old "Tbl"`, `New.Tbl`},
		"non-public":      {"tenant_a", "w", "w2"},
		"prefix of other": {"public", "w", "ww"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, s.schema,
				[]roll.Option{roll.WithLockTimeoutMs(200)},
				func(mig *roll.Roll, db *sql.DB) {
					q := func(n string) string { return `"` + strings.ReplaceAll(n, `"`, `""`) + `"` }
					prev, next := s.schema+"_00_base", s.schema+"_09_last"
					r2Base(t, mig, fmt.Sprintf(`CREATE TABLE %[1]s (id integer PRIMARY KEY, x integer);
						CREATE TABLE z (id integer PRIMARY KEY, c integer);
						CREATE TABLE "wother" (id integer PRIMARY KEY, x integer);
						INSERT INTO %[1]s VALUES (1, 10)`, q(s.from)))
					r2Train(t, mig, []*migrations.Migration{
						{Name: "01_rename", Operations: migrations.Operations{&migrations.OpRenameTable{From: s.from, To: s.to}}},
						r2BlockedOnZ("02_blocked"),
						r2Add("03_add_a", s.to, "a", "x + 1"),
						r2Add("04_add_b", "wother", "b", "x + 1"),
						r2Last(),
					})
					require.NoError(t, r2ExecAs(t, db, prev, fmt.Sprintf(`INSERT INTO %s (id, x) VALUES (2, 20)`, q(s.from))), "previous revision")
					require.NoError(t, r2ExecAs(t, db, next, fmt.Sprintf(`INSERT INTO %s (id, x, a) VALUES (3, 30, 7)`, q(s.to))), "new revision")

					var windowErr, otherErr error
					err := r2ContractWithAWindow(t, mig, db, s.schema, "01_rename", func() {
						windowErr = r2ExecAs(t, db, next, fmt.Sprintf(`INSERT INTO %s (id, x, a) VALUES (4, 40, 7)`, q(s.to)))
						otherErr = r2ExecAs(t, db, next, `INSERT INTO wother (id, x, b) VALUES (4, 40, 7)`)
					})
					require.NoError(t, err, "the contraction")
					assert.NoError(t, windowErr, "the new revision's write to the renamed table between two drains")
					assert.NoError(t, otherErr, "and to the other table")
					r2Want(t, db, 21, fmt.Sprintf(`SELECT a FROM %s.%s WHERE id = 2`, q(s.schema), q(s.to)), "filled for the previous revision")
					assert.Zero(t, r2Leftovers(t, db, s.schema))
				})
		})
	}
}

// Two tables get a unique column of the same name in one train. The index a
// unique column builds is named for the column alone, so the second is
// refused while the first still holds the name.
func TestTrainCaseSameNamedUniqueColumnsOnTwoTables(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		r2Base(t, mig, `CREATE TABLE t1 (id integer PRIMARY KEY); CREATE TABLE t2 (id integer PRIMARY KEY);
			INSERT INTO t1 VALUES (1), (2); INSERT INTO t2 VALUES (1), (2)`)
		add := func(name, table string) *migrations.Migration {
			return &migrations.Migration{Name: name, Operations: migrations.Operations{&migrations.OpAddColumn{
				Table: table, Up: "id * 10",
				Column: migrations.Column{Name: "ext", Type: "integer", Nullable: true, Unique: true},
			}}}
		}
		err := r2TrainErr(mig, []*migrations.Migration{add("01_t1", "t1"), add("02_t2", "t2"), r2Last()})
		require.ErrorContains(t, err, "is still in use by table")
	})
}

// `up` names columns that need quoting, in mixed case and outside ASCII,
// and one whose name is the prefix of another.
func TestTrainCaseUpNamingOddIdentifiers(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		r2Base(t, mig, `CREATE TABLE w (id integer PRIMARY KEY, "userId" integer, "my col" text, "naïve" integer, usr integer, usr_id integer, "Größe" integer);
			INSERT INTO w VALUES (1, 1, 'ab', 3, 4, 5, 6)`)
		r2Train(t, mig, []*migrations.Migration{
			{Name: "01_add", Operations: migrations.Operations{&migrations.OpAddColumn{
				Table: "w", Up: `"userId" + char_length("my col") + "naïve" + USR_ID + "Größe"`,
				Column: migrations.Column{Name: "New Col", Type: "integer", Nullable: true},
			}}},
			{Name: "02_drop_usr", Operations: migrations.Operations{&migrations.OpDropColumn{Table: "w", Column: "usr", Down: "0"}}},
			r2Last(),
		})
		require.NoError(t, r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, "userId", "my col", "naïve", usr, usr_id, "Größe") VALUES (2, 10, 'abc', 30, 40, 50, 60)`), "previous revision")
		r2Want(t, db, 1+2+3+5+6, `SELECT "New Col" FROM public_09_last.w WHERE id = 1`)
		r2Want(t, db, 10+3+30+50+60, `SELECT "New Col" FROM public_09_last.w WHERE id = 2`)
		require.NoError(t, r2Safely(func() error { return mig.Complete(ctx) }))
		assert.Zero(t, r2Leftovers(t, db, "public"))
	})
}

// The re-added column is the train's last migration, and is rolled back.
func TestTrainCaseReAddedColumnNameAsTheLeafRolledBack(t *testing.T) {
	t.Parallel()

	leaf := &migrations.Migration{Name: "09_last", Operations: migrations.Operations{
		&migrations.OpAddColumn{Table: "w", Up: "5", Column: migrations.Column{Name: "x", Type: "integer", Nullable: false}},
	}}
	for name, first := range map[string]*migrations.Migration{
		"drop":   {Name: "01_drop", Operations: migrations.Operations{&migrations.OpDropColumn{Table: "w", Column: "x", Down: "0"}}},
		"rename": {Name: "01_rename", Operations: migrations.Operations{&migrations.OpRenameColumn{Table: "w", From: "x", To: "y"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				r2Base(t, mig, r2W)
				r2Train(t, mig, []*migrations.Migration{first, leaf})
				require.NoError(t, r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x) VALUES (2, 20)`), "previous revision")
				require.NoError(t, r2Safely(func() error { return mig.Rollback(ctx) }), "pgroll rollback of the leaf")
				r2Want(t, db, 10, `SELECT x FROM public.w WHERE id = 1`, "the original column and its value")
				require.NoError(t, r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x) VALUES (3, 30)`), "previous revision after the rollback")
				require.NoError(t, r2Safely(func() error { _, err := mig.Revert(ctx); return err }), "then the revert of the rest")
				r2Want(t, db, 30, `SELECT x FROM public.w WHERE id = 3`)
				assert.Zero(t, r2Leftovers(t, db, "public"))
			})
		})
	}
}

// One migration renames the table and adds a column with `up` to it, in
// either order; and one creates a table and adds a column to it.
func TestTrainCaseAddColumnBesideATableOperationInOneMigration(t *testing.T) {
	t.Parallel()

	rename := &migrations.OpRenameTable{From: "w", To: "w2"}
	add := func(table string) *migrations.OpAddColumn {
		return &migrations.OpAddColumn{Table: table, Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: false}}
	}
	type shape struct {
		ops   migrations.Operations
		table string
	}
	for name, s := range map[string]shape{
		"rename then add": {migrations.Operations{rename, add("w2")}, "w2"},
		"add then rename": {migrations.Operations{add("w"), rename}, "w2"},
	} {
		for _, end := range []string{"contract", "revert"} {
			t.Run(name+"/"+end, func(t *testing.T) {
				t.Parallel()
				testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
					ctx := context.Background()
					r2Base(t, mig, r2W)
					r2Train(t, mig, []*migrations.Migration{{Name: "01_both", Operations: s.ops}, r2Last()})
					require.NoError(t, r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x) VALUES (2, 20)`), "previous revision")
					require.NoError(t, r2ExecAs(t, db, r2New, `INSERT INTO w2 (id, x, a) VALUES (3, 30, 7)`), "new revision")
					r2Want(t, db, 21, `SELECT a FROM public_09_last.w2 WHERE id = 2`)
					if end == "revert" {
						require.NoError(t, r2Safely(func() error { _, err := mig.Revert(ctx); return err }))
						require.NoError(t, r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x) VALUES (4, 40)`), "previous revision after the revert")
						r2Want(t, db, 2, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'w'`)
					} else {
						require.NoError(t, r2Safely(func() error { return mig.Complete(ctx) }))
						r2Want(t, db, 21, `SELECT a FROM public.w2 WHERE id = 2`)
						require.NoError(t, r2ExecAs(t, db, r2New, `INSERT INTO w2 (id, x, a) VALUES (5, 50, 7)`), "after the contraction")
					}
					assert.Zero(t, r2Leftovers(t, db, "public"))
				})
			})
		}
	}
}

func TestTrainCaseCreateTableAndAddColumnInOneMigration(t *testing.T) {
	t.Parallel()
	for _, end := range []string{"contract", "revert"} {
		t.Run(end, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				r2Base(t, mig, r2W)
				r2Train(t, mig, []*migrations.Migration{
					{Name: "01_create", Operations: migrations.Operations{
						&migrations.OpCreateTable{Name: "t2", Columns: []migrations.Column{{Name: "id", Type: "integer", Pk: true}, {Name: "x", Type: "integer", Nullable: true}}},
						&migrations.OpAddColumn{Table: "t2", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
					}},
					{Name: "02_index", Operations: migrations.Operations{
						&migrations.OpCreateIndex{Name: "ix_t2_a", Table: "t2", Columns: []migrations.IndexField{{Column: "a"}}},
					}},
					r2Last(),
				})
				require.NoError(t, r2ExecAs(t, db, r2New, `INSERT INTO t2 (id, x, a) VALUES (1, 10, 7)`), "new revision")
				require.NoError(t, r2ExecAs(t, db, "", `INSERT INTO t2 (id, x) VALUES (2, 20)`), "a session on public")
				if end == "revert" {
					require.NoError(t, r2Safely(func() error { _, err := mig.Revert(ctx); return err }))
				} else {
					require.NoError(t, r2Safely(func() error { return mig.Complete(ctx) }))
					r2Want(t, db, 7, `SELECT a FROM public.t2 WHERE id = 1`)
				}
				assert.Zero(t, r2Leftovers(t, db, "public"))
			})
		})
	}
}

// A contracted train with an add_column is reverted by inversion.
func TestTrainCaseSealedRevertOfAnAddColumn(t *testing.T) {
	t.Parallel()
	for name, expandOnly := range map[string]bool{"one shot": false, "expand only": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				r2Base(t, mig, r2W)
				r2Train(t, mig, []*migrations.Migration{
					{Name: "01_add_a", Operations: migrations.Operations{
						&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: false}},
					}},
					{Name: "02_drop_x", Operations: migrations.Operations{&migrations.OpDropColumn{Table: "w", Column: "x", Down: "a - 1"}}},
					r2Last(),
				})
				require.NoError(t, mig.Complete(ctx))

				var opts []roll.SealedRevertOption
				if expandOnly {
					opts = append(opts, roll.WithExpandOnly())
				}
				require.NoError(t, r2Safely(func() error { _, err := mig.RevertSealed(ctx, "00_base", nil, opts...); return err }), "sealed revert")
				if expandOnly {
					require.NoError(t, r2ExecAs(t, db, r2New, `INSERT INTO w (id, a) VALUES (2, 21)`), "the current revision, while the fleet repins")
					require.NoError(t, r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x) VALUES (3, 30)`), "the restored revision")
					r2Want(t, db, 20, `SELECT x FROM public_00_base.w WHERE id = 2`, "x restored for the current revision's row")
					require.NoError(t, r2ExecAs(t, db, r2New, `UPDATE w SET a = 100 WHERE id = 2`), "the current revision updates its row")
					r2Want(t, db, 99, `SELECT x FROM public_00_base.w WHERE id = 2`, "x follows the update")
					require.NoError(t, r2Safely(func() error { return mig.Complete(ctx) }))
					_, err := mig.FinishPendingSealedRevert(ctx)
					require.NoError(t, err)
					r2Want(t, db, 99, `SELECT x FROM public.w WHERE id = 2`)
					r2Want(t, db, 30, `SELECT x FROM public.w WHERE id = 3`)
				}
				r2Want(t, db, 10, `SELECT x FROM public.w WHERE id = 1`)
				r2Want(t, db, 2, `SELECT count(*) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'w'`)
				assert.Zero(t, r2Leftovers(t, db, "public"))
			})
		})
	}
}

// ---------------------------------------------------------------------------
// One migration, several operations on the added column.
// ---------------------------------------------------------------------------

func TestTrainCaseAddColumnThenAnotherOperationOnItInOneMigration(t *testing.T) {
	t.Parallel()

	add := &migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}}
	type shape struct {
		ops  migrations.Operations
		name string // the column's final name
	}
	for name, s := range map[string]shape{
		"rename_column": {migrations.Operations{add, &migrations.OpRenameColumn{Table: "w", From: "a", To: "b"}}, "b"},
		"second add_column reads the first": {migrations.Operations{
			&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "zz", Type: "integer", Nullable: true}},
			&migrations.OpAddColumn{Table: "w", Up: "zz + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
		}, "a"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				r2Base(t, mig, r2W)
				require.NoError(t, r2TrainErr(mig, []*migrations.Migration{{Name: "01_multi", Operations: s.ops}, r2Last()}), "the expand")
				require.NoError(t, r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x) VALUES (2, 20)`), "previous revision")
				require.NoError(t, r2ExecAs(t, db, r2New, fmt.Sprintf(`INSERT INTO w (id, x, %s) VALUES (3, 30, 7)`, s.name)), "new revision")
				require.NoError(t, r2ExecAs(t, db, r2Prev, `UPDATE w SET x = 40 WHERE id = 2`), "previous revision updates")
				want1, want2 := 11, 41
				if s.name == "a" && len(s.ops) == 2 && name == "second add_column reads the first" {
					want1, want2 = 12, 42
				}
				sel := fmt.Sprintf(`SELECT %s FROM public_09_last.w WHERE id = `, s.name)
				r2Want(t, db, want1, sel+"1", "backfilled")
				r2Want(t, db, want2, sel+"2", "filled for the previous revision")
				r2Want(t, db, 7, sel+"3", "stored by the new revision")
				require.NoError(t, r2Safely(func() error { return mig.Complete(ctx) }), "the contraction")
				sel = fmt.Sprintf(`SELECT %s FROM public.w WHERE id = `, s.name)
				r2Want(t, db, want1, sel+"1", "backfilled, after the contraction")
				r2Want(t, db, want2, sel+"2", "filled for the previous revision, after the contraction")
				r2Want(t, db, 7, sel+"3", "stored by the new revision, after the contraction")
				assert.Zero(t, r2Leftovers(t, db, "public"))
			})
		})
	}
}

// ---------------------------------------------------------------------------
// Re-projecting the previous revision's schema while a train is part-way.
// ---------------------------------------------------------------------------

// A train stopped after its first migration. The fleet is still on the
// previous deployment's schema, that schema is lost, and the operator puts
// it back with `pgroll materialize --version <previous>`.
func TestTrainCaseMaterializeThePreviousSchemaOfAPartialTrain(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		r2Base(t, mig, `CREATE TABLE w (id integer PRIMARY KEY, x integer, y integer);
			INSERT INTO w VALUES (1, 10, 1)`)
		final := r2Last()
		first := &migrations.Migration{Name: "01_first", Operations: migrations.Operations{
			&migrations.OpDropColumn{Table: "w", Column: "y", Down: "0"},
			&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
		}}
		require.NoError(t, mig.Start(ctx, first, backfill.NewConfig(), r2MidOpts(mig, final)...))
		require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))

		_, err := db.ExecContext(ctx, `DROP SCHEMA public_00_base CASCADE`)
		require.NoError(t, err)
		sc, err := r2MaterializeSchema(ctx, mig, "00_base")
		require.NoError(t, err)
		require.NoError(t, mig.Materialize(ctx, "00_base", sc))

		assert.NoError(t, r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x, y) VALUES (2, 20, 2)`),
			"the previous revision writes the column the train drops at its contraction")
		r2Want(t, db, 0, `SELECT count(*) FROM information_schema.columns
			WHERE table_schema = 'public_00_base' AND table_name = 'w' AND column_name = 'a'`,
			"the previous revision does not see the train's new column")
	})
}

// A CHECK ties the new column to the column `up` derives it from.
func TestTrainCaseUnpinnedUpdateAndACheckOnTheDerivedColumn(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
		[]roll.Option{roll.WithLockTimeoutMs(500), roll.WithVersionSchema(false)},
		func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			r2Base(t, mig, r2W)
			require.NoError(t, mig.Start(ctx, &migrations.Migration{Name: "01_add_a", Operations: migrations.Operations{
				&migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{
					Name: "a", Type: "integer", Nullable: true,
					Check: &migrations.CheckConstraint{Name: "a_follows_x", Constraint: "a > x"},
				}},
			}}, backfill.NewConfig()))
			assert.NoError(t, r2ExecAs(t, db, "", `UPDATE w SET x = 50 WHERE id = 1`),
				"old code on `public` updates the source column while the migration runs")
			require.NoError(t, mig.Complete(ctx))
		})
}

// The same train on the host that runs `migrate --complete` with version
// schemas disabled: the rolling host accepts a name re-added after a drop.
func TestTrainCaseReAddedColumnNameOnTheNonRollingHost(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
		[]roll.Option{roll.WithLockTimeoutMs(500), roll.WithVersionSchema(false)},
		func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			r2Base(t, mig, r2W)
			err := r2TrainErr(mig, []*migrations.Migration{
				{Name: "01_drop", Operations: migrations.Operations{&migrations.OpDropColumn{Table: "w", Column: "x", Down: "0"}}},
				{Name: "02_add", Operations: migrations.Operations{
					&migrations.OpAddColumn{Table: "w", Up: "5", Column: migrations.Column{Name: "x", Type: "integer", Nullable: true}},
				}},
				r2Last(),
			})
			require.NoError(t, err, "the train, as `migrate --complete --use-version-schema=false` applies it")
			require.NoError(t, r2Safely(func() error { return mig.Complete(ctx) }))
		})
}

// The contraction gives up between the two drains (here: a lock it cannot
// get). Nothing is in progress any more; the table stays as the first drain
// left it until someone re-runs `pgroll complete` to the end.
func TestTrainCaseContractionStoppedBetweenDrainsWhenUpReadsARenamedColumn(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
		[]roll.Option{roll.WithLockTimeoutMs(100), roll.WithLockRetryTimeout(time.Second)},
		func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			r2Base(t, mig, `CREATE TABLE w (id integer PRIMARY KEY, x integer);
				CREATE TABLE z (id integer PRIMARY KEY, c integer);
				INSERT INTO w VALUES (1, 10)`)
			r2Train(t, mig, []*migrations.Migration{
				{Name: "01_rename", Operations: migrations.Operations{&migrations.OpRenameColumn{Table: "w", From: "x", To: "y"}}},
				r2BlockedOnZ("02_blocked"),
				r2Add("03_add_a", "w", "a", "y + 1"),
				r2Last(),
			})

			blocker, err := db.Conn(ctx)
			require.NoError(t, err)
			defer blocker.Close()
			_, err = blocker.ExecContext(ctx, `BEGIN; SELECT count(*) FROM public.z`)
			require.NoError(t, err)
			require.Error(t, r2Safely(func() error { return mig.Complete(ctx) }), "the contraction gives up on z")
			_, err = blocker.ExecContext(ctx, `ROLLBACK`)
			require.NoError(t, err)

			r2Want(t, db, 1, `SELECT count(*) FROM pgroll.migrations WHERE name = '01_rename' AND NOT complete_deferred`, "the rename drained")
			assert.NoError(t, r2ExecAs(t, db, r2New, `INSERT INTO w (id, y, a) VALUES (2, 20, 7)`),
				"the new revision's INSERT after the contraction stopped")
			assert.NoError(t, r2ExecAs(t, db, r2New, `UPDATE w SET y = 11 WHERE id = 1`),
				"the new revision's UPDATE after the contraction stopped")
		})
}
