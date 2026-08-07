// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/xataio/pgroll/pkg/migrations"
)

// CheckIssue represents a single issue found during a check.
type CheckIssue struct {
	File    string
	Message string
}

func (i CheckIssue) String() string {
	if i.File != "" {
		return fmt.Sprintf("%s: %s", i.File, i.Message)
	}
	return i.Message
}

// CheckResult holds the results of a migration directory check.
type CheckResult struct {
	Errors   []CheckIssue
	Warnings []CheckIssue
}

// HasErrors returns true if any hard errors were found.
func (r *CheckResult) HasErrors() bool {
	return len(r.Errors) > 0
}

func (r *CheckResult) addError(file, msg string) {
	r.Errors = append(r.Errors, CheckIssue{File: file, Message: msg})
}

func (r *CheckResult) addWarning(file, msg string) {
	r.Warnings = append(r.Warnings, CheckIssue{File: file, Message: msg})
}

// CheckMigrationsDir runs filesystem-only checks on a migration directory.
// No database connection is required. Checks include:
//   - YAML/JSON syntax validation
//   - Required operations field (non-empty)
//   - Schema name length (public_<name> ≤ 63 chars)
//   - depends_on targets exist in the migration set
//   - Dependency cycle detection
//   - Advisory: raw SQL operations without preconditions
//   - Reversibility: ops that need a 'down' expression have one, unless the
//     migration is marked 'irreversible: true'
//
// checkTargets validates the shape of a migration's `targets` field. The
// values themselves are never validated — pgroll has no vocabulary of its own
// for target names — but a malformed field is always an authoring error.
func checkTargets(result *CheckResult, file string, raw *migrations.RawMigration) {
	if raw.Targets == nil {
		return
	}
	if len(raw.Targets) == 0 {
		result.addError(file, "declares an empty 'targets' list; omit the field or name at least one target")
		return
	}

	seen := make(map[string]struct{}, len(raw.Targets))
	for _, t := range raw.Targets {
		if strings.TrimSpace(t) == "" {
			result.addError(file, "'targets' contains an empty entry")
			continue
		}
		if _, dup := seen[t]; dup {
			result.addError(file, fmt.Sprintf("'targets' lists %q more than once", t))
		}
		seen[t] = struct{}{}
	}
}

func CheckMigrationsDir(dir fs.FS) (*CheckResult, error) {
	result := &CheckResult{}

	files, err := migrations.CollectFilesFromDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading migration files: %w", err)
	}

	if len(files) == 0 {
		return result, nil
	}

	// Parse all migrations, collecting errors for unparseable ones
	type parsedMig struct {
		file string
		raw  *migrations.RawMigration
	}
	var parsed []parsedMig
	allNames := make(map[string]struct{}, len(files))

	for _, file := range files {
		raw, err := migrations.ReadRawMigration(dir, file)
		if err != nil {
			result.addError(file, fmt.Sprintf("invalid syntax: %v", err))
			continue
		}

		// Check operations field is non-empty
		if len(raw.Operations) == 0 || string(raw.Operations) == "null" || string(raw.Operations) == "[]" {
			result.addError(file, "missing or empty 'operations' field")
			continue
		}

		// Check schema name length
		schemaName := "public_" + raw.Name
		if len(schemaName) > 63 {
			result.addError(file, fmt.Sprintf(
				"name too long: schema %q would be %d chars (max 63)",
				schemaName, len(schemaName),
			))
		}

		checkTargets(result, file, raw)

		allNames[raw.Name] = struct{}{}
		parsed = append(parsed, parsedMig{file: file, raw: raw})
	}

	// Build file lookup for reporting
	fileByName := make(map[string]string, len(parsed))
	for _, p := range parsed {
		fileByName[p.raw.Name] = p.file
	}

	// Check depends_on targets exist
	for _, p := range parsed {
		for _, dep := range p.raw.DependsOn {
			if _, ok := allNames[dep]; !ok {
				result.addError(p.file, fmt.Sprintf(
					"depends_on target %q not found in migration set", dep,
				))
			}
		}
	}

	rawByName := make(map[string]*migrations.RawMigration, len(parsed))
	for _, p := range parsed {
		rawByName[p.raw.Name] = p.raw
	}

	// Advisory: a dependency that does not cover every target of the migration
	// depending on it will be skipped on at least one database, where pgroll
	// treats it as satisfied by construction. That is correct when depends_on
	// expresses ordering — its documented purpose — and a latent runtime
	// failure when it was meant as a semantic prerequisite. pgroll cannot tell
	// the two apart, so warn and let the author confirm.
	for _, p := range parsed {
		if len(p.raw.Targets) == 0 {
			continue
		}
		for _, dep := range p.raw.DependsOn {
			depRaw, ok := rawByName[dep]
			if !ok || len(depRaw.Targets) == 0 {
				// Missing deps already errored above; an untagged dependency
				// applies everywhere and so covers any target.
				continue
			}
			uncovered := make([]string, 0, len(p.raw.Targets))
			for _, t := range p.raw.Targets {
				if !slices.Contains(depRaw.Targets, t) {
					uncovered = append(uncovered, t)
				}
			}
			if len(uncovered) > 0 {
				result.addWarning(p.file, fmt.Sprintf(
					"depends_on %q does not declare target(s) %v, so the dependency is skipped there; "+
						"fine if it only expresses ordering, a latent failure if it is a prerequisite",
					dep, uncovered,
				))
			}
		}
	}

	// Advisory: target names that differ only by case or surrounding
	// whitespace. pgroll compares them verbatim and has no vocabulary of its
	// own, so "ETL" and "etl" are two different targets and one of them
	// selects nothing. Which names are legal stays with the caller's linter;
	// this only flags the directory being inconsistent with itself.
	variants := make(map[string]map[string]struct{})
	for _, p := range parsed {
		for _, t := range p.raw.Targets {
			key := strings.ToLower(strings.TrimSpace(t))
			if variants[key] == nil {
				variants[key] = map[string]struct{}{}
			}
			variants[key][t] = struct{}{}
		}
	}
	for key, spellings := range variants {
		if len(spellings) < 2 {
			continue
		}
		result.addWarning("", fmt.Sprintf(
			"target %q is spelled %d different ways across the directory (%v); "+
				"pgroll matches target names verbatim, so these are distinct targets",
			key, len(spellings), slices.Sorted(maps.Keys(spellings)),
		))
	}

	// Check for dependency cycles using topological sort
	rawMigs := make([]*migrations.RawMigration, len(parsed))
	for i, p := range parsed {
		rawMigs[i] = p.raw
	}
	// nil excluded set: check validates the directory as a whole corpus and
	// never filters by target, so no migration is excluded here.
	_, err = TopoSortMigrations(rawMigs, map[string]struct{}{}, nil)
	if err != nil {
		result.addError("", fmt.Sprintf("dependency graph: %v", err))
	}

	// Advisory: raw SQL operations without preconditions
	for _, p := range parsed {
		if len(p.raw.Preconditions) > 0 {
			continue
		}
		if hasRawSQLOps(p.raw.Operations) {
			result.addWarning(p.file,
				"contains raw SQL operations without preconditions")
		}
	}

	// Reversibility by construction: every migration must be revertible or
	// explicitly marked `irreversible: true`
	for _, p := range parsed {
		mig, err := migrations.ParseMigration(p.raw)
		if err != nil {
			result.addError(p.file, fmt.Sprintf("invalid operations: %v", err))
			continue
		}
		if err := mig.ValidateReversibility(); err != nil {
			result.addError(p.file, err.Error())
		}
	}

	return result, nil
}

// hasRawSQLOps checks if the operations JSON contains any raw SQL operations.
func hasRawSQLOps(ops json.RawMessage) bool {
	var opList []map[string]json.RawMessage
	if err := json.Unmarshal(ops, &opList); err != nil {
		return false
	}
	for _, op := range opList {
		if _, ok := op["sql"]; ok {
			return true
		}
	}
	return false
}

// CheckBaseOrdering checks that new migration files in the current HEAD
// sort lexicographically after the latest migration on the base branch.
// This requires git to be available on PATH.
func CheckBaseOrdering(migrationsDir string, baseRef string) (*CheckResult, error) {
	result := &CheckResult{}

	// Find the merge base
	mergeBaseOut, err := exec.Command("git", "merge-base", "HEAD", baseRef).Output()
	if err != nil {
		return nil, fmt.Errorf("git merge-base failed: %w", err)
	}
	mergeBase := strings.TrimSpace(string(mergeBaseOut))

	// Get migration filenames on the base branch
	lsTreeOut, err := exec.Command("git", "ls-tree", "--name-only",
		mergeBase, "--", migrationsDir+"/").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-tree failed: %w", err)
	}

	baseFiles := strings.Split(strings.TrimSpace(string(lsTreeOut)), "\n")
	// Filter to migration files only
	var baseMigrations []string
	for _, f := range baseFiles {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f))
		if ext == ".yaml" || ext == ".yml" || ext == ".json" {
			baseMigrations = append(baseMigrations, filepath.Base(f))
		}
	}

	if len(baseMigrations) == 0 {
		// No migrations on base branch — nothing to check
		return result, nil
	}

	sort.Strings(baseMigrations)
	lastBase := baseMigrations[len(baseMigrations)-1]

	// Find new migration files added in this branch.
	// #nosec G204 -- exec.Command does not invoke a shell; baseRef and
	// migrationsDir are passed as literal argv args to git, not interpolated
	// into a shell command.
	diffOut, err := exec.Command("git", "diff", "--name-only", "--diff-filter=A",
		mergeBase+"..HEAD", "--",
		migrationsDir+"/*.yaml", migrationsDir+"/*.yml", migrationsDir+"/*.json").Output()
	if err != nil {
		return nil, fmt.Errorf("git diff failed: %w", err)
	}

	newFiles := strings.Split(strings.TrimSpace(string(diffOut)), "\n")
	for _, f := range newFiles {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		basename := filepath.Base(f)
		if basename <= lastBase {
			result.addError(basename, fmt.Sprintf(
				"sorts before or equal to base branch's latest migration %q — run pgroll rebase",
				lastBase,
			))
		}
	}

	return result, nil
}
