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

// tailState is how far a duplicator's tail got for one column.
type tailState int

const (
	// tailPending: original and duplicate both there; the tail has not run.
	tailPending tailState = iota
	// tailOrphaned: the original is gone and the duplicate not yet renamed,
	// a state only an older, non-atomic pgroll leaves (see resumeDuplicate).
	tailOrphaned
	// tailDone: the duplicate has been renamed into place; a retry after the
	// tail committed must not drop the column it now finds there.
	tailDone
)

// duplicatedColumn is one column a duplicator's Complete puts back.
type duplicatedColumn struct {
	name  string
	state tailState
}

// pending reports whether the tail has not run for this column, so the
// steps that come before it still have to run too.
func (c duplicatedColumn) pending() bool { return c.state == tailPending }

// duplicatorTail is the destructive end of a duplicator's Complete, run as
// one transaction: for each column, move sequence ownership to the duplicate,
// drop the triggers that keep the two in sync, drop the original, and rename
// the duplicate into its place with its indexes and constraints, re-adding
// the primary key. An interrupt leaves the table before it or after it,
// never between, so nothing reading the table ever sees the column missing.
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
		for _, c := range a.columns {
			temp := TemporaryName(a.scope, c.name)
			actions := make([]DBAction, 0, 4)
			if c.state == tailPending {
				actions = append(actions, NewAlterSequenceOwnerAction(conn, table.Name, c.name, temp))
			}
			actions = append(actions, NewDropFunctionAction(conn,
				backfill.TriggerFunctionName(a.scope, a.triggerTable, c.name),
				backfill.TriggerFunctionName(a.scope, a.triggerTable, temp)))
			if c.state == tailPending {
				actions = append(actions, NewDropColumnAction(conn, table.Name, c.name))
			}
			if c.state != tailDone {
				actions = append(actions, NewRenameDuplicatedColumnAction(conn, a.scope, table, c.name))
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

// resumeDuplicate reports whether a duplicator's Complete was interrupted
// after it dropped the original column: the original is gone and its
// duplicate is still there. The atomic tail never leaves this state; an
// older pgroll could. What is left then is the tail without the drop, and the
// caller must have restored the primary key and constraint metadata the drop
// took with it from pgroll's schema snapshot (roll's reviveFromSnapshot):
// the catalog no longer has them.
func resumeDuplicate(table *schema.Table, scope, column string) bool {
	return table.GetColumn(column) == nil && table.GetColumn(TemporaryName(scope, column)) != nil
}

// duplicatedColumns classifies a duplicator's columns for its tail by what
// the catalog holds: both columns (pending), only the duplicate (orphaned by
// an older pgroll), or only the column, the duplicate renamed into place
// (done; a retry after the tail committed).
func duplicatedColumns(table *schema.Table, scope string, names ...string) []duplicatedColumn {
	columns := make([]duplicatedColumn, len(names))
	for i, name := range names {
		state := tailPending
		switch {
		case resumeDuplicate(table, scope, name):
			state = tailOrphaned
		case table.GetColumn(name) != nil && table.GetColumn(TemporaryName(scope, name)) == nil:
			state = tailDone
		}
		columns[i] = duplicatedColumn{name: name, state: state}
	}
	return columns
}

// DuplicatedColumnsOf returns, for each table a migration's duplicator
// operations rewrite, the logical columns they duplicate. Constraint-based
// operations name a constraint, not a column, so its columns are read from
// `constraintsFrom`: pass pgroll's schema snapshot from before the migration,
// because the catalog loses a constraint when an interrupted contraction
// drops its column.
func DuplicatedColumnsOf(m *Migration, constraintsFrom *schema.Schema) map[string][]string {
	out := map[string][]string{}
	constraintColumns := func(table, constraint string) []string {
		if constraintsFrom == nil {
			return nil
		}
		t := constraintsFrom.GetTable(table)
		if t == nil {
			return nil
		}
		cols := t.GetConstraintColumns(constraint)
		for i, c := range cols {
			cols[i] = logicalName(c)
		}
		return cols
	}
	for _, op := range m.Operations {
		switch o := op.(type) {
		case *OpAlterColumn:
			out[o.Table] = append(out[o.Table], o.Column)
		case *OpCreateConstraint:
			out[o.Table] = append(out[o.Table], o.Columns...)
		case *OpDropConstraint:
			out[o.Table] = append(out[o.Table], constraintColumns(o.Table, o.Name)...)
		case *OpDropMultiColumnConstraint:
			out[o.Table] = append(out[o.Table], constraintColumns(o.Table, o.Name)...)
		}
	}
	return out
}

// ResumesDuplicate is resumeDuplicate for callers outside the package.
func ResumesDuplicate(table *schema.Table, scope, column string) bool {
	return resumeDuplicate(table, scope, column)
}

// tailCommitted reports whether a duplicator whose constraint is gone from
// the catalog, and from every snapshot, has already run its tail: no
// duplicate made under scope is left on the table. Only the backfill marker
// is then left to drop. A duplicate still there means an older pgroll
// orphaned it, and the column it belongs to must come from a snapshot.
func tailCommitted(table *schema.Table, scope string) bool {
	suffix := scopeSuffix(scope)
	for name := range table.Columns {
		if strings.HasPrefix(name, temporaryPrefix) && strings.HasSuffix(name, suffix) {
			return false
		}
	}
	return true
}

// LogicalColumnName is logicalName for callers outside the package.
func LogicalColumnName(physical string) string { return logicalName(physical) }

// logicalName strips a duplicator's temporary prefix and scope from a
// physical column name, e.g. `_pgroll_new_user_id_bf4acbf8` -> `user_id`.
func logicalName(physical string) string {
	if !strings.HasPrefix(physical, temporaryPrefix) {
		return physical
	}
	return strings.TrimPrefix(temporaryNameRebase("", physical), temporaryPrefix)
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
