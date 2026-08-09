// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"fmt"
	"io/fs"

	"github.com/xataio/pgroll/pkg/migrations"
)

var (
	ErrNoMigrationFiles   = fmt.Errorf("no migration files found")
	ErrNoMigrationApplied = fmt.Errorf("no migrations applied")
	ErrNoVersionSchema    = fmt.Errorf("no version schemas found")
)

// LatestVersionLocal returns the version schema name of the last migration in
// `dir` selected by `target`, where the migration files are lexicographically
// ordered by filename. An empty target selects every migration.
func LatestVersionLocal(ctx context.Context, dir fs.FS, target string) (string, error) {
	migration, err := latestMigrationLocal(dir, target)
	if err != nil {
		return "", fmt.Errorf("getting latest local migration: %w", err)
	}

	return migration.VersionSchemaName(), nil
}

// LatestMigrationNameLocal returns the name of the last migration in `dir`
// selected by `target`, where the migration files are lexicographically
// ordered by filename. An empty target selects every migration.
func LatestMigrationNameLocal(ctx context.Context, dir fs.FS, target string) (string, error) {
	migration, err := latestMigrationLocal(dir, target)
	if err != nil {
		return "", fmt.Errorf("getting latest local migration: %w", err)
	}

	return migration.Name, nil
}

// LatestVersionRemote returns the version schema name of the last migration to
// have been applied to the target schema.
func (m *Roll) LatestVersionRemote(ctx context.Context) (string, error) {
	latestVersion, err := m.State().LatestVersion(ctx, m.Schema())
	if err != nil {
		return "", fmt.Errorf("failed to get latest version: %w", err)
	}

	if latestVersion == nil {
		return "", ErrNoVersionSchema
	}

	return *latestVersion, nil
}

// LatestMigrationNameRemote returns the name of the last migration to have been
// applied to the target schema.
func (m *Roll) LatestMigrationNameRemote(ctx context.Context) (string, error) {
	latestName, err := m.State().LatestMigration(ctx, m.Schema())
	if err != nil {
		return "", fmt.Errorf("failed to get latest migration name: %w", err)
	}

	if latestName == nil {
		return "", ErrNoMigrationApplied
	}

	return *latestName, nil
}

// latestMigrationLocal returns the latest migration from the local migration
// directory selected by `target`, where the migration files are
// lexicographically ordered by filename.
//
// The target filter matters more here than it looks: this feeds
// `pgroll latest schema`, which deploys use to compute the version schema to
// repin an application fleet to. Unfiltered, a directory whose newest file
// belongs to another target would have the fleet repinned to a version schema
// this database never created.
func latestMigrationLocal(dir fs.FS, target string) (*migrations.Migration, error) {
	files, err := migrations.CollectFilesFromDir(dir)
	if err != nil {
		return nil, fmt.Errorf("getting migration files from dir: %w", err)
	}

	for i := len(files) - 1; i >= 0; i-- {
		raw, err := migrations.ReadRawMigration(dir, files[i])
		if err != nil {
			return nil, fmt.Errorf("reading migration file %q: %w", files[i], err)
		}

		// Same rule as resolveLocalSet, deliberately. An undeclared migration
		// under a target must not be silently skipped here while it is a hard
		// error there: that would make "untagged" mean two different things
		// depending on which command you ran, and this path would answer
		// "no migrations" for a directory full of them.
		if target != "" && len(raw.Targets) == 0 {
			return nil, TargetRequiredError(files[i], target, raw.Targets != nil)
		}
		if !raw.SelectedBy(target) {
			continue
		}

		migration, err := migrations.ParseMigration(raw)
		if err != nil {
			return nil, fmt.Errorf("reading migration file %q: %w", files[i], err)
		}
		return migration, nil
	}

	return nil, ErrNoMigrationFiles
}
