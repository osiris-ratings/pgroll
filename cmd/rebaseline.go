// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/pterm/pterm"
	"github.com/spf13/cobra"

	"github.com/xataio/pgroll/cmd/flags"
	"github.com/xataio/pgroll/pkg/roll"
)

func rebaselineCmd() *cobra.Command {
	var check, yes bool

	cmd := &cobra.Command{
		Use:   "rebaseline <directory>",
		Short: "Adopt the directory's marked baseline by converting its applied row in place",
		Long: "Rebaseline reconciles this database's baseline with the directory's: it finds\n" +
			"the migration marked `baseline: true` and, when this database applied that\n" +
			"migration as an ordinary migration, converts its history row to a baseline in\n" +
			"place (UPDATE migration_type = 'baseline', nothing else). The baseline folds in\n" +
			"every migration whose name sorts at or before its own: their history rows are\n" +
			"hidden, not deleted, and their files are ignored.\n\n" +
			"This is the deploy-time companion of migration-history truncation: after old\n" +
			"migration files are deleted and the anchor file is rewritten as a marked\n" +
			"baseline, run `pgroll rebaseline <dir>` before `pgroll plan` or\n" +
			"`pgroll migrate` so databases carrying the full applied history accept the\n" +
			"truncated directory.\n\n" +
			"Idempotent and safe to run unconditionally: already-converted databases,\n" +
			"databases whose baseline is newer than the directory's (an old checkout), and\n" +
			"databases with no history at all are all no-ops. A database whose history is\n" +
			"missing the anchor entirely is a hard error (exit 2): it must catch up from a\n" +
			"pre-truncation checkout or be rebuilt — running `pgroll migrate` there would\n" +
			"execute the baseline's full schema snapshot.\n\n" +
			"The conversion refuses unless the anchor row is done, sealed, and contracted,\n" +
			"nothing is in progress, and no baseline sorts after the anchor. The folded\n" +
			"migrations are then checked in one of two ways.\n\n" +
			"When `<baseline>.folds` sits beside the baseline file, it lists the folded\n" +
			"migrations, one per line: a name, then optionally its comma-separated targets.\n" +
			"The database must have applied and contracted every listed migration that\n" +
			"--target selects (all of them with no --target), and must hold no row at or\n" +
			"below the anchor that the list does not name. The order it applied them in\n" +
			"does not matter. Pass the same --target the database migrates with: a host\n" +
			"that receives only some targets never applied the others' migrations.\n\n" +
			"Without that file, the database must have applied its migrations in filename\n" +
			"order across the anchor, and --target has no effect.\n\n" +
			"Exit codes: 0 — converged (converted, or nothing to do); 2 — refusal (missing\n" +
			"or misplaced baseline marker, absent anchor row, or a failed safety audit);\n" +
			"3 — with --check, a conversion is pending; 1 — unexpected error.",
		Example:   "rebaseline ./migrations",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"directory"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			dir := args[0]

			info, err := os.Stat(dir)
			if err != nil {
				return fmt.Errorf("failed to stat directory: %w", err)
			}
			if !info.IsDir() {
				return fmt.Errorf("migrations directory %q is not a directory", dir)
			}

			m, err := NewRollWithInitCheck(ctx)
			if err != nil {
				return err
			}
			defer m.Close()

			// exitWith closes the roll instance before exiting so the deferred
			// Close (skipped by os.Exit) cannot leak the connection.
			exitWith := func(code int) {
				m.Close()
				os.Exit(code)
			}

			// Always audit first (dry run): converged outcomes and refusals are
			// reported without writing, and the confirmation prompt below can
			// describe exactly what a real run would do.
			res, err := m.Rebaseline(ctx, os.DirFS(dir), true)
			if err != nil {
				if errors.Is(err, roll.ErrRebaselineRefused) {
					fmt.Fprintf(os.Stderr, "Error: %s\n", err)
					exitWith(2)
				}
				return err
			}

			printRebaselineStatus(dir, m.Schema(), res)

			if res.Outcome != roll.RebaselinePending {
				return nil
			}

			if check {
				fmt.Println("Conversion pending: run `pgroll rebaseline` without --check to convert.")
				exitWith(3)
			}

			if !yes {
				ok, _ := pterm.DefaultInteractiveConfirm.Show(fmt.Sprintf(
					"Convert %q to this database's baseline, hiding every migration named at or before it?",
					res.BaselineName,
				))
				if !ok {
					return nil
				}
			}

			res, err = m.Rebaseline(ctx, os.DirFS(dir), false)
			if err != nil {
				if errors.Is(err, roll.ErrRebaselineRefused) {
					fmt.Fprintf(os.Stderr, "Error: %s\n", err)
					exitWith(2)
				}
				return err
			}
			fmt.Printf("%s Converted %q to baseline in place\n",
				pterm.FgGreen.Sprint("✓"), res.BaselineName)
			return nil
		},
	}

	cmd.Flags().BoolVar(&check, "check", false, "Report whether a conversion is pending without writing anything (exit 3 when pending)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")

	return cmd
}

// printRebaselineStatus reports what rebaseline found, in the same visual
// style as the migrate / stamp pre-flight summaries.
func printRebaselineStatus(dir, schema string, res *roll.RebaselineResult) {
	const cycleColWidth = 18

	title := pterm.NewStyle(pterm.FgWhite, pterm.Bold).Sprint("▶ pgroll rebaseline")
	subtitle := pterm.FgGray.Sprintf("%s · schema %s", dir, schema)
	fmt.Printf("\n%s   %s\n\n", title, subtitle)

	stateColor := pterm.FgGray
	detail := ""
	switch res.Outcome {
	case roll.RebaselinePending:
		stateColor = pterm.FgYellow
		detail = fmt.Sprintf("%q is applied as an ordinary migration; every audit passes", res.BaselineName)
	case roll.RebaselineAlreadyBaseline:
		detail = fmt.Sprintf("%q is already this database's baseline", res.BaselineName)
	case roll.RebaselineDBBaselineNewer:
		detail = fmt.Sprintf("database baseline %q is newer than directory baseline %q", res.DBBaseline, res.BaselineName)
	case roll.RebaselineEmptyHistory:
		detail = "schema has no migration history; baseline adoption belongs to bootstrap"
	case roll.RebaselineConverted:
		stateColor = pterm.FgGreen
		detail = fmt.Sprintf("%q converted to baseline in place", res.BaselineName)
	}

	bullet := stateColor.Sprint("●")
	label := pterm.NewStyle(stateColor, pterm.Bold).Sprint(string(res.Outcome))
	pad := cycleColWidth - len(string(res.Outcome))
	if pad < 1 {
		pad = 1
	}
	fmt.Printf("  %s %s%s%s\n", bullet, label, fmt.Sprintf("%*s", pad, ""), detail)

	if res.DBBaseline != "" && res.Outcome == roll.RebaselinePending {
		fmt.Printf("    %s%s\n", pterm.FgGray.Sprint("Replaces        "), res.DBBaseline)
	}
	if res.Outcome == roll.RebaselinePending || res.Outcome == roll.RebaselineConverted {
		audit := "apply order (the directory declares no fold set)"
		if res.FoldSetDeclared {
			audit = "fold set"
			if target := flags.Target(); target != "" {
				audit += fmt.Sprintf(" · target %s", target)
			}
		}
		fmt.Printf("    %s%s\n", pterm.FgGray.Sprint("Audited by      "), audit)
	}
	for _, note := range res.Notes {
		fmt.Printf("    %s %s\n", pterm.FgYellow.Sprint("⚠"), note)
	}
	fmt.Println()
}
