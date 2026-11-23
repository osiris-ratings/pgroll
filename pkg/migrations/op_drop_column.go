// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"

	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

var (
	_ Operation  = (*OpDropColumn)(nil)
	_ Createable = (*OpDropColumn)(nil)
)

func (o *OpDropColumn) Start(ctx context.Context, l Logger, conn db.DB, s *schema.Schema) (*StartResult, error) {
	l.LogOperationStart(o)
	scope := s.MigrationScope

	table := s.GetTable(o.Table)
	if table == nil {
		return nil, TableDoesNotExistError{Name: o.Table}
	}
	column := table.GetColumn(o.Column)
	if column == nil {
		return nil, ColumnDoesNotExistError{Table: o.Table, Name: o.Column}
	}

	var task *backfill.Task
	if o.Down != "" {
		task = backfill.NewTask(
			nil,
			backfill.OperationTrigger{
				Name:      backfill.TriggerName(scope, o.Table, o.Column),
				Direction: backfill.TriggerDirectionDown,
				Columns:   table.Columns,
				TableName: table.Name,
				// The column's physical name: an earlier migration of the
				// train may have renamed it, or be about to.
				PhysicalColumn: column.Name,
				SQL:            o.Down,
			},
		)
	}

	s.GetTable(o.Table).RemoveColumn(o.Column)

	return &StartResult{BackfillTask: task}, nil
}

func (o *OpDropColumn) Complete(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationComplete(o)
	scope := s.MigrationScope

	// Target the physical base relation (table.Name); under a deferred in-train
	// rename it differs from the logical o.Table. Trigger-function identifiers
	// stay keyed by o.Table to match what Start installed.
	table := s.GetTable(o.Table)
	if table == nil {
		return nil, TableDoesNotExistError{Name: o.Table}
	}

	return []DBAction{
		NewDropColumnAction(conn, table.Name, o.Column),
		NewDropFunctionAction(conn, backfill.TriggerFunctionName(scope, o.Table, o.Column)),
		NewDropColumnAction(conn, table.Name, backfill.NeedsBackfillColumnName(scope)),
	}, nil
}

func (o *OpDropColumn) Rollback(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationRollback(o)
	scope := s.MigrationScope

	table := s.GetTable(o.Table)
	if table == nil {
		return nil, TableDoesNotExistError{Name: o.Table}
	}

	// Mark the column as no longer deleted so thats it's visible to preceding
	// rollback operations in the same migration
	table.UnRemoveColumn(o.Column)

	// NeedsBackfill cleanup targets the physical base relation (table.Name);
	// the trigger-function identifier stays keyed by o.Table to match Start.
	return []DBAction{
		NewDropFunctionAction(conn, backfill.TriggerFunctionName(scope, o.Table, o.Column)),
		NewDropColumnAction(conn, table.Name, backfill.NeedsBackfillColumnName(scope)),
	}, nil
}

func (o *OpDropColumn) Validate(ctx context.Context, s *schema.Schema) error {
	table := s.GetTable(o.Table)

	if table == nil {
		return TableDoesNotExistError{Name: o.Table}
	}
	if table.GetColumn(o.Column) == nil {
		return ColumnDoesNotExistError{Table: o.Table, Name: o.Column}
	}
	return nil
}
