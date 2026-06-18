// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"fmt"
	"slices"

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
// crash-safe under one invariant: *sealed precedes contraction*. Every
// queued row is stamped sealed before any contraction DDL runs, so no crash
// window can leave a physically-contracted row looking revertible —
// `pgroll revert` refuses sealed rows, and the still-set complete_deferred
// flags drive the resume: a re-run picks up where the previous attempt
// stopped. The last migration's flag clears only after the live projection
// has been recreated and the boundary snapshot refreshed, so no crash can
// leave the queue empty with the live schema missing. Re-running
// already-applied Complete actions is safe — they are idempotent by
// construction (catalog probes guard renames and constraint adds).
func (m *Roll) SealDeferredCompletes(ctx context.Context) (int, error) {
	queued, err := m.state.DeferredCompletes(ctx, m.schema)
	if err != nil {
		return 0, fmt.Errorf("unable to query deferred completes: %w", err)
	}
	if len(queued) == 0 {
		// Nothing to drain — but heal strands: drained defer-class rows
		// left done-and-unsealed by a crash between an older binary's
		// Complete drain and its (post-drain) seal stamp. Those rows brick
		// every revert (the window guard refuses them) and no drain will
		// ever stamp them, since the queue is empty. Inline-class unsealed
		// rows are NOT touched: their window may legitimately still be
		// open (e.g. a bounded revert re-opened the train).
		return 0, m.sealStrandedCompletes(ctx)
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

	// A non-empty queue is normally the previous train's — but it is also
	// what a crashed `pgroll migrate` leaves mid-train: the applied prefix's
	// defer-class rows, with the train never finished. Sealing that queue
	// would fire the point of no return mid-train AND, because the latest
	// migration is then a version-schema-less intermediate, drop the
	// previous train-final's schema — the one production apps are still
	// pinned to. Distinguish the two by physics:
	//   - any sealed row still queued  → an interrupted seal; resume it.
	//   - queue unsealed and the latest migration has no physical version
	//     schema → an unfinished train; skip the seal entirely so the
	//     caller (migrate) resumes the train. The whole train — crashed
	//     prefix and resumed suffix — seals together at the next departure.
	resuming, err := m.state.HasSealedDeferred(ctx, m.schema)
	if err != nil {
		return 0, err
	}
	if !resuming && !m.disableVersionSchemas {
		existing, err := m.ExistingVersionSchemas(ctx)
		if err != nil {
			return 0, err
		}
		if !slices.Contains(existing, VersionedSchemaName(m.schema, live.VersionSchemaName())) {
			m.logger.Info("skipping seal: the deferred queue belongs to an unfinished deployment "+
				"(latest migration has no version schema); re-run `pgroll migrate` to resume it",
				"latest", live.Name, "queued", len(queued))
			return 0, nil
		}
	}

	m.logger.Info("sealing previous deployment: draining deferred completes",
		"count", len(queued), "live", live.Name)

	// Seal at intent: stamp the whole deployment (deferred and inline rows
	// alike) BEFORE any contraction DDL runs. From this point `pgroll
	// revert` refuses every row, so no crash window below can present a
	// physically-contracted row as losslessly revertible. The still-set
	// complete_deferred flags — not the sealed bit — drive the resume of an
	// interrupted seal.
	if _, err := m.state.MarkSealed(ctx, m.schema); err != nil {
		return 0, err
	}

	// The live version schema only needs to be dropped (and rebuilt afterwards)
	// when the drain will actually contract a user-facing identifier its views
	// project — a DROP/RENAME of a column or table, or a duplicator's Complete.
	// An additive drain (e.g. a deferred final migration kept queued only for
	// the revert window) touches just pgroll-internal artifacts the live views
	// never reference, so dropping the live schema is unnecessary and only
	// risks an unwinnable lock fight with the apps actively reading it.
	needsLiveDrop := false
	for _, dm := range queued {
		if dm.CompleteAffectsLiveProjection() {
			needsLiveDrop = true
			break
		}
	}

	if !m.disableVersionSchemas {
		// Reap every dead intermediate version schema. Best-effort: a schema a
		// live backend is still reading must not be dropped (it would break
		// that session) and must not fail the deployment — collecting garbage
		// cannot sit on the critical path of applying new work. Such schemas
		// are carried forward and reaped by a later deployment once idle.
		deferred, err := m.ReapVersionSchemasExcept(ctx, live.VersionSchemaName())
		if err != nil {
			return 0, fmt.Errorf("unable to drop old version schemas: %w", err)
		}
		if len(deferred) > 0 {
			m.logger.Warn("deferred schema cleanup: intermediate schemas still in use; "+
				"a later deployment will reap them", "schemas", deferred)
		}

		versionSchema := VersionedSchemaName(m.schema, live.VersionSchemaName())
		if needsLiveDrop {
			// Required for contraction and therefore strict: it cannot be
			// deferred (the drain's DROP/RENAME would fail on the view's
			// deptype=n dependency), so a blocked drop must fail the seal
			// rather than corrupt it. Name the blocking sessions so the
			// straggler (apps still reading the live schema) can be cleared.
			if _, err := m.pgConn.ExecContext(ctx,
				fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", pq.QuoteIdentifier(versionSchema))); err != nil {
				if isLockNotAvailable(err) {
					return 0, fmt.Errorf("unable to drop live version schema %q before contraction "+
						"(apps still reading it must move off it first); blocked by:%s: %w",
						versionSchema, m.schemaLockHolders(ctx, versionSchema), err)
				}
				return 0, fmt.Errorf("unable to drop live version schema before drain: %w", err)
			}
		} else {
			m.logger.Info("seal drain is projection-preserving; keeping the live version schema in place",
				"schema", versionSchema)
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

	if !m.disableVersionSchemas && needsLiveDrop {
		// Only rebuild what was dropped. A projection-preserving drain left the
		// live schema in place, so its views are still valid and re-running
		// ensureViews would needlessly reacquire the AccessExclusive locks the
		// drop was skipped to avoid.
		currentSchema, err := m.state.ReadSchema(ctx, m.schema)
		if err != nil {
			return 0, fmt.Errorf("unable to read schema after drain: %w", err)
		}
		if err := m.ensureViews(ctx, currentSchema, live); err != nil {
			return 0, fmt.Errorf("unable to recreate live version schema: %w", err)
		}
	}

	if err := m.state.ClearCompleteDeferred(ctx, m.schema, last.Name); err != nil {
		return 0, fmt.Errorf("unable to clear complete_deferred for %q: %w", last.Name, err)
	}
	m.logger.Info("drained deferred complete", "migration", last.Name)
	m.logger.Info("previous deployment sealed", "drained", len(queued))

	return len(queued), nil
}

// sealStrandedCompletes stamps drained-but-unsealed defer-class rows: done
// migrations with destructive (defer-class) operations whose complete_deferred
// flag has cleared but which were never sealed. That state arises only from a
// crash between a drain and its seal stamp on an older binary (current code
// stamps before draining) or from manual state manipulation — the row's
// contraction has physically run, so it must be sealed, and the revert-window
// guard refuses every revert until it is.
func (m *Roll) sealStrandedCompletes(ctx context.Context) error {
	records, err := m.state.UnsealedMigrations(ctx, m.schema)
	if err != nil {
		return err
	}
	var stranded []string
	for _, r := range records {
		if r.Done && !r.CompleteDeferred && r.Migration.CompleteMustBeDeferred() {
			stranded = append(stranded, r.Name)
		}
	}
	if len(stranded) == 0 {
		return nil
	}
	m.logger.Info("sealing stranded drained migrations from an interrupted complete", "migrations", stranded)
	return m.state.MarkSealedByName(ctx, m.schema, stranded)
}

// SealWindow stamps every unsealed done migration as sealed without draining
// anything, returning the number of rows stamped. This is the explicit
// "close the revert window now" affordance for windows with nothing queued —
// e.g. an inline-only window left by a bounded revert. `pgroll complete`
// uses it after SealDeferredCompletes so a manual seal always closes the
// window completely.
func (m *Roll) SealWindow(ctx context.Context) (int64, error) {
	return m.state.MarkSealed(ctx, m.schema)
}
