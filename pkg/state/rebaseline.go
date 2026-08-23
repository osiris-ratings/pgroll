// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

// ConvertToBaselineResult reports the outcome of ConvertMigrationToBaseline.
type ConvertToBaselineResult struct {
	// AlreadyBaseline is true when the row was already migration_type =
	// 'baseline' and nothing was changed — the call is an idempotent no-op.
	AlreadyBaseline bool

	// EmptyResultingSchema is true when the anchor row's resulting_schema is
	// the '{}' placeholder rather than a real schema snapshot. That is the
	// signature of a hand-stamped ancestry (INSERT with a '{}' literal instead
	// of `pgroll stamp`/`pgroll baseline`). Advisory only: every consumer of
	// LatestBaseline reads just the name, but SchemaAfterMigration on the
	// anchor will return an empty schema.
	EmptyResultingSchema bool
}

// ConvertMigrationToBaseline converts an already-applied migration row into a
// baseline in place: UPDATE migration_type = 'baseline', touching nothing
// else. Because the row keeps its original created_at, the baseline lands at
// exactly the point in history where the migration was applied — SchemaHistory
// hides everything at or before it, and resolveLocalSet skips files at or
// before its name. This is the safe primitive for truncating migration
// history: a fresh baseline row (CreateBaseline, Stamp) is stamped with
// CURRENT_TIMESTAMP, lands at the chain tip, and would hide the entire
// history — making every retained migration look unapplied.
//
// The conversion runs in one transaction with the anchor row locked, and
// refuses (wrapping ErrRebaselineRefused) unless the history is shaped so the
// conversion cannot change what `pgroll migrate` would do:
//
//   - the anchor row exists, is done, sealed, and not awaiting a deferred
//     complete (an uncontracted anchor still has revert state behind it)
//   - no migration is in progress on the schema
//   - every row at or below the anchor's created_at is sealed with no
//     deferred complete (a baseline must not bury a pending contraction or an
//     open revert window)
//   - no existing baseline row has created_at at or after the anchor's
//     (LatestBaseline picks by created_at, so a later baseline would mask the
//     conversion entirely)
//   - name order agrees with apply order across the anchor boundary: no row
//     at or below the anchor's created_at has a name sorting after the
//     anchor, and no row above it has a name sorting at or before. The two
//     cuts — SchemaHistory's created_at cut and resolveLocalSet's name cut —
//     must select the same set, or migrate would re-apply hidden migrations
//     or hard-fail on false-missing files. Inferred rows are excluded: they
//     never correspond to files, and post-anchor inferred rows keep failing
//     migrate exactly as they do today.
//
// dryRun runs every audit and reports the result without writing anything.
func (s *State) ConvertMigrationToBaseline(
	ctx context.Context,
	schemaName, name string,
	dryRun bool,
) (*ConvertToBaselineResult, error) {
	tx, err := s.pgConn.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("rebaseline: beginning transaction: %w", err)
	}
	defer tx.Rollback()

	var (
		migrationType    string
		done             bool
		sealed           bool
		completeDeferred bool
		emptySchema      bool
	)
	err = tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT migration_type, done, sealed, complete_deferred,
			resulting_schema = '{}'::jsonb
			FROM %s.migrations WHERE schema = $1 AND name = $2 FOR UPDATE`,
			pq.QuoteIdentifier(s.schema)),
		schemaName, name).Scan(&migrationType, &done, &sealed, &completeDeferred, &emptySchema)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: migration %q has no row in the history of schema %q",
			ErrRebaselineRefused, name, schemaName)
	}
	if err != nil {
		return nil, fmt.Errorf("rebaseline: reading migration %q: %w", name, err)
	}

	result := &ConvertToBaselineResult{EmptyResultingSchema: emptySchema}

	if migrationType == "baseline" {
		result.AlreadyBaseline = true
		return result, nil
	}
	if migrationType == "inferred" {
		return nil, fmt.Errorf("%w: migration %q is an inferred row (out-of-band DDL); "+
			"it has no migration file and cannot anchor a baseline", ErrRebaselineRefused, name)
	}
	if !done || !sealed || completeDeferred {
		return nil, fmt.Errorf("%w: migration %q is not fully applied and contracted "+
			"(done=%t sealed=%t complete_deferred=%t); complete the deployment first",
			ErrRebaselineRefused, name, done, sealed, completeDeferred)
	}

	var active bool
	err = tx.QueryRowContext(ctx,
		fmt.Sprintf("SELECT %s.is_active_migration_period($1)", pq.QuoteIdentifier(s.schema)),
		schemaName).Scan(&active)
	if err != nil {
		return nil, fmt.Errorf("rebaseline: checking active migration period: %w", err)
	}
	if active {
		return nil, fmt.Errorf("%w: a migration is in progress on schema %q; "+
			"complete it or run `pgroll rollback` first", ErrRebaselineRefused, schemaName)
	}

	// Every row at or below the anchor must be sealed with nothing deferred:
	// a baseline seals the boundary, and burying a pending contraction or an
	// open revert window behind it would strand that state forever.
	unsealed, err := collectNames(ctx, tx, fmt.Sprintf(`SELECT m.name FROM %[1]s.migrations m
		WHERE m.schema = $1 AND m.name <> $2 AND (NOT m.sealed OR m.complete_deferred)
		AND m.created_at <= (SELECT created_at FROM %[1]s.migrations WHERE schema = $1 AND name = $2)
		ORDER BY m.created_at LIMIT 5`, pq.QuoteIdentifier(s.schema)), schemaName, name)
	if err != nil {
		return nil, fmt.Errorf("rebaseline: checking for unsealed history below the anchor: %w", err)
	}
	if len(unsealed) > 0 {
		return nil, fmt.Errorf("%w: migration(s) at or below the anchor are unsealed or awaiting "+
			"a deferred complete: %s; contract them (`pgroll complete`) before rebaselining",
			ErrRebaselineRefused, strings.Join(unsealed, ", "))
	}

	// LatestBaseline picks by created_at DESC. A baseline at or after the
	// anchor's created_at would mask the conversion entirely (equality
	// included: SchemaHistory's cut is strict, so a tie is ambiguous).
	laterBaselines, err := collectNames(ctx, tx, fmt.Sprintf(`SELECT m.name FROM %[1]s.migrations m
		WHERE m.schema = $1 AND m.migration_type = 'baseline'
		AND m.created_at >= (SELECT created_at FROM %[1]s.migrations WHERE schema = $1 AND name = $2)
		ORDER BY m.created_at LIMIT 5`, pq.QuoteIdentifier(s.schema)), schemaName, name)
	if err != nil {
		return nil, fmt.Errorf("rebaseline: checking for later baselines: %w", err)
	}
	if len(laterBaselines) > 0 {
		return nil, fmt.Errorf("%w: baseline migration(s) exist at or after the anchor's "+
			"created_at: %s; the anchor would never be selected as the latest baseline",
			ErrRebaselineRefused, strings.Join(laterBaselines, ", "))
	}

	// Order agreement across the anchor boundary. SchemaHistory hides rows by
	// created_at; resolveLocalSet skips files by name. The conversion is only
	// safe when the two cuts select the same set. A row applied out of
	// filename order across the boundary breaks that: hidden-but-later-named
	// rows would be re-applied from disk, applied-but-earlier-named rows
	// would hard-fail as missing files. Ties on created_at count as below the
	// cut (SchemaHistory's > is strict), which is why both comparisons here
	// mirror its <= / > exactly.
	hiddenLaterNames, err := collectNames(ctx, tx, fmt.Sprintf(`SELECT m.name FROM %[1]s.migrations m
		WHERE m.schema = $1 AND m.name <> $2 AND m.migration_type <> 'inferred'
		AND m.created_at <= (SELECT created_at FROM %[1]s.migrations WHERE schema = $1 AND name = $2)
		AND m.name > $2
		ORDER BY m.name LIMIT 5`, pq.QuoteIdentifier(s.schema)), schemaName, name)
	if err != nil {
		return nil, fmt.Errorf("rebaseline: checking order agreement below the anchor: %w", err)
	}
	if len(hiddenLaterNames) > 0 {
		return nil, fmt.Errorf("%w: migration(s) applied at or before the anchor sort after its "+
			"name: %s; the baseline would hide them from history while their files remain "+
			"candidates, so `pgroll migrate` would re-apply them",
			ErrRebaselineRefused, strings.Join(hiddenLaterNames, ", "))
	}

	appliedEarlierNames, err := collectNames(ctx, tx, fmt.Sprintf(`SELECT m.name FROM %[1]s.migrations m
		WHERE m.schema = $1 AND m.name <> $2 AND m.migration_type <> 'inferred'
		AND m.created_at > (SELECT created_at FROM %[1]s.migrations WHERE schema = $1 AND name = $2)
		AND m.name <= $2
		ORDER BY m.name LIMIT 5`, pq.QuoteIdentifier(s.schema)), schemaName, name)
	if err != nil {
		return nil, fmt.Errorf("rebaseline: checking order agreement above the anchor: %w", err)
	}
	if len(appliedEarlierNames) > 0 {
		return nil, fmt.Errorf("%w: migration(s) applied after the anchor sort at or before its "+
			"name: %s; they would stay in visible history while their files sort below the "+
			"baseline, so `pgroll migrate` would fail on files missing from disk",
			ErrRebaselineRefused, strings.Join(appliedEarlierNames, ", "))
	}

	if dryRun {
		return result, nil
	}

	_, err = tx.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s.migrations SET migration_type = 'baseline',
			updated_at = CURRENT_TIMESTAMP WHERE schema = $1 AND name = $2`,
			pq.QuoteIdentifier(s.schema)),
		schemaName, name)
	if err != nil {
		return nil, fmt.Errorf("rebaseline: converting migration %q to baseline: %w", name, err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("rebaseline: committing transaction: %w", err)
	}
	return result, nil
}

// collectNames runs a query returning a single name column and collects the
// values.
func collectNames(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}
