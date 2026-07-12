// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pterm/pterm"
	"github.com/spf13/cobra"

	"github.com/xataio/pgroll/pkg/roll"
)

func planCmd() *cobra.Command {
	var jsonOut bool
	var toMigration string

	cmd := &cobra.Command{
		Use:   "plan <directory>",
		Short: "Print the plan to converge the database to a local migrations directory",
		Long: "Plan is read-only. It computes what it would take to make the target\n" +
			"database's migration history match a local migrations directory — which\n" +
			"migrations to apply forward, which to revert, and the restore target — \n" +
			"WITHOUT executing anything. It is the machine-readable form (via --json)\n" +
			"of what `migrate` and `revert` would do.\n\n" +
			"Exit status is zero whenever a plan can be produced, including when there\n" +
			"is nothing to do or the convergence is blocked; branch on the JSON fields\n" +
			"(apply / revert / diverged / blocked), not the exit code. A non-zero exit\n" +
			"means no plan could be produced at all (database unreachable, pgroll\n" +
			"uninitialized, or a --to target absent from history).",
		Example:   "plan ./migrations --json",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"directory"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			migrationsDir := args[0]

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

			var opts []roll.PlanOption
			if toMigration != "" {
				opts = append(opts, roll.WithPlanTo(toMigration))
			}

			plan, err := m.Plan(ctx, os.DirFS(migrationsDir), opts...)
			if err != nil {
				return err
			}

			if jsonOut {
				out, err := json.MarshalIndent(plan, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(out))
				return nil
			}

			printPlanHuman(plan, migrationsDir, os.Stdout)
			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the plan as JSON")
	cmd.Flags().StringVar(&toMigration, "to", "", "override the convergence target: revert history down to this migration, which must already exist in the database")

	return cmd
}

// planState is the one-word verdict shown at the top of the human plan.
type planState struct {
	label string
	color pterm.Color
}

func planHeadline(p *roll.PlanResult) planState {
	switch {
	case p.InSync:
		return planState{"IN SYNC", pterm.FgGreen}
	case p.Blocked.Count > 0:
		return planState{"BLOCKED", pterm.FgRed}
	case p.Diverged:
		return planState{"DIVERGED", pterm.FgYellow}
	case p.Apply.Count > 0 && p.Revert.Count > 0:
		return planState{"CONVERGE", pterm.FgYellow}
	case p.Revert.Count > 0:
		return planState{"REVERT", pterm.FgYellow}
	case p.Apply.Count > 0:
		return planState{"APPLY", pterm.FgGreen}
	default:
		return planState{"NOTHING TO DO", pterm.FgGray}
	}
}

// printPlanHuman renders the plan as an aligned, colorized summary: the
// convergence verdict, point state, then the Apply / Revert / Blocked legs
// with the restore target and pin warnings. There is no seal phase —
// contraction is an explicit `pgroll complete`, never surfaced here.
func printPlanHuman(p *roll.PlanResult, dir string, out io.Writer) {
	const fieldColWidth = 16

	dash := func(s *string) string {
		if s == nil || *s == "" {
			return pterm.FgGray.Sprint("—")
		}
		return *s
	}

	field := func(name, value string) {
		gray := pterm.FgGray.Sprint(name)
		pad := strings.Repeat(" ", fieldColWidth-len(name))
		fmt.Fprintf(out, "    %s%s%s\n", gray, pad, value)
	}

	// list prints "Label   <first>" and aligns the rest beneath it.
	list := func(label string, names []string) {
		if len(names) == 0 {
			field(label, pterm.FgGray.Sprint("—"))
			return
		}
		field(label, names[0])
		contIndent := strings.Repeat(" ", 4+fieldColWidth)
		for _, n := range names[1:] {
			fmt.Fprintf(out, "%s%s\n", contIndent, n)
		}
	}

	title := pterm.NewStyle(pterm.FgWhite, pterm.Bold).Sprint("▶ pgroll plan")
	subtitle := pterm.FgGray.Sprintf("%s · schema %s", dir, p.Schema)
	fmt.Fprintf(out, "\n%s   %s\n\n", title, subtitle)

	state := planHeadline(p)
	bullet := state.color.Sprint("●")
	label := pterm.NewStyle(state.color, pterm.Bold).Sprint(state.label)
	fmt.Fprintf(out, "  %s %s\n\n", bullet, label)

	field("Status", p.Status)
	field("Live schema", dash(p.LiveSchema))
	field("DB latest", dash(p.DBLatest))
	field("Local latest", dash(p.LocalLatest))
	fmt.Fprintln(out)

	list(fmt.Sprintf("Apply (%d)", p.Apply.Count), p.Apply.Migrations)
	list(fmt.Sprintf("Revert (%d)", p.Revert.Count), p.Revert.Migrations)

	if p.Revert.Count > 0 {
		field("Restore target", dash(p.Revert.ToSchema))
	}

	if p.Blocked.Count > 0 {
		fmt.Fprintln(out)
		reason := ""
		if p.Blocked.Reason != nil {
			reason = *p.Blocked.Reason
		}
		list(fmt.Sprintf("Blocked (%d)", p.Blocked.Count), p.Blocked.Migrations)
		fmt.Fprintf(out, "\n    %s convergence is blocked (%s): these database migrations are absent from the checkout and cannot be cleanly reverted.\n",
			pterm.FgRed.Sprint("✗"), reason)
	}

	if p.Revert.Count > 0 {
		fmt.Fprintln(out)
		pterm.Warning.WithWriter(out).Printfln("Applications must be pinned to %s before reverting — newer version schemas will be dropped.", dash(p.Revert.ToSchema))
		if p.Revert.ContainsContracted {
			pterm.Warning.WithWriter(out).Printfln("The revert includes contracted migrations: schema shape is restored exactly, data is re-derived best-effort.")
		}
	}

	if p.Diverged {
		fmt.Fprintln(out)
		pterm.Warning.WithWriter(out).Printfln("Local and database histories have diverged: neither leaf appears in the other's history.")
	}

	fmt.Fprintln(out)
}
