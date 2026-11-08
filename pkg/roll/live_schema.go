// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/lib/pq"

	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/schema"
)

// Contraction never drops or rebuilds the live version schema, the one apps
// are pinned to. It refuses to start without that schema, and it runs each
// onComplete raw SQL statement in a transaction that rolls back if the
// statement removes a live view whose table remains (liveSQLAction).

// hasVersionSchema reports whether mig's version schema exists.
func (m *Roll) hasVersionSchema(ctx context.Context, mig *migrations.Migration) (bool, error) {
	existing, err := m.ExistingVersionSchemas(ctx)
	if err != nil {
		return false, err
	}
	return slices.Contains(existing, VersionedSchemaName(m.schema, mig.VersionSchemaName())), nil
}

// requireLiveViews refuses a contraction when the version schema of `version`
// is missing a view of a table project returns. Only a table that already
// existed when that schema was created counts: one created after it, such as a
// partition pg_partman adds or a table onComplete SQL creates, was never
// projected. When the projection cannot be rebuilt (a Complete that ran part
// way), there is nothing to compare against and the check is skipped.
func (m *Roll) requireLiveViews(ctx context.Context, version string, project func() (*schema.Schema, error)) error {
	projection, err := project()
	if err != nil {
		m.logger.Info("not checking the live views: their projection cannot be rebuilt", "error", err.Error())
		return nil
	}
	versionSchema := VersionedSchemaName(m.schema, version)
	views, err := liveViewReads(ctx, m.pgConn, versionSchema)
	if err != nil {
		return err
	}
	rows, err := m.pgConn.QueryContext(ctx, `SELECT c.relname FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relkind IN ('r', 'p')
			AND c.oid < (SELECT oid FROM pg_catalog.pg_namespace WHERE nspname = $2)`, m.schema, versionSchema)
	if err != nil {
		return fmt.Errorf("unable to list the tables %q projects: %w", versionSchema, err)
	}
	defer rows.Close()
	older := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return fmt.Errorf("unable to scan table name: %w", err)
		}
		older[name] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}

	var missing []string
	for name, table := range projection.Tables {
		if table == nil || table.Deleted || strings.HasPrefix(table.Name, "_pgroll_del_") || !older[table.Name] ||
			slices.ContainsFunc(views, func(r liveViewRead) bool { return r.view == name }) {
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)
	return fmt.Errorf("cannot contract: the live version schema is missing %s, which apps read, and "+
		"contraction would finish without it. Restore it with `pgroll materialize`, then run `pgroll complete` "+
		"again", qualifiedViews(versionSchema, missing))
}

// guardLiveViews runs every onComplete raw SQL action among a contraction's
// actions as a liveSQLAction against the live version schema of `live`. The
// actions are `completing`'s; pendingLeaf is the active migration when its
// Start is still unprojected over the tables (nil once it is completing).
func (m *Roll) guardLiveViews(actions []migrations.DBAction, live, completing, pendingLeaf *migrations.Migration) []migrations.DBAction {
	if live == nil || m.disableVersionSchemas {
		return actions
	}
	out := make([]migrations.DBAction, len(actions))
	for i, action := range actions {
		raw, ok := action.(migrations.SQLAction)
		if !ok {
			out[i] = action
			continue
		}
		out[i] = &liveSQLAction{
			m:           m,
			raw:         raw,
			version:     live.VersionSchemaName(),
			schema:      VersionedSchemaName(m.schema, live.VersionSchemaName()),
			swap:        isRawInverse(completing),
			completing:  completing,
			pendingLeaf: pendingLeaf,
		}
	}
	return out
}

// isRawInverse reports whether mig is an inverse that a sealed revert
// generated from a raw SQL migration's `down`, and holds nothing else. Such a
// `down` can remove what the restored version schema still projects.
func isRawInverse(mig *migrations.Migration) bool {
	if mig == nil || mig.RevertOf == "" || len(mig.Operations) == 0 {
		return false
	}
	for _, op := range mig.Operations {
		if raw, ok := op.(*migrations.OpRawSQL); !ok || !raw.OnComplete {
			return false
		}
	}
	return true
}

// liveSQLAction runs one onComplete raw SQL statement in a transaction that
// keeps the live version schema whole. A statement that a live view blocks
// (2BP01, 0A000) fails with the view named, and one that leaves a live view
// missing while its table remains (a CASCADE) is rolled back. Only a raw
// inverse (swap) may drop the views its statement blocks, recreating, from
// the projection the statement leaves, those whose table remains.
type liveSQLAction struct {
	m           *Roll
	raw         migrations.SQLAction
	version     string
	schema      string
	swap        bool
	completing  *migrations.Migration
	pendingLeaf *migrations.Migration
}

func (a *liveSQLAction) ID() string { return a.raw.ID() }

func (a *liveSQLAction) Execute(ctx context.Context) error {
	if err := migrations.CheckNoTransactionControl(a.raw.SQL()); err != nil {
		return fmt.Errorf("%w. It was not run, the contraction stopped and the live schema is unchanged; the "+
			"deployment stays uncontracted until the statement is changed by hand", err)
	}
	err := a.m.pgConn.WithRetryableTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		before, err := liveViewReads(ctx, tx, a.schema)
		if err != nil {
			return err
		}
		if err := a.run(ctx, tx); err != nil {
			return err
		}
		if err := checkLiveViews(ctx, tx, a.schema, before); err != nil {
			return fmt.Errorf("%w; it was rolled back, and the live schema is unchanged. pgroll never "+
				"drops a live view during contraction: run the statement at Start in a migration of its own", err)
		}
		return nil
	})
	if !isNonTransactional(err) {
		return err
	}
	return a.runOutsideTransaction(ctx)
}

func (a *liveSQLAction) run(ctx context.Context, tx *sql.Tx) error {
	if a.swap {
		return a.runAroundViews(ctx, tx)
	}
	_, err := tx.ExecContext(ctx, a.raw.SQL())
	return a.conflict(err)
}

// runOutsideTransaction runs a statement Postgres refuses inside a
// transaction (CONCURRENTLY, VACUUM, a procedure that commits), then checks
// the live views. A view it removed cannot be put back by a rollback.
func (a *liveSQLAction) runOutsideTransaction(ctx context.Context) error {
	before, err := liveViewReads(ctx, a.m.pgConn, a.schema)
	if err != nil {
		return err
	}
	if err := a.conflict(a.raw.Execute(ctx)); err != nil {
		return err
	}
	if err := checkLiveViews(ctx, a.m.pgConn, a.schema, before); err != nil {
		return fmt.Errorf("%w; the statement cannot run in a transaction, so it was not rolled back: "+
			"restore the view with `pgroll materialize`", err)
	}
	return nil
}

// conflict names the live views err says block the statement.
func (a *liveSQLAction) conflict(err error) error {
	views := liveViewsBlocking(err, a.schema)
	if len(views) == 0 {
		return err
	}
	return fmt.Errorf("onComplete raw SQL conflicts with %s of the live version schema, which apps read; "+
		"the contraction stopped and the live schema is unchanged. pgroll never drops or rebuilds a live "+
		"view during contraction: run the statement at Start in a migration of its own, or change it so no "+
		"live view depends on what it changes: %w", qualifiedViews(a.schema, views), err)
}

// maxConflictRounds bounds how many times a statement is retried as Postgres
// names more blocking views (it lists at most 100 dependents per error).
const maxConflictRounds = 64

// runAroundViews runs a raw inverse's statement inside tx, dropping each live
// view it blocks first, then recreates those whose table the projection still
// has. The caller's check fails the transaction if one whose table remains is
// left missing.
func (a *liveSQLAction) runAroundViews(ctx context.Context, tx *sql.Tx) error {
	var dropped []string
	for round := 0; ; round++ {
		if _, err := tx.ExecContext(ctx, "SAVEPOINT pgroll_live_views"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, a.raw.SQL())
		if err == nil {
			break
		}
		if _, rbErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT pgroll_live_views"); rbErr != nil {
			return errors.Join(err, rbErr)
		}
		var fresh []string
		for _, view := range liveViewsBlocking(err, a.schema) {
			if !slices.Contains(dropped, view) && !slices.Contains(fresh, view) {
				fresh = append(fresh, view)
			}
		}
		if len(fresh) == 0 || round == maxConflictRounds {
			return err
		}
		for _, view := range fresh {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf("DROP VIEW %s.%s",
				pq.QuoteIdentifier(a.schema), pq.QuoteIdentifier(view))); err != nil {
				return err
			}
		}
		dropped = append(dropped, fresh...)
	}
	if len(dropped) == 0 {
		return nil
	}

	a.m.logger.Info("a raw inverse statement removed what live views read; recreated them in its transaction",
		"schema", a.schema, "views", strings.Join(dropped, ","))
	projection, err := a.projection(ctx, tx)
	if err != nil {
		return err
	}
	for _, view := range dropped {
		table := projection.Tables[view]
		if table == nil || table.Deleted || strings.HasPrefix(table.Name, "_pgroll_del_") {
			continue
		}
		for _, stmt := range a.m.createViewStatements(a.version, view, table) {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
	}
	return nil
}

// projection is the schema the live views project once the statement has run
// in tx: the tables as tx sees them, with every pending Start but the
// completing migration's replayed over them.
func (a *liveSQLAction) projection(ctx context.Context, tx *sql.Tx) (*schema.Schema, error) {
	var raw []byte
	if err := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT %s.read_schema($1)",
		pq.QuoteIdentifier(a.m.state.Schema())), a.m.schema).Scan(&raw); err != nil {
		return nil, fmt.Errorf("unable to read schema: %w", err)
	}
	var s schema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("unable to unmarshal schema: %w", err)
	}
	return a.m.replayPending(ctx, &s, []string{a.completing.Name}, a.pendingLeaf)
}

// querier is what the live view checks read the catalog through: the
// contraction's transaction, or its connection.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// liveViewRead is a live view and the relations it reads.
type liveViewRead struct {
	view   string
	oids   []int64
	tables []string
}

// liveViewReads lists each view in versionSchema with the relations it reads.
func liveViewReads(ctx context.Context, q querier, versionSchema string) ([]liveViewRead, error) {
	rows, err := q.QueryContext(ctx, `SELECT v.relname,
			coalesce(array_agg(DISTINCT b.oid::bigint) FILTER (WHERE b.oid IS NOT NULL), '{}'),
			coalesce(array_agg(DISTINCT format('%I.%I', bn.nspname, b.relname)) FILTER (WHERE b.oid IS NOT NULL), '{}')
		FROM pg_catalog.pg_class v
		JOIN pg_catalog.pg_namespace vn ON vn.oid = v.relnamespace
		LEFT JOIN pg_catalog.pg_rewrite r ON r.ev_class = v.oid
		LEFT JOIN pg_catalog.pg_depend d ON d.classid = 'pg_catalog.pg_rewrite'::regclass AND d.objid = r.oid
			AND d.refclassid = 'pg_catalog.pg_class'::regclass AND d.refobjid <> v.oid
		LEFT JOIN pg_catalog.pg_class b ON b.oid = d.refobjid
		LEFT JOIN pg_catalog.pg_namespace bn ON bn.oid = b.relnamespace
		WHERE vn.nspname = $1 AND v.relkind = 'v'
		GROUP BY v.relname
		ORDER BY v.relname`, versionSchema)
	if err != nil {
		return nil, fmt.Errorf("unable to list views in %q: %w", versionSchema, err)
	}
	defer rows.Close()

	var reads []liveViewRead
	for rows.Next() {
		var r liveViewRead
		if err := rows.Scan(&r.view, pq.Array(&r.oids), pq.Array(&r.tables)); err != nil {
			return nil, fmt.Errorf("unable to scan view: %w", err)
		}
		reads = append(reads, r)
	}
	return reads, rows.Err()
}

// checkLiveViews fails when a view of `before` is gone while a relation it
// read is still there, by OID or by name.
func checkLiveViews(ctx context.Context, q querier, versionSchema string, before []liveViewRead) error {
	after, err := liveViewReads(ctx, q, versionSchema)
	if err != nil {
		return err
	}
	for _, r := range before {
		if slices.ContainsFunc(after, func(a liveViewRead) bool { return a.view == r.view }) {
			continue
		}
		remains, err := anyRelationExists(ctx, q, r)
		if err != nil {
			return err
		}
		if remains {
			return fmt.Errorf("onComplete raw SQL dropped %s, which apps read, while the table it reads "+
				"(%s) remains", qualifiedViews(versionSchema, []string{r.view}), strings.Join(r.tables, ", "))
		}
	}
	return nil
}

func anyRelationExists(ctx context.Context, q querier, r liveViewRead) (bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_class WHERE oid::bigint = ANY ($1))
		OR EXISTS (SELECT 1 FROM unnest($2::text[]) AS t(name) WHERE to_regclass(t.name) IS NOT NULL)`,
		pq.Array(r.oids), pq.Array(r.tables))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var exists bool
	if rows.Next() {
		if err := rows.Scan(&exists); err != nil {
			return false, err
		}
	}
	return exists, rows.Err()
}

func qualifiedViews(versionSchema string, views []string) string {
	names := make([]string, len(views))
	for i, v := range views {
		names[i] = "view " + pq.QuoteIdentifier(versionSchema) + "." + pq.QuoteIdentifier(v)
	}
	return strings.Join(names, ", ")
}

// viewInDetail matches `view <schema>.<name>` in a Postgres dependency
// message, each part bare or double-quoted.
var viewInDetail = regexp.MustCompile(`\bview ((?:"(?:[^"]|"")+")|[^\s".]+)\.((?:"(?:[^"]|"")+")|[^\s".]+)`)

// liveViewsBlocking returns the views of versionSchema that Postgres names as
// blocking a change: err is a 2BP01 (dependent_objects_still_exist) or 0A000
// (e.g. altering the type of a column a view uses) whose detail lists them.
func liveViewsBlocking(err error, versionSchema string) []string {
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) || (pqErr.Code != "2BP01" && pqErr.Code != "0A000") {
		return nil
	}
	var views []string
	for _, text := range []string{pqErr.Detail, pqErr.Message} {
		for _, match := range viewInDetail.FindAllStringSubmatch(text, -1) {
			if unquoteIdent(match[1]) == versionSchema {
				views = append(views, unquoteIdent(match[2]))
			}
		}
	}
	return views
}

func unquoteIdent(ident string) string {
	if len(ident) >= 2 && strings.HasPrefix(ident, `"`) && strings.HasSuffix(ident, `"`) {
		return strings.ReplaceAll(ident[1:len(ident)-1], `""`, `"`)
	}
	return ident
}

// isNonTransactional reports whether err is Postgres refusing a statement
// because it cannot run inside a transaction: 25001 (CONCURRENTLY, VACUUM) or
// 2D000 (a procedure that commits).
func isNonTransactional(err error) bool {
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		return false
	}
	return pqErr.Code == "25001" || pqErr.Code == "2D000"
}

// RestoreActiveVersionSchema creates the views missing from the active
// migration's version schema, the one apps are pinned to, as its Start
// projected them: every pending Start, its own last, replayed over the
// tables. It never drops or rebuilds a view, and creates nothing when the
// replay fails. Returns the schema and the number of views created.
func (m *Roll) RestoreActiveVersionSchema(ctx context.Context) (string, int, error) {
	if m.disableVersionSchemas {
		return "", 0, fmt.Errorf("version schemas are disabled")
	}
	active, err := m.state.GetActiveMigration(ctx, m.schema)
	if err != nil {
		return "", 0, fmt.Errorf("unable to get active migration: %w", err)
	}
	versionSchema := VersionedSchemaName(m.schema, active.VersionSchemaName())
	projection, err := m.readSchemaReplaying(ctx, "", active)
	if err != nil {
		return versionSchema, 0, fmt.Errorf("unable to project %q as the Start of %q did, so nothing was "+
			"created: %w", versionSchema, active.Name, err)
	}
	n, err := m.createMissingViews(ctx, active.VersionSchemaName(), projection)
	return versionSchema, n, err
}

// ReadSchemaWithDeferred is the schema as the latest migration's Start
// projected it: the tables with every still-deferred Start replayed over
// them.
func (m *Roll) ReadSchemaWithDeferred(ctx context.Context) (*schema.Schema, error) {
	return m.readSchemaWithDeferred(ctx)
}

// createMissingViews creates version's schema if needed and, in it, a view
// for each table of sc that has none. A view that exists is left as it is.
// Returns the number of views created.
func (m *Roll) createMissingViews(ctx context.Context, version string, sc *schema.Schema) (int, error) {
	if err := validateVersionSchemaName(m.schema, version); err != nil {
		return 0, err
	}
	versionSchema := VersionedSchemaName(m.schema, version)
	if _, err := m.pgConn.ExecContext(ctx,
		fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", pq.QuoteIdentifier(versionSchema))); err != nil {
		return 0, fmt.Errorf("unable to create version schema %q: %w", versionSchema, err)
	}
	existing, err := liveViewReads(ctx, m.pgConn, versionSchema)
	if err != nil {
		return 0, err
	}

	names := make([]string, 0, len(sc.Tables))
	for name := range sc.Tables {
		names = append(names, name)
	}
	sort.Strings(names)

	created := 0
	for _, name := range names {
		table := sc.Tables[name]
		if table == nil || table.Deleted || strings.HasPrefix(table.Name, "_pgroll_del_") ||
			slices.ContainsFunc(existing, func(r liveViewRead) bool { return r.view == name }) {
			continue
		}
		if err := runInTransaction(ctx, m.pgConn, m.createViewStatements(version, name, table)); err != nil {
			return created, fmt.Errorf("unable to create view %s.%s: %w", versionSchema, name, err)
		}
		created++
	}
	m.logger.Info("created missing version schema views", "schema", versionSchema, "views", created)
	return created, nil
}

// runInTransaction runs stmts in one retryable transaction.
func runInTransaction(ctx context.Context, conn db.DB, stmts []string) error {
	return conn.WithRetryableTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, stmt := range stmts {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
}
