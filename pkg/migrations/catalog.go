// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"
	"strings"

	"github.com/lib/pq"

	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
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

// postgresIdentifier returns name as Postgres stores it.
func postgresIdentifier(name string) string { return schema.StoredIdentifier(name) }

// presence asks the catalog, when an action runs, whether an object is
// there. known is false against the FakeDB.
type presence func(ctx context.Context) (exists, known bool, err error)

func constraintPresent(conn db.DB, table, constraint string) presence {
	return func(ctx context.Context) (bool, bool, error) {
		return constraintExists(ctx, conn, table, postgresIdentifier(constraint))
	}
}

func indexPresent(conn db.DB, index string) presence {
	return func(ctx context.Context) (bool, bool, error) {
		return probeBool(ctx, conn, `SELECT to_regclass($1) IS NOT NULL`, pq.QuoteIdentifier(postgresIdentifier(index)))
	}
}

// onlyWhen runs actions when the object is present, or cannot be asked
// about. Without it the actions have nothing left to do.
func onlyWhen(present presence, actions ...DBAction) DBAction {
	ids := make([]string, len(actions))
	for i, a := range actions {
		ids[i] = a.ID()
	}
	return &conditionalAction{id: "when_present_" + strings.Join(ids, "_"), present: present, actions: actions}
}

type conditionalAction struct {
	id      string
	present presence
	actions []DBAction
}

func (a *conditionalAction) ID() string { return a.id }

func (a *conditionalAction) Execute(ctx context.Context) error {
	exists, known, err := a.present(ctx)
	if err != nil {
		return err
	}
	if known && !exists {
		return nil
	}
	for _, action := range a.actions {
		if err := action.Execute(ctx); err != nil {
			return err
		}
	}
	return nil
}

// validateColumnCheckAction validates the check an add_column gave its
// column: by name, or, when a later migration renamed it, whichever checks
// on the column are not yet valid.
type validateColumnCheckAction struct {
	conn                 db.DB
	table, column, check string
}

func newValidateColumnCheckAction(conn db.DB, table, column, check string) *validateColumnCheckAction {
	return &validateColumnCheckAction{conn: conn, table: table, column: column, check: check}
}

func (a *validateColumnCheckAction) ID() string {
	return fmt.Sprintf("validate_column_check_%s_%s_%s", a.table, a.column, a.check)
}

func (a *validateColumnCheckAction) Execute(ctx context.Context) error {
	exists, known, err := constraintExists(ctx, a.conn, a.table, postgresIdentifier(a.check))
	if err != nil {
		return err
	}
	if exists || !known {
		return NewValidateConstraintAction(a.conn, a.table, a.check).Execute(ctx)
	}

	rows, err := a.conn.QueryContext(ctx, `
		SELECT c.conname
		FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
		WHERE c.conrelid = to_regclass($1)
		AND c.contype = 'c' AND NOT c.convalidated
		AND a.attname = $2
		AND c.conname NOT LIKE '\_pgroll\_%'`,
		pq.QuoteIdentifier(a.table), postgresIdentifier(a.column))
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		names = append(names, name)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, name := range names {
		if err := NewValidateConstraintAction(a.conn, a.table, name).Execute(ctx); err != nil {
			return err
		}
	}
	return nil
}
