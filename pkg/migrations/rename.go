// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/lib/pq"

	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

// RenameDuplicatedColumn
// * Renames a duplicated column to its original name
// * Renames any foreign keys on the duplicated column to their original name.
// * Validates and renames any temporary `CHECK` constraints on the duplicated column.
type renameDuplicatedColumnAction struct {
	conn  db.DB
	id    string
	table *schema.Table
	from  string
	to    string
}

func NewRenameDuplicatedColumnAction(conn db.DB, scope string, table *schema.Table, column string) *renameDuplicatedColumnAction {
	return &renameDuplicatedColumnAction{
		conn:  conn,
		id:    fmt.Sprintf("rename_duplicated_%s_%s", table.Name, column),
		table: table,
		from:  TemporaryName(scope, column),
		to:    column,
	}
}

func (a *renameDuplicatedColumnAction) ID() string { return a.id }

func (a *renameDuplicatedColumnAction) Execute(ctx context.Context) error {
	const (
		cRenameIndexSQL = `ALTER INDEX IF EXISTS %s RENAME TO %s`
	)

	err := NewRenameColumnAction(a.conn, a.table.Name, a.from, a.to).Execute(ctx)
	if err != nil {
		return fmt.Errorf("failed to rename duplicated column %q: %w", a.to, err)
	}

	// Rename any foreign keys on the duplicated column from their temporary name
	// to their original name
	for _, fk := range a.table.ForeignKeys {
		if !IsDuplicatedName(fk.Name) {
			continue
		}

		if slices.Contains(fk.Columns, a.from) {
			err = NewRenameConstraintAction(a.conn, a.table.Name, fk.Name, StripDuplicationPrefix(fk.Name)).Execute(ctx)
			if err != nil {
				return fmt.Errorf("failed to rename foreign key constraint %q: %w", fk.Name, err)
			}
			delete(a.table.ForeignKeys, fk.Name)
		}
	}

	// Validate and rename any temporary `CHECK` constraints on the duplicated
	// column.
	for _, cc := range a.table.CheckConstraints {
		if !IsDuplicatedName(cc.Name) {
			continue
		}

		if slices.Contains(cc.Columns, a.from) {
			err := NewValidateConstraintAction(a.conn, a.table.Name, cc.Name).Execute(ctx)
			if err != nil {
				return fmt.Errorf("failed to validate check constraint %q: %w", cc.Name, err)
			}

			err = NewRenameConstraintAction(a.conn, a.table.Name, cc.Name, StripDuplicationPrefix(cc.Name)).Execute(ctx)
			if err != nil {
				return fmt.Errorf("failed to rename check constraint %q: %w", cc.Name, err)
			}
			delete(a.table.CheckConstraints, cc.Name)

			// If the constraint is a `NOT NULL` constraint, convert the duplicated
			// unchecked `NOT NULL` constraint into a `NOT NULL` attribute on the
			// column.
			if IsNotNullConstraintName(StripDuplicationPrefix(cc.Name)) {
				// Apply `NOT NULL` attribute to the column. This uses the validated
				// constraint. Pass the canonical name so the auto-named pg_constraint
				// row produced on PostgreSQL 17+ is renamed to match what pg_dump
				// treats as the default form.
				if err := NewSetNotNullAction(a.conn, a.table.Name, a.to, CanonicalNotNullName(a.table.Name, a.to)).Execute(ctx); err != nil {
					return fmt.Errorf("failed to set column not null: %w", err)
				}

				// Drop the constraint
				err = NewDropConstraintAction(a.conn, a.table.Name, NotNullConstraintName(a.to)).Execute(ctx)
				if err != nil {
					return fmt.Errorf("failed to drop not null constraint: %w", err)
				}
			}
		}
	}

	// Rename any indexes on the duplicated column and use unique indexes to
	// create `UNIQUE` constraints.
	for _, idx := range a.table.Indexes {
		if !IsDuplicatedName(idx.Name) || !slices.Contains(idx.Columns, a.from) {
			continue
		}

		// Rename the index to its original name
		renameIndexSQL := fmt.Sprintf(
			cRenameIndexSQL,
			pq.QuoteIdentifier(idx.Name),
			pq.QuoteIdentifier(StripDuplicationPrefix(idx.Name)),
		)

		_, err = a.conn.ExecContext(ctx, renameIndexSQL)
		if err != nil {
			return fmt.Errorf("failed to rename index %q: %w", idx.Name, err)
		}

		// Index no longer exists, remove it from the table
		delete(a.table.Indexes, idx.Name)

		if _, ok := a.table.UniqueConstraints[StripDuplicationPrefix(idx.Name)]; idx.Unique && ok {
			// Create a unique constraint using the unique index
			err := NewAddConstraintUsingUniqueIndex(
				a.conn,
				a.table.Name,
				StripDuplicationPrefix(idx.Name),
				StripDuplicationPrefix(idx.Name),
			).Execute(ctx)
			if err != nil {
				return fmt.Errorf("failed to create unique constraint from index %q: %w", idx.Name, err)
			}
		}
	}

	if slices.Contains(a.table.PrimaryKey, a.to) {
		err := NewAddPrimaryKeyAction(a.conn, a.table.Name, primaryKeyName(a.table.Name)).Execute(ctx)
		if err != nil {
			return fmt.Errorf("failed to re-add primary key constraint: %w", err)
		}
	}

	return nil
}

func primaryKeyName(tableName string) string {
	return fmt.Sprintf("%s_pkey", tableName)
}

// duplicateOnly reports whether a duplicator's Complete was interrupted after
// it dropped the original column: the original is gone and its duplicate is
// still there. What is left is idempotent (drop the trigger function and the
// backfill marker, rename the duplicate), so the Complete resumes with that.
func duplicateOnly(table *schema.Table, scope, column string) bool {
	return table.GetColumn(column) == nil && table.GetColumn(TemporaryName(scope, column)) != nil
}

// duplicatedColumnName returns the original name of the one column of table
// duplicated under scope, for a Complete that can no longer read it from the
// constraint it dropped with the original column.
func duplicatedColumnName(table *schema.Table, scope string) (string, bool) {
	prefix, suffix := temporaryPrefix, scopeSuffix(scope)
	var found []string
	for name := range table.Columns {
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) && len(name) > len(prefix)+len(suffix) {
			found = append(found, name[len(prefix):len(name)-len(suffix)])
		}
	}
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}
