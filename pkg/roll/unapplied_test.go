// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"maps"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

func TestUnappliedMigrations(t *testing.T) {
	t.Parallel()

	t.Run("all migrations are unapplied", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_migration_1.json": &fstest.MapFile{Data: exampleMigration(t, "01_migration_1")},
			"02_migration_2.json": &fstest.MapFile{Data: exampleMigration(t, "02_migration_2")},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Get migrations to apply
			migs, err := roll.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)

			// Assert that all migrations are unapplied
			require.Len(t, migs, 2)
			require.Equal(t, "01_migration_1", migs[0].Name)
			require.Equal(t, "02_migration_2", migs[1].Name)
		})
	})

	t.Run("all but the first migration are unapplied", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_migration_1.json": &fstest.MapFile{Data: exampleMigration(t, "01_migration_1")},
			"02_migration_2.json": &fstest.MapFile{Data: exampleMigration(t, "02_migration_2")},
			"03_migration_3.json": &fstest.MapFile{Data: exampleMigration(t, "03_migration_3")},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Unmarshal the first migration from the migrations directory
			migration, err := migrations.ReadMigration(fs, "01_migration_1.json")
			require.NoError(t, err)

			// Apply the first migration
			err = roll.Start(ctx, migration, backfill.NewConfig())
			require.NoError(t, err)
			err = roll.Complete(ctx)
			require.NoError(t, err)

			// Get migrations to apply
			migs, err := roll.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)

			// Assert that the second and third migrations are unapplied
			require.Len(t, migs, 2)
			require.Equal(t, "02_migration_2", migs[0].Name)
			require.Equal(t, "03_migration_3", migs[1].Name)
		})
	})

	t.Run("all migrations are applied", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_migration_1.json": &fstest.MapFile{Data: exampleMigration(t, "01_migration_1")},
			"02_migration_2.json": &fstest.MapFile{Data: exampleMigration(t, "02_migration_2")},
			"03_migration_3.json": &fstest.MapFile{Data: exampleMigration(t, "03_migration_3")},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Unmarshal and apply all migrations from the migrations directory
			for _, filename := range slices.Sorted(maps.Keys(fs)) {
				migration, err := migrations.ReadMigration(fs, filename)
				require.NoError(t, err)

				err = roll.Start(ctx, migration, backfill.NewConfig())
				require.NoError(t, err)
				err = roll.Complete(ctx)
				require.NoError(t, err)
			}

			// Get migrations to apply
			migs, err := roll.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)

			// Assert that no migrations are unapplied
			require.Len(t, migs, 0)
		})
	})

	t.Run("remote migration history does not match local history", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_migration_1.json": &fstest.MapFile{Data: exampleMigration(t, "01_migration_1")},
			"02_migration_2.json": &fstest.MapFile{Data: exampleMigration(t, "02_migration_2")},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Apply a migration that does not exist in the migrations directory
			err := m.Start(ctx, &migrations.Migration{
				Name: "01a_migration_1a",
				Operations: migrations.Operations{
					&migrations.OpRawSQL{Up: "SELECT 1"},
				},
			}, backfill.NewConfig())
			require.NoError(t, err)
			err = m.Complete(ctx)
			require.NoError(t, err)

			// Get migrations to apply
			_, err = m.UnappliedMigrations(ctx, fs)

			// Assert that a mismatched migration error is returned
			assert.ErrorIs(t, err, roll.ErrMismatchedMigration)
		})
	})

	t.Run("empty directory has no unapplied migrations", func(t *testing.T) {
		fs := fstest.MapFS{}

		testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Get migrations to apply
			migs, err := roll.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)

			// Assert that there are no unapplied migrations
			require.Len(t, migs, 0)
		})
	})

	t.Run("migrations with no name use filename as migration name", func(t *testing.T) {
		// Create migration JSON without a name field
		migrationWithoutName := migrations.Migration{
			Operations: migrations.Operations{
				&migrations.OpRawSQL{Up: "SELECT 1"},
			},
		}
		migBytes, err := json.Marshal(migrationWithoutName)
		require.NoError(t, err)

		fs := fstest.MapFS{
			"03_unnamed_migration.json": &fstest.MapFile{Data: migBytes},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Get migrations to apply
			migs, err := roll.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)

			// Assert that the migration name is taken from the filename
			require.Len(t, migs, 1)
			require.Equal(t, "03_unnamed_migration", migs[0].Name)
		})
	})
}

func TestUnappliedMigrationsWithDivergentHistories(t *testing.T) {
	t.Parallel()

	t.Run("out-of-order applied migrations are tolerated", func(t *testing.T) {
		// Local files: A, B, C, D
		// DB applied: A, C (applied out of filesystem order)
		// Expected unapplied: B, D
		fs := fstest.MapFS{
			"01_migration_A.json": &fstest.MapFile{Data: exampleMigration(t, "01_migration_A")},
			"02_migration_B.json": &fstest.MapFile{Data: exampleMigration(t, "02_migration_B")},
			"03_migration_C.json": &fstest.MapFile{Data: exampleMigration(t, "03_migration_C")},
			"04_migration_D.json": &fstest.MapFile{Data: exampleMigration(t, "04_migration_D")},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Apply A then C (skipping B)
			for _, filename := range []string{"01_migration_A.json", "03_migration_C.json"} {
				migration, err := migrations.ReadMigration(fs, filename)
				require.NoError(t, err)
				err = roll.Start(ctx, migration, backfill.NewConfig())
				require.NoError(t, err)
				err = roll.Complete(ctx)
				require.NoError(t, err)
			}

			// Get unapplied migrations
			migs, err := roll.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)

			// Assert B and D are unapplied, in filesystem order
			require.Len(t, migs, 2)
			require.Equal(t, "02_migration_B", migs[0].Name)
			require.Equal(t, "04_migration_D", migs[1].Name)
		})
	})

	t.Run("hotfix branch scenario: migration applied out of filesystem order", func(t *testing.T) {
		// Local files: A, B, C, D, H (H = hotfix, sorts last)
		// DB applied: A, B, H (hotfix deployed before C, D existed)
		// Expected unapplied: C, D
		fs := fstest.MapFS{
			"01_migration_A.json": &fstest.MapFile{Data: exampleMigration(t, "01_migration_A")},
			"02_migration_B.json": &fstest.MapFile{Data: exampleMigration(t, "02_migration_B")},
			"03_migration_C.json": &fstest.MapFile{Data: exampleMigration(t, "03_migration_C")},
			"04_migration_D.json": &fstest.MapFile{Data: exampleMigration(t, "04_migration_D")},
			"05_migration_H.json": &fstest.MapFile{Data: exampleMigration(t, "05_migration_H")},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Apply A, B, then H (skipping C and D — simulates hotfix deploy)
			for _, filename := range []string{"01_migration_A.json", "02_migration_B.json", "05_migration_H.json"} {
				migration, err := migrations.ReadMigration(fs, filename)
				require.NoError(t, err)
				err = roll.Start(ctx, migration, backfill.NewConfig())
				require.NoError(t, err)
				err = roll.Complete(ctx)
				require.NoError(t, err)
			}

			// Get unapplied migrations
			migs, err := roll.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)

			// Assert C and D are unapplied, in filesystem order
			require.Len(t, migs, 2)
			require.Equal(t, "03_migration_C", migs[0].Name)
			require.Equal(t, "04_migration_D", migs[1].Name)
		})
	})

	t.Run("applied migration not in local files returns error", func(t *testing.T) {
		// Local files: A, B
		// DB applied: A, X (X has no local file)
		// Expected: error
		fs := fstest.MapFS{
			"01_migration_A.json": &fstest.MapFile{Data: exampleMigration(t, "01_migration_A")},
			"02_migration_B.json": &fstest.MapFile{Data: exampleMigration(t, "02_migration_B")},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Apply A
			migration, err := migrations.ReadMigration(fs, "01_migration_A.json")
			require.NoError(t, err)
			err = m.Start(ctx, migration, backfill.NewConfig())
			require.NoError(t, err)
			err = m.Complete(ctx)
			require.NoError(t, err)

			// Apply X (not in local files)
			err = m.Start(ctx, &migrations.Migration{
				Name: "01a_migration_X",
				Operations: migrations.Operations{
					&migrations.OpRawSQL{Up: "SELECT 1"},
				},
			}, backfill.NewConfig())
			require.NoError(t, err)
			err = m.Complete(ctx)
			require.NoError(t, err)

			// Get unapplied migrations
			_, err = m.UnappliedMigrations(ctx, fs)

			// Assert mismatch error
			assert.ErrorIs(t, err, roll.ErrMismatchedMigration)
		})
	})

	t.Run("all migrations applied out of order returns empty unapplied", func(t *testing.T) {
		// Local files: A, B, C
		// DB applied: A, C, B (different order)
		// Expected unapplied: [] (all applied)
		fs := fstest.MapFS{
			"01_migration_A.json": &fstest.MapFile{Data: exampleMigration(t, "01_migration_A")},
			"02_migration_B.json": &fstest.MapFile{Data: exampleMigration(t, "02_migration_B")},
			"03_migration_C.json": &fstest.MapFile{Data: exampleMigration(t, "03_migration_C")},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Apply A, C, B (out of filesystem order)
			for _, filename := range []string{"01_migration_A.json", "03_migration_C.json", "02_migration_B.json"} {
				migration, err := migrations.ReadMigration(fs, filename)
				require.NoError(t, err)
				err = roll.Start(ctx, migration, backfill.NewConfig())
				require.NoError(t, err)
				err = roll.Complete(ctx)
				require.NoError(t, err)
			}

			// Get unapplied migrations
			migs, err := roll.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)

			// Assert no migrations are unapplied
			require.Len(t, migs, 0)
		})
	})
}

func TestUnappliedMigrationsWithBaselines(t *testing.T) {
	t.Parallel()

	t.Run("baseline migration is the only migration in the remote history, local dir is empty", func(t *testing.T) {
		fs := fstest.MapFS{}

		testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Define a baseline migration
			err := roll.CreateBaseline(ctx, "01_initial_version")
			require.NoError(t, err)

			// Get migrations to apply
			migs, err := roll.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)

			// Assert that no migrations are unapplied
			require.Len(t, migs, 0)
		})
	})

	t.Run("baseline migration is the only migration in the remote history, local dir contains only the baseline", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_initial_version.json": &fstest.MapFile{Data: exampleMigration(t, "01_initial_version")},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Define a baseline migration
			err := roll.CreateBaseline(ctx, "01_initial_version")
			require.NoError(t, err)

			// Get migrations to apply
			migs, err := roll.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)

			// Assert that no migrations are unapplied
			require.Len(t, migs, 0)
		})
	})

	t.Run("baseline migration is the only migration in the remote history, all local migrations precede the baseline", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_migration_1.json": &fstest.MapFile{Data: exampleMigration(t, "01_migration_1")},
			"02_migration_2.json": &fstest.MapFile{Data: exampleMigration(t, "02_migration_2")},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Define a baseline migration
			err := roll.CreateBaseline(ctx, "03_baseline_version")
			require.NoError(t, err)

			// Get migrations to apply
			migs, err := roll.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)

			// Assert that no migrations are unapplied - even though all local
			// migrations have not been applied, they all precede the baseline
			require.Len(t, migs, 0)
		})
	})

	t.Run("baseline migration is the only migration in the remote history, one local unapplied migration comes after the baseline", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_migration_1.json":      &fstest.MapFile{Data: exampleMigration(t, "01_migration_1")},
			"02_baseline_version.json": &fstest.MapFile{Data: exampleMigration(t, "02_baseline_version")},
			"03_migration_3.json":      &fstest.MapFile{Data: exampleMigration(t, "03_migration_3")},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Define a baseline migration
			err := roll.CreateBaseline(ctx, "02_baseline_version")
			require.NoError(t, err)

			// Get migrations to apply
			migs, err := roll.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)

			// Assert the unapplied migration is the one that comes after the
			// baseline - the previous migration precedes the baseline so is not
			// considered unapplied.
			require.Len(t, migs, 1)
			require.Equal(t, "03_migration_3", migs[0].Name)
		})
	})

	t.Run("remote history has migrations after the baseline, local has also applied the same migrations after the baseline", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_migration_1.json": &fstest.MapFile{Data: exampleMigration(t, "01_migration_1")},
			// Baseline migration is not present on the filesystem - this is allowed
			"03_migration_3.json": &fstest.MapFile{Data: exampleMigration(t, "03_migration_3")},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Define a baseline migration
			err := roll.CreateBaseline(ctx, "02_baseline_version")
			require.NoError(t, err)

			// Unmarshal the migration that comes after the baseline version
			migration, err := migrations.ReadMigration(fs, "03_migration_3.json")
			require.NoError(t, err)

			// Apply the migration
			err = roll.Start(ctx, migration, backfill.NewConfig())
			require.NoError(t, err)
			err = roll.Complete(ctx)
			require.NoError(t, err)

			// Get migrations to apply
			migs, err := roll.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)

			// Assert that no local migrations are unapplied - the first local
			// migration precedes the baseline version and the other has been applied
			require.Len(t, migs, 0)
		})
	})

	t.Run("multiple baselines in remote history, only the latest baseline matters", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_initial_baseline.json": &fstest.MapFile{Data: exampleMigration(t, "01_initial_baseline")},
			"02_migration_2.json":      &fstest.MapFile{Data: exampleMigration(t, "02_migration_2")},
			// Second baseline is not present on the filesystem - this is allowed
			"04_migration_4.json": &fstest.MapFile{Data: exampleMigration(t, "04_migration_4")},
			"05_migration_5.json": &fstest.MapFile{Data: exampleMigration(t, "05_migration_5")},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Create an early baseline migration
			err := roll.CreateBaseline(ctx, "01_initial_baseline")
			require.NoError(t, err)

			// Create a later baseline migration that supersedes the first one
			err = roll.CreateBaseline(ctx, "03_second_baseline")
			require.NoError(t, err)

			// Get migrations to apply
			migs, err := roll.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)

			// Assert that only migrations after the latest baseline are considered
			// unapplied
			require.Len(t, migs, 2)
			require.Equal(t, "04_migration_4", migs[0].Name)
			require.Equal(t, "05_migration_5", migs[1].Name)
		})
	})

	t.Run("applied migrations between two baselines are ignored", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_migration_1.json":     &fstest.MapFile{Data: exampleMigration(t, "01_migration_1")},
			"02_first_baseline.json":  &fstest.MapFile{Data: exampleMigration(t, "02_first_baseline")},
			"03_migration_3.json":     &fstest.MapFile{Data: exampleMigration(t, "03_migration_3")},
			"04_second_baseline.json": &fstest.MapFile{Data: exampleMigration(t, "04_second_baseline")},
			"05_migration_5.json":     &fstest.MapFile{Data: exampleMigration(t, "05_migration_5")},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Create first baseline
			err := roll.CreateBaseline(ctx, "02_first_baseline")
			require.NoError(t, err)

			// Apply a migration after the first baseline
			migration, err := migrations.ReadMigration(fs, "03_migration_3.json")
			require.NoError(t, err)
			err = roll.Start(ctx, migration, backfill.NewConfig())
			require.NoError(t, err)
			err = roll.Complete(ctx)
			require.NoError(t, err)

			// Create a later baseline that supersedes both the first baseline and
			// the applied migration
			err = roll.CreateBaseline(ctx, "04_second_baseline")
			require.NoError(t, err)

			// Get migrations to apply
			migs, err := roll.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)

			// Assert that only the migration after the latest baseline is considered
			// unapplied
			require.Len(t, migs, 1)
			require.Equal(t, "05_migration_5", migs[0].Name)
		})
	})
}

func TestUnappliedMigrationsWithOldMigrationFormats(t *testing.T) {
	t.Parallel()

	t.Run("local directory contains an un-deserializable migration", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_migration_1.json": &fstest.MapFile{Data: unDeserializableMigration(t, "01_migration_1")},
			"02_migration_2.json": &fstest.MapFile{Data: exampleMigration(t, "02_migration_2")},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Get unapplied migrations
			migs, err := roll.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)

			// Assert that both migrations are unapplied
			require.Len(t, migs, 2)
			require.Equal(t, "01_migration_1", migs[0].Name)
			require.Equal(t, "02_migration_2", migs[1].Name)
		})
	})

	t.Run("remote migration history contains an un-deserializable migration", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_migration_1.json": &fstest.MapFile{Data: exampleMigration(t, "01_migration_1")},
			"02_migration_2.json": &fstest.MapFile{Data: exampleMigration(t, "02_migration_2")},
			"03_migration_3.json": &fstest.MapFile{Data: exampleMigration(t, "03_migration_3")},
		}

		testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, db *sql.DB) {
			ctx := context.Background()

			// Unmarshal the first migration from the migrations directory
			migration, err := migrations.ReadMigration(fs, "01_migration_1.json")
			require.NoError(t, err)

			// Apply the first migration
			err = roll.Start(ctx, migration, backfill.NewConfig())
			require.NoError(t, err)
			err = roll.Complete(ctx)
			require.NoError(t, err)

			// Modify the first migration in the schema history to be un-deserializable; in
			// practice this could happen if the migration was applied with an older
			// version of pgroll that had a different migration format
			_, err = db.ExecContext(ctx, `UPDATE pgroll.migrations
				SET migration = REPLACE(migration::text, '"up"', '"upxxx"')::jsonb
				WHERE name = '01_migration_1'`)
			require.NoError(t, err)

			// Get unapplied migrations
			migs, err := roll.UnappliedMigrations(ctx, fs)
			require.NoError(t, err)

			// Assert that the second and third migrations are unapplied
			require.Len(t, migs, 2)
			require.Equal(t, "02_migration_2", migs[0].Name)
			require.Equal(t, "03_migration_3", migs[1].Name)
		})
	})
}

func exampleMigration(t *testing.T, name string) []byte {
	t.Helper()

	mig := &migrations.Migration{
		Name: name,
		Operations: migrations.Operations{
			&migrations.OpRawSQL{Up: "SELECT 1"},
		},
	}

	bytes, err := json.Marshal(mig)
	require.NoError(t, err)

	return bytes
}

func exampleMigrationWithVersionSchema(t *testing.T, name, versionSchema string) []byte {
	t.Helper()

	mig := &migrations.Migration{
		Name:          name,
		VersionSchema: versionSchema,
		Operations: migrations.Operations{
			&migrations.OpRawSQL{Up: "SELECT 1"},
		},
	}

	bytes, err := json.Marshal(mig)
	require.NoError(t, err)

	return bytes
}

// unDeserializableMigration creates a migration JSON that is valid but
// contains an operation with invalid fields; this could happen if the
// migration wasa created by an older version of `pgroll` before a breaking
// change to the migration format.
func unDeserializableMigration(t *testing.T, name string) []byte {
	t.Helper()

	migJSON := `{
		"operations": [
			{
				"sql": {
					"upxxx": "SELECT 1"
				}
			}
		]
	}`

	return []byte(migJSON)
}
