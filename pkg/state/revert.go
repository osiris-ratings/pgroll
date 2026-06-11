// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/lib/pq"

	"github.com/xataio/pgroll/pkg/migrations"
)

// MigrationRecord is a migration row together with the state flags that
// determine how (and whether) it can be reverted.
type MigrationRecord struct {
	Name             string
	Migration        *migrations.Migration
	Done             bool
	CompleteDeferred bool
	MigrationType    string
	Parent           *string
}

// UnsealedMigrations returns the rows forming the revertible window — every
// migration not yet sealed — newest first. Under delayed contraction this is
// the most recent deployment: its rows are still physically in their expand
// phase (or, for inline-class operations, have only renamed pgroll-internal
// artifacts), so each can be rolled back losslessly.
func (s *State) UnsealedMigrations(ctx context.Context, schema string) ([]*MigrationRecord, error) {
	q := fmt.Sprintf(`
		SELECT name, migration, done, complete_deferred, migration_type, parent
		FROM %s.migrations
		WHERE schema = $1 AND sealed = FALSE
		ORDER BY created_at DESC
	`, pq.QuoteIdentifier(s.schema))

	rows, err := s.pgConn.QueryContext(ctx, q, schema)
	if err != nil {
		return nil, fmt.Errorf("unable to query unsealed migrations: %w", err)
	}
	defer rows.Close()

	var out []*MigrationRecord
	for rows.Next() {
		var (
			name, raw, migrationType string
			done, deferred           bool
			parent                   *string
		)
		if err := rows.Scan(&name, &raw, &done, &deferred, &migrationType, &parent); err != nil {
			return nil, fmt.Errorf("unable to scan unsealed migration row: %w", err)
		}
		var mig migrations.Migration
		if err := json.Unmarshal([]byte(raw), &mig); err != nil {
			return nil, fmt.Errorf("unable to unmarshal migration %q: %w", name, err)
		}
		mig.Name = name
		out = append(out, &MigrationRecord{
			Name:             name,
			Migration:        &mig,
			Done:             done,
			CompleteDeferred: deferred,
			MigrationType:    migrationType,
			Parent:           parent,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating unsealed migrations: %w", err)
	}
	return out, nil
}

// MarkSealed stamps every unsealed done migration as sealed and returns the
// number of rows stamped. Called at the *start* of a drain (seal at intent:
// the stamp must precede any contraction DDL, so that no crash window can
// leave a physically-contracted row looking revertible) and again after a
// non-deferred Complete (to stamp the just-completed row itself). Sealed
// rows are past the point of no return and must never be reverted.
func (s *State) MarkSealed(ctx context.Context, schema string) (int64, error) {
	res, err := s.pgConn.ExecContext(ctx, fmt.Sprintf(
		"UPDATE %s.migrations SET sealed=TRUE WHERE schema=$1 AND sealed=FALSE AND done=TRUE",
		pq.QuoteIdentifier(s.schema),
	), schema)
	if err != nil {
		return 0, fmt.Errorf("unable to mark migrations sealed: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("unable to count sealed migrations: %w", err)
	}
	return n, nil
}

// MarkSealedByName stamps the named migrations as sealed. Used to heal
// stranded rows (drained defer-class migrations left unsealed by a crash
// between a Complete's drain and its seal stamp on an older binary) without
// touching other unsealed rows, whose window may legitimately still be open.
func (s *State) MarkSealedByName(ctx context.Context, schema string, names []string) error {
	if len(names) == 0 {
		return nil
	}
	_, err := s.pgConn.ExecContext(ctx, fmt.Sprintf(
		"UPDATE %s.migrations SET sealed=TRUE WHERE schema=$1 AND name = ANY($2)",
		pq.QuoteIdentifier(s.schema),
	), schema, pq.Array(names))
	if err != nil {
		return fmt.Errorf("unable to mark migrations sealed by name: %w", err)
	}
	return nil
}

// DeleteMigration removes the named migration row regardless of its done
// state. Used by the revert walk, which always removes the current leaf —
// the parent foreign key guarantees a row with a child cannot be deleted,
// so history stays linear.
func (s *State) DeleteMigration(ctx context.Context, schema, name string) error {
	res, err := s.pgConn.ExecContext(ctx, fmt.Sprintf(
		"DELETE FROM %s.migrations WHERE schema=$1 AND name=$2",
		pq.QuoteIdentifier(s.schema),
	), schema, name)
	if err != nil {
		return err
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("no migration found with name %s", name)
	}

	return nil
}
