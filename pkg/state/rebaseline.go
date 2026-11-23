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

// FoldSet names the migrations a baseline folds in: the ones whose files a
// history truncation deleted, anchor excluded or not. It lets the conversion
// check a database against the set itself instead of against the order the
// database happened to apply it in.
type FoldSet struct {
	// All is every folded migration, whichever database it is routed to.
	All []string
	// Required is the subset this database must have applied: the entries
	// its target selects.
	Required []string
}

// ConvertMigrationToBaseline converts an already-applied migration row into a
// baseline in place: UPDATE migration_type = 'baseline', touching nothing
// else. The baseline then folds in every row whose name sorts at or before
// the anchor's (see SchemaHistory), which is the set of files resolveLocalSet
// ignores. This is the safe primitive for truncating migration history: a
// fresh baseline row (CreateBaseline, Stamp) records only the schema as it
// stands, and says nothing about which migrations produced it.
//
// The conversion runs in one transaction with the anchor row locked, and
// refuses (wrapping ErrRebaselineRefused) unless:
//
//   - the anchor row exists, is done, sealed, and not awaiting a deferred
//     complete (an uncontracted anchor still has revert state behind it)
//   - no migration is in progress on the schema
//   - no existing baseline's name sorts after the anchor's (LatestBaseline
//     picks by name, so that baseline would mask the conversion entirely)
//
// and the rows being folded pass one of two audits.
//
// With a FoldSet, the audit is on the set. The rows in question are the
// non-inferred ones named above this database's own baseline and at or below
// the anchor:
//
//   - each is sealed with no deferred complete (a baseline must not bury a
//     pending contraction or an open revert window)
//   - each is in FoldSet.All. A row outside it was applied from a file the
//     truncation never saw, so the baseline's snapshot does not describe it
//   - every name in FoldSet.Required above the database's own baseline has
//     such a row. A missing one is a migration this database never applied
//     and, with its file deleted, never would
//
// The last check is skipped for a database that applied the anchor as the
// baseline file itself (it was built from the truncated directory, and got
// the folded migrations through the snapshot). A fold set has to reach back
// to the oldest baseline a database may still be on, so it is cumulative:
// each truncation carries the previous one's entries forward.
//
// Apply order is not examined: both cuts are by name, so they agree however
// the rows are ordered.
//
// Without a FoldSet (a baseline written before fold sets existed) nothing
// says which migrations the baseline covers, so the conversion falls back to
// requiring that name order agree with apply order across the anchor: no row
// applied at or before the anchor sorts after it, and none applied after it
// sorts at or before. Everything applied up to the anchor must be sealed.
//
// dryRun runs every audit and reports the result without writing anything.
func (s *State) ConvertMigrationToBaseline(
	ctx context.Context,
	schemaName, name string,
	folds *FoldSet,
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
		appliedSnapshot  bool
	)
	err = tx.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT migration_type, done, sealed, complete_deferred,
			resulting_schema = '{}'::jsonb,
			COALESCE(migration->>'baseline', 'false') = 'true'
			FROM %s.migrations WHERE schema = $1 AND name = $2 FOR UPDATE`,
			pq.QuoteIdentifier(s.schema)),
		schemaName, name).Scan(&migrationType, &done, &sealed, &completeDeferred, &emptySchema, &appliedSnapshot)
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

	// LatestBaseline picks the baseline whose name sorts last, so one above
	// the anchor would mask the conversion entirely.
	laterBaselines, err := collectNames(ctx, tx, fmt.Sprintf(`SELECT m.name FROM %[1]s.migrations m
		WHERE m.schema = $1 AND m.migration_type = 'baseline'
		AND m.name COLLATE "C" > $2 COLLATE "C"
		ORDER BY m.name COLLATE "C" LIMIT 5`, pq.QuoteIdentifier(s.schema)), schemaName, name)
	if err != nil {
		return nil, fmt.Errorf("rebaseline: checking for later baselines: %w", err)
	}
	if len(laterBaselines) > 0 {
		return nil, fmt.Errorf("%w: baseline migration(s) sort after the anchor: %s; "+
			"the anchor would never be selected as the latest baseline",
			ErrRebaselineRefused, strings.Join(laterBaselines, ", "))
	}

	if folds != nil {
		err = auditFoldSet(ctx, tx, s.schema, schemaName, name, folds, appliedSnapshot)
	} else {
		err = auditOrderAgreement(ctx, tx, s.schema, schemaName, name)
	}
	if err != nil {
		return nil, err
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

// maxNamesInRefusal bounds how many migration names a refusal lists.
const maxNamesInRefusal = 5

// auditFoldSet checks the rows the anchor would fold in against the set the
// baseline declares. See ConvertMigrationToBaseline.
//
// appliedSnapshot is true when this database applied the anchor as the
// baseline file itself: it was built from the truncated directory, so the
// folded migrations reached it through the snapshot and it has no rows for
// them to check.
func auditFoldSet(
	ctx context.Context, tx *sql.Tx,
	stateSchema, schemaName, anchor string,
	folds *FoldSet, appliedSnapshot bool,
) error {
	var ownBaseline string
	err := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT COALESCE((
			SELECT b.name FROM %[1]s.migrations b
			WHERE b.schema = $1 AND b.migration_type = 'baseline'
			ORDER BY b.name COLLATE "C" DESC LIMIT 1
		), '')`, pq.QuoteIdentifier(stateSchema)), schemaName).Scan(&ownBaseline)
	if err != nil {
		return fmt.Errorf("rebaseline: reading this database's baseline: %w", err)
	}

	// Names are compared byte-wise, as resolveLocalSet compares filenames:
	// in Go below, and under the C collation here, since the database's
	// default collation may order them differently.
	contracted := map[string]bool{}
	var applied []string
	err = func() error {
		rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT m.name, m.sealed AND NOT m.complete_deferred
			FROM %[1]s.migrations m
			WHERE m.schema = $1 AND m.name <> $2 AND m.migration_type <> 'inferred'
			AND NOT (m.migration ? 'revert_of')
			AND m.name COLLATE "C" <= $2 COLLATE "C"
			AND m.name COLLATE "C" > $3 COLLATE "C"
			ORDER BY m.name COLLATE "C"`, pq.QuoteIdentifier(stateSchema)), schemaName, anchor, ownBaseline)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			var ok bool
			if err := rows.Scan(&n, &ok); err != nil {
				return err
			}
			contracted[n] = ok
			applied = append(applied, n)
		}
		return rows.Err()
	}()
	if err != nil {
		return fmt.Errorf("rebaseline: reading the rows to fold: %w", err)
	}

	var unsealed []string
	for _, n := range applied {
		if !contracted[n] {
			unsealed = append(unsealed, n)
		}
	}
	if len(unsealed) > 0 {
		return fmt.Errorf("%w: migration(s) the baseline folds in are unsealed or awaiting "+
			"a deferred complete: %s; contract them (`pgroll complete`) before rebaselining",
			ErrRebaselineRefused, listNames(unsealed))
	}

	declared := make(map[string]struct{}, len(folds.All))
	for _, n := range folds.All {
		declared[n] = struct{}{}
	}
	var undeclared []string
	for _, n := range applied {
		if _, ok := declared[n]; !ok {
			undeclared = append(undeclared, n)
		}
	}
	if len(undeclared) > 0 {
		return fmt.Errorf("%w: this database applied migration(s) that sort at or before the "+
			"anchor but are not among those the baseline folds in: %s. Either they were "+
			"applied from files the truncation never saw (a branch, a since-renamed "+
			"migration): prune them. Or this database's own baseline (%q) is older than the "+
			"fold set reaches back: catch up from a checkout that still has those files. "+
			"Otherwise rebuild the database",
			ErrRebaselineRefused, listNames(undeclared), ownBaseline)
	}

	if appliedSnapshot {
		return nil
	}

	var missing []string
	for _, n := range folds.Required {
		if n == anchor || n <= ownBaseline {
			continue
		}
		if _, ok := contracted[n]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: this database never applied migration(s) the baseline folds "+
			"in: %s; their files are gone from this checkout, so nothing would apply them. "+
			"Catch up by running `pgroll migrate` from a checkout that still has them, or "+
			"rebuild the database",
			ErrRebaselineRefused, listNames(missing))
	}
	return nil
}

// auditOrderAgreement is the audit for a baseline that declares no fold set.
// See ConvertMigrationToBaseline.
func auditOrderAgreement(ctx context.Context, tx *sql.Tx, stateSchema, schemaName, name string) error {
	// Every row applied up to the anchor must be sealed with nothing
	// deferred: a baseline seals the boundary, and burying a pending
	// contraction or an open revert window behind it would strand that state
	// forever.
	unsealed, err := collectNames(ctx, tx, fmt.Sprintf(`SELECT m.name FROM %[1]s.migrations m
		WHERE m.schema = $1 AND m.name <> $2 AND (NOT m.sealed OR m.complete_deferred)
		AND m.created_at <= (SELECT created_at FROM %[1]s.migrations WHERE schema = $1 AND name = $2)
		ORDER BY m.created_at LIMIT 5`, pq.QuoteIdentifier(stateSchema)), schemaName, name)
	if err != nil {
		return fmt.Errorf("rebaseline: checking for unsealed history below the anchor: %w", err)
	}
	if len(unsealed) > 0 {
		return fmt.Errorf("%w: migration(s) at or below the anchor are unsealed or awaiting "+
			"a deferred complete: %s; contract them (`pgroll complete`) before rebaselining",
			ErrRebaselineRefused, strings.Join(unsealed, ", "))
	}

	appliedBeforeNamedAfter, err := collectNames(ctx, tx, fmt.Sprintf(`SELECT m.name FROM %[1]s.migrations m
		WHERE m.schema = $1 AND m.name <> $2 AND m.migration_type <> 'inferred'
		AND m.created_at <= (SELECT created_at FROM %[1]s.migrations WHERE schema = $1 AND name = $2)
		AND m.name COLLATE "C" > $2 COLLATE "C"
		ORDER BY m.name COLLATE "C" LIMIT 5`, pq.QuoteIdentifier(stateSchema)), schemaName, name)
	if err != nil {
		return fmt.Errorf("rebaseline: checking order agreement below the anchor: %w", err)
	}
	if len(appliedBeforeNamedAfter) > 0 {
		return fmt.Errorf("%w: migration(s) applied at or before the anchor sort after its "+
			"name: %s; the baseline declares no fold set, so it can only be adopted by a "+
			"database that applied its migrations in name order",
			ErrRebaselineRefused, strings.Join(appliedBeforeNamedAfter, ", "))
	}

	appliedAfterNamedBefore, err := collectNames(ctx, tx, fmt.Sprintf(`SELECT m.name FROM %[1]s.migrations m
		WHERE m.schema = $1 AND m.name <> $2 AND m.migration_type <> 'inferred'
		AND m.created_at > (SELECT created_at FROM %[1]s.migrations WHERE schema = $1 AND name = $2)
		AND m.name COLLATE "C" <= $2 COLLATE "C"
		ORDER BY m.name COLLATE "C" LIMIT 5`, pq.QuoteIdentifier(stateSchema)), schemaName, name)
	if err != nil {
		return fmt.Errorf("rebaseline: checking order agreement above the anchor: %w", err)
	}
	if len(appliedAfterNamedBefore) > 0 {
		return fmt.Errorf("%w: migration(s) applied after the anchor sort at or before its "+
			"name: %s; the baseline declares no fold set, so it can only be adopted by a "+
			"database that applied its migrations in name order",
			ErrRebaselineRefused, strings.Join(appliedAfterNamedBefore, ", "))
	}
	return nil
}

// listNames joins names for a refusal, naming the first few and counting
// the rest.
func listNames(names []string) string {
	if len(names) <= maxNamesInRefusal {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more",
		strings.Join(names[:maxNamesInRefusal], ", "), len(names)-maxNamesInRefusal)
}
