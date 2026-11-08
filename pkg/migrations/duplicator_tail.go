// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/lib/pq"

	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

// duplicatedColumn is one column a duplicator's Complete puts back. done: the
// tail has already renamed the duplicate into place, so a retry after it
// committed must not drop the column it now finds there.
type duplicatedColumn struct {
	name string
	done bool
}

// pending reports whether the tail has not run for this column, so the
// steps that come before it still have to run too.
func (c duplicatedColumn) pending() bool { return !c.done }

// duplicatorTail is the destructive end of a duplicator's Complete, run as
// one transaction: lock the table, then for each column move sequence
// ownership to the duplicate, drop the triggers that keep the two in sync,
// drop the original, and rename the duplicate into its place with its indexes
// and constraints, re-adding the primary key. An interrupt leaves the table
// before it or after it, never between, so nothing reading the table ever
// sees the column missing.
//
// The duplicate's CHECK constraints are validated before the transaction
// (validateDuplicatedChecks), so the rename's VALIDATE inside it is a no-op
// and the ACCESS EXCLUSIVE lock the drop takes is not held through a scan.
type duplicatorTail struct {
	conn         db.DB
	id           string
	scope        string
	table        *schema.Table
	triggerTable string
	columns      []duplicatedColumn
}

func newDuplicatorTail(conn db.DB, scope string, table *schema.Table, triggerTable string, columns []duplicatedColumn) *duplicatorTail {
	names := make([]string, len(columns))
	for i, c := range columns {
		names[i] = c.name
	}
	return &duplicatorTail{
		conn:         conn,
		id:           fmt.Sprintf("duplicator_tail_%s_%s_%s", table.Name, strings.Join(names, "_"), scope),
		scope:        scope,
		table:        table,
		triggerTable: triggerTable,
		columns:      columns,
	}
}

func (a *duplicatorTail) ID() string { return a.id }

func (a *duplicatorTail) Execute(ctx context.Context) error {
	return a.conn.WithRetryableTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		conn := txConn{tx}
		// The rename consumes the table's metadata as it goes, so every
		// attempt (a lock timeout retries the whole transaction) starts from
		// a fresh copy.
		table, err := cloneTable(a.table)
		if err != nil {
			return err
		}
		// The table first: moving sequence ownership locks the sequence, and
		// taken before the table it deadlocks with an app transaction that
		// holds a row lock on the table and then calls nextval.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("LOCK TABLE %s IN ACCESS EXCLUSIVE MODE",
			pq.QuoteIdentifier(table.Name))); err != nil {
			return err
		}
		for _, c := range a.columns {
			temp := TemporaryName(a.scope, c.name)
			actions := make([]DBAction, 0, 4)
			if c.pending() {
				actions = append(actions, NewAlterSequenceOwnerAction(conn, table.Name, c.name, temp))
			}
			actions = append(actions, NewDropFunctionAction(conn,
				backfill.TriggerFunctionName(a.scope, a.triggerTable, c.name),
				backfill.TriggerFunctionName(a.scope, a.triggerTable, temp)))
			if c.pending() {
				actions = append(actions,
					NewDropColumnAction(conn, table.Name, c.name),
					NewRenameDuplicatedColumnAction(conn, a.scope, table, c.name))
			}
			for _, action := range actions {
				if err := action.Execute(ctx); err != nil {
					return fmt.Errorf("column %q: %w", c.name, err)
				}
			}
		}
		return nil
	})
}

// validateDuplicatedChecks validates the duplicate's copied CHECK constraints
// ahead of the duplicator tail: VALIDATE scans the table, and inside the tail
// it would hold the drop's ACCESS EXCLUSIVE lock for the whole scan.
type validateDuplicatedChecks struct {
	conn  db.DB
	id    string
	table *schema.Table
	from  []string
}

func newValidateDuplicatedChecks(conn db.DB, scope string, table *schema.Table, columns []duplicatedColumn) *validateDuplicatedChecks {
	from := make([]string, len(columns))
	for i, c := range columns {
		from[i] = TemporaryName(scope, c.name)
	}
	return &validateDuplicatedChecks{
		conn:  conn,
		id:    fmt.Sprintf("validate_duplicated_checks_%s_%s", table.Name, strings.Join(from, "_")),
		table: table,
		from:  from,
	}
}

func (a *validateDuplicatedChecks) ID() string { return a.id }

func (a *validateDuplicatedChecks) Execute(ctx context.Context) error {
	for _, cc := range a.table.CheckConstraints {
		if !IsDuplicatedName(cc.Name) || !slices.ContainsFunc(cc.Columns, func(c string) bool { return slices.Contains(a.from, c) }) {
			continue
		}
		if _, err := a.conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s",
			pq.QuoteIdentifier(a.table.Name), pq.QuoteIdentifier(cc.Name))); err != nil {
			return fmt.Errorf("failed to validate check constraint %q: %w", cc.Name, err)
		}
	}
	return nil
}

// duplicatorTailActions is what a duplicator's Complete ends with: validate
// the duplicates' checks, run the tail, then drop the backfill marker (shared
// across a migration's operations, so the coordinator keeps one, last).
func duplicatorTailActions(conn db.DB, scope string, table *schema.Table, triggerTable string, columns []duplicatedColumn) []DBAction {
	return []DBAction{
		newValidateDuplicatedChecks(conn, scope, table, columns),
		newDuplicatorTail(conn, scope, table, triggerTable, columns),
		NewDropColumnAction(conn, table.Name, backfill.NeedsBackfillColumnName(scope)),
	}
}

// duplicatedColumns classifies a duplicator's columns for its tail by what
// the catalog holds: both columns (pending), or only the column, the duplicate
// renamed into place (done; a retry after the tail committed). Only the
// duplicate (an older, non-atomic pgroll dropped the original and stopped
// before the rename) is an error: the table's primary key and constraints on
// that column went with the original, and the catalog cannot say what they
// were.
func duplicatedColumns(table *schema.Table, scope string, names ...string) ([]duplicatedColumn, error) {
	columns := make([]duplicatedColumn, len(names))
	for i, name := range names {
		temp := TemporaryName(scope, name)
		if table.GetColumn(name) == nil && table.GetColumn(temp) != nil {
			return nil, orphanedDuplicateError(table.Name, name, temp)
		}
		columns[i] = duplicatedColumn{name: name, done: table.GetColumn(name) != nil && table.GetColumn(temp) == nil}
	}
	return columns, nil
}

func orphanedDuplicateError(table, column, duplicate string) error {
	return fmt.Errorf("column %q of table %q is gone and its duplicate %q is still there: an older pgroll's "+
		"contraction dropped the original and stopped before renaming the duplicate into place, and the "+
		"table's primary key and constraints on %q went with it. pgroll cannot restore them from the catalog: "+
		"rename %q to %q and re-create them by hand from the migration's stored schema "+
		"(resulting_schema in pgroll's migrations table), then run `pgroll complete` again",
		column, table, duplicate, column, duplicate, column)
}

// tailCommitted reports whether a duplicator whose constraint is gone from
// the catalog has already run its tail: no duplicate made under scope is left
// on the table. Only the backfill marker is then left to drop. A duplicate
// still there means an older pgroll dropped the original column, and the
// constraint with it, before renaming the duplicate.
func tailCommitted(table *schema.Table, scope string) (bool, string) {
	suffix := scopeSuffix(scope)
	for name := range table.Columns {
		if strings.HasPrefix(name, temporaryPrefix) && strings.HasSuffix(name, suffix) {
			return false, name
		}
	}
	return true, ""
}

func cloneTable(t *schema.Table) (*schema.Table, error) {
	raw, err := json.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("unable to copy table %q: %w", t.Name, err)
	}
	var out schema.Table
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("unable to copy table %q: %w", t.Name, err)
	}
	return &out, nil
}

// txConn is a db.DB that runs everything on one transaction.
type txConn struct{ tx *sql.Tx }

func (c txConn) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	return c.tx.ExecContext(ctx, query, args...)
}

func (c txConn) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	return c.tx.QueryContext(ctx, query, args...)
}

func (c txConn) WithRetryableTransaction(ctx context.Context, f func(context.Context, *sql.Tx) error) error {
	return f(ctx, c.tx)
}

func (c txConn) Close() error { return nil }
