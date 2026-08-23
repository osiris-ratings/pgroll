// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
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
	var toMigration string

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

			resolution, err := m.ResolveMigrations(ctx, os.DirFS(migrationsDir))
			if err != nil {
				return fmt.Errorf("failed to get migrations to apply: %w", err)
			}
			rawMigs := resolution.Apply

			// Bound the train: apply only up to (and including) the target.
			//
			// The target is located in the full local sequence, not in the
			// list this run would apply. Those are different sets — the target
			// may already be applied, or be routed to another --target — and
			// searching only the apply list means an already-applied target
			// reads as "nothing to do" even when migrations *before* it are
			// still pending, silently no-opping the whole run.
			if toMigration != "" {
				idx := -1
				for i, rm := range resolution.All {
					if rm.Name == toMigration {
						idx = i
						break
					}
				}
				if idx == -1 {
					exists, err := m.State().MigrationExists(ctx, m.Schema(), toMigration)
					if err != nil {
						return fmt.Errorf("unable to check for migration %q: %w", toMigration, err)
					}
					if exists {
						fmt.Printf("Database is already at %q; nothing to apply.\n", toMigration)
						return nil
					}
					return fmt.Errorf("migration %q not found in %s", toMigration, migrationsDir)
				}

				if _, excluded := resolution.Excluded[toMigration]; excluded {
					fmt.Printf("Note: %q is not routed to target %q; bounding this run at its position.\n",
						toMigration, m.Target())
				}

				bound := make(map[string]struct{}, idx+1)
				for _, rm := range resolution.All[:idx+1] {
					bound[rm.Name] = struct{}{}
				}

				// Close the bound under depends_on.
				//
				// Locating --to in the full sequence makes the bound a
				// filename-order prefix, and filename order is not
				// dependency-closed: given 01, 02, 03 where 02 depends_on 03,
				// `--to 02` selects {01, 02} and 02 then starts without 03.
				// Truncating the topologically sorted list — what this used to
				// do — was closed by construction, and that property has to be
				// put back explicitly now that the bound is positional.
				//
				// Only dependencies inside this run's apply list are pulled in.
				// One that is already applied needs nothing, and one excluded
				// by the active target is satisfied by construction.
				inRun := make(map[string]*migrations.RawMigration, len(rawMigs))
				for _, rm := range rawMigs {
					inRun[rm.Name] = rm
				}
				var pull func(name string)
				pull = func(name string) {
					rm, ok := inRun[name]
					if !ok {
						return
					}
					for _, dep := range rm.DependsOn {
						if _, already := bound[dep]; already {
							continue
						}
						if _, isDep := inRun[dep]; !isDep {
							continue
						}
						bound[dep] = struct{}{}
						fmt.Printf("Note: also applying %q, which %q depends on.\n", dep, name)
						pull(dep)
					}
				}
				for name := range bound {
					pull(name)
				}

				bounded := make([]*migrations.RawMigration, 0, len(rawMigs))
				for _, rm := range rawMigs {
					if _, ok := bound[rm.Name]; ok {
						bounded = append(bounded, rm)
					}
				}
				rawMigs = bounded
			}

			// A migration marked `baseline: true` is a schema snapshot, not a
			// change to run. On a database with existing history it can only
			// reach the apply list when its anchor row is missing or hidden —
			// the truncated-history trap, where executing it would replay the
			// entire schema — so refuse before doing any work. On a fresh
			// database (no history at all) executing the baseline is a
			// legitimate bootstrap and is allowed through.
			if latest, err := m.State().LatestMigration(ctx, m.Schema()); err != nil {
				return fmt.Errorf("unable to determine latest migration: %w", err)
			} else if latest != nil {
				if err := roll.RefuseBaselineExecution(rawMigs); err != nil {
					return err
				}
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
				switch preFlightState {
				case cycleInProgress:
					return fmt.Errorf(
						"migration %q is currently being run by another pgroll process; "+
							"wait for that run to finish before retrying",
						*latestMigration,
					)
				case cycleAwaitingComplete:
					// The final migration of a prior `pgroll migrate` is
					// intentionally left active — its Start (expand) applied
					// and projected the new version schema, and `pgroll
					// complete` will contract it. This is the normal
					// production state between expand and contract, so
					// re-running migrate here must be idempotent: a deploy
					// interrupted in this window (e.g. a failed fleet rollout)
					// has to be retryable without a destructive `pgroll
					// rollback` that would tear down the expand the fleet may
					// already be pinned to.
					if !complete {
						fmt.Printf("Migration %q is already applied (expand phase); awaiting `pgroll complete`. Nothing to do\n", *latestMigration)
						return nil
					}
					// --complete was requested (the dev/CI one-shot converge):
					// finish the pending contraction instead of erroring, so
					// `migrate --complete` is idempotent too.
					if err := m.Complete(ctx); err != nil {
						return fmt.Errorf("failed to complete already-applied migration %q: %w", *latestMigration, err)
					}
					fmt.Printf("Migration %q completed\n", *latestMigration)
					return nil
				default:
					return fmt.Errorf("%s", interruptedMessage(ctx, m, *latestMigration))
				}
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

			// Re-application tombstones: a sealed revert prunes its targets
			// from history, which makes their unchanged files look unapplied
			// again — re-running them would silently re-apply the DDL the
			// revert just undid. Refuse while the content still matches the
			// tombstone; any edit to the file changes the hash and confirms
			// intent.
			tombstones, err := m.State().RevertedMigrations(ctx, m.Schema())
			if err != nil {
				return fmt.Errorf("failed to check reverted migrations: %w", err)
			}
			if len(tombstones) > 0 {
				for _, mig := range migs {
					want, ok := tombstones[mig.Name]
					if !ok {
						continue
					}
					hash, err := mig.ContentHash()
					if err != nil {
						return err
					}
					if hash == want {
						return fmt.Errorf(
							"migration %q was undone by a sealed revert and its content is unchanged; "+
								"re-applying would re-run the reverted DDL. Edit the migration to confirm "+
								"intent, or remove it from the migrations directory", mig.Name,
						)
					}
				}
			}

			backfillConfig := backfill.NewConfig(
				backfill.WithBatchSize(batchSize),
				backfill.WithBatchDelay(batchDelay),
			)

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
			// Dependencies on migrations the active --target does not select
			// are satisfied by construction: they will never be applied to
			// this database. Start cannot work that out for itself — it never
			// sees the migrations directory — so the set resolved above is
			// handed to every Start in the batch.
			satisfiedDeps := AsStartOption(roll.WithSatisfiedDependencies(resolution.Excluded))

			for _, mig := range migs[:len(migs)-1] {
				completeOpt := AsCompleteOption(roll.WithSkipSchemaDrop())
				if mig.CompleteMustBeDeferred() {
					completeOpt = AsCompleteOption(roll.WithDeferComplete())
				}
				if err := runMigration(
					ctx, m, mig, true, backfillConfig,
					AsStartOption(roll.WithoutVersionSchema()),
					satisfiedDeps,
					completeOpt,
				); err != nil {
					return fmt.Errorf("failed to run migration file %q: %w", mig.Name, err)
				}
			}

			// Run the final migration. Its Start projects the new target
			// version schema. If --complete is set, the final migration runs
			// a full (non-deferred) Complete: the batch's queued contraction
			// drains, old version schemas are dropped, and the deployment is
			// sealed — a one-shot converge for environments with nothing
			// pinned to the previous schema (dev, CI, disposable instances).
			// If --complete is not set — the production path — the final
			// migration is left in-progress: the fleet repins to its version
			// schema and a later `pgroll complete` contracts the deployment.
			// Until that contraction, the whole batch is losslessly
			// revertible via `pgroll revert`.
			if err := runMigration(ctx, m, migs[len(migs)-1], complete, backfillConfig, satisfiedDeps); err != nil {
				return err
			}

			// The batch applied: clear any tombstones its (necessarily
			// edited) migrations matched by name — the engineer has
			// confirmed intent and the tombstones have served their purpose.
			if len(tombstones) > 0 {
				applied := make([]string, 0, len(migs))
				for _, mig := range migs {
					if _, ok := tombstones[mig.Name]; ok {
						applied = append(applied, mig.Name)
					}
				}
				if err := m.State().ClearRevertedMigrations(ctx, m.Schema(), applied); err != nil {
					return fmt.Errorf("failed to clear re-application tombstones: %w", err)
				}
			}

			return nil
		},
	}

	migrateCmd.Flags().IntVar(&batchSize, "backfill-batch-size", backfill.DefaultBatchSize, "Number of rows backfilled in each batch")
	migrateCmd.Flags().DurationVar(&batchDelay, "backfill-batch-delay", backfill.DefaultDelay, "Duration of delay between batch backfills (eg. 1s, 1000ms)")
	migrateCmd.Flags().BoolVar(&expectOne, "expect-one", false, "Abort if there is more than one migration to be applied")
	migrateCmd.Flags().BoolVarP(&complete, "complete", "c", false, "complete the final migration rather than leaving it active")
	migrateCmd.Flags().StringVar(&toMigration, "to", "", "apply migrations only up to (and including) this one; already applied is a no-op")

	return migrateCmd
}

// cycleState classifies the deployment state for a `pgroll migrate` run.
type cycleState string

const (
	cycleFresh            cycleState = "FRESH"
	cycleIncremental      cycleState = "INCREMENTAL"
	cycleRecovery         cycleState = "RECOVERY"
	cycleInProgress       cycleState = "IN-PROGRESS"
	cycleInterrupted      cycleState = "INTERRUPTED"
	cycleAwaitingComplete cycleState = "EXPANDED"
	cycleNoOp             cycleState = "NO-OP"
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
	liveSchemas, err := m.ExistingVersionSchemas(ctx)
	if err != nil {
		return "", fmt.Errorf("listing existing version schemas: %w", err)
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
	var expandComplete bool
	otherBackends := 0
	if activePeriod {
		latestMig, err := m.State().LatestMigration(ctx, m.Schema())
		if err != nil {
			return "", fmt.Errorf("reading latest migration: %w", err)
		}
		if latestMig != nil {
			inProgressName = *latestMig
		}

		// Decide whether the active migration's Start (expand) fully
		// materialized. That requires BOTH:
		//
		//   1. Its version schema was projected — the atomic marker that the
		//      DDL phase finished and the fleet can pin to the new schema.
		//   2. Its backfill (if any) completed — no rows still carry the
		//      `_pgroll_needs_backfill` marker.
		//
		// Both are needed because Start projects the version schema *before*
		// running the backfill, so a Start killed mid-backfill leaves the
		// schema in place with data half-filled. Treating that as "done"
		// would let a deploy proceed on incomplete data and seal it at
		// Complete. Only when both hold is the migration the normal "left
		// active awaiting `pgroll complete`" state rather than a genuinely
		// interrupted run. On any error we leave expandComplete false and
		// fall back to the INTERRUPTED classification — never weaker than the
		// status quo.
		if activeMig, err := m.State().GetActiveMigration(ctx, m.Schema()); err == nil {
			want := roll.VersionedSchemaName(m.Schema(), activeMig.VersionSchemaName())
			schemaProjected := false
			for _, s := range liveSchemas {
				if s == want {
					schemaProjected = true
					break
				}
			}
			if schemaProjected {
				pending, err := m.HasPendingBackfill(ctx, activeMig.Name)
				if err == nil && !pending {
					expandComplete = true
				}
			}
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
	// Is the history leaf's projection actually deployed?
	//
	// This used to compare state.LatestVersion against liveSchemas, which
	// could never disagree and so made RECOVERY unreachable: LatestVersion
	// resolves through find_version_schema, whose own WHERE clause requires
	// the schema to exist in information_schema.schemata, and liveSchemas is
	// the same set matched with a broader LIKE. The wanted value was in the
	// live set by construction, `stateInSync` was always true, and
	// classifyCycle never reached its default branch. The unit test passed
	// `stateInSync: false` straight into the pure function, so the branch
	// stayed green while being dead.
	//
	// The condition worth reporting is the one the warning already describes:
	// history has advanced to a migration whose version schema is not
	// deployed — a projection that was dropped, or never created — which is
	// exactly what `pgroll materialize` exists to repair. Compare the LEAF's
	// schema, which find_version_schema deliberately skips past.
	//
	// Trivially in sync when version schemas are disabled: there is no
	// projection to be missing.
	stateInSync := true
	if m.UseVersionSchema() && len(history) > 0 {
		leaf := history[len(history)-1].Migration
		leafVersion := leaf.VersionSchema
		if leafVersion == "" {
			leafVersion = leaf.Name
		}
		want := prefix + leafVersion
		stateInSync = slices.Contains(liveSchemas, want)
	}

	state := classifyCycle(activePeriod, otherBackends, remainingCount, appliedCount, stateInSync, expandComplete)

	stateColor := pterm.FgGreen
	switch state {
	case cycleInterrupted:
		stateColor = pterm.FgRed
	case cycleInProgress, cycleRecovery:
		stateColor = pterm.FgYellow
	case cycleAwaitingComplete, cycleNoOp:
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
		if len(liveSchemas) != 1 {
			liveLabel = "Live schemas"
		}
		liveValue := "—"
		if len(liveSchemas) > 0 {
			liveValue = strings.Join(liveSchemas, ", ")
		}
		field(liveLabel, liveValue)
		fmt.Fprintln(out)
		if state == cycleInProgress {
			fmt.Fprintln(out, pterm.FgYellow.Sprint("    Another pgroll process is running this migration — wait for it to finish."))
		} else {
			fmt.Fprintln(out, pterm.FgYellow.Sprint("    Run `pgroll rollback` to clean up before retrying."))
		}
	case cycleAwaitingComplete:
		// Expand applied, awaiting contraction: benign. Show the migration
		// left active and the live schemas, and make clear a re-run is a
		// no-op — this is what an operator sees when they retry a deploy that
		// failed after `pgroll migrate` but before `pgroll complete`.
		if inProgressName != "" {
			field("Applied", inProgressName)
		}
		liveLabel := "Live schema"
		if len(liveSchemas) != 1 {
			liveLabel = "Live schemas"
		}
		liveValue := "—"
		if len(liveSchemas) > 0 {
			liveValue = strings.Join(liveSchemas, ", ")
		}
		field(liveLabel, liveValue)
		fmt.Fprintln(out)
		fmt.Fprintln(out, pterm.FgGray.Sprint("    Expand phase already applied — awaiting `pgroll complete`. Re-running migrate is a no-op."))
	case cycleNoOp:
		current := "—"
		if len(liveSchemas) > 0 {
			current = strings.Join(liveSchemas, ", ")
		}
		field("Current", current)
	default:
		// Plan: "<source> → <target> (N migrations)"
		source := pterm.FgGray.Sprint("(empty)")
		if len(liveSchemas) == 1 {
			source = liveSchemas[0]
		} else if len(liveSchemas) > 1 {
			source = strings.Join(liveSchemas, ", ")
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

		// Surface a pre-existing deferred queue: contraction left pending by
		// an earlier run (a resumed batch, or a database upgraded from the
		// delayed-contraction lifecycle). It drains at this deployment's
		// `pgroll complete`.
		deferred, err := m.State().DeferredCompletes(ctx, m.Schema())
		if err != nil {
			return "", fmt.Errorf("reading deferred completes: %w", err)
		}
		if len(deferred) > 0 {
			field("Drains", fmt.Sprintf("%d pending deferred completion(s) from an earlier run — they contract at this deployment's `pgroll complete`", len(deferred)))
		}

		names := make([]string, len(unapplied))
		for i, mig := range unapplied {
			names[i] = mig.Name
		}
		applies(names)

		if state == cycleRecovery && len(history) > 0 {
			leaf := history[len(history)-1].Migration
			leafVersion := leaf.VersionSchema
			if leafVersion == "" {
				leafVersion = leaf.Name
			}
			fmt.Fprintln(out)
			fmt.Fprintf(out, "    %s history is at %s, but %s is not deployed.\n",
				pterm.FgYellow.Sprint("⚠"), leaf.Name, prefix+leafVersion)
			fmt.Fprintln(out, pterm.FgYellow.Sprint("    Its projection was dropped or never created; `pgroll materialize` rebuilds it."))
		}
	}

	fmt.Fprintln(out)
	return state, nil
}

// classifyCycle picks the deployment state label for the pre-flight
// summary. otherBackends is the count of *other* pgroll processes connected
// to the same database; when an activePeriod row exists but a live pgroll
// backend is also present, the migration is genuinely in progress (a
// concurrent run) rather than abandoned. expandComplete reports whether the
// active migration's Start fully materialized (its version schema is live and
// no backfill is still pending); an active period with a completed expand and
// nothing left to apply is the benign "awaiting `pgroll complete`" state
// (EXPANDED), not an interruption.
func classifyCycle(activePeriod bool, otherBackends int, remaining, applied int, stateInSync, expandComplete bool) cycleState {
	switch {
	case activePeriod && otherBackends > 0:
		return cycleInProgress
	case activePeriod && expandComplete && remaining == 0:
		// Expand applied and its version schema projected, with nothing left
		// to apply: the deployment is in the normal window between `pgroll
		// migrate` (expand) and `pgroll complete` (contract), not broken.
		return cycleAwaitingComplete
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

// interruptedMessage explains a genuinely interrupted run and — crucially —
// which verb actually cleans it up.
//
// `pgroll rollback` undoes the *one* active migration and deletes its row. In
// a batch that is almost never the whole story: `pgroll migrate` applies a
// train, so a run that died on the third of five leaves the first two applied,
// unsealed, and carrying queued contraction. Telling the operator to run
// `rollback` there hands them a partial cleanup and says nothing about the
// remainder — which is the worst time to be imprecise, because they are
// mid-incident and about to retry.
//
// So name both verbs and say what each covers, and count the already-applied
// migrations rather than describing them in the abstract. The count comes from
// the unsealed set: everything this deployment has applied and not yet
// contracted, minus the active migration itself.
func interruptedMessage(ctx context.Context, m *roll.Roll, active string) string {
	base := fmt.Sprintf(
		"migration %q is in progress and was not completed; "+
			"this usually means a previous run was interrupted "+
			"(e.g. lock_timeout under contention or SIGINT).",
		active,
	)

	// Best effort: if the count cannot be read, fall back to naming both verbs
	// without it rather than failing the error path. The unsealed set is
	// everything this deployment has applied and not yet contracted, so
	// excluding the active migration leaves exactly the ones `rollback` would
	// walk past.
	applied, counted := 0, false
	if unsealed, err := m.State().UnsealedMigrations(ctx, m.Schema()); err == nil {
		counted = true
		for _, rec := range unsealed {
			if rec.Name != active {
				applied++
			}
		}
	}

	switch {
	case counted && applied > 0:
		return fmt.Sprintf(
			"%s\n\n"+
				"  This deployment has already applied %d other migration(s) that are not yet\n"+
				"  contracted, so the two cleanup verbs differ:\n\n"+
				"    pgroll rollback   undoes ONLY %q, leaving the other %d applied\n"+
				"    pgroll revert     walks the whole uncontracted window back\n\n"+
				"  Re-running `pgroll migrate` also resumes from where it stopped, which is\n"+
				"  usually what you want after a transient failure.",
			base, applied, active, applied,
		)
	case counted:
		return fmt.Sprintf(
			"%s\n\n"+
				"  Nothing else is applied-but-uncontracted, so `pgroll rollback` cleans this\n"+
				"  up completely. Re-running `pgroll migrate` also resumes from where it\n"+
				"  stopped, which is usually what you want after a transient failure.",
			base,
		)
	default:
		return fmt.Sprintf(
			"%s\n\n"+
				"  Run `pgroll rollback` to undo this migration, or `pgroll revert` to walk\n"+
				"  back the whole uncontracted window if earlier migrations in the same batch\n"+
				"  were already applied. Re-running `pgroll migrate` resumes from where it\n"+
				"  stopped.",
			base,
		)
	}
}
