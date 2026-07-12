// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

// localFS builds an in-memory migrations directory whose file (and therefore
// migration) names are the given names. Only the names matter to planning —
// the operations are inert placeholders.
func localFS(t *testing.T, names ...string) fstest.MapFS {
	t.Helper()
	fs := fstest.MapFS{}
	for _, n := range names {
		fs[n+".json"] = &fstest.MapFile{Data: exampleMigration(t, n)}
	}
	return fs
}

// createTable returns a minimal, invertible migration.
func createTable(name, table string) *migrations.Migration {
	return &migrations.Migration{
		Name: name,
		Operations: migrations.Operations{
			&migrations.OpRawSQL{
				Up:   "CREATE TABLE " + table + " (id integer PRIMARY KEY)",
				Down: "DROP TABLE " + table,
			},
		},
	}
}

// applyAndSeal applies a migration and contracts (seals) it immediately — the
// completed, no-longer-revertible-losslessly state.
func applyAndSeal(t *testing.T, mig *roll.Roll, m *migrations.Migration) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, mig.Start(ctx, m, backfill.NewConfig()))
	require.NoError(t, mig.Complete(ctx))
}

func vs(name string) string { return roll.VersionedSchemaName(cSchema, name) }

// TestPlanFreshDatabase: an empty database with local migrations is a pure
// forward apply.
func TestPlanFreshDatabase(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, _ *sql.DB) {
		ctx := context.Background()

		plan, err := mig.Plan(ctx, localFS(t, "00_a", "01_b"))
		require.NoError(t, err)

		assert.Equal(t, "public", plan.Schema)
		assert.Equal(t, string(roll.NoneMigrationStatus), plan.Status)
		assert.Nil(t, plan.LiveSchema)
		assert.Nil(t, plan.DBLatest)
		assert.Nil(t, plan.ActiveMigration)
		require.NotNil(t, plan.LocalLatest)
		assert.Equal(t, "01_b", *plan.LocalLatest)
		assert.False(t, plan.InSync)
		assert.False(t, plan.Diverged)

		assert.Equal(t, 2, plan.Apply.Count)
		assert.Equal(t, []string{"00_a", "01_b"}, plan.Apply.Migrations)
		assert.Equal(t, 0, plan.Revert.Count)
		assert.Equal(t, 0, plan.Blocked.Count)
	})
}

// TestPlanInSync: database leaf equals local leaf and the deployment is
// contracted — nothing to do.
func TestPlanInSync(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, _ *sql.DB) {
		ctx := context.Background()

		applyAndSeal(t, mig, createTable("00_a", "t_a"))
		applyAndSeal(t, mig, createTable("01_b", "t_b"))

		plan, err := mig.Plan(ctx, localFS(t, "00_a", "01_b"))
		require.NoError(t, err)

		assert.Equal(t, string(roll.CompleteMigrationStatus), plan.Status)
		assert.True(t, plan.InSync)
		assert.False(t, plan.Diverged)
		assert.Equal(t, 0, plan.Apply.Count)
		assert.Equal(t, 0, plan.Revert.Count)
		assert.Equal(t, 0, plan.Blocked.Count)
		require.NotNil(t, plan.DBLatest)
		require.NotNil(t, plan.LocalLatest)
		assert.Equal(t, *plan.DBLatest, *plan.LocalLatest)
	})
}

// TestPlanApplyOnly: the database is a prefix of the checkout — forward only,
// no revert, not in sync.
func TestPlanApplyOnly(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, _ *sql.DB) {
		ctx := context.Background()

		applyAndSeal(t, mig, createTable("00_a", "t_a"))

		plan, err := mig.Plan(ctx, localFS(t, "00_a", "01_b", "02_c"))
		require.NoError(t, err)

		assert.Equal(t, string(roll.CompleteMigrationStatus), plan.Status)
		assert.False(t, plan.InSync)
		assert.False(t, plan.Diverged)
		assert.Equal(t, []string{"01_b", "02_c"}, plan.Apply.Migrations)
		assert.Equal(t, 0, plan.Revert.Count)
		assert.Equal(t, 0, plan.Blocked.Count)
		require.NotNil(t, plan.DBLatest)
		assert.Equal(t, "00_a", *plan.DBLatest)
	})
}

// TestPlanRevertInFlight: the checkout drops the in-flight window on top of a
// sealed boundary — a lossless window revert, no contracted targets.
func TestPlanRevertInFlight(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, _ *sql.DB) {
		ctx := context.Background()

		applyAndSeal(t, mig, createTable("00_a", "t_a"))
		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			createTable("01_b", "t_b"),
			createTable("02_c", "t_c"),
		})

		plan, err := mig.Plan(ctx, localFS(t, "00_a"))
		require.NoError(t, err)

		assert.Equal(t, 0, plan.Apply.Count)
		assert.Equal(t, 0, plan.Blocked.Count)
		assert.Equal(t, 2, plan.Revert.Count)
		assert.Equal(t, []string{"02_c", "01_b"}, plan.Revert.Migrations)
		assert.False(t, plan.Revert.ContainsContracted)
		assert.True(t, plan.Revert.Contiguous)
		require.NotNil(t, plan.Revert.To)
		assert.Equal(t, "00_a", *plan.Revert.To)
		require.NotNil(t, plan.Revert.ToSchema)
		assert.Equal(t, vs("00_a"), *plan.Revert.ToSchema)
		assert.Equal(t, []string{vs("02_c"), vs("01_b")}, plan.Revert.WouldDropSchemas)
	})
}

// TestPlanRevertContracted: the checkout drops a contracted deployment — the
// revert proceeds by inversion (contains_contracted, drop-set populated).
func TestPlanRevertContracted(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, _ *sql.DB) {
		ctx := context.Background()

		applyAndSeal(t, mig, createTable("00_a", "t_a"))
		applyAndSeal(t, mig, createTable("01_b", "t_b"))

		plan, err := mig.Plan(ctx, localFS(t, "00_a"))
		require.NoError(t, err)

		assert.Equal(t, 0, plan.Apply.Count)
		assert.Equal(t, 0, plan.Blocked.Count)
		assert.Equal(t, 1, plan.Revert.Count)
		assert.Equal(t, []string{"01_b"}, plan.Revert.Migrations)
		assert.True(t, plan.Revert.ContainsContracted)
		require.NotNil(t, plan.Revert.To)
		assert.Equal(t, "00_a", *plan.Revert.To)
		require.NotNil(t, plan.Revert.ToSchema)
		assert.Equal(t, vs("00_a"), *plan.Revert.ToSchema)
		assert.Equal(t, []string{vs("01_b")}, plan.Revert.WouldDropSchemas)
	})
}

// TestPlanComposedWindowAndSealed: an open window sits above a contracted
// segment; a revert to the boundary composes both legs (contains_contracted).
func TestPlanComposedWindowAndSealed(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, _ *sql.DB) {
		ctx := context.Background()

		applyAndSeal(t, mig, createTable("00_a", "t_a"))
		applyAndSeal(t, mig, createTable("01_b", "t_b"))
		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			createTable("02_c", "t_c"),
			createTable("03_d", "t_d"),
		})

		plan, err := mig.Plan(ctx, localFS(t, "00_a"))
		require.NoError(t, err)

		assert.Equal(t, 0, plan.Apply.Count)
		assert.Equal(t, 0, plan.Blocked.Count)
		// Window leg (newest first) then the contracted leg.
		assert.Equal(t, []string{"03_d", "02_c", "01_b"}, plan.Revert.Migrations)
		assert.True(t, plan.Revert.ContainsContracted)
		require.NotNil(t, plan.Revert.To)
		assert.Equal(t, "00_a", *plan.Revert.To)
		assert.Equal(t, []string{vs("03_d"), vs("02_c"), vs("01_b")}, plan.Revert.WouldDropSchemas)
	})
}

// TestPlanDiverged: the histories fork above a shared point — both an apply
// and a revert leg, with diverged flagged.
func TestPlanDiverged(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, _ *sql.DB) {
		ctx := context.Background()

		applyAndSeal(t, mig, createTable("00_a", "t_a"))
		applyAndSeal(t, mig, createTable("01_b", "t_b"))
		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			createTable("02_c", "t_c"),
		})

		// Local forks at 01_b: it drops 02_c and adds 03_d, 04_e.
		plan, err := mig.Plan(ctx, localFS(t, "00_a", "01_b", "03_d", "04_e"))
		require.NoError(t, err)

		assert.True(t, plan.Diverged)
		assert.Equal(t, []string{"03_d", "04_e"}, plan.Apply.Migrations)
		assert.Equal(t, []string{"02_c"}, plan.Revert.Migrations)
		assert.False(t, plan.Revert.ContainsContracted)
		require.NotNil(t, plan.Revert.To)
		assert.Equal(t, "01_b", *plan.Revert.To)
		assert.Equal(t, 0, plan.Blocked.Count)
	})
}

// TestPlanBlockedNonContiguous: a database migration absent from the checkout
// sits beneath an in-checkout leaf — it cannot be cleanly reverted.
func TestPlanBlockedNonContiguous(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, _ *sql.DB) {
		ctx := context.Background()

		applyAndSeal(t, mig, createTable("00_a", "t_a"))
		applyAndSeal(t, mig, createTable("01_b", "t_b"))
		applyAndSeal(t, mig, createTable("02_c", "t_c"))

		// Local keeps 00_a and 02_c but omits 01_b.
		plan, err := mig.Plan(ctx, localFS(t, "00_a", "02_c"))
		require.NoError(t, err)

		assert.Equal(t, 0, plan.Apply.Count)
		assert.Equal(t, 0, plan.Revert.Count)
		assert.False(t, plan.Diverged)
		assert.Equal(t, 1, plan.Blocked.Count)
		require.NotNil(t, plan.Blocked.Reason)
		assert.Equal(t, "non-contiguous", *plan.Blocked.Reason)
		assert.Equal(t, []string{"01_b"}, plan.Blocked.Migrations)
	})
}

// TestPlanExplicitTarget: --to overrides the convergence target with an
// explicit (already-applied) revert boundary; the forward leg is empty.
func TestPlanExplicitTarget(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, _ *sql.DB) {
		ctx := context.Background()

		applyAndSeal(t, mig, createTable("00_a", "t_a"))
		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			createTable("01_b", "t_b"),
			createTable("02_c", "t_c"),
		})

		plan, err := mig.Plan(ctx, localFS(t, "00_a", "01_b", "02_c"), roll.WithPlanTo("00_a"))
		require.NoError(t, err)

		assert.Equal(t, 0, plan.Apply.Count)
		assert.Equal(t, []string{"02_c", "01_b"}, plan.Revert.Migrations)
		require.NotNil(t, plan.Revert.To)
		assert.Equal(t, "00_a", *plan.Revert.To)
		assert.Equal(t, 0, plan.Blocked.Count)
	})
}

// TestPlanExplicitTargetNotFound: --to naming a migration absent from history
// is the one convergence error that fails (non-zero exit at the CLI).
func TestPlanExplicitTargetNotFound(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, _ *sql.DB) {
		ctx := context.Background()

		applyAndSeal(t, mig, createTable("00_a", "t_a"))

		_, err := mig.Plan(ctx, localFS(t, "00_a"), roll.WithPlanTo("99_nope"))
		require.Error(t, err)
		assert.ErrorContains(t, err, "not found in database history")
	})
}

// TestPlanNotInSyncWhenBackdatedMigrationUnapplied: leaf equality alone must
// not report in_sync — a checkout migration older than the shared leaf that
// was never applied still needs applying.
func TestPlanNotInSyncWhenBackdatedMigrationUnapplied(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, _ *sql.DB) {
		ctx := context.Background()

		// DB applied 00_a then 02_c, skipping 01_b (added late on a branch).
		applyAndSeal(t, mig, createTable("00_a", "t_a"))
		applyAndSeal(t, mig, createTable("02_c", "t_c"))

		plan, err := mig.Plan(ctx, localFS(t, "00_a", "01_b", "02_c"))
		require.NoError(t, err)

		require.NotNil(t, plan.DBLatest)
		require.NotNil(t, plan.LocalLatest)
		assert.Equal(t, *plan.DBLatest, *plan.LocalLatest) // both 02_c
		assert.False(t, plan.InSync, "an unapplied older migration must defeat in_sync")
		assert.Equal(t, []string{"01_b"}, plan.Apply.Migrations)
		assert.Equal(t, 0, plan.Revert.Count)
		assert.Equal(t, 0, plan.Blocked.Count)
	})
}

// TestPlanExplicitTargetBaseline: --to naming the baseline is accepted (a
// legal revert boundary) rather than rejected as "not found in history".
func TestPlanExplicitTargetBaseline(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, _ *sql.DB) {
		ctx := context.Background()

		require.NoError(t, mig.CreateBaseline(ctx, "00_baseline"))
		applyAndSeal(t, mig, createTable("01_a", "t_a"))

		plan, err := mig.Plan(ctx, localFS(t, "00_baseline"), roll.WithPlanTo("00_baseline"))
		require.NoError(t, err)

		assert.Equal(t, 0, plan.Apply.Count)
		assert.Equal(t, 0, plan.Blocked.Count)
		assert.Equal(t, []string{"01_a"}, plan.Revert.Migrations)
		require.NotNil(t, plan.Revert.To)
		assert.Equal(t, "00_baseline", *plan.Revert.To)
	})
}

// migrationsSignature snapshots the observable state of the pgroll history for
// a schema so a dry-run can be asserted to leave it untouched.
func migrationsSignature(t *testing.T, db *sql.DB, schema string) string {
	t.Helper()
	var sig string
	err := db.QueryRow(`
		SELECT COALESCE(string_agg(name || ':' || done || ':' || sealed, ',' ORDER BY created_at), '')
		FROM pgroll.migrations WHERE schema = $1`, schema).Scan(&sig)
	require.NoError(t, err)
	return sig
}

// TestPreviewRevertDryRun exercises the standalone revert preview for each
// bound (bare / --steps / --to) and asserts it makes no state changes.
func TestPreviewRevertDryRun(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		applyAndSeal(t, mig, createTable("00_a", "t_a"))
		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			createTable("01_b", "t_b"),
			createTable("02_c", "t_c"),
		})

		before := migrationsSignature(t, db, cSchema)

		// Bare: the whole in-flight window, restoring onto the sealed parent.
		bare, err := mig.PreviewRevert(ctx)
		require.NoError(t, err)
		assert.Equal(t, []string{"02_c", "01_b"}, bare.Migrations)
		assert.False(t, bare.ContainsContracted)
		require.NotNil(t, bare.To)
		assert.Equal(t, "00_a", *bare.To)

		// --steps 1: only the newest in-flight migration.
		stepped, err := mig.PreviewRevert(ctx, roll.WithRevertSteps(1))
		require.NoError(t, err)
		assert.Equal(t, []string{"02_c"}, stepped.Migrations)
		require.NotNil(t, stepped.To)
		assert.Equal(t, "01_b", *stepped.To)

		// --to 00_a: reverts everything newer than the named migration.
		toPlan, err := mig.PreviewRevert(ctx, roll.WithRevertTo("00_a"))
		require.NoError(t, err)
		assert.Equal(t, []string{"02_c", "01_b"}, toPlan.Migrations)
		require.NotNil(t, toPlan.To)
		assert.Equal(t, "00_a", *toPlan.To)

		after := migrationsSignature(t, db, cSchema)
		assert.Equal(t, before, after, "revert --dry-run must not change pgroll.migrations")
	})
}
