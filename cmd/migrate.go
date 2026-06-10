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

	"github.com/pterm/pterm"
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
			preFlightState, err := printMigratePreFlight(ctx, m, migrationsDir, rawMigs, os.Stdout)
			if err != nil {
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
				if preFlightState == cycleInProgress {
					return fmt.Errorf(
						"migration %q is currently being run by another pgroll process; "+
							"wait for that run to finish before retrying",
						*latestMigration,
					)
				}
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

			// Seal the previous deployment before applying this one. Under
			// delayed contraction every migration in a train — including the
			// final one — defers its destructive DDL, keeping the whole train
			// in its expand phase (and therefore losslessly revertible) for a
			// full release cycle. The next train departing is the previous
			// train's point of no return: drain its queued contraction now,
			// while production apps stay pinned to its (briefly recreated)
			// version schema.
			sealed, err := m.SealDeferredCompletes(ctx)
			if err != nil {
				return fmt.Errorf("failed to seal previous deployment: %w", err)
			}
			if sealed > 0 {
				fmt.Printf("Sealed previous deployment: %d deferred completion(s) drained. The revert window now covers only this deployment.\n\n", sealed)
			}

			// Apply each intermediate migration without projecting a version
			// schema. No apps will ever connect to an intermediate version, so
			// projecting it would just waste a schema and create view
			// dependencies that block destructive operations later in the
			// batch.
			//
			// Pick the right Complete strategy per migration:
			//
			//   - WithDeferComplete for migrations whose Complete would be
			//     blocked by the prev-prod version schema's views (DROP/
			//     RENAME of user-facing identifiers, OnComplete=true raw
			//     SQL). Their actions queue and replay at final Complete
			//     after the prev-prod schema is dropped.
			//   - WithSkipSchemaDrop for additive migrations (add column/
			//     index/constraint, create table, alter column, regular
			//     raw SQL). Their Completes touch only pgroll-internal
			//     temp names and trigger machinery, which prev-prod views
			//     don't reference — running inline is safer than deferring
			//     because it avoids cross-migration interactions on shared
			//     internal state (e.g. the per-table _pgroll_needs_backfill
			//     marker column or temp column names colliding with the
			//     next migration's Start).
			for _, mig := range migs[:len(migs)-1] {
				completeOpt := AsCompleteOption(roll.WithSkipSchemaDrop())
				if mig.CompleteMustBeDeferred() {
					completeOpt = AsCompleteOption(roll.WithDeferComplete())
				}
				if err := runMigration(
					ctx, m, mig, true, backfillConfig,
					AsStartOption(roll.WithoutVersionSchema()),
					completeOpt,
				); err != nil {
					return fmt.Errorf("failed to run migration file %q: %w", mig.Name, err)
				}
			}

			// Run the final migration. Its Start projects the new target
			// version schema. If --complete is set, the final migration is
			// marked done with its contraction deferred (delayed
			// contraction): the previous-production version schema survives
			// and the whole train remains losslessly revertible via `pgroll
			// revert` until the next train departs (or `pgroll complete`
			// seals it manually). If --complete is not set, the final
			// migration is left in-progress for a later `pgroll complete`.
			finalOpts := []migrationOption{}
			if complete {
				finalOpts = append(finalOpts, AsCompleteOption(roll.WithDeferComplete()))
			}
			if err := runMigration(ctx, m, migs[len(migs)-1], complete, backfillConfig, finalOpts...); err != nil {
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

// cycleState classifies the deployment state for a `pgroll migrate` run.
type cycleState string

const (
	cycleFresh       cycleState = "FRESH"
	cycleIncremental cycleState = "INCREMENTAL"
	cycleRecovery    cycleState = "RECOVERY"
	cycleInProgress  cycleState = "IN-PROGRESS"
	cycleInterrupted cycleState = "INTERRUPTED"
	cycleNoOp        cycleState = "NO-OP"
)

// printMigratePreFlight reports the current deployment state and the plan
// for this run before any migrations execute. This makes `pgroll migrate`
// auditable and idempotent: an operator can re-run after an interruption
// (lock_timeout, SIGINT, network blip) and immediately see what state the
// database is actually in vs what pgroll's state table thinks.
//
// Headline is the cycle state (FRESH / INCREMENTAL / RECOVERY / INTERRUPTED
// / NO-OP) plus `N applied · M remaining`. The Plan row describes the run
// as a schema-level transition (`source → target (N migrations)`); the
// individual migration filenames listed under Applies are the manifest. We
// surface pgroll's `state.LatestVersion` only when it diverges from the
// live schema (RECOVERY), since in all other cases the live schema name
// already encodes it.
func printMigratePreFlight(ctx context.Context, m *roll.Roll, migrationsDir string, unapplied []*migrations.RawMigration, out io.Writer) (cycleState, error) {
	existingSchemas, err := m.ExistingVersionSchemas(ctx)
	if err != nil {
		return "", fmt.Errorf("listing existing version schemas: %w", err)
	}

	stateLatestVersion, err := m.State().LatestVersion(ctx, m.Schema())
	if err != nil {
		return "", fmt.Errorf("reading state.LatestVersion: %w", err)
	}

	history, err := m.State().SchemaHistory(ctx, m.Schema())
	if err != nil {
		return "", fmt.Errorf("reading schema history: %w", err)
	}

	activePeriod, err := m.State().IsActiveMigrationPeriod(ctx, m.Schema())
	if err != nil {
		return "", fmt.Errorf("reading active migration period: %w", err)
	}
	var inProgressName string
	otherBackends := 0
	if activePeriod {
		latestMig, err := m.State().LatestMigration(ctx, m.Schema())
		if err != nil {
			return "", fmt.Errorf("reading latest migration: %w", err)
		}
		if latestMig != nil {
			inProgressName = *latestMig
		}

		// Probe pg_stat_activity for other live pgroll backends so we can
		// distinguish a concurrent run (IN-PROGRESS) from an abandoned one
		// (INTERRUPTED). On error (e.g. restricted pg_stat_activity), fall
		// back to today's behaviour by treating the count as zero — the
		// caller will still classify as INTERRUPTED, never weaker than the
		// status quo.
		statePID, sErr := m.State().BackendPID(ctx)
		rollPID, rErr := m.BackendPID(ctx)
		if sErr == nil && rErr == nil {
			n, err := m.State().OtherPgrollBackends(ctx, []int{statePID, rollPID})
			if err == nil {
				otherBackends = n
			}
		}
	}

	totalCount := len(history) + len(unapplied)
	appliedCount := len(history)
	remainingCount := len(unapplied)

	prefix := m.Schema() + "_"

	// Drift detection: state.LatestVersion can advance past the
	// production-active schema during a batched migrate (intermediates
	// are marked done=true without projecting a schema), so a mismatch
	// is the signal that a prior batch ran past where deployment caught
	// up to. That's a recovery run, not necessarily an error.
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

	state := classifyCycle(activePeriod, otherBackends, remainingCount, appliedCount, stateInSync)

	stateColor := pterm.FgGreen
	switch state {
	case cycleInterrupted:
		stateColor = pterm.FgRed
	case cycleInProgress, cycleRecovery:
		stateColor = pterm.FgYellow
	case cycleNoOp:
		stateColor = pterm.FgGray
	}

	const (
		cycleColWidth = 14
		fieldColWidth = 16
	)

	title := pterm.NewStyle(pterm.FgWhite, pterm.Bold).Sprint("▶ pgroll migrate")
	subtitle := pterm.FgGray.Sprintf("%s · %d total", migrationsDir, totalCount)
	fmt.Fprintf(out, "\n%s   %s\n\n", title, subtitle)

	bullet := stateColor.Sprint("●")
	label := pterm.NewStyle(stateColor, pterm.Bold).Sprint(string(state))
	labelPad := strings.Repeat(" ", cycleColWidth-len(string(state)))
	progress := fmt.Sprintf("%d applied · %d remaining", appliedCount, remainingCount)
	fmt.Fprintf(out, "  %s %s%s%s\n", bullet, label, labelPad, progress)

	field := func(name, value string) {
		gray := pterm.FgGray.Sprint(name)
		pad := strings.Repeat(" ", fieldColWidth-len(name))
		fmt.Fprintf(out, "    %s%s%s\n", gray, pad, value)
	}

	// applies prints "Applies   <name>" then aligns subsequent names under
	// the first one. Migration names are long; vertical avoids wrapping
	// and lets operators count and copy individual entries.
	applies := func(names []string) {
		if len(names) == 0 {
			return
		}
		field("Applies", names[0])
		contIndent := strings.Repeat(" ", 4+fieldColWidth)
		for _, n := range names[1:] {
			fmt.Fprintf(out, "%s%s\n", contIndent, n)
		}
	}

	switch state {
	case cycleInterrupted, cycleInProgress:
		label := "Stuck on"
		if state == cycleInProgress {
			label = "Running"
		}
		if inProgressName != "" {
			field(label, inProgressName)
		}
		liveLabel := "Live schema"
		if len(existingSchemas) != 1 {
			liveLabel = "Live schemas"
		}
		liveValue := "—"
		if len(existingSchemas) > 0 {
			liveValue = strings.Join(existingSchemas, ", ")
		}
		field(liveLabel, liveValue)
		fmt.Fprintln(out)
		if state == cycleInProgress {
			fmt.Fprintln(out, pterm.FgYellow.Sprint("    Another pgroll process is running this migration — wait for it to finish."))
		} else {
			fmt.Fprintln(out, pterm.FgYellow.Sprint("    Run `pgroll rollback` to clean up before retrying."))
		}
	case cycleNoOp:
		current := "—"
		if len(existingSchemas) > 0 {
			current = strings.Join(existingSchemas, ", ")
		}
		field("Current", current)
	default:
		// Plan: "<source> → <target> (N migrations)"
		source := pterm.FgGray.Sprint("(empty)")
		if len(existingSchemas) == 1 {
			source = existingSchemas[0]
		} else if len(existingSchemas) > 1 {
			source = strings.Join(existingSchemas, ", ")
		}

		finalRaw := unapplied[len(unapplied)-1]
		finalVersion := finalRaw.Name
		if finalRaw.VersionSchema != "" {
			finalVersion = finalRaw.VersionSchema
		}
		target := roll.VersionedSchemaName(m.Schema(), finalVersion)

		unit := "migrations"
		if remainingCount == 1 {
			unit = "migration"
		}
		count := pterm.FgGray.Sprintf("(%d %s)", remainingCount, unit)
		plan := fmt.Sprintf("%s %s %s %s", source, pterm.FgGray.Sprint("→"), target, count)
		field("Plan", plan)

		// Surface the point of no return: applying this batch seals the
		// previous deployment by draining its deferred contraction DDL.
		deferred, err := m.State().DeferredCompletes(ctx, m.Schema())
		if err != nil {
			return "", fmt.Errorf("reading deferred completes: %w", err)
		}
		if len(deferred) > 0 {
			field("Seals", fmt.Sprintf("%d deferred completion(s) — closes the previous deployment's revert window", len(deferred)))
		}

		names := make([]string, len(unapplied))
		for i, mig := range unapplied {
			names[i] = mig.Name
		}
		applies(names)

		if state == cycleRecovery && stateLatestVersion != nil {
			fmt.Fprintln(out)
			fmt.Fprintf(out, "    %s pgroll state advanced to %s — ahead of the live schema.\n",
				pterm.FgYellow.Sprint("⚠"), *stateLatestVersion)
			fmt.Fprintln(out, pterm.FgYellow.Sprint("    A previous migrate completed migrations whose schema was never deployed."))
		}
	}

	fmt.Fprintln(out)
	return state, nil
}

// classifyCycle picks the deployment state label for the pre-flight
// summary. otherBackends is the count of *other* pgroll processes connected
// to the same database; when an activePeriod row exists but a live pgroll
// backend is also present, the migration is genuinely in progress (a
// concurrent run) rather than abandoned.
func classifyCycle(activePeriod bool, otherBackends int, remaining, applied int, stateInSync bool) cycleState {
	switch {
	case activePeriod && otherBackends > 0:
		return cycleInProgress
	case activePeriod:
		return cycleInterrupted
	case remaining == 0:
		return cycleNoOp
	case stateInSync && applied == 0:
		return cycleFresh
	case stateInSync:
		return cycleIncremental
	default:
		return cycleRecovery
	}
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
