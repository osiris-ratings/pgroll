// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"

	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

var _ Operation = (*OpRenameTable)(nil)

func (o *OpRenameTable) Start(ctx context.Context, l Logger, conn db.DB, s *schema.Schema) (*StartResult, error) {
	l.LogOperationStart(o)

	return nil, s.RenameTable(o.From, o.To)
}

func (o *OpRenameTable) Complete(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationComplete(o)

	s.RenameTable(o.From, o.To)

	// The action returned below physically renames the base relation within
	// this same Complete batch, so advance the in-memory physical name too:
	// later operations in this batch that resolve the table by its (logical)
	// name must target the post-rename physical relation their DDL will run
	// against. This is what distinguishes an in-batch rename from a deferred
	// one — readSchemaWithDeferred replays a deferred rename's Start (below),
	// whose physical rename has NOT run in this batch, and deliberately leaves
	// Table.Name at the old physical name so dependent ops target the relation
	// that still physically exists.
	if t := s.GetTable(o.To); t != nil {
		t.Name = o.To
	}

	return []DBAction{NewRenameTableAction(conn, o.From, o.To)}, nil
}

func (o *OpRenameTable) Rollback(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationRollback(o)

	s.RenameTable(o.To, o.From)
	return nil, nil
}

func (o *OpRenameTable) Validate(ctx context.Context, s *schema.Schema) error {
	if s.GetTable(o.From) == nil {
		return TableDoesNotExistError{Name: o.From}
	}
	if s.GetTable(o.To) != nil {
		return TableAlreadyExistsError{Name: o.To}
	}
	if err := ValidateIdentifierLength(o.To); err != nil {
		return err
	}

	s.RenameTable(o.From, o.To)
	return nil
}
