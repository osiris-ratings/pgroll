// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/pkg/migrations"
)

// TestNotNullConstraintNameHasNoTempToken guards against the PostgreSQL 17+
// leak where a NOT NULL constraint keeps a name derived from pgroll's in-flight
// temp column (`_pgroll_new_*`). PG17 promoted NOT NULL to a real named
// constraint; because pgroll adds/sets NOT NULL against the temp column, PG
// auto-derives a name that embeds the temp token and does not follow the
// column rename at Complete, surfacing explicitly in pg_dump output.
func TestNotNullConstraintNameHasNoTempToken(t *testing.T) {
	t.Parallel()

	ExecuteTests(t, TestCases{
		{
			name: "add NOT NULL column names its constraint canonically",
			migrations: []migrations.Migration{
				{
					Name:          "01_add_table",
					VersionSchema: "add_table",
					Operations: migrations.Operations{
						&migrations.OpCreateTable{
							Name: "users",
							Columns: []migrations.Column{
								{Name: "id", Type: "serial", Pk: true},
							},
						},
					},
				},
				{
					Name:          "02_add_column",
					VersionSchema: "add_column",
					Operations: migrations.Operations{
						&migrations.OpAddColumn{
							Table: "users",
							Column: migrations.Column{
								Name:     "age",
								Type:     "integer",
								Nullable: false,
								Default:  ptr("0"),
							},
						},
					},
				},
			},
			afterComplete: func(t *testing.T, db *sql.DB, schema string) {
				rel := schema + ".users"

				// No NOT NULL constraint may carry the in-flight temp token.
				var leaked int
				require.NoError(t, db.QueryRow(`
					SELECT count(*) FROM pg_constraint
					WHERE conrelid = to_regclass($1)
					  AND contype = 'n'
					  AND conname LIKE '%\_pgroll\_new\_%'`, rel).Scan(&leaked))
				assert.Equal(t, 0, leaked,
					"NOT NULL constraint name must not fossilize the temp column token")

				// On PostgreSQL 17+ NOT NULL is a real constraint (contype='n');
				// it must carry the canonical <table>_<col>_not_null name so
				// pg_dump treats it as the default and omits the CONSTRAINT clause.
				if serverVersionNum(t, db) >= 170000 {
					var name string
					require.NoError(t, db.QueryRow(`
						SELECT con.conname
						FROM pg_constraint con
						JOIN pg_attribute att
						  ON att.attrelid = con.conrelid AND att.attnum = ANY(con.conkey)
						WHERE con.conrelid = to_regclass($1)
						  AND att.attname = 'age'
						  AND con.contype = 'n'
						LIMIT 1`, rel).Scan(&name))
					assert.Equal(t, migrations.CanonicalNotNullName("users", "age"), name)
				}
			},
		},
	})
}

func serverVersionNum(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow("SELECT current_setting('server_version_num')::int").Scan(&n))
	return n
}
