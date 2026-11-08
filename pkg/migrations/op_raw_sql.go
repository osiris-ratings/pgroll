// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"
	"strings"

	pgq "github.com/xataio/pg_query_go/v6"

	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

var (
	_ Operation  = (*OpRawSQL)(nil)
	_ Createable = (*OpRawSQL)(nil)
)

func (o *OpRawSQL) Start(ctx context.Context, l Logger, conn db.DB, s *schema.Schema) (*StartResult, error) {
	l.LogOperationStart(o)

	if o.OnComplete {
		return nil, nil
	}

	dbActions := []DBAction{
		NewRawSQLAction(conn, o.Up),
	}
	return &StartResult{Actions: dbActions}, nil
}

func (o *OpRawSQL) Complete(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationComplete(o)

	if !o.OnComplete {
		return nil, nil
	}

	return []DBAction{NewRawSQLAction(conn, o.Up)}, nil
}

func (o *OpRawSQL) Rollback(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationRollback(o)

	if o.Down == "" {
		return nil, nil
	}

	return []DBAction{NewRawSQLAction(conn, o.Down)}, nil
}

func (o *OpRawSQL) Validate(ctx context.Context, s *schema.Schema) error {
	if o.Up == "" {
		return EmptyMigrationError{}
	}

	if o.OnComplete && o.Down != "" {
		return InvalidMigrationError{Reason: "down is not allowed with onComplete"}
	}

	if o.OnComplete {
		if err := CheckNoTransactionControl(o.Up); err != nil {
			return InvalidMigrationError{Reason: err.Error()}
		}
	}

	return nil
}

// CheckNoTransactionControl fails when sql holds a top-level transaction
// control statement (BEGIN, COMMIT, ROLLBACK, SAVEPOINT, ...), or cannot be
// parsed to tell. Contraction runs onComplete SQL inside a transaction of its
// own, and such a statement would end it early.
func CheckNoTransactionControl(sql string) error {
	tree, err := pgq.Parse(sql)
	if err != nil {
		return fmt.Errorf("onComplete SQL could not be parsed, so pgroll cannot tell whether it controls "+
			"transactions: %w", err)
	}
	for _, raw := range tree.GetStmts() {
		if raw.GetStmt().GetTransactionStmt() == nil {
			continue
		}
		stmt := sql
		if start, n := int(raw.GetStmtLocation()), int(raw.GetStmtLen()); start >= 0 && start < len(sql) {
			stmt = sql[start:]
			if n > 0 && start+n <= len(sql) {
				stmt = sql[start : start+n]
			}
		}
		return fmt.Errorf("onComplete SQL must not control transactions, and %q does: pgroll runs it inside a "+
			"transaction of its own to keep the live version schema whole", strings.TrimSpace(stmt))
	}
	return nil
}

// IsIsolated returns true if the operation is isolated and should be run with other operations.
func (o *OpRawSQL) IsIsolated() bool {
	return !o.OnComplete
}

func (o *OpRawSQL) RequiresSchemaRefresh() {}
