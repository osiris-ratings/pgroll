// SPDX-License-Identifier: Apache-2.0

package roll_test

// Adversarial review of 8ca1af2. Builds with the base build too (6b2e5ea):
// it uses only the r2*/r3*/r4* helpers. Differential tests log SIG|/CAT|/ROW|
// lines that <scratch>/r6diff.sh compares between the two builds; the
// TestTrainWaiting* tests are assertions that pass on the base build.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
	"github.com/xataio/pgroll/pkg/state"
)

// r5Modes are the ways one add_column migration reaches its Complete.
func r5Modes() map[string]struct {
	versionSchemas bool
	apply          func(mig *roll.Roll, add *migrations.Migration) error
} {
	ctx := context.Background()
	type mode = struct {
		versionSchemas bool
		apply          func(mig *roll.Roll, add *migrations.Migration) error
	}
	finish := func(mig *roll.Roll, err error) error {
		if err != nil {
			return err
		}
		return r4Finish(mig)
	}
	return map[string]mode{
		"start complete": {true, func(mig *roll.Roll, add *migrations.Migration) error {
			if err := mig.Start(ctx, add, backfill.NewConfig()); err != nil {
				return err
			}
			return mig.Complete(ctx)
		}},
		"rolling train": {true, func(mig *roll.Roll, add *migrations.Migration) error {
			return finish(mig, r2TrainErr(mig, []*migrations.Migration{add, r2Last()}))
		}},
		"complete train": {true, func(mig *roll.Roll, add *migrations.Migration) error {
			return finish(mig, r3CompleteTrainErr(mig, []*migrations.Migration{add, r2Last()}))
		}},
		"no version schemas": {false, func(mig *roll.Roll, add *migrations.Migration) error {
			return finish(mig, r2TrainErr(mig, []*migrations.Migration{add, r2Last()}))
		}},
	}
}

func r5Unique(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`
		SELECT count(*) FROM pg_constraint con
		JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = ANY (con.conkey)
		WHERE con.conrelid = ('public.' || quote_ident($1))::regclass AND con.contype = 'u' AND a.attname = $2`, table, column).Scan(&n))
	return n > 0
}

// ---------------------------------------------------------------------------
// F1. Complete skips what it takes for settled by looking the pending object
// up under the name pgroll gave it. Postgres cut that name at 63 bytes.
// ---------------------------------------------------------------------------

const (
	// 49 bytes: `_pgroll_check_not_null_` + name passes 63.
	r5Long49 = "account_holder_secondary_verification_status_code"
	// 52 bytes: `_pgroll_uniq_` + name passes 63 too.
	r5Long52 = "account_holder_secondary_verification_status_code_v2"
)

func TestTrainWaitingLongColumnNameKeepsNotNullAndUnique(t *testing.T) {
	t.Parallel()
	shapes := map[string]migrations.Column{
		"notnull 49 bytes":        {Name: r5Long49, Type: "integer"},
		"unique 52 bytes":         {Name: r5Long52, Type: "integer", Nullable: true, Unique: true},
		"notnull unique 52 bytes": {Name: r5Long52, Type: "integer", Unique: true},
	}
	for sn, column := range shapes {
		for mn, mode := range r5Modes() {
			t.Run(sn+"/"+mn, func(t *testing.T) {
				t.Parallel()
				r4RunChain(t, "long "+sn+"|"+mn, mode.versionSchemas, r4Base, func(t *testing.T, mig *roll.Roll, db *sql.DB) string {
					add := r4One("01_add", &migrations.OpAddColumn{Table: "w", Up: "x + 100", Column: column})
					err := mode.apply(mig, add)
					assert.NoError(t, err)
					if !column.Nullable {
						assert.True(t, r4NotNull(t, db, "w", column.Name), "the column is NOT NULL")
						assert.Error(t, r2ExecAs(t, db, "public", fmt.Sprintf(`UPDATE w SET %q = NULL WHERE id = 1`, column.Name)))
					}
					if column.Unique {
						assert.True(t, r5Unique(t, db, "w", column.Name), "the column has its UNIQUE constraint")
					}
					assert.Zero(t, r2Leftovers(t, db, "public"), "pgroll artifacts left")
					return "apply=" + r3Err(err)
				})
			})
		}
	}
}

// The same lookup for a unique column whose name the catalog query that
// reads index names mangles: a double quote or a dot.
func TestTrainWaitingOddColumnNameKeepsUnique(t *testing.T) {
	t.Parallel()
	for sn, name := range map[string]string{"dot": "a.b", "quote": `a"b`, "upper space": "A b"} {
		for mn, mode := range r5Modes() {
			t.Run(sn+"/"+mn, func(t *testing.T) {
				t.Parallel()
				r4RunChain(t, "oddunique "+sn+"|"+mn, mode.versionSchemas, r4Base, func(t *testing.T, mig *roll.Roll, db *sql.DB) string {
					add := r4One("01_add", &migrations.OpAddColumn{Table: "w", Up: "x + 100", Column: migrations.Column{Name: name, Type: "integer", Nullable: true, Unique: true}})
					err := mode.apply(mig, add)
					assert.NoError(t, err)
					assert.True(t, r5Unique(t, db, "w", name), "the column has its UNIQUE constraint")
					assert.Zero(t, r2Leftovers(t, db, "public"), "pgroll artifacts left")
					return "apply=" + r3Err(err)
				})
			})
		}
	}
}

// ---------------------------------------------------------------------------
// F2. A waiting column's CHECK, renamed later in the train, is never
// validated: Complete takes a check it cannot find by name for settled.
// ---------------------------------------------------------------------------

func r5Validated(t *testing.T, db *sql.DB, constraint string) string {
	t.Helper()
	return r3Str(t, db, fmt.Sprintf(`SELECT COALESCE((SELECT convalidated::text FROM pg_constraint WHERE conrelid = 'public.w'::regclass AND conname = '%s'), 'absent')`, constraint))
}

func TestTrainWaitingRenamedCheckOfAWaitingColumnIsValidated(t *testing.T) {
	t.Parallel()
	renames := map[string]*migrations.Migration{
		"rename_constraint": r4One("02_rename", &migrations.OpRenameConstraint{Table: "w", From: "a_pos", To: "a_positive"}),
		"raw sql":           r2Raw("02_rename", `ALTER TABLE w RENAME CONSTRAINT a_pos TO a_positive`),
	}
	trains := map[string]func(mig *roll.Roll, train []*migrations.Migration) error{
		"migrate then complete": r2TrainErr,
		"migrate --complete":    r3CompleteTrainErr,
	}
	for rn, rename := range renames {
		for tn, apply := range trains {
			t.Run(rn+"/"+tn, func(t *testing.T) {
				t.Parallel()
				r4RunChain(t, "renamed check "+rn+"|"+tn, true, r4Base, func(t *testing.T, mig *roll.Roll, db *sql.DB) string {
					err := apply(mig, []*migrations.Migration{r4Add("01_add", r4AddShapes()["check"]), rename, r2Last()})
					assert.NoError(t, err)
					err = r4Finish(mig)
					assert.NoError(t, err)
					assert.Equal(t, "true", r5Validated(t, db, "a_positive"), "the renamed check is validated")
					return "finish=" + r3Err(err)
				})
			})
		}
	}
}

// ---------------------------------------------------------------------------
// F3. A later raw SQL migration of the train sets the waiting column NOT
// NULL or changes its type, and a later typed operation copies the column.
// The replayed add_column takes default and comment from the catalog, not
// nullability or type.
// ---------------------------------------------------------------------------

func TestTrainWaitingRawSQLNullabilityOfAWaitingColumnSurvivesALaterCopy(t *testing.T) {
	t.Parallel()
	bigint := "bigint"
	laters := map[string]*migrations.Migration{
		"alter type": r4One("03_later", &migrations.OpAlterColumn{Table: "w", Column: "a", Type: &bigint, Up: "a", Down: "a"}),
		"alter check": r4One("03_later", &migrations.OpAlterColumn{
			Table: "w", Column: "a", Up: "a", Down: "a",
			Check: &migrations.CheckConstraint{Name: "a_small", Constraint: "a < 100000"},
		}),
		"create_constraint check a": r4One("03_later", &migrations.OpCreateConstraint{
			Name: "a_small", Table: "w", Type: "check", Columns: []string{"a"}, Check: r4Ptr("a < 100000"),
			Up: map[string]string{"a": "a"}, Down: map[string]string{"a": "a"},
		}),
	}
	for ln, later := range laters {
		t.Run(ln, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				r2Base(t, mig, r4Base)
				require.NoError(t, r2TrainErr(mig, []*migrations.Migration{
					r4Add("01_add", r4AddShapes()["plain"]),
					r2Raw("02_raw", `ALTER TABLE w ALTER COLUMN a SET NOT NULL`),
					later,
					r2Last(),
				}))
				require.NoError(t, r4Finish(mig))
				assert.True(t, r4NotNull(t, db, "w", "a"), "a is NOT NULL after a train that set it NOT NULL")
			})
		})
	}
}

// ---------------------------------------------------------------------------
// F4. An add_column without `up` starts under its temporary name, which
// Postgres cuts at 63 bytes for a column name over 42. Complete looks the
// column up by the uncut name.
// ---------------------------------------------------------------------------

func TestTrainWaitingLongColumnNameWithoutUpCompletes(t *testing.T) {
	t.Parallel()
	shapes := map[string]migrations.Operations{
		"nullable no up": {&migrations.OpAddColumn{Table: "w", Column: migrations.Column{Name: r5Long49, Type: "integer", Nullable: true}}},
		"unique no up":   {&migrations.OpAddColumn{Table: "w", Column: migrations.Column{Name: r5Long49, Type: "integer", Nullable: true, Unique: true}}},
	}
	for sn, ops := range shapes {
		for mn, mode := range r5Modes() {
			t.Run(sn+"/"+mn, func(t *testing.T) {
				t.Parallel()
				r4RunChain(t, "longnoup "+sn+"|"+mn, mode.versionSchemas, r4Base, func(t *testing.T, mig *roll.Roll, db *sql.DB) string {
					err := mode.apply(mig, r4One("01_add", ops...))
					assert.NoError(t, err)
					assert.Equal(t, "1", r3Str(t, db, fmt.Sprintf(`SELECT count(*)::text FROM pg_attribute WHERE attrelid = 'public.w'::regclass AND attname = '%s'`, r5Long49)), "the column exists under its own name")
					assert.Zero(t, r2Leftovers(t, db, "public"), "pgroll artifacts left")
					return "apply=" + r3Err(err)
				})
			})
		}
	}
}

// ---------------------------------------------------------------------------
// F5. One migration adds a filling column and a column with a volatile
// default (`up` repeats it). The migration waits for the contraction, and
// the second column gets its default only then. The new revision's view is
// projected from a replay that now takes the default from the catalog.
// ---------------------------------------------------------------------------

func TestTrainWaitingVolatileDefaultSiblingKeepsItsDefaultInTheNewRevision(t *testing.T) {
	t.Parallel()
	uuid := "gen_random_uuid()"
	for name, nullable := range map[string]bool{"notnull": false, "nullable": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r4RunChain(t, "volatile sibling "+name+"|contract", true, r4Base, func(t *testing.T, mig *roll.Roll, db *sql.DB) string {
				err := r2TrainErr(mig, []*migrations.Migration{
					r4One(
						"01_add",
						&migrations.OpAddColumn{Table: "w", Up: "x + 100", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
						&migrations.OpAddColumn{Table: "w", Up: uuid, Column: migrations.Column{Name: "u", Type: "uuid", Nullable: nullable, Default: &uuid}},
					),
					r2Last(),
				})
				require.NoError(t, err)
				out := "viewDefault=" + r3Str(t, db, `SELECT column_default FROM information_schema.columns WHERE table_schema = 'public_09_last' AND table_name = 'w' AND column_name = 'u'`)
				ins := r2ExecAs(t, db, r2New, `INSERT INTO w (id, x, a) VALUES (4, 40, 99)`)
				assert.NoError(t, ins, "the new revision inserts without the column")
				assert.Equal(t, "false", r3Str(t, db, `SELECT (u IS NULL)::text FROM public.w WHERE id = 4`), "the row the new revision inserted has the default")
				out += " newIns=" + r3Err(ins) + " prevIns=" + r3Err(r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x) VALUES (3, 30)`))
				err = r4Finish(mig)
				assert.NoError(t, err)
				assert.Equal(t, "0", r3Str(t, db, `SELECT count(*)::text FROM public.w WHERE u IS NULL`), "rows without the default")
				return out + " finish=" + r3Err(err)
			})
		})
	}
}

// A probe, not a regression: the base build cannot run this train at all. A
// column re-added NOT NULL under a name an earlier migration of the train
// renames away starts under its temporary name; a later alter then copies
// its pending NOT NULL check twice.
func TestTrainSweepProbeReAddedNotNullColumnThenAlter(t *testing.T) {
	skipUnlessTrainMatrix(t)
	skipUnlessTrainMatrix(t)
	t.Parallel()
	bigint := "bigint"
	base := `CREATE TABLE p (id integer PRIMARY KEY); INSERT INTO p VALUES (1), (2), (99);
		CREATE TABLE w (id integer PRIMARY KEY, x integer, c integer); INSERT INTO w VALUES (1, 10, 1), (2, 20, 2)`
	addC := r4One("02_add", &migrations.OpAddColumn{Table: "w", Up: "x + 100", Column: migrations.Column{Name: "c", Type: "integer"}})
	alterC := r4One("03_alter", &migrations.OpAlterColumn{Table: "w", Column: "c", Type: &bigint, Up: "c", Down: "c"})
	ends := []string{"contract", "contract quiet", "revert", "revert to 2 then contract"}
	r4RunTrains(t, map[string]r4Train{
		"rename c d; add c notnull; alter c type": {
			base: base, ends: ends, newIns: `INSERT INTO w (id, x, d, c) VALUES (4, 40, 4, 99)`,
			migs: r4Seq(r4One("01_rename", &migrations.OpRenameColumn{Table: "w", From: "c", To: "d"}), addC, alterC),
		},
		"drop c; add c notnull; alter c type": {
			base: base, ends: ends, newIns: `INSERT INTO w (id, x, c) VALUES (4, 40, 99)`,
			migs: r4Seq(r4One("01_drop", &migrations.OpDropColumn{Table: "w", Column: "c", Down: "x"}), addC, alterC),
		},
	})
}

// ---------------------------------------------------------------------------
// Class B of the chain matrix: the same chains with `up` and `down`
// expressions whose values the foreign key accepts.
// ---------------------------------------------------------------------------

func TestTrainSweepForeignKeyChainsWithValidExpressions(t *testing.T) {
	skipUnlessTrainMatrix(t)
	skipUnlessTrainMatrix(t)
	t.Parallel()
	shapes := r4AddShapes()
	bigint := "bigint"
	insNoA := `INSERT INTO w (id, x) VALUES (4, 40)`
	insA2 := `INSERT INTO w (id, x, a) VALUES (4, 40, 2)`
	fkAdd := r4Add("01_add", shapes["fk"]) // up is "1"
	add1 := func(shape string) *migrations.Migration {
		return r4One("01_add", &migrations.OpAddColumn{Table: "w", Up: "1", Column: shapes[shape]})
	}
	fk := func(up string) *migrations.Migration {
		return r4One("02_fk", &migrations.OpCreateConstraint{
			Name: "fk_a2", Table: "w", Type: "foreign_key", Columns: []string{"a"},
			References: &migrations.TableForeignKeyReference{Table: "p", Columns: []string{"id"}},
			Up:         map[string]string{"a": up}, Down: map[string]string{"a": "a"},
		})
	}
	ends3 := []string{"contract", "revert", "rollback revert", "revert to 1 then contract", "revert to 1 then revert", "revert to 2 then contract", "revert to 2 then revert"}
	ends2 := []string{"contract", "revert", "rollback revert", "revert to 1 then contract", "revert to 1 then revert"}
	r4RunTrains(t, map[string]r4Train{
		"fkvalid fk + drop column": {ends: ends2, newIns: insNoA, migs: r4Seq(fkAdd,
			r4One("02_drop", &migrations.OpDropColumn{Table: "w", Column: "a", Down: "1"}))},
		"fkvalid fk + alter type; drop column": {ends: ends3, newIns: insNoA, migs: r4Seq(fkAdd,
			r4One("02_alter", &migrations.OpAlterColumn{Table: "w", Column: "a", Type: &bigint, Up: "a", Down: "a"}),
			r4One("03_drop", &migrations.OpDropColumn{Table: "w", Column: "a", Down: "1"}))},
		"fkvalid fk + rename; drop": {ends: ends3, newIns: insNoA, migs: r4Seq(fkAdd,
			r4One("02_rename", &migrations.OpRenameColumn{Table: "w", From: "a", To: "b"}),
			r4One("03_drop", &migrations.OpDropColumn{Table: "w", Column: "b", Down: "1"}))},
		"fkvalid plain + constraint fk":      {ends: ends2, newIns: insA2, migs: r4Seq(add1("plain"), fk("a"))},
		"fkvalid notnull + constraint fk":    {ends: ends2, newIns: insA2, migs: r4Seq(add1("notnull"), fk("a"))},
		"fkvalid plain + constraint fk up 1": {ends: ends2, newIns: insA2, migs: r4Seq(add1("plain"), fk("1"))},
		// The original expressions, for the same run.
		"fkorig fk + drop column": {ends: ends2, newIns: insNoA, migs: r4Seq(fkAdd,
			r4One("02_drop", &migrations.OpDropColumn{Table: "w", Column: "a", Down: "x + 100"}))},
		"fkorig plain + constraint fk": {ends: ends2, newIns: insA2, migs: r4Seq(r4Add("01_add", shapes["plain"]), fk("1"))},
	})
}

// ---------------------------------------------------------------------------
// Shapes the chain matrix does not cover.
// ---------------------------------------------------------------------------

// Two or three waiting migrations on different columns of one table, then a
// raw SQL data migration.
func TestTrainSweepWaitingColumnsThenRawData(t *testing.T) {
	skipUnlessTrainMatrix(t)
	skipUnlessTrainMatrix(t)
	t.Parallel()
	shapes := r4AddShapes()
	addB := func(name, up string, c migrations.Column) *migrations.Migration {
		c.Name = "b"
		return r4One(name, &migrations.OpAddColumn{Table: "w", Up: up, Column: c})
	}
	addC := r4One("03_add", &migrations.OpAddColumn{Table: "w", Up: "a + b", Column: migrations.Column{Name: "c", Type: "integer", Nullable: true}})
	insAB := `INSERT INTO w (id, x, a, b) VALUES (4, 40, 99, 98)`
	trains := map[string]r4Train{}
	raws := map[string]string{
		"update x":          `UPDATE w SET x = x + 1`,
		"update a":          `UPDATE w SET a = 5 WHERE id = 1`,
		"update b from a":   `UPDATE w SET b = a + 1`,
		"update a null":     `UPDATE w SET a = NULL WHERE id = 2`,
		"insert":            `INSERT INTO w (id, x) VALUES (7, 70)`,
		"insert a":          `INSERT INTO w (id, x, a) VALUES (7, 70, 5)`,
		"insert a b":        `INSERT INTO w (id, x, a, b) VALUES (7, 70, 5, 6)`,
		"upsert":            `INSERT INTO w (id, x, a) VALUES (1, 12, 5) ON CONFLICT (id) DO UPDATE SET x = EXCLUDED.x`,
		"upsert set a":      `INSERT INTO w (id, x, a) VALUES (1, 12, 5) ON CONFLICT (id) DO UPDATE SET a = EXCLUDED.a`,
		"delete":            `DELETE FROM w WHERE id = 2`,
		"insert select":     `INSERT INTO w (id, x, a, b) SELECT id + 10, x, a, b FROM w`,
		"add column c":      `ALTER TABLE w ADD COLUMN c integer DEFAULT 3`,
		"index on a b":      `CREATE INDEX ix_ab ON w (a, b)`,
		"set default a":     `ALTER TABLE w ALTER COLUMN a SET DEFAULT 5`,
		"check a b":         `ALTER TABLE w ADD CONSTRAINT ab_sum CHECK (a + b > 0)`,
		"unique a b":        `ALTER TABLE w ADD CONSTRAINT ab_uq UNIQUE (a, b)`,
		"view on a":         `CREATE VIEW w_view AS SELECT id, a, b FROM w`,
		"update in a block": `DO $$ BEGIN UPDATE w SET a = a + 1; UPDATE w SET b = b + 1; END $$`,
	}
	for rn, raw := range raws {
		trains["plain a; plain b reading a; raw "+rn] = r4Train{newIns: insAB, migs: r4Seq(
			r4Add("01_add", shapes["plain"]), addB("02_add", "a * 2", shapes["plain"]), r2Raw("03_raw", raw),
		)}
	}
	for _, rn := range []string{"update x", "update a", "insert", "insert a b", "upsert", "insert select"} {
		trains["notnull a; nndefault b; raw "+rn] = r4Train{newIns: insAB, migs: r4Seq(
			r4Add("01_add", shapes["notnull"]), addB("02_add", "x + 200", shapes["nndefault"]), r2Raw("03_raw", raws[rn]),
		)}
		trains["unique a; check b; raw "+rn] = r4Train{newIns: insAB, migs: r4Seq(
			r4Add("01_add", shapes["unique"]), addB("02_add", "x + 200", migrations.Column{Type: "integer", Nullable: true, Check: &migrations.CheckConstraint{Name: "b_pos", Constraint: "b > 0"}}), r2Raw("03_raw", raws[rn]),
		)}
	}
	trains["a; b; c reading both; raw update x; raw insert"] = r4Train{
		newIns: `INSERT INTO w (id, x, a, b, c) VALUES (4, 40, 99, 98, 97)`,
		migs: r4Seq(r4Add("01_add", shapes["plain"]), addB("02_add", "x + 200", shapes["plain"]), addC,
			r2Raw("04_raw", raws["update x"]), r2Raw("05_raw", raws["insert"])),
	}
	trains["a; raw update a; b reading a; raw insert"] = r4Train{newIns: insAB, migs: r4Seq(
		r4Add("01_add", shapes["plain"]), r2Raw("02_raw", `UPDATE w SET a = a + 1`), addB("03_add", "a * 2", shapes["plain"]), r2Raw("04_raw", raws["insert"]),
	)}
	r4RunTrains(t, trains)
}

// A waiting add_column, then a later migration's index with a predicate or
// an expression naming the column, a later index over two waiting columns,
// and the ways back.
func TestTrainSweepWaitingColumnThenIndexes(t *testing.T) {
	skipUnlessTrainMatrix(t)
	skipUnlessTrainMatrix(t)
	t.Parallel()
	shapes := r4AddShapes()
	ix := func(name string, o migrations.OpCreateIndex) *migrations.Migration {
		o.Table = "w"
		return r4One(name, &o)
	}
	ends := []string{"contract", "contract quiet", "revert", "rollback revert", "complete train", "one by one", "sealed one shot", "sealed expand only", "revert to 1 then contract", "revert to 1 then revert", "revert to 2 then contract"}
	bigint := "bigint"
	trains := map[string]r4Train{}
	for _, sn := range []string{"plain", "notnull", "unique", "everything"} {
		trains[sn+"; partial index"] = r4Train{ends: ends, migs: r4Seq(r4Add("01_add", shapes[sn]),
			ix("02_index", migrations.OpCreateIndex{Name: "ix_a", Predicate: "a > 0", Columns: []migrations.IndexField{{Column: "x"}}}))}
		trains[sn+"; partial index on a"] = r4Train{ends: ends, migs: r4Seq(r4Add("01_add", shapes[sn]),
			ix("02_index", migrations.OpCreateIndex{Name: "ix_a", Predicate: "a > 0 AND x > 0", Columns: []migrations.IndexField{{Column: "a"}, {Column: "x"}}}))}
		trains[sn+"; unique index a x"] = r4Train{ends: ends, migs: r4Seq(r4Add("01_add", shapes[sn]),
			ix("02_index", migrations.OpCreateIndex{Name: "ix_a", Unique: true, Columns: []migrations.IndexField{{Column: "a"}, {Column: "x"}}}))}
		trains[sn+"; raw expression index"] = r4Train{ends: ends, migs: r4Seq(r4Add("01_add", shapes[sn]),
			r2Raw("02_index", `CREATE INDEX ix_a ON w ((a + x)) WHERE a IS NOT NULL`))}
		trains[sn+"; partial index; alter type"] = r4Train{ends: ends, migs: r4Seq(r4Add("01_add", shapes[sn]),
			ix("02_index", migrations.OpCreateIndex{Name: "ix_a", Predicate: "a > 0", Columns: []migrations.IndexField{{Column: "a"}}}),
			r4One("03_alter", &migrations.OpAlterColumn{Table: "w", Column: "a", Type: &bigint, Up: "a", Down: "a"}))}
		trains[sn+"; partial index; rename"] = r4Train{ends: ends, newIns: `INSERT INTO w (id, x, b) VALUES (4, 40, 99)`, migs: r4Seq(r4Add("01_add", shapes[sn]),
			ix("02_index", migrations.OpCreateIndex{Name: "ix_a", Predicate: "a > 0", Columns: []migrations.IndexField{{Column: "a"}}}),
			r4One("03_rename", &migrations.OpRenameColumn{Table: "w", From: "a", To: "b"}))}
		trains[sn+"; partial index; drop column"] = r4Train{ends: ends, newIns: `INSERT INTO w (id, x) VALUES (4, 40)`, migs: r4Seq(r4Add("01_add", shapes[sn]),
			ix("02_index", migrations.OpCreateIndex{Name: "ix_a", Predicate: "a > 0", Columns: []migrations.IndexField{{Column: "x"}}}),
			r4One("03_drop", &migrations.OpDropColumn{Table: "w", Column: "a", Down: "x + 100"}))}
	}
	r4RunTrains(t, trains)
}

// A waiting add_column on a partitioned table, then partitions created,
// attached and detached by a later raw SQL migration of the train.
func TestTrainSweepPartitionedTable(t *testing.T) {
	skipUnlessTrainMatrix(t)
	skipUnlessTrainMatrix(t)
	t.Parallel()
	shapes := r4AddShapes()
	base := `CREATE TABLE p (id integer PRIMARY KEY); INSERT INTO p VALUES (1), (2), (99);
		CREATE TABLE w (id integer, x integer, PRIMARY KEY (id)) PARTITION BY RANGE (id);
		CREATE TABLE w_1 PARTITION OF w FOR VALUES FROM (0) TO (100);
		CREATE TABLE w_2 PARTITION OF w FOR VALUES FROM (100) TO (200);
		INSERT INTO w VALUES (1, 10), (2, 20), (101, 110)`
	raws := map[string]string{
		"none":                  `SELECT 1`,
		"create partition":      `CREATE TABLE w_3 PARTITION OF w FOR VALUES FROM (200) TO (300); INSERT INTO w (id, x) VALUES (201, 210)`,
		"attach like":           `CREATE TABLE w_3 (LIKE w INCLUDING ALL); ALTER TABLE w ATTACH PARTITION w_3 FOR VALUES FROM (200) TO (300); INSERT INTO w (id, x) VALUES (201, 210)`,
		"attach hand written":   `CREATE TABLE w_3 (id integer PRIMARY KEY, x integer, a integer); ALTER TABLE w ATTACH PARTITION w_3 FOR VALUES FROM (200) TO (300)`,
		"detach":                `ALTER TABLE w DETACH PARTITION w_2`,
		"detach attach":         `ALTER TABLE w DETACH PARTITION w_2; ALTER TABLE w ATTACH PARTITION w_2 FOR VALUES FROM (100) TO (200)`,
		"detach drop":           `ALTER TABLE w DETACH PARTITION w_2; DROP TABLE w_2`,
		"move row to partition": `UPDATE w SET id = 150 WHERE id = 2`,
	}
	ends := []string{"contract", "contract quiet", "revert", "complete train", "one by one", "sealed one shot", "revert to 1 then contract"}
	trains := map[string]r4Train{}
	for _, sn := range []string{"plain", "notnull", "check", "nndefault"} {
		for rn, raw := range raws {
			trains["partitioned "+sn+"; raw "+rn] = r4Train{base: base, ends: ends, migs: r4Seq(r4Add("01_add", shapes[sn]), r2Raw("02_raw", raw))}
		}
	}
	r4RunTrains(t, trains)
}

// r5SchemaCatalog is r4Catalog for a schema other than `public`.
func r5SchemaCatalog(t *testing.T, db *sql.DB, schema string) string {
	t.Helper()
	return r3Str(t, db, fmt.Sprintf(`
	SELECT COALESCE(string_agg(line, ' ;; ' ORDER BY line), '') FROM (
		SELECT 'col ' || c.relname || ' ' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod)
			|| CASE WHEN a.attnotnull THEN ' NOT NULL' ELSE '' END
			|| COALESCE(' DEFAULT ' || pg_get_expr(d.adbin, d.adrelid), '') AS line
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE n.nspname = %[1]s AND c.relkind IN ('r', 'p') AND a.attnum > 0 AND NOT a.attisdropped
		UNION ALL
		SELECT 'con ' || c.relname || '.' || con.conname || ' ' || pg_get_constraintdef(con.oid) || CASE WHEN con.convalidated THEN '' ELSE ' !NOTVALID' END
		FROM pg_constraint con JOIN pg_class c ON c.oid = con.conrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = %[1]s
		UNION ALL
		SELECT 'idx ' || pg_get_indexdef(i.indexrelid)
		FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = %[1]s
		UNION ALL
		SELECT 'trg ' || c.relname || ' -> ' || p.proname
		FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace JOIN pg_proc p ON p.oid = t.tgfoid
		WHERE n.nspname = %[1]s AND NOT t.tgisinternal
		UNION ALL
		SELECT 'fn ' || p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = %[1]s
	) s`, "'"+strings.ReplaceAll(schema, "'", "''")+"'"))
}

// r5WithMigratorInQuotedSchema is testutils' migrator-in-schema helper for a
// schema whose name needs quoting.
func r5WithMigratorInQuotedSchema(t *testing.T, schema string, fn func(mig *roll.Roll, db *sql.DB)) {
	t.Helper()
	testutils.WithConnectionToContainer(t, func(db *sql.DB, connStr string) {
		ctx := context.Background()
		st, err := state.New(ctx, connStr, "pgroll")
		require.NoError(t, err)
		require.NoError(t, st.Init(ctx))
		_, err = db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %q", schema))
		require.NoError(t, err)
		mig, err := roll.New(ctx, connStr, schema, st, r3Opts(true)...)
		require.NoError(t, err)
		t.Cleanup(func() { _ = mig.Close() })
		fn(mig, db)
	})
}

// Trains over a schema other than `public`, one of them with a name that
// needs quoting.
func TestTrainSweepNonPublicSchema(t *testing.T) {
	skipUnlessTrainMatrix(t)
	t.Parallel()
	ctx := context.Background()
	bigint := "bigint"
	// A name that needs quoting cannot be tried: roll.New puts the schema in
	// the connection string unquoted, on this build and the base build.
	for _, schema := range []string{"tenant"} {
		q := fmt.Sprintf("%q", schema)
		prev, next := fmt.Sprintf("%q", schema+"_00_base"), fmt.Sprintf("%q", schema+"_09_last")
		trains := map[string][]*migrations.Migration{
			"add notnull unique; alter type; rename x": {
				r4One("01_add", &migrations.OpAddColumn{Table: "w", Up: "x + 100", Column: migrations.Column{Name: "a", Type: "integer", Unique: true}}),
				r4One("02_alter", &migrations.OpAlterColumn{Table: "w", Column: "a", Type: &bigint, Up: "a", Down: "a"}),
				r4One("03_rename", &migrations.OpRenameColumn{Table: "w", From: "x", To: "y"}),
			},
			"add; rename table; add reading a": {
				r4One("01_add", &migrations.OpAddColumn{Table: "w", Up: "x + 100", Column: migrations.Column{Name: "a", Type: "integer"}}),
				r4One("02_rename", &migrations.OpRenameTable{From: "w", To: "w2"}),
				r4One("03_add", &migrations.OpAddColumn{Table: "w2", Up: "a * 2", Column: migrations.Column{Name: "b", Type: "integer", Nullable: true}}),
			},
			"add; constraint check a; drop column a": {
				r4One("01_add", &migrations.OpAddColumn{Table: "w", Up: "x + 100", Column: migrations.Column{Name: "a", Type: "integer"}}),
				r4One("02_ck", &migrations.OpCreateConstraint{
					Name: "a_small", Table: "w", Type: "check", Columns: []string{"a"}, Check: r4Ptr("a < 100000"),
					Up: map[string]string{"a": "a"}, Down: map[string]string{"a": "a"},
				}),
				r4One("03_drop", &migrations.OpDropColumn{Table: "w", Column: "a", Down: "x + 100"}),
			},
		}
		ends := map[string]func(mig *roll.Roll) string{
			"contract": func(mig *roll.Roll) string { return r4Step("complete", func() error { return r4Finish(mig) }) },
			"revert": func(mig *roll.Roll) string {
				return r4Step("revert", func() error { _, err := mig.Revert(ctx); return err })
			},
			"revert to 1 then contract": func(mig *roll.Roll) string {
				return r4Step("revertTo", func() error { _, err := mig.Revert(ctx, roll.WithRevertTo("01_add")); return err }) + " " +
					r4Step("complete", func() error { return r4Finish(mig) })
			},
			"sealed one shot": func(mig *roll.Roll) string {
				return r4Step("complete", func() error { return r4Finish(mig) }) + " " +
					r4Step("sealed", func() error { _, err := mig.RevertSealed(ctx, "00_base", nil); return err })
			},
		}
		for tn, train := range trains {
			for en, end := range ends {
				t.Run(schema+"/"+tn+"/"+en, func(t *testing.T) {
					t.Parallel()
					r5WithMigratorInQuotedSchema(t, schema, func(mig *roll.Roll, db *sql.DB) {
						id := "schema " + schema + " " + tn + "|" + en
						require.NoError(t, mig.Start(ctx, r2Raw("00_base", fmt.Sprintf(`CREATE TABLE %[1]s.w (id integer PRIMARY KEY, x integer); INSERT INTO %[1]s.w VALUES (1, 10), (2, 20)`, q)), backfill.NewConfig()))
						require.NoError(t, mig.Complete(ctx))
						out := r4Step("train", func() error { return r2TrainErr(mig, append(append([]*migrations.Migration{}, train...), r2Last())) })
						out += " prevIns=" + r3Err(r2ExecAs(t, db, prev, `INSERT INTO w (id, x) VALUES (3, 30)`))
						out += " prevUpd=" + r3Err(r2ExecAs(t, db, prev, `UPDATE w SET x = 11 WHERE id = 1`))
						out += " newUpd=" + r3Err(r2ExecAs(t, db, next, `UPDATE w SET id = id WHERE id = 2`))
						out += " newUpd2=" + r3Err(r2ExecAs(t, db, next, `UPDATE w2 SET id = id WHERE id = 2`))
						out += " mid{" + r3TryStr(db, fmt.Sprintf(`SELECT string_agg(to_jsonb(t)::text, ' ' ORDER BY id) FROM %s.w t`, q)) + "}"
						out += " " + end(mig)
						t.Logf("SIG|%s|%s", id, out)
						t.Logf("CAT|%s|%s", id, r5SchemaCatalog(t, db, schema))
						rows := r3TryStr(db, fmt.Sprintf(`SELECT string_agg(to_jsonb(t)::text, ' ' ORDER BY id) FROM %s.w t`, q))
						if strings.HasPrefix(rows, "ERR") {
							rows = r3TryStr(db, fmt.Sprintf(`SELECT string_agg(to_jsonb(t)::text, ' ' ORDER BY id) FROM %s.w2 t`, q))
						}
						t.Logf("ROW|%s|%s", id, rows)
					})
				})
			}
		}
	}
}

// ---------------------------------------------------------------------------
// A contraction interrupted at each DDL statement, then retried: trains with
// a waiting add_column followed by alter, rename, create_constraint and
// drop_column.
// ---------------------------------------------------------------------------

func r5InterruptTrains() map[string]r4Train {
	shapes := r4AddShapes()
	bigint, f, tr := "bigint", false, true
	alter := func(name string, o migrations.OpAlterColumn) *migrations.Migration {
		o.Table, o.Column = "w", "a"
		if o.Up == "" {
			o.Up = "a"
		}
		o.Down = "a"
		return r4One(name, &o)
	}
	ck := func(name string) *migrations.Migration {
		return r4One(name, &migrations.OpCreateConstraint{
			Name: "a_small", Table: "w", Type: "check", Columns: []string{"a"}, Check: r4Ptr("a < 100000"),
			Up: map[string]string{"a": "a"}, Down: map[string]string{"a": "a"},
		})
	}
	uqAX := r4One("02_uq", &migrations.OpCreateConstraint{
		Name: "uq_ax", Table: "w", Type: "unique", Columns: []string{"a", "x"},
		Up: map[string]string{"a": "a", "x": "x"}, Down: map[string]string{"a": "a", "x": "x"},
	})
	rename := func(name string) *migrations.Migration {
		return r4One(name, &migrations.OpRenameColumn{Table: "w", From: "a", To: "b"})
	}
	drop := func(name, column string) *migrations.Migration {
		return r4One(name, &migrations.OpDropColumn{Table: "w", Column: column, Down: "x + 100"})
	}
	insB := `INSERT INTO w (id, x, b) VALUES (4, 40, 99)`
	insNoA := `INSERT INTO w (id, x) VALUES (4, 40)`
	return map[string]r4Train{
		"notnull; alter type":                   {migs: r4Seq(r4Add("01_add", shapes["notnull"]), alter("02_alter", migrations.OpAlterColumn{Type: &bigint}))},
		"unique; alter type":                    {migs: r4Seq(r4Add("01_add", shapes["unique"]), alter("02_alter", migrations.OpAlterColumn{Type: &bigint}))},
		"everything; alter type":                {migs: r4Seq(r4Add("01_add", shapes["everything"]), alter("02_alter", migrations.OpAlterColumn{Type: &bigint}))},
		"plain; alter notnull":                  {migs: r4Seq(r4Add("01_add", shapes["plain"]), alter("02_alter", migrations.OpAlterColumn{Nullable: &f, Up: "COALESCE(a, 1)"}))},
		"notnull; alter nullable":               {migs: r4Seq(r4Add("01_add", shapes["notnull"]), alter("02_alter", migrations.OpAlterColumn{Nullable: &tr}))},
		"check; alter unique":                   {migs: r4Seq(r4Add("01_add", shapes["check"]), alter("02_alter", migrations.OpAlterColumn{Unique: &migrations.UniqueConstraint{Name: "uq_a"}}))},
		"fk; alter type":                        {migs: r4Seq(r4Add("01_add", shapes["fk"]), alter("02_alter", migrations.OpAlterColumn{Type: &bigint})), newIns: `INSERT INTO w (id, x, a) VALUES (4, 40, 2)`},
		"nndefault; alter type":                 {migs: r4Seq(r4Add("01_add", shapes["nndefault"]), alter("02_alter", migrations.OpAlterColumn{Type: &bigint}))},
		"everything; rename":                    {migs: r4Seq(r4Add("01_add", shapes["everything"]), rename("02_rename")), newIns: insB},
		"notnull; rename; alter type":           {migs: r4Seq(r4Add("01_add", shapes["notnull"]), rename("02_rename"), r4One("03_alter", &migrations.OpAlterColumn{Table: "w", Column: "b", Type: &bigint, Up: "b", Down: "b"})), newIns: insB},
		"notnull; constraint check":             {migs: r4Seq(r4Add("01_add", shapes["notnull"]), ck("02_ck"))},
		"everything; constraint check":          {migs: r4Seq(r4Add("01_add", shapes["everything"]), ck("02_ck"))},
		"notnull; constraint unique a x":        {migs: r4Seq(r4Add("01_add", shapes["notnull"]), uqAX)},
		"unique; constraint check; alter type":  {migs: r4Seq(r4Add("01_add", shapes["unique"]), ck("02_ck"), alter("03_alter", migrations.OpAlterColumn{Type: &bigint}))},
		"notnull; constraint check; rename":     {migs: r4Seq(r4Add("01_add", shapes["notnull"]), ck("02_ck"), rename("03_rename")), newIns: insB},
		"everything; drop column":               {migs: r4Seq(r4Add("01_add", shapes["everything"]), drop("02_drop", "a")), newIns: insNoA},
		"notnull; alter type; drop column":      {migs: r4Seq(r4Add("01_add", shapes["notnull"]), alter("02_alter", migrations.OpAlterColumn{Type: &bigint}), drop("03_drop", "a")), newIns: insNoA},
		"unique; rename; drop column":           {migs: r4Seq(r4Add("01_add", shapes["unique"]), rename("02_rename"), drop("03_drop", "b")), newIns: insNoA},
		"check; constraint check; drop column":  {migs: r4Seq(r4Add("01_add", shapes["check"]), ck("02_ck"), drop("03_drop", "a")), newIns: insNoA},
		"notnull; alter type; alter check; ren": {migs: r4Seq(r4Add("01_add", shapes["notnull"]), alter("02_alter", migrations.OpAlterColumn{Type: &bigint}), alter("03_alter", migrations.OpAlterColumn{Check: &migrations.CheckConstraint{Name: "a_small", Constraint: "a < 100000"}}), rename("04_rename")), newIns: insB},
	}
}

func TestTrainSweepInterruptedContraction(t *testing.T) {
	skipUnlessTrainMatrix(t)
	skipUnlessTrainMatrix(t)
	t.Parallel()
	for tn, tr := range r5InterruptTrains() {
		t.Run(tn, func(t *testing.T) {
			t.Parallel()
			newIns := tr.newIns
			if newIns == "" {
				newIns = `INSERT INTO w (id, x, a) VALUES (4, 40, 99)`
			}
			apply := func(t *testing.T, mig *roll.Roll, db *sql.DB) {
				r2Base(t, mig, r4Base)
				require.NoError(t, r2TrainErr(mig, tr.train()))
				_ = r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x) VALUES (3, 30)`)
			}
			var clean string
			var total int
			t.Run("clean", func(t *testing.T) {
				testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public", r3Opts(true), func(mig *roll.Roll, db *sql.DB) {
					apply(t, mig, db)
					require.NoError(t, r2ExecAs(t, db, r2New, `UPDATE w SET id = id WHERE id = 1`))
					r4ArmFault(t, db, 1_000_000)
					err := r4Finish(mig)
					total = r4DisarmFault(t, db)
					ins := r2ExecAs(t, db, r2New, newIns)
					clean = r4Step("complete", func() error { return err }) + " || " + r4Catalog(t, db) + " || " + r4Rows(t, db)
					if ins != nil {
						clean += " newIns=" + r3Err(ins)
					}
				})
			})
			t.Logf("clean (%d DDL statements): %s", total, clean)
			t.Logf("CAT|%s|interrupted clean|%s", tn, clean)

			var bad []string
			for n := 1; n <= total; n++ {
				t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
					testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public", r3Opts(true), func(mig *roll.Roll, db *sql.DB) {
						apply(t, mig, db)
						r4ArmFault(t, db, n)
						first := r4Finish(mig)
						r4DisarmFault(t, db)
						between := "mid=" + r3Err(r2ExecAs(t, db, r2New, `UPDATE w SET id = id WHERE id = 1`))
						between += " midPublic=" + r3Err(r2ExecAs(t, db, "public", `UPDATE w SET id = id WHERE id = 2`))
						retry := r4Finish(mig)
						ins := r2ExecAs(t, db, r2New, newIns)
						got := r4Step("complete", func() error { return retry }) + " || " + r4Catalog(t, db) + " || " + r4Rows(t, db)
						if ins != nil {
							got += " newIns=" + r3Err(ins)
						}
						if got != clean || strings.Contains(between, "ERR") {
							bad = append(bad, fmt.Sprintf("n=%d first=%s %s retry=%s", n, r3Err(first), between, r3Err(retry)))
							t.Logf("n=%d first=%v %s\n  got:   %s\n  clean: %s", n, first, between, got, clean)
						}
					})
				})
			}
			t.Logf("SIG|%s|interrupted|BAD %d of %d: %s", tn, len(bad), total, strings.Join(bad, " ;; "))
		})
	}
}
