// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
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

			// Pre-flight summary: print the deployment state and the plan for
			// this run before doing any work. This is the single point where
			// operators can verify pgroll's state matches their understanding
			// of production and catch drift introduced by prior aborted runs.
			if err := printMigratePreFlight(ctx, m, migrationsDir, rawMigs, os.Stdout); err != nil {
				return fmt.Errorf("pre-flight summary: %w", err)
			}

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

// printMigratePreFlight reports the current deployment state and the plan
// for this run before any migrations execute. This makes `pgroll migrate`
// auditable and idempotent: an operator can re-run after an interruption
// (lock_timeout, SIGINT, network blip) and immediately see what state the
// database is actually in vs what pgroll's state table thinks.
//
// What it reports:
//
//  1. Existing version schemas (the live `<schema>_*` schemas in Postgres).
//     Under the no-intermediate-schemas model there is normally exactly
//     one — the production-active version apps are connected to.
//  2. pgroll's `state.LatestVersion`. If this matches one of the existing
//     schemas the run is a fresh cycle. If it has advanced beyond the
//     existing schemas, this is a recovery run from an aborted prior batch.
//  3. Total / already-applied / remaining migration counts so the operator
//     can sanity-check the scope of the run.
//  4. The final migration name and the schema name it will create when
//     completed — the target apps will eventually deploy to.
func printMigratePreFlight(ctx context.Context, m *roll.Roll, migrationsDir string, unapplied []*migrations.RawMigration, out io.Writer) error {
	existingSchemas, err := m.ExistingVersionSchemas(ctx)
	if err != nil {
		return fmt.Errorf("listing existing version schemas: %w", err)
	}

	stateLatestVersion, err := m.State().LatestVersion(ctx, m.Schema())
	if err != nil {
		return fmt.Errorf("reading state.LatestVersion: %w", err)
	}

	history, err := m.State().SchemaHistory(ctx, m.Schema())
	if err != nil {
		return fmt.Errorf("reading schema history: %w", err)
	}

	// Detect a stuck in-progress migration up front. If one exists, the
	// active-period check immediately after pre-flight will error out — but
	// the operator wants to see this state called out clearly in the
	// pre-flight so they understand why the run is about to refuse.
	activePeriod, err := m.State().IsActiveMigrationPeriod(ctx, m.Schema())
	if err != nil {
		return fmt.Errorf("reading active migration period: %w", err)
	}
	var inProgressName string
	if activePeriod {
		latestMig, err := m.State().LatestMigration(ctx, m.Schema())
		if err != nil {
			return fmt.Errorf("reading latest migration: %w", err)
		}
		if latestMig != nil {
			inProgressName = *latestMig
		}
	}

	totalCount := len(history) + len(unapplied)
	appliedCount := len(history)
	remainingCount := len(unapplied)

	prefix := m.Schema() + "_"

	// Drift detection: does state.LatestVersion correspond to one of the
	// existing version schemas? Under the no-intermediate-schemas model,
	// state can advance past the production-active schema during a batch
	// (intermediates are marked done=true without projecting a schema), so
	// a mismatch is the signal that a prior batch ran past where deployment
	// caught up to. That's a recovery run, not necessarily an error — but
	// the operator wants to see it.
	stateInSync := stateLatestVersion == nil
	if stateLatestVersion != nil {
		want := prefix + *stateLatestVersion
		for _, s := range existingSchemas {
			if s == want {
				stateInSync = true
				break
			}
		}
	}

	fmt.Fprintln(out, "▶ pgroll migrate — pre-flight")
	fmt.Fprintf(out, "  Migrations directory:    %s\n", migrationsDir)
	fmt.Fprintf(out, "  Total migrations:        %d\n", totalCount)
	fmt.Fprintf(out, "  Already applied:         %d\n", appliedCount)
	fmt.Fprintf(out, "  Remaining to apply:      %d\n", remainingCount)
	fmt.Fprintln(out)

	if len(existingSchemas) == 0 {
		fmt.Fprintln(out, "  Active version schema(s): (none — fresh database or post-cleanup)")
	} else {
		fmt.Fprintln(out, "  Active version schema(s):")
		for _, s := range existingSchemas {
			fmt.Fprintf(out, "    • %s    (migration: %s)\n", s, strings.TrimPrefix(s, prefix))
		}
	}
	if stateLatestVersion != nil {
		fmt.Fprintf(out, "  pgroll state.LatestVersion: %s\n", *stateLatestVersion)
	} else {
		fmt.Fprintln(out, "  pgroll state.LatestVersion: (none)")
	}
	fmt.Fprintln(out)

	switch {
	case activePeriod:
		fmt.Fprintln(out, "  Cycle: INTERRUPTED — a previous migration is in progress and was never completed.")
		if inProgressName != "" {
			fmt.Fprintf(out, "         In-progress migration: %s\n", inProgressName)
		}
		fmt.Fprintln(out, "         Run `pgroll rollback` to clean up before retrying. This run will refuse to proceed.")
	case remainingCount == 0:
		fmt.Fprintln(out, "  Cycle: NO-OP — all migrations from the directory are already applied.")
	case stateInSync && appliedCount == 0:
		fmt.Fprintln(out, "  Cycle: FRESH — no migrations from this directory have been applied yet.")
	case stateInSync:
		fmt.Fprintln(out, "  Cycle: INCREMENTAL — pgroll state matches the active schema; new migrations to apply.")
	default:
		fmt.Fprintln(out, "  Cycle: RECOVERY — pgroll state has advanced beyond the active schema.")
		fmt.Fprintln(out, "         A previous migrate run completed migrations whose schema is not yet deployed.")
	}
	if !activePeriod && remainingCount > 0 {
		fmt.Fprintf(out, "  Resuming from:    %s\n", unapplied[0].Name)
	}
	fmt.Fprintln(out)

	if !activePeriod && remainingCount > 0 {
		final := unapplied[len(unapplied)-1]
		finalVersion := final.Name
		if final.VersionSchema != "" {
			finalVersion = final.VersionSchema
		}
		fmt.Fprintf(out, "  Final migration:  %s\n", final.Name)
		fmt.Fprintf(out, "  Final schema:     %s\n", roll.VersionedSchemaName(m.Schema(), finalVersion))
		fmt.Fprintln(out)
	}
	return nil
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
