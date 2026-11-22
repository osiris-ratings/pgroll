// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

// A train applied by `pgroll migrate` serves both revisions whatever the
// position of its alter_column, drop_column and add_column migrations.
// Not parallel: the command reads its connection from process-wide flags.
func TestMigrateTrainServesBothRevisions(t *testing.T) {
	testutils.WithMigratorAndConnStrToContainer(t, []roll.Option{roll.WithLockTimeoutMs(500)},
		func(_ *roll.Roll, connStr string, db *sql.DB) {
			ctx := context.Background()
			viper.Set("PG_URL", connStr)
			viper.Set("SCHEMA", "public")
			viper.Set("STATE_SCHEMA", "pgroll")
			viper.Set("LOCK_TIMEOUT", 500)
			viper.Set("USE_VERSION_SCHEMA", true)
			t.Cleanup(viper.Reset)

			dir := t.TempDir()
			write := func(name, body string) {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o600))
			}
			run := func(args ...string) {
				c := migrateCmd()
				c.SetArgs(args)
				require.NoError(t, c.ExecuteContext(ctx))
			}
			execAs := func(versionSchema, stmt string) error {
				conn, err := db.Conn(ctx)
				require.NoError(t, err)
				defer conn.Close()
				_, err = conn.ExecContext(ctx, "SET search_path TO "+versionSchema)
				require.NoError(t, err)
				_, err = conn.ExecContext(ctx, stmt)
				return err
			}
			value := func(query string) int {
				var v int
				require.NoError(t, db.QueryRowContext(ctx, query).Scan(&v))
				return v
			}

			write("00_base", `
operations:
  - sql:
      up: |
        CREATE TABLE u (id integer PRIMARY KEY, v integer, gone integer NOT NULL);
        CREATE TABLE w (id integer PRIMARY KEY, x integer);
      down: |
        DROP TABLE w;
        DROP TABLE u;
`)
			run(dir, "--complete")

			write("01_check_v", `
operations:
  - alter_column:
      table: u
      column: v
      check:
        name: v_pos
        constraint: v > 0
      up: CASE WHEN v > 0 THEN v ELSE 1 END
      down: v
`)
			write("02_drop_gone", `
operations:
  - drop_column:
      table: u
      column: gone
      down: "0"
`)
			write("03_add_a", `
operations:
  - add_column:
      table: w
      up: x + 1
      column:
        name: a
        type: integer
        nullable: true
        check:
          name: a_set
          constraint: a IS NOT NULL
`)
			write("09_later", `
operations:
  - create_index:
      name: ix_w_a
      table: w
      columns:
        - column: a
`)
			run(dir)

			const previous, target = "public_00_base", "public_09_later"

			require.Error(t, execAs(target, `INSERT INTO u (id, v) VALUES (10, -1)`))
			require.NoError(t, execAs(target, `INSERT INTO u (id, v) VALUES (11, 7)`))
			require.NoError(t, execAs(previous, `INSERT INTO u (id, v, gone) VALUES (12, -3, 5)`))
			require.Equal(t, 7, value(`SELECT v FROM public_00_base.u WHERE id = 11`))
			require.Equal(t, 0, value(`SELECT gone FROM public_00_base.u WHERE id = 11`))

			require.NoError(t, execAs(previous, `INSERT INTO w (id, x) VALUES (1, 20)`))
			require.NoError(t, execAs(target, `INSERT INTO w (id, x, a) VALUES (2, 30, 99)`))
			require.Equal(t, 21, value(`SELECT a FROM public_09_later.w WHERE id = 1`))
			require.Equal(t, 99, value(`SELECT a FROM public_09_later.w WHERE id = 2`))

			complete := completeCmd()
			complete.SetArgs(nil)
			require.NoError(t, complete.ExecuteContext(ctx))

			require.Equal(t, 1, value(`SELECT v FROM public.u WHERE id = 12`))
			require.Equal(t, 2, value(`SELECT count(*) FROM public.u`))
			require.Equal(t, 0, value(`
				SELECT (SELECT count(*) FROM pg_proc WHERE proname LIKE '\_pgroll\_%')
				     + (SELECT count(*) FROM information_schema.columns
				        WHERE table_schema = 'public' AND column_name LIKE '\_pgroll\_%')`))
		})
}

// After a bounded revert the fleet pins the leaf it left; a new train is
// refused until that one is contracted.
// Not parallel: the command reads its connection from process-wide flags.
func TestMigrateRefusesANewTrainOnAReopenedOne(t *testing.T) {
	testutils.WithMigratorAndConnStrToContainer(t, []roll.Option{roll.WithLockTimeoutMs(500)},
		func(m *roll.Roll, connStr string, _ *sql.DB) {
			ctx := context.Background()
			viper.Set("PG_URL", connStr)
			viper.Set("SCHEMA", "public")
			viper.Set("STATE_SCHEMA", "pgroll")
			viper.Set("LOCK_TIMEOUT", 500)
			viper.Set("USE_VERSION_SCHEMA", true)
			t.Cleanup(viper.Reset)

			dir := t.TempDir()
			write := func(name, up, down string) {
				body := "operations:\n  - sql:\n      up: " + up + "\n      down: " + down + "\n"
				require.NoError(t, os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o600))
			}
			migrate := func(args ...string) error {
				c := migrateCmd()
				c.SetArgs(append([]string{dir}, args...))
				c.SilenceUsage, c.SilenceErrors = true, true
				return c.ExecuteContext(ctx)
			}

			write("00_base", "CREATE TABLE u (id integer PRIMARY KEY)", "DROP TABLE u")
			require.NoError(t, migrate("--complete"))

			// The open train needs a trigger for a third revision to confuse.
			require.NoError(t, os.WriteFile(filepath.Join(dir, "01_one.yaml"), []byte(addColumnWithUp), 0o600))
			write("02_two", "COMMENT ON TABLE u IS 'two'", "COMMENT ON TABLE u IS 'one'")
			require.NoError(t, migrate())

			_, err := m.Revert(ctx, roll.WithRevertTo("01_one"))
			require.NoError(t, err)

			require.NoError(t, os.Remove(filepath.Join(dir, "02_two.yaml")))
			write("03_three", "COMMENT ON TABLE u IS 'three'", "COMMENT ON TABLE u IS 'one'")
			require.ErrorContains(t, migrate(), "pgroll complete")

			complete := completeCmd()
			complete.SetArgs(nil)
			require.NoError(t, complete.ExecuteContext(ctx))
			require.NoError(t, migrate())
		})
}

// Materializing the leaf of a train that stopped part-way leaves the same
// state as a bounded revert. Both commands refuse it; saying that nothing
// is pinned to the materialized schema resumes the train.
// Not parallel: the command reads its connection from process-wide flags.
func TestAnOpenTrainWithAMaterializedLeafNeedsLeafUnpinned(t *testing.T) {
	testutils.WithMigratorAndConnStrToContainer(t, []roll.Option{roll.WithLockTimeoutMs(500)},
		func(m *roll.Roll, connStr string, _ *sql.DB) {
			ctx := context.Background()
			viper.Set("PG_URL", connStr)
			viper.Set("SCHEMA", "public")
			viper.Set("STATE_SCHEMA", "pgroll")
			viper.Set("LOCK_TIMEOUT", 500)
			viper.Set("USE_VERSION_SCHEMA", true)
			t.Cleanup(viper.Reset)

			dir := t.TempDir()
			write := func(name, up, down string) string {
				path := filepath.Join(dir, name+".yaml")
				body := "operations:\n  - sql:\n      up: " + up + "\n      down: " + down + "\n"
				require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
				return path
			}
			migrate := func(args ...string) error {
				c := migrateCmd()
				c.SetArgs(append([]string{dir}, args...))
				c.SilenceUsage, c.SilenceErrors = true, true
				return c.ExecuteContext(ctx)
			}

			write("00_base", "CREATE TABLE u (id integer PRIMARY KEY)", "DROP TABLE u")
			require.NoError(t, migrate("--complete"))

			require.NoError(t, os.WriteFile(filepath.Join(dir, "01_one.yaml"), []byte(addColumnWithUp), 0o600))
			two := write("02_two", "COMMENT ON TABLE u IS 'two'", "COMMENT ON TABLE u IS 'one'")

			// The train stops after its first migration.
			one, err := migrations.ReadMigration(os.DirFS(dir), "01_one.yaml")
			require.NoError(t, err)
			require.NoError(t, m.Start(ctx, one, backfill.NewConfig(), roll.WithoutVersionSchema()))
			require.NoError(t, m.Complete(ctx, roll.WithDeferComplete()))

			// Its leaf is then materialized.
			sc, err := m.SchemaWithDeferred(ctx)
			require.NoError(t, err)
			require.NoError(t, m.Materialize(ctx, "01_one", sc))

			require.ErrorContains(t, migrate(), "--leaf-unpinned")

			start := startCmd()
			start.SetArgs([]string{two})
			start.SilenceUsage, start.SilenceErrors = true, true
			require.ErrorContains(t, start.ExecuteContext(ctx), "--leaf-unpinned")

			require.NoError(t, migrate("--leaf-unpinned"))
		})
}

const addColumnWithUp = `operations:
  - add_column:
      table: u
      up: "1"
      column:
        name: v
        type: integer
        nullable: true
`
