// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/xataio/pgroll/pkg/roll"
)

func checkCmd() *cobra.Command {
	var baseRef string
	var requireTargets bool

	cmd := &cobra.Command{
		Use:   "check <directory>",
		Short: "Validate migration files without requiring a database connection",
		Long: `Check validates migration files in a directory for common issues:

  - YAML/JSON syntax errors
  - Missing or empty 'operations' field
  - Schema name length (must fit PostgreSQL's 63-char limit)
  - depends_on targets that don't exist in the migration set
  - Dependency cycles
  - Raw SQL operations without preconditions (advisory warning)
  - Malformed 'targets', and cross-target depends_on (advisory warning)

With --require-targets, every migration must declare 'targets'. Use it in CI
for a repository that routes migrations to more than one database, so an
undeclared migration fails review instead of the deploy.

With --base, also checks that new migrations sort after the base
branch's latest migration (requires git).`,
		Example: `  pgroll check migrations/
  pgroll check --base origin/main migrations/`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"directory"},
		RunE: func(cmd *cobra.Command, args []string) error {
			migrationsDir := args[0]

			info, err := os.Stat(migrationsDir)
			if err != nil {
				return fmt.Errorf("cannot access directory: %w", err)
			}
			if !info.IsDir() {
				return fmt.Errorf("%q is not a directory", migrationsDir)
			}

			// Run filesystem checks
			result, err := roll.CheckMigrationsDir(os.DirFS(migrationsDir), requireTargets)
			if err != nil {
				return err
			}

			// Run base-branch ordering check if requested
			if baseRef != "" {
				baseResult, err := roll.CheckBaseOrdering(migrationsDir, baseRef)
				if err != nil {
					return fmt.Errorf("base ordering check: %w", err)
				}
				result.Errors = append(result.Errors, baseResult.Errors...)
				result.Warnings = append(result.Warnings, baseResult.Warnings...)
			}

			// Print results
			for _, w := range result.Warnings {
				fmt.Printf("WARN: %s\n", w)
			}
			for _, e := range result.Errors {
				fmt.Printf("ERROR: %s\n", e)
			}

			if result.HasErrors() {
				return fmt.Errorf("check failed with %d error(s)", len(result.Errors))
			}

			if len(result.Warnings) > 0 {
				fmt.Printf("\n%d warning(s), 0 errors\n", len(result.Warnings))
			} else {
				fmt.Println("All checks passed")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&baseRef, "base", "",
		"Git ref for base-branch ordering check (e.g., origin/main)")
	cmd.Flags().BoolVar(&requireTargets, "require-targets", false,
		"Require every migration to declare `targets`; catches at review time what a targeted deploy would otherwise refuse")

	return cmd
}
