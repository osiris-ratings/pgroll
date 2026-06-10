// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

// migrateAndComplete runs Start+Complete for each migration in order,
// leaving the chain fully completed — the state a shared database is in
// after a branch's migrations were applied and completed.
func migrateAndComplete(t *testing.T, ctx context.Context, m *roll.Roll, migs ...*migrations.Migration) {
	t.Helper()
	for _, mig := range migs {
		require.NoError(t, m.Start(ctx, mig, backfill.NewConfig()))
		require.NoError(t, m.Complete(ctx))
	}
}

// historyChain returns (name, parent) pairs for the schema's history in
// created_at order, parent rendered as "" for NULL.
func historyChain(t *testing.T, ctx context.Context, db *sql.DB) [][2]string {
	t.Helper()
	rows, err := db.QueryContext(ctx,
		`SELECT name, COALESCE(parent, '') FROM pgroll.migrations
		 WHERE schema = $1 ORDER BY created_at`, cSchema)
	require.NoError(t, err)
	defer rows.Close()

	var chain [][2]string
	for rows.Next() {
		var name, parent string
		require.NoError(t, rows.Scan(&name, &parent))
		chain = append(chain, [2]string{name, parent})
	}
	require.NoError(t, rows.Err())
	return chain
}

func TestPruneRemovesCompletedTailAndRewiresChain(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		migrateAndComplete(
			t, ctx, m,
			&migrations.Migration{Name: "01_keep", Operations: migrations.Operations{createTableOp("table1")}},
			&migrations.Migration{Name: "02_branch", Operations: migrations.Operations{createTableOp("table2")}},
			&migrations.Migration{Name: "03_branch", Operations: migrations.Operations{createTableOp("table3")}},
		)

		pruned, err := m.Prune(ctx, []string{"02_branch", "03_branch"})
		require.NoError(t, err)
		require.Len(t, pruned, 2)
		assert.Equal(t, "02_branch", pruned[0].Name)
		assert.Equal(t, "03_branch", pruned[1].Name)

		// Only the keeper remains, as a valid chain root.
		assert.Equal(t, [][2]string{{"01_keep", ""}}, historyChain(t, ctx, db))

		// latest_migration() resolves the new leaf via the parent chain.
		latest, err := m.State().LatestMigration(ctx, m.Schema())
		require.NoError(t, err)
		require.NotNil(t, latest)
		assert.Equal(t, "01_keep", *latest)

		// The pruned migrations' version schemas are gone (03's existed as
		// the live leaf; 02's was already reaped when 03 completed).
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "02_branch")))
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "03_branch")))

		// Physical effects of completed migrations are NOT reverted.
		_, err = db.ExecContext(ctx, "SELECT * FROM table3")
		assert.NoError(t, err)
	})
}

func TestPruneMidChainRewiresParentAcrossGap(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		migrateAndComplete(
			t, ctx, m,
			&migrations.Migration{Name: "01_first", Operations: migrations.Operations{createTableOp("table1")}},
			&migrations.Migration{Name: "02_middle", Operations: migrations.Operations{createTableOp("table2")}},
			&migrations.Migration{Name: "03_last", Operations: migrations.Operations{createTableOp("table3")}},
		)

		_, err := m.Prune(ctx, []string{"02_middle"})
		require.NoError(t, err)

		assert.Equal(t, [][2]string{
			{"01_first", ""},
			{"03_last", "01_first"},
		}, historyChain(t, ctx, db))

		// The leaf and its version schema are untouched.
		latest, err := m.State().LatestMigration(ctx, m.Schema())
		require.NoError(t, err)
		require.NotNil(t, latest)
		assert.Equal(t, "03_last", *latest)
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "03_last")))
	})
}

func TestPruneHonorsVersionSchemaField(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		migrateAndComplete(
			t, ctx, m,
			&migrations.Migration{Name: "01_keep", Operations: migrations.Operations{createTableOp("table1")}},
			&migrations.Migration{
				Name:          "02_branch",
				VersionSchema: "custom_version",
				Operations:    migrations.Operations{createTableOp("table2")},
			},
		)
		require.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "custom_version")))

		pruned, err := m.Prune(ctx, []string{"02_branch"})
		require.NoError(t, err)
		require.Len(t, pruned, 1)
		assert.Equal(t, "custom_version", pruned[0].VersionSchema)

		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "custom_version")))
	})
}

func TestPruneRefusesUnknownMigration(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		migrateAndComplete(
			t, ctx, m,
			&migrations.Migration{Name: "01_keep", Operations: migrations.Operations{createTableOp("table1")}},
		)

		_, err := m.Prune(ctx, []string{"01_keep", "02_missing"})
		require.ErrorContains(t, err, "02_missing")
		require.ErrorContains(t, err, "not found")

		// Nothing was modified.
		assert.Equal(t, [][2]string{{"01_keep", ""}}, historyChain(t, ctx, db))
	})
}

func TestPruneRefusesWhileMigrationActive(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		migrateAndComplete(
			t, ctx, m,
			&migrations.Migration{Name: "01_done", Operations: migrations.Operations{createTableOp("table1")}},
		)
		require.NoError(t, m.Start(ctx, &migrations.Migration{
			Name:       "02_active",
			Operations: migrations.Operations{createTableOp("table2")},
		}, backfill.NewConfig()))

		// Refuses both the active migration itself and completed rows
		// beneath it.
		_, err := m.Prune(ctx, []string{"02_active"})
		require.ErrorContains(t, err, "in progress")
		require.ErrorContains(t, err, "rollback")

		_, err = m.Prune(ctx, []string{"01_done"})
		require.ErrorContains(t, err, "in progress")
	})
}

func TestPruneRefusesBaseline(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		require.NoError(t, m.CreateBaseline(ctx, "01_baseline"))

		_, err := m.Prune(ctx, []string{"01_baseline"})
		require.ErrorContains(t, err, "baseline")
	})
}
