// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"fmt"

	"github.com/lib/pq"
)

// Prune deletes the named migrations from the history of schemaName and
// rewires the parent chain across the gaps so history stays linear. It is
// purely a state operation: no user-table DDL is executed and no version
// schemas are dropped — callers that need those side effects should use
// roll.Prune.
//
// The migrations table has three interlocking constraints that prevent
// in-place chain surgery: the parent FK, the unique history_is_linear index
// on (schema, parent), and the unique only_first_migration_without_parent
// index on (schema) WHERE parent IS NULL. The strategy (proven by long use
// in operational tooling) is to copy the keeper rows to an unconstrained
// temp table, rewire parents there with a window function, then swap the
// schema's rows wholesale inside one transaction. The single re-INSERT is
// safe against the non-deferrable FK because referential integrity checks
// are evaluated at end of statement.
//
// All names must exist for the schema or the call fails without modifying
// anything. Semantic validation (active migration, baseline rows) is the
// caller's responsibility.
func (s *State) Prune(ctx context.Context, schemaName string, names []string) error {
	if len(names) == 0 {
		return nil
	}

	tx, err := s.pgConn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("prune: beginning transaction: %w", err)
	}
	defer tx.Rollback()

	// Refuse to proceed unless every requested name is present, so a typo
	// can't silently no-op.
	var found int
	err = tx.QueryRowContext(ctx,
		fmt.Sprintf("SELECT count(*) FROM %s.migrations WHERE schema = $1 AND name = ANY($2)",
			pq.QuoteIdentifier(s.schema)),
		schemaName, pq.Array(names)).Scan(&found)
	if err != nil {
		return fmt.Errorf("prune: counting migrations: %w", err)
	}
	if found != len(names) {
		return fmt.Errorf("prune: %d of %d migrations not found in schema history for %q",
			len(names)-found, len(names), schemaName)
	}

	_, err = tx.ExecContext(ctx,
		fmt.Sprintf(`CREATE TEMP TABLE _pgroll_prune_keep ON COMMIT DROP AS
			SELECT * FROM %s.migrations
			WHERE schema = $1 AND NOT (name = ANY($2))`,
			pq.QuoteIdentifier(s.schema)),
		schemaName, pq.Array(names))
	if err != nil {
		return fmt.Errorf("prune: collecting keeper rows: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		`WITH ordered AS (
			SELECT name, LAG(name) OVER (ORDER BY created_at) AS correct_parent
			FROM _pgroll_prune_keep
		)
		UPDATE _pgroll_prune_keep t
		SET parent = o.correct_parent
		FROM ordered o
		WHERE t.name = o.name`)
	if err != nil {
		return fmt.Errorf("prune: rewiring parent chain: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		fmt.Sprintf("DELETE FROM %s.migrations WHERE schema = $1",
			pq.QuoteIdentifier(s.schema)),
		schemaName)
	if err != nil {
		return fmt.Errorf("prune: clearing schema history: %w", err)
	}

	_, err = tx.ExecContext(ctx,
		fmt.Sprintf("INSERT INTO %s.migrations SELECT * FROM _pgroll_prune_keep",
			pq.QuoteIdentifier(s.schema)))
	if err != nil {
		return fmt.Errorf("prune: reinserting keeper rows: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("prune: committing transaction: %w", err)
	}
	return nil
}
