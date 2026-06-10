// SPDX-License-Identifier: Apache-2.0

package cmd

import (
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

	cmd := &cobra.Command{
		Use:   "revert",
		Short: "Revert the most recent deployment, restoring the previous schema and data",
		Long: "Revert rolls back every migration applied since the last seal point —\n" +
			"the most recent `pgroll migrate` batch — restoring the database's schema,\n" +
			"data, and migration history to the state before that deployment.\n\n" +
			"The revert is lossless: under delayed contraction, destructive DDL is\n" +
			"queued (not executed) until the next deployment departs, so the migrations\n" +
			"being reverted are still physically in their expand phase.\n\n" +
			"The walk can be bounded: --steps N reverts at most N migrations (newest\n" +
			"first); --to <name> reverts everything newer than the named migration,\n" +
			"which becomes the history leaf. After a bounded revert, a version schema\n" +
			"is materialized for the new leaf if it lacks one.\n\n" +
			"The revert window closes when the deployment is sealed: by the next\n" +
			"`pgroll migrate`, or by running `pgroll complete` with no migration in\n" +
			"progress. Sealed migrations cannot be reverted.\n\n" +
			"Applications pinned to the reverted migrations' version schemas must be\n" +
			"repinned to the restore target BEFORE reverting — the newer version\n" +
			"schemas are dropped by the revert.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			m, err := NewRollWithInitCheck(ctx)
			if err != nil {
				return err
			}
			defer m.Close()

			var revertOpts []roll.RevertOption
			if steps > 0 {
				revertOpts = append(revertOpts, roll.WithRevertSteps(steps))
			}
			if toMigration != "" {
				revertOpts = append(revertOpts, roll.WithRevertTo(toMigration))
			}

			targets, err := m.RevertPlan(ctx, revertOpts...)
			if err != nil {
				return err
			}
			if len(targets) == 0 {
				if toMigration != "" {
					fmt.Printf("Nothing to revert: history is already at %q.\n", toMigration)
				} else {
					fmt.Println("Nothing to revert: the last deployment has been sealed (revert window closed).")
				}
				return nil
			}

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

			fmt.Printf("The following %d migration(s) will be reverted (newest first):\n\n", len(targets))
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
			reverted, err := m.Revert(ctx, revertOpts...)
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
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.Flags().IntVar(&steps, "steps", 0, "revert at most N migrations (newest first)")
	cmd.Flags().StringVar(&toMigration, "to", "", "revert everything newer than this migration, which becomes the history leaf")
	cmd.MarkFlagsMutuallyExclusive("steps", "to")

	return cmd
}
