// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

// targetedMigration builds a migration file carrying an explicit `targets`
// list. A nil targets slice produces a file with no `targets` key at all,
// which is what every migration authored before the field existed looks like.
func targetedMigration(t *testing.T, name string, targets []string) []byte {
	t.Helper()

	mig := &migrations.Migration{
		Name:       name,
		Targets:    targets,
		Operations: migrations.Operations{&migrations.OpRawSQL{Up: "SELECT 1"}},
	}

	b, err := json.Marshal(mig)
	require.NoError(t, err)
	return b
}

// cloneFixture is the shape this whole feature exists for: a directory where
// some migrations belong to one target, some to another, and some to both.
func cloneFixture(t *testing.T) fstest.MapFS {
	t.Helper()

	return fstest.MapFS{
		"01_app_only.json":   &fstest.MapFile{Data: targetedMigration(t, "01_app_only", []string{"app"})},
		"02_shared.json":     &fstest.MapFile{Data: targetedMigration(t, "02_shared", []string{"app", "etl"})},
		"03_app_only_2.json": &fstest.MapFile{Data: targetedMigration(t, "03_app_only_2", []string{"app"})},
		"04_etl_new.json":    &fstest.MapFile{Data: targetedMigration(t, "04_etl_new", []string{"etl"})},
	}
}

// applyAll runs every migration in the fixture through an untargeted migrator,
// which is how the database being cloned got its history.
func applyAll(t *testing.T, ctx context.Context, m *roll.Roll, fs fstest.MapFS, names ...string) {
	t.Helper()

	for _, name := range names {
		raw, err := migrations.ReadRawMigration(fs, name+".json")
		require.NoError(t, err)
		mig, err := migrations.ParseMigration(raw)
		require.NoError(t, err)
		require.NoError(t, m.Start(ctx, mig, backfill.NewConfig()))
		require.NoError(t, m.Complete(ctx))
	}
}

func TestTargetFiltering(t *testing.T) {
	t.Parallel()

	// The load-bearing test. A database that inherited another target's
	// history must still validate — validation reads the unfiltered directory
	// — and must apply only the migrations its own target selects.
	//
	// If someone ever filters the validation pass as well as the selection
	// pass, this fails with ErrMismatchedMigration on "01_app_only", and the
	// whole "no re-baseline, no cutover" property is gone.
	t.Run("a cloned database keeps another target's history", func(t *testing.T) {
		fs := cloneFixture(t)

		testutils.WithMigratorAndConnStrToContainer(t, []roll.Option{roll.WithLockTimeoutMs(500)},
			func(seed *roll.Roll, connStr string, _ *sql.DB) {
				ctx := context.Background()

				// The app database applies its own chain. The ETL host is a
				// volume clone of it, so it starts life with exactly this
				// history even though it is not the app target.
				applyAll(t, ctx, seed, fs, "01_app_only", "02_shared", "03_app_only_2")

				etl := testutils.NewMigratorForConnStr(t, connStr,
					roll.WithLockTimeoutMs(500), roll.WithTarget("etl"))

				res, err := etl.ResolveMigrations(ctx, fs)
				require.NoError(t, err, "inherited app-only history must not be a mismatch")

				require.Len(t, res.Apply, 1)
				require.Equal(t, "04_etl_new", res.Apply[0].Name)
			})
	})

	t.Run("no target applies everything", func(t *testing.T) {
		fs := cloneFixture(t)

		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			migs, err := m.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)
			require.Len(t, migs, 4, "targets are ignored entirely when no --target is given")
		})
	})

	t.Run("the app target selects its own migrations", func(t *testing.T) {
		fs := cloneFixture(t)

		testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
			[]roll.Option{roll.WithLockTimeoutMs(500), roll.WithTarget("app")},
			func(m *roll.Roll, _ *sql.DB) {
				ctx := context.Background()

				migs, err := m.UnappliedMigrations(ctx, fs)
				require.NoError(t, err)
				require.Len(t, migs, 3)
				require.Equal(t, "01_app_only", migs[0].Name)
				require.Equal(t, "02_shared", migs[1].Name)
				require.Equal(t, "03_app_only_2", migs[2].Name)
			})
	})

	t.Run("an untagged candidate is a hard error under a target", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_tagged.json":   &fstest.MapFile{Data: targetedMigration(t, "01_tagged", []string{"etl"})},
			"02_untagged.json": &fstest.MapFile{Data: targetedMigration(t, "02_untagged", nil)},
		}

		testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
			[]roll.Option{roll.WithLockTimeoutMs(500), roll.WithTarget("etl")},
			func(m *roll.Roll, _ *sql.DB) {
				ctx := context.Background()

				_, err := m.UnappliedMigrations(ctx, fs)
				require.Error(t, err)
				require.ErrorContains(t, err, "02_untagged.json")
				require.ErrorContains(t, err, "targets")
			})
	})

	t.Run("an untagged migration is fine with no target", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_untagged.json": &fstest.MapFile{Data: targetedMigration(t, "01_untagged", nil)},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			migs, err := m.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)
			require.Len(t, migs, 1)
		})
	})

	// The no-back-stamping property: adopting --target on a database that
	// already has history must not require every historical file to be tagged.
	t.Run("an untagged migration that is already applied is not inspected", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_untagged.json": &fstest.MapFile{Data: targetedMigration(t, "01_untagged", nil)},
			"02_etl.json":      &fstest.MapFile{Data: targetedMigration(t, "02_etl", []string{"etl"})},
		}

		testutils.WithMigratorAndConnStrToContainer(t, []roll.Option{roll.WithLockTimeoutMs(500)},
			func(seed *roll.Roll, connStr string, _ *sql.DB) {
				ctx := context.Background()

				applyAll(t, ctx, seed, fs, "01_untagged")

				etl := testutils.NewMigratorForConnStr(t, connStr,
					roll.WithLockTimeoutMs(500), roll.WithTarget("etl"))

				migs, err := etl.UnappliedMigrations(ctx, fs)
				require.NoError(t, err, "an applied migration is never inspected for targets")
				require.Len(t, migs, 1)
				require.Equal(t, "02_etl", migs[0].Name)
			})
	})

	t.Run("target names are matched verbatim", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_etl.json": &fstest.MapFile{Data: targetedMigration(t, "01_etl", []string{"ETL"})},
		}

		testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
			[]roll.Option{roll.WithLockTimeoutMs(500), roll.WithTarget("etl")},
			func(m *roll.Roll, _ *sql.DB) {
				ctx := context.Background()

				migs, err := m.UnappliedMigrations(ctx, fs)
				require.NoError(t, err, `"ETL" is a declared target, so this is not an untagged candidate`)
				require.Empty(t, migs, `"ETL" and "etl" are different targets`)
			})
	})

	// depends_on expresses ordering. A dependency this target never applies
	// imposes no ordering here, so it must not wedge the run.
	t.Run("a dependency on an excluded migration is satisfied by construction", func(t *testing.T) {
		dependent := &migrations.Migration{
			Name:       "02_etl",
			Targets:    []string{"etl"},
			DependsOn:  []string{"01_app"},
			Operations: migrations.Operations{&migrations.OpRawSQL{Up: "SELECT 1"}},
		}
		body, err := json.Marshal(dependent)
		require.NoError(t, err)

		fs := fstest.MapFS{
			"01_app.json": &fstest.MapFile{Data: targetedMigration(t, "01_app", []string{"app"})},
			"02_etl.json": &fstest.MapFile{Data: body},
		}

		testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
			[]roll.Option{roll.WithLockTimeoutMs(500), roll.WithTarget("etl")},
			func(m *roll.Roll, _ *sql.DB) {
				ctx := context.Background()

				res, err := m.ResolveMigrations(ctx, fs)
				require.NoError(t, err)
				require.Len(t, res.Apply, 1)
				require.Equal(t, "02_etl", res.Apply[0].Name)
				require.Contains(t, res.Excluded, "01_app")
			})
	})
}

// TestPlanWithTarget locks the catastrophic-revert failure out. Filtering
// plan's localSet as well as its forward leg makes every inherited app-only
// migration read as "absent from the checkout", and the planner then proposes
// reverting the entire history.
func TestPlanWithTarget(t *testing.T) {
	t.Parallel()

	t.Run("a cloned database is not proposed for revert", func(t *testing.T) {
		fs := cloneFixture(t)

		testutils.WithMigratorAndConnStrToContainer(t, []roll.Option{roll.WithLockTimeoutMs(500)},
			func(seed *roll.Roll, connStr string, _ *sql.DB) {
				ctx := context.Background()

				applyAll(t, ctx, seed, fs, "01_app_only", "02_shared", "03_app_only_2")

				etl := testutils.NewMigratorForConnStr(t, connStr,
					roll.WithLockTimeoutMs(500), roll.WithTarget("etl"))

				res, err := etl.Plan(ctx, fs)
				require.NoError(t, err)

				require.Equal(t, 0, res.Revert.Count,
					"inherited app-only history must never be proposed for revert")
				require.Equal(t, 0, res.Blocked.Count,
					"inherited app-only history is not interleaved history")
				require.Equal(t, []string{"04_etl_new"}, res.Apply.Migrations)
				require.Equal(t, "etl", res.Target)
				require.False(t, res.Diverged)
			})
	})

	t.Run("a converged target reports in sync despite a foreign leaf", func(t *testing.T) {
		fs := cloneFixture(t)

		testutils.WithMigratorAndConnStrToContainer(t, []roll.Option{roll.WithLockTimeoutMs(500)},
			func(seed *roll.Roll, connStr string, _ *sql.DB) {
				ctx := context.Background()

				// Everything applied, including the etl migration — but the
				// history leaf is an app-only migration this target never
				// selects, so leaf equality cannot be the convergence signal.
				applyAll(t, ctx, seed, fs, "01_app_only", "02_shared", "04_etl_new", "03_app_only_2")

				etl := testutils.NewMigratorForConnStr(t, connStr,
					roll.WithLockTimeoutMs(500), roll.WithTarget("etl"))

				res, err := etl.Plan(ctx, fs)
				require.NoError(t, err)

				require.Equal(t, 0, res.Apply.Count)
				require.Equal(t, 0, res.Revert.Count)
				require.Equal(t, 0, res.Blocked.Count)
				require.True(t, res.InSync)
			})
	})
}
