// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"

	"github.com/lib/pq"

	"github.com/xataio/pgroll/pkg/db"
)

// Catalog inspection helpers used to make `Complete`-path DDL actions
// idempotent. `complete` is not wrapped in a transaction and flips a
// migration's `done` flag only after all of its DDL has been applied, so an
// interruption (SIGINT, pod eviction, lock_timeout, dropped connection) in
// that window leaves the physical schema fully migrated while the migration is
// still recorded as in-progress. Re-running `complete` then replays the same
// actions from the top, and any action that is not idempotent — chiefly the
// RENAME / ADD CONSTRAINT / ADD PRIMARY KEY actions, which have no native
// IF [NOT] EXISTS guard — would fail against the already-migrated schema and
// wedge the migration. These probes let those actions detect work that has
// already happened and no-op instead.
//
// Every probe returns a `known` flag. It is false only when the action runs
// against the unit-test FakeDB (whose QueryContext returns no rows); callers
// then fall back to unconditional execution, preserving both the FakeDB-based
// unit tests and the `ALTER TABLE IF EXISTS` no-op-on-missing-table semantics
// the actions already relied on.

// columnExists reports whether table has a live (non-dropped) column named
// column. The table name is resolved through the session search_path, so a
// missing table yields (false, true, nil).
func columnExists(ctx context.Context, conn db.DB, table, column string) (exists, known bool, err error) {
	return probeBool(ctx, conn,
		`SELECT EXISTS(
			SELECT 1 FROM pg_attribute
			WHERE attrelid = to_regclass($1)
			  AND attname = $2
			  AND attnum > 0
			  AND NOT attisdropped)`,
		pq.QuoteIdentifier(table), column)
}

// constraintExists reports whether table has a constraint named constraint.
func constraintExists(ctx context.Context, conn db.DB, table, constraint string) (exists, known bool, err error) {
	return probeBool(ctx, conn,
		`SELECT EXISTS(
			SELECT 1 FROM pg_constraint
			WHERE conrelid = to_regclass($1)
			  AND conname = $2)`,
		pq.QuoteIdentifier(table), constraint)
}

// primaryKeyExists reports whether table already has a primary key constraint.
func primaryKeyExists(ctx context.Context, conn db.DB, table string) (exists, known bool, err error) {
	return probeBool(ctx, conn,
		`SELECT EXISTS(
			SELECT 1 FROM pg_constraint
			WHERE conrelid = to_regclass($1)
			  AND contype = 'p')`,
		pq.QuoteIdentifier(table))
}

// probeBool runs a query that returns a single boolean and reports its value.
// known is false when the query hit the FakeDB (nil rows), signalling callers
// to proceed unconditionally.
func probeBool(ctx context.Context, conn db.DB, query string, args ...any) (value, known bool, err error) {
	rows, err := conn.QueryContext(ctx, query, args...)
	if err != nil {
		return false, false, err
	}
	if rows == nil {
		// FakeDB used by unit tests: existence is unknowable here.
		return false, false, nil
	}
	defer rows.Close()

	if err := db.ScanFirstValue(rows, &value); err != nil {
		return false, false, err
	}
	return value, true, nil
}
