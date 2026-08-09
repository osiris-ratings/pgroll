// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"os"
	"time"

	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
)

func pruneCmd() *cobra.Command {
	var yes bool
	var names []string

	cmd := &cobra.Command{
		Use:   "prune --name <name> [--name <name> ...]",
		Short: "Remove migrations from pgroll's history without executing DDL",
		Long: "Prune deletes the named migrations from pgroll's internal history\n" +
			"(pgroll.migrations) and drops their version schemas (view layers). The\n" +
			"parent chain is rewired across the gaps so history stays linear.\n\n" +
			"No user-table DDL is executed: the physical effects of completed\n" +
			"migrations are NOT reverted. Use prune to reconcile history when applied\n" +
			"migrations no longer exist on disk — e.g. a branch was tested against a\n" +
			"shared database and then abandoned, leaving rows that block\n" +
			"`pgroll migrate` with \"remote migration does not match local migration\".\n\n" +
			"Refuses while a migration is in progress (complete it or run\n" +
			"`pgroll rollback` first) and refuses to prune baseline migrations.\n" +
			"After pruning, the new leaf's version schema may not exist — recreate it\n" +
			"with `pgroll materialize`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := refuseTarget(cmd); err != nil {
				return err
			}

			ctx := cmd.Context()

			m, err := NewRollWithInitCheck(ctx)
			if err != nil {
				return err
			}
			defer m.Close()

			targets, err := m.PruneTargets(ctx, names)
			if err != nil {
				return err
			}
			if len(targets) == 0 {
				fmt.Println("No migrations to prune.")
				return nil
			}

			fmt.Printf("The following %d migration(s) will be removed from pgroll's history:\n\n", len(targets))
			tableData := pterm.TableData{{"Name", "Type", "Created at", "Operations", "Version schema"}}
			for _, t := range targets {
				tableData = append(tableData, []string{
					t.Name,
					t.MigrationType,
					t.CreatedAt.Format(time.RFC3339),
					fmt.Sprintf("%d", t.OperationCount),
					t.VersionSchema,
				})
			}
			if err := pterm.DefaultTable.WithHasHeader().WithData(tableData).Render(); err != nil {
				return err
			}
			fmt.Println()
			pterm.Warning.Println("Physical schema changes made by these migrations are NOT reverted.")

			if !yes {
				ok, _ := pterm.DefaultInteractiveConfirm.Show(
					fmt.Sprintf("Prune %d migration(s) from history?", len(targets)),
				)
				if !ok {
					return nil
				}
			}

			sp, _ := pterm.DefaultSpinner.WithText(
				fmt.Sprintf("Pruning %d migration(s)...", len(targets)),
			).Start()
			pruned, err := m.Prune(ctx, names)
			if err != nil {
				sp.Fail(fmt.Sprintf("Failed to prune: %s", err))
				return err
			}
			sp.Success(fmt.Sprintf("Pruned %d migration(s) from history", len(pruned)))
			for _, t := range pruned {
				fmt.Fprintf(os.Stdout, "    %s %s\n", pterm.FgGreen.Sprint("✓"), t.Name)
			}
			return nil
		},
	}

	cmd.Flags().StringArrayVar(&names, "name", nil, "name of a migration to prune (repeatable)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "skip the confirmation prompt")
	cmd.MarkFlagRequired("name")

	return cmd
}
