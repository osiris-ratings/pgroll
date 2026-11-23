// SPDX-License-Identifier: Apache-2.0

package roll_test

// Trains that stop part-way, are resumed, reverted or recovered, and the
// order a write crosses a train's triggers in.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
	"github.com/xataio/pgroll/pkg/schema"
)

const r8Prev = "public_00_base"

func r8Raw(name, up string) *migrations.Migration {
	return &migrations.Migration{
		Name:       name,
		Operations: migrations.Operations{&migrations.OpRawSQL{Up: up, Down: "SELECT 1"}},
	}
}

func r8One(name string, ops ...migrations.Operation) *migrations.Migration {
	return &migrations.Migration{Name: name, Operations: ops}
}

func r8Base(t *testing.T, mig *roll.Roll, up string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, mig.Start(ctx, r8Raw("00_base", up), backfill.NewConfig()))
	require.NoError(t, mig.Complete(ctx))
}

func r8Safely(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("PANIC: %v", r)
		}
	}()
	return fn()
}

// r8MidErr applies one earlier migration of a train whose last is final.
func r8MidErr(mig *roll.Roll, m, final *migrations.Migration) error {
	ctx := context.Background()
	return r8Safely(func() error {
		if err := mig.Start(ctx, m, backfill.NewConfig(), r8MidOpts(mig, final)...); err != nil {
			return fmt.Errorf("start %s: %w", m.Name, err)
		}
		opt := roll.WithSkipSchemaDrop()
		if m.CompleteMustBeDeferred() {
			opt = roll.WithDeferComplete()
		}
		if err := mig.Complete(ctx, opt); err != nil {
			return fmt.Errorf("complete %s: %w", m.Name, err)
		}
		return nil
	})
}

// r8TrainErr applies a train the way `pgroll migrate` does without --complete.
func r8TrainErr(mig *roll.Roll, train []*migrations.Migration) error {
	ctx := context.Background()
	final := train[len(train)-1]
	for _, m := range train[:len(train)-1] {
		if err := r8MidErr(mig, m, final); err != nil {
			return err
		}
	}
	return r8Safely(func() error {
		if err := mig.Start(ctx, final, backfill.NewConfig(), r8LeafOpts(mig, final)...); err != nil {
			return fmt.Errorf("start %s: %w", final.Name, err)
		}
		return nil
	})
}

func r8ExecAs(t *testing.T, db *sql.DB, searchPath, stmt string) error {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.ExecContext(ctx, "SET search_path TO "+searchPath)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, stmt)
	return err
}

func r8Str(db *sql.DB, query string) string {
	var v sql.NullString
	if err := db.QueryRow(query).Scan(&v); err != nil {
		return "ERR:" + r8Short(err)
	}
	if !v.Valid {
		return "NULL"
	}
	return v.String
}

func r8Short(err error) string {
	if err == nil {
		return "ok"
	}
	s := err.Error()
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 150 {
		s = s[:150]
	}
	return s
}

func r8Alter(name, table, column, check, up, down string) *migrations.Migration {
	return r8One(name, &migrations.OpAlterColumn{
		Table: table, Column: column,
		Check: &migrations.CheckConstraint{Name: check, Constraint: column + " <> -99999"},
		Up:    up, Down: down,
	})
}

func r8Index(name, table, column string) *migrations.Migration {
	return r8One(name, &migrations.OpCreateIndex{
		Name: "ix_" + name, Table: table, Columns: []migrations.IndexField{{Column: column}},
	})
}

func r8Triggers(db *sql.DB, table string) string {
	return r8Str(db, fmt.Sprintf(`SELECT coalesce(string_agg(tgname || '>' || p.proname, ' ' ORDER BY tgname), '')
		FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid
		WHERE tgrelid = 'public.%s'::regclass AND NOT tgisinternal`, table))
}

func r8Target(db *sql.DB) string {
	return r8Str(db, `SELECT coalesce((SELECT prosrc FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public' AND proname = '_pgroll_train_target'), 'none')`)
}

func r8Leftovers(db *sql.DB) string {
	return r8Str(db, `SELECT coalesce(string_agg(x, ',' ORDER BY x), 'none') FROM (
		SELECT 'fn:' || proname AS x FROM pg_proc WHERE proname LIKE '\_pgroll\_%'
		UNION ALL SELECT 'col:' || table_name || '.' || column_name FROM information_schema.columns
			WHERE table_schema = 'public' AND column_name LIKE '\_pgroll\_%') s`)
}

// ---------------------------------------------------------------------------
// Trigger order across three migrations altering one column.
// ---------------------------------------------------------------------------

// Three alters of one column in a train, with `up` and `down` that do not
// commute. The previous revision's write must cross them first to last, the
// new revision's last to first, as if each had been contracted on its own.
func TestTrainRecoveryChainOfThreeAltersCrossesInOrder(t *testing.T) {
	t.Parallel()

	for name, withLater := range map[string]bool{"a later migration is last": true, "the third alter is last": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				r8Base(t, mig, `CREATE TABLE u (id integer PRIMARY KEY, v integer); INSERT INTO u VALUES (1, 5)`)
				train := []*migrations.Migration{
					r8Alter("01_plus", "u", "v", "c1", "v + 1", "v - 1"),
					r8Alter("02_times", "u", "v", "c2", "v * 2", "v / 2"),
					r8Alter("03_hundred", "u", "v", "c3", "v + 100", "v - 100"),
				}
				if withLater {
					train = append(train, r8Index("09_later", "u", "id"))
				}
				require.NoError(t, r8TrainErr(mig, train))
				next := "public_" + train[len(train)-1].Name
				t.Logf("triggers: %s", r8Triggers(db, "u"))

				require.NoError(t, r8ExecAs(t, db, r8Prev, `INSERT INTO u (id, v) VALUES (2, 5)`))
				require.NoError(t, r8ExecAs(t, db, next, `INSERT INTO u (id, v) VALUES (3, 112)`))
				assert.Equal(t, "112", r8Str(db, `SELECT v FROM `+next+`.u WHERE id = 1`), "backfilled row, new revision")
				assert.Equal(t, "112", r8Str(db, `SELECT v FROM `+next+`.u WHERE id = 2`), "previous revision's insert, new revision")
				assert.Equal(t, "5", r8Str(db, `SELECT v FROM `+r8Prev+`.u WHERE id = 3`), "new revision's insert, previous revision")

				require.NoError(t, r8ExecAs(t, db, r8Prev, `UPDATE u SET v = 7 WHERE id = 3`))
				require.NoError(t, r8ExecAs(t, db, next, `UPDATE u SET v = 212 WHERE id = 2`))
				assert.Equal(t, "116", r8Str(db, `SELECT v FROM `+next+`.u WHERE id = 3`), "previous revision's update, new revision")
				assert.Equal(t, "55", r8Str(db, `SELECT v FROM `+r8Prev+`.u WHERE id = 2`), "new revision's update, previous revision")

				require.NoError(t, mig.Complete(ctx))
				assert.Equal(t, "1:112 2:212 3:116", r8Str(db, `SELECT string_agg(id || ':' || v, ' ' ORDER BY id) FROM public.u`))
				assert.Equal(t, "none", r8Leftovers(db))
			})
		})
	}
}

// Positions after a bounded revert removes the tail of a train, and after a
// Start that failed and was rolled back: the migrations started next must
// still sort after the one that stayed.
func TestTrainRecoveryPositionsAfterABoundedRevertAndAFailedStart(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		r8Base(t, mig, `CREATE TABLE u (id integer PRIMARY KEY, v integer); INSERT INTO u VALUES (1, 5)`)
		require.NoError(t, r8TrainErr(mig, []*migrations.Migration{
			r8Alter("01_plus", "u", "v", "c1", "v + 1", "v - 1"),
			r8Alter("02_times", "u", "v", "c2", "v * 2", "v / 2"),
			r8Index("09_later", "u", "id"),
		}))
		before := r8Triggers(db, "u")

		_, err := mig.Revert(ctx, roll.WithRevertTo("01_plus"))
		require.NoError(t, err)
		t.Logf("after the bounded revert: target %s, triggers %s", r8Target(db), r8Triggers(db, "u"))

		// A pod on the new leaf and one on the previous schema.
		require.NoError(t, r8ExecAs(t, db, "public_01_plus", `INSERT INTO u (id, v) VALUES (2, 50)`))
		assert.Equal(t, "49", r8Str(db, `SELECT v FROM `+r8Prev+`.u WHERE id = 2`))

		// The next train: its first Start fails in the backfill and is rolled back.
		final := r8Index("19_later", "u", "id")
		bad := r8Alter("12_bad", "u", "v", "c2", "v / 0", "v")
		require.Error(t, r8MidErr(mig, bad, final))
		t.Logf("after the failed start: target %s, triggers %s", r8Target(db), r8Triggers(db, "u"))

		require.NoError(t, r8TrainErr(mig, []*migrations.Migration{
			r8Alter("12_tens", "u", "v", "c2", "v * 10", "v / 10"),
			final,
		}))
		after := r8Triggers(db, "u")
		t.Logf("before %s\nafter %s", before, after)

		require.NoError(t, r8ExecAs(t, db, r8Prev, `INSERT INTO u (id, v) VALUES (3, 5)`))
		require.NoError(t, r8ExecAs(t, db, "public_19_later", `INSERT INTO u (id, v) VALUES (4, 60)`))
		assert.Equal(t, "60", r8Str(db, `SELECT v FROM public_19_later.u WHERE id = 3`))
		assert.Equal(t, "5", r8Str(db, `SELECT v FROM `+r8Prev+`.u WHERE id = 4`))

		require.NoError(t, mig.Complete(ctx))
		assert.Equal(t, "none", r8Leftovers(db))
	})
}

// ---------------------------------------------------------------------------
// The target function across a re-point, in one long-lived session.
// ---------------------------------------------------------------------------

// A session that wrote through a train's trigger before the target was
// re-pointed must follow the new target on its next write: the trigger
// function's cached plan must not keep the old one.
func TestTrainRecoveryLongLivedSessionFollowsARepoint(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		r8Base(t, mig, `CREATE TABLE u (id integer PRIMARY KEY, v integer); INSERT INTO u VALUES (1, 5)`)

		abandoned := r8Index("09_later", "u", "id")
		require.NoError(t, r8MidErr(mig, r8Alter("01_plus", "u", "v", "c1", "v + 1", "v - 1"), abandoned))
		if r8Tip {
			require.Contains(t, r8Target(db), "public_09_later")
		}

		// One session, pinned by name to the schema the resumed train will
		// project. It writes through the previous schema's view.
		conn, err := db.Conn(ctx)
		require.NoError(t, err)
		defer conn.Close()
		_, err = conn.ExecContext(ctx, "SET search_path TO public_10_resumed")
		require.NoError(t, err)
		for i := 0; i < 6; i++ { // enough executions for a generic plan
			_, err = conn.ExecContext(ctx, fmt.Sprintf(`INSERT INTO public_00_base.u (id, v) VALUES (%d, 5)`, 20+i))
			require.NoError(t, err)
		}
		// A transaction left open across the re-point.
		tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `INSERT INTO public_00_base.u (id, v) VALUES (30, 5)`)
		require.NoError(t, err)

		resumed := r8Raw("10_resumed", `COMMENT ON COLUMN u.id IS 'resumed'`)
		done := make(chan error, 1)
		go func() { done <- r8TrainErr(mig, []*migrations.Migration{resumed}) }()
		// The Start waits on nothing the open transaction holds? If it does,
		// commit and let it through.
		select {
		case err := <-done:
			require.NoError(t, err)
			_, err = tx.ExecContext(ctx, `INSERT INTO public_00_base.u (id, v) VALUES (31, 5)`)
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
		default:
			require.NoError(t, tx.Commit())
			require.NoError(t, <-done)
		}
		if r8Tip {
			require.Contains(t, r8Target(db), "public_10_resumed")
		}

		// Now the session is the new revision: `up` must not run for it.
		_, err = conn.ExecContext(ctx, `INSERT INTO public_10_resumed.u (id, v) VALUES (40, 77)`)
		require.NoError(t, err)
		assert.Equal(t, "77", r8Str(db, `SELECT v FROM public_10_resumed.u WHERE id = 40`),
			"the session's write as the new revision is kept")
		assert.Equal(t, "76", r8Str(db, `SELECT v FROM public_00_base.u WHERE id = 40`),
			"and `down` ran for it")
		t.Logf("row 31 (same transaction, after the re-point): %s", r8Str(db, `SELECT to_jsonb(t)::text FROM public.u t WHERE id = 31`))
	})
}

// ---------------------------------------------------------------------------
// Train shapes whose trigger functions name columns or tables another
// migration of the train changes. Differential: SIG lines compared by build.
// ---------------------------------------------------------------------------

type r8Shape struct {
	base   string
	train  func() []*migrations.Migration
	table  string // the table's name in the new revision; "" when dropped
	oldIns string
	newIns string
	oldUpd string // previous revision updates the new revision's row
	newUpd string // new revision updates the previous revision's row
}

const r8U = `CREATE TABLE u (id integer PRIMARY KEY, v integer, x integer, c integer);
	INSERT INTO u VALUES (1, 5, 10, 100)`

func r8Last() *migrations.Migration { return r8Raw("09_last", "SELECT 1") }

func r8Shapes() map[string]r8Shape {
	oldIns := `INSERT INTO u (id, v, x, c) VALUES (2, 6, 20, 200)`
	alter := func(name, table, col string) *migrations.Migration {
		return r8Alter(name, table, col, "k_"+name, col+" + 1", col+" - 1")
	}
	str := func(s string) *string { return &s }
	return map[string]r8Shape{
		"rename_then_alter": {
			base: r8U, table: "u", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					r8One("01_rename", &migrations.OpRenameColumn{Table: "u", From: "x", To: "xx"}),
					alter("02_alter", "u", "xx"), r8Last(),
				}
			},
			newIns: `INSERT INTO u (id, v, xx, c) VALUES (3, 7, 30, 300)`,
			oldUpd: `UPDATE u SET x = 41 WHERE id = 3`, newUpd: `UPDATE u SET xx = 52 WHERE id = 2`,
		},
		"alter_then_rename": {
			base: r8U, table: "u", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					alter("01_alter", "u", "x"),
					r8One("02_rename", &migrations.OpRenameColumn{Table: "u", From: "x", To: "xx"}), r8Last(),
				}
			},
			newIns: `INSERT INTO u (id, v, xx, c) VALUES (3, 7, 30, 300)`,
			oldUpd: `UPDATE u SET x = 41 WHERE id = 3`, newUpd: `UPDATE u SET xx = 52 WHERE id = 2`,
		},
		"rename_table_then_alter": {
			base: r8U, table: "uu", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					r8One("01_rename", &migrations.OpRenameTable{From: "u", To: "uu"}),
					alter("02_alter", "uu", "v"), r8Last(),
				}
			},
			newIns: `INSERT INTO uu (id, v, x, c) VALUES (3, 7, 30, 300)`,
			oldUpd: `UPDATE u SET v = 41 WHERE id = 3`, newUpd: `UPDATE uu SET v = 52 WHERE id = 2`,
		},
		"alter_then_rename_table": {
			base: r8U, table: "uu", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					alter("01_alter", "u", "v"),
					r8One("02_rename", &migrations.OpRenameTable{From: "u", To: "uu"}), r8Last(),
				}
			},
			newIns: `INSERT INTO uu (id, v, x, c) VALUES (3, 7, 30, 300)`,
			oldUpd: `UPDATE u SET v = 41 WHERE id = 3`, newUpd: `UPDATE uu SET v = 52 WHERE id = 2`,
		},
		"drop_reading_then_alter": {
			base: r8U, table: "u", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					r8One("01_drop", &migrations.OpDropColumn{Table: "u", Column: "c", Down: "x + 100"}),
					alter("02_alter", "u", "x"), r8Last(),
				}
			},
			newIns: `INSERT INTO u (id, v, x) VALUES (3, 7, 30)`,
			oldUpd: `UPDATE u SET x = 41 WHERE id = 3`, newUpd: `UPDATE u SET x = 52 WHERE id = 2`,
		},
		"alter_then_drop_reading": {
			base: r8U, table: "u", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					alter("01_alter", "u", "x"),
					r8One("02_drop", &migrations.OpDropColumn{Table: "u", Column: "c", Down: "x + 100"}), r8Last(),
				}
			},
			newIns: `INSERT INTO u (id, v, x) VALUES (3, 7, 30)`,
			oldUpd: `UPDATE u SET x = 41 WHERE id = 3`, newUpd: `UPDATE u SET x = 52 WHERE id = 2`,
		},
		"alter_then_drop_same": {
			base: r8U, table: "u", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					alter("01_alter", "u", "v"),
					r8One("02_drop", &migrations.OpDropColumn{Table: "u", Column: "v", Down: "x"}), r8Last(),
				}
			},
			newIns: `INSERT INTO u (id, x, c) VALUES (3, 30, 300)`,
			oldUpd: `UPDATE u SET v = 41 WHERE id = 3`, newUpd: `UPDATE u SET x = 52 WHERE id = 2`,
		},
		"alter_type_then_drop_same": {
			base: r8U, table: "u", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					r8One("01_type", &migrations.OpAlterColumn{Table: "u", Column: "v", Type: str("text"), Up: "v::text", Down: "v::integer"}),
					r8One("02_drop", &migrations.OpDropColumn{Table: "u", Column: "v", Down: "'7'"}), r8Last(),
				}
			},
			newIns: `INSERT INTO u (id, x, c) VALUES (3, 30, 300)`,
			oldUpd: `UPDATE u SET v = 41 WHERE id = 3`, newUpd: `UPDATE u SET x = 52 WHERE id = 2`,
		},
		"drop_constraint_then_alter": {
			base: `CREATE TABLE u (id integer PRIMARY KEY, v integer CONSTRAINT v_pos CHECK (v > 0), x integer, c integer);
				INSERT INTO u VALUES (1, 5, 10, 100)`, table: "u", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					r8One("01_dropc", &migrations.OpDropConstraint{Table: "u", Name: "v_pos", Up: "v", Down: "CASE WHEN v > 0 THEN v ELSE 1 END"}),
					alter("02_alter", "u", "v"), r8Last(),
				}
			},
			newIns: `INSERT INTO u (id, v, x, c) VALUES (3, -7, 30, 300)`,
			oldUpd: `UPDATE u SET v = 41 WHERE id = 3`, newUpd: `UPDATE u SET v = -52 WHERE id = 2`,
		},
		"alter_then_drop_constraint": {
			base: `CREATE TABLE u (id integer PRIMARY KEY, v integer CONSTRAINT v_pos CHECK (v > 0), x integer, c integer);
				INSERT INTO u VALUES (1, 5, 10, 100)`, table: "u", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					alter("01_alter", "u", "v"),
					r8One("02_dropc", &migrations.OpDropConstraint{Table: "u", Name: "v_pos", Up: "v", Down: "CASE WHEN v > 0 THEN v ELSE 1 END"}),
					r8Last(),
				}
			},
			newIns: `INSERT INTO u (id, v, x, c) VALUES (3, -7, 30, 300)`,
			oldUpd: `UPDATE u SET v = 41 WHERE id = 3`, newUpd: `UPDATE u SET v = -52 WHERE id = 2`,
		},
		"rename_then_drop_constraint": {
			base: `CREATE TABLE u (id integer PRIMARY KEY, v integer, x integer CONSTRAINT x_pos CHECK (x > 0), c integer);
				INSERT INTO u VALUES (1, 5, 10, 100)`, table: "u", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					r8One("01_rename", &migrations.OpRenameColumn{Table: "u", From: "x", To: "xx"}),
					r8One("02_dropc", &migrations.OpDropConstraint{Table: "u", Name: "x_pos", Up: "xx", Down: "CASE WHEN xx > 0 THEN xx ELSE 1 END"}),
					r8Last(),
				}
			},
			newIns: `INSERT INTO u (id, v, xx, c) VALUES (3, 7, -30, 300)`,
			oldUpd: `UPDATE u SET x = 41 WHERE id = 3`, newUpd: `UPDATE u SET xx = -52 WHERE id = 2`,
		},
		"create_constraint_then_drop_reading": {
			base: r8U, table: "u", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					r8One("01_uq", &migrations.OpCreateConstraint{
						Table: "u", Name: "x_uq", Type: migrations.OpCreateConstraintTypeUnique, Columns: []string{"x"},
						Up: migrations.MultiColumnUpSQL{"x": "x"}, Down: migrations.MultiColumnDownSQL{"x": "x"},
					}),
					r8One("02_drop", &migrations.OpDropColumn{Table: "u", Column: "c", Down: "x + 100"}), r8Last(),
				}
			},
			newIns: `INSERT INTO u (id, v, x) VALUES (3, 7, 30)`,
			oldUpd: `UPDATE u SET x = 41 WHERE id = 3`, newUpd: `UPDATE u SET x = 52 WHERE id = 2`,
		},
		"drop_reading_then_create_constraint": {
			base: r8U, table: "u", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					r8One("01_drop", &migrations.OpDropColumn{Table: "u", Column: "c", Down: "x + 100"}),
					r8One("02_ck", &migrations.OpCreateConstraint{
						Table: "u", Name: "x_ck", Type: migrations.OpCreateConstraintTypeCheck, Columns: []string{"x"},
						Check: str("x > 0"),
						Up:    migrations.MultiColumnUpSQL{"x": "x"}, Down: migrations.MultiColumnDownSQL{"x": "x"},
					}), r8Last(),
				}
			},
			newIns: `INSERT INTO u (id, v, x) VALUES (3, 7, 30)`,
			oldUpd: `UPDATE u SET x = 41 WHERE id = 3`, newUpd: `UPDATE u SET x = 52 WHERE id = 2`,
		},
		"rename_then_create_constraint_then_drop": {
			base: r8U, table: "u", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					r8One("01_rename", &migrations.OpRenameColumn{Table: "u", From: "x", To: "xx"}),
					r8One("02_uq", &migrations.OpCreateConstraint{
						Table: "u", Name: "xx_uq", Type: migrations.OpCreateConstraintTypeUnique, Columns: []string{"xx"},
						Up: migrations.MultiColumnUpSQL{"xx": "xx"}, Down: migrations.MultiColumnDownSQL{"xx": "xx"},
					}),
					r8One("03_drop", &migrations.OpDropColumn{Table: "u", Column: "c", Down: "xx + 100"}), r8Last(),
				}
			},
			newIns: `INSERT INTO u (id, v, xx) VALUES (3, 7, 30)`,
			oldUpd: `UPDATE u SET x = 41 WHERE id = 3`, newUpd: `UPDATE u SET xx = 52 WHERE id = 2`,
		},
		"alter_then_drop_table": {
			base: r8U, table: "", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					alter("01_alter", "u", "v"),
					r8One("02_dropt", &migrations.OpDropTable{Name: "u"}), r8Last(),
				}
			},
			oldUpd: `UPDATE u SET v = 41 WHERE id = 2`,
		},
		"drop_then_drop_table": {
			base: r8U, table: "", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					r8One("01_drop", &migrations.OpDropColumn{Table: "u", Column: "c", Down: "x + 100"}),
					r8One("02_dropt", &migrations.OpDropTable{Name: "u"}), r8Last(),
				}
			},
			oldUpd: `UPDATE u SET v = 41 WHERE id = 2`,
		},
		"drop_then_raw_update": {
			base: r8U, table: "u", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					r8One("01_drop", &migrations.OpDropColumn{Table: "u", Column: "c", Down: "x + 100"}),
					r8Raw("02_data", `UPDATE u SET v = v + 1000`), r8Last(),
				}
			},
			newIns: `INSERT INTO u (id, v, x) VALUES (3, 7, 30)`,
			oldUpd: `UPDATE u SET x = 41 WHERE id = 3`, newUpd: `UPDATE u SET x = 52 WHERE id = 2`,
		},
		"alter_then_raw_update_of_it": {
			base: r8U, table: "u", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					alter("01_alter", "u", "v"),
					r8Raw("02_data", `UPDATE u SET v = v + 1000`), r8Last(),
				}
			},
			newIns: `INSERT INTO u (id, v, x, c) VALUES (3, 7, 30, 300)`,
			oldUpd: `UPDATE u SET v = 41 WHERE id = 3`, newUpd: `UPDATE u SET v = 52 WHERE id = 2`,
		},
		"two_drops_one_reading_the_other": {
			base: r8U, table: "u", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					r8One("01_drop", &migrations.OpDropColumn{Table: "u", Column: "c", Down: "x + 100"}),
					r8One("02_drop", &migrations.OpDropColumn{Table: "u", Column: "x", Down: "v * 2"}), r8Last(),
				}
			},
			newIns: `INSERT INTO u (id, v) VALUES (3, 7)`,
			oldUpd: `UPDATE u SET x = 41 WHERE id = 3`, newUpd: `UPDATE u SET v = 52 WHERE id = 2`,
		},
		"alter_and_drop_in_one_migration": {
			base: r8U, table: "u", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					r8One("01_both",
						&migrations.OpAlterColumn{Table: "u", Column: "x", Check: &migrations.CheckConstraint{Name: "k", Constraint: "x <> -9"}, Up: "x + 1", Down: "x - 1"},
						&migrations.OpDropColumn{Table: "u", Column: "c", Down: "x + 100"}),
					r8Last(),
				}
			},
			newIns: `INSERT INTO u (id, v, x) VALUES (3, 7, 30)`,
			oldUpd: `UPDATE u SET x = 41 WHERE id = 3`, newUpd: `UPDATE u SET x = 52 WHERE id = 2`,
		},
		"add_then_alter_of_another_reading_it": {
			base: r8U, table: "u", oldIns: oldIns,
			train: func() []*migrations.Migration {
				return []*migrations.Migration{
					r8One("01_add_and_rename",
						&migrations.OpAddColumn{Table: "u", Column: migrations.Column{Name: "d", Type: "integer", Nullable: true}, Up: "x * 2"},
						&migrations.OpRenameColumn{Table: "u", From: "c", To: "cc"}),
					r8One("02_alter", &migrations.OpAlterColumn{Table: "u", Column: "v", Check: &migrations.CheckConstraint{Name: "k", Constraint: "v <> -9"}, Up: "v + coalesce(d, 0)", Down: "v - coalesce(d, 0)"}),
					r8Last(),
				}
			},
			newIns: `INSERT INTO u (id, v, x, cc, d) VALUES (3, 7, 30, 300, 9)`,
			oldUpd: `UPDATE u SET v = 41 WHERE id = 3`, newUpd: `UPDATE u SET v = 52 WHERE id = 2`,
		},
	}
}

func r8Rows(db *sql.DB, schema, table string) string {
	return r8Str(db, fmt.Sprintf(`SELECT coalesce(string_agg(to_jsonb(t)::text, ' ' ORDER BY id), '') FROM %s.%s t`, schema, table))
}

func TestTrainRecoveryShapes(t *testing.T) {
	if os.Getenv("PGROLL_TRAIN_MATRIX") == "" {
		t.Skip("set PGROLL_TRAIN_MATRIX=1 to log one signature per train shape, for comparison with another build")
	}
	t.Parallel()
	only := os.Getenv("R8_SHAPE")
	shapes := r8Shapes()
	names := make([]string, 0, len(shapes))
	for name := range shapes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		shape := shapes[name]
		if only != "" && only != name {
			continue
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				r8Base(t, mig, shape.base)
				train := shape.train()
				next := "public_" + train[len(train)-1].Name
				sig := []string{"train=" + r8Short(r8TrainErr(mig, train))}
				step := func(label, path, stmt string) {
					if stmt == "" {
						return
					}
					sig = append(sig, label+"="+r8Short(r8ExecAs(t, db, path, stmt)))
				}
				step("oldIns", r8Prev, shape.oldIns)
				step("newIns", next, shape.newIns)
				step("oldUpd", r8Prev, shape.oldUpd)
				step("newUpd", next, shape.newUpd)
				sig = append(sig, "prev["+r8Rows(db, r8Prev, "u")+"]")
				if shape.table != "" {
					sig = append(sig, "next["+r8Rows(db, next, shape.table)+"]")
				}
				sig = append(sig, "complete="+r8Short(r8Safely(func() error { return mig.Complete(ctx) })))
				if shape.table != "" {
					sig = append(sig, "final["+r8Rows(db, "public", shape.table)+"]")
				}
				sig = append(sig, "left="+r8Leftovers(db))
				t.Logf("R8SIG|%s|%s", name, strings.Join(sig, " | "))
			})
		})
	}
}

// ---------------------------------------------------------------------------
// `pgroll materialize` of the leaf after a contraction stopped part-way.
// ---------------------------------------------------------------------------

// r8ArmFault makes the n-th DDL statement from now on fail, once.
func r8ArmFault(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	_, err := db.Exec(fmt.Sprintf(`
		CREATE SCHEMA IF NOT EXISTS r8fault;
		DROP EVENT TRIGGER IF EXISTS r8_fault;
		DROP SEQUENCE IF EXISTS r8fault.seq;
		CREATE SEQUENCE r8fault.seq;
		CREATE OR REPLACE FUNCTION r8fault.trip() RETURNS event_trigger LANGUAGE plpgsql AS $f$
		BEGIN
			IF nextval('r8fault.seq') = %d THEN
				RAISE EXCEPTION 'r8 injected fault at %%', tg_tag;
			END IF;
		END $f$;
		CREATE EVENT TRIGGER r8_fault ON ddl_command_start EXECUTE FUNCTION r8fault.trip()`, n))
	require.NoError(t, err)
}

func r8DisarmFault(t *testing.T, db *sql.DB) {
	t.Helper()
	_, err := db.Exec(`DROP EVENT TRIGGER IF EXISTS r8_fault`)
	require.NoError(t, err)
}

// A bounded revert leaves a leaf the fleet pins and a queue to contract.
// `pgroll complete` then fails at its n-th DDL statement, and the operator
// reaches for `pgroll materialize` to put the live schema back (the views
// are dropped here to stand for whatever lost them). The leaf projects what
// the queue still has to contract.
func TestTrainRecoveryMaterializeAfterAContractionStoppedPartWay(t *testing.T) {
	t.Parallel()

	for n := 1; n <= 14; n++ {
		t.Run(fmt.Sprintf("fault_at_%02d", n), func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()
				r8Base(t, mig, `CREATE TABLE u (id integer PRIMARY KEY, v integer, x integer); INSERT INTO u VALUES (1, 5, 10);
					CREATE TABLE other (id integer PRIMARY KEY, n integer); INSERT INTO other VALUES (1, 1)`)
				require.NoError(t, r8TrainErr(mig, []*migrations.Migration{
					r8Alter("01_alter", "u", "v", "k1", "v + 1", "v - 1"),
					r8One("02_rename", &migrations.OpRenameColumn{Table: "u", From: "x", To: "xx"}),
					r8Raw("03_comment", `COMMENT ON TABLE u IS 'kept'`),
					r8Index("09_later", "u", "id"),
				}))
				_, err := mig.Revert(ctx, roll.WithRevertTo("03_comment"))
				require.NoError(t, err)

				r8ArmFault(t, db, n)
				_, _, cerr := mig.FinishContraction(ctx)
				r8DisarmFault(t, db)
				if cerr == nil {
					t.Skipf("the contraction has fewer than %d DDL statements", n)
				}
				cols := r8Str(db, `SELECT string_agg(attname, ',' ORDER BY attnum) FROM pg_attribute
					WHERE attrelid = 'public.u'::regclass AND attnum > 0 AND NOT attisdropped`)
				queue := r8Str(db, `SELECT coalesce(string_agg(name, ',' ORDER BY name), '') FROM pgroll.migrations WHERE complete_deferred`)

				_, err = db.Exec(`DROP SCHEMA IF EXISTS public_03_comment CASCADE`)
				require.NoError(t, err)
				var merr error
				sc, err := r8MaterializeSchema(ctx, mig)
				if err != nil {
					merr = fmt.Errorf("read schema: %w", err)
				} else {
					merr = mig.Materialize(ctx, "03_comment", sc, roll.WithNewRevisionPinned(), roll.WithUnprojectableTablesAsTheyAre())
				}
				other := r8Str(db, `SELECT n FROM public_03_comment.other WHERE id = 1`)
				u := r8Str(db, `SELECT to_jsonb(t)::text FROM public_03_comment.u t WHERE id = 1`)
				t.Logf("R8MAT|%02d|fault=%s|columns=%s|queue=%s|materialize=%s|other=%s|u=%s",
					n, r8Short(cerr), cols, queue, r8Short(merr), other, u)

				require.NoError(t, merr, "materialize after a fault at statement %d", n)
				require.Equal(t, "1", other)

				// The retry is the real recovery.
				_, _, rerr := mig.FinishContraction(ctx)
				require.NoError(t, rerr, "the contraction, retried")
				require.Equal(t, "none", r8Leftovers(db), "what is left of pgroll after the retry")
				t.Logf("R8MAT|%02d|retry=%s|u=%s", n, r8Short(rerr), r8Str(db, `SELECT to_jsonb(t)::text FROM public_03_comment.u t WHERE id = 1`))
			})
		})
	}
}

// ---------------------------------------------------------------------------
// // part-way, when the last unapplied migration is not the one the stalled
// train was aimed at.
// ---------------------------------------------------------------------------

// The stalled train's triggers follow the target its first attempt set. The
// fast-forward projection is now built from the waiting migrations' changes,
// so a pod pinned to it writes the duplicated column, but the triggers still
// take it for the previous revision and overwrite what it wrote. The base
// build projected the tables as they are and kept the write.
func TestTrainRecoveryFastForwardMaterializeOfAStalledTrain(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		r8Base(t, mig, `CREATE TABLE u (id integer PRIMARY KEY, v integer); INSERT INTO u VALUES (1, 5)`)

		// `pgroll migrate` aimed at 03_first stops after 01_check.
		first := r8Raw("03_first", `SELECT 1`)
		check := r8One("01_check", &migrations.OpAlterColumn{
			Table: "u", Column: "v",
			Check: &migrations.CheckConstraint{Name: "v_pos", Constraint: "v > 0"},
			Up:    "CASE WHEN v > 0 THEN v ELSE 1 END", Down: "v",
		})
		require.NoError(t, r8MidErr(mig, check, first))

		// The directory now ends with another migration, and the operator
		// runs `pgroll materialize --migrations <dir>` as cmd/materialize.go
		// does: read the schema, layer the unapplied migrations, project.
		other := r8Raw("05_other", `COMMENT ON TABLE u IS 'five'`)
		sc, err := r8MaterializeSchema(ctx, mig)
		require.NoError(t, err)
		require.NoError(t, other.UpdateVirtualSchema(ctx, sc))
		require.NoError(t, mig.Materialize(ctx, other.VersionSchemaName(), sc, roll.WithNewRevisionPinned()))
		t.Logf("target after materialize: %s", r8Target(db))

		const pinned = "public_05_other"
		require.NoError(t, r8ExecAs(t, db, pinned, `INSERT INTO u (id, v) VALUES (3, 3)`))
		require.NoError(t, r8ExecAs(t, db, pinned, `UPDATE u SET v = 8 WHERE id = 1`))
		assert.Equal(t, "1:8 3:3", r8Str(db, `SELECT string_agg(id || ':' || coalesce(v::text, 'NULL'), ' ' ORDER BY id) FROM `+pinned+`.u`),
			"a pod pinned to the materialized schema reads what it wrote")

		// The train is finished and contracted.
		require.NoError(t, r8TrainErr(mig, []*migrations.Migration{other}))
		require.NoError(t, mig.Complete(ctx))
		assert.Equal(t, "1:8 3:3", r8Str(db, `SELECT string_agg(id || ':' || coalesce(v::text, 'NULL'), ' ' ORDER BY id) FROM public.u`),
			"and the table keeps it")
	})
}

// ---------------------------------------------------------------------------
// The open-train guard after a contraction that stopped part-way.
// ---------------------------------------------------------------------------

// A bounded revert, then `pgroll complete` fails while draining the second
// of two waiting migrations. Every row is sealed by then, one migration
// still has its triggers, and the fleet runs on the leaf. The guard reads the
// train as open: a new one would re-point the triggers and overwrite the
// fleet's writes.
func TestTrainRecoveryGuardAfterAContractionStoppedPartWay(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		r8Base(t, mig, `CREATE TABLE u (id integer PRIMARY KEY, v integer); INSERT INTO u VALUES (1, 5);
			CREATE TABLE u2 (id integer PRIMARY KEY, v integer); INSERT INTO u2 VALUES (1, 5)`)
		require.NoError(t, r8TrainErr(mig, []*migrations.Migration{
			r8Alter("01_plus", "u", "v", "k1", "v + 1", "v - 1"),
			r8Alter("02_times", "u2", "v", "k2", "v * 2", "v / 2"),
			r8Raw("03_comment", `COMMENT ON TABLE u IS 'kept'`),
			r8Index("09_later", "u", "id"),
		}))
		_, err := mig.Revert(ctx, roll.WithRevertTo("03_comment"))
		require.NoError(t, err)
		const leaf = "public_03_comment"

		r8ArmFault(t, db, 7) // the first statement of 02_times' drain
		_, _, cerr := mig.FinishContraction(ctx)
		r8DisarmFault(t, db)
		require.Error(t, cerr)
		require.Equal(t, "02_times", r8Str(db, `SELECT string_agg(name, ',') FROM pgroll.migrations WHERE complete_deferred`))
		require.NotEmpty(t, r8Triggers(db, "u2"), "02_times still has its triggers")

		require.NoError(t, r8ExecAs(t, db, leaf, `INSERT INTO u2 (id, v) VALUES (3, 70)`))
		require.Equal(t, "70", r8Str(db, `SELECT v FROM `+leaf+`.u2 WHERE id = 3`))

		reopened, err := r8Reopened(ctx, mig)
		require.NoError(t, err)
		require.Equal(t, "03_comment", reopened, "the guard refuses to start a train here")
	})
}

// ---------------------------------------------------------------------------
// // leaf. cmd/stamp.go still projects the tables as they are, and
// Roll.Materialize now re-points the train's triggers at that schema.
// ---------------------------------------------------------------------------

// A pod pinned to the schema reads and writes the old column through the
// view, and the re-pointed triggers take it for the new revision: `up` no
// longer copies its write and `down` overwrites it from the duplicate.
func TestTrainRecoveryStampMaterializeOfAStalledTrainsLeaf(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		r8Base(t, mig, `CREATE TABLE u (id integer PRIMARY KEY, v integer); INSERT INTO u VALUES (1, 5)`)

		final := r8Raw("09_later", `SELECT 1`)
		require.NoError(t, r8MidErr(mig, r8One("01_check", &migrations.OpAlterColumn{
			Table: "u", Column: "v",
			Check: &migrations.CheckConstraint{Name: "v_pos", Constraint: "v > 0"},
			Up:    "CASE WHEN v > 0 THEN v ELSE 1 END", Down: "v",
		}), final))
		require.NoError(t, r8MidErr(mig, r8Raw("02_comment", `COMMENT ON TABLE u IS 'x'`), final))

		// cmd/stamp.go with --materialize and nothing left to stamp.
		sc, err := mig.State().ReadSchema(ctx, mig.Schema())
		require.NoError(t, err)
		require.NoError(t, mig.Materialize(ctx, "02_comment", sc))
		t.Logf("target after stamp --materialize: %s", r8Target(db))

		const pinned = "public_02_comment"
		require.NoError(t, r8ExecAs(t, db, pinned, `INSERT INTO u (id, v) VALUES (3, 3)`))
		require.NoError(t, r8ExecAs(t, db, pinned, `UPDATE u SET v = 8 WHERE id = 1`))
		assert.Equal(t, "1:8 3:3", r8Str(db, `SELECT string_agg(id || ':' || coalesce(v::text, 'NULL'), ' ' ORDER BY id) FROM `+pinned+`.u`),
			"a pod pinned to the materialized schema reads what it wrote")
	})
}

const r8Tip = true

func r8MidOpts(mig *roll.Roll, final *migrations.Migration) []roll.StartOption {
	return append([]roll.StartOption{roll.WithoutVersionSchema()}, r8LeafOpts(mig, final)...)
}

func r8LeafOpts(mig *roll.Roll, final *migrations.Migration) []roll.StartOption {
	if !mig.UseVersionSchema() {
		return nil
	}
	return []roll.StartOption{roll.WithTrainTarget(final)}
}

// r8MaterializeSchema is the schema `pgroll materialize` projects for the
// leaf when no migration is in progress (cmd/materialize.go).
func r8MaterializeSchema(ctx context.Context, mig *roll.Roll) (*schema.Schema, error) {
	return mig.SchemaWithDeferred(ctx)
}

// r8Reopened is the guard `pgroll migrate` and `pgroll start` ask.
func r8Reopened(ctx context.Context, mig *roll.Roll) (string, error) {
	return mig.ReopenedTrainLeaf(ctx)
}
