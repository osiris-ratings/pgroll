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
	"time"

	"github.com/lib/pq"

	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/schema"
)

// restoreTimeout bounds putting the live version schema back after the
// caller's context was cancelled. It must finish before a supervisor's hard
// kill: a deploy engine typically escalates an abort to SIGKILL 20 s after the
// interrupt, so the 30 s rollbackCleanupTimeout would lose that race.
const restoreTimeout = 15 * time.Second

// liveGuard is what a contraction knows about the live version schema, the
// projection apps are pinned to, so it can keep that schema readable:
// contraction never drops it, never leaves it missing views, and drops a view
// only inside a transaction that recreates it (see runAroundLiveViews).
type liveGuard struct {
	// live is the migration whose version schema apps read.
	live *migrations.Migration
	// pendingLeaf: live is the active migration, its Start has run and its
	// own Complete has not, so its Start is replayed when projecting.
	pendingLeaf bool
	// completed names a still-queued migration whose Complete has already run,
	// left out of the replay (FinishContraction clears the last flag late).
	completed string
	// rawRan: onComplete raw SQL ran against the tables, so views may be
	// missing (a raw CASCADE) or stale (a raw rename).
	rawRan bool
}

func (g *liveGuard) versionSchema(m *Roll) string {
	return VersionedSchemaName(m.schema, g.live.VersionSchemaName())
}

// liveProjection is the schema the live views project, as Start projected
// it: the physical schema with every Start whose Complete has not run yet
// replayed over it. A physical read alone would show pre-rename names and
// bind views to columns about to be dropped.
func (m *Roll) liveProjection(ctx context.Context, g *liveGuard) (*schema.Schema, error) {
	var leaf *migrations.Migration
	if g.pendingLeaf {
		leaf = g.live
	}
	s, err := m.state.ReadSchema(ctx, m.schema)
	if err != nil {
		return nil, err
	}
	return m.replayPending(ctx, s, []string{g.completed}, leaf)
}

// restoreLiveViews makes the live version schema project every table it
// should. A missing schema or view is always created. With refresh, a view
// whose columns no longer match the projection is rebuilt too; views that
// match are left alone, so nothing re-locks a view apps are reading for no
// reason. Each view falls back to the physical schema when the replayed
// projection cannot build it, so one odd table never leaves the rest
// missing. Returns the number of views created or rebuilt.
func (m *Roll) restoreLiveViews(ctx context.Context, g *liveGuard, refresh bool) (int, error) {
	if g == nil || m.disableVersionSchemas {
		return 0, nil
	}
	versionSchema := g.versionSchema(m)

	projection, projErr := m.liveProjection(ctx, g)
	physical, err := m.state.ReadSchema(ctx, m.schema)
	if err != nil {
		return 0, fmt.Errorf("unable to read schema: %w", err)
	}
	if projErr != nil {
		m.logger.Info("live projection unavailable; projecting the physical schema",
			"schema", versionSchema, "error", projErr.Error())
		projection = physical
	}

	existing, err := m.liveViewColumns(ctx, versionSchema)
	if err != nil {
		return 0, err
	}

	names := make([]string, 0, len(projection.Tables))
	for name := range projection.Tables {
		names = append(names, name)
	}
	sort.Strings(names)

	created := false
	rebuilt := 0
	var errs []error
	for _, name := range names {
		table := projection.Tables[name]
		if table == nil || table.Deleted || strings.HasPrefix(table.Name, "_pgroll_del_") {
			continue
		}
		have, ok := existing[name]
		if ok && (!refresh || slices.Equal(have, projectedColumns(table))) {
			continue
		}
		if !created {
			if _, err := m.pgConn.ExecContext(ctx,
				fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", pq.QuoteIdentifier(versionSchema))); err != nil {
				return rebuilt, fmt.Errorf("unable to create live version schema %q: %w", versionSchema, err)
			}
			created = true
		}
		err := m.ensureView(ctx, g.live.VersionSchemaName(), name, table)
		if err != nil && projection != physical {
			if alt := physical.GetTable(name); alt != nil && !strings.HasPrefix(alt.Name, "_pgroll_del_") {
				err = m.ensureView(ctx, g.live.VersionSchemaName(), name, alt)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("view %s.%s: %w", versionSchema, name, err))
			continue
		}
		rebuilt++
	}
	if rebuilt > 0 {
		m.logger.Info("restored live version schema views", "schema", versionSchema, "views", rebuilt)
	}
	return rebuilt, errors.Join(errs...)
}

// projectedColumns is the sorted list of column names ensureView projects
// for a table.
func projectedColumns(table *schema.Table) []string {
	cols := make([]string, 0, len(table.Columns))
	for name, col := range table.Columns {
		if col.Deleted || strings.HasPrefix(name, "_pgroll_") {
			continue
		}
		cols = append(cols, name)
	}
	sort.Strings(cols)
	return cols
}

// liveViewColumns returns each view in the version schema with its sorted
// column names; empty when the schema does not exist.
func (m *Roll) liveViewColumns(ctx context.Context, versionSchema string) (map[string][]string, error) {
	rows, err := m.pgConn.QueryContext(ctx, `SELECT c.relname, a.attname
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid
		WHERE n.nspname = $1 AND c.relkind = 'v' AND a.attnum > 0 AND NOT a.attisdropped`,
		versionSchema)
	if err != nil {
		return nil, fmt.Errorf("unable to list views in %q: %w", versionSchema, err)
	}
	defer rows.Close()

	views := map[string][]string{}
	for rows.Next() {
		var view, column string
		if err := rows.Scan(&view, &column); err != nil {
			return nil, fmt.Errorf("unable to scan view column: %w", err)
		}
		views[view] = append(views[view], column)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating view columns: %w", err)
	}
	for _, cols := range views {
		sort.Strings(cols)
	}
	return views, nil
}

// restoreAfterFailure puts the live views back after a contraction failed,
// and adds to cause anything it could not restore. A cancelled caller gets a
// fresh context of at most restoreTimeout.
func (m *Roll) restoreAfterFailure(ctx context.Context, g *liveGuard, cause error) error {
	if g == nil || m.disableVersionSchemas {
		return cause
	}
	rctx, cancel := ctx, context.CancelFunc(func() {})
	if ctx.Err() != nil {
		m.logger.Info("context cancelled; restoring the live version schema with a fresh context",
			"timeout", restoreTimeout.String())
		rctx, cancel = context.WithTimeout(context.Background(), restoreTimeout)
	}
	defer cancel()

	if _, err := m.restoreLiveViews(rctx, g, false); err != nil {
		return errors.Join(cause, fmt.Errorf(
			"the live version schema %q could not be fully restored; run `pgroll materialize`: %w",
			g.versionSchema(m), err,
		))
	}
	return cause
}

// viewConflictRecovery returns the coordinator hook a contraction runs its
// Complete actions with. An action Postgres refuses because a live view
// depends on what it changes is raw SQL pgroll cannot reason about; it runs
// again inside one transaction with the blocking views dropped and recreated
// (runAroundLiveViews). Any other failure is returned as it is. `completing`
// is the migration whose actions run, left out of the projection's replay.
func (m *Roll) viewConflictRecovery(g *liveGuard, completing *migrations.Migration) func(context.Context, migrations.DBAction, error) error {
	if g == nil || m.disableVersionSchemas {
		return nil
	}
	return func(ctx context.Context, action migrations.DBAction, err error) error {
		versionSchema := g.versionSchema(m)
		if !isLiveViewConflict(err, versionSchema) {
			return err
		}
		raw, ok := action.(migrations.SQLAction)
		if !ok {
			return fmt.Errorf("a view in the live version schema %q depends on what %s changes; "+
				"pgroll runs only raw SQL around live views, so the live schema is unchanged: %w",
				versionSchema, action.ID(), err)
		}
		return m.runAroundLiveViews(ctx, g, completing, raw.SQL(), err)
	}
}

// maxConflictRounds bounds how many times a statement is retried as Postgres
// names more blocking views (it lists at most 100 dependents per error).
const maxConflictRounds = 64

// runAroundLiveViews runs stmt, which the live views named in conflict block,
// in one transaction: drop those views, run it, recreate them from the
// projection it leaves. Readers of those views wait for the lock instead of
// erroring, and any failure rolls back to the intact schema. A statement that
// cannot run inside a transaction fails the contraction instead.
func (m *Roll) runAroundLiveViews(ctx context.Context, g *liveGuard, completing *migrations.Migration, stmt string, conflict error) error {
	versionSchema := g.versionSchema(m)
	m.logger.Info("live views block a contraction statement; running it in a transaction that drops "+
		"and recreates them", "schema", versionSchema, "error", conflict.Error())

	err := m.pgConn.WithRetryableTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var dropped []string
		seen := map[string]bool{}
		blocked := conflict
		for round := 0; ; round++ {
			var fresh []string
			for _, view := range liveViewsBlocking(blocked, versionSchema) {
				if !seen[view] {
					seen[view] = true
					fresh = append(fresh, view)
				}
			}
			if len(fresh) == 0 || round == maxConflictRounds {
				return blocked
			}
			for _, view := range fresh {
				if _, err := tx.ExecContext(ctx, fmt.Sprintf("DROP VIEW IF EXISTS %s.%s",
					pq.QuoteIdentifier(versionSchema), pq.QuoteIdentifier(view))); err != nil {
					return err
				}
			}
			dropped = append(dropped, fresh...)

			if _, err := tx.ExecContext(ctx, "SAVEPOINT pgroll_live_views"); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, stmt)
			if err == nil {
				break
			}
			if _, rbErr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT pgroll_live_views"); rbErr != nil {
				return errors.Join(err, rbErr)
			}
			if !isLiveViewConflict(err, versionSchema) {
				return err
			}
			blocked = err
		}

		projection, err := m.liveProjectionTx(ctx, tx, g, completing)
		if err != nil {
			return err
		}
		for _, view := range dropped {
			table := projection.Tables[view]
			if table == nil || table.Deleted || strings.HasPrefix(table.Name, "_pgroll_del_") {
				m.logger.Info("not recreating a live view whose table the statement removed",
					"schema", versionSchema, "view", view)
				continue
			}
			for _, viewStmt := range m.viewStatements(g.live.VersionSchemaName(), view, table) {
				if _, err := tx.ExecContext(ctx, viewStmt); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err == nil {
		g.rawRan = true
		return nil
	}
	if isNonTransactional(err) {
		return fmt.Errorf("live views in %q block a contraction statement that cannot run inside a "+
			"transaction, so pgroll cannot run it around them; the live schema is unchanged. Make the "+
			"statement transactional, or change it so no live view depends on what it changes: %w",
			versionSchema, err)
	}
	return fmt.Errorf("unable to run a contraction statement around the live views of %q; "+
		"the live schema is unchanged: %w", versionSchema, err)
}

// liveProjectionTx is liveProjection read inside tx, so it sees what the
// transaction has changed. The migration being completed is left out of the
// replay: its Complete is what is running.
func (m *Roll) liveProjectionTx(ctx context.Context, tx *sql.Tx, g *liveGuard, completing *migrations.Migration) (*schema.Schema, error) {
	var raw []byte
	if err := tx.QueryRowContext(ctx, fmt.Sprintf("SELECT %s.read_schema($1)",
		pq.QuoteIdentifier(m.state.Schema())), m.schema).Scan(&raw); err != nil {
		return nil, fmt.Errorf("unable to read schema: %w", err)
	}
	var s schema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("unable to unmarshal schema: %w", err)
	}
	exclude := []string{g.completed}
	var leaf *migrations.Migration
	if completing != nil {
		exclude = append(exclude, completing.Name)
	}
	if g.pendingLeaf && (completing == nil || completing.Name != g.live.Name) {
		leaf = g.live
	}
	return m.replayPending(ctx, &s, exclude, leaf)
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

func isLiveViewConflict(err error, versionSchema string) bool {
	return len(liveViewsBlocking(err, versionSchema)) > 0
}

func unquoteIdent(ident string) string {
	if len(ident) >= 2 && strings.HasPrefix(ident, `"`) && strings.HasSuffix(ident, `"`) {
		return strings.ReplaceAll(ident[1:len(ident)-1], `""`, `"`)
	}
	return ident
}

// isNonTransactional reports whether err is Postgres refusing a statement
// because it cannot run inside a transaction: 25001 (CONCURRENTLY, VACUUM),
// 2D000 (a procedure that COMMITs), 55P04 (using an enum value added in the
// same transaction).
func isNonTransactional(err error) bool {
	var pqErr *pq.Error
	if !errors.As(err, &pqErr) {
		return false
	}
	switch pqErr.Code {
	case "25001", "2D000", "55P04":
		return true
	}
	return false
}

// RestoreActiveVersionSchema re-projects the active migration's version
// schema from the tables, creating missing views and rebuilding stale ones,
// without touching pgroll state or user tables. It is the recovery for a
// contraction that left the live schema dropped while the migration it
// belongs to is still in progress. Returns the schema and the number of views
// created or rebuilt.
func (m *Roll) RestoreActiveVersionSchema(ctx context.Context) (string, int, error) {
	if m.disableVersionSchemas {
		return "", 0, fmt.Errorf("version schemas are disabled")
	}
	active, err := m.state.GetActiveMigration(ctx, m.schema)
	if err != nil {
		return "", 0, fmt.Errorf("unable to get active migration: %w", err)
	}
	g := &liveGuard{live: active, pendingLeaf: true}
	n, err := m.restoreLiveViews(ctx, g, true)
	return g.versionSchema(m), n, err
}
