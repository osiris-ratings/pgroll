// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

func migrateCmd() *cobra.Command {
	var complete, expectOne bool
	var batchSize int
	var batchDelay time.Duration

	migrateCmd := &cobra.Command{
		Use:       "migrate <directory>",
		Short:     "Apply outstanding migrations from a directory to a database",
		Example:   "migrate ./migrations",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"directory"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			migrationsDir := args[0]

			// Create a roll instance and check if pgroll is initialized
			m, err := NewRollWithInitCheck(ctx)
			if err != nil {
				return err
			}
			defer m.Close()

			latestMigration, err := m.State().LatestMigration(ctx, m.Schema())
			if err != nil {
				return fmt.Errorf("unable to determine latest version: %w", err)
			}

			active, err := m.State().IsActiveMigrationPeriod(ctx, m.Schema())
			if err != nil {
				return fmt.Errorf("unable to determine active migration period: %w", err)
			}
			if active {
				return fmt.Errorf(
					"migration %q is in progress and was not completed; "+
						"this usually means a previous run was interrupted "+
						"(e.g. lock_timeout under contention or SIGINT). "+
						"Run `pgroll rollback` to clean up before retrying",
					*latestMigration,
				)
			}

			info, err := os.Stat(migrationsDir)
			if err != nil {
				return fmt.Errorf("failed to stat directory: %w", err)
			}
			if !info.IsDir() {
				return fmt.Errorf("migrations directory %q is not a directory", migrationsDir)
			}

			// Check whether the schema needs an initial baseline migration
			needsBaseline, err := m.State().HasExistingSchemaWithoutHistory(ctx, m.Schema())
			if err != nil {
				return fmt.Errorf("failed to check for existing schema: %w", err)
			}
			if needsBaseline {
				fmt.Printf("Schema %q is non-empty but has no migration history. Run `pgroll baseline` first\n", m.Schema())
				return nil
			}

			rawMigs, err := m.UnappliedMigrations(ctx, os.DirFS(migrationsDir))
			if err != nil {
				return fmt.Errorf("failed to get migrations to apply: %w", err)
			}

			if len(rawMigs) == 0 {
				fmt.Println("Database is up to date; no migrations to apply")
				return nil
			}

			// In 'expect one' mode, abort if there is more than one unapplied migration
			if expectOne && len(rawMigs) > 1 {
				return fmt.Errorf("expected one migration to apply but found %d", len(rawMigs))
			}

			// fail early if there is an incompatible migration
			migs, err := parseMigrations(rawMigs)
			if err != nil {
				return fmt.Errorf("failed to run migrate: %w", err)
			}

			backfillConfig := backfill.NewConfig(
				backfill.WithBatchSize(batchSize),
				backfill.WithBatchDelay(batchDelay),
			)

			// Apply each intermediate migration without projecting a version
			// schema. No apps will ever connect to an intermediate version, so
			// projecting it would just waste a schema and create view
			// dependencies that block destructive operations later in the
			// batch. WithSkipSchemaDrop on Complete prevents the intermediate
			// from dropping the production-active version schema. Net effect:
			// at any point during the run, the only version schema that
			// exists is the production-active one — until the final
			// migration's Start creates the new target.
			for _, mig := range migs[:len(migs)-1] {
				if err := runMigration(
					ctx, m, mig, true, backfillConfig,
					AsStartOption(roll.WithoutVersionSchema()),
					AsCompleteOption(roll.WithSkipSchemaDrop()),
				); err != nil {
					return fmt.Errorf("failed to run migration file %q: %w", mig.Name, err)
				}
			}

			// Run the final migration. Its Start projects the new target
			// version schema. If --complete is set, the final Complete()
			// reaps every other version schema (the production-active
			// version goes away). If --complete is not set, the final
			// migration is left in-progress; the production-active version
			// schema is preserved until the operator runs `pgroll complete`
			// after deploying apps to the new target.
			if err := runMigration(ctx, m, migs[len(migs)-1], complete, backfillConfig); err != nil {
				return err
			}

			return nil
		},
	}

	migrateCmd.Flags().IntVar(&batchSize, "backfill-batch-size", backfill.DefaultBatchSize, "Number of rows backfilled in each batch")
	migrateCmd.Flags().DurationVar(&batchDelay, "backfill-batch-delay", backfill.DefaultDelay, "Duration of delay between batch backfills (eg. 1s, 1000ms)")
	migrateCmd.Flags().BoolVar(&expectOne, "expect-one", false, "Abort if there is more than one migration to be applied")
	migrateCmd.Flags().BoolVarP(&complete, "complete", "c", false, "complete the final migration rather than leaving it active")

	return migrateCmd
}

// parseMigrations tries to parse all RawMigrations and collects all the errors
// if any.
func parseMigrations(migs []*migrations.RawMigration) ([]*migrations.Migration, error) {
	parsedMigrations := make([]*migrations.Migration, 0, len(migs))
	var errs error
	for _, rawMigration := range migs {
		m, err := migrations.ParseMigration(rawMigration)
		if err != nil {
			errs = errors.Join(errs, err)
		}
		parsedMigrations = append(parsedMigrations, m)
	}
	if errs != nil {
		return nil, fmt.Errorf("incompatible migration(s): %w", errs)
	}
	return parsedMigrations, nil
}
