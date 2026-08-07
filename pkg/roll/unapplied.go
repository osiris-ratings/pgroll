// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/xataio/pgroll/pkg/migrations"
)

// Resolution is the outcome of matching a migrations directory against this
// database's history for one run.
type Resolution struct {
	// Apply are the migrations to apply, in apply order (depends_on
	// topological order, filename order as the tiebreaker).
	Apply []*migrations.RawMigration

	// Excluded are local migration names the active target does not select.
	// They are never applied to this database, so depends_on references to
	// them are satisfied by construction; pass this to
	// WithSatisfiedDependencies when starting each migration.
	Excluded map[string]struct{}

	// All is every post-baseline migration in the directory, in filename
	// order, regardless of target or applied state. Callers that need to
	// position a migration within the local sequence — bounding a run with
	// --to, say — must use this rather than Apply: the migration they are
	// naming may be one this run will not apply, and it still bounds the run.
	All []*migrations.RawMigration
}

// ResolveMigrations matches the migrations in `dir` against this database's
// schema history and returns the work to do.
//
// Migrations are matched by name rather than by position, allowing for
// divergent histories where migrations were applied in a different order than
// the local filesystem. If a migration exists in the database schema history
// but has no corresponding local file, an `ErrMismatchedMigration` error is
// returned.
//
// When a --target is in effect only migrations declaring that target are
// applied, but validation still reads the whole directory. See resolveLocalSet
// for why that asymmetry is the load-bearing part.
func (m *Roll) ResolveMigrations(ctx context.Context, dir fs.FS) (*Resolution, error) {
	history, err := m.State().SchemaHistory(ctx, m.Schema())
	if err != nil {
		return nil, fmt.Errorf("reading schema history: %w", err)
	}

	baseline, err := m.State().LatestBaseline(ctx, m.Schema())
	if err != nil {
		return nil, fmt.Errorf("reading baseline: %w", err)
	}
	baselineName := ""
	if baseline != nil {
		baselineName = baseline.Name
	}

	// The applied set is needed before the directory is resolved: the tag
	// requirement is scoped to selection candidates, and a candidate is
	// defined as post-baseline AND not yet applied.
	applied := make(map[string]struct{}, len(history))
	for _, h := range history {
		applied[h.Migration.Name] = struct{}{}
	}

	local, err := resolveLocalSet(dir, baselineName, applied, m.target)
	if err != nil {
		return nil, err
	}

	// Validate: every applied migration must have a corresponding local file.
	//
	// This reads local.All, never local.Selected. A database may hold history
	// for migrations the active target does not select — an ETL host cloned
	// from the application database carries the application's whole chain —
	// and checking against the filtered set would flag every one of those rows
	// as missing, so `pgroll migrate --target ...` would refuse to run before
	// doing anything. Nothing target-aware may move above this loop.
	localNames := make(map[string]struct{}, len(local.All))
	for _, mig := range local.All {
		localNames[mig.Name] = struct{}{}
	}
	for name := range applied {
		if _, ok := localNames[name]; !ok {
			return nil, fmt.Errorf("%w: migration %q exists in schema history but not in local migration files",
				ErrMismatchedMigration, name)
		}
	}

	// Select: local migrations not yet applied, preserving filesystem order.
	unapplied := make([]*migrations.RawMigration, 0, len(local.Selected))
	for _, mig := range local.Selected {
		if _, ok := applied[mig.Name]; !ok {
			unapplied = append(unapplied, mig)
		}
	}

	// Sort respecting depends_on constraints, with filesystem order as tiebreaker
	sorted, err := TopoSortMigrations(unapplied, applied, local.Excluded)
	if err != nil {
		return nil, err
	}

	return &Resolution{Apply: sorted, Excluded: local.Excluded, All: local.All}, nil
}

// UnappliedMigrations returns the slice of unapplied migrations from `dir`
// that have not yet been applied to the database. Applying each of the
// returned migrations in order will bring the database up to date with `dir`.
//
// Migrations are matched by name rather than by position, allowing for
// divergent histories where migrations were applied in a different order
// than the local filesystem. If a migration exists in the database schema
// history but has no corresponding local file, an `ErrMismatchedMigration`
// error is returned.
//
// When a --target is in effect the result is restricted to migrations
// declaring that target. Callers that start the returned migrations should
// use ResolveMigrations instead, so cross-target dependencies can be declared
// satisfied.
func (m *Roll) UnappliedMigrations(ctx context.Context, dir fs.FS) ([]*migrations.RawMigration, error) {
	res, err := m.ResolveMigrations(ctx, dir)
	if err != nil {
		return nil, err
	}
	return res.Apply, nil
}
