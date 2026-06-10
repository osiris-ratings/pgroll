// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"fmt"

	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/schema"
)

// RevertTargetState classifies how a migration in the revertible window will
// be rolled back.
type RevertTargetState string

const (
	// RevertStateInProgress is an active (done=false) migration; reverted
	// with the standard in-flight rollback recipe.
	RevertStateInProgress RevertTargetState = "in-progress"
	// RevertStateDeferred is a done migration whose Complete operations are
	// still queued — physically in its expand phase, reverted losslessly
	// with the standard rollback recipe.
	RevertStateDeferred RevertTargetState = "deferred"
	// RevertStateApplied is a done migration whose inline Complete already
	// ran. Inline Completes only touch pgroll-internal artifacts (rename
	// temp names onto final names, drop trigger machinery), so its
	// operations roll back against the physical post-complete schema.
	RevertStateApplied RevertTargetState = "applied"
)

// RevertTarget describes one migration in the revertible window.
type RevertTarget struct {
	Name           string
	State          RevertTargetState
	OperationCount int
	VersionSchema  string
	// Parent is the name of the migration this row was applied on top of;
	// nil for the first migration in history. The oldest target's Parent is
	// the migration the database returns to after the revert.
	Parent *string

	migration *migrations.Migration
}

// RevertTargets computes the revertible window: every unsealed migration,
// newest first. The window is the most recent deployment — sealing (the
// next train departing, or a manual `pgroll complete`) is the point of no
// return that closes it.
//
// Returns an error if the window contains rows that cannot be reverted:
// inferred DDL captures, or completed destructive migrations left unsealed
// by an interrupted seal (resume the seal instead of reverting).
func (m *Roll) RevertTargets(ctx context.Context) ([]RevertTarget, error) {
	records, err := m.state.UnsealedMigrations(ctx, m.schema)
	if err != nil {
		return nil, err
	}

	targets := make([]RevertTarget, 0, len(records))
	for _, r := range records {
		// Defensive: baselines and inferred rows are sealed at insert, but
		// guard against manually manipulated state.
		if r.MigrationType == MigrationTypeBaseline {
			break
		}
		if r.MigrationType == MigrationTypeInferred {
			return nil, fmt.Errorf(
				"migration %q was captured from DDL run outside pgroll and cannot be reverted; "+
					"run `pgroll complete` to seal it", r.Name,
			)
		}

		var state RevertTargetState
		switch {
		case !r.Done:
			state = RevertStateInProgress
		case r.CompleteDeferred:
			state = RevertStateDeferred
		default:
			if r.Migration.CompleteMustBeDeferred() {
				return nil, fmt.Errorf(
					"migration %q has completed destructive operations but is not sealed — "+
						"a previous seal was interrupted; re-run `pgroll migrate` or `pgroll complete` "+
						"to finish draining before reverting", r.Name,
				)
			}
			state = RevertStateApplied
		}

		targets = append(targets, RevertTarget{
			Name:           r.Name,
			State:          state,
			OperationCount: len(r.Migration.Operations),
			VersionSchema:  r.Migration.VersionSchemaName(),
			Parent:         r.Parent,
			migration:      r.Migration,
		})
	}
	return targets, nil
}

// Revert rolls back every migration in the revertible window, newest first,
// returning the database — schema, data, and migration history — to the
// state before the most recent deployment. Lossless: deferred rows never
// contracted (old columns and dual-write triggers still live), and applied
// (inline-completed) rows only renamed pgroll-internal artifacts.
//
// Apps pinned to the deployment's version schema must be repinned to the
// previous deployment's schema (still alive under delayed contraction)
// before reverting — the walk drops the newer version schema first.
//
// Idempotent and resumable: each step is independently durable and the walk
// always operates on the current history leaf, so an interrupted revert can
// simply be re-run.
func (m *Roll) Revert(ctx context.Context) ([]RevertTarget, error) {
	targets, err := m.RevertTargets(ctx)
	if err != nil {
		return nil, err
	}

	for _, t := range targets {
		if err := m.revertMigration(ctx, t); err != nil {
			return nil, fmt.Errorf("unable to revert migration %q: %w", t.Name, err)
		}
	}

	return targets, nil
}

// revertMigration rolls back a single migration (the current history leaf)
// and removes it from history.
func (m *Roll) revertMigration(ctx context.Context, t RevertTarget) error {
	migration := t.migration
	m.logger.LogMigrationRollback(migration)

	switch t.State {
	case RevertStateApplied:
		// Inline Complete already renamed internal artifacts onto their
		// final names; roll back against the physical schema so operation
		// Rollbacks resolve post-complete names (e.g. OpAddColumn drops the
		// final column rather than its long-gone temporary name).
		// Operations whose Complete restructures user-facing objects
		// implement RollbackCompleted and take that path instead.
		if err := m.dropVersionSchema(ctx, migration); err != nil {
			return err
		}
		sch, err := m.state.ReadSchema(ctx, m.schema)
		if err != nil {
			return fmt.Errorf("unable to read schema: %w", err)
		}
		sch.MigrationScope = migrations.MigrationScopeFor(migration.Name)
		if err := m.rollbackCompletedOperations(ctx, migration, sch); err != nil {
			return err
		}
	default:
		// In-progress or deferred: still in the expand phase — standard
		// rollback recipe (parent snapshot + virtual replay of Start).
		if err := m.rollbackExpandPhase(ctx, migration, t.Parent); err != nil {
			return err
		}
	}

	if err := m.state.DeleteMigration(ctx, m.schema, migration.Name); err != nil {
		return fmt.Errorf("unable to remove migration from history: %w", err)
	}

	m.logger.LogMigrationRollbackComplete(migration)
	return nil
}

// rollbackCompletedOperations runs each operation's post-complete rollback
// in reverse order against the given (physical) schema, preferring
// RollbackCompleted where the operation implements it.
func (m *Roll) rollbackCompletedOperations(ctx context.Context, migration *migrations.Migration, sch *schema.Schema) error {
	for i := len(migration.Operations) - 1; i >= 0; i-- {
		op := migration.Operations[i]

		var actions []migrations.DBAction
		var err error
		if cr, ok := op.(migrations.CompletedRollbackable); ok {
			actions, err = cr.RollbackCompleted(m.logger, m.pgConn, sch)
		} else {
			actions, err = op.Rollback(m.logger, m.pgConn, sch)
		}
		if err != nil {
			return fmt.Errorf("unable to collect actions for rollback operation: %w", err)
		}

		coordinator := migrations.NewCoordinator(actions)
		if err := coordinator.Execute(ctx); err != nil {
			return fmt.Errorf("unable to execute rollback operation: %w", err)
		}
	}
	return nil
}
