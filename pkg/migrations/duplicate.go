// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

// duplicator duplicates a column in a table, including all constraints and
// comments.
type duplicator struct {
	id                string
	scope             string
	stmtBuilder       *duplicatorStmtBuilder
	conn              db.DB
	columns           map[string]*columnToDuplicate
	columnOrder       []string
	withoutConstraint []string
}

type columnToDuplicate struct {
	column         *schema.Column
	asName         string
	withoutNotNull bool
	withType       string
}

// duplicatorStmtBuilder is a helper for building SQL statements to duplicate
// columns and constraints in a table.
type duplicatorStmtBuilder struct {
	scope string
	table *schema.Table
}

const (
	dataTypeMismatchErrorCode  pqerror.Code = "42804"
	undefinedFunctionErrorCode pqerror.Code = "42883"
)

// NewColumnDuplicator creates a new Duplicator for a column. The migration
// scope makes the duplicated identifiers (`_pgroll_new_<col>_<scope>`,
// `_pgroll_dup_<constraint>_<scope>`) unique per migration so concurrently-
// deferred duplicator-pattern ops on the same source column don't collide.
//
// asName uses temporaryNameRebase(scope, column.Name) so a source column
// whose physical name is itself a `_pgroll_new_<base>_<otherScope>` left
// over from a prior deferred migration produces a single-prefixed
// `_pgroll_new_<base>_<scope>` rather than the double-prefixed
// `_pgroll_new__pgroll_new_<base>_<otherScope>_<scope>`.
func NewColumnDuplicator(conn db.DB, scope string, table *schema.Table, columns ...*schema.Column) *duplicator {
	cols := make(map[string]*columnToDuplicate, len(columns))
	columnOrder := make([]string, 0, len(columns))
	for _, column := range columns {
		cols[column.Name] = &columnToDuplicate{
			column:   column,
			asName:   temporaryNameRebase(scope, column.Name),
			withType: column.Type,
		}
		columnOrder = append(columnOrder, column.Name)
	}
	return &duplicator{
		id:    fmt.Sprintf("duplicate_%s_%s", table.Name, strings.Join(columnOrder, "_")),
		scope: scope,
		stmtBuilder: &duplicatorStmtBuilder{
			scope: scope,
			table: table,
		},
		conn:              conn,
		columns:           cols,
		columnOrder:       columnOrder,
		withoutConstraint: make([]string, 0),
	}
}

// temporaryNameRebase produces a temp name for a column when the column's
// current physical name might already be a temp from an earlier deferred
// migration. For a fresh user-facing name like "review" it returns
// `_pgroll_new_review_<scope>`. For an already-temp name like
// `_pgroll_new_review_<otherScope>` it strips the prior prefix+scope and
// re-applies the new scope, returning `_pgroll_new_review_<scope>` rather
// than nesting the temp prefix.
func temporaryNameRebase(scope, name string) string {
	base := name
	if strings.HasPrefix(base, temporaryPrefix) {
		rest := strings.TrimPrefix(base, temporaryPrefix)
		// Strip a trailing `_<MigrationScopeLength hex chars>` if present.
		if len(rest) > MigrationScopeLength+1 {
			tail := rest[len(rest)-MigrationScopeLength-1:]
			if tail[0] == '_' && isHexLower(tail[1:]) {
				rest = rest[:len(rest)-MigrationScopeLength-1]
			}
		}
		base = rest
	}
	return TemporaryName(scope, base)
}

func (d *duplicator) ID() string { return d.id }

// WithType sets the type of the new column.
func (d *duplicator) WithType(columnName, t string) *duplicator {
	d.columns[columnName].withType = t
	return d
}

// WithoutConstraint excludes a constraint from being duplicated.
func (d *duplicator) WithoutConstraint(c string) *duplicator {
	d.withoutConstraint = append(d.withoutConstraint, c)
	return d
}

// WithoutNotNull excludes the NOT NULL constraint from being duplicated.
func (d *duplicator) WithoutNotNull(columnName string) *duplicator {
	d.columns[columnName].withoutNotNull = true
	return d
}

// WithName sets the name of the new column.
func (d *duplicator) WithName(columnName, asName string) *duplicator {
	d.columns[columnName].asName = asName
	return d
}

// Duplicate duplicates a column in the table, including all constraints and
// comments.
func (d *duplicator) Execute(ctx context.Context) error {
	colNames := make([]string, 0, len(d.columns))
	// Iterate columns in the order they were provided rather than ranging
	// the map: the order in which the duplicate `_pgroll_new_*` columns are
	// added (ADD COLUMN) fixes their physical position (attnum) in the
	// completed table — completion only drops the originals and renames the
	// duplicates, which never changes attnum. Ranging Go's map randomized
	// that order, so a recreated table's column order was non-deterministic
	// across applications. The replay (per-migration start+complete) and the
	// deferred train+seal paths share this code, so both could diverge — e.g.
	// a `create_constraint` unique over `[name, person_id]` sealed as either
	// `name, person_id` or `person_id, name`. See ENG-6193.
	for _, name := range d.columnOrder {
		c := d.columns[name]
		colNames = append(colNames, name)

		// Duplicate the column with the new type
		if sql := d.stmtBuilder.duplicateColumn(c.column, c.asName, c.withoutNotNull, c.withType); sql != "" {
			_, err := d.conn.ExecContext(ctx, sql)
			if err != nil {
				return err
			}
		}

		// Duplicate the column's default value
		if sql := d.stmtBuilder.duplicateDefault(c.column, c.asName); sql != "" {
			_, err := d.conn.ExecContext(ctx, sql)
			err = errorIgnoringErrorCode(err, dataTypeMismatchErrorCode)
			if err != nil {
				return err
			}
		}

		// Duplicate the column's comment
		if sql := d.stmtBuilder.duplicateComment(c.column, c.asName); sql != "" {
			_, err := d.conn.ExecContext(ctx, sql)
			if err != nil {
				return err
			}
		}
	}

	// Generate SQL to duplicate any check constraints on the columns. This may faile
	// if the check constraint is not valid for the new column type, in which case
	// the error is ignored.
	for _, sql := range d.stmtBuilder.duplicateCheckConstraints(d.withoutConstraint, colNames...) {
		_, err := d.conn.ExecContext(ctx, sql)
		err = errorIgnoringErrorCode(err, undefinedFunctionErrorCode)
		if err != nil {
			return err
		}
	}

	// Create indexes for unique constraints on the columns concurrently.
	// The index is converted into a unique constraint on migration completion.
	for _, uc := range d.stmtBuilder.table.UniqueConstraints {
		if slices.Contains(d.withoutConstraint, uc.Name) {
			continue
		}
		if duplicatedMember, constraintColumns := d.stmtBuilder.allConstraintColumns(uc.Columns, colNames...); duplicatedMember {
			action := NewCreateUniqueIndexConcurrentlyAction(d.conn, "", DuplicationName(d.scope, uc.Name), d.stmtBuilder.table.Name, constraintColumns...)
			if err := action.Execute(ctx); err != nil {
				return err
			}
		}
	}

	// Generate SQL to duplicate any foreign key constraints on the columns.
	// If the foreign key constraint is not valid for a new column type, the error is ignored.
	for _, sql := range d.stmtBuilder.duplicateForeignKeyConstraints(d.withoutConstraint, colNames...) {
		_, err := d.conn.ExecContext(ctx, sql)
		err = errorIgnoringErrorCode(err, dataTypeMismatchErrorCode)
		if err != nil {
			return err
		}
	}

	// Generate SQL to duplicate any indexes on the columns.
	for _, sql := range d.stmtBuilder.duplicateIndexes(d.withoutConstraint, colNames...) {
		if _, err := d.conn.ExecContext(ctx, sql); err != nil {
			return err
		}
	}

	return nil
}

func (d *duplicatorStmtBuilder) duplicateCheckConstraints(withoutConstraint []string, colNames ...string) []string {
	stmts := make([]string, 0, len(d.table.CheckConstraints))
	for _, cc := range d.table.CheckConstraints {
		if slices.Contains(withoutConstraint, cc.Name) || IsDuplicatedName(cc.Name) {
			continue
		}
		if duplicatedConstraintColumns := d.duplicatedConstraintColumns(cc.Columns, colNames...); len(duplicatedConstraintColumns) > 0 {
			sql := fmt.Sprintf("ALTER TABLE %s ADD ", pq.QuoteIdentifier(d.table.Name))
			writer := ConstraintSQLWriter{Name: DuplicationName(d.scope, cc.Name), SkipValidation: true}
			sql += writer.WriteCheck(rewriteCheckExpression(d.scope, cc.Definition, duplicatedConstraintColumns...), cc.NoInherit)
			stmts = append(stmts, sql)
		}
	}
	return stmts
}

func (d *duplicatorStmtBuilder) duplicateForeignKeyConstraints(withoutConstraint []string, colNames ...string) []string {
	stmts := make([]string, 0, len(d.table.ForeignKeys))
	for _, fk := range d.table.ForeignKeys {
		if slices.Contains(withoutConstraint, fk.Name) {
			continue
		}
		if duplicatedMember, constraintColumns := d.allConstraintColumns(fk.Columns, colNames...); duplicatedMember {
			sql := fmt.Sprintf("ALTER TABLE %s ADD ", pq.QuoteIdentifier(d.table.Name))
			writer := ConstraintSQLWriter{
				Name:    DuplicationName(d.scope, fk.Name),
				Columns: constraintColumns,
			}
			sql += writer.WriteForeignKey(
				fk.ReferencedTable,
				fk.ReferencedColumns,
				ForeignKeyAction(fk.OnDelete),
				ForeignKeyAction(fk.OnUpdate),
				fk.OnDeleteSetColumns,
				ForeignKeyMatchType(fk.MatchType),
			)
			stmts = append(stmts, sql)
		}
	}
	return stmts
}

func (d *duplicatorStmtBuilder) duplicateIndexes(withoutConstraint []string, colNames ...string) []string {
	stmts := make([]string, 0, len(d.table.Indexes))
	for _, idx := range d.table.Indexes {
		if slices.Contains(withoutConstraint, idx.Name) || IsDuplicatedName(idx.Name) {
			continue
		}
		if _, ok := d.table.UniqueConstraints[idx.Name]; ok && idx.Unique {
			// unique constraints are duplicated as unique indexes
			continue
		}

		if duplicatedMember, columns := d.allConstraintColumns(idx.Columns, colNames...); duplicatedMember {
			stmtFmt := "CREATE INDEX CONCURRENTLY %s ON %s"
			if idx.Unique {
				stmtFmt = "CREATE UNIQUE INDEX CONCURRENTLY %s ON %s"
			}
			stmt := fmt.Sprintf(stmtFmt, pq.QuoteIdentifier(DuplicationName(d.scope, idx.Name)), pq.QuoteIdentifier(d.table.Name))
			if idx.Method != "" {
				stmt += fmt.Sprintf(" USING %s", string(idx.Method))
			}

			stmt += fmt.Sprintf(" (%s)", strings.Join(quoteColumnNames(columns), ", "))

			if storageParamStart := strings.Index(idx.Definition, " WITH ("); storageParamStart != -1 {
				end := strings.Index(idx.Definition[storageParamStart:], ")")
				stmt += idx.Definition[storageParamStart : storageParamStart+end+1]
			}

			if idx.Predicate != nil {
				pred := strings.Replace(*idx.Predicate, strings.Join(idx.Columns, ", "), strings.Join(quoteColumnNames(columns), ", "), 1)
				stmt += fmt.Sprintf(" WHERE %s", pred)
			}

			stmts = append(stmts, stmt)
		}
	}
	return stmts
}

// columnIsDuplicated reports whether a constraint's column reference is among
// the columns this duplicator will copy onto a new temp column.
//
// The matching rules cover three schema-shape cases:
//
//  1. Fresh-from-pg_catalog: constraint Columns[] and column.Name are both the
//     user-facing key. duplicatedColumns also contains the user-facing key.
//     Direct match.
//
//  2. Cross-migration deferred batch: an earlier deferred migration's Start
//     rewrote in-memory `column.Name` to its own temp (e.g.
//     `_pgroll_new_review_<v15Scope>`). The constraint's Columns[] still
//     references the user-facing key. duplicatedColumns contains the prior
//     migration's temp. We need to duplicate — when the prior migration drains
//     it'll rename its temp back to the user-facing column, then this
//     migration's drain drops that column (taking the constraint with it),
//     so we must build our own copy on the new temp now.
//
//  3. Same-migration multi-sub-op: an earlier sub-op of the *same*
//     OpAlterColumn already created the constraint physically on
//     `_pgroll_new_<col>_<thisScope>`. column.Name is also `_pgroll_new_<col>_<thisScope>`.
//     The constraint already lives where this duplicator wants it — duplicating
//     would create a redundant copy that conflicts at rename time. Skip.
//
// The skip-rule is: if the column resolves to *this* scope's temp name, the
// constraint is already in the target slot. Any other resolved physical name
// (user-facing or another scope's temp) means duplicate.
func (d *duplicatorStmtBuilder) columnIsDuplicated(column string, duplicatedColumns []string) bool {
	if slices.Contains(duplicatedColumns, column) {
		return true
	}
	c := d.table.GetColumn(column)
	if c == nil {
		return false
	}
	if c.Name == temporaryNameRebase(d.scope, column) {
		return false
	}
	return slices.Contains(duplicatedColumns, c.Name)
}

// duplicatedConstraintColumns returns a new slice of constraint columns with
// the columns that are duplicated replaced with temporary names.
func (d *duplicatorStmtBuilder) duplicatedConstraintColumns(constraintColumns []string, duplicatedColumns ...string) []string {
	newConstraintColumns := make([]string, 0)
	for _, column := range constraintColumns {
		if d.columnIsDuplicated(column, duplicatedColumns) {
			newConstraintColumns = append(newConstraintColumns, column)
		}
	}
	return newConstraintColumns
}

// allConstraintColumns returns a new slice of constraint columns with the columns
// that are duplicated replaced with temporary names and a boolean indicating if
// any of the columns are duplicated.
func (d *duplicatorStmtBuilder) allConstraintColumns(constraintColumns []string, duplicatedColumns ...string) (bool, []string) {
	duplicatedMember := false
	newConstraintColumns := make([]string, len(constraintColumns))
	for i, column := range constraintColumns {
		if d.columnIsDuplicated(column, duplicatedColumns) {
			newConstraintColumns[i] = temporaryNameRebase(d.scope, column)
			duplicatedMember = true
		} else {
			newConstraintColumns[i] = column
		}
	}
	return duplicatedMember, newConstraintColumns
}

func (d *duplicatorStmtBuilder) duplicateColumn(
	column *schema.Column,
	asName string,
	withoutNotNull bool,
	withType string,
) string {
	const (
		cAlterTableSQL         = `ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s`
		cAddCheckConstraintSQL = `ALTER TABLE %s ADD CONSTRAINT %s %s NOT VALID`
	)

	// Generate SQL to duplicate the column's name and type
	sql := fmt.Sprintf(cAlterTableSQL,
		pq.QuoteIdentifier(d.table.Name),
		pq.QuoteIdentifier(asName),
		withType)

	// Generate SQL to add an unchecked NOT NULL constraint if the original column
	// is NOT NULL. The constraint will be validated on migration completion.
	if !column.Nullable && !withoutNotNull {
		constraintName := DuplicationName(d.scope, NotNullConstraintName(column.Name))
		if _, ok := d.table.CheckConstraints[constraintName]; ok {
			return sql // Skip if the constraint already exists
		}
		sql += fmt.Sprintf(
			"; "+cAddCheckConstraintSQL,
			pq.QuoteIdentifier(d.table.Name),
			pq.QuoteIdentifier(constraintName),
			fmt.Sprintf("CHECK (%s IS NOT NULL)", pq.QuoteIdentifier(asName)),
		)
		if d.table.CheckConstraints == nil {
			d.table.CheckConstraints = make(map[string]*schema.CheckConstraint)
		}
		d.table.CheckConstraints[constraintName] = &schema.CheckConstraint{
			Name:    constraintName,
			Columns: []string{asName},
		}
	}

	return sql
}

func (d *duplicatorStmtBuilder) duplicateDefault(column *schema.Column, asName string) string {
	if column.Default == nil {
		return ""
	}

	const cSetDefaultSQL = `ALTER TABLE %s ALTER COLUMN %s SET DEFAULT %s`

	// Generate SQL to duplicate any default value on the column. This may fail
	// if the default value is not valid for the new column type, in which case
	// the error is ignored.
	return fmt.Sprintf(cSetDefaultSQL, pq.QuoteIdentifier(d.table.Name), asName, *column.Default)
}

func (d *duplicatorStmtBuilder) duplicateComment(column *schema.Column, asName string) string {
	if column.Comment == "" {
		return ""
	}

	const cCommentOnColumnSQL = `COMMENT ON COLUMN %s.%s IS %s`

	// Generate SQL to duplicate the column's comment
	return fmt.Sprintf(
		cCommentOnColumnSQL,
		pq.QuoteIdentifier(d.table.Name),
		pq.QuoteIdentifier(asName),
		pq.QuoteLiteral(column.Comment),
	)
}

// DuplicationName returns the per-migration name of a duplicated constraint
// or index. The scope suffix prevents concurrently-deferred duplicator-pattern
// ops on the same source object from colliding.
func DuplicationName(scope, name string) string {
	return "_pgroll_dup_" + name + scopeSuffix(scope)
}

// IsDuplicatedName returns true if the name is a duplicated column name.
func IsDuplicatedName(name string) bool {
	return strings.HasPrefix(name, "_pgroll_dup_")
}

// StripDuplicationPrefix removes the duplication prefix and any
// MigrationScope suffix from a duplicated identifier.
func StripDuplicationPrefix(name string) string {
	stripped := strings.TrimPrefix(name, "_pgroll_dup_")
	// Trim a trailing "_<8 hex chars>" if present (the scope suffix shape
	// produced by DuplicationName); leave the name alone if it doesn't
	// match so legacy unscoped names round-trip cleanly.
	if len(stripped) > MigrationScopeLength+1 {
		tail := stripped[len(stripped)-MigrationScopeLength-1:]
		if tail[0] == '_' && isHexLower(tail[1:]) {
			return stripped[:len(stripped)-MigrationScopeLength-1]
		}
	}
	return stripped
}

func isHexLower(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func errorIgnoringErrorCode(err error, code pqerror.Code) error {
	pqErr := &pq.Error{}
	if ok := errors.As(err, &pqErr); ok {
		if pqErr.Code == code {
			return nil
		}
	}

	return err
}
