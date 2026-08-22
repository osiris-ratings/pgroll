// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/xataio/pgroll/pkg/migrations"
)

// MigrationType classifies a row in pgroll.migrations. The constants mirror
// the values stored by pgroll itself (Start writes "pgroll" by default,
// CreateBaseline writes "baseline", and the inferred-migration trigger writes
// "inferred"). They are surfaced here so `pgroll stamp` callers can pick a
// type explicitly.
const (
	MigrationTypePgroll   = "pgroll"
	MigrationTypeBaseline = "baseline"
	MigrationTypeInferred = "inferred"
)

// Stamp records a chain of migrations as already-applied in pgroll's state
// without executing any DDL. It is alembic-style stamping: "the database is
// already in this state, just record the rows."
//
// Each input migration becomes one row in pgroll.migrations with done=TRUE,
// the parsed migration body, the supplied migrationType, and the parent chain
// wired in input order. The final row's resulting_schema is set to the live
// schema (via state.ReadSchema); intermediate rows store the SQL default
// '{}'. This matches what `pgroll baseline` writes and is what downstream
// callers (state.SchemaAfterMigration, Roll.Status) need from the leaf.
//
// Already-recorded names are skipped silently — Stamp is idempotent. The
// returned slice contains the names that were newly inserted, in input
// order. Refuses if there is an active migration period; the caller should
// run `pgroll rollback` first.
//
// No virtual-schema replay happens. Stamp does not need to understand
// operation semantics — it stores the body verbatim and trusts the caller
// that the live DB already reflects the cumulative effect of the chain.
func (m *Roll) Stamp(
	ctx context.Context,
	migs []*migrations.RawMigration,
	migrationType string,
) ([]string, error) {
	if len(migs) == 0 {
		return nil, nil
	}

	active, err := m.state.IsActiveMigrationPeriod(ctx, m.schema)
	if err != nil {
		return nil, fmt.Errorf("stamp: checking active migration period: %w", err)
	}
	if active {
		return nil, fmt.Errorf(
			"stamp: a migration is currently in progress on schema %q; run `pgroll rollback` first",
			m.schema,
		)
	}

	// Find the new (unstamped) names up-front so we know which one is the
	// leaf — only the leaf needs a live resulting_schema read. Walks the
	// input slice once and skips already-recorded names.
	type pending struct {
		raw  *migrations.RawMigration
		body []byte
	}
	var todo []pending
	for _, raw := range migs {
		exists, err := m.state.MigrationExists(ctx, m.schema, raw.Name)
		if err != nil {
			return nil, fmt.Errorf("stamp: %w", err)
		}
		if exists {
			m.logger.Info("stamp: skipping already-recorded migration", "name", raw.Name)
			continue
		}
		body, err := json.Marshal(raw)
		if err != nil {
			return nil, fmt.Errorf("stamp: marshalling migration %q: %w", raw.Name, err)
		}
		todo = append(todo, pending{raw: raw, body: body})
	}

	if len(todo) == 0 {
		return nil, nil
	}

	// Resolve the parent for the first new row: prefer the most-recent
	// state.LatestMigration. Subsequent rows chain off the previous new
	// row's name, building a linear parent chain explicitly rather than
	// relying on latest_migration() to resolve mid-batch.
	var parent *string
	if first, err := m.state.LatestMigration(ctx, m.schema); err != nil {
		return nil, fmt.Errorf("stamp: reading latest migration: %w", err)
	} else if first != nil && *first != "" {
		v := *first
		parent = &v
	}

	// Live schema snapshot, marshalled once, applied to the LAST row only.
	var leafSchemaJSON []byte
	if sc, err := m.state.ReadSchema(ctx, m.schema); err != nil {
		return nil, fmt.Errorf("stamp: reading live schema: %w", err)
	} else if leafSchemaJSON, err = json.Marshal(sc); err != nil {
		return nil, fmt.Errorf("stamp: marshalling live schema: %w", err)
	}

	stamped := make([]string, 0, len(todo))
	for i, p := range todo {
		var resulting []byte
		if i == len(todo)-1 {
			resulting = leafSchemaJSON
		}
		// A file marked `baseline: true` IS a baseline, whatever --type the
		// caller passed: recording it as an ordinary migration would leave
		// the chain unanchored, and the next directory reconciliation would
		// hard-fail on every pre-baseline file missing from disk. This is
		// what keeps the dump-restore recovery flow (load dump → stamp the
		// directory) correct against a truncated migrations directory.
		typ := migrationType
		if p.raw.Baseline {
			typ = MigrationTypeBaseline
		}
		if err := m.state.Stamp(
			ctx, m.schema, p.raw.Name, p.body, resulting, parent, typ,
		); err != nil {
			return stamped, err
		}
		m.logger.Info("stamped migration", "name", p.raw.Name, "type", typ)
		stamped = append(stamped, p.raw.Name)
		name := p.raw.Name
		parent = &name
	}

	return stamped, nil
}
