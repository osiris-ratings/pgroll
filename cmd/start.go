// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/xataio/pgroll/cmd/flags"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

func startCmd() *cobra.Command {
	var complete bool
	var batchSize int
	var batchDelay time.Duration

	startCmd := &cobra.Command{
		Use:       "start <file>",
		Short:     "Start a migration for the operations present in the given file",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"file"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			fileName := args[0]

			// Create a roll instance and check if pgroll is initialized
			m, err := NewRollWithInitCheck(ctx)
			if err != nil {
				return err
			}
			defer m.Close()

			// Check whether the schema needs an initial baseline migration
			needsBaseline, err := m.State().HasExistingSchemaWithoutHistory(ctx, m.Schema())
			if err != nil {
				return fmt.Errorf("failed to check for existing schema: %w", err)
			}
			if needsBaseline {
				fmt.Printf("Schema %q is non-empty but has no migration history. Run `pgroll baseline` first\n", m.Schema())
				return nil
			}

			c := backfill.NewConfig(
				backfill.WithBatchSize(batchSize),
				backfill.WithBatchDelay(batchDelay),
			)

			return runMigrationFromFile(ctx, m, fileName, complete, c)
		},
	}

	startCmd.Flags().IntVar(&batchSize, "backfill-batch-size", backfill.DefaultBatchSize, "Number of rows backfilled in each batch")
	startCmd.Flags().DurationVar(&batchDelay, "backfill-batch-delay", backfill.DefaultDelay, "Duration of delay between batch backfills (eg. 1s, 1000ms)")
	startCmd.Flags().BoolVarP(&complete, "complete", "c", false, "Mark the migration as complete")
	startCmd.Flags().BoolP("skip-validation", "s", false, "skip migration validation")

	viper.BindPFlag("SKIP_VALIDATION", startCmd.Flags().Lookup("skip-validation"))

	return startCmd
}

func runMigrationFromFile(ctx context.Context, m *roll.Roll, fileName string, complete bool, c *backfill.Config) error {
	migration, err := migrations.ReadMigration(os.DirFS(filepath.Dir(fileName)), filepath.Base(fileName))
	if err != nil {
		return err
	}

	// Same guard as `pgroll migrate`: a `baseline: true` file is a schema
	// snapshot, and executing it into a database with existing history is
	// the truncated-history trap. A fresh database (no history) may
	// bootstrap by executing it.
	if migration.Baseline {
		latest, err := m.State().LatestMigration(ctx, m.Schema())
		if err != nil {
			return fmt.Errorf("unable to determine latest migration: %w", err)
		}
		if latest != nil {
			return roll.BaselineExecutionRefusedError(migration.Name)
		}
	}

	return runMigration(ctx, m, migration, complete, c)
}

// migrationOption is the union of Start- and Complete-phase options that
// runMigration accepts. Letting the caller mix freely is cleaner than two
// variadic slices because most call sites (e.g. cmd/migrate.go) want to
// configure both phases at once.
type migrationOption interface{ migrationOptionMarker() }

type startOpt struct{ opt roll.StartOption }

func (startOpt) migrationOptionMarker() {}

type completeOpt struct{ opt roll.CompleteOption }

func (completeOpt) migrationOptionMarker() {}

// AsStartOption wraps a roll.StartOption for runMigration.
func AsStartOption(o roll.StartOption) migrationOption { return startOpt{o} }

// AsCompleteOption wraps a roll.CompleteOption for runMigration.
func AsCompleteOption(o roll.CompleteOption) migrationOption { return completeOpt{o} }

func runMigration(ctx context.Context, m *roll.Roll, migration *migrations.Migration, complete bool, c *backfill.Config, opts ...migrationOption) error {
	var startOpts []roll.StartOption
	var completeOpts []roll.CompleteOption
	for _, o := range opts {
		switch v := o.(type) {
		case startOpt:
			startOpts = append(startOpts, v.opt)
		case completeOpt:
			completeOpts = append(completeOpts, v.opt)
		}
	}

	sp, _ := pterm.DefaultSpinner.WithText("Starting migration...").Start()
	c.AddCallback(func(n int64, total int64) {
		if total > 0 {
			percent := float64(n) / float64(total) * 100
			// Percent can be > 100 if we're on the last batch in which case we still want to display 100.
			percent = math.Min(percent, 100)
			sp.UpdateText(fmt.Sprintf("%d records complete... (%.2f%%)", n, percent))
		} else {
			sp.UpdateText(fmt.Sprintf("%d records complete...", n))
		}
	})

	err := m.Start(ctx, migration, c, startOpts...)
	if err != nil {
		sp.Fail(fmt.Sprintf("Failed to start migration: %s", err))
		return err
	}

	if complete {
		if err = m.Complete(ctx, completeOpts...); err != nil {
			sp.Fail(fmt.Sprintf("Failed to complete migration: %s", err))
			return err
		}
	}

	// A version schema is projected only when (a) the Roll instance has
	// version schemas enabled globally and (b) this Start call did not
	// pass WithoutVersionSchema. Intermediate migrations in `pgroll
	// migrate` batches pass WithoutVersionSchema and therefore do NOT
	// produce a version schema — the spinner message must reflect that
	// truthfully so we don't tell operators a schema is available when
	// it isn't.
	projectedSchema := m.UseVersionSchema() && !roll.StartOptionsSkipVersionSchema(startOpts...)

	var msg string
	if projectedSchema {
		viewName := roll.VersionedSchemaName(flags.Schema(), migration.VersionSchemaName())
		msg = fmt.Sprintf("New version of the schema available under the postgres %q schema", viewName)
	} else {
		msg = fmt.Sprintf("Migration %q applied", migration.Name)
	}

	sp.Success(msg)

	return nil
}
