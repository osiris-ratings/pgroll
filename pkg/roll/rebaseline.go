// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/state"
)

// ErrRebaselineRefused wraps every rebaseline failure that means "this
// database or directory is not shaped for the conversion" — a missing or
// misplaced baseline marker, an anchor with no applied row, or a state-layer
// audit refusal. Callers use it to distinguish refusals (which have a runbook
// remedy) from transient errors.
var ErrRebaselineRefused = errors.New("rebaseline refused")

// RebaselineOutcome classifies what Rebaseline found and did.
type RebaselineOutcome string

const (
	// RebaselineConverted: the anchor's applied row was converted to a
	// baseline in place.
	RebaselineConverted RebaselineOutcome = "CONVERTED"
	// RebaselineAlreadyBaseline: the anchor is already the database's
	// baseline; nothing to do.
	RebaselineAlreadyBaseline RebaselineOutcome = "ALREADY-BASELINE"
	// RebaselineDBBaselineNewer: the database's baseline sorts after the
	// directory's — an older checkout reconciling against a database that has
	// since been rebaselined (the hotfix case). Nothing to do.
	RebaselineDBBaselineNewer RebaselineOutcome = "DB-BASELINE-NEWER"
	// RebaselineEmptyHistory: the schema has no migration history at all;
	// baseline adoption belongs to the bootstrap flow (`pgroll baseline` /
	// `pgroll stamp`), not to rebaseline. Nothing to do.
	RebaselineEmptyHistory RebaselineOutcome = "EMPTY-HISTORY"
	// RebaselinePending: check-only mode found an applied row that would be
	// converted, and every audit passes.
	RebaselinePending RebaselineOutcome = "PENDING"
)

// RebaselineResult reports what Rebaseline found and did.
type RebaselineResult struct {
	// Outcome classifies the run.
	Outcome RebaselineOutcome
	// BaselineName is the directory's marked baseline (the anchor).
	BaselineName string
	// DBBaseline is the database's latest baseline before this run, empty if
	// none existed.
	DBBaseline string
	// Notes carries advisory findings (e.g. a '{}' resulting_schema on the
	// anchor row).
	Notes []string
}

// FindBaselineMigration scans a migrations directory for the single file
// marked `baseline: true` and returns it. It refuses (wrapping
// ErrRebaselineRefused) when no file carries the marker, when more than one
// does, or when the marked file is not lexicographically first — the same
// rules `pgroll check` enforces, restated here because rebaseline acts on
// them.
func FindBaselineMigration(dir fs.FS) (*migrations.RawMigration, error) {
	files, err := migrations.CollectFilesFromDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading migration files: %w", err)
	}

	var marked *migrations.RawMigration
	var markedFile string
	for _, file := range files {
		raw, err := migrations.ReadRawMigration(dir, file)
		if err != nil {
			return nil, fmt.Errorf("reading migration file %q: %w", file, err)
		}
		if !raw.Baseline {
			continue
		}
		if marked != nil {
			return nil, fmt.Errorf("%w: both %q and %q declare `baseline: true`; "+
				"a directory has at most one baseline", ErrRebaselineRefused, markedFile, file)
		}
		marked = raw
		markedFile = file
	}

	if marked == nil {
		return nil, fmt.Errorf("%w: no migration declares `baseline: true`; "+
			"rebaseline needs a marked baseline to anchor on", ErrRebaselineRefused)
	}
	if len(files) > 0 && markedFile != files[0] {
		return nil, fmt.Errorf("%w: baseline %q is not the first migration in the directory "+
			"(%q sorts before it)", ErrRebaselineRefused, markedFile, files[0])
	}
	return marked, nil
}

// Rebaseline reconciles the database's baseline with the directory's marked
// baseline. It is the deploy-time companion of migration-history truncation:
// after old migration files are deleted and the anchor file is rewritten as a
// marked baseline, every database that applied the anchor as an ordinary
// migration adopts the truncated directory by having that row converted to a
// baseline in place (state.ConvertMigrationToBaseline has the full safety
// contract).
//
// Idempotent and safe to run unconditionally before `pgroll plan` /
// `pgroll migrate`:
//
//   - anchor row already a baseline → ALREADY-BASELINE, no-op
//   - database baseline sorts after the directory's (an old checkout against
//     a newer-baselined database) → DB-BASELINE-NEWER, no-op
//   - schema has no history at all (fresh database; bootstrap owns baseline
//     adoption) → EMPTY-HISTORY, no-op
//   - anchor applied as an ordinary migration → audit and convert in place
//     (or report PENDING in check-only mode)
//   - anchor has no row and history is non-empty → hard error wrapping
//     ErrRebaselineRefused: this database either predates the anchor (catch
//     up from a pre-truncation checkout first) or must be rebuilt. Running
//     `pgroll migrate` in that state would treat the baseline file as an
//     ordinary unapplied migration and execute its full schema snapshot.
//
// checkOnly runs every audit and reports without writing.
func (m *Roll) Rebaseline(ctx context.Context, dir fs.FS, checkOnly bool) (*RebaselineResult, error) {
	marked, err := FindBaselineMigration(dir)
	if err != nil {
		return nil, err
	}

	result := &RebaselineResult{BaselineName: marked.Name}

	latest, err := m.state.LatestMigration(ctx, m.schema)
	if err != nil {
		return nil, fmt.Errorf("rebaseline: reading latest migration: %w", err)
	}
	if latest == nil || *latest == "" {
		result.Outcome = RebaselineEmptyHistory
		return result, nil
	}

	dbBaseline, err := m.state.LatestBaseline(ctx, m.schema)
	if err != nil {
		return nil, fmt.Errorf("rebaseline: reading latest baseline: %w", err)
	}
	if dbBaseline != nil {
		result.DBBaseline = dbBaseline.Name
		if dbBaseline.Name == marked.Name {
			result.Outcome = RebaselineAlreadyBaseline
			return result, nil
		}
		if dbBaseline.Name > marked.Name {
			result.Outcome = RebaselineDBBaselineNewer
			result.Notes = append(result.Notes, fmt.Sprintf(
				"database baseline %q sorts after directory baseline %q; "+
					"this checkout predates a later rebaseline and needs nothing done",
				dbBaseline.Name, marked.Name,
			))
			return result, nil
		}
	}

	exists, err := m.state.MigrationExists(ctx, m.schema, marked.Name)
	if err != nil {
		return nil, fmt.Errorf("rebaseline: checking for anchor row: %w", err)
	}
	if !exists {
		return nil, fmt.Errorf("%w: baseline %q has no applied row in the history of schema %q. "+
			"This database either predates the baseline — catch up by running `pgroll migrate` "+
			"from a checkout that still has the pre-truncation migrations — or must be rebuilt. "+
			"Refusing, because `pgroll migrate` would otherwise execute the baseline's full "+
			"schema snapshot into a non-empty database",
			ErrRebaselineRefused, marked.Name, m.schema)
	}

	convRes, err := m.state.ConvertMigrationToBaseline(ctx, m.schema, marked.Name, checkOnly)
	if err != nil {
		if errors.Is(err, state.ErrRebaselineRefused) {
			return nil, fmt.Errorf("%w: %w", ErrRebaselineRefused, err)
		}
		return nil, err
	}

	if convRes.EmptyResultingSchema {
		result.Notes = append(result.Notes, fmt.Sprintf(
			"anchor %q carries a '{}' resulting_schema (hand-stamped ancestry); "+
				"SchemaAfterMigration on it will return an empty schema", marked.Name,
		))
	}

	switch {
	case convRes.AlreadyBaseline:
		// Unlikely here (LatestBaseline above catches it) but kept for
		// idempotency under races.
		result.Outcome = RebaselineAlreadyBaseline
	case checkOnly:
		result.Outcome = RebaselinePending
	default:
		result.Outcome = RebaselineConverted
		m.logger.Info("rebaseline: converted migration to baseline in place",
			"schema", m.schema, "name", marked.Name)
	}
	return result, nil
}

// BaselineExecutionRefusedError explains why pgroll will not execute a
// migration marked `baseline: true` into a database that already has
// migration history. A baseline is a schema snapshot, not a change to run —
// executing one replays the entire schema into a database that already has
// it.
func BaselineExecutionRefusedError(name string) error {
	return fmt.Errorf("refusing to execute baseline migration %q: it is marked `baseline: true`, "+
		"a schema snapshot rather than a change to run, and this database already has migration "+
		"history. A database that applied it as an ordinary migration adopts it with "+
		"`pgroll rebaseline`; a database missing it entirely must catch up from a checkout that "+
		"still has the pre-truncation migrations, or be rebuilt", name)
}

// RefuseBaselineExecution errors if any migration about to be executed
// carries the baseline marker. Callers gate this on the database having
// migration history: on a completely fresh database (no history, empty
// schema) executing the baseline file is a legitimate bootstrap.
func RefuseBaselineExecution(migs []*migrations.RawMigration) error {
	for _, mig := range migs {
		if mig.Baseline {
			return BaselineExecutionRefusedError(mig.Name)
		}
	}
	return nil
}
