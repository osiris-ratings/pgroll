// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"fmt"
	"slices"

	"github.com/xataio/pgroll/pkg/state"
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
// place through the drain, and the contraction keeps it whole whether it
// succeeds, fails or is cancelled: missing views are put back first and last,
// and after any onComplete raw SQL (see Complete and liveGuard).
func (m *Roll) FinishContraction(ctx context.Context, opts ...ContractOption) (_ int, _ int64, err error) {
	var o contractOptions
	for _, opt := range opts {
		opt(&o)
	}

	queued, err := m.state.DeferredCompletes(ctx, m.schema)
	if err != nil {
		return 0, 0, fmt.Errorf("unable to query deferred completes: %w", err)
	}
	if len(queued) == 0 {
		// A leaf that was already contracted (sealed) before this call is live:
		// if a contraction left its version schema missing (an older binary's
		// retry could report success without rebuilding it), put it back. An
		// unsealed leaf without one is an unfinished batch, left alone below.
		contracted, err := m.contractedLeafGuard(ctx)
		if err != nil {
			return 0, 0, err
		}

		// Nothing to drain — close any open window: done rows never stamped
		// sealed, whether stranded by an older binary's crash between drain
		// and stamp, or legitimately open (e.g. a train re-opened by a
		// bounded revert). `pgroll complete` means "finish this deployment",
		// so the stamp is always intended.
		stamped, err := m.state.MarkSealed(ctx, m.schema)
		if err != nil {
			return 0, 0, err
		}
		if _, err := m.restoreLiveViews(ctx, contracted, false); err != nil {
			return 0, stamped, fmt.Errorf("unable to restore live version schema: %w", err)
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
				existing, err := m.ExistingVersionSchemas(ctx)
				if err != nil {
					return 0, 0, err
				}
				if slices.Contains(existing, VersionedSchemaName(m.schema, live.VersionSchemaName())) {
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
	// still pinned to. Distinguish the two by physics:
	//   - any sealed row still queued → an interrupted contraction; resume it
	//     (the leaf's schema may legitimately be missing mid-resume when the
	//     queue's raw SQL forced a live-schema drop);
	//   - queue unsealed and the leaf has no physical version schema → an
	//     unfinished batch; refuse so the operator resumes or aborts it.
	resuming, err := m.state.HasSealedDeferred(ctx, m.schema)
	if err != nil {
		return 0, 0, err
	}
	if !resuming && !o.force {
		if m.disableVersionSchemas {
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

		existing, err := m.ExistingVersionSchemas(ctx)
		if err != nil {
			return 0, 0, err
		}
		if !slices.Contains(existing, VersionedSchemaName(m.schema, live.VersionSchemaName())) {
			return 0, 0, fmt.Errorf(
				"cannot contract: the latest migration %q has no version schema, so the deferred queue "+
					"belongs to an unfinished `pgroll migrate` batch; resume it by re-running `pgroll migrate`, "+
					"or abort it with `pgroll revert`", live.Name,
			)
		}
	}

	// Put back a live schema a part-way contraction left missing (resuming
	// one an older binary began, which dropped it before the drain), then
	// whatever a failure or cancel below leaves missing.
	var guard *liveGuard
	if !m.disableVersionSchemas {
		guard = &liveGuard{live: live}
		if _, err := m.restoreLiveViews(ctx, guard, false); err != nil {
			return 0, 0, fmt.Errorf("the live version schema %q is incomplete and could not be restored; "+
				"nothing was contracted: %w", guard.versionSchema(m), err)
		}
		defer func() {
			if err != nil {
				err = m.restoreAfterFailure(ctx, guard, err)
			}
		}()
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
		if err := m.drainDeferredMigration(ctx, dm, guard); err != nil {
			return 0, 0, err
		}
	}

	// The last migration's flag must outlive the projection rebuild: clear
	// it only after the live schema and boundary snapshot are restored, so
	// an interrupted contraction always re-runs with a non-empty queue.
	last := queued[len(queued)-1]
	if err := m.applyDeferredComplete(ctx, last, guard); err != nil {
		return 0, 0, err
	}

	if err := m.state.RefreshResultingSchema(ctx, m.schema, live.Name); err != nil {
		return 0, 0, fmt.Errorf("unable to refresh boundary snapshot for %q: %w", live.Name, err)
	}

	// Check the live views against what they now project: missing views are
	// created, and after raw SQL, views whose columns changed are rebuilt.
	// The last Complete has run, so it is left out of the replay.
	if guard != nil {
		guard.completed = last.Name
		if _, err := m.restoreLiveViews(ctx, guard, guard.rawRan); err != nil {
			return 0, 0, fmt.Errorf("unable to restore live version schema: %w", err)
		}
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

// contractedLeafGuard returns a guard for the history leaf when it was sealed
// (contracted) before this call, so its version schema is the live one; nil
// when there is no such leaf or version schemas are disabled.
func (m *Roll) contractedLeafGuard(ctx context.Context) (*liveGuard, error) {
	if m.disableVersionSchemas {
		return nil, nil
	}
	latest, err := m.state.LatestMigration(ctx, m.schema)
	if err != nil {
		return nil, fmt.Errorf("unable to determine latest migration: %w", err)
	}
	if latest == nil {
		return nil, nil
	}
	unsealed, err := m.state.UnsealedMigrations(ctx, m.schema)
	if err != nil {
		return nil, err
	}
	if slices.ContainsFunc(unsealed, func(r *state.MigrationRecord) bool { return r.Name == *latest }) {
		return nil, nil
	}
	leaf, err := m.state.GetMigration(ctx, m.schema, *latest)
	if err != nil {
		return nil, fmt.Errorf("unable to load latest migration: %w", err)
	}
	return &liveGuard{live: leaf}, nil
}
