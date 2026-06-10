// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"

	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

var (
	_ Operation  = (*OpCreateConstraint)(nil)
	_ Createable = (*OpCreateConstraint)(nil)
)

func (o *OpCreateConstraint) Start(ctx context.Context, l Logger, conn db.DB, s *schema.Schema) (*StartResult, error) {
	l.LogOperationStart(o)
	scope := s.MigrationScope

	table := s.GetTable(o.Table)
	if table == nil {
		return nil, TableDoesNotExistError{Name: o.Table}
	}

	columns := make([]*schema.Column, len(o.Columns))
	for i, colName := range o.Columns {
		columns[i] = table.GetColumn(colName)
		if columns[i] == nil {
			return nil, ColumnDoesNotExistError{Table: o.Table, Name: colName}
		}
	}

	// Duplicate each column using its final name after migration completion
	d := NewColumnDuplicator(conn, scope, table, columns...)
	for _, colName := range o.Columns {
		d = d.WithName(table.GetColumn(colName).Name, TemporaryName(scope, colName))
	}
	dbActions := []DBAction{d}

	// Copy the columns from table columns, so we can use it later
	// in the down trigger with the physical name
	upColumns := make(map[string]*schema.Column)
	for name, col := range table.Columns {
		upColumns[name] = col
	}

	// Setup triggers
	triggers := make([]backfill.OperationTrigger, 0)
	for _, colName := range o.Columns {
		upSQL := o.Up[colName]
		triggers = append(
			triggers,
			backfill.OperationTrigger{
				Name:           backfill.TriggerName(scope, o.Table, colName),
				Direction:      backfill.TriggerDirectionUp,
				Columns:        upColumns,
				TableName:      table.Name,
				PhysicalColumn: TemporaryName(scope, colName),
				SQL:            upSQL,
			},
		)

		// Add the new column to the internal schema representation. This is done
		// here, before creation of the down trigger, so that the trigger can declare
		// a variable for the new column. Save the old column name for use as the
		// physical column name in the down trigger first. Preserve column
		// metadata so replays across deferred migrations don't strip fields
		// downstream Validate steps depend on.
		oldCol := table.GetColumn(colName)
		oldPhysicalColumn := oldCol.Name
		newCol := *oldCol
		newCol.Name = TemporaryName(scope, colName)
		table.AddColumn(colName, &newCol)

		downSQL := o.Down[colName]
		triggers = append(
			triggers,
			backfill.OperationTrigger{
				Name:           backfill.TriggerName(scope, o.Table, TemporaryName(scope, colName)),
				Direction:      backfill.TriggerDirectionDown,
				Columns:        table.Columns,
				TableName:      table.Name,
				PhysicalColumn: oldPhysicalColumn,
				SQL:            downSQL,
			},
		)
	}

	task := backfill.NewTask(table, triggers...)

	switch o.Type {
	case OpCreateConstraintTypeUnique, OpCreateConstraintTypePrimaryKey:
		dbActions = append(
			dbActions,
			NewCreateUniqueIndexConcurrentlyAction(conn, s.Name, o.Name, table.Name, temporaryNames(scope, o.Columns)...),
		)
		return &StartResult{Actions: dbActions, BackfillTask: task}, nil

	case OpCreateConstraintTypeCheck:
		dbActions = append(
			dbActions,
			NewCreateCheckConstraintAction(conn, scope, table.Name, o.Name, *o.Check, o.Columns, o.NoInherit, true),
		)
		return &StartResult{Actions: dbActions, BackfillTask: task}, nil

	case OpCreateConstraintTypeForeignKey:
		dbActions = append(
			dbActions,
			NewCreateFKConstraintAction(conn, table.Name, o.Name, temporaryNames(scope, o.Columns), o.References, false, false, true),
		)
		return &StartResult{Actions: dbActions, BackfillTask: task}, nil
	}

	return &StartResult{Actions: dbActions, BackfillTask: task}, nil
}

func (o *OpCreateConstraint) Complete(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationComplete(o)
	scope := s.MigrationScope

	dbActions := make([]DBAction, 0)
	switch o.Type {
	case OpCreateConstraintTypeUnique:
		uniqueOp := &OpSetUnique{
			Table: o.Table,
			Name:  o.Name,
		}
		actions, err := uniqueOp.Complete(l, conn, s)
		if err != nil {
			return nil, err
		}
		dbActions = append(dbActions, actions...)
	case OpCreateConstraintTypeCheck:
		checkOp := &OpSetCheckConstraint{
			Table: o.Table,
			Check: CheckConstraint{
				Name: o.Name,
			},
		}
		actions, err := checkOp.Complete(l, conn, s)
		if err != nil {
			return nil, err
		}
		dbActions = append(dbActions, actions...)
	case OpCreateConstraintTypeForeignKey:
		fkOp := &OpSetForeignKey{
			Table: o.Table,
			References: ForeignKeyReference{
				Name: o.Name,
			},
		}
		actions, err := fkOp.Complete(l, conn, s)
		if err != nil {
			return nil, err
		}
		dbActions = append(dbActions, actions...)
	case OpCreateConstraintTypePrimaryKey:
		dbActions = append(dbActions, NewAddPrimaryKeyAction(conn, o.Table, o.Name))
	}

	for _, col := range o.Columns {
		dbActions = append(dbActions, NewAlterSequenceOwnerAction(conn, o.Table, col, TemporaryName(scope, col)))
	}

	dbActions = append(dbActions, NewDropColumnAction(conn, o.Table, o.Columns...))

	// rename new columns to old name
	table := s.GetTable(o.Table)
	if table == nil {
		return nil, TableDoesNotExistError{Name: o.Table}
	}
	table.Name = o.Table
	for _, col := range o.Columns {
		column := table.GetColumn(col)
		if column == nil {
			return nil, ColumnDoesNotExistError{Table: o.Table, Name: col}
		}
		dbActions = append(dbActions, NewRenameDuplicatedColumnAction(conn, scope, table, col))
	}
	dbActions = append(
		dbActions,
		o.removeTriggers(conn, scope),
		NewDropColumnAction(conn, o.Table, backfill.NeedsBackfillColumnName(scope)),
	)

	return dbActions, nil
}

func (o *OpCreateConstraint) Rollback(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationRollback(o)
	scope := s.MigrationScope

	table := s.GetTable(o.Table)
	if table == nil {
		return nil, TableDoesNotExistError{Name: o.Table}
	}

	return []DBAction{
		NewDropColumnAction(conn, table.Name, temporaryNames(scope, o.Columns)...),
		o.removeTriggers(conn, scope),
		NewDropColumnAction(conn, table.Name, backfill.NeedsBackfillColumnName(scope)),
	}, nil
}

// RollbackCompleted undoes a create_constraint whose Complete has already
// run. By then the original columns are gone and the backfilled duplicates
// have been renamed into their place, with the new constraint attached —
// dropping the constraint by name restores the prior schema shape. The
// column contents keep the `up`-transformed values; for the standard
// add-a-constraint-to-valid-data case `up` is the identity, so this is
// lossless.
func (o *OpCreateConstraint) RollbackCompleted(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationRollback(o)

	return []DBAction{
		NewDropConstraintAction(conn, o.Table, o.Name),
	}, nil
}

func (o *OpCreateConstraint) removeTriggers(conn db.DB, scope string) DBAction {
	dropFuncs := make([]string, 0, len(o.Columns)*2)
	for _, column := range o.Columns {
		dropFuncs = append(dropFuncs, backfill.TriggerFunctionName(scope, o.Table, column))
		dropFuncs = append(dropFuncs, backfill.TriggerFunctionName(scope, o.Table, TemporaryName(scope, column)))
	}
	return NewDropFunctionAction(conn, dropFuncs...)
}

func (o *OpCreateConstraint) Validate(ctx context.Context, s *schema.Schema) error {
	table := s.GetTable(o.Table)
	if table == nil {
		return TableDoesNotExistError{Name: o.Table}
	}

	if err := ValidateIdentifierLength(o.Name); err != nil {
		return err
	}

	if table.ConstraintExists(o.Name) {
		return ConstraintAlreadyExistsError{
			Table:      o.Table,
			Constraint: o.Name,
		}
	}

	for _, col := range o.Columns {
		if table.GetColumn(col) == nil {
			return ColumnDoesNotExistError{
				Table: o.Table,
				Name:  col,
			}
		}
		if _, ok := o.Up[col]; !ok {
			return ColumnMigrationMissingError{
				Table: o.Table,
				Name:  col,
			}
		}
		if _, ok := o.Down[col]; !ok {
			return ColumnMigrationMissingError{
				Table: o.Table,
				Name:  col,
			}
		}
	}

	switch o.Type {
	case OpCreateConstraintTypeUnique:
		if len(o.Columns) == 0 {
			return FieldRequiredError{Name: "columns"}
		}
	case OpCreateConstraintTypeCheck:
		if o.Check == nil || *o.Check == "" {
			return FieldRequiredError{Name: "check"}
		}
	case OpCreateConstraintTypeForeignKey:
		if o.References == nil {
			return FieldRequiredError{Name: "references"}
		}
		table := s.GetTable(o.References.Table)
		if table == nil {
			return TableDoesNotExistError{Name: o.References.Table}
		}
		for _, col := range o.References.Columns {
			if table.GetColumn(col) == nil {
				return ColumnDoesNotExistError{
					Table: o.References.Table,
					Name:  col,
				}
			}
		}
	}

	return nil
}

func temporaryNames(scope string, columns []string) []string {
	names := make([]string, len(columns))
	for i, col := range columns {
		names[i] = TemporaryName(scope, col)
	}
	return names
}
