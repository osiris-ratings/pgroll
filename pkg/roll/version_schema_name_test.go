// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

func TestStartRejectsMigrationsWithOverlongVersionSchemaName(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// "public_" is 7 bytes; pick a name that pushes the combined length
		// past the 63-byte Postgres identifier limit.
		longName := strings.Repeat("x", migrations.MaxIdentifierLength-len(cSchema))
		require.Greater(t, len(cSchema)+1+len(longName), migrations.MaxIdentifierLength)

		err := m.Start(ctx, &migrations.Migration{
			Name:       longName,
			Operations: migrations.Operations{createTableOp("new_table")},
		}, backfill.NewConfig())

		var tooLong migrations.VersionSchemaNameTooLongError
		require.ErrorAs(t, err, &tooLong)
		assert.Equal(t, cSchema, tooLong.Schema)
		assert.Equal(t, longName, tooLong.VersionName)
		assert.Equal(t, migrations.MaxIdentifierLength, tooLong.Max)

		// The truncated schema must not have been created.
		truncated := roll.VersionedSchemaName(cSchema, longName)[:migrations.MaxIdentifierLength]
		assert.False(t, schemaExists(t, db, truncated),
			"expected truncated schema %q not to have been created", truncated)

		// And the migration must not have been recorded in pgroll state.
		var count int
		require.NoError(t, db.QueryRowContext(ctx,
			"SELECT count(*) FROM pgroll.migrations WHERE schema = $1 AND name = $2",
			cSchema, longName).Scan(&count))
		assert.Equal(t, 0, count, "migration row should not have been inserted")
	})
}

func TestStartAcceptsMigrationAtVersionSchemaNameLimit(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// Construct a name that hits the 63-byte limit exactly:
		// len("public") + 1 ("_") + len(name) == 63.
		name := strings.Repeat("x", migrations.MaxIdentifierLength-len(cSchema)-1)
		require.Equal(t, migrations.MaxIdentifierLength, len(cSchema)+1+len(name))

		err := m.Start(ctx, &migrations.Migration{
			Name:       name,
			Operations: migrations.Operations{createTableOp("new_table")},
		}, backfill.NewConfig())
		require.NoError(t, err)

		// And the version schema actually exists at the full computed name.
		full := roll.VersionedSchemaName(cSchema, name)
		assert.True(t, schemaExists(t, db, full))
	})
}
