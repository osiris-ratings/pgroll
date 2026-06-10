// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// PruneTarget describes one migration row that Prune will remove from
// pgroll's history.
type PruneTarget struct {
	Name           string
	Done           bool
	MigrationType  string
	VersionSchema  string
	CreatedAt      time.Time
	OperationCount int
}

// PruneTargets resolves and validates the named migrations for pruning.
// It returns one PruneTarget per name, ordered by created_at. It fails if
// any migration is currently in progress (prune does not revert DDL — run
// `pgroll rollback` first), if any name is missing from the schema history,
// or if any name refers to a baseline migration.
func (m *Roll) PruneTargets(ctx context.Context, names []string) ([]PruneTarget, error) {
	if len(names) == 0 {
		return nil, nil
	}

	active, err := m.state.IsActiveMigrationPeriod(ctx, m.schema)
	if err != nil {
		return nil, fmt.Errorf("prune: checking active migration period: %w", err)
	}
	if active {
		return nil, fmt.Errorf(
			"prune: a migration is currently in progress on schema %q; complete it or run `pgroll rollback` first",
			m.schema,
		)
	}

	rows, err := m.state.PgConn().QueryContext(ctx,
		fmt.Sprintf(`SELECT name, done, migration_type, created_at,
			COALESCE(migration->>'version_schema', name),
			COALESCE(jsonb_array_length(migration->'operations'), 0)
		FROM %s.migrations
		WHERE schema = $1 AND name = ANY($2)
		ORDER BY created_at`, pq.QuoteIdentifier(m.state.Schema())),
		m.schema, pq.Array(names))
	if err != nil {
		return nil, fmt.Errorf("prune: reading migrations: %w", err)
	}
	defer rows.Close()

	targets := make([]PruneTarget, 0, len(names))
	byName := make(map[string]struct{}, len(names))
	for rows.Next() {
		var t PruneTarget
		if err := rows.Scan(&t.Name, &t.Done, &t.MigrationType, &t.CreatedAt,
			&t.VersionSchema, &t.OperationCount); err != nil {
			return nil, fmt.Errorf("prune: scanning migration row: %w", err)
		}
		targets = append(targets, t)
		byName[t.Name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("prune: iterating migration rows: %w", err)
	}

	for _, name := range names {
		if _, ok := byName[name]; !ok {
			return nil, fmt.Errorf("prune: migration %q not found in schema history for %q", name, m.schema)
		}
	}

	for _, t := range targets {
		if t.MigrationType == MigrationTypeBaseline {
			return nil, fmt.Errorf("prune: refusing to prune baseline migration %q", t.Name)
		}
		// Unreachable while the active-period check above holds; kept as a
		// guard against racing migrations started between the two queries.
		if !t.Done {
			return nil, fmt.Errorf("prune: migration %q is in progress; use `pgroll rollback`", t.Name)
		}
	}

	return targets, nil
}

// Prune removes the named migrations from pgroll's history and drops their
// version schemas (view layers). No user-table DDL is executed: the physical
// effects of completed migrations are NOT reverted. Its purpose is history
// reconciliation — e.g. removing rows recorded by a branch that was tested
// against a shared database and then abandoned, which otherwise block
// `pgroll migrate` with ErrMismatchedMigration.
//
// Version schemas are dropped before the history rows so that a failure
// between the two steps leaves the operation retryable: the drops are
// idempotent and the rows are still present for a re-run. After pruning,
// the new leaf's version schema may not exist (completing each migration
// reaps its predecessor's schema) — recreate it with `pgroll materialize`.
//
// Returns the pruned targets ordered by created_at.
func (m *Roll) Prune(ctx context.Context, names []string) ([]PruneTarget, error) {
	targets, err := m.PruneTargets(ctx, names)
	if err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, nil
	}

	if !m.disableVersionSchemas {
		for _, t := range targets {
			versionSchema := VersionedSchemaName(m.schema, t.VersionSchema)
			m.logger.Info("prune: dropping version schema", "schema", versionSchema)
			_, err := m.pgConn.ExecContext(ctx,
				fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", pq.QuoteIdentifier(versionSchema)))
			if err != nil {
				return nil, fmt.Errorf("prune: dropping version schema %q: %w", versionSchema, err)
			}
		}
	}

	pruneNames := make([]string, len(targets))
	for i, t := range targets {
		pruneNames[i] = t.Name
	}
	if err := m.state.Prune(ctx, m.schema, pruneNames); err != nil {
		return nil, err
	}

	m.logger.Info("pruned migrations from history", "schema", m.schema, "count", len(targets))
	return targets, nil
}
