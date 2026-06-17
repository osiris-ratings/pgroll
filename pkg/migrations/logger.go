// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"fmt"
	"strings"
	"time"

	"github.com/pterm/pterm"
)

// Logger is responsible for logging all migration steps.
type Logger interface {
	LogMigrationStart(*Migration)
	LogMigrationComplete(*Migration)
	LogMigrationRollback(*Migration)
	LogMigrationRollbackComplete(*Migration)

	LogOperationStart(Operation)
	LogOperationComplete(Operation)
	LogOperationRollback(Operation)

	LogBackfillStart(table string)
	LogBackfillComplete(table string)
	LogBackfillProgress(table string, rowsProcessed, total int64, elapsed time.Duration)
	LogSchemaCreation(migration, schema string)
	LogSchemaDeletion(migration, schema string)

	LogLockTimeoutRetry(query string, attempt int, sleep, elapsed, budget time.Duration)
	LogLockTimeoutGiveUp(attempts int, elapsed time.Duration)
	LogLockTimeoutInterrupted(attempts int, elapsed time.Duration)

	LogRollbackOnFailure(reason string)

	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}

type migrationLogger struct {
	logger pterm.Logger
}

type noopLogger struct{}

func NewLogger() Logger {
	return &migrationLogger{logger: pterm.DefaultLogger}
}

func NewNoopLogger() Logger {
	return &noopLogger{}
}

func (l *migrationLogger) LogMigrationStart(m *Migration) {
	l.logger.Info("starting migration", l.logger.Args(
		"name", m.Name,
		"operation_count", len(m.Operations),
	))
}

func (l *migrationLogger) LogMigrationComplete(m *Migration) {
	l.logger.Info("completing migration", l.logger.Args(
		"name", m.Name,
		"operation_count", len(m.Operations),
	))
}

func (l *migrationLogger) LogMigrationRollback(m *Migration) {
	l.logger.Info("rolling back migration", l.logger.Args(
		"name", m.Name,
		"operation_count", len(m.Operations),
	))
}

func (l *migrationLogger) LogMigrationRollbackComplete(m *Migration) {
	l.logger.Info("rolled back migration", l.logger.Args(
		"name", m.Name,
		"operation_count", len(m.Operations),
	))
}

func (l *migrationLogger) LogBackfillStart(table string) {
	l.logger.Info("backfilling started", l.logger.Args("table", table))
}

func (l *migrationLogger) LogBackfillComplete(table string) {
	l.logger.Info("backfilling completed", l.logger.Args("table", table))
}

func (l *migrationLogger) LogBackfillProgress(table string, rowsProcessed, total int64, elapsed time.Duration) {
	if total > 0 {
		l.logger.Info("backfill progress", l.logger.Args(
			"table", table,
			"rows_processed", rowsProcessed,
			"total_estimate", total,
			"percent", fmt.Sprintf("%.1f", 100*float64(rowsProcessed)/float64(total)),
			"elapsed", elapsed.Round(time.Second).String(),
		))
		return
	}
	l.logger.Info("backfill progress", l.logger.Args(
		"table", table,
		"rows_processed", rowsProcessed,
		"elapsed", elapsed.Round(time.Second).String(),
	))
}

func (l *migrationLogger) LogLockTimeoutRetry(query string, attempt int, sleep, elapsed, budget time.Duration) {
	l.logger.Warn("lock_timeout hit; retrying", l.logger.Args(
		"query", truncateQuery(query),
		"attempt", attempt,
		"sleep", sleep.Round(time.Millisecond).String(),
		"elapsed", elapsed.Round(time.Millisecond).String(),
		"retry_budget", budget.String(),
	))
}

func (l *migrationLogger) LogLockTimeoutGiveUp(attempts int, elapsed time.Duration) {
	l.logger.Error("lock_timeout retry budget exhausted; giving up", l.logger.Args(
		"attempts", attempts,
		"elapsed", elapsed.Round(time.Millisecond).String(),
	))
}

func (l *migrationLogger) LogLockTimeoutInterrupted(attempts int, elapsed time.Duration) {
	l.logger.Warn("interrupted while waiting on lock; cleaning up", l.logger.Args(
		"attempts", attempts,
		"elapsed", elapsed.Round(time.Millisecond).String(),
	))
}

func (l *migrationLogger) LogRollbackOnFailure(reason string) {
	l.logger.Warn("rolling back partial migration after failure", l.logger.Args("reason", reason))
}

func (l *migrationLogger) LogSchemaCreation(migration, schema string) {
	l.logger.Info("created versioned schema for migration", l.logger.Args("migration", migration, "schema_name", schema))
}

func (l *migrationLogger) LogSchemaDeletion(migration, schema string) {
	l.logger.Info("dropped versioned schema for migration", l.logger.Args("migration", migration, "schema_name", schema))
}

func (l migrationLogger) LogOperationStart(op Operation) {
	l.logger.Info("starting operation", l.logger.Args(l.extractOpArgs(op)...))
}

func (l migrationLogger) LogOperationComplete(op Operation) {
	l.logger.Info("completing operation", l.logger.Args(l.extractOpArgs(op)...))
}

func (l migrationLogger) LogOperationRollback(op Operation) {
	l.logger.Info("rolling back operation", l.logger.Args(l.extractOpArgs(op)...))
}

func (l migrationLogger) Info(msg string, args ...any) {
	l.logger.Info(msg, l.logger.Args(args...))
}

func (l migrationLogger) Warn(msg string, args ...any) {
	l.logger.Warn(msg, l.logger.Args(args...))
}

func (l migrationLogger) extractOpArgs(op Operation) []any {
	switch o := op.(type) {
	case *OpAddColumn:
		return []any{
			"operation", OpNameAddColumn,
			"name", o.Column.Name,
			"type", o.Column.Type,
			"table", o.Table,
			"nullable", o.Column.Nullable,
			"unique", o.Column.Unique,
		}
	case *OpAlterColumn:
		return []any{
			"operation", OpNameAlterColumn,
			"column", o.Column,
			"table", o.Table,
		}
	case *OpChangeType:
		return []any{
			"operation", OpNameAlterColumn,
			"column", o.Column,
			"table", o.Table,
			"type", o.Type,
		}
	case *OpCreateConstraint:
		return []any{
			"operation", OpCreateConstraintName,
			"table", o.Table,
			"name", o.Name,
			"type", o.Type,
		}
	case *OpCreateIndex:
		return []any{
			"operation", OpNameCreateIndex,
			"name", o.Name,
			"table", o.Table,
			"index_type", o.Method,
		}
	case *OpCreateTable:
		return []any{
			"operation", OpNameCreateTable,
			"name", o.Name,
			"columns", getColumnNames(o.Columns),
			"comment", o.Comment,
			"constraints", getConstraintNames(o.Constraints),
		}
	case *OpDropColumn:
		return []any{
			"operation", OpNameDropColumn,
			"column", o.Column,
			"table", o.Table,
		}
	case *OpDropConstraint:
		return []any{
			"operation", OpNameDropConstraint,
			"constraint", o.Name,
			"table", o.Table,
		}
	case *OpDropIndex:
		return []any{
			"operation", OpNameDropIndex,
			"name", o.Name,
		}

	case *OpDropMultiColumnConstraint:
		return []any{
			"operation", OpNameDropMultiColumnConstraint,
			"constraint", o.Name,
			"table", o.Table,
		}
	case *OpDropTable:
		return []any{
			"operation", OpNameDropTable,
			"name", o.Name,
		}
	case *OpRawSQL:
		return []any{
			"operation", OpRawSQLName,
			"up_expression", o.Up,
			"down_expression", o.Down,
		}
	case *OpRenameColumn:
		return []any{
			"operation", OpNameRenameColumn,
			"from", o.From,
			"to", o.To,
			"table", o.Table,
		}
	case *OpRenameConstraint:
		return []any{
			"operation", OpNameRenameConstraint,
			"from", o.From,
			"to", o.To,
			"table", o.Table,
		}
	case *OpRenameTable:
		return []any{
			"operation", OpNameRenameTable,
			"from", o.From,
			"to", o.To,
		}
	case *OpSetCheckConstraint:
		return []any{
			"operation", OpNameAlterColumn,
			"column", o.Column,
			"table", o.Table,
			"constraint", o.Check.Name,
			"check", o.Check.Constraint,
		}
	case *OpSetComment:
		args := []any{
			"operation", OpNameAlterColumn,
			"column", o.Column,
			"table", o.Table,
		}
		if o.Comment != nil {
			args = append(args, "comment", *o.Comment)
		}
		return args
	case *OpSetDefault:
		args := []any{
			"operation", OpNameAlterColumn,
			"table", o.Table,
			"column", o.Column,
		}
		if o.Default != nil {
			args = append(args, "default", *o.Default)
		}
		return args
	case *OpSetForeignKey:
		return []any{
			"operation", OpNameAlterColumn,
			"table", o.Table,
			"column", o.Column,
			"references", o.References.Name,
		}
	case *OpSetNotNull:
		return []any{
			"operation", OpNameAlterColumn,
			"column", o.Column,
			"table", o.Table,
			"nullable", false,
		}
	case *OpSetReplicaIdentity:
		return []any{
			"operation", OpNameSetReplicaIdentity,
			"table", o.Table,
			"identity_type", o.Identity.Type,
			"identity_index", o.Identity.Index,
		}
	case *OpSetUnique:
		return []any{
			"operation", OpNameAlterColumn,
			"column", o.Column,
			"table", o.Table,
			"unique", true,
		}
	default:
		return []any{}
	}
}

func getColumnNames(cols []Column) []string {
	columns := make([]string, len(cols))
	for i, c := range cols {
		columns[i] = c.Name
	}
	return columns
}

func getConstraintNames(cons []Constraint) []string {
	constraints := make([]string, len(cons))
	for i, c := range cons {
		constraints[i] = c.Name
	}
	return constraints
}

func (l *noopLogger) LogMigrationStart(m *Migration)            {}
func (l *noopLogger) LogMigrationComplete(m *Migration)         {}
func (l *noopLogger) LogMigrationRollback(m *Migration)         {}
func (l *noopLogger) LogMigrationRollbackComplete(m *Migration) {}
func (l *noopLogger) LogBackfillStart(table string)             {}
func (l *noopLogger) LogBackfillComplete(table string)          {}
func (l *noopLogger) LogBackfillProgress(table string, rowsProcessed, total int64, elapsed time.Duration) {
}
func (l *noopLogger) LogSchemaCreation(migration, schema string) {}
func (l *noopLogger) LogSchemaDeletion(migration, schema string) {}
func (l *noopLogger) LogOperationStart(op Operation)             {}
func (l *noopLogger) LogOperationComplete(op Operation)          {}
func (l *noopLogger) LogOperationRollback(op Operation)          {}
func (l *noopLogger) LogLockTimeoutRetry(query string, attempt int, sleep, elapsed, budget time.Duration) {
}
func (l *noopLogger) LogLockTimeoutGiveUp(attempts int, elapsed time.Duration)      {}
func (l *noopLogger) LogLockTimeoutInterrupted(attempts int, elapsed time.Duration) {}
func (l *noopLogger) LogRollbackOnFailure(reason string)                            {}
func (l *noopLogger) Info(msg string, args ...any)                                  {}
func (l *noopLogger) Warn(msg string, args ...any)                                  {}

// truncateQuery returns a single-line, length-bounded form of a SQL query
// suitable for logging without flooding the terminal.
func truncateQuery(q string) string {
	const maxLen = 200
	q = strings.Join(strings.Fields(q), " ")
	if len(q) > maxLen {
		return q[:maxLen] + "..."
	}
	return q
}
