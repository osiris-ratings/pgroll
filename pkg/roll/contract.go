// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"fmt"
	"slices"

	"github.com/lib/pq"
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
// The leaf's version schema — the projection apps are pinned to — is dropped
// before the drain only when the queue contains onComplete raw SQL, whose
// arbitrary statements may drop or rename objects the views project (typed
// contractions never invalidate the live views: renames auto-follow, dropped
// columns were never projected, dropped tables are filtered out). It is
// recreated from the post-drain physical state immediately after.
func (m *Roll) FinishContraction(ctx context.Context) (int, int64, error) {
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
	if !resuming && !m.disableVersionSchemas {
		existing, err := m.ExistingVersionSchemas(ctx)
		if err != nil {
			return 0, 0, err
		}
		if !slices.Contains(existing, VersionedSchemaName(m.schema, live.VersionSchemaName())) {
			return 0, 0, fmt.Errorf(
				"cannot contract: the latest migration %q has no version schema, so the deferred queue "+
					"belongs to an unfinished `pgroll migrate` batch; resume it by re-running `pgroll migrate`, "+
					"or abort it with `pgroll revert`", live.Name)
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

	// The one Complete pgroll cannot reason about is onComplete raw SQL,
	// whose arbitrary statements may drop or rename anything the live views
	// project — that forces the conservative whole-schema drop below.
	needsLiveDrop := false
	for _, dm := range queued {
		if dm.CompleteRequiresLiveSchemaDrop() {
			needsLiveDrop = true
			break
		}
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

		versionSchema := VersionedSchemaName(m.schema, live.VersionSchemaName())
		if needsLiveDrop {
			// Reached only for an onComplete raw SQL drain: it cannot be
			// skipped (the raw SQL could fail on a view's deptype=n
			// dependency), so a blocked drop must fail the contraction
			// rather than corrupt it.
			if _, err := m.pgConn.ExecContext(ctx,
				fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", pq.QuoteIdentifier(versionSchema))); err != nil {
				if isLockNotAvailable(err) {
					return 0, 0, fmt.Errorf("unable to drop live version schema %q before contraction "+
						"(apps still reading it must move off it first); blocked by:%s: %w",
						versionSchema, m.schemaLockHolders(ctx, versionSchema), err)
				}
				return 0, 0, fmt.Errorf("unable to drop live version schema before drain: %w", err)
			}
		} else {
			m.logger.Info("keeping the live version schema in place; the drain's typed contractions "+
				"do not invalidate its views", "schema", versionSchema)
		}
	}

	for _, dm := range queued[:len(queued)-1] {
		if err := m.drainDeferredMigration(ctx, dm); err != nil {
			return 0, 0, err
		}
	}

	// The last migration's flag must outlive the projection rebuild: clear
	// it only after the live schema and boundary snapshot are restored, so
	// an interrupted contraction always re-runs with a non-empty queue.
	last := queued[len(queued)-1]
	if err := m.applyDeferredComplete(ctx, last); err != nil {
		return 0, 0, err
	}

	if err := m.state.RefreshResultingSchema(ctx, m.schema, live.Name); err != nil {
		return 0, 0, fmt.Errorf("unable to refresh boundary snapshot for %q: %w", live.Name, err)
	}

	if !m.disableVersionSchemas && needsLiveDrop {
		// Only rebuild what was dropped. A projection-preserving drain left
		// the live schema in place, so its views are still valid and
		// re-running ensureViews would needlessly reacquire the
		// AccessExclusive locks the drop was skipped to avoid.
		currentSchema, err := m.state.ReadSchema(ctx, m.schema)
		if err != nil {
			return 0, 0, fmt.Errorf("unable to read schema after drain: %w", err)
		}
		if err := m.ensureViews(ctx, currentSchema, live); err != nil {
			return 0, 0, fmt.Errorf("unable to recreate live version schema: %w", err)
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
