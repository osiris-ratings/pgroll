// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/oapi-codegen/nullable"

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
	// Invert returns the inverse operations, in application order. pre is
	// the schema state immediately before this operation ran — for prior
	// definitions (dropped columns' types, index definitions, previous
	// defaults) that the operation itself does not record. Most inverses
	// are a single operation; a dropped table inverts to its re-creation
	// plus the re-creation of its non-constraint indexes.
	Invert(pre *schema.Schema) ([]Operation, error)
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
// The inverse migration is named "revert_<name>", carries RevertOf, and no
// version schema; callers (the revert orchestrator) assign one.
func (m *Migration) Invert(ctx context.Context, parent *schema.Schema) (*Migration, error) {
	inverses, err := InvertSegment(ctx, []*Migration{m}, parent)
	if err != nil {
		return nil, err
	}
	return inverses[0], nil
}

// InvertSegment synthesizes inverse migrations for a contiguous sealed
// segment of history, newest-inverse-first. base is the clean boundary
// snapshot immediately below the segment (a refreshed train-final or a
// baseline). Per-operation pre-states are reconstructed by replaying every
// operation's Start virtually from the boundary upward — intermediate rows'
// stored snapshots (captured mid-flight, polluted with expand artifacts)
// are never consulted.
func InvertSegment(ctx context.Context, migs []*Migration, base *schema.Schema) ([]*Migration, error) {
	work, err := copySchema(base)
	if err != nil {
		return nil, fmt.Errorf("unable to copy boundary schema: %w", err)
	}

	fakeDB := &db.FakeDB{}
	preStates := make([][]*schema.Schema, len(migs))
	// Raw SQL is opaque to the virtual replay: its Up never mutates the
	// work schema (the real engine compensates with a physical re-read the
	// replay does not have). Once a raw-SQL operation has been replayed,
	// every later pre-state in the segment is untrustworthy for operations
	// whose inverse reads it — refuse those loudly at plan time instead of
	// synthesizing a silently wrong restoration.
	taintedBy := ""
	for mi, mig := range migs {
		work.MigrationScope = MigrationScopeFor(mig.Name)
		preStates[mi] = make([]*schema.Schema, len(mig.Operations))
		for oi, op := range mig.Operations {
			if _, isRaw := op.(*OpRawSQL); isRaw {
				taintedBy = mig.Name
			} else if taintedBy != "" && invertReadsPreState(op) {
				return nil, fmt.Errorf(
					"cannot invert: migration %q contains raw SQL whose schema effects cannot be replayed, "+
						"so the pre-state for %q in %q is untrustworthy; revert this segment manually or "+
						"choose a boundary above %q", taintedBy, OperationName(op), mig.Name, taintedBy,
				)
			}
			preStates[mi][oi], err = copySchema(work)
			if err != nil {
				return nil, fmt.Errorf("unable to snapshot schema state: %w", err)
			}
			preStates[mi][oi].MigrationScope = work.MigrationScope
			if err := replayStart(ctx, op, mig.Name, fakeDB, work); err != nil {
				return nil, err
			}
		}
	}

	inverses := make([]*Migration, 0, len(migs))
	for mi := len(migs) - 1; mi >= 0; mi-- {
		mig := migs[mi]
		ops := make(Operations, 0, len(mig.Operations))
		for oi := len(mig.Operations) - 1; oi >= 0; oi-- {
			op := mig.Operations[oi]
			inv, ok := op.(Invertible)
			if !ok {
				return nil, fmt.Errorf("operation %q in migration %q does not support inversion", OperationName(op), mig.Name)
			}
			invOps, err := inv.Invert(preStates[mi][oi])
			if err != nil {
				return nil, fmt.Errorf("unable to invert operation %q in migration %q: %w", OperationName(op), mig.Name, err)
			}
			ops = append(ops, invOps...)
		}
		// Structural validation: the inverse runs through the normal engine,
		// whose Migration.Validate rejects isolated operations combined with
		// others. Catch that here, at plan time, rather than mid-train.
		if len(ops) > 1 {
			for _, op := range ops {
				if iso, ok := op.(IsolatedOperation); ok && iso.IsIsolated() {
					return nil, fmt.Errorf(
						"internal: synthesized inverse of %q combines isolated operation %q with other operations",
						mig.Name, OperationName(op),
					)
				}
			}
		}
		inverses = append(inverses, &Migration{
			Name:       "revert_" + mig.Name,
			Operations: ops,
			RevertOf:   mig.Name,
		})
	}

	return inverses, nil
}

// replayStart virtually applies an operation's Start to the work schema,
// converting panics into errors: typed operations dereference tables their
// Start expects to exist, and a segment whose raw SQL created those objects
// leaves the virtual schema without them.
func replayStart(ctx context.Context, op Operation, migName string, fakeDB db.DB, work *schema.Schema) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf(
				"unable to replay operation %q of %q against the reconstructed schema "+
					"(it likely references objects created by raw SQL in the segment): %v",
				OperationName(op), migName, r,
			)
		}
	}()
	if _, err := op.Start(ctx, NewNoopLogger(), fakeDB, work); err != nil {
		return fmt.Errorf("unable to replay operation %q of %q: %w", OperationName(op), migName, err)
	}
	return nil
}

// invertReadsPreState reports whether the operation's Invert consults the
// pre-state snapshot (prior definitions, types, defaults). Only these are
// at risk from raw-SQL replay blindness; renames, creates, and adds invert
// from their own fields.
func invertReadsPreState(op Operation) bool {
	switch op.(type) {
	case *OpDropColumn, *OpDropTable, *OpDropIndex, *OpAlterColumn:
		return true
	default:
		return false
	}
}

// copySchema deep-copies a schema via its JSON representation, preserving
// the Deleted markers that json:"-" would otherwise silently drop — losing
// them resurrects dropped tables/columns in later pre-states, synthesizing
// inverses that collide mid-train.
func copySchema(s *schema.Schema) (*schema.Schema, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var out schema.Schema
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	for name, tbl := range s.Tables {
		if tbl == nil {
			continue
		}
		ot := out.Tables[name]
		if ot == nil {
			continue
		}
		ot.Deleted = tbl.Deleted
		for cname, col := range tbl.Columns {
			if col == nil || !col.Deleted {
				continue
			}
			if oc := ot.Columns[cname]; oc != nil {
				oc.Deleted = true
			}
		}
	}
	return &out, nil
}

// --- Per-operation inverses -------------------------------------------------
//
// Grouped here (rather than per op file) so the fork's divergence from
// upstream stays in one place.

// Invert swaps the rename's direction.
func (o *OpRenameColumn) Invert(_ *schema.Schema) ([]Operation, error) {
	return []Operation{&OpRenameColumn{Table: o.Table, From: o.To, To: o.From}}, nil
}

// Invert swaps the rename's direction.
func (o *OpRenameTable) Invert(_ *schema.Schema) ([]Operation, error) {
	return []Operation{&OpRenameTable{From: o.To, To: o.From}}, nil
}

// Invert swaps the rename's direction.
func (o *OpRenameConstraint) Invert(_ *schema.Schema) ([]Operation, error) {
	return []Operation{&OpRenameConstraint{Table: o.Table, From: o.To, To: o.From}}, nil
}

// Invert drops the created table. Data written to the table since creation
// is destroyed — that is what reverting a create means.
func (o *OpCreateTable) Invert(_ *schema.Schema) ([]Operation, error) {
	return []Operation{&OpDropTable{Name: o.Name}}, nil
}

// Invert drops the created index.
func (o *OpCreateIndex) Invert(_ *schema.Schema) ([]Operation, error) {
	return []Operation{&OpDropIndex{Name: o.Name}}, nil
}

// Invert drops the added column. The drop's down expression — which keeps
// the column derivable while the inverse migration is itself in its expand
// phase — is the original add's up expression; a column without one falls
// back to its default, then (if nullable) to NULL.
func (o *OpAddColumn) Invert(_ *schema.Schema) ([]Operation, error) {
	down := o.Up
	if down == "" {
		switch {
		case o.Column.HasDefault():
			down = *o.Column.Default
		case o.Column.IsNullable():
			down = "NULL"
		default:
			return nil, fmt.Errorf("add_column %q on %q has no up expression, no default, and is not nullable; its inverse drop cannot re-derive values", o.Column.Name, o.Table)
		}
	}
	return []Operation{&OpDropColumn{Table: o.Table, Column: o.Column.Name, Down: down}}, nil
}

// Invert runs the original down expression as the inverse's statement —
// deferred to the inverse train's drain (onComplete) so destructive
// counter-statements (e.g. DROP TABLE undoing a raw CREATE) execute after
// the sealed train's version schemas are reaped, exactly like forward
// destructive DDL. Until the drain, nothing has run, so rolling back an
// interrupted inverse train is a clean no-op for this operation.
func (o *OpRawSQL) Invert(_ *schema.Schema) ([]Operation, error) {
	if o.Down == "" {
		return nil, fmt.Errorf("raw SQL operation has no down expression")
	}
	return []Operation{&OpRawSQL{Up: o.Down, OnComplete: true}}, nil
}

// Invert recreates the dropped index from its definition in the pre-state
// snapshot (pg_get_indexdef output — full fidelity including expressions,
// predicates, opclasses).
func (o *OpDropIndex) Invert(pre *schema.Schema) ([]Operation, error) {
	for _, table := range pre.Tables {
		if table == nil {
			continue
		}
		if idx, ok := table.Indexes[o.Name]; ok && idx != nil {
			if idx.Definition == "" {
				return nil, fmt.Errorf("index %q has no recorded definition in the parent snapshot", o.Name)
			}
			// OnComplete: a bare raw-SQL op IsIsolated and would be rejected
			// by Migration.Validate when the inverse combines it with other
			// operations; deferring to the drain also matches forward
			// semantics (and onComplete structurally forbids down).
			return []Operation{&OpRawSQL{
				Up:         idx.Definition,
				OnComplete: true,
			}}, nil
		}
	}
	return nil, fmt.Errorf("index %q not found in the parent snapshot", o.Name)
}

// Invert re-adds the dropped column with its definition recovered from the
// pre-state snapshot. The add's up expression — which re-derives the
// column's values — is the original drop's down expression (required by
// reversibility-by-construction).
func (o *OpDropColumn) Invert(pre *schema.Schema) ([]Operation, error) {
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
	if col.Identity != "" {
		return nil, fmt.Errorf(
			"column %q on %q was a GENERATED AS IDENTITY column; identity restoration is not supported "+
				"and recreating it as a plain column would silently break inserts", o.Column, o.Table,
		)
	}

	added := Column{
		Name:     o.Column,
		Type:     col.Type,
		Nullable: col.Nullable,
		Default:  col.Default,
		Unique:   col.Unique,
	}
	// A serial column's owned sequence was dropped with the column; the
	// default would reference a nonexistent regclass and the re-add would
	// fail. Recreate via the serial pseudo-type, which recreates the
	// sequence too. The new sequence restarts at 1 — callers re-derive the
	// column's values via down, but future inserts draw fresh numbers.
	if st, ok := serialTypeFor(col, o.Table, o.Column); ok {
		added.Type = st
		added.Default = nil
	}
	if col.Comment != "" {
		added.Comment = &col.Comment
	}

	return []Operation{&OpAddColumn{
		Table:  o.Table,
		Column: added,
		Up:     o.Down,
	}}, nil
}

// Invert restores the column's prior type/nullability/default/comment from
// the pre-state snapshot, with the data expressions swapped (the original
// down re-derives the prior values; the original up re-derives the new ones
// while the inverse is itself in its expand phase). Constraint-adding
// sub-operations (check/unique/references) invert to constraint drops,
// which are not yet expressible — refused by name.
func (o *OpAlterColumn) Invert(pre *schema.Schema) ([]Operation, error) {
	if o.Check != nil || o.Unique != nil || o.References != nil {
		return nil, fmt.Errorf("alter_column on %q.%q adds a constraint; constraint inverses are not supported yet", o.Table, o.Column)
	}

	table := pre.GetTable(o.Table)
	if table == nil {
		return nil, fmt.Errorf("table %q not found in the parent snapshot", o.Table)
	}
	col := table.GetColumn(o.Column)
	if col == nil {
		return nil, fmt.Errorf("column %q on %q not found in the parent snapshot", o.Column, o.Table)
	}

	inv := &OpAlterColumn{
		Table:  o.Table,
		Column: o.Column,
		Up:     o.Down,
		Down:   o.Up,
	}
	changed := false
	if o.Type != nil {
		priorType := col.Type
		inv.Type = &priorType
		changed = true
	}
	if o.Nullable != nil {
		priorNullable := col.Nullable
		inv.Nullable = &priorNullable
		changed = true
	}
	if o.Default.IsSpecified() {
		if col.Default == nil {
			inv.Default = nullable.NewNullNullable[string]()
		} else {
			inv.Default = nullable.NewNullableWithValue(*col.Default)
		}
		changed = true
	}
	if o.Comment.IsSpecified() {
		if col.Comment == "" {
			inv.Comment = nullable.NewNullNullable[string]()
		} else {
			inv.Comment = nullable.NewNullableWithValue(col.Comment)
		}
		changed = true
	}
	if !changed {
		return nil, fmt.Errorf("alter_column on %q.%q has no invertible sub-operation", o.Table, o.Column)
	}

	return []Operation{inv}, nil
}

// Invert recreates the dropped table from the pre-state snapshot: columns
// (with types, nullability, defaults, comments, primary key), table-level
// constraints (checks, uniques, foreign keys), and non-constraint indexes
// (from their pg_get_indexdef definitions). The table comes back EMPTY —
// the drop's drained contraction destroyed the rows, and no expression can
// re-derive a whole table. That is the honest meaning of best-effort here.
func (o *OpDropTable) Invert(pre *schema.Schema) ([]Operation, error) {
	table := pre.GetTable(o.Name)
	if table == nil {
		return nil, fmt.Errorf("table %q not found in the parent snapshot", o.Name)
	}

	create := &OpCreateTable{Name: o.Name}
	if table.Comment != "" {
		comment := table.Comment
		create.Comment = &comment
	}

	for _, key := range slices.Sorted(maps.Keys(table.Columns)) {
		c := table.Columns[key]
		if c == nil || c.Deleted {
			continue
		}
		if c.Identity != "" {
			return nil, fmt.Errorf(
				"column %q on dropped table %q was a GENERATED AS IDENTITY column; identity restoration "+
					"is not supported and recreating it as a plain column would silently break inserts",
				key, o.Name,
			)
		}
		col := Column{
			Name:     key,
			Type:     c.Type,
			Nullable: c.Nullable,
			Default:  c.Default,
			Unique:   c.Unique,
			Pk:       slices.Contains(table.PrimaryKey, key),
		}
		// Serial columns: the owned sequence was dropped with the table, so
		// the recorded nextval default references a nonexistent regclass and
		// CREATE TABLE would fail. Recreate via the serial pseudo-type
		// (which recreates the sequence; it restarts at 1 — the table comes
		// back empty, so only the counter is lost).
		if st, ok := serialTypeFor(c, o.Name, key); ok {
			col.Type = st
			col.Default = nil
		}
		if c.Comment != "" {
			comment := c.Comment
			col.Comment = &comment
		}
		create.Columns = append(create.Columns, col)
	}

	for _, key := range slices.Sorted(maps.Keys(table.CheckConstraints)) {
		cc := table.CheckConstraints[key]
		if cc == nil {
			continue
		}
		create.Constraints = append(create.Constraints, Constraint{
			Name:      cc.Name,
			Type:      ConstraintTypeCheck,
			Columns:   cc.Columns,
			Check:     stripCheckDefinition(cc.Definition),
			NoInherit: cc.NoInherit,
		})
	}
	for _, key := range slices.Sorted(maps.Keys(table.UniqueConstraints)) {
		uc := table.UniqueConstraints[key]
		if uc == nil {
			continue
		}
		create.Constraints = append(create.Constraints, Constraint{
			Name:    uc.Name,
			Type:    ConstraintTypeUnique,
			Columns: uc.Columns,
		})
	}
	for _, key := range slices.Sorted(maps.Keys(table.ForeignKeys)) {
		fk := table.ForeignKeys[key]
		if fk == nil {
			continue
		}
		create.Constraints = append(create.Constraints, Constraint{
			Name:    fk.Name,
			Type:    ConstraintTypeForeignKey,
			Columns: fk.Columns,
			References: &TableForeignKeyReference{
				Table:    fk.ReferencedTable,
				Columns:  fk.ReferencedColumns,
				OnDelete: ForeignKeyAction(fk.OnDelete),
				OnUpdate: ForeignKeyAction(fk.OnUpdate),
			},
		})
	}

	ops := []Operation{create}

	// Non-constraint indexes are not part of create_table; recreate them
	// from their stored definitions. Constraint-backed indexes come back
	// with their constraints.
	constraintNames := map[string]bool{}
	for name := range table.UniqueConstraints {
		constraintNames[name] = true
	}
	for _, key := range slices.Sorted(maps.Keys(table.Indexes)) {
		idx := table.Indexes[key]
		if idx == nil || constraintNames[idx.Name] {
			continue
		}
		if isPrimaryKeyIndex(idx.Name, table) {
			continue
		}
		if idx.Definition == "" {
			return nil, fmt.Errorf("index %q on dropped table %q has no recorded definition", idx.Name, o.Name)
		}
		// OnComplete: see OpDropIndex.Invert — a non-onComplete raw-SQL op
		// IsIsolated and Migration.Validate rejects it alongside the
		// create_table, which made every drop_table with a secondary index
		// uninvertible.
		ops = append(ops, &OpRawSQL{
			Up:         idx.Definition,
			OnComplete: true,
		})
	}

	return ops, nil
}

// serialTypeFor maps an integer column whose default draws from the
// sequence pgroll/Postgres would own for (table, column) — the serial
// pattern — to the serial pseudo-type that recreates both the column and
// its owned sequence. Defaults drawing from other (shared) sequences are
// returned unchanged: those sequences are not owned by the dropped object
// and survive it.
func serialTypeFor(c *schema.Column, table, column string) (string, bool) {
	if c.Default == nil {
		return "", false
	}
	def := *c.Default
	if !strings.Contains(def, "nextval(") || !strings.Contains(def, fmt.Sprintf("%s_%s_seq", table, column)) {
		return "", false
	}
	switch c.Type {
	case "integer", "int4", "int":
		return "serial", true
	case "bigint", "int8":
		return "bigserial", true
	case "smallint", "int2":
		return "smallserial", true
	}
	return "", false
}

// stripCheckDefinition extracts the bare expression from a
// pg_get_constraintdef-style "CHECK ((expr))" definition.
func stripCheckDefinition(def string) string {
	trimmed := strings.TrimSpace(def)
	upper := strings.ToUpper(trimmed)
	if strings.HasPrefix(upper, "CHECK") {
		trimmed = strings.TrimSpace(trimmed[len("CHECK"):])
	}
	if strings.HasPrefix(trimmed, "(") && strings.HasSuffix(trimmed, ")") {
		trimmed = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
	}
	return trimmed
}

// isPrimaryKeyIndex reports whether the index backs the table's primary key
// (recreated implicitly by the pk columns on create_table).
func isPrimaryKeyIndex(name string, table *schema.Table) bool {
	return len(table.PrimaryKey) > 0 && strings.HasSuffix(name, "_pkey")
}
