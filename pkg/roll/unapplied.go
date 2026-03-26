// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"fmt"
	"io/fs"
	"sort"

	"github.com/xataio/pgroll/pkg/migrations"
)

// UnappliedMigrations returns the slice of unapplied migrations from `dir`
// that have not yet been applied to the database. Applying each of the
// returned migrations in order will bring the database up to date with `dir`.
//
// Migrations are matched by name rather than by position, allowing for
// divergent histories where migrations were applied in a different order
// than the local filesystem. If a migration exists in the database schema
// history but has no corresponding local file, an `ErrMismatchedMigration`
// error is returned.
func (m *Roll) UnappliedMigrations(ctx context.Context, dir fs.FS) ([]*migrations.RawMigration, error) {
	history, err := m.State().SchemaHistory(ctx, m.Schema())
	if err != nil {
		return nil, fmt.Errorf("reading schema history: %w", err)
	}

	baseline, err := m.State().LatestBaseline(ctx, m.Schema())
	if err != nil {
		return nil, fmt.Errorf("reading baseline: %w", err)
	}

	// Get all local migration files
	files, err := migrations.CollectFilesFromDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading migration files: %w", err)
	}

	baselineName := ""
	if baseline != nil {
		baselineName = baseline.Name
	}

	// Find the index of the first local migration after the baseline
	filesStartIdx := sort.Search(len(files), func(i int) bool {
		var migration *migrations.RawMigration
		migration, err = migrations.ReadRawMigration(dir, files[i])
		if err != nil {
			return false
		}
		return migration.Name > baselineName
	})
	if err != nil {
		return nil, fmt.Errorf("finding migration after baseline: %w", err)
	}

	// Read all migrations that come after the baseline
	migsAfterBaseline := make([]*migrations.RawMigration, 0, len(files))
	for _, file := range files[filesStartIdx:] {
		migration, err := migrations.ReadRawMigration(dir, file)
		if err != nil {
			return nil, fmt.Errorf("reading migration file %q: %w", file, err)
		}
		migsAfterBaseline = append(migsAfterBaseline, migration)
	}

	// Build a set of applied migration names from the database history
	applied := make(map[string]struct{}, len(history))
	for _, h := range history {
		applied[h.Migration.Name] = struct{}{}
	}

	// Validate: every applied migration must have a corresponding local file
	localNames := make(map[string]struct{}, len(migsAfterBaseline))
	for _, m := range migsAfterBaseline {
		localNames[m.Name] = struct{}{}
	}
	for name := range applied {
		if _, ok := localNames[name]; !ok {
			return nil, fmt.Errorf("%w: migration %q exists in schema history but not in local migration files",
				ErrMismatchedMigration, name)
		}
	}

	// Return local migrations not yet applied, preserving filesystem order
	unapplied := make([]*migrations.RawMigration, 0)
	for _, m := range migsAfterBaseline {
		if _, ok := applied[m.Name]; !ok {
			unapplied = append(unapplied, m)
		}
	}
	return unapplied, nil
}
