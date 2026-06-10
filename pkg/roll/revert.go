// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"fmt"
	"slices"

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

// revertOptions holds bounds for a Revert invocation.
type revertOptions struct {
	steps int
	to    string
}

// RevertOption bounds how far a Revert walks.
type RevertOption func(*revertOptions)

// WithRevertSteps bounds the revert to at most n migrations, newest first.
func WithRevertSteps(n int) RevertOption {
	return func(o *revertOptions) { o.steps = n }
}

// WithRevertTo bounds the revert so that the named migration becomes the
// history leaf: everything newer is reverted, the named migration itself is
// kept. The target must be inside the revertible window or be the newest
// sealed migration (in which case the whole window reverts).
func WithRevertTo(name string) RevertOption {
	return func(o *revertOptions) { o.to = name }
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

// RevertPlan computes the bounded set of migrations a Revert with the same
// options would roll back, newest first. An empty plan means there is
// nothing to do (window closed, or already at the requested target).
func (m *Roll) RevertPlan(ctx context.Context, opts ...RevertOption) ([]RevertTarget, error) {
	var o revertOptions
	for _, opt := range opts {
		opt(&o)
	}
	if o.steps > 0 && o.to != "" {
		return nil, fmt.Errorf("revert bounds are mutually exclusive: specify steps or a target migration, not both")
	}
	if o.steps < 0 {
		return nil, fmt.Errorf("revert steps must be positive, got %d", o.steps)
	}

	targets, err := m.RevertTargets(ctx)
	if err != nil {
		return nil, err
	}

	if o.steps > 0 {
		if o.steps < len(targets) {
			targets = targets[:o.steps]
		}
		return targets, nil
	}

	if o.to != "" {
		// Target inside the window: keep it, revert everything newer.
		for i, t := range targets {
			if t.Name == o.to {
				return targets[:i], nil
			}
		}
		// Target is the newest sealed migration (the window's lower
		// boundary): the whole window reverts onto it.
		if len(targets) > 0 {
			oldest := targets[len(targets)-1]
			if oldest.Parent != nil && *oldest.Parent == o.to {
				return targets, nil
			}
		}
		// Already there: the target is the current leaf (sealed, so the
		// window loop above didn't see it).
		latest, err := m.state.LatestMigration(ctx, m.schema)
		if err != nil {
			return nil, fmt.Errorf("unable to determine latest migration: %w", err)
		}
		if latest != nil && *latest == o.to {
			return nil, nil
		}
		exists, err := m.state.MigrationExists(ctx, m.schema, o.to)
		if err != nil {
			return nil, err
		}
		if exists {
			boundary := "(none)"
			if len(targets) > 0 {
				if p := targets[len(targets)-1].Parent; p != nil {
					boundary = *p
				}
			} else if latest != nil {
				boundary = *latest
			}
			return nil, fmt.Errorf(
				"migration %q is sealed: its contraction has run and revert cannot restore the prior state; "+
					"the deepest reachable target is %q", o.to, boundary,
			)
		}
		return nil, fmt.Errorf("migration %q not found in history", o.to)
	}

	return targets, nil
}

// Revert rolls back every migration in the revertible window — or the
// bounded subset selected by RevertOptions — newest first, returning the
// database (schema, data, and migration history) to the state before those
// migrations. Lossless: deferred rows never contracted (old columns and
// dual-write triggers still live), and applied (inline-completed) rows only
// renamed pgroll-internal artifacts.
//
// Apps pinned to the deployment's version schema must be repinned to the
// previous deployment's schema (still alive under delayed contraction)
// before reverting — the walk drops the newer version schema first.
//
// After a bounded revert the new leaf may be a train intermediate that
// never projected a version schema; one is materialized for it so apps and
// search-path resolution have a live projection.
//
// Idempotent and resumable: each step is independently durable and the walk
// always operates on the current history leaf, so an interrupted revert can
// simply be re-run.
func (m *Roll) Revert(ctx context.Context, opts ...RevertOption) ([]RevertTarget, error) {
	targets, err := m.RevertPlan(ctx, opts...)
	if err != nil {
		return nil, err
	}

	for _, t := range targets {
		if err := m.revertMigration(ctx, t); err != nil {
			return nil, fmt.Errorf("unable to revert migration %q: %w", t.Name, err)
		}
	}

	if len(targets) > 0 {
		if err := m.ensureLeafVersionSchema(ctx); err != nil {
			return nil, fmt.Errorf("unable to materialize version schema for the new leaf: %w", err)
		}
	}

	return targets, nil
}

// ensureLeafVersionSchema materializes the current history leaf's version
// schema if it does not exist. Train intermediates are applied without one;
// when a bounded revert makes such a row the leaf, apps need a projection
// to repin to. The projection is built from the deferred-replayed schema so
// still-queued expand-state artifacts project under their virtual names,
// exactly as the leaf's own Start would have projected them.
func (m *Roll) ensureLeafVersionSchema(ctx context.Context) error {
	if m.disableVersionSchemas {
		return nil
	}
	latest, err := m.state.LatestMigration(ctx, m.schema)
	if err != nil {
		return fmt.Errorf("unable to determine latest migration: %w", err)
	}
	if latest == nil {
		return nil
	}
	leaf, err := m.state.GetMigration(ctx, m.schema, *latest)
	if err != nil {
		return err
	}
	want := VersionedSchemaName(m.schema, leaf.VersionSchemaName())
	existing, err := m.ExistingVersionSchemas(ctx)
	if err != nil {
		return err
	}
	if slices.Contains(existing, want) {
		return nil
	}

	sch, err := m.readSchemaWithDeferred(ctx)
	if err != nil {
		return fmt.Errorf("unable to read schema: %w", err)
	}
	m.logger.Info("materializing version schema for new history leaf", "schema", want)
	return m.ensureViews(ctx, sch, leaf)
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
