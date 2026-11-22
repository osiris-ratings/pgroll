// SPDX-License-Identifier: Apache-2.0

package roll_test

// Adversarial review of c2a0c8a. Builds with the base build too (6b2e5ea):
// it uses only the r2*/r3* helpers, which train_cases_test.go and
// train_matrix_test.go hold here and review2_test.go/review3_test.go hold in
// the base copy. Differential tests log SIG|/CAT|/ROW| lines that
// <scratch>/r4diff.sh compares between the two builds.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/oapi-codegen/nullable"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
	"github.com/xataio/pgroll/pkg/state"
)

// r4Catalog is everything in `public` a migration can leave behind: columns
// in physical order with type, nullability, default and comment; constraints
// with definition and validity; indexes with definition and validity;
// triggers; functions; replica identity; and the views of every version
// schema.
func r4Catalog(t *testing.T, db *sql.DB) string {
	t.Helper()
	return r3Str(t, db, `
	SELECT COALESCE(string_agg(line, ' ;; ' ORDER BY line), '') FROM (
		SELECT 'col ' || c.relname || '#' || row_number() OVER (PARTITION BY c.oid ORDER BY a.attnum) || ' ' || a.attname || ' ' || format_type(a.atttypid, a.atttypmod)
			|| CASE WHEN a.attnotnull THEN ' NOT NULL' ELSE '' END
			|| COALESCE(' DEFAULT ' || pg_get_expr(d.adbin, d.adrelid), '')
			|| COALESCE(' COMMENT ' || col_description(c.oid, a.attnum), '') AS line
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND a.attnum > 0 AND NOT a.attisdropped
		UNION ALL
		SELECT 'con ' || c.relname || '.' || con.conname || ' ' || pg_get_constraintdef(con.oid) || CASE WHEN con.convalidated THEN '' ELSE ' !NOTVALID' END
		FROM pg_constraint con JOIN pg_class c ON c.oid = con.conrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
		UNION ALL
		SELECT 'idx ' || pg_get_indexdef(i.indexrelid) || CASE WHEN i.indisvalid THEN '' ELSE ' !INVALID' END || CASE WHEN i.indisreplident THEN ' REPLIDENT' ELSE '' END
		FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
		UNION ALL
		SELECT 'trg ' || c.relname || '.' || t.tgname || ' -> ' || p.proname
		FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace JOIN pg_proc p ON p.oid = t.tgfoid
		WHERE n.nspname = 'public' AND NOT t.tgisinternal
		UNION ALL
		SELECT 'fn ' || p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'public'
		UNION ALL
		SELECT 'tbl ' || c.relname || ' replident=' || c.relreplident::text
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p')
		UNION ALL
		SELECT 'view ' || table_schema || '.' || table_name || '(' || string_agg(column_name, ',' ORDER BY column_name) || ')'
		FROM information_schema.columns WHERE table_schema LIKE 'public\_%' GROUP BY table_schema, table_name
	) s`)
}

// r4Rows is every row of every table in `public`.
func r4Rows(t *testing.T, db *sql.DB) string {
	t.Helper()
	rows, err := db.Query(`SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relkind = 'r' ORDER BY 1`)
	require.NoError(t, err)
	var tables []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		tables = append(tables, name)
	}
	require.NoError(t, rows.Close())
	out := make([]string, 0, len(tables))
	for _, table := range tables {
		out = append(out, table+"{"+r3TryStr(db, fmt.Sprintf(`SELECT string_agg(to_jsonb(t)::text, ' ' ORDER BY to_jsonb(t)::text) FROM public.%q t`, table))+"}")
	}
	return strings.Join(out, " ")
}

// r4History is pgroll's own record of the schema's migrations.
func r4History(t *testing.T, mig *roll.Roll, db *sql.DB) string {
	t.Helper()
	return r3TryStr(db, fmt.Sprintf(`SELECT COALESCE(string_agg(name || CASE WHEN done THEN '' ELSE ':active' END || CASE WHEN sealed THEN '' ELSE ':unsealed' END || CASE WHEN complete_deferred THEN ':deferred' ELSE '' END, ',' ORDER BY name), '') FROM %q.migrations WHERE schema = 'public'`, mig.State().Schema()))
}

// r4Finish is `pgroll complete`.
func r4Finish(mig *roll.Roll) error {
	ctx := context.Background()
	err := r2Safely(func() error { return mig.Complete(ctx) })
	if errors.Is(err, state.ErrNoActiveMigration) {
		err = r2Safely(func() error { _, _, err := mig.FinishContraction(ctx); return err })
	}
	if err != nil {
		return err
	}
	return r2Safely(func() error { _, err := mig.FinishPendingSealedRevert(ctx); return err })
}

func r4Step(label string, fn func() error) string { return label + "=" + r3Err(r2Safely(fn)) }

func r4One(name string, ops ...migrations.Operation) *migrations.Migration {
	return &migrations.Migration{Name: name, Operations: ops}
}

func r4Ptr[T any](v T) *T { return &v }

const r4Base = `CREATE TABLE p (id integer PRIMARY KEY);
	INSERT INTO p VALUES (1), (2), (99);
	CREATE TABLE w (id integer PRIMARY KEY, x integer);
	INSERT INTO w VALUES (1, 10), (2, 20)`

// r4AddShapes are the waiting columns a chain starts from.
func r4AddShapes() map[string]migrations.Column {
	zero, seven, comment := "0", "7", "a comment"
	return map[string]migrations.Column{
		"plain":      {Name: "a", Type: "integer", Nullable: true},
		"notnull":    {Name: "a", Type: "integer"},
		"unique":     {Name: "a", Type: "integer", Nullable: true, Unique: true},
		"check":      {Name: "a", Type: "integer", Nullable: true, Check: &migrations.CheckConstraint{Name: "a_pos", Constraint: "a > 0"}},
		"fk":         {Name: "a", Type: "integer", Nullable: true, References: &migrations.ForeignKeyReference{Name: "fk_a", Table: "p", Column: "id"}},
		"nndefault":  {Name: "a", Type: "integer", Default: &zero},
		"default":    {Name: "a", Type: "integer", Nullable: true, Default: &seven},
		"everything": {Name: "a", Type: "integer", Unique: true, Comment: &comment, Check: &migrations.CheckConstraint{Name: "a_pos", Constraint: "a > 0"}},
	}
}

func r4Add(name string, c migrations.Column) *migrations.Migration {
	up := "x + 100"
	if c.References != nil {
		up = "1"
	}
	return r4One(name, &migrations.OpAddColumn{Table: "w", Up: up, Column: c})
}

type r4Follow struct {
	only   []string // add shapes it applies to; empty for all
	migs   func() []*migrations.Migration
	newIns string
}

// r4Followers are what the later migrations of the train do to the waiting
// column. Names 02.. sort after 01_add.
func r4Followers() map[string]r4Follow {
	bigint, numeric, f, tr := "bigint", "numeric", false, true
	alter := func(name string, o migrations.OpAlterColumn) *migrations.Migration {
		o.Table, o.Column = "w", "a"
		if o.Up == "" {
			o.Up = "a"
		}
		if o.Down == "" {
			o.Down = "a"
		}
		return r4One(name, &o)
	}
	aType := func(name, typ string) *migrations.Migration {
		return alter(name, migrations.OpAlterColumn{Type: &typ})
	}
	renameAB := func(name string) *migrations.Migration {
		return r4One(name, &migrations.OpRenameColumn{Table: "w", From: "a", To: "b"})
	}
	insB := `INSERT INTO w (id, x, b) VALUES (4, 40, 99)`
	insNoA := `INSERT INTO w (id, x) VALUES (4, 40)`
	seq := func(ms ...*migrations.Migration) func() []*migrations.Migration {
		return func() []*migrations.Migration { return ms }
	}
	return map[string]r4Follow{
		"alter type":          {migs: seq(aType("02_alter", bigint))},
		"alter notnull":       {only: []string{"plain", "unique", "check", "fk", "default"}, migs: seq(alter("02_alter", migrations.OpAlterColumn{Nullable: &f, Up: "COALESCE(a, 1)"}))},
		"alter nullable":      {only: []string{"notnull", "nndefault", "everything"}, migs: seq(alter("02_alter", migrations.OpAlterColumn{Nullable: &tr}))},
		"alter unique":        {only: []string{"plain", "notnull", "check", "fk", "nndefault"}, migs: seq(alter("02_alter", migrations.OpAlterColumn{Unique: &migrations.UniqueConstraint{Name: "uq_a"}}))},
		"alter check":         {migs: seq(alter("02_alter", migrations.OpAlterColumn{Check: &migrations.CheckConstraint{Name: "a_small", Constraint: "a < 100000"}}))},
		"alter fk":            {only: []string{"plain", "notnull"}, migs: seq(alter("02_alter", migrations.OpAlterColumn{Up: "1", References: &migrations.ForeignKeyReference{Name: "fk_a2", Table: "p", Column: "id"}}))},
		"alter type twice":    {migs: seq(aType("02_alter", bigint), aType("03_alter", numeric))},
		"alter type notnull":  {only: []string{"plain", "unique", "check", "fk", "default"}, migs: seq(aType("02_alter", bigint), alter("03_alter", migrations.OpAlterColumn{Nullable: &f, Up: "COALESCE(a, 1)"}))},
		"alter nullable type": {only: []string{"notnull", "nndefault", "everything"}, migs: seq(alter("02_alter", migrations.OpAlterColumn{Nullable: &tr}), aType("03_alter", bigint))},
		"rename":              {migs: seq(renameAB("02_rename")), newIns: insB},
		"alter type; rename":  {migs: seq(aType("02_alter", bigint), renameAB("03_rename")), newIns: insB},
		"rename; alter type":  {migs: seq(renameAB("02_rename"), r4One("03_alter", &migrations.OpAlterColumn{Table: "w", Column: "b", Type: &bigint, Up: "b", Down: "b"})), newIns: insB},
		"rename; rename": {
			migs:   seq(renameAB("02_rename"), r4One("03_rename", &migrations.OpRenameColumn{Table: "w", From: "b", To: "c"})),
			newIns: `INSERT INTO w (id, x, c) VALUES (4, 40, 99)`,
		},
		"rename; drop": {migs: seq(renameAB("02_rename"), r4One("03_drop", &migrations.OpDropColumn{Table: "w", Column: "b", Down: "x + 100"})), newIns: insNoA},
		"rename; add a again": {
			migs:   seq(renameAB("02_rename"), r4One("03_add", &migrations.OpAddColumn{Table: "w", Up: "x + 7", Column: migrations.Column{Name: "a", Type: "text", Nullable: true}})),
			newIns: `INSERT INTO w (id, x, b, a) VALUES (4, 40, 99, 'new')`,
		},
		"index":             {migs: seq(r4One("02_index", &migrations.OpCreateIndex{Name: "ix_a", Table: "w", Columns: []migrations.IndexField{{Column: "a"}}}))},
		"index; rename":     {migs: seq(r4One("02_index", &migrations.OpCreateIndex{Name: "ix_a", Table: "w", Columns: []migrations.IndexField{{Column: "a"}}}), renameAB("03_rename")), newIns: insB},
		"index; alter type": {migs: seq(r4One("02_index", &migrations.OpCreateIndex{Name: "ix_a", Table: "w", Columns: []migrations.IndexField{{Column: "a"}}}), aType("03_alter", bigint))},
		"constraint unique": {only: []string{"plain", "notnull", "check", "fk", "nndefault", "default"}, migs: seq(r4One("02_uq", &migrations.OpCreateConstraint{
			Name: "uq_ax", Table: "w", Type: "unique", Columns: []string{"a", "x"},
			Up: map[string]string{"a": "a", "x": "x"}, Down: map[string]string{"a": "a", "x": "x"},
		}))},
		"constraint check": {migs: seq(r4One("02_ck", &migrations.OpCreateConstraint{
			Name: "a_small", Table: "w", Type: "check", Columns: []string{"a"}, Check: r4Ptr("a < 100000"),
			Up: map[string]string{"a": "a"}, Down: map[string]string{"a": "a"},
		}))},
		"constraint fk": {only: []string{"plain", "notnull", "unique"}, migs: seq(r4One("02_fk", &migrations.OpCreateConstraint{
			Name: "fk_a2", Table: "w", Type: "foreign_key", Columns: []string{"a"},
			References: &migrations.TableForeignKeyReference{Table: "p", Columns: []string{"id"}},
			Up:         map[string]string{"a": "1"}, Down: map[string]string{"a": "a"},
		})), newIns: `INSERT INTO w (id, x, a) VALUES (4, 40, 2)`},
		"constraint; alter type": {only: []string{"plain", "notnull", "unique"}, migs: seq(r4One("02_ck", &migrations.OpCreateConstraint{
			Name: "a_small", Table: "w", Type: "check", Columns: []string{"a"}, Check: r4Ptr("a < 100000"),
			Up: map[string]string{"a": "a"}, Down: map[string]string{"a": "a"},
		}), aType("03_alter", bigint))},
		"drop check":  {only: []string{"check", "everything"}, migs: seq(r4One("02_drop", &migrations.OpDropConstraint{Table: "w", Name: "a_pos", Up: "a", Down: "a"}))},
		"drop fk":     {only: []string{"fk"}, migs: seq(r4One("02_drop", &migrations.OpDropConstraint{Table: "w", Name: "fk_a", Up: "a", Down: "1"}))},
		"drop unique": {only: []string{"unique", "everything"}, migs: seq(r4One("02_drop", &migrations.OpDropConstraint{Table: "w", Name: "a", Up: "a", Down: "a"}))},
		"drop multi check": {only: []string{"check", "everything"}, migs: seq(r4One("02_drop", &migrations.OpDropMultiColumnConstraint{
			Table: "w", Name: "a_pos",
			Up: map[string]string{"a": "a"}, Down: map[string]string{"a": "a"},
		}))},
		"drop column":             {migs: seq(r4One("02_drop", &migrations.OpDropColumn{Table: "w", Column: "a", Down: "x + 100"})), newIns: insNoA},
		"alter type; drop column": {migs: seq(aType("02_alter", bigint), r4One("03_drop", &migrations.OpDropColumn{Table: "w", Column: "a", Down: "x + 100"})), newIns: insNoA},
		"drop column; add again": {migs: seq(r4One("02_drop", &migrations.OpDropColumn{Table: "w", Column: "a", Down: "x + 100"}),
			r4One("03_add", &migrations.OpAddColumn{Table: "w", Up: "x + 7", Column: migrations.Column{Name: "a", Type: "bigint", Nullable: true}}))},
		"replica identity full":    {migs: seq(r4One("02_ri", &migrations.OpSetReplicaIdentity{Table: "w", Identity: migrations.ReplicaIdentity{Type: "full"}}))},
		"replica identity index a": {only: []string{"everything"}, migs: seq(r4One("02_ri", &migrations.OpSetReplicaIdentity{Table: "w", Identity: migrations.ReplicaIdentity{Type: "index", Index: "a"}}))},
		"alter type; alter check; rename": {migs: seq(aType("02_alter", bigint),
			alter("03_alter", migrations.OpAlterColumn{Check: &migrations.CheckConstraint{Name: "a_small", Constraint: "a < 100000"}}), renameAB("04_rename")), newIns: insB},
		"raw then alter": {migs: seq(r2Raw("02_data", `UPDATE w SET a = a + 1`), aType("03_alter", bigint))},
	}
}

func r4Applies(f r4Follow, shape string) bool {
	if len(f.only) == 0 {
		return true
	}
	for _, s := range f.only {
		if s == shape {
			return true
		}
	}
	return false
}

// r4RollWrites are the writes of both revisions during a rollout.
func r4RollWrites(t *testing.T, db *sql.DB, newIns string) string {
	t.Helper()
	if newIns == "" {
		newIns = `INSERT INTO w (id, x, a) VALUES (4, 40, 99)`
	}
	var out []string
	run := func(label, path, stmt string) { out = append(out, label+"="+r3Err(r2ExecAs(t, db, path, stmt))) }
	run("prevIns", r2Prev, `INSERT INTO w (id, x) VALUES (3, 30)`)
	run("prevUpd", r2Prev, `UPDATE w SET x = 11 WHERE id = 1`)
	run("newIns", r2New, newIns)
	run("newUpd", r2New, `UPDATE w SET x = 21 WHERE id = 2`)
	run("prevUpdNewRow", r2Prev, `UPDATE w SET x = 41 WHERE id = 4`)
	return strings.Join(out, " ")
}

type r4End struct {
	versionSchemas bool
	run            func(t *testing.T, mig *roll.Roll, db *sql.DB, train []*migrations.Migration, newIns string) string
}

func r4Ends(n int) map[string]r4End {
	ctx := context.Background()
	rolling := func(t *testing.T, mig *roll.Roll, db *sql.DB, train []*migrations.Migration, newIns string) string {
		out := r4Step("train", func() error { return r2TrainErr(mig, train) })
		return out + " " + r4RollWrites(t, db, newIns)
	}
	ends := map[string]r4End{
		"contract": {true, func(t *testing.T, mig *roll.Roll, db *sql.DB, train []*migrations.Migration, newIns string) string {
			return rolling(t, mig, db, train, newIns) + " " + r4Step("complete", func() error { return r4Finish(mig) })
		}},
		"contract quiet": {true, func(t *testing.T, mig *roll.Roll, db *sql.DB, train []*migrations.Migration, newIns string) string {
			return r4Step("train", func() error { return r2TrainErr(mig, train) }) + " " + r4Step("complete", func() error { return r4Finish(mig) })
		}},
		"revert": {true, func(t *testing.T, mig *roll.Roll, db *sql.DB, train []*migrations.Migration, newIns string) string {
			return rolling(t, mig, db, train, newIns) + " " + r4Step("revert", func() error { _, err := mig.Revert(ctx); return err })
		}},
		"rollback revert": {true, func(t *testing.T, mig *roll.Roll, db *sql.DB, train []*migrations.Migration, newIns string) string {
			out := rolling(t, mig, db, train, newIns) + " " + r4Step("rollback", func() error { return mig.Rollback(ctx) })
			return out + " " + r4Step("revert", func() error { _, err := mig.Revert(ctx); return err })
		}},
		"sealed one shot": {true, func(t *testing.T, mig *roll.Roll, db *sql.DB, train []*migrations.Migration, newIns string) string {
			out := r4Step("train", func() error { return r2TrainErr(mig, train) }) + " " + r4Step("complete", func() error { return r4Finish(mig) })
			return out + " " + r4Step("sealed", func() error { _, err := mig.RevertSealed(ctx, "00_base", nil); return err })
		}},
		"sealed expand only": {true, func(t *testing.T, mig *roll.Roll, db *sql.DB, train []*migrations.Migration, newIns string) string {
			out := r4Step("train", func() error { return r2TrainErr(mig, train) }) + " " + r4Step("complete", func() error { return r4Finish(mig) })
			out += " " + r4Step("sealed", func() error { _, err := mig.RevertSealed(ctx, "00_base", nil, roll.WithExpandOnly()); return err })
			if newIns == "" {
				newIns = `INSERT INTO w (id, x, a) VALUES (4, 40, 99)`
			}
			out += " cur=" + r3Err(r2ExecAs(t, db, r2New, newIns))
			out += " restored=" + r3Err(r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x) VALUES (3, 30)`))
			out += " restoredUpd=" + r3Err(r2ExecAs(t, db, r2Prev, `UPDATE w SET x = 41 WHERE id = 4`))
			return out + " " + r4Step("finish", func() error { return r4Finish(mig) })
		}},
		"complete train": {true, func(t *testing.T, mig *roll.Roll, db *sql.DB, train []*migrations.Migration, newIns string) string {
			return r4Step("train", func() error { return r3CompleteTrainErr(mig, train) }) + " " + r4Step("complete", func() error { return r4Finish(mig) })
		}},
		"no version schemas": {false, func(t *testing.T, mig *roll.Roll, db *sql.DB, train []*migrations.Migration, newIns string) string {
			return r4Step("train", func() error { return r2TrainErr(mig, train) }) + " " + r4Step("complete", func() error { return r4Finish(mig) })
		}},
		"one by one": {true, func(t *testing.T, mig *roll.Roll, db *sql.DB, train []*migrations.Migration, newIns string) string {
			out := make([]string, 0, len(train))
			for _, m := range train {
				out = append(out, r4Step(m.Name, func() error {
					if err := mig.Start(ctx, m, backfill.NewConfig()); err != nil {
						return err
					}
					return mig.Complete(ctx)
				}))
			}
			return strings.Join(out, " ")
		}},
	}
	// A bounded revert to each migration of the train but the last, then
	// the contraction of what is left.
	for i := 0; i < n-1; i++ {
		ends[fmt.Sprintf("revert to %d then contract", i+1)] = r4End{true, func(t *testing.T, mig *roll.Roll, db *sql.DB, train []*migrations.Migration, newIns string) string {
			out := rolling(t, mig, db, train, newIns)
			out += " " + r4Step("revertTo", func() error { _, err := mig.Revert(ctx, roll.WithRevertTo(train[i].Name)); return err })
			return out + " " + r4Step("complete", func() error { return r4Finish(mig) })
		}}
		ends[fmt.Sprintf("revert to %d then revert", i+1)] = r4End{true, func(t *testing.T, mig *roll.Roll, db *sql.DB, train []*migrations.Migration, newIns string) string {
			out := rolling(t, mig, db, train, newIns)
			out += " " + r4Step("revertTo", func() error { _, err := mig.Revert(ctx, roll.WithRevertTo(train[i].Name)); return err })
			return out + " " + r4Step("revert", func() error { _, err := mig.Revert(ctx); return err })
		}}
	}
	return ends
}

func r4RunChain(t *testing.T, id string, versionSchemas bool, base string, body func(t *testing.T, mig *roll.Roll, db *sql.DB) string) {
	t.Helper()
	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public", r3Opts(versionSchemas), func(mig *roll.Roll, db *sql.DB) {
		r2Base(t, mig, base)
		steps := body(t, mig, db)
		t.Logf("SIG|%s|%s", id, steps)
		t.Logf("CAT|%s|%s || history[%s]", id, r4Catalog(t, db), r4History(t, mig, db))
		t.Logf("ROW|%s|%s", id, r4Rows(t, db))
	})
}

// TestTrainChainChains: every waiting add_column shape, followed by one to three
// later migrations of the train that touch the column, ended every way a
// train can end.
func TestTrainChainChains(t *testing.T) {
	skipUnlessTrainMatrix(t)
	t.Parallel()
	shapes := r4AddShapes()
	for fn, f := range r4Followers() {
		for sn, shape := range shapes {
			if !r4Applies(f, sn) {
				continue
			}
			n := len(f.migs()) + 1
			for en, end := range r4Ends(n) {
				t.Run(sn+"/"+fn+"/"+en, func(t *testing.T) {
					t.Parallel()
					r4RunChain(t, sn+" + "+fn+"|"+en, end.versionSchemas, r4Base, func(t *testing.T, mig *roll.Roll, db *sql.DB) string {
						train := append([]*migrations.Migration{r4Add("01_add", shape)}, f.migs()...)
						train = append(train, r2Last())
						return end.run(t, mig, db, train, f.newIns)
					})
				})
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Named trains: each a list of migrations (09_last is appended), ended the
// ways r4Ends lists.
// ---------------------------------------------------------------------------

type r4Train struct {
	base   string
	migs   func() []*migrations.Migration
	newIns string
	ends   []string // empty for a default set
	whole  bool     // migs is the whole train: 09_last is not appended
}

func (tr r4Train) train() []*migrations.Migration {
	if tr.whole {
		return tr.migs()
	}
	return append(tr.migs(), r2Last())
}

func r4RunTrains(t *testing.T, trains map[string]r4Train) {
	t.Helper()
	for tn, tr := range trains {
		n := len(tr.migs())
		all := r4Ends(n)
		names := tr.ends
		if len(names) == 0 {
			names = []string{"contract", "contract quiet", "revert", "complete train", "one by one", "sealed one shot"}
			for i := 0; i < n-1; i++ {
				names = append(names, fmt.Sprintf("revert to %d then contract", i+1))
			}
		}
		for _, en := range names {
			end, ok := all[en]
			if !ok {
				continue
			}
			t.Run(tn+"/"+en, func(t *testing.T) {
				t.Parallel()
				base := tr.base
				if base == "" {
					base = r4Base
				}
				r4RunChain(t, tn+"|"+en, end.versionSchemas, base, func(t *testing.T, mig *roll.Roll, db *sql.DB) string {
					return end.run(t, mig, db, tr.train(), tr.newIns)
				})
			})
		}
	}
}

func r4Seq(ms ...*migrations.Migration) func() []*migrations.Migration {
	return func() []*migrations.Migration { return ms }
}

// A later raw SQL migration of the train changes the column a waiting
// add_column added. The add_column's Start is replayed, over the schema read
// from the database, by every later Start of the train.
func TestTrainChainRawSQLChangesTheWaitingColumn(t *testing.T) {
	skipUnlessTrainMatrix(t)
	t.Parallel()
	shapes := r4AddShapes()
	bigint, f := "bigint", false
	insNoA := `INSERT INTO w (id, x) VALUES (4, 40)`
	alterType := r4One("03_alter", &migrations.OpAlterColumn{Table: "w", Column: "a", Type: &bigint, Up: "a", Down: "a"})
	alterNotNull := r4One("03_alter", &migrations.OpAlterColumn{Table: "w", Column: "a", Nullable: &f, Up: "COALESCE(a, 1)", Down: "a"})
	r4RunTrains(t, map[string]r4Train{
		"nndefault; raw set default 42":             {migs: r4Seq(r4Add("01_add", shapes["nndefault"]), r2Raw("02_raw", `ALTER TABLE w ALTER COLUMN a SET DEFAULT 42`)), newIns: insNoA},
		"default; raw drop default":                 {migs: r4Seq(r4Add("01_add", shapes["default"]), r2Raw("02_raw", `ALTER TABLE w ALTER COLUMN a DROP DEFAULT`)), newIns: insNoA},
		"plain; raw set default 42":                 {migs: r4Seq(r4Add("01_add", shapes["plain"]), r2Raw("02_raw", `ALTER TABLE w ALTER COLUMN a SET DEFAULT 42`)), newIns: insNoA},
		"nndefault; raw set default 42; alter type": {migs: r4Seq(r4Add("01_add", shapes["nndefault"]), r2Raw("02_raw", `ALTER TABLE w ALTER COLUMN a SET DEFAULT 42`), alterType), newIns: insNoA},
		"plain; raw type bigint; alter notnull":     {migs: r4Seq(r4Add("01_add", shapes["plain"]), r2Raw("02_raw", `ALTER TABLE w ALTER COLUMN a TYPE bigint`), alterNotNull), newIns: `INSERT INTO w (id, x, a) VALUES (4, 40, 5000000000)`},
		"notnull; raw drop not null":                {migs: r4Seq(r4Add("01_add", shapes["notnull"]), r2Raw("02_raw", `ALTER TABLE w ALTER COLUMN a DROP NOT NULL`))},
		"notnull; raw drop not null; alter type":    {migs: r4Seq(r4Add("01_add", shapes["notnull"]), r2Raw("02_raw", `ALTER TABLE w ALTER COLUMN a DROP NOT NULL`), alterType)},
		"plain; raw set not null; alter type":       {migs: r4Seq(r4Add("01_add", shapes["plain"]), r2Raw("02_raw", `ALTER TABLE w ALTER COLUMN a SET NOT NULL`), alterType)},
		"check; raw drop constraint":                {migs: r4Seq(r4Add("01_add", shapes["check"]), r2Raw("02_raw", `ALTER TABLE w DROP CONSTRAINT a_pos`))},
		"check; raw validate constraint":            {migs: r4Seq(r4Add("01_add", shapes["check"]), r2Raw("02_raw", `ALTER TABLE w VALIDATE CONSTRAINT a_pos`))},
		"unique; raw drop constraint":               {migs: r4Seq(r4Add("01_add", shapes["unique"]), r2Raw("02_raw", `ALTER TABLE w DROP CONSTRAINT a`))},
		"fk; raw drop constraint":                   {migs: r4Seq(r4Add("01_add", shapes["fk"]), r2Raw("02_raw", `ALTER TABLE w DROP CONSTRAINT fk_a`)), newIns: `INSERT INTO w (id, x, a) VALUES (4, 40, 2)`},
		"plain; raw rename":                         {migs: r4Seq(r4Add("01_add", shapes["plain"]), r2Raw("02_raw", `ALTER TABLE w RENAME COLUMN a TO b`)), newIns: `INSERT INTO w (id, x, b) VALUES (4, 40, 99)`},
		"plain; raw drop column":                    {migs: r4Seq(r4Add("01_add", shapes["plain"]), r2Raw("02_raw", `ALTER TABLE w DROP COLUMN a`)), newIns: insNoA},
		"plain; raw comment; alter type":            {migs: r4Seq(r4Add("01_add", shapes["plain"]), r2Raw("02_raw", `COMMENT ON COLUMN w.a IS 'hello'`), alterType)},
		"everything; raw comment":                   {migs: r4Seq(r4Add("01_add", shapes["everything"]), r2Raw("02_raw", `COMMENT ON COLUMN w.a IS 'hello'`))},
		"unique; new revision upserts on the constraint": {
			migs:   r4Seq(r4Add("01_add", shapes["unique"]), r2Raw("02_raw", `SELECT 1`)),
			newIns: `INSERT INTO w (id, x, a) VALUES (4, 40, 99) ON CONFLICT ON CONSTRAINT a DO NOTHING`,
		},
		"unique; raw sql upserts on the constraint": {migs: r4Seq(r4Add("01_add", shapes["unique"]), r2Raw("02_raw", `INSERT INTO w (id, x, a) VALUES (7, 70, 170) ON CONFLICT ON CONSTRAINT a DO NOTHING`))},
		"plain; raw index on a; alter type":         {migs: r4Seq(r4Add("01_add", shapes["plain"]), r2Raw("02_raw", `CREATE INDEX ix_raw ON w (a)`), alterType)},
		"plain; raw table rename":                   {migs: r4Seq(r4Add("01_add", shapes["plain"]), r2Raw("02_raw", `ALTER TABLE w RENAME TO w2`)), newIns: `INSERT INTO w2 (id, x, a) VALUES (4, 40, 99)`},
	})
}

// Two alter_columns on one existing column in one train, and other
// duplicating operations after a set NOT NULL: no add_column anywhere. The
// duplicator no longer copies a `_pgroll_check_not_null_*` check.
func TestTrainChainAltersOfAnExistingColumn(t *testing.T) {
	skipUnlessTrainMatrix(t)
	t.Parallel()
	bigint, f, tr := "bigint", false, true
	alter := func(name string, o migrations.OpAlterColumn) *migrations.Migration {
		o.Table, o.Column = "w", "x"
		if o.Up == "" {
			o.Up = "x"
		}
		if o.Down == "" {
			o.Down = "x"
		}
		return r4One(name, &o)
	}
	notNull := func(name string) *migrations.Migration {
		return alter(name, migrations.OpAlterColumn{Nullable: &f, Up: "COALESCE(x, 0)"})
	}
	typ := func(name string) *migrations.Migration { return alter(name, migrations.OpAlterColumn{Type: &bigint}) }
	check := func(name string) *migrations.Migration {
		return alter(name, migrations.OpAlterColumn{Check: &migrations.CheckConstraint{Name: "x_pos", Constraint: "x >= 0"}})
	}
	ins := `INSERT INTO w (id, x) VALUES (4, 40)`
	nnBase := `CREATE TABLE p (id integer PRIMARY KEY); INSERT INTO p VALUES (1), (2), (99);
		CREATE TABLE w (id integer PRIMARY KEY, x integer NOT NULL); INSERT INTO w VALUES (1, 10), (2, 20)`
	r4RunTrains(t, map[string]r4Train{
		"notnull; type":        {migs: r4Seq(notNull("01_nn"), typ("02_type")), newIns: ins},
		"type; notnull":        {migs: r4Seq(typ("01_type"), notNull("02_nn")), newIns: ins},
		"notnull; check":       {migs: r4Seq(notNull("01_nn"), check("02_check")), newIns: ins},
		"notnull; unique":      {migs: r4Seq(notNull("01_nn"), alter("02_uq", migrations.OpAlterColumn{Unique: &migrations.UniqueConstraint{Name: "uq_x"}})), newIns: ins},
		"notnull; type; check": {migs: r4Seq(notNull("01_nn"), typ("02_type"), check("03_check")), newIns: ins},
		"notnull; rename": {
			migs:   r4Seq(notNull("01_nn"), r4One("02_rename", &migrations.OpRenameColumn{Table: "w", From: "x", To: "y"})),
			newIns: `INSERT INTO w (id, y) VALUES (4, 40)`,
		},
		"notnull; constraint check": {migs: r4Seq(notNull("01_nn"), r4One("02_ck", &migrations.OpCreateConstraint{
			Name: "x_pos", Table: "w", Type: "check", Columns: []string{"x"}, Check: r4Ptr("x >= 0"),
			Up: map[string]string{"x": "x"}, Down: map[string]string{"x": "x"},
		})), newIns: ins},
		"notnull; constraint unique": {migs: r4Seq(notNull("01_nn"), r4One("02_uq", &migrations.OpCreateConstraint{
			Name: "uq_x", Table: "w", Type: "unique", Columns: []string{"x"},
			Up: map[string]string{"x": "x"}, Down: map[string]string{"x": "x"},
		})), newIns: ins},
		"notnull; add a reading x": {migs: r4Seq(notNull("01_nn"), r4Add("02_add", r4AddShapes()["notnull"]))},
		"nullable; type":           {base: nnBase, migs: r4Seq(alter("01_null", migrations.OpAlterColumn{Nullable: &tr}), typ("02_type")), newIns: ins},
		"nullable; notnull":        {base: nnBase, migs: r4Seq(alter("01_null", migrations.OpAlterColumn{Nullable: &tr}), notNull("02_nn")), newIns: ins},
		"type; type":               {base: nnBase, migs: r4Seq(typ("01_type"), alter("02_type", migrations.OpAlterColumn{Type: r4Ptr("numeric")})), newIns: ins},
		"type; check":              {base: nnBase, migs: r4Seq(typ("01_type"), check("02_check")), newIns: ins},
	})
}

// A table whose own constraint or index has a name pgroll also uses for a
// column in flight. One migration, started and completed.
func TestTrainChainUserObjectsNamedLikePgrolls(t *testing.T) {
	t.Parallel()
	bigint := "bigint"
	alterX := r4One("01_alter", &migrations.OpAlterColumn{Table: "w", Column: "x", Type: &bigint, Up: "x", Down: "x"})
	bases := map[string]string{
		"check named _pgroll_check_not_null_x":                   `CREATE TABLE w (id integer PRIMARY KEY, x integer, CONSTRAINT _pgroll_check_not_null_x CHECK (x IS NOT NULL)); INSERT INTO w VALUES (1, 10), (2, 20)`,
		"check named _pgroll_check_not_null_positive":            `CREATE TABLE w (id integer PRIMARY KEY, x integer, CONSTRAINT _pgroll_check_not_null_positive CHECK (x > 0)); INSERT INTO w VALUES (1, 10), (2, 20)`,
		"index named _pgroll_uniq_x beside a constraint named x": `CREATE TABLE w (id integer PRIMARY KEY, x integer, y integer, CONSTRAINT x UNIQUE (y)); CREATE UNIQUE INDEX _pgroll_uniq_x ON w (x); INSERT INTO w VALUES (1, 10, 1), (2, 20, 2)`,
	}
	for bn, base := range bases {
		for en, end := range map[string]bool{"complete": true, "rollback": false} {
			t.Run(bn+"/"+en, func(t *testing.T) {
				t.Parallel()
				r4RunChain(t, bn+"|"+en, true, base, func(t *testing.T, mig *roll.Roll, db *sql.DB) string {
					ctx := context.Background()
					out := r4Step("start", func() error { return mig.Start(ctx, alterX, backfill.NewConfig()) })
					out += " newInsNull=" + r3Err(r2ExecAs(t, db, "public_01_alter", `INSERT INTO w (id, x) VALUES (4, NULL)`))
					out += " newInsNeg=" + r3Err(r2ExecAs(t, db, "public_01_alter", `INSERT INTO w (id, x) VALUES (5, -5)`))
					out += " newInsDup=" + r3Err(r2ExecAs(t, db, "public_01_alter", `INSERT INTO w (id, x) VALUES (6, 10)`))
					if end {
						out += " " + r4Step("complete", func() error { return mig.Complete(ctx) })
					} else {
						out += " " + r4Step("rollback", func() error { return mig.Rollback(ctx) })
					}
					out += " insNull=" + r3Err(r2ExecAs(t, db, "public", `INSERT INTO w (id, x) VALUES (7, NULL)`))
					out += " insNeg=" + r3Err(r2ExecAs(t, db, "public", `INSERT INTO w (id, x) VALUES (8, -5)`))
					out += " insDup=" + r3Err(r2ExecAs(t, db, "public", `INSERT INTO w (id, x) VALUES (9, 20)`))
					return out
				})
			})
		}
	}
}

// One migration that adds a filling column and does something else: it now
// waits for the contraction as a whole, its other operations included.
func TestTrainChainAddColumnBesideOtherOperations(t *testing.T) {
	skipUnlessTrainMatrix(t)
	t.Parallel()
	shapes := r4AddShapes()
	bigint, f := "bigint", false
	add := func(shape string) *migrations.OpAddColumn {
		c := shapes[shape]
		up := "x + 100"
		if c.References != nil {
			up = "1"
		}
		return &migrations.OpAddColumn{Table: "w", Up: up, Column: c}
	}
	uqX := &migrations.OpCreateConstraint{Name: "uq_x", Table: "w", Type: "unique", Columns: []string{"x"}, Up: map[string]string{"x": "x"}, Down: map[string]string{"x": "x"}}
	ckX := func(name string) *migrations.OpCreateConstraint {
		return &migrations.OpCreateConstraint{Name: name, Table: "w", Type: "check", Columns: []string{"x"}, Check: r4Ptr("x >= 0"), Up: map[string]string{"x": "x"}, Down: map[string]string{"x": "x"}}
	}
	base := `CREATE TABLE p (id integer PRIMARY KEY); INSERT INTO p VALUES (1), (2), (99);
		CREATE TABLE w (id integer PRIMARY KEY, x integer, CONSTRAINT x_small CHECK (x < 1000000)); INSERT INTO w VALUES (1, 10), (2, 20)`
	r4RunTrains(t, map[string]r4Train{
		// The other operation is on the added column.
		"[add notnull, alter a type]":  {migs: r4Seq(r4One("01_m", add("notnull"), &migrations.OpAlterColumn{Table: "w", Column: "a", Type: &bigint, Up: "a", Down: "a"}))},
		"[add unique, alter a type]":   {migs: r4Seq(r4One("01_m", add("unique"), &migrations.OpAlterColumn{Table: "w", Column: "a", Type: &bigint, Up: "a", Down: "a"}))},
		"[add check, alter a type]":    {migs: r4Seq(r4One("01_m", add("check"), &migrations.OpAlterColumn{Table: "w", Column: "a", Type: &bigint, Up: "a", Down: "a"}))},
		"[add plain, alter a notnull]": {migs: r4Seq(r4One("01_m", add("plain"), &migrations.OpAlterColumn{Table: "w", Column: "a", Nullable: &f, Up: "COALESCE(a, 1)", Down: "a"}))},
		"[add plain, constraint check a]": {migs: r4Seq(r4One("01_m", add("plain"), &migrations.OpCreateConstraint{
			Name: "a_small", Table: "w", Type: "check", Columns: []string{"a"}, Check: r4Ptr("a < 100000"),
			Up: map[string]string{"a": "a"}, Down: map[string]string{"a": "a"},
		}))},
		"[add notnull, constraint unique a x]": {migs: r4Seq(r4One("01_m", add("notnull"), &migrations.OpCreateConstraint{
			Name: "uq_ax", Table: "w", Type: "unique", Columns: []string{"a", "x"},
			Up: map[string]string{"a": "a", "x": "x"}, Down: map[string]string{"a": "a", "x": "x"},
		}))},
		"[add check, drop constraint a_pos]": {migs: r4Seq(r4One("01_m", add("check"), &migrations.OpDropConstraint{Table: "w", Name: "a_pos", Up: "a", Down: "a"}))},
		"[add plain, rename a b]":            {migs: r4Seq(r4One("01_m", add("plain"), &migrations.OpRenameColumn{Table: "w", From: "a", To: "b"})), newIns: `INSERT INTO w (id, x, b) VALUES (4, 40, 99)`},
		// The other operation is on another column of the table, and a later
		// migration of the train builds on it.
		"[add, constraint uq_x]":                          {base: base, migs: r4Seq(r4One("01_m", add("plain"), uqX))},
		"[add, constraint uq_x]; constraint check x":      {base: base, migs: r4Seq(r4One("01_m", add("plain"), uqX), r4One("02_ck", ckX("x_pos")))},
		"[add, constraint uq_x]; alter x type":            {base: base, migs: r4Seq(r4One("01_m", add("plain"), uqX), r4One("02_alter", &migrations.OpAlterColumn{Table: "w", Column: "x", Type: &bigint, Up: "x", Down: "x"}))},
		"[add, constraint uq_x]; drop constraint uq_x":    {base: base, migs: r4Seq(r4One("01_m", add("plain"), uqX), r4One("02_drop", &migrations.OpDropMultiColumnConstraint{Table: "w", Name: "uq_x", Up: map[string]string{"x": "x"}, Down: map[string]string{"x": "x"}}))},
		"[add, constraint uq_x]; drop constraint x_small": {base: base, migs: r4Seq(r4One("01_m", add("plain"), uqX), r4One("02_drop", &migrations.OpDropConstraint{Table: "w", Name: "x_small", Up: "x", Down: "x"}))},
		"[add, constraint uq_x]; table with fk to x": {base: base, migs: r4Seq(r4One("01_m", add("plain"), uqX), r4One("02_table", &migrations.OpCreateTable{Name: "t", Columns: []migrations.Column{
			{Name: "id", Type: "integer", Pk: true},
			{Name: "w_x", Type: "integer", Nullable: true, References: &migrations.ForeignKeyReference{Name: "fk_t_w", Table: "w", Column: "x"}},
		}}))},
		"[add, constraint uq_x]; raw upsert on uq_x": {base: base, migs: r4Seq(r4One("01_m", add("plain"), uqX), r2Raw("02_raw", `INSERT INTO w (id, x) VALUES (7, 10) ON CONFLICT ON CONSTRAINT uq_x DO NOTHING`))},
		"[add, constraint ck x]; constraint uq_x":    {base: base, migs: r4Seq(r4One("01_m", add("plain"), ckX("x_pos")), r4One("02_uq", uqX))},
		"[add, constraint ck x]; add b; constraint ck2 x": {base: base, migs: r4Seq(r4One("01_m", add("plain"), ckX("x_pos")),
			r4One("02_add", &migrations.OpAddColumn{Table: "w", Up: "x * 2", Column: migrations.Column{Name: "b", Type: "integer", Nullable: true}}), r4One("03_ck", ckX("x_pos2")))},
		"[add, set replica identity full]": {base: base, migs: r4Seq(r4One("01_m", add("plain"), &migrations.OpSetReplicaIdentity{Table: "w", Identity: migrations.ReplicaIdentity{Type: "full"}}))},
		"[add, create table t]; drop table t; create table t": {base: base, migs: r4Seq(
			r4One("01_m", &migrations.OpCreateTable{Name: "t", Columns: []migrations.Column{{Name: "id", Type: "integer", Pk: true}, {Name: "v", Type: "text", Nullable: true}}}, add("plain")),
			r4One("02_drop", &migrations.OpDropTable{Name: "t"}),
			r4One("03_create", &migrations.OpCreateTable{Name: "t", Columns: []migrations.Column{{Name: "id", Type: "bigint", Pk: true}, {Name: "n", Type: "integer", Nullable: true}}}),
		)},
		"[add, create table t]; drop table t": {base: base, migs: r4Seq(
			r4One("01_m", &migrations.OpCreateTable{Name: "t", Columns: []migrations.Column{{Name: "id", Type: "integer", Pk: true}, {Name: "v", Type: "text", Nullable: true}}}, add("plain")),
			r4One("02_drop", &migrations.OpDropTable{Name: "t"}),
		)},
		"[add, create table t]; alter t.v; drop column t.v": {base: base, migs: r4Seq(
			r4One("01_m", &migrations.OpCreateTable{Name: "t", Columns: []migrations.Column{{Name: "id", Type: "integer", Pk: true}, {Name: "v", Type: "integer", Nullable: true}}}, add("plain")),
			r4One("02_alter", &migrations.OpAlterColumn{Table: "t", Column: "v", Type: &bigint, Up: "v", Down: "v"}),
			r4One("03_drop", &migrations.OpDropColumn{Table: "t", Column: "v", Down: "0"}),
		)},
		"[add, create table t]; rename t u; add column u.c": {base: base, migs: r4Seq(
			r4One("01_m", &migrations.OpCreateTable{Name: "t", Columns: []migrations.Column{{Name: "id", Type: "integer", Pk: true}, {Name: "v", Type: "integer", Nullable: true}}}, add("plain")),
			r4One("02_rename", &migrations.OpRenameTable{From: "t", To: "u"}),
			r4One("03_add", &migrations.OpAddColumn{Table: "u", Up: "id", Column: migrations.Column{Name: "c", Type: "integer", Nullable: true}}),
		)},
		"add; create t; drop t; create t": {base: base, migs: r4Seq(
			r4One("01_add", add("plain")),
			r4One("02_create", &migrations.OpCreateTable{Name: "t", Columns: []migrations.Column{{Name: "id", Type: "integer", Pk: true}, {Name: "v", Type: "text", Nullable: true}}}),
			r4One("03_drop", &migrations.OpDropTable{Name: "t"}),
			r4One("04_create", &migrations.OpCreateTable{Name: "t", Columns: []migrations.Column{{Name: "id", Type: "bigint", Pk: true}, {Name: "n", Type: "integer", Nullable: true}}}),
		)},
	})
}

// ---------------------------------------------------------------------------
// A contraction interrupted at each DDL statement, then retried.
// ---------------------------------------------------------------------------

// r4ArmFault makes the n-th DDL statement from now on fail, once.
func r4ArmFault(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	_, err := db.Exec(fmt.Sprintf(`
		CREATE SCHEMA IF NOT EXISTS r4fault;
		DROP EVENT TRIGGER IF EXISTS r4_fault;
		DROP SEQUENCE IF EXISTS r4fault.seq;
		CREATE SEQUENCE r4fault.seq;
		CREATE OR REPLACE FUNCTION r4fault.trip() RETURNS event_trigger LANGUAGE plpgsql AS $f$
		BEGIN
			IF nextval('r4fault.seq') = %d THEN
				RAISE EXCEPTION 'r4 injected fault at %%', tg_tag;
			END IF;
		END $f$;
		CREATE EVENT TRIGGER r4_fault ON ddl_command_start EXECUTE FUNCTION r4fault.trip()`, n))
	require.NoError(t, err)
}

// r4DisarmFault returns how many DDL statements ran while armed.
func r4DisarmFault(t *testing.T, db *sql.DB) int {
	t.Helper()
	_, err := db.Exec(`DROP EVENT TRIGGER IF EXISTS r4_fault`)
	require.NoError(t, err)
	var called bool
	var last int
	require.NoError(t, db.QueryRow(`SELECT is_called, last_value FROM r4fault.seq`).Scan(&called, &last))
	if !called {
		return 0
	}
	return last
}

func r4InterruptTrains() map[string]r4Train {
	shapes := r4AddShapes()
	bigint := "bigint"
	table := func(name string) *migrations.OpCreateTable {
		return &migrations.OpCreateTable{Name: name, Columns: []migrations.Column{{Name: "id", Type: "integer", Pk: true}, {Name: "v", Type: "text", Nullable: true}}}
	}
	addB := func(name, up string, unique bool) *migrations.Migration {
		return r4One(name, &migrations.OpAddColumn{Table: "w", Up: up, Column: migrations.Column{Name: "b", Type: "integer", Nullable: true, Unique: unique}})
	}
	return map[string]r4Train{
		"add notnull; add b unique; alter x": {
			migs: r4Seq(r4Add("01_add", shapes["notnull"]), addB("02_add", "x + 200", true),
				r4One("03_alter", &migrations.OpAlterColumn{Table: "w", Column: "x", Type: &bigint, Up: "x", Down: "x"})),
			newIns: `INSERT INTO w (id, x, a, b) VALUES (4, 40, 99, 98)`,
		},
		"add; add b reading a; rename x": {
			migs: r4Seq(r4Add("01_add", shapes["plain"]), addB("02_add", "a * 10", false),
				r4One("03_rename", &migrations.OpRenameColumn{Table: "w", From: "x", To: "y"})),
			newIns: `INSERT INTO w (id, y, a, b) VALUES (4, 40, 99, 98)`,
		},
		"add everything; alter a; rename a": {
			migs: r4Seq(r4Add("01_add", shapes["everything"]),
				r4One("02_alter", &migrations.OpAlterColumn{Table: "w", Column: "a", Type: &bigint, Up: "a", Down: "a"}),
				r4One("03_rename", &migrations.OpRenameColumn{Table: "w", From: "a", To: "b"})),
			newIns: `INSERT INTO w (id, x, b) VALUES (4, 40, 99)`,
		},
		"add check; index; drop column": {
			migs: r4Seq(r4Add("01_add", shapes["check"]),
				r4One("02_index", &migrations.OpCreateIndex{Name: "ix_a", Table: "w", Columns: []migrations.IndexField{{Column: "a"}}}),
				r4One("03_drop", &migrations.OpDropColumn{Table: "w", Column: "a", Down: "x + 100"})),
			newIns: `INSERT INTO w (id, x) VALUES (4, 40)`,
		},
		"add nndefault; add fk": {
			migs: r4Seq(r4Add("01_add", shapes["nndefault"]),
				r4One("02_add", &migrations.OpAddColumn{Table: "w", Up: "1", Column: migrations.Column{Name: "b", Type: "integer", Nullable: true, References: &migrations.ForeignKeyReference{Name: "fk_b", Table: "p", Column: "id"}}})),
			newIns: `INSERT INTO w (id, x, a, b) VALUES (4, 40, 99, 2)`,
		},
		"[create t, add]; drop t; create t": {migs: r4Seq(r4One("01_m", table("t"), &migrations.OpAddColumn{Table: "w", Up: "x + 100", Column: shapes["plain"]}),
			r4One("02_drop", &migrations.OpDropTable{Name: "t"}), r4One("03_create", table("t")))},
		"add unique alone": {migs: r4Seq(r4Add("01_add", shapes["everything"]))},
		// The same column as the train's last migration, which completes
		// at the contraction on both builds.
		"add everything as the leaf": {whole: true, migs: r4Seq(r2Raw("01_first", "SELECT 1"), r4Add("09_last", shapes["everything"]))},
		"add notnull as the leaf":    {whole: true, migs: r4Seq(r2Raw("01_first", "SELECT 1"), r4Add("09_last", shapes["notnull"]))},
		"add notnull alone":          {migs: r4Seq(r4Add("01_add", shapes["notnull"]))},
		"add nndefault alone":        {migs: r4Seq(r4Add("01_add", shapes["nndefault"]))},
		"add check alone":            {migs: r4Seq(r4Add("01_add", shapes["check"]))},
	}
}

// TestTrainChainInterruptedContraction fails the contraction at its n-th DDL
// statement, for every n, lets both the new revision and a session on
// `public` write, and retries. BAD lists the n where the retry did not end
// in the catalog an uninterrupted contraction leaves, or where a write
// failed in between.
func TestTrainChainInterruptedContraction(t *testing.T) {
	skipUnlessTrainMatrix(t)
	t.Parallel()
	for tn, tr := range r4InterruptTrains() {
		t.Run(tn, func(t *testing.T) {
			t.Parallel()
			newIns := tr.newIns
			if newIns == "" {
				newIns = `INSERT INTO w (id, x, a) VALUES (4, 40, 99)`
			}
			apply := func(t *testing.T, mig *roll.Roll, db *sql.DB) {
				r2Base(t, mig, r4Base)
				require.NoError(t, r2TrainErr(mig, tr.train()))
				// The base build refuses this for a NOT NULL column that
				// completed inline; either way the contraction starts the same.
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
					require.NoError(t, r2ExecAs(t, db, r2New, newIns))
					clean = r4Step("complete", func() error { return err }) + " || " + r4Catalog(t, db) + " || " + r4Rows(t, db)
				})
			})
			t.Logf("clean (%d DDL statements): %s", total, clean)

			var bad []string
			for n := 1; n <= total; n++ {
				t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
					testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public", r3Opts(true), func(mig *roll.Roll, db *sql.DB) {
						apply(t, mig, db)
						r4ArmFault(t, db, n)
						first := r4Finish(mig)
						r4DisarmFault(t, db)
						between := "mid=" + r3Err(r2ExecAs(t, db, r2New, `UPDATE w SET id = id WHERE id = 1`))
						retry := r4Finish(mig)
						ins := r2ExecAs(t, db, r2New, newIns)
						got := r4Step("complete", func() error { return retry }) + " || " + r4Catalog(t, db) + " || " + r4Rows(t, db)
						if ins != nil {
							got += " newIns=" + r3Err(ins)
						}
						if got != clean || !strings.HasSuffix(between, "=ok") {
							bad = append(bad, fmt.Sprintf("n=%d first=%s %s retry=%s", n, r3Err(first), between, r3Err(retry)))
							t.Logf("n=%d first=%v %s\n  got:   %s\n  clean: %s", n, first, between, got, clean)
						}
					})
				})
			}
			t.Logf("SIG|%s|interrupted|BAD %d: %s", tn, len(bad), strings.Join(bad, " ;; "))
		})
	}
}

// ---------------------------------------------------------------------------
// Writers racing the contraction.
// ---------------------------------------------------------------------------

// TestTrainChainWritersRaceTheContraction: two waiting add_columns, then an alter
// and a rename on the same table. While the contraction drains them, the new
// revision inserts and updates through its version schema and a session on
// `public` updates rows. SIG reports the distinct write errors and whether
// any row the new revision wrote lost a value it gave.
func TestTrainChainWritersRaceTheContraction(t *testing.T) {
	t.Parallel()
	bigint := "bigint"
	trains := map[string]r4Train{
		"add a; add b reading a; alter x; rename c": {migs: r4Seq(
			r4One("01_add", &migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}}),
			r4One("02_add", &migrations.OpAddColumn{Table: "w", Up: "a * 10", Column: migrations.Column{Name: "b", Type: "integer", Nullable: true}}),
			r4One("03_alter", &migrations.OpAlterColumn{Table: "w", Column: "x", Type: &bigint, Up: "x", Down: "x"}),
			r4One("04_rename", &migrations.OpRenameColumn{Table: "w", From: "c", To: "d"}),
		)},
		"add a notnull; add b unique reading c; rename c; alter x": {migs: r4Seq(
			r4One("01_add", &migrations.OpAddColumn{Table: "w", Up: "x + 1", Column: migrations.Column{Name: "a", Type: "integer"}}),
			r4One("02_add", &migrations.OpAddColumn{Table: "w", Up: "c + id", Column: migrations.Column{Name: "b", Type: "integer", Nullable: true}}),
			r4One("03_rename", &migrations.OpRenameColumn{Table: "w", From: "c", To: "d"}),
			r4One("04_alter", &migrations.OpAlterColumn{Table: "w", Column: "x", Type: &bigint, Up: "x", Down: "x"}),
		)},
	}
	for tn, tr := range trains {
		t.Run(tn, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public", r3Opts(true), func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				r2Base(t, mig, `CREATE TABLE w (id integer PRIMARY KEY, x integer, c integer);
					INSERT INTO w SELECT g, g, g FROM generate_series(1, 20000) g`)
				require.NoError(t, r2TrainErr(mig, append(tr.migs(), r2Last())))

				stop := make(chan struct{})
				type result struct {
					errs  map[string]int
					wrote int
				}
				results := make(chan result, 8)
				writer := func(k int, path string, stmt func(i int) string) {
					res := result{errs: map[string]int{}}
					defer func() { results <- res }()
					conn, err := db.Conn(ctx)
					if err != nil {
						res.errs["conn: "+err.Error()]++
						return
					}
					defer conn.Close()
					if _, err := conn.ExecContext(ctx, "SET search_path TO "+path); err != nil {
						res.errs["set: "+err.Error()]++
						return
					}
					for i := 0; ; i++ {
						select {
						case <-stop:
							return
						default:
						}
						if _, err := conn.ExecContext(ctx, stmt(i)); err != nil {
							res.errs[r3Err(err)]++
						} else {
							res.wrote++
						}
					}
				}
				for k := 0; k < 3; k++ {
					go writer(k, r2New, func(i int) string {
						id := 100000 + k*1000000 + i
						return fmt.Sprintf(`INSERT INTO w (id, x, a, b) VALUES (%d, 5, -7, -8)`, id)
					})
				}
				go writer(3, r2New, func(i int) string { return fmt.Sprintf(`UPDATE w SET x = x + 1 WHERE id = %d`, 1+i%20000) })
				go writer(4, "public", func(i int) string { return fmt.Sprintf(`UPDATE w SET id = id WHERE id = %d`, 1+(i*7)%20000) })

				err := r4Finish(mig)
				close(stop)
				errs := map[string]int{}
				wrote := 0
				for k := 0; k < 5; k++ {
					res := <-results
					wrote += res.wrote
					for e, n := range res.errs {
						errs[e] += n
					}
				}
				list := make([]string, 0, len(errs))
				for e := range errs {
					list = append(list, e)
				}
				lost := r3TryStr(db, `SELECT count(*)::text FROM public.w WHERE id >= 100000 AND (a IS DISTINCT FROM -7 OR b IS DISTINCT FROM -8)`)
				t.Logf("wrote %d statements; errors: %v", wrote, errs)
				t.Logf("SIG|%s|race|complete=%s lostValues=%s errors=%d kinds=%v leftovers=%d", tn, r3Err(err), lost, len(errs), list, r2Leftovers(t, db, "public"))
			})
		})
	}
}

// Later migrations of the train that name a constraint a waiting add_column
// holds, or build one on its column.
func TestTrainChainConstraintsOfAWaitingColumn(t *testing.T) {
	skipUnlessTrainMatrix(t)
	t.Parallel()
	shapes := r4AddShapes()
	uq := func(name string, cols ...string) *migrations.Migration {
		up := map[string]string{}
		for _, c := range cols {
			up[c] = c
		}
		return r4One(name, &migrations.OpCreateConstraint{Name: "uq_new", Table: "w", Type: "unique", Columns: cols, Up: up, Down: up})
	}
	addB := r4One("02_add", &migrations.OpAddColumn{Table: "w", Up: "x + 200", Column: migrations.Column{Name: "b", Type: "integer"}})
	insAB := `INSERT INTO w (id, x, a, b) VALUES (4, 40, 99, 98)`
	r4RunTrains(t, map[string]r4Train{
		"notnull; constraint unique a":       {migs: r4Seq(r4Add("01_add", shapes["notnull"]), uq("02_uq", "a"))},
		"plain; constraint unique a":         {migs: r4Seq(r4Add("01_add", shapes["plain"]), uq("02_uq", "a"))},
		"notnull a; notnull b; unique a b":   {migs: r4Seq(r4Add("01_add", shapes["notnull"]), addB, uq("03_uq", "a", "b")), newIns: insAB},
		"[notnull a, notnull b]; unique a b": {migs: r4Seq(r4One("01_add", r4Add("x", shapes["notnull"]).Operations[0], addB.Operations[0]), uq("02_uq", "a", "b")), newIns: insAB},
		"nndefault; constraint unique a":     {migs: r4Seq(r4Add("01_add", shapes["nndefault"]), uq("02_uq", "a"))},
		"check; rename constraint":           {migs: r4Seq(r4Add("01_add", shapes["check"]), r4One("02_rename", &migrations.OpRenameConstraint{Table: "w", From: "a_pos", To: "a_positive"}))},
		"unique; rename constraint":          {migs: r4Seq(r4Add("01_add", shapes["unique"]), r4One("02_rename", &migrations.OpRenameConstraint{Table: "w", From: "a", To: "a_key"}))},
		"fk; rename constraint": {
			migs:   r4Seq(r4Add("01_add", shapes["fk"]), r4One("02_rename", &migrations.OpRenameConstraint{Table: "w", From: "fk_a", To: "fk_a_renamed"})),
			newIns: `INSERT INTO w (id, x, a) VALUES (4, 40, 2)`,
		},
		"unique; drop multi constraint a": {migs: r4Seq(r4Add("01_add", shapes["unique"]), r4One("02_drop", &migrations.OpDropMultiColumnConstraint{
			Table: "w", Name: "a",
			Up: map[string]string{"a": "a"}, Down: map[string]string{"a": "a"},
		}))},
		"unique; drop unique; alter type": {migs: r4Seq(r4Add("01_add", shapes["unique"]), r4One("02_drop", &migrations.OpDropConstraint{Table: "w", Name: "a", Up: "a", Down: "a"}),
			r4One("03_alter", &migrations.OpAlterColumn{Table: "w", Column: "a", Type: r4Ptr("bigint"), Up: "a", Down: "a"}))},
		"unique; table with fk to a; alter type": {migs: r4Seq(r4Add("01_add", shapes["unique"]),
			r4One("02_table", &migrations.OpCreateTable{Name: "t", Columns: []migrations.Column{
				{Name: "id", Type: "integer", Pk: true},
				{Name: "w_a", Type: "integer", Nullable: true, References: &migrations.ForeignKeyReference{Name: "fk_t_w", Table: "w", Column: "a"}},
			}}))},
	})
}

// Identifiers that need quoting, hold a double quote, or are multibyte and
// long enough that pgroll's derived names pass 63 bytes.
func TestTrainChainOddIdentifiers(t *testing.T) {
	skipUnlessTrainMatrix(t)
	t.Parallel()
	bigint := "bigint"
	type odd struct {
		base                     string
		migs                     func() []*migrations.Migration
		prevIns, newIns, prevUpd string
	}
	const (
		qt, qx, qa, qc = `Odd "T`, `X y`, `A "q`, `q"c`
		gt             = `πίνακας_δοκιμής_με_μακρύ_όνομα`
		ga             = `στήλη_με_πολύ_μακρύ_όνομα_εδώ`
		gb             = `δεύτερη_στήλη_με_μακρύ_όνομα`
	)
	trains := map[string]odd{
		"quoted": {
			base: `CREATE TABLE "Odd ""T" (id integer PRIMARY KEY, "X y" integer, "q""c" integer); INSERT INTO "Odd ""T" VALUES (1, 10, 1), (2, 20, 2)`,
			migs: r4Seq(
				r4One("01_add", &migrations.OpAddColumn{Table: qt, Up: `"X y" + 100`, Column: migrations.Column{Name: qa, Type: "integer"}}),
				r4One("02_rename", &migrations.OpRenameColumn{Table: qt, From: qx, To: "Z w"}),
				r4One("03_alter", &migrations.OpAlterColumn{Table: qt, Column: qa, Type: &bigint, Up: `"A ""q"`, Down: `"A ""q"`}),
				r4One("04_rename", &migrations.OpRenameColumn{Table: qt, From: qc, To: `r"d`}),
			),
			prevIns: `INSERT INTO "Odd ""T" (id, "X y", "q""c") VALUES (3, 30, 3)`,
			newIns:  `INSERT INTO "Odd ""T" (id, "Z w", "A ""q", "r""d") VALUES (4, 40, 99, 4)`,
			prevUpd: `UPDATE "Odd ""T" SET "X y" = 41 WHERE id = 4`,
		},
		"quoted table rename": {
			base: `CREATE TABLE "Odd ""T" (id integer PRIMARY KEY, "X y" integer, "q""c" integer); INSERT INTO "Odd ""T" VALUES (1, 10, 1), (2, 20, 2)`,
			migs: r4Seq(
				r4One("01_add", &migrations.OpAddColumn{Table: qt, Up: `"X y" + 100`, Column: migrations.Column{Name: qa, Type: "integer", Nullable: true, Unique: true}}),
				r4One("02_rename", &migrations.OpRenameTable{From: qt, To: `New "T`}),
				r4One("03_add", &migrations.OpAddColumn{Table: `New "T`, Up: `"A ""q" * 2`, Column: migrations.Column{Name: `B "q`, Type: "integer", Nullable: true}}),
			),
			prevIns: `INSERT INTO "Odd ""T" (id, "X y", "q""c") VALUES (3, 30, 3)`,
			newIns:  `INSERT INTO "New ""T" (id, "X y", "A ""q", "B ""q") VALUES (4, 40, 99, 98)`,
			prevUpd: `UPDATE "Odd ""T" SET "X y" = 41 WHERE id = 4`,
		},
		"multibyte": {
			base: fmt.Sprintf(`CREATE TABLE %q (id integer PRIMARY KEY, x integer, y integer); INSERT INTO %q VALUES (1, 10, 1), (2, 20, 2)`, gt, gt),
			migs: r4Seq(
				r4One("01_add", &migrations.OpAddColumn{Table: gt, Up: `x + 100`, Column: migrations.Column{Name: ga, Type: "integer"}}),
				r4One("02_rename", &migrations.OpRenameColumn{Table: gt, From: "x", To: "χ"}),
				r4One("03_add", &migrations.OpAddColumn{Table: gt, Up: fmt.Sprintf(`%q + "χ"`, ga), Column: migrations.Column{Name: gb, Type: "integer", Nullable: true, Unique: true}}),
				r4One("04_rename", &migrations.OpRenameColumn{Table: gt, From: "y", To: "ψ"}),
			),
			prevIns: fmt.Sprintf(`INSERT INTO %q (id, x, y) VALUES (3, 30, 3)`, gt),
			newIns:  fmt.Sprintf(`INSERT INTO %q (id, "χ", "ψ", %q, %q) VALUES (4, 40, 4, 99, 98)`, gt, ga, gb),
			prevUpd: fmt.Sprintf(`UPDATE %q SET x = 41 WHERE id = 4`, gt),
		},
		"swap renames": {
			base: `CREATE TABLE w (id integer PRIMARY KEY, x integer, y integer); INSERT INTO w VALUES (1, 10, 1), (2, 20, 2)`,
			migs: r4Seq(
				r4One("01_add", &migrations.OpAddColumn{Table: "w", Up: `x + y * 1000`, Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}}),
				r4One("02_rename", &migrations.OpRenameColumn{Table: "w", From: "x", To: "t"}),
				r4One("03_rename", &migrations.OpRenameColumn{Table: "w", From: "y", To: "x"}),
				r4One("04_rename", &migrations.OpRenameColumn{Table: "w", From: "t", To: "y"}),
				r4One("05_add", &migrations.OpAddColumn{Table: "w", Up: `x + y * 1000`, Column: migrations.Column{Name: "b", Type: "integer", Nullable: true}}),
			),
			prevIns: `INSERT INTO w (id, x, y) VALUES (3, 30, 3)`,
			newIns:  `INSERT INTO w (id, x, y, a, b) VALUES (4, 4, 40, 99, 98)`,
			prevUpd: `UPDATE w SET x = 41 WHERE id = 4`,
		},
	}
	ctx := context.Background()
	for tn, tr := range trains {
		n := len(tr.migs())
		ends := map[string]func(mig *roll.Roll, train []*migrations.Migration) string{
			"contract": func(mig *roll.Roll, _ []*migrations.Migration) string {
				return r4Step("complete", func() error { return r4Finish(mig) })
			},
			"revert": func(mig *roll.Roll, _ []*migrations.Migration) string {
				return r4Step("revert", func() error { _, err := mig.Revert(ctx); return err })
			},
			"rollback revert": func(mig *roll.Roll, _ []*migrations.Migration) string {
				return r4Step("rollback", func() error { return mig.Rollback(ctx) }) + " " + r4Step("revert", func() error { _, err := mig.Revert(ctx); return err })
			},
			"sealed one shot": func(mig *roll.Roll, _ []*migrations.Migration) string {
				return r4Step("complete", func() error { return r4Finish(mig) }) + " " + r4Step("sealed", func() error { _, err := mig.RevertSealed(ctx, "00_base", nil); return err })
			},
			"sealed expand only": func(mig *roll.Roll, _ []*migrations.Migration) string {
				out := r4Step("complete", func() error { return r4Finish(mig) })
				out += " " + r4Step("sealed", func() error { _, err := mig.RevertSealed(ctx, "00_base", nil, roll.WithExpandOnly()); return err })
				return out + " " + r4Step("finish", func() error { return r4Finish(mig) })
			},
		}
		for i := 0; i < n; i++ {
			ends[fmt.Sprintf("revert to %d then contract", i+1)] = func(mig *roll.Roll, train []*migrations.Migration) string {
				return r4Step("revertTo", func() error { _, err := mig.Revert(ctx, roll.WithRevertTo(train[i].Name)); return err }) + " " +
					r4Step("complete", func() error { return r4Finish(mig) })
			}
		}
		for en, end := range ends {
			t.Run(tn+"/"+en, func(t *testing.T) {
				t.Parallel()
				r4RunChain(t, "odd "+tn+"|"+en, true, tr.base, func(t *testing.T, mig *roll.Roll, db *sql.DB) string {
					train := append(tr.migs(), r2Last())
					out := r4Step("train", func() error { return r2TrainErr(mig, train) })
					out += " prevIns=" + r3Err(r2ExecAs(t, db, r2Prev, tr.prevIns))
					out += " newIns=" + r3Err(r2ExecAs(t, db, r2New, tr.newIns))
					out += " prevUpd=" + r3Err(r2ExecAs(t, db, r2Prev, tr.prevUpd))
					out += " mid{" + r4Rows(t, db) + "}"
					return out + " " + end(mig, train)
				})
			})
		}
	}
}

// A waiting unique add_column, then the drop of its unique constraint. The
// constraint exists only after the contraction, so the drop is refused
// before it starts, and the train stays as it was.
func TestTrainChainDropOfAWaitingColumnsUniqueConstraintIsRefused(t *testing.T) {
	t.Parallel()
	drops := map[string]migrations.Operation{
		"drop_constraint":             &migrations.OpDropConstraint{Table: "w", Name: "a", Up: "a", Down: "a"},
		"drop_multicolumn_constraint": &migrations.OpDropMultiColumnConstraint{Table: "w", Name: "a", Up: map[string]string{"a": "a"}, Down: map[string]string{"a": "a"}},
	}
	for name, drop := range drops {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public", r3Opts(true), func(mig *roll.Roll, db *sql.DB) {
				r2Base(t, mig, r4Base)
				err := r2TrainErr(mig, []*migrations.Migration{r4Add("01_add", r4AddShapes()["unique"]), r4One("02_drop", drop), r2Last()})
				require.ErrorContains(t, err, "exists only after `pgroll complete`")
				assert.NoError(t, r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x) VALUES (3, 30)`), "the previous revision still writes")
			})
		})
	}
}

// ---------------------------------------------------------------------------
// Findings, as assertions. Each passes on the base build (6b2e5ea).
// ---------------------------------------------------------------------------

func r4NotNull(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	var notNull bool
	require.NoError(t, db.QueryRow(`SELECT attnotnull FROM pg_attribute WHERE attrelid = ('public.' || quote_ident($1))::regclass AND attname = $2`, table, column).Scan(&notNull))
	return notNull
}

// F1. alter_column sets x NOT NULL; a later migration of the same train
// alters x again. No add_column anywhere. The column must end NOT NULL.
func TestTrainChainFindingSetNotNullThenAnotherAlterOfTheColumn(t *testing.T) {
	t.Parallel()
	f, bigint := false, "bigint"
	second := map[string]*migrations.OpAlterColumn{
		"type":   {Table: "w", Column: "x", Type: &bigint, Up: "x", Down: "x"},
		"check":  {Table: "w", Column: "x", Check: &migrations.CheckConstraint{Name: "x_pos", Constraint: "x >= 0"}, Up: "x", Down: "x"},
		"unique": {Table: "w", Column: "x", Unique: &migrations.UniqueConstraint{Name: "uq_x"}, Up: "x", Down: "x"},
	}
	modes := map[string]func(mig *roll.Roll, train []*migrations.Migration) error{
		"migrate then complete": r2TrainErr,
		"migrate --complete":    r3CompleteTrainErr,
	}
	for sn, op := range second {
		for mn, apply := range modes {
			t.Run(sn+"/"+mn, func(t *testing.T) {
				t.Parallel()
				testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
					r2Base(t, mig, r2W)
					require.NoError(t, apply(mig, []*migrations.Migration{
						r4One("01_not_null", &migrations.OpAlterColumn{Table: "w", Column: "x", Nullable: &f, Up: "COALESCE(x, 0)", Down: "x"}),
						r4One("02_again", op),
						r2Last(),
					}))
					require.NoError(t, r4Finish(mig))
					assert.True(t, r4NotNull(t, db, "w", "x"), "x is NOT NULL after a train that set it NOT NULL")
					assert.Error(t, r2ExecAs(t, db, "public", `INSERT INTO w (id, x) VALUES (9, NULL)`), "a NULL x is refused")
				})
			})
		}
	}
}

// F2. rename_column then drop_column (with `down`) of the renamed column in
// one train: the new revision must be able to write the table.
func TestTrainChainFindingRenameThenDropOfAColumn(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		r2Base(t, mig, `CREATE TABLE w (id integer PRIMARY KEY, x integer, c integer); INSERT INTO w VALUES (1, 10, 1)`)
		require.NoError(t, r2TrainErr(mig, []*migrations.Migration{
			r4One("01_rename", &migrations.OpRenameColumn{Table: "w", From: "c", To: "d"}),
			r4One("02_drop", &migrations.OpDropColumn{Table: "w", Column: "d", Down: "x"}),
			r2Last(),
		}))
		assert.NoError(t, r2ExecAs(t, db, r2New, `INSERT INTO w (id, x) VALUES (2, 20)`), "the new revision inserts")
		assert.NoError(t, r2ExecAs(t, db, r2New, `UPDATE w SET x = 11 WHERE id = 1`), "the new revision updates")
		assert.NoError(t, r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x, c) VALUES (3, 30, 3)`), "the previous revision inserts")
	})
}

// F3. The shape create_constraint is meant for: a constraint over columns
// the same train adds. The columns are NOT NULL with `up`, so they wait for
// the contraction, which must then succeed.
func TestTrainChainFindingConstraintOverWaitingNotNullColumns(t *testing.T) {
	t.Parallel()
	add := func(name, column, up string) *migrations.Migration {
		return r4One(name, &migrations.OpAddColumn{Table: "w", Up: up, Column: migrations.Column{Name: column, Type: "integer"}})
	}
	constraints := map[string]*migrations.OpCreateConstraint{
		"unique a b": {Name: "uq_ab", Table: "w", Type: "unique", Columns: []string{"a", "b"}, Up: map[string]string{"a": "a", "b": "b"}, Down: map[string]string{"a": "a", "b": "b"}},
		"check a":    {Name: "a_pos", Table: "w", Type: "check", Columns: []string{"a"}, Check: r4Ptr("a > 0"), Up: map[string]string{"a": "a"}, Down: map[string]string{"a": "a"}},
	}
	for cn, c := range constraints {
		t.Run(cn, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				r2Base(t, mig, r2W)
				require.NoError(t, r2TrainErr(mig, []*migrations.Migration{add("01_add_a", "a", "x + 100"), add("02_add_b", "b", "x + 200"), r4One("03_constraint", c), r2Last()}))
				err := r2Safely(func() error { return mig.Complete(ctx) })
				assert.NoError(t, err, "the contraction")
				if err != nil {
					assert.NoError(t, r2Safely(func() error { return mig.Complete(ctx) }), "the contraction, retried")
					t.Logf("history: %s", r4History(t, mig, db))
				}
				assert.Zero(t, r2Leftovers(t, db, "public"), "pgroll artifacts left")
			})
		})
	}
}

// A later raw SQL migration of the train changes what a waiting add_column
// declared for its column. What the raw SQL set must stand.
func TestTrainChainFindingRawSQLOnAWaitingColumnIsUndone(t *testing.T) {
	t.Parallel()
	shapes := r4AddShapes()
	def := func(t *testing.T, db *sql.DB, schema string) string {
		return r3Str(t, db, fmt.Sprintf(`SELECT column_default FROM information_schema.columns WHERE table_schema = '%s' AND table_name = 'w' AND column_name = 'a'`, schema))
	}
	t.Run("drop default", func(t *testing.T) {
		t.Parallel()
		testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
			r2Base(t, mig, r4Base)
			require.NoError(t, r2TrainErr(mig, []*migrations.Migration{r4Add("01_add", shapes["default"]), r2Raw("02_raw", `ALTER TABLE w ALTER COLUMN a DROP DEFAULT`), r2Last()}))
			assert.Equal(t, "<NULL>", def(t, db, "public"), "the column's default before the contraction")
			assert.Equal(t, "<NULL>", def(t, db, r2New), "the default the new revision's view gives the column")
			require.NoError(t, r2ExecAs(t, db, r2New, `INSERT INTO w (id, x) VALUES (4, 40)`))
			assert.Equal(t, "<NULL>", r3Str(t, db, `SELECT a::text FROM w WHERE id = 4`), "a row the new revision inserts without the column")
			require.NoError(t, r4Finish(mig))
			assert.Equal(t, "<NULL>", def(t, db, "public"), "the column's default after the contraction")
		})
	})
	t.Run("set default", func(t *testing.T) {
		t.Parallel()
		testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
			r2Base(t, mig, r4Base)
			require.NoError(t, r2TrainErr(mig, []*migrations.Migration{r4Add("01_add", shapes["nndefault"]), r2Raw("02_raw", `ALTER TABLE w ALTER COLUMN a SET DEFAULT 42`), r2Last()}))
			assert.Equal(t, "42", def(t, db, "public"), "the column's default before the contraction")
			assert.Equal(t, "42", def(t, db, r2New), "the default the new revision's view gives the column")
			require.NoError(t, r2ExecAs(t, db, r2New, `INSERT INTO w (id, x) VALUES (4, 40)`))
			assert.Equal(t, "42", r3Str(t, db, `SELECT a::text FROM w WHERE id = 4`), "a row the new revision inserts without the column")
			require.NoError(t, r4Finish(mig))
			require.NoError(t, r2ExecAs(t, db, r2New, `INSERT INTO w (id, x) VALUES (5, 50)`))
			assert.Equal(t, "42", r3Str(t, db, `SELECT a::text FROM w WHERE id = 5`), "a row the new revision inserts without the column, after the contraction")
		})
	})
	t.Run("drop check", func(t *testing.T) {
		t.Parallel()
		testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
			r2Base(t, mig, r4Base)
			require.NoError(t, r2TrainErr(mig, []*migrations.Migration{r4Add("01_add", shapes["check"]), r2Raw("02_raw", `ALTER TABLE w DROP CONSTRAINT a_pos`), r2Last()}))
			assert.NoError(t, r4Finish(mig), "the contraction")
		})
	})
}

// A migration that creates a table and then fails: Start rolls it back. The
// same after an earlier migration of the train that also created a table
// waits for the contraction.
func TestTrainChainCreateTableRolledBackAfterALaterFailure(t *testing.T) {
	t.Parallel()
	table := func(name string) *migrations.OpCreateTable {
		return &migrations.OpCreateTable{Name: name, Columns: []migrations.Column{{Name: "id", Type: "integer", Pk: true}, {Name: "v", Type: "text", Nullable: true}}}
	}
	// The backfill of a unique column whose `up` repeats a value fails.
	failing := &migrations.OpAddColumn{Table: "w", Up: "1", Column: migrations.Column{Name: "dup", Type: "integer", Nullable: true, Unique: true}}
	trains := map[string][]*migrations.Migration{
		"alone":                      {r4One("01_m", table("t"), failing)},
		"after a waiting create":     {r4One("01_m", table("t"), &migrations.OpAddColumn{Table: "w", Up: "x + 100", Column: r4AddShapes()["plain"]}), r4One("02_m", table("u"), failing)},
		"recreating a dropped table": {r4One("01_m", table("t"), &migrations.OpAddColumn{Table: "w", Up: "x + 100", Column: r4AddShapes()["plain"]}), r4One("02_drop", &migrations.OpDropTable{Name: "t"}), r4One("03_m", table("t"), failing)},
	}
	for tn, train := range trains {
		t.Run(tn, func(t *testing.T) {
			t.Parallel()
			r4RunChain(t, "create table rollback "+tn+"|x", true, r4Base, func(t *testing.T, mig *roll.Roll, db *sql.DB) string {
				ctx := context.Background()
				out := r4Step("train", func() error { return r2TrainErr(mig, append(train, r2Last())) })
				out += " after{" + r4Catalog(t, db) + " || " + r4History(t, mig, db) + "}"
				out += " prevIns=" + r3Err(r2ExecAs(t, db, r2Prev, `INSERT INTO w (id, x) VALUES (3, 30)`))
				return out + " " + r4Step("revert", func() error { _, err := mig.Revert(ctx); return err })
			})
		})
	}
}

// Final-schema parity: one migration of add_column operations, alone, as an
// earlier migration of a train, and as its last, completed every way.
func TestTrainChainSingleAdds(t *testing.T) {
	skipUnlessTrainMatrix(t)
	t.Parallel()
	zero, seven, now, uuid, comment := "0", "7", "now()", "gen_random_uuid()", "a comment"
	col := func(c migrations.Column) migrations.Column {
		c.Name = "a"
		if c.Type == "" {
			c.Type = "integer"
		}
		return c
	}
	one := func(up string, c migrations.Column) migrations.Operations {
		return migrations.Operations{&migrations.OpAddColumn{Table: "w", Up: up, Column: col(c)}}
	}
	shapes := map[string]migrations.Operations{
		"nullable up":                  one("x + 100", migrations.Column{Nullable: true}),
		"notnull up":                   one("x + 100", migrations.Column{}),
		"unique up":                    one("x + 100", migrations.Column{Nullable: true, Unique: true}),
		"notnull unique check comment": one("x + 100", migrations.Column{Unique: true, Comment: &comment, Check: &migrations.CheckConstraint{Name: "a_pos", Constraint: "a > 0"}}),
		"fk up":                        one("1", migrations.Column{Nullable: true, References: &migrations.ForeignKeyReference{Name: "fk_a", Table: "p", Column: "id"}}),
		"notnull fk on delete":         one("1", migrations.Column{References: &migrations.ForeignKeyReference{Name: "fk_a", Table: "p", Column: "id", OnDelete: "CASCADE"}}),
		"notnull default up":           one("x * 2", migrations.Column{Default: &zero}),
		"notnull default up unique":    one("x * 2", migrations.Column{Default: &zero, Unique: true}),
		"nullable default up":          one("x * 2", migrations.Column{Nullable: true, Default: &seven}),
		"default eq up":                one("7", migrations.Column{Nullable: true, Default: &seven}),
		"volatile default":             one(now, migrations.Column{Type: "timestamptz", Nullable: true, Default: &now}),
		"notnull volatile default":     one(uuid, migrations.Column{Type: "uuid", Default: &uuid}),
		"no up":                        one("", migrations.Column{Nullable: true}),
		"no up notnull default":        one("", migrations.Column{Default: &zero}),
		"no up unique check":           one("", migrations.Column{Nullable: true, Unique: true, Check: &migrations.CheckConstraint{Name: "a_pos", Constraint: "a > 0"}}),
		"text up":                      one("'v' || x", migrations.Column{Type: "text"}),
		"varchar up":                   one("left('v' || x, 10)", migrations.Column{Type: "varchar(10)", Nullable: true, Unique: true}),
		"two adds": {
			&migrations.OpAddColumn{Table: "w", Up: "x + 100", Column: migrations.Column{Name: "a", Type: "integer"}},
			&migrations.OpAddColumn{Table: "w", Up: "a * 2", Column: migrations.Column{Name: "b", Type: "integer", Unique: true}},
		},
		"add and unique index": {
			&migrations.OpAddColumn{Table: "w", Up: "x + 100", Column: migrations.Column{Name: "a", Type: "integer"}},
			&migrations.OpCreateIndex{Name: "ix_w_a", Table: "w", Unique: true, Columns: []migrations.IndexField{{Column: "a"}}},
		},
		"add and partial index": {
			&migrations.OpAddColumn{Table: "w", Up: "x + 100", Column: migrations.Column{Name: "a", Type: "integer", Nullable: true}},
			&migrations.OpCreateIndex{Name: "ix_w_a", Table: "w", Predicate: "a > 0", Columns: []migrations.IndexField{{Column: "a"}, {Column: "x"}}},
		},
		"create table and add to it": {
			&migrations.OpCreateTable{Name: "t", Columns: []migrations.Column{{Name: "id", Type: "integer", Pk: true}, {Name: "v", Type: "integer", Nullable: true}}},
			&migrations.OpAddColumn{Table: "t", Up: "id", Column: migrations.Column{Name: "c", Type: "integer", Unique: true}},
			&migrations.OpAddColumn{Table: "w", Up: "x + 100", Column: migrations.Column{Name: "a", Type: "integer"}},
		},
	}
	newIns := func(sn string) string {
		switch {
		case strings.Contains(sn, "fk"):
			return `INSERT INTO w (id, x, a) VALUES (4, 40, 2)`
		case sn == "volatile default":
			return `INSERT INTO w (id, x, a) VALUES (4, 40, '2020-01-01')`
		case sn == "notnull volatile default":
			return `INSERT INTO w (id, x, a) VALUES (4, 40, '00000000-0000-0000-0000-000000000001')`
		case strings.HasPrefix(sn, "text"), strings.HasPrefix(sn, "varchar"):
			return `INSERT INTO w (id, x, a) VALUES (4, 40, 'n')`
		case sn == "two adds":
			return `INSERT INTO w (id, x, a, b) VALUES (4, 40, 99, 98)`
		}
		return ""
	}
	ends := []string{"contract", "contract quiet", "revert", "rollback revert", "complete train", "no version schemas", "one by one", "sealed one shot", "sealed expand only", "revert to 1 then contract"}
	trains := map[string]r4Train{}
	for sn, ops := range shapes {
		trains["mid "+sn] = r4Train{migs: r4Seq(r4One("01_add", ops...)), newIns: newIns(sn), ends: ends}
		trains["leaf "+sn] = r4Train{
			whole: true, migs: r4Seq(r2Raw("01_first", "SELECT 1"), r4One("09_last", ops...)), newIns: newIns(sn),
			ends: []string{"contract", "contract quiet", "revert", "complete train", "no version schemas", "sealed one shot"},
		}
	}
	r4RunTrains(t, trains)
}

// More trains over one existing column, without an add_column: a set NOT
// NULL followed by the other duplicating operations, and a sealed revert
// whose inverse train sets NOT NULL and then alters the column again.
func TestTrainChainAltersOfAnExistingColumnMore(t *testing.T) {
	skipUnlessTrainMatrix(t)
	t.Parallel()
	bigint, f, tr := "bigint", false, true
	alter := func(name string, o migrations.OpAlterColumn) *migrations.Migration {
		o.Table, o.Column, o.Up, o.Down = "w", "x", "x", "x"
		return r4One(name, &o)
	}
	notNull := r4One("01_nn", &migrations.OpAlterColumn{Table: "w", Column: "x", Nullable: &f, Up: "COALESCE(x, 0)", Down: "x"})
	ins := `INSERT INTO w (id, x) VALUES (4, 40)`
	checked := `CREATE TABLE p (id integer PRIMARY KEY); INSERT INTO p VALUES (1), (2), (99);
		CREATE TABLE w (id integer PRIMARY KEY, x integer, CONSTRAINT x_small CHECK (x < 1000000), CONSTRAINT uq_x UNIQUE (x)); INSERT INTO w VALUES (1, 10), (2, 20)`
	nnBase := `CREATE TABLE p (id integer PRIMARY KEY); INSERT INTO p VALUES (1), (2), (99);
		CREATE TABLE w (id integer PRIMARY KEY, x integer NOT NULL); INSERT INTO w VALUES (1, 10), (2, 20)`
	sealed := []string{"contract quiet", "complete train", "sealed one shot", "sealed expand only", "one by one"}
	r4RunTrains(t, map[string]r4Train{
		"notnull; drop check": {base: checked, newIns: ins, migs: r4Seq(notNull, r4One("02_drop", &migrations.OpDropConstraint{Table: "w", Name: "x_small", Up: "x", Down: "x"}))},
		"notnull; drop unique": {base: checked, newIns: ins, migs: r4Seq(notNull, r4One("02_drop", &migrations.OpDropMultiColumnConstraint{
			Table: "w", Name: "uq_x",
			Up: map[string]string{"x": "x"}, Down: map[string]string{"x": "x"},
		}))},
		"notnull; default": {newIns: ins, migs: r4Seq(notNull, r4One("02_default", &migrations.OpAlterColumn{Table: "w", Column: "x", Default: nullable.NewNullableWithValue("5"), Up: "x", Down: "x"}))},
		"notnull; fk":      {newIns: `INSERT INTO w (id, x) VALUES (4, 2)`, base: `CREATE TABLE p (id integer PRIMARY KEY); INSERT INTO p VALUES (1), (2), (10), (11), (20), (21), (30), (41), (99); CREATE TABLE w (id integer PRIMARY KEY, x integer); INSERT INTO w VALUES (1, 10), (2, 20)`, migs: r4Seq(notNull, alter("02_fk", migrations.OpAlterColumn{References: &migrations.ForeignKeyReference{Name: "fk_x", Table: "p", Column: "id"}}))},
		"type; nullable":   {base: nnBase, newIns: ins, ends: sealed, migs: r4Seq(alter("01_type", migrations.OpAlterColumn{Type: &bigint}), alter("02_null", migrations.OpAlterColumn{Nullable: &tr}))},
		"check; nullable":  {base: nnBase, newIns: ins, ends: sealed, migs: r4Seq(alter("01_check", migrations.OpAlterColumn{Check: &migrations.CheckConstraint{Name: "x_pos", Constraint: "x >= 0"}}), alter("02_null", migrations.OpAlterColumn{Nullable: &tr}))},
		"unique; nullable": {base: nnBase, newIns: ins, ends: sealed, migs: r4Seq(alter("01_uq", migrations.OpAlterColumn{Unique: &migrations.UniqueConstraint{Name: "uq_x"}}), alter("02_null", migrations.OpAlterColumn{Nullable: &tr}))},
	})
}

// F8. One migration, started and completed: an add_column whose `up` reads a
// column with a double quote in its name.
func TestTrainChainFindingUpReadsAColumnWithAQuoteInItsName(t *testing.T) {
	t.Parallel()
	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		r2Base(t, mig, `CREATE TABLE w (id integer PRIMARY KEY, "a""q" integer); INSERT INTO w VALUES (1, 10)`)
		err := mig.Start(ctx, r4One("01_add", &migrations.OpAddColumn{Table: "w", Up: `"a""q" + 1`, Column: migrations.Column{Name: "b", Type: "integer", Nullable: true}}), backfill.NewConfig())
		require.NoError(t, err, "start")
		require.NoError(t, mig.Complete(ctx))
		assert.Equal(t, "11", r3Str(t, db, `SELECT b::text FROM w WHERE id = 1`))
	})
}

// skipUnlessTrainMatrix skips a matrix of trains unless PGROLL_TRAIN_MATRIX
// is set. A matrix logs one signature line per case (SIG, CAT, ROW) for
// comparison with another build's, and every case takes a database that the
// container keeps, so the largest run one shape at a time
// (-run 'TestTrainChainChains/^unique$').
func skipUnlessTrainMatrix(t *testing.T) {
	t.Helper()
	if os.Getenv("PGROLL_TRAIN_MATRIX") == "" {
		t.Skip("set PGROLL_TRAIN_MATRIX=1 to run the train matrix")
	}
}
