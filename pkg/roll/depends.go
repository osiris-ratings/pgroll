// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"fmt"
	"strings"

	"github.com/xataio/pgroll/pkg/migrations"
)

var (
	ErrDependencyNotApplied = fmt.Errorf("migration dependency not satisfied")
	ErrDependencyCycle      = fmt.Errorf("dependency cycle detected")
)

// validateDependencies checks that all migrations listed in the migration's
// depends_on field have already been applied to the database.
func (m *Roll) validateDependencies(ctx context.Context, migration *migrations.Migration) error {
	if len(migration.DependsOn) == 0 {
		return nil
	}

	history, err := m.State().SchemaHistory(ctx, m.Schema())
	if err != nil {
		return fmt.Errorf("reading schema history for dependency check: %w", err)
	}

	applied := make(map[string]struct{}, len(history))
	for _, h := range history {
		applied[h.Migration.Name] = struct{}{}
	}

	var missing []string
	for _, dep := range migration.DependsOn {
		if _, ok := applied[dep]; !ok {
			missing = append(missing, dep)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("%w: migration %q depends on unapplied migrations: %v",
			ErrDependencyNotApplied, migration.Name, missing)
	}
	return nil
}

// TopoSortMigrations sorts migrations respecting depends_on constraints,
// using filesystem position as a tiebreaker for migrations with no dependency
// relationship. Migrations without depends_on preserve their original order.
//
// The applied set contains names of migrations already applied to the database;
// dependencies satisfied by applied migrations are considered met.
func TopoSortMigrations(migs []*migrations.RawMigration, applied map[string]struct{}) ([]*migrations.RawMigration, error) {
	// Check if any migration has depends_on; if none do, skip sorting entirely
	hasDeps := false
	for _, m := range migs {
		if len(m.DependsOn) > 0 {
			hasDeps = true
			break
		}
	}
	if !hasDeps {
		return migs, nil
	}

	// Build index of unapplied migrations by name
	migByName := make(map[string]*migrations.RawMigration, len(migs))
	posByName := make(map[string]int, len(migs))
	for i, m := range migs {
		migByName[m.Name] = m
		posByName[m.Name] = i
	}

	// Build in-degree counts and adjacency list (only for unapplied deps)
	inDegree := make(map[string]int, len(migs))
	dependents := make(map[string][]string) // dep -> list of migrations that depend on it
	for _, m := range migs {
		inDegree[m.Name] = 0
	}
	for _, m := range migs {
		for _, dep := range m.DependsOn {
			// Only count dependencies on other unapplied migrations
			if _, isUnapplied := migByName[dep]; isUnapplied {
				inDegree[m.Name]++
				dependents[dep] = append(dependents[dep], m.Name)
			} else if _, isApplied := applied[dep]; !isApplied {
				// Dependency is neither applied nor in the unapplied set
				return nil, fmt.Errorf("%w: migration %q depends on unknown migration %q",
					ErrMismatchedMigration, m.Name, dep)
			}
			// If dep is in applied set, it's already satisfied — skip
		}
	}

	// Kahn's algorithm with filesystem-position tiebreaker.
	// We use a simple approach: repeatedly scan for the migration with
	// in-degree 0 and the lowest filesystem position.
	result := make([]*migrations.RawMigration, 0, len(migs))
	for len(result) < len(migs) {
		// Find the best candidate: in-degree 0, lowest filesystem position
		bestName := ""
		bestPos := len(migs) // sentinel
		for name, deg := range inDegree {
			if deg == 0 {
				pos := posByName[name]
				if pos < bestPos {
					bestPos = pos
					bestName = name
				}
			}
		}

		if bestName == "" {
			// All remaining migrations have in-degree > 0 — cycle exists
			var cycleNames []string
			for name, deg := range inDegree {
				if deg > 0 {
					cycleNames = append(cycleNames, name)
				}
			}
			return nil, fmt.Errorf("%w: migrations involved: [%s]",
				ErrDependencyCycle, strings.Join(cycleNames, ", "))
		}

		result = append(result, migByName[bestName])
		delete(inDegree, bestName)

		// Reduce in-degree for dependents
		for _, dependent := range dependents[bestName] {
			if _, exists := inDegree[dependent]; exists {
				inDegree[dependent]--
			}
		}
	}

	return result, nil
}
