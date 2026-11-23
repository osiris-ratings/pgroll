// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/roll"
)

type guardEnv struct {
	t   *testing.T
	ctx context.Context
	m   *roll.Roll
	db  *sql.DB
	dir string
}

func guardWith(t *testing.T, fn func(e *guardEnv)) {
	testutils.WithMigratorAndConnStrToContainer(t, []roll.Option{roll.WithLockTimeoutMs(500)},
		func(m *roll.Roll, connStr string, db *sql.DB) {
			viper.Set("PG_URL", connStr)
			viper.Set("SCHEMA", "public")
			viper.Set("STATE_SCHEMA", "pgroll")
			viper.Set("LOCK_TIMEOUT", 500)
			viper.Set("USE_VERSION_SCHEMA", true)
			t.Cleanup(viper.Reset)
			fn(&guardEnv{t: t, ctx: context.Background(), m: m, db: db, dir: t.TempDir()})
		})
}

func (e *guardEnv) write(name, body string) {
	require.NoError(e.t, os.WriteFile(filepath.Join(e.dir, name+".yaml"), []byte(body), 0o600))
}

func (e *guardEnv) sqlMig(name, up, down string) {
	e.write(name, "operations:\n  - sql:\n      up: "+up+"\n      down: "+down+"\n")
}

func (e *guardEnv) remove(name string) {
	require.NoError(e.t, os.Remove(filepath.Join(e.dir, name+".yaml")))
}

func (e *guardEnv) migrate(args ...string) error {
	c := migrateCmd()
	c.SetArgs(append([]string{e.dir}, args...))
	c.SilenceUsage, c.SilenceErrors = true, true
	return c.ExecuteContext(e.ctx)
}

func (e *guardEnv) materialize(args ...string) error {
	c := materializeCmd()
	c.SetArgs(append([]string{"--yes"}, args...))
	c.SilenceUsage, c.SilenceErrors = true, true
	return c.ExecuteContext(e.ctx)
}

func (e *guardEnv) execAs(versionSchema, stmt string) error {
	conn, err := e.db.Conn(e.ctx)
	require.NoError(e.t, err)
	defer conn.Close()
	_, err = conn.ExecContext(e.ctx, "SET search_path TO "+versionSchema)
	require.NoError(e.t, err)
	_, err = conn.ExecContext(e.ctx, stmt)
	return err
}

func (e *guardEnv) value(query string) sql.NullInt64 {
	var v sql.NullInt64
	require.NoError(e.t, e.db.QueryRowContext(e.ctx, query).Scan(&v))
	return v
}

func (e *guardEnv) target() string {
	var v sql.NullString
	require.NoError(e.t, e.db.QueryRowContext(e.ctx, `
		SELECT (SELECT prosrc FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		        WHERE n.nspname = 'public' AND p.proname = '_pgroll_train_target')`).Scan(&v))
	return v.String
}

const guardFill = `
operations:
  - add_column:
      table: fresh
      up: id * 2
      column:
        name: twice
        type: integer
        nullable: true
`

const guardCreate = `
operations:
  - create_table:
      name: fresh
      columns:
        - name: id
          type: integer
          pk: true
`

// A bounded revert leaves 01_create as the leaf with a version schema
// the fleet pins ("apps need a projection to repin to"), and that fleet
// writes the table 01_create made. 01_create is applied and unsealed, so the
// next train's fill of that table is exempt from the refusal, completes
// before the last migration and loses its trigger: the fleet's writes are no
// longer filled. 2b244ad refused this run.
func TestMigrateRefusesAFillOnATableAFleetOnTheLeafWrites(t *testing.T) {
	guardWith(t, func(e *guardEnv) {
		e.sqlMig("00_base", "CREATE TABLE w (id integer PRIMARY KEY, v integer)", "DROP TABLE w")
		require.NoError(t, e.migrate("--complete"))

		e.write("01_create", guardCreate)
		e.sqlMig("02_two", "COMMENT ON TABLE w IS 'two'", "COMMENT ON TABLE w IS NULL")
		require.NoError(t, e.migrate())
		_, err := e.m.Revert(e.ctx, roll.WithRevertTo("01_create"))
		require.NoError(t, err)
		e.remove("02_two")

		// The fleet on the reverted leaf writes the table.
		require.NoError(t, e.execAs("public_01_create", `INSERT INTO fresh (id) VALUES (1)`))

		e.write("03_fill", guardFill)
		e.sqlMig("09_later", "COMMENT ON TABLE w IS 'nine'", "COMMENT ON TABLE w IS NULL")
		require.ErrorContains(t, e.migrate(), `migration "03_fill" adds column "twice"`)
	})
}

// `--version <previous> --migrations <dir>` with nothing unapplied in
// the directory (LAYER_MIGRATIONS=true VERSION=… on a database already at the
// directory's tail). Without --migrations the previous revision's schema is
// projected as the tables are and the triggers are left alone. With it, the
// same schema is projected as the new revision's and the train is re-pointed
// at it: the fleet on the leaf has its writes rewritten and the previous
// fleet's are rejected. The fast-forward guard does not fire: nothing was
// fast-forwarded.
func TestMaterializeAnEarlierVersionLeavesTheTrainAlone(t *testing.T) {
	guardWith(t, func(e *guardEnv) {
		e.sqlMig("00_base", "CREATE TABLE u (id integer PRIMARY KEY, v integer)", "DROP TABLE u")
		require.NoError(t, e.migrate("--complete"))

		e.write("01_one", checkOnV)
		e.sqlMig("02_two", "COMMENT ON TABLE u IS 'two'", "COMMENT ON TABLE u IS NULL")
		require.NoError(t, e.migrate())
		_, err := e.m.Revert(e.ctx, roll.WithRevertTo("01_one"))
		require.NoError(t, err)
		e.remove("02_two")

		require.NoError(t, e.execAs("public_01_one", `INSERT INTO u (id, v) VALUES (10, 7)`))
		require.Equal(t, int64(7), e.value(`SELECT v FROM public_01_one.u WHERE id = 10`).Int64)
		before := e.target()

		require.NoError(t, e.materialize("--version", "00_base", "--migrations", e.dir))

		assert.Equal(t, before, e.target(), "the train's target after re-projecting the previous revision's schema")
		require.NoError(t, e.execAs("public_01_one", `INSERT INTO u (id, v) VALUES (12, 7)`))
		assert.Equal(t, int64(7), e.value(`SELECT v FROM public_01_one.u WHERE id = 12`).Int64,
			"a write of 7 by the fleet on the leaf")
		assert.NoError(t, e.execAs("public_00_base", `INSERT INTO u (id, v) VALUES (13, -3)`),
			"a write the previous revision is allowed")
	})
}

// guardStalled leaves a train stopped after 01_one (a failing 02), repairs 02,
// and recovers with `pgroll materialize --migrations`: the fleet pins the
// fast-forwarded schema public_09_later and writes as the new revision.
func guardStalled(e *guardEnv) {
	t := e.t
	e.sqlMig("00_base", "CREATE TABLE u (id integer PRIMARY KEY, v integer)", "DROP TABLE u")
	require.NoError(t, e.migrate("--complete"))

	e.write("01_one", checkOnV)
	e.sqlMig("02_two", "SELECT 1/0", "SELECT 1")
	e.sqlMig("09_later", "COMMENT ON TABLE u IS 'nine'", "COMMENT ON TABLE u IS NULL")
	require.Error(t, e.migrate())
	e.sqlMig("02_two", "SELECT 1", "SELECT 1")

	require.NoError(t, e.materialize("--migrations", e.dir))
	require.Equal(t, "SELECT 'public_09_later'::text", e.target())
	require.NoError(t, e.execAs("public_09_later", `INSERT INTO u (id, v) VALUES (10, 7)`))
	require.Equal(t, int64(7), e.value(`SELECT v FROM public_09_later.u WHERE id = 10`).Int64)
}

// The guards ask whether the LEAF's version schema exists. After the
// interrupted-migrate recovery the fleet is on a fast-forwarded schema, the
// leaf has none, and every command that moves the target does so unrefused:
// the fleet's writes are then rewritten as the previous revision's.
func TestAFleetOnAFastForwardedSchemaKeepsItsWrites(t *testing.T) {
	check := func(e *guardEnv, moved error) {
		require.ErrorContains(e.t, moved, "--leaf-unpinned")
		require.NoError(e.t, e.execAs("public_09_later", `INSERT INTO u (id, v) VALUES (12, 7)`))
		assert.Equal(e.t, int64(7), e.value(`SELECT v FROM public_09_later.u WHERE id = 12`).Int64,
			"a write of 7 by the fleet on public_09_later; the target is now %s", e.target())
	}
	t.Run("plain materialize", func(t *testing.T) {
		guardWith(t, func(e *guardEnv) {
			guardStalled(e)
			check(e, e.materialize())
		})
	})
	t.Run("migrate with one more migration", func(t *testing.T) {
		guardWith(t, func(e *guardEnv) {
			guardStalled(e)
			e.sqlMig("12_next", "COMMENT ON TABLE u IS 'twelve'", "COMMENT ON TABLE u IS NULL")
			check(e, e.migrate())
		})
	})
	t.Run("a second fast-forward", func(t *testing.T) {
		guardWith(t, func(e *guardEnv) {
			guardStalled(e)
			e.sqlMig("12_next", "COMMENT ON TABLE u IS 'twelve'", "COMMENT ON TABLE u IS NULL")
			check(e, e.materialize("--migrations", e.dir))
		})
	})
}

// The refusal for "every migration is contracted, yet trigger functions
// are installed" offers --leaf-unpinned for triggers of a pruned migration.
// Taking it re-points the orphaned triggers at the new train: the orphaned
// `down` then overwrites what the new revision writes to the column with the
// duplicate's value, NULL on an insert. On 6b2e5ea the same sequence keeps
// the write.
func TestMigrateRefusesBesideTheTriggersOfAPrunedMigration(t *testing.T) {
	guardWith(t, func(e *guardEnv) {
		e.sqlMig("00_base", "CREATE TABLE u (id integer PRIMARY KEY, v integer)", "DROP TABLE u")
		require.NoError(t, e.migrate("--complete"))

		e.write("01_one", checkOnV)
		e.sqlMig("02_two", "COMMENT ON TABLE u IS 'two'", "COMMENT ON TABLE u IS NULL")
		require.NoError(t, e.migrate())
		_, err := e.m.Revert(e.ctx, roll.WithRevertTo("01_one"))
		require.NoError(t, err)
		e.remove("02_two")
		e.remove("01_one")

		_, err = e.m.Prune(e.ctx, []string{"01_one"})
		require.NoError(t, err)

		e.sqlMig("12_next", "COMMENT ON TABLE u IS 'twelve'", "COMMENT ON TABLE u IS NULL")
		err = e.migrate()
		require.ErrorContains(t, err, "pgroll prune")
		require.ErrorContains(t, e.migrate("--leaf-unpinned"), "pgroll prune", "no flag starts beside triggers nothing owns")
	})
}
