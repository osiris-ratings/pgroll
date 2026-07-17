// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"

	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

type OpSetCheckConstraint struct {
	Table  string          `json:"table"`
	Column string          `json:"column"`
	Check  CheckConstraint `json:"check"`
	Up     string          `json:"up"`
	Down   string          `json:"down"`
}

var _ Operation = (*OpSetCheckConstraint)(nil)

func (o *OpSetCheckConstraint) Start(ctx context.Context, l Logger, conn db.DB, s *schema.Schema) (*StartResult, error) {
	l.LogOperationStart(o)

	table := s.GetTable(o.Table)
	if table == nil {
		return nil, TableDoesNotExistError{Name: o.Table}
	}

	// Add the check constraint to the new column as NOT VALID.
	skipValidate := true // We will validate the constraint later in the Complete step.
	dbActions := []DBAction{
		NewCreateCheckConstraintAction(
			conn,
			s.MigrationScope,
			table.Name,
			o.Check.Name,
			o.Check.Constraint,
			[]string{o.Column},
			o.Check.NoInherit,
			skipValidate,
		),
	}

	// Register the constraint in the in-memory schema so a subsequent
	// migration in a deferred batch can find it via readSchemaWithDeferred
	// replay. Without this, replay leaves the constraint absent until its
	// originating migration's Complete physically applied — which it
	// hasn't because Complete is what we're replaying for.
	if table.CheckConstraints == nil {
		table.CheckConstraints = make(map[string]*schema.CheckConstraint)
	}
	table.CheckConstraints[o.Check.Name] = &schema.CheckConstraint{
		Name:       o.Check.Name,
		Columns:    []string{o.Column},
		Definition: o.Check.Constraint,
		NoInherit:  o.Check.NoInherit,
	}

	return &StartResult{Actions: dbActions, BackfillTask: backfill.NewTask(table)}, nil
}

func (o *OpSetCheckConstraint) Complete(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationComplete(o)

	table := s.GetTable(o.Table)
	if table == nil {
		return nil, TableDoesNotExistError{Name: o.Table}
	}

	return []DBAction{
		// Validate the check constraint on the physical base relation
		NewValidateConstraintAction(conn, table.Name, o.Check.Name),
	}, nil
}

func (o *OpSetCheckConstraint) Rollback(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationRollback(o)

	return nil, nil
}

func (o *OpSetCheckConstraint) Validate(ctx context.Context, s *schema.Schema) error {
	if err := o.Check.Validate(); err != nil {
		return CheckConstraintError{
			Table:  o.Table,
			Column: o.Column,
			Err:    err,
		}
	}

	table := s.GetTable(o.Table)
	if table == nil {
		return TableDoesNotExistError{Name: o.Table}
	}

	if table.ConstraintExists(o.Check.Name) {
		return ConstraintAlreadyExistsError{
			Table:      table.Name,
			Constraint: o.Check.Name,
		}
	}

	if o.Up == "" {
		return FieldRequiredError{Name: "up"}
	}

	if o.Down == "" {
		return FieldRequiredError{Name: "down"}
	}

	return nil
}
