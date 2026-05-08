// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"encoding/json"
	"fmt"

	_ "github.com/lib/pq"

	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

// Operation is an operation that can be applied to a schema
type Operation interface {
	// Start will return the list of required changes to enable supporting the new schema
	// version in the database (through a view)
	// update the given views to expose the new schema version
	// Returns the table that requires backfilling, if any.
	Start(ctx context.Context, l Logger, conn db.DB, s *schema.Schema) (*StartResult, error)

	// Complete will update the database schema to match the current version
	// after calling Start.
	// This method should be called once the previous version is no longer used.
	Complete(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error)

	// Rollback will revert the changes made by Start. It is not possible to
	// rollback a completed migration.
	Rollback(l Logger, conn db.DB, s *schema.Schema) ([]DBAction, error)

	// Validate returns a descriptive error if the operation cannot be applied to the given schema.
	Validate(ctx context.Context, s *schema.Schema) error
}

// Createable interface must be implemented for all operations
// that can be created using the CLI create command.
//
// The function must prompt users to configure all attributes of an operation.
//
// Example implementation for OpMyOperation that has 3 attributes: table, column and down:
//
//	func (o *OpMyOperation) Create() {
//		o.Table, _ = pterm.DefaultInteractiveTextInput.WithDefaultText("table").Show()
//		o.Column, _ = pterm.DefaultInteractiveTextInput.WithDefaultText("column").Show()
//		o.Down, _ = pterm.DefaultInteractiveTextInput.WithDefaultText("down").Show()
//	}
type Createable interface {
	Create()
}

// IsolatedOperation is an operation that cannot be executed with other operations
// in the same migration.
type IsolatedOperation interface {
	// IsIsolated defines where this operation is isolated when executed on start, cannot be executed
	// with other operations.
	IsIsolated() bool
}

// RequiresSchemaRefreshOperation is an operation that requires the resulting schema to be refreshed.
type RequiresSchemaRefreshOperation interface {
	// RequiresSchemaRefresh defines if this operation requires the resulting schema to be refreshed when
	// executed on start.
	RequiresSchemaRefresh()
}

type (
	Operations []Operation
	Migration  struct {
		Name          string     `json:"-"`
		VersionSchema string     `json:"version_schema,omitempty"`
		Operations    Operations `json:"operations"`
	}
	RawMigration struct {
		Name          string          `json:"-"`
		VersionSchema string          `json:"version_schema,omitempty"`
		Operations    json.RawMessage `json:"operations"`
	}

	StartResult struct {
		Actions      []DBAction
		BackfillTask *backfill.Task
	}
)

// VersionSchemaName returns the version schema name for the migration.
// It defaults to the migration name if no version schema is set.
func (m *Migration) VersionSchemaName() string {
	if m.VersionSchema != "" {
		return m.VersionSchema
	}
	return m.Name
}

// Validate will check that the migration can be applied to the given schema
// returns a descriptive error if the migration is invalid
func (m *Migration) Validate(ctx context.Context, s *schema.Schema) error {
	for _, op := range m.Operations {
		if isolatedOp, ok := op.(IsolatedOperation); ok {
			if isolatedOp.IsIsolated() && len(m.Operations) > 1 {
				return InvalidMigrationError{Reason: fmt.Sprintf("operation %q cannot be executed with other operations", OperationName(op))}
			}
		}
	}

	for _, op := range m.Operations {
		err := op.Validate(ctx, s)
		if err != nil {
			return err
		}
	}

	return nil
}

// CompleteMustBeDeferred reports whether this migration's Complete actions
// must be deferred to the final `pgroll complete` so destructive DDL
// doesn't run while the prev-production version schema's views still
// reference the affected objects.
//
// "Defer everything" is tempting given namespacing, but it doesn't work
// for additive ops in a batch: downstream migrations may reference the
// added column by its user-facing name in raw SQL or typed lookups, and
// while a deferred OpAddColumn's Complete is queued, the column only
// exists physically as `_pgroll_new_<col>_<scope>`. Subsequent raw-SQL
// operations against the user-facing name fail.
//
// So additive ops (add column, create table, indexes, set
// default/comment, regular raw SQL) run inline. Their Completes don't
// touch user-facing identifiers prev-prod's view references — they
// rename temp names and drop pgroll-internal trigger machinery, which
// prev-prod doesn't know about. Inline is safe.
//
// Destructive and duplicator-pattern ops defer. With per-migration
// namespacing of internal artifacts (TemporaryName, TriggerFunctionName,
// DuplicationName, NeedsBackfillColumnName, DeletionName), concurrently
// deferred migrations don't collide on temp columns, trigger functions,
// or marker columns. Deferral is what unblocks them — their Complete
// drops user-facing columns/tables that prev-prod's view references,
// which Postgres rejects until prev-prod's view is gone.
func (m *Migration) CompleteMustBeDeferred() bool {
	for _, op := range m.Operations {
		switch v := op.(type) {
		case *OpDropColumn,
			*OpDropTable,
			*OpDropConstraint,
			*OpDropIndex,
			*OpDropMultiColumnConstraint,
			*OpRenameColumn,
			*OpRenameTable,
			*OpAlterColumn:
			return true
		case *OpRawSQL:
			if v.OnComplete {
				return true
			}
		}
	}
	return false
}

// UpdateVirtualSchema updates the in-memory schema representation with the changes
// made by the migration. No changes are made to the physical database.
func (m *Migration) UpdateVirtualSchema(ctx context.Context, s *schema.Schema) error {
	db := &db.FakeDB{}

	// Run `Start` on each operation using the fake DB. Updates will be made to
	// the in-memory schema `s` without touching the physical database.
	for _, op := range m.Operations {
		if _, err := op.Start(ctx, NewNoopLogger(), db, s); err != nil {
			return err
		}
	}
	return nil
}
