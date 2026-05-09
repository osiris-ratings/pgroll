// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/pterm/pterm"
	"github.com/spf13/cobra"

	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
	"github.com/xataio/pgroll/pkg/schema"
)

func materializeCmd() *cobra.Command {
	var migrationsDir string
	var versionOverride string
	var yes bool

	cmd := &cobra.Command{
		Use:   "materialize",
		Short: "Re-create the version schema and views from the live database state",
		Long: "Materialize re-projects the version schema (<schema>_<version>) and the views\n" +
			"apps connect to, using the live target schema as the source of truth. It is a\n" +
			"recovery tool for situations where the live tables exist but the version schema\n" +
			"is missing — for example after an interrupted batched migrate, or when the\n" +
			"version schema was dropped manually. It does NOT modify pgroll state and does\n" +
			"NOT execute migration operations against user tables.\n\n" +
			"Pass --migrations <dir> to fast-forward through unapplied migration files in\n" +
			"memory before projecting views; this lets the projected views match what apps\n" +
			"will see after those migrations land, even though the live tables haven't yet.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()

			m, err := NewRollWithInitCheck(ctx)
			if err != nil {
				return err
			}
			defer m.Close()

			active, err := m.State().IsActiveMigrationPeriod(ctx, m.Schema())
			if err != nil {
				return fmt.Errorf("unable to determine active migration period: %w", err)
			}
			if active {
				return fmt.Errorf(
					"a migration is currently in progress on schema %q; "+
						"run `pgroll rollback` (or wait for the active run to finish) before materializing",
					m.Schema(),
				)
			}

			sc, err := m.State().ReadSchema(ctx, m.Schema())
			if err != nil {
				return fmt.Errorf("failed to read live schema: %w", err)
			}

			var parsedMigs []*migrations.Migration
			if migrationsDir != "" {
				info, err := os.Stat(migrationsDir)
				if err != nil {
					return fmt.Errorf("failed to stat migrations directory: %w", err)
				}
				if !info.IsDir() {
					return fmt.Errorf("migrations directory %q is not a directory", migrationsDir)
				}
				rawMigs, err := m.UnappliedMigrations(ctx, os.DirFS(migrationsDir))
				if err != nil {
					return fmt.Errorf("failed to discover unapplied migrations: %w", err)
				}
				parsedMigs, err = parseMigrations(rawMigs)
				if err != nil {
					return fmt.Errorf("failed to parse migrations: %w", err)
				}
				for _, mig := range parsedMigs {
					if err := mig.UpdateVirtualSchema(ctx, sc); err != nil {
						return fmt.Errorf("failed to project migration %q in memory: %w", mig.Name, err)
					}
				}
			}

			version, err := resolveMaterializeVersion(ctx, m, parsedMigs, versionOverride)
			if err != nil {
				return err
			}

			if err := printMaterializePreFlight(m, sc, version, parsedMigs, os.Stdout); err != nil {
				return fmt.Errorf("pre-flight summary: %w", err)
			}

			if !yes {
				ok, _ := pterm.DefaultInteractiveConfirm.Show(fmt.Sprintf("Materialize version schema %q?", roll.VersionedSchemaName(m.Schema(), version)))
				if !ok {
					return nil
				}
			}

			sp, _ := pterm.DefaultSpinner.WithText(fmt.Sprintf("Materializing %q...", roll.VersionedSchemaName(m.Schema(), version))).Start()
			if err := m.Materialize(ctx, version, sc); err != nil {
				sp.Fail(fmt.Sprintf("Failed to materialize: %s", err))
				return err
			}
			sp.Success(fmt.Sprintf("Version schema %q materialized", roll.VersionedSchemaName(m.Schema(), version)))
			return nil
		},
	}

	cmd.Flags().StringVar(&migrationsDir, "migrations", "", "Optional directory of migration files; unapplied migrations are replayed in memory before projecting views")
	cmd.Flags().StringVar(&versionOverride, "version", "", "Override the version name (default: derived from --migrations or pgroll state)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")

	return cmd
}

// resolveMaterializeVersion picks the version name to materialize.
//
// Precedence:
//  1. --version override, if set.
//  2. Final unapplied migration's VersionSchemaName, if --migrations resolved any.
//  3. state.LatestMigration, if non-nil and non-empty.
//  4. Error — caller must pass --version or --migrations.
func resolveMaterializeVersion(ctx context.Context, m *roll.Roll, parsedMigs []*migrations.Migration, override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if n := len(parsedMigs); n > 0 {
		return parsedMigs[n-1].VersionSchemaName(), nil
	}
	latest, err := m.State().LatestMigration(ctx, m.Schema())
	if err != nil {
		return "", fmt.Errorf("reading latest migration from state: %w", err)
	}
	if latest != nil && *latest != "" {
		return *latest, nil
	}
	return "", fmt.Errorf("no version name could be derived: pass --version or --migrations <dir>")
}

// printMaterializePreFlight reports what `pgroll materialize` is about to
// do, in the same visual style as `pgroll migrate`'s pre-flight.
func printMaterializePreFlight(m *roll.Roll, sc *schema.Schema, version string, parsedMigs []*migrations.Migration, out io.Writer) error {
	const (
		cycleColWidth = 14
		fieldColWidth = 16
	)

	tableCount := 0
	for _, t := range sc.Tables {
		if t.Deleted {
			continue
		}
		if strings.HasPrefix(t.Name, "_pgroll_del_") {
			continue
		}
		tableCount++
	}

	target := roll.VersionedSchemaName(m.Schema(), version)

	title := pterm.NewStyle(pterm.FgWhite, pterm.Bold).Sprint("▶ pgroll materialize")
	subtitle := pterm.FgGray.Sprintf("%s · %d table(s)", m.Schema(), tableCount)
	fmt.Fprintf(out, "\n%s   %s\n\n", title, subtitle)

	stateColor := pterm.FgCyan
	bullet := stateColor.Sprint("●")
	label := pterm.NewStyle(stateColor, pterm.Bold).Sprint("MATERIALIZE")
	labelPad := strings.Repeat(" ", cycleColWidth-len("MATERIALIZE"))
	headline := fmt.Sprintf("project %d table view(s) into %s", tableCount, target)
	fmt.Fprintf(out, "  %s %s%s%s\n", bullet, label, labelPad, headline)

	field := func(name, value string) {
		gray := pterm.FgGray.Sprint(name)
		pad := strings.Repeat(" ", fieldColWidth-len(name))
		fmt.Fprintf(out, "    %s%s%s\n", gray, pad, value)
	}

	field("Source", m.Schema())
	field("Target", target)
	if len(parsedMigs) > 0 {
		field("Layered", fmt.Sprintf("%d unapplied migration(s)", len(parsedMigs)))
		contIndent := strings.Repeat(" ", 4+fieldColWidth)
		for _, mig := range parsedMigs {
			fmt.Fprintf(out, "%s%s\n", contIndent, mig.Name)
		}
	} else {
		field("Layered", pterm.FgGray.Sprint("—"))
	}

	fmt.Fprintln(out)
	return nil
}
