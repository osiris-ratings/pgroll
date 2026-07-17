// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"

	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

var (
	_ Operation             = (*OpRenameConstraint)(nil)
	_ CompletedRollbackable = (*OpRenameConstraint)(nil)
)

func (o *OpRenameConstraint) Start(ctx context.Context, l Logger, conn db.DB, s *schema.Schema) (*StartResult, error) {
	l.LogOperationStart(o)

	// no-op
	return nil, nil
}

func (o *OpRenameConstraint) Complete(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationComplete(o)

	table := s.GetTable(o.Table)
	if table == nil {
		return nil, TableDoesNotExistError{Name: o.Table}
	}

	return []DBAction{
		// rename the constraint in the underlying (physical) table
		NewRenameConstraintAction(conn, table.Name, o.From, o.To),
	}, nil
}

func (o *OpRenameConstraint) Rollback(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationRollback(o)

	// no-op: the rename only happens at Complete, so there is nothing to
	// undo while the migration is still in its expand phase.
	return nil, nil
}

// RollbackCompleted undoes a rename_constraint whose Complete has already
// run: the physical rename happened, so renaming To back to From restores
// the prior schema. Without this, an inline-completed rename_constraint
// reverted as a silent no-op — the history row was deleted while the
// constraint kept its new name, and re-applying the train failed Validate
// because From no longer existed.
func (o *OpRenameConstraint) RollbackCompleted(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationRollback(o)

	return []DBAction{
		NewRenameConstraintAction(conn, o.Table, o.To, o.From),
	}, nil
}

func (o *OpRenameConstraint) Validate(ctx context.Context, s *schema.Schema) error {
	table := s.GetTable(o.Table)

	if table == nil {
		return TableDoesNotExistError{Name: o.Table}
	}

	if !table.ConstraintExists(o.From) {
		return ConstraintDoesNotExistError{Table: o.Table, Constraint: o.From}
	}

	if table.ConstraintExists(o.To) {
		return ConstraintAlreadyExistsError{Table: o.Table, Constraint: o.To}
	}

	if err := ValidateIdentifierLength(o.To); err != nil {
		return err
	}

	return nil
}
