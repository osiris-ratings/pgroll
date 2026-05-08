// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"

	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

var (
	_ Operation  = (*OpDropIndex)(nil)
	_ Createable = (*OpDropIndex)(nil)
)

func (o *OpDropIndex) Start(ctx context.Context, l Logger, conn db.DB, s *schema.Schema) (*StartResult, error) {
	l.LogOperationStart(o)

	// Remove the index from the in-memory schema so that subsequent
	// migrations in a deferred batch — which see the schema state via
	// readSchemaWithDeferred replaying our Start — don't observe the
	// index. Without this a subsequent CreateIndex with the same name
	// (e.g. drop+recreate-with-different-method) would fail Validate as
	// "already exists" until our Complete physically dropped it at
	// final drain.
	for _, table := range s.Tables {
		if _, ok := table.Indexes[o.Name]; ok {
			delete(table.Indexes, o.Name)
			break
		}
	}

	return nil, nil
}

func (o *OpDropIndex) Complete(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationComplete(o)

	return []DBAction{
		NewDropIndexAction(conn, o.Name),
	}, nil
}

func (o *OpDropIndex) Rollback(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error) {
	l.LogOperationRollback(o)

	// no-op
	return nil, nil
}

func (o *OpDropIndex) Validate(ctx context.Context, s *schema.Schema) error {
	for _, table := range s.Tables {
		_, ok := table.Indexes[o.Name]
		if ok {
			return nil
		}
	}
	return IndexDoesNotExistError{Name: o.Name}
}
