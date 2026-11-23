// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

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
	// FoldSetDeclared is true when the directory lists the migrations its
	// baseline folds in, so the database was (or would be) checked against
	// that set and not against the order it applied them in.
	FoldSetDeclared bool
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

// FoldsFileExt is the extension of the file that lists the migrations a
// baseline folds in. It sits beside the baseline migration and shares its
// name: `<baseline>.folds`.
const FoldsFileExt = ".folds"

// FoldedMigration is one entry of a baseline's fold set: a migration whose
// file a history truncation deleted, and the targets it was routed to.
type FoldedMigration struct {
	Name string
	// Targets is empty for a migration that declared none; such a migration
	// is required of every database.
	Targets []string
}

// ReadFoldSet reads the fold set declared beside the baseline migration, or
// returns nil when the directory declares none.
//
// The file is plain text, one migration per line: its name, then optionally
// its targets, comma-separated. Blank lines and lines starting with `#` are
// ignored. The set is cumulative: it lists everything folded since the
// oldest baseline a database may still be on, not only what the latest
// truncation deleted. It is its own file, not a field of the migration, because only
// rebaseline reads it and a migration file is parsed strictly by every
// command.
//
//	# Migrations folded into 20260829130000_validate_address_requests_address_key_fk
//	20260709224955_enrich_pep_entities app,etl
//	20260710120000_add_widgets app
func ReadFoldSet(dir fs.FS, baselineName string) ([]FoldedMigration, error) {
	file := baselineName + FoldsFileExt
	// A fold set named for anything else is a leftover or a typo. Ignoring
	// it would quietly select the audit for a directory that declares none.
	all, err := fs.Glob(dir, "*"+FoldsFileExt)
	if err != nil {
		return nil, fmt.Errorf("reading directory: %w", err)
	}
	for _, other := range all {
		if other != file {
			return nil, fmt.Errorf("%w: fold set %q does not belong to the baseline %q; "+
				"a directory holds one fold set, named %q",
				ErrRebaselineRefused, other, baselineName, file)
		}
	}
	f, err := dir.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("opening fold set %q: %w", file, err)
	}
	defer f.Close()

	// Non-nil even when the file lists nothing: a declared empty set is a
	// different statement from no declaration.
	folds := []FoldedMigration{}
	seen := map[string]struct{}{}
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if len(fields) > 2 {
			return nil, fmt.Errorf("%w: %s line %d: expected a migration name and an optional "+
				"comma-separated target list", ErrRebaselineRefused, file, line)
		}
		entry := FoldedMigration{Name: fields[0]}
		if entry.Name > baselineName {
			return nil, fmt.Errorf("%w: %s line %d: %q sorts after the baseline; a baseline "+
				"folds in only migrations at or before its own name",
				ErrRebaselineRefused, file, line, entry.Name)
		}
		if _, dup := seen[entry.Name]; dup {
			return nil, fmt.Errorf("%w: %s line %d: %q is listed twice",
				ErrRebaselineRefused, file, line, entry.Name)
		}
		seen[entry.Name] = struct{}{}
		if len(fields) == 2 {
			for _, target := range strings.Split(fields[1], ",") {
				if target == "" {
					return nil, fmt.Errorf("%w: %s line %d: empty target in %q",
						ErrRebaselineRefused, file, line, fields[1])
				}
				entry.Targets = append(entry.Targets, target)
			}
		}
		folds = append(folds, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading fold set %q: %w", file, err)
	}
	return folds, nil
}

// foldSetFor narrows a directory's fold set to what one database is held to.
// With no target every entry is required: that is single-database mode,
// where the database applies every migration.
func foldSetFor(folds []FoldedMigration, target string) *state.FoldSet {
	if folds == nil {
		return nil
	}
	set := &state.FoldSet{
		All:      make([]string, 0, len(folds)),
		Required: make([]string, 0, len(folds)),
	}
	for _, f := range folds {
		set.All = append(set.All, f.Name)
		if target == "" || len(f.Targets) == 0 || slices.Contains(f.Targets, target) {
			set.Required = append(set.Required, f.Name)
		}
	}
	return set
}

// Rebaseline reconciles the database's baseline with the directory's marked
// baseline. It is the deploy-time companion of migration-history truncation:
// after old migration files are deleted and the anchor file is rewritten as a
// marked baseline, every database that applied the anchor as an ordinary
// migration adopts the truncated directory by having that row converted to a
// baseline in place (state.ConvertMigrationToBaseline has the full safety
// contract).
//
// When the directory declares a fold set (ReadFoldSet), the database is
// checked against it: it must have applied every folded migration its target
// selects, and nothing at or below the anchor that the set does not name.
// The order it applied them in does not matter. A directory without one is
// adopted only by a database that applied its migrations in name order.
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

	folds, err := ReadFoldSet(dir, marked.Name)
	if err != nil {
		return nil, err
	}

	result := &RebaselineResult{BaselineName: marked.Name, FoldSetDeclared: folds != nil}

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

	convRes, err := m.state.ConvertMigrationToBaseline(ctx, m.schema, marked.Name, foldSetFor(folds, m.target), checkOnly)
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
