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
