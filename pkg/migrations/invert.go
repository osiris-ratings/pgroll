// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

// Invertible is implemented by operations that can synthesize their own
// inverse: an operation that, applied to the schema state this operation
// left behind, restores the state it started from.
//
// Inversion is the sealed-history counterpart to Rollback: once a
// migration's contraction has drained, its expand-phase artifacts are gone
// and Rollback no longer applies, but an inverse migration can be run
// FORWARD through the normal expand/contract engine to restore the prior
// schema shape. Data restoration is best-effort by construction: the
// inverse's data expressions re-derive values via the original operation's
// up/down expressions; independently-stored data destroyed by a drained
// contraction is not recoverable.
//
// Operations that do not implement Invertible make their migration
// non-invertible (callers refuse with the operation named).
type Invertible interface {
	// Invert returns the inverse operation. pre is the schema state
	// immediately before this operation ran — for prior definitions
	// (dropped columns' types, index definitions, previous defaults) that
	// the operation itself does not record.
	Invert(pre *schema.Schema) (Operation, error)
}

// Invert synthesizes the inverse migration: the original operations
// inverted, in reverse order. parent is the schema state immediately before
// this migration ran (a clean train-boundary snapshot).
//
// Each operation is inverted against the virtual schema state it actually
// ran on: the parent state with the preceding operations' Start effects
// replayed. This makes multi-operation migrations invert correctly (e.g. a
// rename followed by an alter of the renamed column).
//
// The inverse migration is named "revert_<name>" and carries no version
// schema; callers (the revert orchestrator) assign one.
func (m *Migration) Invert(ctx context.Context, parent *schema.Schema) (*Migration, error) {
	// Capture the pre-state of every operation by replaying Starts
	// virtually on a working copy.
	work, err := copySchema(parent)
	if err != nil {
		return nil, fmt.Errorf("unable to copy parent schema: %w", err)
	}
	work.MigrationScope = MigrationScopeFor(m.Name)

	preStates := make([]*schema.Schema, len(m.Operations))
	fakeDB := &db.FakeDB{}
	for i, op := range m.Operations {
		preStates[i], err = copySchema(work)
		if err != nil {
			return nil, fmt.Errorf("unable to snapshot schema state: %w", err)
		}
		preStates[i].MigrationScope = work.MigrationScope
		if _, err := op.Start(ctx, NewNoopLogger(), fakeDB, work); err != nil {
			return nil, fmt.Errorf("unable to replay operation %q: %w", OperationName(op), err)
		}
	}

	ops := make(Operations, 0, len(m.Operations))
	for i := len(m.Operations) - 1; i >= 0; i-- {
		op := m.Operations[i]
		inv, ok := op.(Invertible)
		if !ok {
			return nil, fmt.Errorf("operation %q does not support inversion", OperationName(op))
		}
		invOp, err := inv.Invert(preStates[i])
		if err != nil {
			return nil, fmt.Errorf("unable to invert operation %q: %w", OperationName(op), err)
		}
		ops = append(ops, invOp)
	}

	return &Migration{
		Name:       "revert_" + m.Name,
		Operations: ops,
	}, nil
}

// copySchema deep-copies a schema via its JSON representation.
func copySchema(s *schema.Schema) (*schema.Schema, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var out schema.Schema
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// --- Per-operation inverses -------------------------------------------------
//
// Grouped here (rather than per op file) so the fork's divergence from
// upstream stays in one place.

// Invert swaps the rename's direction.
func (o *OpRenameColumn) Invert(_ *schema.Schema) (Operation, error) {
	return &OpRenameColumn{Table: o.Table, From: o.To, To: o.From}, nil
}

// Invert swaps the rename's direction.
func (o *OpRenameTable) Invert(_ *schema.Schema) (Operation, error) {
	return &OpRenameTable{From: o.To, To: o.From}, nil
}

// Invert swaps the rename's direction.
func (o *OpRenameConstraint) Invert(_ *schema.Schema) (Operation, error) {
	return &OpRenameConstraint{Table: o.Table, From: o.To, To: o.From}, nil
}

// Invert drops the created table. Data written to the table since creation
// is destroyed — that is what reverting a create means.
func (o *OpCreateTable) Invert(_ *schema.Schema) (Operation, error) {
	return &OpDropTable{Name: o.Name}, nil
}

// Invert drops the created index.
func (o *OpCreateIndex) Invert(_ *schema.Schema) (Operation, error) {
	return &OpDropIndex{Name: o.Name}, nil
}

// Invert drops the added column. The drop's down expression — which keeps
// the column derivable while the inverse migration is itself in its expand
// phase — is the original add's up expression; a nullable column without
// one falls back to NULL.
func (o *OpAddColumn) Invert(_ *schema.Schema) (Operation, error) {
	down := o.Up
	if down == "" {
		if !o.Column.IsNullable() {
			return nil, fmt.Errorf("add_column %q on %q has no up expression and is not nullable; its inverse drop cannot re-derive values", o.Column.Name, o.Table)
		}
		down = "NULL"
	}
	return &OpDropColumn{Table: o.Table, Column: o.Column.Name, Down: down}, nil
}

// Invert swaps up and down. onComplete SQL becomes plain SQL: the original
// up ran when the migration's contraction drained, so the inverse must run
// its counter-statement during its own expand phase.
func (o *OpRawSQL) Invert(_ *schema.Schema) (Operation, error) {
	if o.Down == "" {
		return nil, fmt.Errorf("raw SQL operation has no down expression")
	}
	return &OpRawSQL{Up: o.Down, Down: o.Up}, nil
}

// Invert recreates the dropped index from its definition in the pre-state
// snapshot (pg_get_indexdef output — full fidelity including expressions,
// predicates, opclasses).
func (o *OpDropIndex) Invert(pre *schema.Schema) (Operation, error) {
	for _, table := range pre.Tables {
		if table == nil {
			continue
		}
		if idx, ok := table.Indexes[o.Name]; ok && idx != nil {
			if idx.Definition == "" {
				return nil, fmt.Errorf("index %q has no recorded definition in the parent snapshot", o.Name)
			}
			return &OpRawSQL{
				Up:   idx.Definition,
				Down: fmt.Sprintf("DROP INDEX IF EXISTS %q", o.Name),
			}, nil
		}
	}
	return nil, fmt.Errorf("index %q not found in the parent snapshot", o.Name)
}

// Invert re-adds the dropped column with its definition recovered from the
// pre-state snapshot. The add's up expression — which re-derives the
// column's values — is the original drop's down expression (required by
// reversibility-by-construction).
func (o *OpDropColumn) Invert(pre *schema.Schema) (Operation, error) {
	table := pre.GetTable(o.Table)
	if table == nil {
		return nil, fmt.Errorf("table %q not found in the parent snapshot", o.Table)
	}
	col := table.GetColumn(o.Column)
	if col == nil {
		return nil, fmt.Errorf("column %q on %q not found in the parent snapshot", o.Column, o.Table)
	}
	if o.Down == "" {
		return nil, fmt.Errorf("drop_column %q on %q has no down expression; its inverse add cannot re-derive values", o.Column, o.Table)
	}

	added := Column{
		Name:     o.Column,
		Type:     col.Type,
		Nullable: col.Nullable,
		Default:  col.Default,
		Unique:   col.Unique,
	}
	if col.Comment != "" {
		added.Comment = &col.Comment
	}

	return &OpAddColumn{
		Table:  o.Table,
		Column: added,
		Up:     o.Down,
	}, nil
}
