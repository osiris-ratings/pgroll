// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"fmt"

	"github.com/lib/pq"
)

// SealDeferredCompletes drains the deferred-complete queue left by the
// previous deployment, applying the queued (destructive) DDL. This is the
// previous train's point of no return: until seal runs, every deferred
// migration is physically still in its expand phase — old columns alive,
// dual-write triggers running — and `pgroll revert` can walk back out of it
// losslessly. After seal, contraction has happened.
//
// Called by `pgroll migrate` before applying a new batch (the next train
// departing closes the previous train's revert window) and by
// `pgroll complete` when there is no active migration (manual contraction,
// closing the window on demand).
//
// The live version schema — the latest done migration's projection, which
// production apps are pinned to — must be dropped before the drain: drain
// DDL drops and renames user-facing columns its views project, and
// pg_depend records those view→column edges as deptype=n, which DROP COLUMN
// refuses to cascade. It is recreated from the post-drain physical state
// immediately after, restoring an identical projection over the contracted
// columns. This is the same brief self-healing window the non-deferred
// `--complete` drain has always had, shifted one deployment later.
//
// Returns the number of deferred migrations drained. Idempotent and
// crash-safe: every drained migration except the last clears its flag
// independently, so a re-run resumes where the previous attempt stopped;
// the last migration's flag clears only after the live projection has been
// recreated and the boundary snapshot refreshed, so no crash can leave the
// queue empty with the live schema missing. Re-running already-applied
// Complete actions is safe — they are idempotent by construction (catalog
// probes guard renames and constraint adds).
func (m *Roll) SealDeferredCompletes(ctx context.Context) (int, error) {
	queued, err := m.state.DeferredCompletes(ctx, m.schema)
	if err != nil {
		return 0, fmt.Errorf("unable to query deferred completes: %w", err)
	}
	if len(queued) == 0 {
		return 0, nil
	}

	// The live projection belongs to the latest done migration — under
	// delayed contraction that is the previous train's final migration,
	// which is also the last row in the queue.
	latest, err := m.state.LatestMigration(ctx, m.schema)
	if err != nil {
		return 0, fmt.Errorf("unable to determine latest migration: %w", err)
	}
	if latest == nil {
		return 0, fmt.Errorf("deferred completes are queued but migration history is empty")
	}
	live, err := m.state.GetMigration(ctx, m.schema, *latest)
	if err != nil {
		return 0, fmt.Errorf("unable to load latest migration: %w", err)
	}
	if last := queued[len(queued)-1]; last.Name != live.Name {
		// Not fatal — drain order and view recreation stay correct — but it
		// means the queue predates delayed contraction or was manipulated.
		m.logger.Info("latest migration is not the last deferred migration",
			"latest", live.Name, "lastDeferred", last.Name)
	}

	m.logger.Info("sealing previous deployment: draining deferred completes",
		"count", len(queued), "live", live.Name)

	if !m.disableVersionSchemas {
		// Reap every version schema except the live one, then drop the live
		// one itself so the drain DDL is unblocked (see doc comment).
		if err := m.DropVersionSchemasExcept(ctx, live.VersionSchemaName()); err != nil {
			return 0, fmt.Errorf("unable to drop old version schemas: %w", err)
		}
		versionSchema := VersionedSchemaName(m.schema, live.VersionSchemaName())
		if _, err := m.pgConn.ExecContext(ctx,
			fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", pq.QuoteIdentifier(versionSchema))); err != nil {
			return 0, fmt.Errorf("unable to drop live version schema before drain: %w", err)
		}
	}

	for _, dm := range queued[:len(queued)-1] {
		if err := m.drainDeferredMigration(ctx, dm); err != nil {
			return 0, err
		}
	}

	// The last migration's flag must outlive the projection rebuild: clear
	// it only after the live schema and boundary snapshot are restored, so
	// an interrupted seal always re-runs with a non-empty queue.
	last := queued[len(queued)-1]
	if err := m.applyDeferredComplete(ctx, last); err != nil {
		return 0, err
	}

	if err := m.state.RefreshResultingSchema(ctx, m.schema, live.Name); err != nil {
		return 0, fmt.Errorf("unable to refresh boundary snapshot for %q: %w", live.Name, err)
	}

	if !m.disableVersionSchemas {
		currentSchema, err := m.state.ReadSchema(ctx, m.schema)
		if err != nil {
			return 0, fmt.Errorf("unable to read schema after drain: %w", err)
		}
		if err := m.ensureViews(ctx, currentSchema, live); err != nil {
			return 0, fmt.Errorf("unable to recreate live version schema: %w", err)
		}
	}

	// Stamp the whole drained deployment (deferred and inline rows alike) as
	// sealed — past the point of no return for `pgroll revert`. Stamped
	// before the last flag clears: a crash in between leaves a sealed row
	// still in the queue, so revert refuses (contraction partially ran) and
	// a re-run of the seal finishes the job.
	if err := m.state.MarkSealed(ctx, m.schema); err != nil {
		return 0, err
	}

	if err := m.state.ClearCompleteDeferred(ctx, m.schema, last.Name); err != nil {
		return 0, fmt.Errorf("unable to clear complete_deferred for %q: %w", last.Name, err)
	}
	m.logger.Info("drained deferred complete", "migration", last.Name)
	m.logger.Info("previous deployment sealed", "drained", len(queued))

	return len(queued), nil
}
