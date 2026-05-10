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
)

func stampCmd() *cobra.Command {
	var upTo string
	var migrationType string
	var materialize bool
	var yes bool

	cmd := &cobra.Command{
		Use:   "stamp <directory>",
		Short: "Record migrations as already-applied without executing DDL",
		Long: "Stamp records every migration in <directory> from the current pgroll leaf\n" +
			"through the latest file (or --up-to <name>) as already-applied, without\n" +
			"executing any DDL. It is alembic-style stamping: \"the database is already\n" +
			"in this state, just record the rows.\"\n\n" +
			"Use after loading a SQL dump (or recovering from missing/corrupt state) so\n" +
			"pgroll's migrations table matches the live tables. Idempotent — already-\n" +
			"recorded names are skipped silently. Refuses if a migration is currently in\n" +
			"progress; run `pgroll rollback` first.\n\n" +
			"Pass --materialize to also create the <schema>_<version> view layer over\n" +
			"the leaf, so apps have a schema to connect to. This is the typical\n" +
			"end-to-end recovery flow: load dump → stamp → materialize.\n\n" +
			"For real baselines (capturing current schema as a fresh starting point), use\n" +
			"`pgroll baseline` instead.",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"directory"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			dir := args[0]

			switch migrationType {
			case roll.MigrationTypePgroll, roll.MigrationTypeBaseline, roll.MigrationTypeInferred:
			default:
				return fmt.Errorf(
					"invalid --type %q: must be one of pgroll, baseline, inferred",
					migrationType,
				)
			}

			info, err := os.Stat(dir)
			if err != nil {
				return fmt.Errorf("failed to stat migrations directory: %w", err)
			}
			if !info.IsDir() {
				return fmt.Errorf("migrations directory %q is not a directory", dir)
			}

			m, err := NewRollWithInitCheck(ctx)
			if err != nil {
				return err
			}
			defer m.Close()

			fsys := os.DirFS(dir)
			files, err := migrations.CollectFilesFromDir(fsys)
			if err != nil {
				return fmt.Errorf("failed to list migration files in %q: %w", dir, err)
			}
			if len(files) == 0 {
				return fmt.Errorf("no migration files found in %q", dir)
			}

			raws := make([]*migrations.RawMigration, 0, len(files))
			for _, name := range files {
				raw, err := migrations.ReadRawMigration(fsys, name)
				if err != nil {
					return fmt.Errorf("failed to read migration %q: %w", name, err)
				}
				raws = append(raws, raw)
			}

			if upTo != "" {
				idx := -1
				for i, r := range raws {
					if r.Name == upTo {
						idx = i
						break
					}
				}
				if idx < 0 {
					return fmt.Errorf("--up-to %q not found among migration files in %q", upTo, dir)
				}
				raws = raws[:idx+1]
			}

			toStamp, alreadyStamped, err := classifyStampInputs(ctx, m, raws)
			if err != nil {
				return err
			}

			if err := printStampPreFlight(m, dir, migrationType, materialize, raws, toStamp, alreadyStamped, os.Stdout); err != nil {
				return fmt.Errorf("pre-flight summary: %w", err)
			}

			// Allow `--materialize` to run even when there's nothing to
			// stamp — common case is re-running the recovery flow after
			// the schema was dropped from a previously-stamped DB.
			if len(toStamp) == 0 && !materialize {
				return nil
			}

			if !yes {
				var prompt string
				switch {
				case len(toStamp) > 0 && materialize:
					prompt = fmt.Sprintf("Stamp %d migration(s) as %s and materialize the leaf?", len(toStamp), migrationType)
				case len(toStamp) > 0:
					prompt = fmt.Sprintf("Stamp %d migration(s) as %s?", len(toStamp), migrationType)
				default:
					prompt = "Materialize the leaf version schema?"
				}
				ok, _ := pterm.DefaultInteractiveConfirm.Show(prompt)
				if !ok {
					return nil
				}
			}

			if len(toStamp) > 0 {
				sp, _ := pterm.DefaultSpinner.WithText(
					fmt.Sprintf("Stamping %d migration(s)...", len(toStamp)),
				).Start()
				stamped, err := m.Stamp(ctx, raws, migrationType)
				if err != nil {
					sp.Fail(fmt.Sprintf("Failed to stamp: %s", err))
					return err
				}
				sp.Success(fmt.Sprintf("Stamped %d migration(s)", len(stamped)))
				for _, name := range stamped {
					fmt.Fprintf(os.Stdout, "    %s %s\n", pterm.FgGreen.Sprint("✓"), name)
				}
			}

			if materialize {
				// Use the chain leaf's VersionSchemaName so the materialized
				// schema name matches what `pgroll migrate` would produce for
				// the same migration. Falls back to the migration's name if
				// no version_schema field is set in the file.
				leaf, err := migrations.ParseMigration(raws[len(raws)-1])
				if err != nil {
					return fmt.Errorf("failed to parse leaf migration for materialize: %w", err)
				}
				version := leaf.VersionSchemaName()
				sc, err := m.State().ReadSchema(ctx, m.Schema())
				if err != nil {
					return fmt.Errorf("failed to read live schema for materialize: %w", err)
				}
				target := roll.VersionedSchemaName(m.Schema(), version)
				sp, _ := pterm.DefaultSpinner.WithText(
					fmt.Sprintf("Materializing %q...", target),
				).Start()
				if err := m.Materialize(ctx, version, sc); err != nil {
					sp.Fail(fmt.Sprintf("Failed to materialize: %s", err))
					return err
				}
				sp.Success(fmt.Sprintf("Version schema %q materialized", target))
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&upTo, "up-to", "", "Stop at this migration name (inclusive); default = latest file in the directory")
	cmd.Flags().StringVar(&migrationType, "type", roll.MigrationTypePgroll, "Migration type to record: pgroll, baseline, or inferred")
	cmd.Flags().BoolVar(&materialize, "materialize", false, "After stamping, create the <schema>_<version> view layer over the leaf")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")

	return cmd
}

// classifyStampInputs partitions a slice of raw migrations into those that
// would be newly stamped vs. those that are already recorded. Used purely for
// pre-flight reporting — Roll.Stamp does its own re-check at execution time.
func classifyStampInputs(
	ctx context.Context, m *roll.Roll, raws []*migrations.RawMigration,
) (toStamp, alreadyStamped []string, err error) {
	for _, r := range raws {
		exists, err := m.State().MigrationExists(ctx, m.Schema(), r.Name)
		if err != nil {
			return nil, nil, err
		}
		if exists {
			alreadyStamped = append(alreadyStamped, r.Name)
		} else {
			toStamp = append(toStamp, r.Name)
		}
	}
	return toStamp, alreadyStamped, nil
}

// printStampPreFlight reports what `pgroll stamp` is about to do, in the
// same visual style as the migrate / materialize pre-flight summaries.
func printStampPreFlight(
	m *roll.Roll, dir, migrationType string, materialize bool,
	raws []*migrations.RawMigration,
	toStamp, alreadyStamped []string,
	out io.Writer,
) error {
	const (
		cycleColWidth = 14
		fieldColWidth = 16
	)

	title := pterm.NewStyle(pterm.FgWhite, pterm.Bold).Sprint("▶ pgroll stamp")
	subtitle := pterm.FgGray.Sprintf("%s · %d candidate(s)", dir, len(raws))
	fmt.Fprintf(out, "\n%s   %s\n\n", title, subtitle)

	stateColor := pterm.FgCyan
	stateLabel := "STAMP"
	if len(toStamp) == 0 {
		stateColor = pterm.FgGray
		stateLabel = "NO-OP"
	}
	bullet := stateColor.Sprint("●")
	label := pterm.NewStyle(stateColor, pterm.Bold).Sprint(stateLabel)
	labelPad := strings.Repeat(" ", cycleColWidth-len(stateLabel))
	headline := fmt.Sprintf("%d to stamp · %d already stamped", len(toStamp), len(alreadyStamped))
	fmt.Fprintf(out, "  %s %s%s%s\n", bullet, label, labelPad, headline)

	field := func(name, value string) {
		gray := pterm.FgGray.Sprint(name)
		pad := strings.Repeat(" ", fieldColWidth-len(name))
		fmt.Fprintf(out, "    %s%s%s\n", gray, pad, value)
	}

	field("Source", m.Schema())
	field("Type", migrationType)
	if len(raws) > 0 {
		field("Up to", raws[len(raws)-1].Name)
	}
	if materialize && len(raws) > 0 {
		field("Materialize", roll.VersionedSchemaName(m.Schema(), raws[len(raws)-1].Name))
	}

	if len(toStamp) > 0 {
		field("Stamping", fmt.Sprintf("%d migration(s)", len(toStamp)))
		contIndent := strings.Repeat(" ", 4+fieldColWidth)
		for _, n := range toStamp {
			fmt.Fprintf(out, "%s%s\n", contIndent, n)
		}
	}

	fmt.Fprintln(out)
	return nil
}
