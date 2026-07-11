// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/pterm/pterm"
	"github.com/spf13/cobra"

	"github.com/xataio/pgroll/pkg/roll"
)

func revertCmd() *cobra.Command {
	var yes bool
	var steps int
	var toMigration string
	var pastSeal bool
	var expandOnly bool

	cmd := &cobra.Command{
		Use:   "revert",
		Short: "Revert migrations, restoring an earlier point in the migration chain",
		Long: "Revert moves the database backward along the migration chain.\n\n" +
			"While a deployment is in flight — applied but not yet contracted by\n" +
			"`pgroll complete` — its migrations are still physically in their expand\n" +
			"phase and revert walks them back out LOSSLESSLY (schema, data, and\n" +
			"history are restored exactly).\n\n" +
			"Once a deployment has been contracted, revert switches to inversion:\n" +
			"synthesized inverse migrations run forward through the normal\n" +
			"zero-downtime engine, then the reverted migrations and their inverses\n" +
			"are pruned from history. Schema shape is restored exactly; data is\n" +
			"re-derived through the original up/down expressions — best effort.\n\n" +
			"Bounds: --steps N reverts at most N in-flight migrations (newest first);\n" +
			"--to <name> reverts everything newer than the named migration, composing\n" +
			"the lossless and inversion legs as needed. --expand-only stops an\n" +
			"inversion revert after its expand phase so apps can repin to the\n" +
			"restored schema before `pgroll complete` contracts and finishes it.\n\n" +
			"Applications pinned to the reverted migrations' version schemas must be\n" +
			"repinned to the restore target BEFORE reverting — the newer version\n" +
			"schemas are dropped as the revert proceeds.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			m, err := NewRollWithInitCheck(ctx)
			if err != nil {
				return err
			}
			defer m.Close()

			if pastSeal {
				pterm.Warning.Println("--past-seal is deprecated: `revert --to` now reverts contracted history automatically.")
			}
			if expandOnly && toMigration == "" {
				return fmt.Errorf("--expand-only requires --to: it applies to inversion reverts of contracted history")
			}

			// Resume an interrupted inversion revert before planning anything
			// new — the intermediate states it leaves behind are refused by
			// the planners.
			resumed, err := maybeResumeSealedRevert(ctx, m, toMigration, yes)
			if err != nil || resumed {
				return err
			}

			// --steps bounds the in-flight window only.
			if steps > 0 {
				plan, err := m.RevertPlan(ctx, roll.WithRevertSteps(steps))
				if err != nil {
					return err
				}
				if len(plan) == 0 {
					fmt.Println("Nothing to revert: no deployment is in flight (everything is contracted). Use --to <name> to revert contracted history.")
					return nil
				}
				return runWindowRevert(ctx, m, plan, yes, roll.WithRevertSteps(steps))
			}

			// Bare revert: walk back the whole in-flight window.
			if toMigration == "" {
				plan, err := m.RevertPlan(ctx)
				if err != nil {
					return err
				}
				if len(plan) == 0 {
					fmt.Println("Nothing to revert: no deployment is in flight (everything is contracted). Use --to <name> to revert contracted history.")
					return nil
				}
				return runWindowRevert(ctx, m, plan, yes)
			}

			// --to <name>: resolve against the in-flight window first; a
			// sealed target switches to inversion, composing both legs when
			// the window is open above it.
			plan, err := m.RevertPlan(ctx, roll.WithRevertTo(toMigration))
			switch {
			case err == nil && len(plan) == 0:
				fmt.Printf("Nothing to revert: history is already at %q.\n", toMigration)
				return nil
			case err == nil:
				if expandOnly {
					return fmt.Errorf("--expand-only does not apply: %q is within the in-flight window and reverts losslessly", toMigration)
				}
				return runWindowRevert(ctx, m, plan, yes, roll.WithRevertTo(toMigration))
			case errors.Is(err, roll.ErrRevertTargetSealed):
				return runSealedRevert(ctx, m, toMigration, yes, expandOnly)
			default:
				return err
			}
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().IntVar(&steps, "steps", 0, "revert at most N in-flight migrations (newest first)")
	cmd.Flags().StringVar(&toMigration, "to", "", "revert everything newer than this migration, which becomes the history leaf")
	cmd.Flags().BoolVar(&expandOnly, "expand-only", false, "stop an inversion revert after its expand phase: the restored schema exists for apps to repin to; `pgroll complete` finishes the revert")
	cmd.Flags().BoolVar(&pastSeal, "past-seal", false, "deprecated: --to now reverts contracted history automatically")
	_ = cmd.Flags().MarkHidden("past-seal")
	cmd.MarkFlagsMutuallyExclusive("steps", "to")

	return cmd
}

// runWindowRevert previews, confirms, and executes a lossless revert of
// in-flight (not yet contracted) migrations.
func runWindowRevert(ctx context.Context, m *roll.Roll, targets []roll.RevertTarget, yes bool, opts ...roll.RevertOption) error {
	// The database returns to the oldest target's parent; that is the
	// version schema apps must be pinned to before the revert runs.
	restoreTo := "(empty database — no version schema will remain)"
	oldest := targets[len(targets)-1]
	if oldest.Parent != nil {
		parentMig, err := m.State().GetMigration(ctx, m.Schema(), *oldest.Parent)
		if err != nil {
			return fmt.Errorf("unable to resolve the migration to restore to: %w", err)
		}
		restoreTo = roll.VersionedSchemaName(m.Schema(), parentMig.VersionSchemaName())
	}

	fmt.Printf("The following %d in-flight migration(s) will be reverted losslessly (newest first):\n\n", len(targets))
	tableData := pterm.TableData{{"Name", "State", "Operations", "Version schema"}}
	for _, t := range targets {
		tableData = append(tableData, []string{
			t.Name,
			string(t.State),
			fmt.Sprintf("%d", t.OperationCount),
			t.VersionSchema,
		})
	}
	if err := pterm.DefaultTable.WithHasHeader().WithData(tableData).Render(); err != nil {
		return err
	}
	fmt.Println()
	pterm.Warning.Printfln("Applications must be pinned to %q before reverting — newer version schemas will be dropped.", restoreTo)

	if !yes {
		ok, _ := pterm.DefaultInteractiveConfirm.Show(
			fmt.Sprintf("Revert %d migration(s)?", len(targets)),
		)
		if !ok {
			return nil
		}
	}

	sp, _ := pterm.DefaultSpinner.WithText(
		fmt.Sprintf("Reverting %d migration(s)...", len(targets)),
	).Start()
	reverted, err := m.Revert(ctx, opts...)
	if err != nil {
		sp.Fail(fmt.Sprintf("Failed to revert: %s", err))
		return err
	}
	sp.Success(fmt.Sprintf("Reverted %d migration(s)", len(reverted)))
	for _, t := range reverted {
		fmt.Fprintf(os.Stdout, "    %s %s\n", pterm.FgGreen.Sprint("✓"), t.Name)
	}
	fmt.Printf("\nDatabase restored. Live version schema: %s\n", restoreTo)
	return nil
}

// maybeResumeSealedRevert finishes an interrupted inversion revert if one is
// pending, reporting whether it handled the invocation.
func maybeResumeSealedRevert(ctx context.Context, m *roll.Roll, to string, yes bool) (bool, error) {
	resume, err := m.PendingSealedRevertResume(ctx)
	if err != nil {
		return false, err
	}
	if resume == "" {
		return false, nil
	}
	if to == "" {
		return true, fmt.Errorf("%s — re-run with --to <boundary> to resume it (or `pgroll complete` to finish a fully-applied one)", resume)
	}
	pterm.Warning.Printfln("Resuming an interrupted revert: %s.", resume)
	if !yes {
		ok, _ := pterm.DefaultInteractiveConfirm.Show("Resume the interrupted revert?")
		if !ok {
			return true, nil
		}
	}
	sp, _ := pterm.DefaultSpinner.WithText("Resuming revert...").Start()
	result, err := m.RevertSealed(ctx, to, nil)
	if err != nil {
		sp.Fail(fmt.Sprintf("Failed to resume: %s", err))
		return true, err
	}
	if result == nil {
		sp.Success("Nothing to revert.")
		return true, nil
	}
	sp.Success(fmt.Sprintf("Reverted %d migration(s)", len(result.Targets)))
	for _, t := range result.Targets {
		fmt.Fprintf(os.Stdout, "    %s %s\n", pterm.FgGreen.Sprint("✓"), t)
	}
	if result.BoundaryVersionSchema != "" {
		fmt.Printf("\nDatabase restored. Live version schema: %s\n",
			roll.VersionedSchemaName(m.Schema(), result.BoundaryVersionSchema))
	}
	return true, nil
}

// runSealedRevert drives an inversion revert of contracted history down to
// `to`, composing a lossless leg first when the in-flight window is open
// above the target: plan both legs, one loud preview, one confirmation, then
// execute window leg → inversion leg.
func runSealedRevert(ctx context.Context, m *roll.Roll, to string, yes, expandOnly bool) error {
	// The window leg: everything in flight reverts before the inversion leg
	// can run. An empty slice means the window is closed (pure inversion).
	windowTargets, err := m.RevertTargets(ctx)
	if err != nil {
		return fmt.Errorf("the in-flight window above %q cannot be reverted: %w", to, err)
	}

	var plan *roll.SealedRevertPlan
	if len(windowTargets) > 0 {
		oldest := windowTargets[len(windowTargets)-1]
		if oldest.Parent == nil {
			return fmt.Errorf("migration %q not found beneath the in-flight window", to)
		}
		plan, err = m.PlanRevertSealedBelowWindow(ctx, to, *oldest.Parent)
	} else {
		plan, err = m.PlanRevertSealed(ctx, to)
	}
	if err != nil {
		return err
	}
	if plan == nil && len(windowTargets) == 0 {
		fmt.Printf("Nothing to revert: history is already at %q.\n", to)
		return nil
	}

	// Preview: the window leg (lossless), then the inversion leg.
	if len(windowTargets) > 0 {
		fmt.Printf("First, %d in-flight migration(s) will be reverted losslessly (newest first):\n\n", len(windowTargets))
		tableData := pterm.TableData{{"Name", "State", "Operations"}}
		for _, t := range windowTargets {
			tableData = append(tableData, []string{t.Name, string(t.State), fmt.Sprintf("%d", t.OperationCount)})
		}
		if err := pterm.DefaultTable.WithHasHeader().WithData(tableData).Render(); err != nil {
			return err
		}
		fmt.Println()
	}

	restoreTo := ""
	if plan != nil {
		restoreTo = roll.VersionedSchemaName(m.Schema(), plan.BoundaryVersionSchema)
		fmt.Printf("The following %d CONTRACTED migration(s) will be reverted via synthesized inverses (newest first):\n\n", len(plan.Targets))
		tableData := pterm.TableData{{"Name", "Inverse"}}
		for i, t := range plan.Targets {
			tableData = append(tableData, []string{t, plan.Inverses[len(plan.Inverses)-1-i].Name})
		}
		if err := pterm.DefaultTable.WithHasHeader().WithData(tableData).Render(); err != nil {
			return err
		}
		fmt.Println()
		pterm.Warning.Printfln("These migrations' contraction has RUN. Schema shape is restored exactly;")
		pterm.Warning.Printfln("data is re-derived through the original up/down expressions — best effort.")
		if expandOnly {
			pterm.Info.Printfln("Expand-only: the restored schema %q will be created alongside the current one;", restoreTo)
			pterm.Info.Printfln("repin applications to it, then run `pgroll complete` to finish the revert.")
		} else {
			pterm.Warning.Printfln("Applications must be pinned to %q before reverting.", restoreTo)
		}
	}

	total := len(windowTargets)
	if plan != nil {
		total += len(plan.Targets)
	}
	if !yes {
		ok, _ := pterm.DefaultInteractiveConfirm.Show(
			fmt.Sprintf("Revert %d migration(s)?", total),
		)
		if !ok {
			return nil
		}
	}

	// Window leg first: after it, the inversion leg re-plans against the
	// now-closed window (the sealed segment beneath is unaffected by the
	// walk, so the executed plan matches the preview).
	if len(windowTargets) > 0 {
		sp, _ := pterm.DefaultSpinner.WithText(
			fmt.Sprintf("Reverting %d in-flight migration(s)...", len(windowTargets)),
		).Start()
		if _, err := m.Revert(ctx); err != nil {
			sp.Fail(fmt.Sprintf("Failed to revert the in-flight window: %s", err))
			return err
		}
		sp.Success(fmt.Sprintf("Reverted %d in-flight migration(s)", len(windowTargets)))
	}
	if plan == nil {
		return nil
	}

	var sealedOpts []roll.SealedRevertOption
	if expandOnly {
		sealedOpts = append(sealedOpts, roll.WithExpandOnly())
	}
	sp, _ := pterm.DefaultSpinner.WithText(
		fmt.Sprintf("Reverting %d contracted migration(s)...", len(plan.Targets)),
	).Start()
	result, err := m.RevertSealed(ctx, to, nil, sealedOpts...)
	if err != nil {
		sp.Fail(fmt.Sprintf("Failed to revert: %s", err))
		return err
	}
	if result == nil {
		sp.Success("Nothing to revert.")
		return nil
	}
	if expandOnly {
		sp.Success(fmt.Sprintf("Applied the inverse train for %d migration(s); final inverse left active", len(result.Targets)))
		fmt.Printf("\nRestored schema available: %s\n", restoreTo)
		fmt.Println("Repin applications to it, then run `pgroll complete` to contract and finish the revert.")
		return nil
	}
	sp.Success(fmt.Sprintf("Reverted %d contracted migration(s)", len(result.Targets)))
	for _, t := range result.Targets {
		fmt.Fprintf(os.Stdout, "    %s %s\n", pterm.FgGreen.Sprint("✓"), t)
	}
	fmt.Printf("\nDatabase restored. Live version schema: %s\n", restoreTo)
	return nil
}
