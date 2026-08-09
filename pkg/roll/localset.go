// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"errors"
	"fmt"
	"io/fs"

	"github.com/xataio/pgroll/pkg/migrations"
)

// ErrTargetRequired is returned when a migration that a --target run would have
// to consider carries no `targets` field. There is no default target and no
// fail-open: a migration silently withheld from a database it should have
// reached does not announce itself, and shows up later as replication quietly
// dropping rows.
//
// Wrapped rather than formatted so callers — and tests — can branch on it with
// errors.Is instead of matching on message text.
var ErrTargetRequired = errors.New("migration declares no targets")

// ErrWrongTarget is returned when a migration is applied directly — by name,
// bypassing directory resolution — to a target its `targets` exclude.
var ErrWrongTarget = errors.New("migration is not routed to this target")

// localSet is the resolved view of a migrations directory for one run: every
// post-baseline file, plus the subset the active target selects.
//
// Both slices are in filename (lexicographic) order and Selected is a
// subsequence of All. Keeping them apart is the whole point — see
// resolveLocalSet for why the two are used in different places and must never
// be swapped.
type localSet struct {
	// All is every post-baseline migration in the directory, regardless of
	// target. This is the set database history is validated against.
	All []*migrations.RawMigration

	// Selected is the subset the active target selects, and the only set that
	// is ever applied. Identical to All when no target is in effect.
	Selected []*migrations.RawMigration

	// Excluded holds the names in All that are not in Selected. A depends_on
	// pointing at one of these is satisfied by construction on this database:
	// the dependency will never be applied here, so there is nothing to order
	// against. Empty when no target is in effect.
	Excluded map[string]struct{}
}

// resolveLocalSet reads dir, drops everything at or before baselineName, and
// splits the remainder into the full set and the target-selected subset.
//
// This is the only place in pgroll where the target filter lives. Everything
// that turns a directory into migrations goes through it, so the rule below
// is stated once rather than remembered at each of the call sites that read a
// migrations directory.
//
// # No target means no filtering
//
// With target == "" nothing is filtered: Selected aliases All and Excluded is
// empty. That is single-database mode — dev worktrees, CI, per-developer
// instances, rebuild-from-migrations — and it is why targets can be adopted
// without touching any of those paths. The number of --target flags in play
// equals the number of databases you have.
//
// # Why the caller must validate against All, not Selected
//
// History validation asks "does every applied migration still have a local
// file?". It must see the unfiltered directory. A database can legitimately
// hold history for migrations this target does not select — an ETL host
// cloned from the application database inherits the application's entire
// pgroll chain — and validating against the filtered set would report every
// one of those rows as missing and refuse to run at all. Leaving validation
// unfiltered is precisely what lets such a host keep its inherited history and
// simply stop receiving migrations it is not a target of, with no re-baseline
// and no cutover.
//
// # Why the tag requirement is scoped to candidates
//
// With a target set, every *selection candidate* — a post-baseline migration
// absent from applied — must declare a non-empty `targets`. An untagged
// candidate is a hard error naming its file: there is no default target and no
// fail-open, because a migration silently withheld from a database it should
// have reached shows up later as replication quietly dropping rows.
//
// Already-applied migrations are never inspected for tags. That is what lets a
// cloned database adopt targets without back-stamping the history it inherited.
// The corollary is worth stating plainly: on a database with *empty* history
// every post-baseline file is a candidate, so bootstrapping a fresh database
// with --target requires the whole directory to be tagged (or a baseline set
// first). That is a supported topology, not an accident.
func resolveLocalSet(
	dir fs.FS,
	baselineName string,
	applied map[string]struct{},
	target string,
) (*localSet, error) {
	files, err := migrations.CollectFilesFromDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading migration files: %w", err)
	}

	set := &localSet{
		All:      make([]*migrations.RawMigration, 0, len(files)),
		Excluded: map[string]struct{}{},
	}

	for _, file := range files {
		// Compare on the filename rather than the parsed body so pre-baseline
		// files are never opened. A corrupt migration below the baseline is
		// then not this run's problem, which matches the intent of a baseline.
		if migrations.MigrationNameFromFile(file) <= baselineName {
			continue
		}

		mig, err := migrations.ReadRawMigration(dir, file)
		if err != nil {
			return nil, fmt.Errorf("reading migration file %q: %w", file, err)
		}
		set.All = append(set.All, mig)

		if target == "" {
			continue
		}

		if _, isApplied := applied[mig.Name]; !isApplied && len(mig.Targets) == 0 {
			return nil, TargetRequiredError(file, target, mig.Targets != nil)
		}

		if !mig.SelectedBy(target) {
			set.Excluded[mig.Name] = struct{}{}
		}
	}

	if target == "" {
		set.Selected = set.All
		return set, nil
	}

	set.Selected = make([]*migrations.RawMigration, 0, len(set.All))
	for _, mig := range set.All {
		if _, excluded := set.Excluded[mig.Name]; !excluded {
			set.Selected = append(set.Selected, mig)
		}
	}

	return set, nil
}

// TargetRequiredError explains that a migration carries no routing while a
// target is in effect. It names the file rather than the migration because the
// fix is an edit to that file, and wraps ErrTargetRequired so callers and tests
// can branch with errors.Is rather than matching message text.
//
// Exported so every site that has to apply the same rule — resolveLocalSet,
// the local `latest` resolution, `check --require-targets` — produces one
// error rather than three lookalikes.
func TargetRequiredError(file, target string, declaredEmpty bool) error {
	if declaredEmpty {
		return fmt.Errorf("%w: migration file %q declares an empty `targets` list; "+
			"omit the field or name at least one target", ErrTargetRequired, file)
	}
	return fmt.Errorf("%w: migration file %q must declare `targets` (--target %q is in effect); "+
		"name the target(s) this migration belongs to, for example `targets: [%s]`. "+
		"There is no default target", ErrTargetRequired, file, target, target)
}
