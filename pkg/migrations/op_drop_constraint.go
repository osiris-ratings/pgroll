// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"

	"github.com/lib/pq"

	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

var _ Operation = (*OpDropConstraint)(nil)

func (o *OpDropConstraint) Start(ctx context.Context, l Logger, conn db.DB, s *schema.Schema) (*StartResult, error) {
	l.LogOperationStart(o)
	scope := s.MigrationScope

	table := s.GetTable(o.Table)
	if table == nil {
		return nil, TableDoesNotExistError{Name: o.Table}
	}

	// During FakeDB-backed replay (readSchemaWithDeferred building up
	// other migrations' Start effects so this Start sees their schema
	// state), an earlier migration's deferred OpSetCheckConstraint /
	// OpSetUnique / OpSetForeignKey may not have been replayed if it was
	// excluded or if the replay hasn't reached it yet. Constraint
	// metadata can therefore legitimately be absent. Treat that as a
	// no-op replay rather than panicking — the live (non-replay) Start
	// path always passes through Validate first, which checks the
	// constraint exists.
	constraintColumns := table.GetConstraintColumns(o.Name)
	if len(constraintColumns) == 0 {
		return nil, nil
	}
	column := table.GetColumn(constraintColumns[0])
	if column == nil {
		return nil, ColumnDoesNotExistError{Table: o.Table, Name: constraintColumns[0]}
	}

	// Create a copy of the column on the underlying table.
	dbActions := []DBAction{
		NewColumnDuplicator(conn, scope, table, column).WithoutConstraint(o.Name),
	}

	// Copy the columns from table columns, so we can use it later
	// in the down trigger with the physical name
	upColumns := make(map[string]*schema.Column)
	for name, col := range table.Columns {
		upColumns[name] = col
	}

	// Add a trigger to copy values from the old column to the new, rewriting values using the `up` SQL.
	triggers := make([]backfill.OperationTrigger, 0, 2)
	triggers = append(
		triggers,
		backfill.OperationTrigger{
			Name:           backfill.TriggerName(scope, o.Table, column.Name),
			Direction:      backfill.TriggerDirectionUp,
			Columns:        upColumns,
			TableName:      o.Table,
			PhysicalColumn: TemporaryName(scope, column.Name),
			SQL:            o.upSQL(column.Name),
		},
	)

	// Add the new column to the internal schema representation. This is done
	// here, before creation of the down trigger, so that the trigger can declare
	// a variable for the new column. Preserve the original column's metadata
	// (Type, Nullable, Default, etc.) so replays across deferred migrations
	// don't strip fields that downstream Validate steps depend on.
	newCol := *column
	newCol.Name = TemporaryName(scope, column.Name)
	table.AddColumn(column.Name, &newCol)

	// Add a trigger to copy values from the new column to the old, rewriting values using the `down` SQL.
	triggers = append(
		triggers,
		backfill.OperationTrigger{
			Name:           backfill.TriggerName(scope, o.Table, TemporaryName(scope, column.Name)),
			Direction:      backfill.TriggerDirectionDown,
			Columns:        table.Columns,
			TableName:      o.Table,
			PhysicalColumn: column.Name,
			SQL:            o.Down,
		},
	)
	return &StartResult{Actions: dbActions, BackfillTask: backfill.NewTask(table, triggers...)}, nil
}

func (o *OpDropConstraint) Complete(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationComplete(o)
	scope := s.MigrationScope

	// We have already validated that there is single column related to this constraint.
	table := s.GetTable(o.Table)
	if table == nil {
		return nil, TableDoesNotExistError{Name: o.Table}
	}
	column := table.GetColumn(table.GetConstraintColumns(o.Name)[0])

	// Target the physical base relation (table.Name); under a deferred in-train
	// rename it differs from the logical o.Table. Trigger-function identifiers
	// stay keyed by o.Table to match what Start installed.
	return []DBAction{
		NewDropFunctionAction(conn,
			backfill.TriggerFunctionName(scope, o.Table, column.Name),
			backfill.TriggerFunctionName(scope, o.Table, TemporaryName(scope, column.Name))),
		NewAlterSequenceOwnerAction(conn, table.Name, column.Name, TemporaryName(scope, column.Name)),
		NewDropColumnAction(conn, table.Name, backfill.NeedsBackfillColumnName(scope)),
		NewDropColumnAction(conn, table.Name, column.Name),
		NewRenameDuplicatedColumnAction(conn, scope, table, column.Name),
	}, nil
}

func (o *OpDropConstraint) Rollback(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationRollback(o)
	scope := s.MigrationScope

	// We have already validated that there is single column related to this constraint.
	table := s.GetTable(o.Table)
	if table == nil {
		return nil, TableDoesNotExistError{Name: o.Table}
	}
	columnName := table.GetConstraintColumns(o.Name)[0]

	// Physical DDL targets the physical base relation (table.Name); under a
	// train that defers an earlier rename of this table it differs from the
	// logical o.Table. Trigger-function identifiers stay keyed by o.Table to
	// match what Start installed.
	return []DBAction{
		NewDropColumnAction(conn, table.Name, TemporaryName(scope, columnName)),
		NewDropFunctionAction(conn,
			backfill.TriggerFunctionName(scope, o.Table, columnName),
			backfill.TriggerFunctionName(scope, o.Table, TemporaryName(scope, columnName))),
		NewDropColumnAction(conn, table.Name, backfill.NeedsBackfillColumnName(scope)),
	}, nil
}

func (o *OpDropConstraint) Validate(ctx context.Context, s *schema.Schema) error {
	table := s.GetTable(o.Table)
	if table == nil {
		return TableDoesNotExistError{Name: o.Table}
	}

	if o.Name == "" {
		return FieldRequiredError{Name: "name"}
	}

	if !table.ConstraintExists(o.Name) {
		return ConstraintDoesNotExistError{Table: o.Table, Constraint: o.Name}
	}

	columns := table.GetConstraintColumns(o.Name)

	// We already know the constraint exists because we checked it earlier so we only need to check the
	// case where there are multiple columns.
	if len(columns) > 1 {
		return MultiColumnConstraintsNotSupportedError{
			Table:      table.Name,
			Constraint: o.Name,
		}
	}

	if o.Down == "" {
		return FieldRequiredError{Name: "down"}
	}

	return nil
}

func (o *OpDropConstraint) upSQL(column string) string {
	if o.Up != "" {
		return o.Up
	}

	return pq.QuoteIdentifier(column)
}
