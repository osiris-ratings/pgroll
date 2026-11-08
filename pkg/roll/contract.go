// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"fmt"
)

// FinishContraction contracts whatever the current deployment left pending
// when there is no active migration: it drains the deferred-complete queue
// (the intra-deploy buffer of destructive DDL), stamps the deployment sealed,
// and drops every version schema except the leaf's. `pgroll complete` calls
// this when it finds no migration in progress — the normal case is a deploy
// whose `pgroll migrate` batch consisted only of already-done rows with
// queued contraction (e.g. a resumed run), or a database upgraded from the
// delayed-contraction lifecycle with its final window still open.
//
// Returns the number of deferred migrations drained and the number of rows
// stamped sealed. Idempotent and crash-safe under one invariant: *sealed
// precedes contraction*. Every queued row is stamped sealed before any
// contraction DDL runs, so no crash window can leave a physically-contracted
// row looking revertible — `pgroll revert` refuses sealed rows, and the
// still-set complete_deferred flags (not the sealed bit) drive the resume of
// an interrupted run. Re-running already-applied Complete actions is safe —
// they are idempotent by construction (catalog probes guard renames and
// constraint adds).
//
// The leaf's version schema — the projection apps are pinned to — stays in
// place through the drain and is never rebuilt. A contraction refuses to start
// without it, because it drops every other version schema; onComplete raw SQL
// runs in a transaction that keeps its views (see guardLiveViews).
func (m *Roll) FinishContraction(ctx context.Context, opts ...ContractOption) (int, int64, error) {
	var o contractOptions
	for _, opt := range opts {
		opt(&o)
	}

	queued, err := m.state.DeferredCompletes(ctx, m.schema)
	if err != nil {
		return 0, 0, fmt.Errorf("unable to query deferred completes: %w", err)
	}
	if len(queued) == 0 {
		// Nothing to drain — close any open window: done rows never stamped
		// sealed, whether stranded by an older binary's crash between drain
		// and stamp, or legitimately open (e.g. a train re-opened by a
		// bounded revert). `pgroll complete` means "finish this deployment",
		// so the stamp is always intended.
		stamped, err := m.state.MarkSealed(ctx, m.schema)
		if err != nil {
			return 0, 0, err
		}
		// Converge the schemas too: inline-class intermediates skip their
		// old-schema cleanup ("deferred to the next non-deferred Complete"),
		// and this is that Complete. Guarded on the leaf's projection
		// physically existing — a version-schema-less leaf means an
		// unfinished batch, whose previous-deployment schema must survive.
		if !m.disableVersionSchemas {
			latest, err := m.state.LatestMigration(ctx, m.schema)
			if err != nil {
				return 0, 0, fmt.Errorf("unable to determine latest migration: %w", err)
			}
			if latest != nil {
				live, err := m.state.GetMigration(ctx, m.schema, *latest)
				if err != nil {
					return 0, 0, fmt.Errorf("unable to load latest migration: %w", err)
				}
				ok, err := m.hasVersionSchema(ctx, live)
				if err != nil {
					return 0, 0, err
				}
				if ok {
					if err := m.DropVersionSchemasExcept(ctx, live.VersionSchemaName()); err != nil {
						return 0, stamped, fmt.Errorf("unable to drop old version schemas: %w", err)
					}
				} else {
					m.logger.Info("leaving version schemas in place: the leaf migration has no projection "+
						"(an unfinished batch?)", "leaf", live.Name)
				}
			}
		}
		return 0, stamped, nil
	}

	// The live projection belongs to the leaf migration; under an
	// end-of-deploy contraction that is this deployment's final migration,
	// which is also the last row in the queue.
	latest, err := m.state.LatestMigration(ctx, m.schema)
	if err != nil {
		return 0, 0, fmt.Errorf("unable to determine latest migration: %w", err)
	}
	if latest == nil {
		return 0, 0, fmt.Errorf("deferred completes are queued but migration history is empty")
	}
	live, err := m.state.GetMigration(ctx, m.schema, *latest)
	if err != nil {
		return 0, 0, fmt.Errorf("unable to load latest migration: %w", err)
	}
	if last := queued[len(queued)-1]; last.Name != live.Name {
		// Not fatal — drain order and view recreation stay correct — but it
		// means the queue predates the current lifecycle or was manipulated.
		m.logger.Info("latest migration is not the last deferred migration",
			"latest", live.Name, "lastDeferred", last.Name)
	}

	// A non-empty queue with no active migration is normally a finished batch
	// awaiting contraction — but it is also what a crashed `pgroll migrate`
	// leaves mid-train: the applied prefix's defer-class rows, with the train
	// never finished. Contracting that queue would fire the point of no
	// return mid-train AND, because the leaf is then a version-schema-less
	// intermediate, drop the previous deployment's schema — the one apps are
	// still pinned to. Contraction never drops the leaf's schema, so it exists
	// whenever contraction may run, interrupted or not: refuse without it.
	// With version schemas disabled there is nothing to probe, so a queue
	// with no sealed row is refused unless forced.
	if m.disableVersionSchemas {
		resuming, err := m.state.HasSealedDeferred(ctx, m.schema)
		if err != nil {
			return 0, 0, err
		}
		if !resuming && !o.force {
			// No version schema exists to probe, so the physical test below is
			// unavailable and this queue cannot be told apart from a crashed
			// batch's leftovers by state alone.
			//
			// Three things produce this exact shape — queue non-empty, nothing
			// active, nothing sealed — and only the first is dangerous:
			//
			//   1. a `pgroll migrate` that died mid-train, leaving the applied
			//      prefix's defer-class rows queued. Draining fires the
			//      destructive half of a batch whose forward half never ran.
			//   2. a train re-opened by a bounded revert.
			//   3. a database carried over from the delayed-contraction
			//      lifecycle with its window still open.
			//
			// Telling them apart needs to know whether migrations beyond the
			// queue are still unapplied, which is a property of the migrations
			// directory and not of this database. So refuse by default — (1)
			// is unrecoverable and (2) and (3) are not — and say plainly that
			// --force is the way through for the harmless two, rather than
			// pointing at `pgroll migrate`, which is a no-op in both.
			return 0, 0, fmt.Errorf(
				"cannot contract: %d deferred completion(s) are queued with nothing active and version "+
					"schemas disabled, so pgroll cannot confirm the batch finished. If a `pgroll migrate` "+
					"was interrupted, resume it by re-running `pgroll migrate`. If this window was opened "+
					"by a bounded revert or carried over from an older lifecycle, the queue is safe to "+
					"drain: re-run with --force", len(queued),
			)
		}
	} else {
		ok, err := m.hasVersionSchema(ctx, live)
		if err != nil {
			return 0, 0, err
		}
		if !ok {
			versionSchema := VersionedSchemaName(m.schema, live.VersionSchemaName())
			return 0, 0, fmt.Errorf(
				"cannot contract: the latest migration %q has no version schema %q, and contraction drops "+
					"every other one. If the deferred queue belongs to an unfinished `pgroll migrate` batch, "+
					"resume it by re-running `pgroll migrate`, or abort it with `pgroll revert`. If apps are "+
					"pinned to %q, restore it with `pgroll materialize` first", live.Name, versionSchema, versionSchema,
			)
		}
	}

	m.logger.Info("contracting deployment: draining deferred completes",
		"count", len(queued), "live", live.Name)

	// Seal at intent: stamp the whole deployment (deferred and inline rows
	// alike) BEFORE any contraction DDL runs. From this point `pgroll revert`
	// refuses every row, so no crash window below can present a physically-
	// contracted row as losslessly revertible.
	if _, err := m.state.MarkSealed(ctx, m.schema); err != nil {
		return 0, 0, err
	}

	if !m.disableVersionSchemas {
		// Drop every version schema except the leaf's. Strict: with
		// contraction at the end of the deploy that created it, a schema
		// still held by another backend means something is still reading a
		// projection this deployment is retiring — fail loudly (naming the
		// blocking sessions) so the operator repins the straggler and
		// re-runs, rather than carrying the schema forward as garbage.
		if err := m.DropVersionSchemasExcept(ctx, live.VersionSchemaName()); err != nil {
			return 0, 0, fmt.Errorf("unable to drop old version schemas: %w", err)
		}
	}

	for _, dm := range queued[:len(queued)-1] {
		if err := m.drainDeferredMigration(ctx, dm, live, nil); err != nil {
			return 0, 0, err
		}
	}

	// The last migration's flag must outlive the boundary snapshot: clear it
	// only after the snapshot is refreshed, so an interrupted contraction
	// always re-runs with a non-empty queue.
	last := queued[len(queued)-1]
	if err := m.applyDeferredComplete(ctx, last, live, nil); err != nil {
		return 0, 0, err
	}

	if err := m.state.RefreshResultingSchema(ctx, m.schema, live.Name); err != nil {
		return 0, 0, fmt.Errorf("unable to refresh boundary snapshot for %q: %w", live.Name, err)
	}

	if err := m.state.ClearCompleteDeferred(ctx, m.schema, last.Name); err != nil {
		return 0, 0, fmt.Errorf("unable to clear complete_deferred for %q: %w", last.Name, err)
	}
	m.logger.Info("drained deferred complete", "migration", last.Name)

	// Close the window completely: stamp any inline-class rows of the same
	// deployment that became done after the pre-drain stamp.
	stamped, err := m.state.MarkSealed(ctx, m.schema)
	if err != nil {
		return 0, 0, err
	}
	m.logger.Info("deployment contracted", "drained", len(queued))

	return len(queued), stamped, nil
}
