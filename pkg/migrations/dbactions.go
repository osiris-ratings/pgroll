// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/xataio/pgroll/pkg/db"
)

// DBAction is an interface for common database actions
// pgroll runs during migrations.
type DBAction interface {
	ID() string
	Execute(context.Context) error
}

type addColumnAction struct {
	conn                  db.DB
	id                    string
	table                 string
	column                Column
	withPK                bool
	notNullConstraintName string
}

// NewAddColumnAction adds a column to a table. notNullConstraintName, if
// non-empty and the column is NOT NULL, names the inline NOT NULL constraint
// explicitly so PostgreSQL 17+ doesn't auto-derive a name from the (possibly
// temporary) column name in play at ADD COLUMN time.
func NewAddColumnAction(conn db.DB, table string, c Column, withPK bool, notNullConstraintName string) *addColumnAction {
	return &addColumnAction{
		conn:                  conn,
		id:                    fmt.Sprintf("add_column_%s_%s", table, c.Name),
		table:                 table,
		column:                c,
		withPK:                withPK,
		notNullConstraintName: notNullConstraintName,
	}
}

func (a *addColumnAction) ID() string { return a.id }

func (a *addColumnAction) Execute(ctx context.Context) error {
	colSQL, err := ColumnSQLWriter{
		WithPK:                a.withPK,
		NotNullConstraintName: a.notNullConstraintName,
	}.Write(a.column)
	if err != nil {
		return err
	}

	_, err = a.conn.ExecContext(ctx, fmt.Sprintf(
		"ALTER TABLE %s ADD COLUMN %s",
		pq.QuoteIdentifier(a.table),
		colSQL,
	))
	return err
}

// dropColumnAction is a DBAction that drops one or more columns from a table.
type dropColumnAction struct {
	conn    db.DB
	id      string
	table   string
	columns []string
}

func NewDropColumnAction(conn db.DB, table string, columns ...string) *dropColumnAction {
	return &dropColumnAction{
		conn:    conn,
		id:      fmt.Sprintf("drop_column_%s_%s", table, strings.Join(columns, "_")),
		table:   table,
		columns: columns,
	}
}

func (a *dropColumnAction) ID() string { return a.id }

func (a *dropColumnAction) Execute(ctx context.Context) error {
	_, err := a.conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE IF EXISTS %s %s",
		pq.QuoteIdentifier(a.table),
		a.dropMultipleColumns()))
	return err
}

func (a *dropColumnAction) dropMultipleColumns() string {
	cols := make([]string, len(a.columns))
	for i, col := range a.columns {
		cols[i] = "DROP COLUMN IF EXISTS " + pq.QuoteIdentifier(col)
	}
	return strings.Join(cols, ", ")
}

// renameTableAction is a DBAction that renames a table.
type renameTableAction struct {
	conn db.DB
	id   string
	from string
	to   string
}

func NewRenameTableAction(conn db.DB, from, to string) *renameTableAction {
	return &renameTableAction{
		conn: conn,
		id:   fmt.Sprintf("rename_table_%s_to_%s", from, to),
		from: from,
		to:   to,
	}
}

func (a *renameTableAction) ID() string { return a.id }

func (a *renameTableAction) Execute(ctx context.Context) error {
	// Already idempotent on re-run: the IF EXISTS guards the source table, so a
	// rename a previous interrupted `complete` already applied (source gone)
	// no-ops cleanly.
	_, err := a.conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE IF EXISTS %s RENAME TO %s",
		pq.QuoteIdentifier(a.from),
		pq.QuoteIdentifier(a.to)))
	return err
}

// renameColumnAction is a DBAction that renames a column in a table.
type renameColumnAction struct {
	conn  db.DB
	id    string
	table string
	from  string
	to    string
}

func NewRenameColumnAction(conn db.DB, table, from, to string) *renameColumnAction {
	return &renameColumnAction{
		conn:  conn,
		id:    fmt.Sprintf("rename_column_%s_%s_to_%s", table, from, to),
		table: table,
		from:  from,
		to:    to,
	}
}

func (a *renameColumnAction) ID() string { return a.id }

func (a *renameColumnAction) Execute(ctx context.Context) error {
	// Idempotent re-run guard (see catalog.go): RENAME COLUMN has no native
	// column-level IF EXISTS, so a re-run after an interrupted `complete` would
	// fail because the source column is already renamed. If the source is gone
	// but the target is present, the rename already happened — no-op. Every
	// other case falls through to behave exactly as before (including erroring
	// on a missing source column when the table exists).
	if a.alreadyRenamed(ctx) {
		return nil
	}

	_, err := a.conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE IF EXISTS %s RENAME COLUMN %s TO %s",
		pq.QuoteIdentifier(a.table),
		pq.QuoteIdentifier(a.from),
		pq.QuoteIdentifier(a.to)))
	return err
}

// alreadyRenamed reports whether the source column is gone while the target is
// present — the signature of a rename a previous interrupted `complete` already
// applied. Conservative: any inability to determine this (FakeDB, query error)
// returns false so the caller proceeds with the normal rename.
func (a *renameColumnAction) alreadyRenamed(ctx context.Context) bool {
	fromExists, known, err := columnExists(ctx, a.conn, a.table, a.from)
	if err != nil || !known || fromExists {
		return false
	}
	toExists, _, err := columnExists(ctx, a.conn, a.table, a.to)
	return err == nil && toExists
}

// renameConstraintAction is a DBAction that renames a constraint in a table.
type renameConstraintAction struct {
	conn  db.DB
	id    string
	table string
	from  string
	to    string
}

func NewRenameConstraintAction(conn db.DB, table, from, to string) *renameConstraintAction {
	return &renameConstraintAction{
		conn:  conn,
		id:    fmt.Sprintf("rename_constraint_%s_%s_to_%s", table, from, to),
		table: table,
		from:  from,
		to:    to,
	}
}

func (a *renameConstraintAction) ID() string { return a.id }

func (a *renameConstraintAction) Execute(ctx context.Context) error {
	// Idempotent re-run guard (see catalog.go): RENAME CONSTRAINT has no native
	// constraint-level IF EXISTS. If the source constraint is gone but the
	// target is present, a previous interrupted `complete` already applied this
	// rename — no-op. Every other case falls through to the original behavior.
	if a.alreadyRenamed(ctx) {
		return nil
	}

	_, err := a.conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE IF EXISTS %s RENAME CONSTRAINT %s TO %s",
		pq.QuoteIdentifier(a.table),
		pq.QuoteIdentifier(a.from),
		pq.QuoteIdentifier(a.to)))
	return err
}

func (a *renameConstraintAction) alreadyRenamed(ctx context.Context) bool {
	fromExists, known, err := constraintExists(ctx, a.conn, a.table, a.from)
	if err != nil || !known || fromExists {
		return false
	}
	toExists, _, err := constraintExists(ctx, a.conn, a.table, a.to)
	return err == nil && toExists
}

type addConstraintUsingUniqueIndexAction struct {
	conn       db.DB
	id         string
	table      string
	constraint string
	indexName  string
}

func NewAddConstraintUsingUniqueIndex(conn db.DB, table, constraint, indexName string) *addConstraintUsingUniqueIndexAction {
	return &addConstraintUsingUniqueIndexAction{
		conn:       conn,
		id:         fmt.Sprintf("add_constraint_using_unique_index_%s_%s", table, constraint),
		table:      table,
		constraint: constraint,
		indexName:  indexName,
	}
}

func (a *addConstraintUsingUniqueIndexAction) ID() string { return a.id }

func (a *addConstraintUsingUniqueIndexAction) Execute(ctx context.Context) error {
	// Idempotent re-run guard (see catalog.go): ADD CONSTRAINT has no native
	// IF NOT EXISTS, so skip if a previous interrupted `complete` already
	// promoted the unique index to a constraint of this name.
	exists, known, err := constraintExists(ctx, a.conn, a.table, a.constraint)
	if err != nil {
		return err
	}
	if known && exists {
		return nil
	}

	_, err = a.conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE IF EXISTS %s ADD CONSTRAINT %s UNIQUE USING INDEX %s",
		pq.QuoteIdentifier(a.table),
		pq.QuoteIdentifier(a.constraint),
		pq.QuoteIdentifier(a.indexName)))
	return err
}

type addPrimaryKeyAction struct {
	conn      db.DB
	id        string
	table     string
	indexName string
}

func NewAddPrimaryKeyAction(conn db.DB, table, indexName string) *addPrimaryKeyAction {
	return &addPrimaryKeyAction{
		conn:      conn,
		id:        fmt.Sprintf("add_pk_%s_%s", table, indexName),
		table:     table,
		indexName: indexName,
	}
}

func (a *addPrimaryKeyAction) ID() string { return a.id }

func (a *addPrimaryKeyAction) Execute(ctx context.Context) error {
	// Idempotent re-run guard (see catalog.go): a table can only have one
	// primary key and ADD PRIMARY KEY has no native IF NOT EXISTS, so skip if a
	// previous interrupted `complete` already added it.
	exists, known, err := primaryKeyExists(ctx, a.conn, a.table)
	if err != nil {
		return err
	}
	if known && exists {
		return nil
	}

	_, err = a.conn.ExecContext(ctx, fmt.Sprintf(
		"ALTER TABLE %s ADD PRIMARY KEY USING INDEX %s",
		pq.QuoteIdentifier(a.table),
		pq.QuoteIdentifier(a.indexName),
	))
	return err
}

// dropFunctionAction is a DBAction that drops a function and all of its dependencies (cascade).
type dropFunctionAction struct {
	conn      db.DB
	id        string
	functions []string
}

func NewDropFunctionAction(conn db.DB, functions ...string) *dropFunctionAction {
	return &dropFunctionAction{
		conn:      conn,
		id:        fmt.Sprintf("drop_function_%s", strings.Join(functions, "_")),
		functions: functions,
	}
}

func (a *dropFunctionAction) ID() string { return a.id }

func (a *dropFunctionAction) Execute(ctx context.Context) error {
	functions := make([]string, len(a.functions))
	for idx, fn := range a.functions {
		functions[idx] = pq.QuoteIdentifier(fn)
	}
	_, err := a.conn.ExecContext(ctx, fmt.Sprintf("DROP FUNCTION IF EXISTS %s CASCADE",
		strings.Join(functions, ",")))
	return err
}

type createIndexConcurrentlyAction struct {
	conn              db.DB
	id                string
	table             string
	name              string
	method            string
	unique            bool
	columns           []IndexField
	storageParameters string
	predicate         string
}

func NewCreateIndexConcurrentlyAction(conn db.DB, table, name, method string, unique bool, columns []IndexField, storageParameters, predicate string) *createIndexConcurrentlyAction {
	return &createIndexConcurrentlyAction{
		conn:              conn,
		id:                fmt.Sprintf("create_index_concurrently_%s_%s", table, name),
		table:             table,
		name:              name,
		method:            method,
		unique:            unique,
		columns:           columns,
		storageParameters: storageParameters,
		predicate:         predicate,
	}
}

func (a *createIndexConcurrentlyAction) ID() string { return a.id }

func (a *createIndexConcurrentlyAction) Execute(ctx context.Context) error {
	return buildIndexConcurrently(ctx, a.conn, a.name, pq.QuoteIdentifier(a.name), a.buildCreateIndexSQL())
}

// buildIndexConcurrently runs a CREATE [UNIQUE] INDEX CONCURRENTLY ... IF NOT
// EXISTS statement to completion, retrying failed builds until the
// connection's lock-retry budget is exhausted.
//
// Two behaviors here are load-bearing (ENG-6174):
//
//   - A failed build leaves an INVALID index behind, and IF NOT EXISTS
//     silently no-ops against it — without healing, every retry (including
//     the ones inside RDB.ExecContext) "succeeds" without building anything,
//     so the retry budget never engages. Invalid leftovers are dropped before
//     the first attempt and between attempts.
//
//   - The aggressive session lock_timeout exists to keep strong-lock DDL from
//     stalling application traffic while queued. CIC holds only locks that
//     block no traffic (ShareUpdateExclusive on the table, virtualxid waits
//     on concurrent transactions), so for the duration of the build the
//     timeout is raised to the retry budget: one productive wait instead of
//     create/drop churn at every timeout.
func buildIndexConcurrently(ctx context.Context, conn db.DB, indexName, quotedQualifiedIndexName, createSQL string) error {
	budget := lockRetryBudget(conn)
	deadline := time.Now().Add(budget)

	if err := dropIndexIfInvalid(ctx, conn, quotedQualifiedIndexName); err != nil {
		return err
	}

	if budget > 0 {
		restore, err := raiseLockTimeout(ctx, conn, budget)
		if err != nil {
			return err
		}
		defer restore()
	}

	for attempt := 1; ; attempt++ {
		if _, err := conn.ExecContext(ctx, createSQL); err != nil {
			return fmt.Errorf("failed to create index %q: %w%s",
				indexName, err, snapshotHolderDiagnostics(ctx, conn))
		}

		if err := waitForIndexBuild(ctx, conn, quotedQualifiedIndexName); err != nil {
			return err
		}

		valid, err := isIndexValid(ctx, conn, quotedQualifiedIndexName)
		if err != nil {
			return err
		}
		if valid {
			return nil
		}

		// The build failed and left an INVALID index; drop it so the next
		// attempt's IF NOT EXISTS cannot no-op against it.
		if err := dropIndexIfInvalid(ctx, conn, quotedQualifiedIndexName); err != nil {
			return err
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("failed to create index %q: %d attempt(s) over %s all ended INVALID%s",
				indexName, attempt, budget, snapshotHolderDiagnostics(ctx, conn))
		}
	}
}

// lockRetryBudget mirrors the retry budget of the underlying connection when
// it is an RDB; plain connections get the package default.
func lockRetryBudget(conn db.DB) time.Duration {
	if rdb, ok := conn.(*db.RDB); ok {
		return rdb.RetryBudget()
	}
	return db.DefaultLockRetryTimeout
}

// dropIndexIfInvalid drops the named index iff it exists and is INVALID (the
// leftover of a failed CREATE INDEX CONCURRENTLY). A valid index is left
// alone so the IF NOT EXISTS in the build still treats it as success.
func dropIndexIfInvalid(ctx context.Context, conn db.DB, quotedQualifiedIndexName string) error {
	rows, err := conn.QueryContext(ctx, `SELECT NOT indisvalid
		FROM pg_catalog.pg_index
		WHERE indexrelid = pg_catalog.to_regclass($1)`,
		quotedQualifiedIndexName)
	if err != nil {
		return fmt.Errorf("checking index %q for INVALID leftover: %w", quotedQualifiedIndexName, err)
	}
	if rows == nil {
		// FakeDB used by unit tests: nothing to heal.
		return nil
	}
	defer rows.Close()

	invalid := false
	if err := db.ScanFirstValue(rows, &invalid); err != nil {
		return fmt.Errorf("scanning INVALID check for index %q: %w", quotedQualifiedIndexName, err)
	}
	if !invalid {
		return nil
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("DROP INDEX IF EXISTS %s", quotedQualifiedIndexName)); err != nil {
		return fmt.Errorf("failed to drop invalid index %q: %w", quotedQualifiedIndexName, err)
	}
	return nil
}

// raiseLockTimeout sets the session lock_timeout to the given duration and
// returns a function that restores the previous value. Relies on the
// migration session being a single dedicated connection — the same assumption
// the session-level SETs in roll.New already make.
func raiseLockTimeout(ctx context.Context, conn db.DB, d time.Duration) (func(), error) {
	rows, err := conn.QueryContext(ctx, "SHOW lock_timeout")
	if err != nil {
		return nil, fmt.Errorf("reading current lock_timeout: %w", err)
	}
	if rows == nil {
		// FakeDB used by unit tests: leave the timeout alone.
		return func() {}, nil
	}
	prev := ""
	scanErr := db.ScanFirstValue(rows, &prev)
	rows.Close()
	if scanErr != nil || prev == "" {
		return func() {}, nil
	}

	if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET lock_timeout TO '%dms'", d.Milliseconds())); err != nil {
		return nil, fmt.Errorf("raising lock_timeout for index build: %w", err)
	}
	return func() {
		// Best effort: restore must run even when ctx is already canceled,
		// or the lowered timeout leaks into subsequent strong-lock DDL.
		_, _ = conn.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf("SET lock_timeout TO '%s'", prev))
	}, nil
}

// waitForIndexBuild polls until no concurrent build is in progress for the
// index.
func waitForIndexBuild(ctx context.Context, conn db.DB, quotedQualifiedIndexName string) error {
	inProgress, err := isIndexInProgress(ctx, conn, quotedQualifiedIndexName)
	if err != nil {
		return err
	}
	if !inProgress {
		return nil
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for inProgress {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
		inProgress, err = isIndexInProgress(ctx, conn, quotedQualifiedIndexName)
		if err != nil {
			return err
		}
	}
	return nil
}

// snapshotHolderDiagnostics reports, best effort, the oldest transactions
// holding snapshots — what a concurrent index build must out-wait, regardless
// of which tables they touch. Returns "" when nothing useful is available;
// diagnostics must never mask the original error.
func snapshotHolderDiagnostics(ctx context.Context, conn db.DB) string {
	rows, err := conn.QueryContext(ctx, `SELECT
			a.pid,
			coalesce(nullif(a.application_name, ''), a.backend_type) AS source,
			a.state,
			coalesce((clock_timestamp() - a.xact_start)::text, '-') AS xact_age,
			left(coalesce(a.query, ''), 120) AS query
		FROM pg_catalog.pg_stat_activity a
		WHERE a.backend_xmin IS NOT NULL
			AND a.pid <> pg_catalog.pg_backend_pid()
			AND a.backend_type = 'client backend'
		ORDER BY a.xact_start
		LIMIT 5`)
	if err != nil || rows == nil {
		return ""
	}
	defer rows.Close()

	var sb strings.Builder
	for rows.Next() {
		var (
			pid                       int
			source, state, age, query string
		)
		if rows.Scan(&pid, &source, &state, &age, &query) != nil {
			return ""
		}
		fmt.Fprintf(&sb, "\n  pid=%d source=%s state=%q xact_age=%s query=%q", pid, source, state, age, query)
	}
	if rows.Err() != nil || sb.Len() == 0 {
		return ""
	}
	return "\nconcurrent index builds must out-wait every transaction holding an older snapshot (any table); oldest snapshot holders:" + sb.String()
}

func (a *createIndexConcurrentlyAction) buildCreateIndexSQL() string {
	stmtFmt := "CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s"
	if a.unique {
		stmtFmt = "CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s"
	}
	stmt := fmt.Sprintf(stmtFmt,
		pq.QuoteIdentifier(a.name),
		pq.QuoteIdentifier(a.table))

	if a.method != "" {
		stmt += fmt.Sprintf(" USING %s", a.method)
	}

	colSQLs := make([]string, 0, len(a.columns))
	for _, settings := range a.columns {
		colSQL := pq.QuoteIdentifier(settings.Column)
		if settings.Collate != "" {
			colSQL += " COLLATE " + settings.Collate
		}
		if settings.Opclass != nil {
			colSQL += " " + settings.Opclass.Name
			if len(settings.Opclass.Params) > 0 {
				colSQL += " " + strings.Join(settings.Opclass.Params, ", ")
			}
		}
		if settings.Sort != "" {
			colSQL += " " + string(settings.Sort)
		}
		if settings.Nulls != nil {
			colSQL += " " + string(*settings.Nulls)
		}
		colSQLs = append(colSQLs, colSQL)
	}
	stmt += fmt.Sprintf(" (%s)", strings.Join(colSQLs, ", "))

	if a.storageParameters != "" {
		stmt += fmt.Sprintf(" WITH (%s)", a.storageParameters)
	}

	if a.predicate != "" {
		stmt += fmt.Sprintf(" WHERE %s", a.predicate)
	}

	return stmt
}

// commentColumnAction is a DBAction that adds a comment to a column in a table.
type commentColumnAction struct {
	conn    db.DB
	id      string
	table   string
	column  string
	comment *string
}

func NewCommentColumnAction(conn db.DB, table, column string, comment *string) *commentColumnAction {
	return &commentColumnAction{
		conn:    conn,
		id:      fmt.Sprintf("comment_column_%s_%s", table, column),
		table:   table,
		column:  column,
		comment: comment,
	}
}

func (a *commentColumnAction) ID() string { return a.id }

func (a *commentColumnAction) Execute(ctx context.Context) error {
	commentSQL := fmt.Sprintf("COMMENT ON COLUMN %s.%s IS %s",
		pq.QuoteIdentifier(a.table),
		pq.QuoteIdentifier(a.column),
		commentToSQL(a.comment))

	_, err := a.conn.ExecContext(ctx, commentSQL)
	return err
}

// commentTableAction is a DBAction that adds a comment to a table.
type commentTableAction struct {
	conn    db.DB
	id      string
	table   string
	comment *string
}

func NewCommentTableAction(conn db.DB, table string, comment *string) *commentTableAction {
	return &commentTableAction{
		conn:    conn,
		id:      fmt.Sprintf("comment_table_%s", table),
		table:   table,
		comment: comment,
	}
}

func (a *commentTableAction) ID() string { return a.id }

func (a *commentTableAction) Execute(ctx context.Context) error {
	commentSQL := fmt.Sprintf("COMMENT ON TABLE %s IS %s",
		pq.QuoteIdentifier(a.table),
		commentToSQL(a.comment))

	_, err := a.conn.ExecContext(ctx, commentSQL)
	return err
}

func commentToSQL(comment *string) string {
	if comment == nil {
		return "NULL"
	}
	return pq.QuoteLiteral(*comment)
}

type createUniqueIndexConcurrentlyAction struct {
	conn        db.DB
	id          string
	schemaName  string
	indexName   string
	tableName   string
	columnNames []string
}

func NewCreateUniqueIndexConcurrentlyAction(conn db.DB, schemaName, indexName, tableName string, columnNames ...string) *createUniqueIndexConcurrentlyAction {
	return &createUniqueIndexConcurrentlyAction{
		conn:        conn,
		id:          fmt.Sprintf("create_unique_index_concurrently_%s_%s", indexName, tableName),
		schemaName:  schemaName,
		indexName:   indexName,
		tableName:   tableName,
		columnNames: columnNames,
	}
}

func (a *createUniqueIndexConcurrentlyAction) ID() string { return a.id }

func (a *createUniqueIndexConcurrentlyAction) Execute(ctx context.Context) error {
	quotedQualifiedIndexName := pq.QuoteIdentifier(a.indexName)
	if a.schemaName != "" {
		quotedQualifiedIndexName = fmt.Sprintf("%s.%s", pq.QuoteIdentifier(a.schemaName), pq.QuoteIdentifier(a.indexName))
	}
	return buildIndexConcurrently(ctx, a.conn, a.indexName, quotedQualifiedIndexName, a.getCreateUniqueIndexConcurrentlySQL())
}

func (a *createUniqueIndexConcurrentlyAction) getCreateUniqueIndexConcurrentlySQL() string {
	// create unique index concurrently
	qualifiedTableName := pq.QuoteIdentifier(a.tableName)
	if a.schemaName != "" {
		qualifiedTableName = fmt.Sprintf("%s.%s", pq.QuoteIdentifier(a.schemaName), pq.QuoteIdentifier(a.tableName))
	}

	indexQuery := fmt.Sprintf(
		"CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s (%s)",
		pq.QuoteIdentifier(a.indexName),
		qualifiedTableName,
		strings.Join(quoteColumnNames(a.columnNames), ", "),
	)

	return indexQuery
}

// isIndexInProgress checks whether a concurrent index creation is still running.
func isIndexInProgress(ctx context.Context, conn db.DB, quotedQualifiedIndexName string) (bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT EXISTS(
			SELECT * FROM pg_catalog.pg_stat_progress_create_index
			WHERE index_relid = $1::regclass
			)`, quotedQualifiedIndexName)
	if err != nil {
		return false, fmt.Errorf("getting index in progress with name %q: %w", quotedQualifiedIndexName, err)
	}
	if rows == nil {
		// if rows == nil && err != nil, then it means we have queried a `FakeDB`.
		// In that case, we can safely return false.
		return false, nil
	}
	defer rows.Close()

	var isInProgress bool
	if err := db.ScanFirstValue(rows, &isInProgress); err != nil {
		return false, fmt.Errorf("scanning index in progress with name %q: %w", quotedQualifiedIndexName, err)
	}

	return isInProgress, nil
}

// isIndexValid checks whether an index is valid (not left invalid by a failed
// CREATE INDEX CONCURRENTLY).
func isIndexValid(ctx context.Context, conn db.DB, quotedQualifiedIndexName string) (bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT indisvalid
		FROM pg_catalog.pg_index
		WHERE indexrelid = $1::regclass`,
		quotedQualifiedIndexName)
	if err != nil {
		return false, fmt.Errorf("getting index with name %q: %w", quotedQualifiedIndexName, err)
	}
	if rows == nil {
		// if rows == nil && err != nil, then it means we have queried a fake db.
		// In that case, we can safely return true.
		return true, nil
	}
	defer rows.Close()

	var isValid bool
	if err := db.ScanFirstValue(rows, &isValid); err != nil {
		return false, fmt.Errorf("scanning index with name %q: %w", quotedQualifiedIndexName, err)
	}

	return isValid, nil
}

// createTableAction is a DBAction that creates a table.
type createTableAction struct {
	conn        db.DB
	id          string
	table       string
	columns     string
	constraints string
}

func NewCreateTableAction(conn db.DB, table, columns, constraints string) *createTableAction {
	return &createTableAction{
		conn:        conn,
		id:          fmt.Sprintf("create_table_%s", table),
		table:       table,
		columns:     columns,
		constraints: constraints,
	}
}

func (a *createTableAction) ID() string { return a.id }

func (a *createTableAction) Execute(ctx context.Context) error {
	_, err := a.conn.ExecContext(ctx, fmt.Sprintf("CREATE TABLE %s (%s %s)",
		pq.QuoteIdentifier(a.table),
		a.columns,
		a.constraints))
	return err
}

// dropIndexAction is a DBAction that drops an index.
type dropIndexAction struct {
	conn db.DB
	id   string
	name string
}

func NewDropIndexAction(conn db.DB, name string) *dropIndexAction {
	return &dropIndexAction{
		conn: conn,
		id:   fmt.Sprintf("drop_index_%s", name),
		name: name,
	}
}

func (a *dropIndexAction) ID() string { return a.id }

func (a *dropIndexAction) Execute(ctx context.Context) error {
	_, err := a.conn.ExecContext(ctx, fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS %s",
		pq.QuoteIdentifier(a.name)))
	return err
}

// DropTableAction is a DBAction that drops a table.
type DropTableAction struct {
	conn  db.DB
	id    string
	table string
}

func NewDropTableAction(conn db.DB, table string) *DropTableAction {
	return &DropTableAction{
		conn:  conn,
		id:    fmt.Sprintf("drop_table_%s", table),
		table: table,
	}
}

func (a *DropTableAction) ID() string { return a.id }

func (a *DropTableAction) Execute(ctx context.Context) error {
	_, err := a.conn.ExecContext(ctx, fmt.Sprintf("DROP TABLE IF EXISTS %s",
		pq.QuoteIdentifier(a.table)))
	return err
}

// validateConstraintAction is a DBAction that validates a constraint in a table.
type validateConstraintAction struct {
	conn       db.DB
	id         string
	table      string
	constraint string
}

func NewValidateConstraintAction(conn db.DB, table, constraint string) *validateConstraintAction {
	return &validateConstraintAction{
		conn:       conn,
		id:         fmt.Sprintf("validate_constraint_%s_%s", table, constraint),
		table:      table,
		constraint: constraint,
	}
}

func (a *validateConstraintAction) ID() string { return a.id }

func (a *validateConstraintAction) Execute(ctx context.Context) error {
	_, err := a.conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE IF EXISTS %s VALIDATE CONSTRAINT %s",
		pq.QuoteIdentifier(a.table),
		pq.QuoteIdentifier(a.constraint)))
	return err
}

// CreateCheckConstraintAction creates a check constraint on a table.
type CreateCheckConstraintAction struct {
	conn           db.DB
	id             string
	scope          string
	table          string
	columns        []string
	constraint     string
	check          string
	noInherit      bool
	skipValidation bool
}

func NewCreateCheckConstraintAction(conn db.DB, scope, table, constraint, check string, columns []string, noInherit, skipValidation bool) *CreateCheckConstraintAction {
	return &CreateCheckConstraintAction{
		conn:           conn,
		id:             fmt.Sprintf("create_check_constraint_%s_%s", table, constraint),
		scope:          scope,
		table:          table,
		columns:        columns,
		check:          check,
		constraint:     constraint,
		noInherit:      noInherit,
		skipValidation: skipValidation,
	}
}

func (a *CreateCheckConstraintAction) ID() string { return a.id }

func (a *CreateCheckConstraintAction) Execute(ctx context.Context) error {
	sql := fmt.Sprintf("ALTER TABLE %s ADD ", pq.QuoteIdentifier(a.table))

	writer := &ConstraintSQLWriter{
		Name:           a.constraint,
		SkipValidation: a.skipValidation,
	}
	sql += writer.WriteCheck(rewriteCheckExpression(a.scope, a.check, a.columns...), a.noInherit)
	_, err := a.conn.ExecContext(ctx, sql)
	return err
}

// In order for the `check` expression to be easy to write, migration authors specify
// the check expression as though it were being applied to the old column,
// On migration start, however, the check is actually applied to the new (temporary)
// column. This function naively rewrites the check expression to apply to the
// per-migration temp name (`_pgroll_new_<col>_<scope>`). Uses temporaryNameRebase
// so a `column` that's already a temp name from an earlier deferred migration
// rebases (strip-and-re-apply) instead of double-prefixing.
func rewriteCheckExpression(scope, check string, columns ...string) string {
	for _, col := range columns {
		check = strings.ReplaceAll(check, col, temporaryNameRebase(scope, col))
	}
	return check
}

// createFKConstraintAction is a DBAction that creates a new foreign key constraint
type createFKConstraintAction struct {
	conn              db.DB
	id                string
	table             string
	constraint        string
	columns           []string
	initiallyDeferred bool
	deferrable        bool
	reference         *TableForeignKeyReference
	skipValidation    bool
}

func NewCreateFKConstraintAction(conn db.DB, table, constraint string, columns []string, reference *TableForeignKeyReference, initiallyDeferred, deferrable, skipValidation bool) *createFKConstraintAction {
	return &createFKConstraintAction{
		conn:              conn,
		id:                fmt.Sprintf("create_fk_constraint_%s_%s", table, constraint),
		table:             table,
		constraint:        constraint,
		columns:           columns,
		reference:         reference,
		initiallyDeferred: initiallyDeferred,
		deferrable:        deferrable,
		skipValidation:    skipValidation,
	}
}

func (a *createFKConstraintAction) ID() string { return a.id }

func (a *createFKConstraintAction) Execute(ctx context.Context) error {
	sql := fmt.Sprintf("ALTER TABLE %s ADD ", pq.QuoteIdentifier(a.table))
	writer := &ConstraintSQLWriter{
		Name:              a.constraint,
		Columns:           a.columns,
		InitiallyDeferred: a.initiallyDeferred,
		Deferrable:        a.deferrable,
		SkipValidation:    a.skipValidation,
	}
	sql += writer.WriteForeignKey(
		a.reference.Table,
		a.reference.Columns,
		a.reference.OnDelete,
		a.reference.OnUpdate,
		a.reference.OnDeleteSetColumns,
		a.reference.MatchType,
	)

	_, err := a.conn.ExecContext(ctx, sql)
	return err
}

type alterSequenceOwnerAction struct {
	conn  db.DB
	id    string
	table string
	from  string
	to    string
}

func NewAlterSequenceOwnerAction(conn db.DB, table, from, to string) *alterSequenceOwnerAction {
	return &alterSequenceOwnerAction{
		conn:  conn,
		id:    fmt.Sprintf("alter_sequence_owner_%s_%s_to_%s", table, from, to),
		table: table,
		from:  from,
		to:    to,
	}
}

func (a *alterSequenceOwnerAction) ID() string { return a.id }

func (a *alterSequenceOwnerAction) Execute(ctx context.Context) error {
	sequence := getSequenceNameForColumn(ctx, a.conn, a.table, a.from)
	if sequence == "" {
		return nil
	}
	_, err := a.conn.ExecContext(ctx, fmt.Sprintf(
		"ALTER SEQUENCE IF EXISTS %s OWNED BY %s.%s",
		sequence,
		pq.QuoteIdentifier(a.table),
		pq.QuoteIdentifier(a.to),
	))

	return err
}

func getSequenceNameForColumn(ctx context.Context, conn db.DB, tableName, columnName string) string {
	var sequenceName string
	query := fmt.Sprintf(`
		SELECT pg_get_serial_sequence('%s', '%s')
	`, pq.QuoteIdentifier(tableName), columnName)
	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return ""
	}
	defer rows.Close()

	if err := db.ScanFirstValue(rows, &sequenceName); err != nil {
		return ""
	}

	return sequenceName
}

type dropConstraintAction struct {
	conn       db.DB
	id         string
	table      string
	constraint string
}

func NewDropConstraintAction(conn db.DB, table, constraint string) *dropConstraintAction {
	return &dropConstraintAction{
		conn:       conn,
		id:         fmt.Sprintf("drop_constraint_%s_%s", table, constraint),
		table:      table,
		constraint: constraint,
	}
}

func (a *dropConstraintAction) ID() string { return a.id }

func (a *dropConstraintAction) Execute(ctx context.Context) error {
	_, err := a.conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE IF EXISTS %s DROP CONSTRAINT IF EXISTS %s",
		pq.QuoteIdentifier(a.table),
		pq.QuoteIdentifier(a.constraint)))
	return err
}

type setNotNullAction struct {
	conn           db.DB
	id             string
	table          string
	column         string
	constraintName string
}

// NewSetNotNullAction sets a column NOT NULL. constraintName, if non-empty,
// renames the resulting NOT NULL constraint to that name on PostgreSQL 17+
// (where SET NOT NULL produces a real, auto-named pg_constraint row whose
// auto-derived name embeds whatever the column was called at action time).
// Pre-PG-17 there is no separate constraint and the rename step is a silent
// no-op. Pass CanonicalNotNullName(table, finalCol) to keep the permanent name
// matching what pg_dump treats as the default form.
func NewSetNotNullAction(conn db.DB, table, column, constraintName string) *setNotNullAction {
	return &setNotNullAction{
		conn:           conn,
		id:             fmt.Sprintf("set_not_null_%s_%s", table, column),
		table:          table,
		column:         column,
		constraintName: constraintName,
	}
}

func (a *setNotNullAction) ID() string { return a.id }

func (a *setNotNullAction) Execute(ctx context.Context) error {
	_, err := a.conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE IF EXISTS %s ALTER COLUMN %s SET NOT NULL",
		pq.QuoteIdentifier(a.table),
		pq.QuoteIdentifier(a.column)))
	if err != nil {
		return err
	}

	if a.constraintName == "" {
		return nil
	}

	return ensureNotNullConstraintName(ctx, a.conn, a.table, a.column, a.constraintName)
}

// ensureNotNullConstraintName renames the NOT NULL constraint on (table, column)
// to desired, if one exists and is not already named that. On PostgreSQL <17
// NOT NULL is a column attribute (no pg_constraint row with contype='n'), so
// the lookup returns nothing and this is a silent no-op.
func ensureNotNullConstraintName(ctx context.Context, conn db.DB, table, column, desired string) error {
	rows, err := conn.QueryContext(ctx, `
		SELECT con.conname
		FROM pg_constraint con
		JOIN pg_attribute att
		  ON att.attrelid = con.conrelid AND att.attnum = ANY(con.conkey)
		WHERE con.conrelid = $1::regclass
		  AND att.attname = $2
		  AND con.contype = 'n'
		LIMIT 1
	`, pq.QuoteIdentifier(table), column)
	if err != nil {
		return fmt.Errorf("looking up not null constraint: %w", err)
	}
	if rows == nil {
		// FakeDB path used in unit tests.
		return nil
	}
	defer rows.Close()

	var current string
	if err := db.ScanFirstValue(rows, &current); err != nil {
		// No row → PG <17 (no contype='n') or no NOT NULL constraint exists.
		return nil
	}
	if current == "" || current == desired {
		return nil
	}

	_, err = conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE IF EXISTS %s RENAME CONSTRAINT %s TO %s",
		pq.QuoteIdentifier(table),
		pq.QuoteIdentifier(current),
		pq.QuoteIdentifier(desired)))
	return err
}

type setDefaultAction struct {
	conn         db.DB
	id           string
	table        string
	column       string
	defaultValue string
}

func NewSetDefaultValueAction(conn db.DB, table, column, defaultValue string) *setDefaultAction {
	return &setDefaultAction{
		conn:         conn,
		id:           fmt.Sprintf("set_default_%s_%s", table, column),
		table:        table,
		column:       column,
		defaultValue: defaultValue,
	}
}

func (a *setDefaultAction) ID() string { return a.id }

func (a *setDefaultAction) Execute(ctx context.Context) error {
	_, err := a.conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE IF EXISTS %s ALTER COLUMN %s SET DEFAULT %s",
		pq.QuoteIdentifier(a.table),
		pq.QuoteIdentifier(a.column),
		a.defaultValue))
	return err
}

type dropDefaultAction struct {
	conn   db.DB
	id     string
	table  string
	column string
}

func NewDropDefaultValueAction(conn db.DB, table, column string) *dropDefaultAction {
	return &dropDefaultAction{
		conn:   conn,
		id:     fmt.Sprintf("drop_default_%s_%s", table, column),
		table:  table,
		column: column,
	}
}

func (a *dropDefaultAction) ID() string { return a.id }

func (a *dropDefaultAction) Execute(ctx context.Context) error {
	_, err := a.conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE IF EXISTS %s ALTER COLUMN %s DROP DEFAULT",
		pq.QuoteIdentifier(a.table),
		pq.QuoteIdentifier(a.column)))
	return err
}

type rawSQLAction struct {
	conn db.DB
	id   string
	sql  string
}

func NewRawSQLAction(conn db.DB, sql string) *rawSQLAction {
	return &rawSQLAction{
		conn: conn,
		id:   fmt.Sprintf("raw_sql_%s", uuid.NewString()),
		sql:  sql,
	}
}

func (a *rawSQLAction) ID() string { return a.id }

func (a *rawSQLAction) Execute(ctx context.Context) error {
	_, err := a.conn.ExecContext(ctx, a.sql)
	return err
}

type setReplicaIdentityAction struct {
	conn     db.DB
	id       string
	table    string
	identity string
	index    string
}

func NewSetReplicaIdentityAction(conn db.DB, table string, identityType, index string) *setReplicaIdentityAction {
	identity := strings.ToUpper(identityType)
	return &setReplicaIdentityAction{
		conn:     conn,
		id:       fmt.Sprintf("set_replica_%s_%s", identity, index),
		table:    table,
		identity: identity,
		index:    index,
	}
}

func (a *setReplicaIdentityAction) ID() string { return a.id }

func (a *setReplicaIdentityAction) Execute(ctx context.Context) error {
	// build the correct form of the `SET REPLICA IDENTITY` statement based on the`identity type
	identitySQL := a.identity
	if identitySQL == "INDEX" {
		identitySQL = fmt.Sprintf("USING INDEX %s", pq.QuoteIdentifier(a.index))
	}

	// set the replica identity on the underlying table
	_, err := a.conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s REPLICA IDENTITY %s",
		pq.QuoteIdentifier(a.table),
		identitySQL))
	return err
}
