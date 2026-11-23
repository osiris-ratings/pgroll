// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/schema"
)

// HistoryEntry represents a single migration in the migration history
// of a schema
type HistoryEntry struct {
	Migration migrations.RawMigration
	CreatedAt time.Time
}

// BaselineMigration represents a baseline migration record
type BaselineMigration struct {
	Name           string
	CreatedAt      time.Time
	SchemaSnapshot schema.Schema
}

// SchemaHistory returns the migrations applied to a schema that the latest
// baseline does not fold in, in ascending timestamp order, excluding inferred
// rows.
//
// A baseline folds in a set of migrations: every row whose name sorts at or
// before the baseline's. That is the same rule resolveLocalSet applies to
// files, so a row is hidden exactly when its file is ignored, whatever order
// the database applied them in. Two databases that applied one directory in
// different orders therefore agree on what a baseline hides.
//
// Inferred rows are DDL captured out of band by a pgroll old enough to still
// install the capture event triggers. They never correspond to a file in the
// migrations directory, and every caller of this method is ultimately
// comparing history against that directory — so left visible, a single stray
// row wedges `migrate`, `plan` and `pull` on a database that is otherwise
// perfectly in sync. Filtering here makes such rows inert on a binary upgrade
// alone, without deleting anything; `pgroll prune` still removes them for
// real. They remain members of the parent chain, so the chain stays linear.
func (s *State) SchemaHistory(ctx context.Context, schema string) ([]HistoryEntry, error) {
	return s.schemaHistory(ctx, schema, true)
}

// SchemaHistoryWithInferred is [State.SchemaHistory] including inferred rows.
//
// Revert planning uses it deliberately: DDL captured out of band is a real
// schema change, and inverting the migrations around it as though it were not
// there would land a schema that does not match the boundary snapshot. The row
// has to be visible in order to be refused.
func (s *State) SchemaHistoryWithInferred(ctx context.Context, schema string) ([]HistoryEntry, error) {
	return s.schemaHistory(ctx, schema, false)
}

func (s *State) schemaHistory(ctx context.Context, schema string, excludeInferred bool) ([]HistoryEntry, error) {
	inferredFilter := ""
	if excludeInferred {
		inferredFilter = "AND m.migration_type <> 'inferred'"
	}

	// An inferred row has no file and so no place in name order. It is hidden
	// when it was captured no later than the newest folded row: the floor,
	// beneath which nothing can be reverted (see BaselineFloor).
	//
	// An inverse row (`revert_of`) is synthesized by a sealed revert and
	// named `revert_<migration>`, which can sort below a baseline. It is
	// never folded: the revert that created it has to find it to finish.
	rows, err := s.pgConn.QueryContext(ctx,
		fmt.Sprintf(`WITH b AS (
				SELECT name FROM %[1]s.migrations
				WHERE schema = $1 AND migration_type = 'baseline'
				ORDER BY name COLLATE "C" DESC LIMIT 1
			), floor AS (
				SELECT MAX(m.created_at) AS created_at
				FROM %[1]s.migrations m, b
				WHERE m.schema = $1 AND m.migration_type <> 'inferred'
				AND NOT (m.migration ? 'revert_of')
				AND m.name COLLATE "C" <= b.name COLLATE "C"
			)
			SELECT m.name, m.migration, m.created_at
			FROM %[1]s.migrations m
			WHERE m.schema = $1
			%[2]s
			AND (
				NOT EXISTS (SELECT 1 FROM b)
				OR (m.migration_type <> 'inferred'
					AND (m.name COLLATE "C" > (SELECT name COLLATE "C" FROM b)
						OR m.migration ? 'revert_of'))
				OR (m.migration_type = 'inferred'
					AND m.created_at > (SELECT created_at FROM floor))
			) ORDER BY m.created_at`,
			pq.QuoteIdentifier(s.schema), inferredFilter), schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var entries []HistoryEntry
	for rows.Next() {
		var name, rawMigration string
		var createdAt time.Time

		if err := rows.Scan(&name, &rawMigration, &createdAt); err != nil {
			return nil, fmt.Errorf("row scan: %w", err)
		}

		var mig migrations.RawMigration
		err = json.Unmarshal([]byte(rawMigration), &mig)
		if err != nil {
			return nil, err
		}
		mig.Name = name

		entries = append(entries, HistoryEntry{
			Migration: mig,
			CreatedAt: createdAt,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rows: %w", err)
	}

	return entries, nil
}

// LatestBaseline returns the baseline in effect for a schema, or nil if no
// baseline exists. With several baseline rows it is the one whose name sorts
// last, not the one applied last: a row converted in place keeps the time its
// migration was applied, which can be earlier than an older baseline's.
func (s *State) LatestBaseline(ctx context.Context, schemaName string) (*BaselineMigration, error) {
	// Construct the SQL query to get the latest baseline migration
	query := fmt.Sprintf(`
		SELECT name, created_at, resulting_schema
		FROM %s.migrations
		WHERE schema = $1
		AND migration_type = 'baseline'
		ORDER BY name COLLATE "C" DESC
		LIMIT 1`,
		pq.QuoteIdentifier(s.schema))

	// Execute the query
	var name string
	var createdAt time.Time
	var rawSchema []byte
	err := s.pgConn.QueryRowContext(ctx, query, schemaName).Scan(&name, &createdAt, &rawSchema)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to query baseline migration: %w", err)
	}

	// Unmarshal the schema snapshot
	var schemaSnapshot schema.Schema
	err = json.Unmarshal(rawSchema, &schemaSnapshot)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal baseline schema: %w", err)
	}

	return &BaselineMigration{
		Name:           name,
		CreatedAt:      createdAt,
		SchemaSnapshot: schemaSnapshot,
	}, nil
}

// FoldedRow identifies a history row that the latest baseline folds in.
type FoldedRow struct {
	Name      string
	CreatedAt time.Time
}

// BaselineFloor returns the most recently applied row that the latest
// baseline folds in, or nil if no baseline exists. It is the baseline row
// itself unless a migration that sorts below the baseline was applied after
// it.
//
// The floor is the oldest state a revert can restore. Rows applied before it
// are interleaved with folded migrations whose files are gone, so the schema
// beneath them is not the schema any visible row recorded.
func (s *State) BaselineFloor(ctx context.Context, schemaName string) (*FoldedRow, error) {
	query := fmt.Sprintf(`
		SELECT m.name, m.created_at
		FROM %[1]s.migrations m
		WHERE m.schema = $1
		AND m.migration_type <> 'inferred'
		AND NOT (m.migration ? 'revert_of')
		AND m.name COLLATE "C" <= (
			SELECT b.name COLLATE "C" FROM %[1]s.migrations b
			WHERE b.schema = $1 AND b.migration_type = 'baseline'
			ORDER BY b.name COLLATE "C" DESC LIMIT 1
		)
		ORDER BY m.created_at DESC
		LIMIT 1`,
		pq.QuoteIdentifier(s.schema))

	var floor FoldedRow
	err := s.pgConn.QueryRowContext(ctx, query, schemaName).Scan(&floor.Name, &floor.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to query the baseline floor: %w", err)
	}
	return &floor, nil
}

// LatestBaselineName returns the name of the baseline in effect for a
// schema, or "" if there is none. It is LatestBaseline without the schema
// snapshot, for callers that only compare names.
func (s *State) LatestBaselineName(ctx context.Context, schemaName string) (string, error) {
	var name string
	err := s.pgConn.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT name FROM %s.migrations
		WHERE schema = $1 AND migration_type = 'baseline'
		ORDER BY name COLLATE "C" DESC
		LIMIT 1`, pq.QuoteIdentifier(s.schema)), schemaName).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to query the latest baseline: %w", err)
	}
	return name, nil
}
