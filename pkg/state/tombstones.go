// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"fmt"

	"github.com/lib/pq"
)

// Tombstone records a migration removed from history by a sealed revert,
// keyed by name with a hash of its content at revert time.
type Tombstone struct {
	Name        string
	ContentHash string
}

// RecordRevertedMigrations upserts tombstones for migrations a sealed revert
// is about to prune from history. Written BEFORE the prune: a crash in
// between leaves a tombstone for a still-applied migration, which is
// harmless (an applied migration is not unapplied, so the re-application
// check never consults it) and the resumed prune completes the pair.
func (s *State) RecordRevertedMigrations(ctx context.Context, schema string, tombstones []Tombstone) error {
	for _, t := range tombstones {
		_, err := s.pgConn.ExecContext(ctx, fmt.Sprintf(`
			INSERT INTO %s.reverted_migrations (schema, name, content_hash)
			VALUES ($1, $2, $3)
			ON CONFLICT (schema, name)
			DO UPDATE SET content_hash = EXCLUDED.content_hash, reverted_at = CURRENT_TIMESTAMP
		`, pq.QuoteIdentifier(s.schema)), schema, t.Name, t.ContentHash)
		if err != nil {
			return fmt.Errorf("unable to record reverted migration %q: %w", t.Name, err)
		}
	}
	return nil
}

// RevertedMigrations returns the tombstones for the given schema as a
// name → content-hash map.
func (s *State) RevertedMigrations(ctx context.Context, schema string) (map[string]string, error) {
	rows, err := s.pgConn.QueryContext(ctx, fmt.Sprintf(
		"SELECT name, content_hash FROM %s.reverted_migrations WHERE schema = $1",
		pq.QuoteIdentifier(s.schema),
	), schema)
	if err != nil {
		return nil, fmt.Errorf("unable to query reverted migrations: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var name, hash string
		if err := rows.Scan(&name, &hash); err != nil {
			return nil, fmt.Errorf("unable to scan reverted migration row: %w", err)
		}
		out[name] = hash
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating reverted migrations: %w", err)
	}
	return out, nil
}

// ClearRevertedMigrations removes tombstones by name. Called after the named
// migrations successfully (re-)apply with changed content — the engineer has
// confirmed intent, so the tombstone has served its purpose.
func (s *State) ClearRevertedMigrations(ctx context.Context, schema string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	_, err := s.pgConn.ExecContext(ctx, fmt.Sprintf(
		"DELETE FROM %s.reverted_migrations WHERE schema = $1 AND name = ANY($2)",
		pq.QuoteIdentifier(s.schema),
	), schema, pq.Array(names))
	if err != nil {
		return fmt.Errorf("unable to clear reverted migrations: %w", err)
	}
	return nil
}
