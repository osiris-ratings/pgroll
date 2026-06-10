// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/schema"
)

// rollbackCleanupTimeout caps best-effort cleanup when the parent context has
// already been cancelled (e.g. by SIGINT). A bounded fresh context lets us
// still attempt to drop the new version schema and clear the migration row
// before exiting, instead of leaving the run dirty for `pgroll rollback`.
const rollbackCleanupTimeout = 30 * time.Second

// rollbackContext returns a context appropriate for running cleanup after a
// failure. If the parent context is still alive (e.g. lock_timeout retry
// budget exhausted), it is returned as-is so the caller's deadline applies.
// If the parent has been cancelled (e.g. SIGINT), a fresh background context
// with rollbackCleanupTimeout is returned so the rollback can still complete.
func (m *Roll) rollbackContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent.Err() == nil {
		return parent, func() {}
	}
	m.logger.Info("parent context cancelled; running cleanup with fresh context",
		"timeout", rollbackCleanupTimeout.String())
	return context.WithTimeout(context.Background(), rollbackCleanupTimeout)
}

func (m *Roll) Validate(ctx context.Context, migration *migrations.Migration) error {
	if m.skipValidation {
		return nil
	}
	if m.requireReversible {
		if err := migration.ValidateReversibility(); err != nil {
			return fmt.Errorf("migration '%s' is invalid: %w", migration.Name, err)
		}
	}
	lastSchema, err := m.readSchemaWithDeferred(ctx)
	if err != nil {
		return err
	}
	// Set the migration's scope on the schema so any naming helpers
	// invoked during Validate (e.g. via sub-op Validate paths that call
	// table.AddColumn(temp_name, ...)) produce the right identifiers.
	lastSchema.MigrationScope = migrations.MigrationScopeFor(migration.Name)
	err = migration.Validate(ctx, lastSchema)
	if err != nil {
		return fmt.Errorf("migration '%s' is invalid: %w", migration.Name, err)
	}

	// Validate database-level preconditions (function_exists, type_exists)
	if err := migrations.ValidateDBPreconditions(ctx, migration.Preconditions, m.pgConn, m.schema); err != nil {
		return fmt.Errorf("migration '%s' is invalid: %w", migration.Name, err)
	}

	if err := validateVersionSchemaName(m.schema, migration.VersionSchemaName()); err != nil {
		return err
	}
	return nil
}

// readSchemaWithDeferred reads the physical schema, then replays the
// virtual-schema effects of every deferred-pending intermediate so callers
// see the schema state as it will exist after those completes drain.
//
// Two side effects matter:
//
//  1. Replay calls each deferred migration's op.Start against a FakeDB,
//     mutating the in-memory schema only. For example, a deferred
//     OpDropColumn flips Deleted=true on the column, and a deferred
//     OpAddColumn registers the new (still virtual) column under its real
//     name with the temp physical name.
//  2. The duplicate-cleanup loop removes physical-name-keyed entries that
//     read_schema returned for columns whose virtual name now differs from
//     the physical name. Without this, OpAddColumn-style temp columns
//     (`_pgroll_new_<col>`) would appear twice in the schema — once under
//     their physical name (from read_schema) and once under their real
//     name (from replay) — confusing both subsequent Validates and view
//     projection. Drain (which expects pre-rename temp names) reads the
//     raw schema directly via state.ReadSchema instead.
func (m *Roll) readSchemaWithDeferred(ctx context.Context) (*schema.Schema, error) {
	return m.readSchemaWithDeferredExcluding(ctx, "")
}

// readSchemaWithDeferredExcluding is readSchemaWithDeferred with one
// migration's Start excluded from the replay. Used by drainDeferredMigration
// when constructing that migration's own Complete actions: the construction
// needs to see the schema as it stands *before* this migration's Start ran,
// so duplicator-pattern Completes resolve column.Name to the source column
// (not the temp this migration installed). All *other* still-deferred
// migrations are replayed normally.
func (m *Roll) readSchemaWithDeferredExcluding(ctx context.Context, excludeMigration string) (*schema.Schema, error) {
	s, err := m.state.ReadSchema(ctx, m.schema)
	if err != nil {
		return nil, err
	}

	deferred, err := m.state.DeferredCompletes(ctx, m.schema)
	if err != nil {
		return nil, fmt.Errorf("unable to query deferred completes: %w", err)
	}
	if excludeMigration != "" {
		filtered := deferred[:0]
		for _, dm := range deferred {
			if dm.Name != excludeMigration {
				filtered = append(filtered, dm)
			}
		}
		deferred = filtered
	}
	if len(deferred) == 0 {
		return s, nil
	}

	// Reverse OpDropTable's soft-delete physical rename so its replay can
	// find the table to mark Deleted. OpDropTable.Start renames the table
	// to `_pgroll_del_<name>_<scope>` physically; read_schema returns it
	// under that physical key. Build a lookup of (scope -> set of orig
	// names that this migration soft-deleted), then for each migration's
	// soft-deletes, fold the physical entry back to the un-prefixed key
	// so the replay can find it.
	for _, dm := range deferred {
		dmScope := migrations.MigrationScopeFor(dm.Name)
		suffix := "_pgroll_del_"
		for k, t := range s.Tables {
			if t == nil {
				continue
			}
			// Look for `_pgroll_del_<orig>_<dmScope>` matching this
			// migration's scope; that's a table this dm soft-deleted.
			if !strings.HasPrefix(t.Name, suffix) {
				continue
			}
			rest := strings.TrimPrefix(t.Name, suffix)
			scopeTail := "_" + dmScope
			if !strings.HasSuffix(rest, scopeTail) {
				continue
			}
			orig := strings.TrimSuffix(rest, scopeTail)
			if _, exists := s.Tables[orig]; exists {
				continue
			}
			s.Tables[orig] = t
			delete(s.Tables, k)
		}
	}

	// Replay each deferred migration's Start with its own scope set on
	// the schema, so naming helpers inside op.Start produce identifiers
	// matching what's physically present in the database.
	for _, dm := range deferred {
		s.MigrationScope = migrations.MigrationScopeFor(dm.Name)
		if err := dm.UpdateVirtualSchema(ctx, s); err != nil {
			return nil, fmt.Errorf("unable to apply deferred virtual schema for %q: %w", dm.Name, err)
		}
	}
	// Reset to empty so subsequent helper calls don't accidentally use
	// a prior migration's scope. The caller (Roll.Start, drain, etc.) is
	// responsible for setting scope to the migration currently being
	// processed before invoking ops on the returned schema.
	s.MigrationScope = ""

	// Don't clean up physical-name-keyed Column entries even when a
	// virtual-keyed entry now references the same physical column. The
	// view-projection filter (skip `_pgroll_*` virtual keys in
	// ensureView) keeps internal columns out of user-facing views, and
	// OpDropConstraint / OpDropMultiColumnConstraint need to look the
	// column up by the physical name stored in the constraint's
	// Columns slice (which pg records based on the SQL the constraint
	// was created with — temp-named when an earlier deferred migration
	// added the constraint via duplicator).

	return s, nil
}

// validateVersionSchemaName rejects migrations whose computed version schema
// name (schema + "_" + versionName) would exceed Postgres' identifier length
// limit. Without this check, Postgres silently truncates the name on
// CREATE SCHEMA, leaving pgroll's metadata pointing at a name that no longer
// matches what's in information_schema.schemata.
func validateVersionSchemaName(schema, versionName string) error {
	computed := VersionedSchemaName(schema, versionName)
	if len(computed) > migrations.MaxIdentifierLength {
		return migrations.VersionSchemaNameTooLongError{
			Schema:       schema,
			VersionName:  versionName,
			ComputedName: computed,
			Max:          migrations.MaxIdentifierLength,
		}
	}
	return nil
}

// Start will apply the required changes to enable supporting the new schema version
func (m *Roll) Start(ctx context.Context, migration *migrations.Migration, cfg *backfill.Config, opts ...StartOption) error {
	var o startOptions
	for _, opt := range opts {
		opt(&o)
	}

	// Fail early if we have existing schema without migration history
	hasExistingSchema, err := m.state.HasExistingSchemaWithoutHistory(ctx, m.schema)
	if err != nil {
		return fmt.Errorf("failed to check for existing schema: %w", err)
	}
	if hasExistingSchema {
		return ErrExistingSchemaWithoutHistory
	}

	m.logger.LogMigrationStart(migration)

	if err := m.Validate(ctx, migration); err != nil {
		return err
	}

	if err := m.validateDependencies(ctx, migration); err != nil {
		return err
	}

	job, err := m.startDDLOperations(ctx, migration, &o)
	if err != nil {
		return err
	}

	// perform backfills for the tables that require it
	return m.performBackfills(ctx, job, cfg)
}

func (m *Roll) startDDLOperations(ctx context.Context, migration *migrations.Migration, o *startOptions) (*backfill.Job, error) {
	// check if there is an active migration, create one otherwise
	active, err := m.state.IsActiveMigrationPeriod(ctx, m.schema)
	if err != nil {
		return nil, err
	}
	if active {
		return nil, fmt.Errorf("a migration for schema %q is already in progress", m.schema)
	}

	// create a new active migration (guaranteed to be unique by constraints)
	if err = m.state.Start(ctx, m.schema, migration); err != nil {
		return nil, fmt.Errorf("unable to start migration: %w", err)
	}

	// run any BeforeStartDDL hooks
	if m.migrationHooks.BeforeStartDDL != nil {
		if err := m.migrationHooks.BeforeStartDDL(m); err != nil {
			return nil, fmt.Errorf("failed to execute BeforeStartDDL hook: %w", err)
		}
	}

	// defer execution of any AfterStartDDL hooks
	if m.migrationHooks.AfterStartDDL != nil {
		defer m.migrationHooks.AfterStartDDL(m)
	}

	// Construct the full name of the version schema that will be created by this
	// migration. The version schema is created after operations have completed
	// but ops need to know the name in advance in order to construct backfill
	// triggers.
	versionSchemaName := VersionedSchemaName(m.schema, migration.VersionSchemaName())

	// Reread the latest schema as validation may have updated the schema object
	// in memory. Use the deferred-aware helper so this migration's operations
	// see the schema state implied by any pending intermediates' Starts —
	// otherwise an OpAddColumn intermediate whose Complete is deferred would
	// leave its column under the temp physical name (`_pgroll_new_<col>`),
	// invisible to subsequent ops looking it up by real name.
	newSchema, err := m.readSchemaWithDeferred(ctx)
	if err != nil {
		return nil, fmt.Errorf("unable to read schema: %w", err)
	}
	// Set the per-migration scope on the schema so naming helpers
	// (TemporaryName, TriggerFunctionName, NeedsBackfillColumnName, etc.)
	// produce identifiers that don't collide with concurrently-deferred
	// migrations.
	scope := migrations.MigrationScopeFor(migration.Name)
	newSchema.MigrationScope = scope

	// execute operations
	job := backfill.NewJob(m.schema, versionSchemaName, scope)
	for _, op := range migration.Operations {
		startOp, err := op.Start(ctx, m.logger, m.pgConn, newSchema)
		if err != nil {
			return nil, fmt.Errorf("unable to collect actions for start %q migration: %w", migration.Name, err)
		}
		if startOp == nil {
			continue
		}

		coordinator := migrations.NewCoordinator(startOp.Actions)
		if err := coordinator.Execute(ctx); err != nil {
			m.logger.LogRollbackOnFailure(fmt.Sprintf("start operation of %q failed: %v", migration.Name, err))
			cleanupCtx, cancel := m.rollbackContext(ctx)
			errRollback := m.Rollback(cleanupCtx)
			cancel()
			if errRollback != nil {
				return nil, errors.Join(
					fmt.Errorf("unable to execute start operation of %q: %w", migration.Name, err),
					fmt.Errorf("unable to roll back failed operation: %w", errRollback),
				)
			}
			return nil, fmt.Errorf("failed to start %q migration, changes rolled back: %w", migration.Name, err)
		}
		// refresh schema when the op is isolated and requires a refresh (for example raw sql)
		// we don't want to refresh the schema if the operation is not isolated as it would
		// override changes made by other operations
		if _, ok := op.(migrations.RequiresSchemaRefreshOperation); ok {
			if isolatedOp, ok := op.(migrations.IsolatedOperation); ok && isolatedOp.IsIsolated() {
				newSchema, err = m.readSchemaWithDeferred(ctx)
				if err != nil {
					return nil, fmt.Errorf("unable to refresh schema: %w", err)
				}
			}
		}
		if startOp.BackfillTask != nil {
			job.AddTask(startOp.BackfillTask)
		}
	}

	// Create views for the new version unless either:
	//   - the Roll instance has version schemas globally disabled, or
	//   - this Start call passed WithoutVersionSchema (used by `pgroll
	//     migrate` for intermediate migrations to avoid projecting schemas
	//     that no apps will connect to and that would otherwise create view
	//     dependencies blocking destructive ops later in the batch).
	if !o.skipVersionSchema && !m.disableVersionSchemas {
		// newSchema already reflects deferred-pending mutations because every
		// schema read in this method goes through readSchemaWithDeferred — so
		// the projected views skip columns scheduled for deferred drops or
		// renames, which is what unblocks the drained DDL at final Complete.
		if err := m.ensureViews(ctx, newSchema, migration); err != nil {
			return nil, err
		}
	}

	return job, nil
}

func (m *Roll) ensureViews(ctx context.Context, schema *schema.Schema, mig *migrations.Migration) error {
	if err := validateVersionSchemaName(m.schema, mig.VersionSchemaName()); err != nil {
		return err
	}
	versionSchema := VersionedSchemaName(m.schema, mig.VersionSchemaName())
	_, err := m.pgConn.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", pq.QuoteIdentifier(versionSchema)))
	if err != nil {
		return err
	}

	// create views in the new schema
	for name, table := range schema.Tables {
		if table.Deleted {
			continue
		}
		// Defensive: skip tables whose physical name is a pgroll soft-delete
		// artifact. These are produced by OpDropTable.Start and persist
		// physically across deferred Completes; without this filter they'd
		// be projected as views and block their own drop at drain time.
		if strings.HasPrefix(table.Name, "_pgroll_del_") {
			continue
		}
		err = m.ensureView(ctx, mig.VersionSchemaName(), name, table)
		if err != nil {
			return fmt.Errorf("unable to create view: %w", err)
		}
	}

	m.logger.LogSchemaCreation(mig.VersionSchemaName(), versionSchema)

	return nil
}

// completeOptions holds options for the Complete method.
type completeOptions struct {
	skipSchemaDrop bool
	deferComplete  bool
}

// CompleteOption is a functional option for the Complete method.
type CompleteOption func(*completeOptions)

// WithSkipSchemaDrop returns a CompleteOption that skips dropping old version
// schemas during Complete. Retained for callers that want to run intermediate
// operations inline while preserving the previous-production version schema —
// fine for purely-additive batches but blocks destructive DDL via dependency
// errors. New `pgroll migrate` callers should prefer WithDeferComplete, which
// also defers the operations themselves.
func WithSkipSchemaDrop() CompleteOption {
	return func(o *completeOptions) { o.skipSchemaDrop = true }
}

// WithDeferComplete returns a CompleteOption that records the migration as
// logically done but queues its Complete operations for replay during the
// next non-deferred Complete. Used by `pgroll migrate` for intermediate
// migrations in a multi-migration batch.
//
// The replay window opens at the next non-deferred Complete *after* old
// version schemas are dropped, so destructive DDL (DROP COLUMN, RENAME
// COLUMN, drop-table, etc.) is no longer blocked by views in the
// previous-production version schema. This is what lets a weekly batched
// release contain mid-chain destructive migrations without getting stuck.
func WithDeferComplete() CompleteOption {
	return func(o *completeOptions) { o.deferComplete = true }
}

// Complete will update the database schema to match the current version
func (m *Roll) Complete(ctx context.Context, opts ...CompleteOption) error {
	var o completeOptions
	for _, opt := range opts {
		opt(&o)
	}
	// get current ongoing migration
	migration, err := m.state.GetActiveMigration(ctx, m.schema)
	if err != nil {
		return fmt.Errorf("unable to get active migration: %w", err)
	}

	m.logger.LogMigrationComplete(migration)

	// Deferred path: record the migration as logically done with its
	// Complete operations queued for replay during the next non-deferred
	// Complete. Skip everything else — no schema cleanup, no operation
	// execution. This is what `pgroll migrate` intermediates use so that
	// destructive DDL (DROP COLUMN, RENAME COLUMN, drop-table) doesn't run
	// while the previous-production version schema's views still reference
	// the affected objects. Replay happens at final Complete after those
	// schemas have been dropped.
	if o.deferComplete {
		if err := m.state.MarkCompleteDeferred(ctx, m.schema, migration.Name); err != nil {
			return fmt.Errorf("unable to mark migration as complete-deferred: %w", err)
		}
		m.logger.Info("complete deferred — operations will replay at next non-deferred Complete", "migration", migration.Name)
		return nil
	}

	// Drop every other version schema, keeping only the version being
	// completed. This must happen before the operations execute so views in
	// old schemas don't block DDL like DROP COLUMN. This is the single
	// cleanup point in pgroll's lifecycle: `pgroll migrate` accumulates
	// schemas without dropping anything; the final `Complete()` (whether
	// triggered by `--complete` on migrate or by a subsequent `pgroll
	// complete`) reaps every prior version. This avoids the heuristic-based
	// "originalVersion" detection that previously could drop a schema apps
	// were still connected to when pgroll's state had drifted from
	// production deployment.
	if !o.skipSchemaDrop && !m.disableVersionSchemas {
		if err := m.DropVersionSchemasExcept(ctx, migration.VersionSchemaName()); err != nil {
			return fmt.Errorf("unable to drop old version schemas: %w", err)
		}
	} else if o.skipSchemaDrop {
		m.logger.Info("skipping old version schema cleanup (deferred to next Complete)", "migration", migration.Name)
	}

	// Drain deferred Completes one migration at a time, in chain order.
	// Each iteration reads the schema fresh, constructs that migration's
	// Complete actions, executes them through their own Coordinator, and
	// clears the migration's complete_deferred flag. By the time
	// migration N's actions are constructed, migrations 1..N-1 have
	// physically applied — so N's op.Complete sees the post-prior-drain
	// state when looking up columns/constraints by name.
	//
	// This restores the per-migration Complete contract pgroll is built
	// around. The merged-Coordinator approach we used previously was
	// load-bearing only when the `_pgroll_needs_backfill` marker was
	// shared across migrations on the same table; with per-migration
	// namespacing each migration's marker is independent, so per-
	// migration drains compose without ordering dependencies.
	//
	// Failure semantics: if migration K's drain fails, the partial state
	// is exactly "1..K-1 fully drained, K still deferred, K+1..end still
	// deferred". The operator fixes the underlying issue and re-runs
	// `pgroll complete`; the retry resumes from K. Cleaner than the
	// merged version's "all-or-nothing" partial-execution.
	//
	// Skip the entire drain under WithSkipSchemaDrop — that path keeps
	// the prev-production schema around, which would block destructive
	// drained DDL with the same dependency error the deferral was set up
	// to avoid. Leave the queue for the next non-deferred Complete.
	if !o.skipSchemaDrop {
		queued, err := m.state.DeferredCompletes(ctx, m.schema)
		if err != nil {
			return fmt.Errorf("unable to query deferred completes: %w", err)
		}
		if len(queued) > 0 {
			m.logger.Info("draining deferred completes", "count", len(queued))

			// Drop the active migration's version schema (and its views)
			// before drain runs. Drain DDL frequently drops/renames
			// user-facing columns the active version schema's views
			// project; pg_depend records those view→column edges as
			// deptype=n (normal), which DROP COLUMN refuses to cascade
			// without an explicit CASCADE. Dropping the version schema
			// up front lets each drain action proceed against the bare
			// table; we recreate the version schema at the end of
			// Complete from the post-drain physical state.
			//
			// Apps connected to the active version schema would observe
			// a brief outage, but at this point in the lifecycle the
			// previous-production schema (V0) has already been reaped,
			// so apps are in a transition window already. Recreating
			// from post-drain state immediately restores the projection.
			if !m.disableVersionSchemas {
				versionSchema := VersionedSchemaName(m.schema, migration.VersionSchemaName())
				_, err := m.pgConn.ExecContext(ctx,
					fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", pq.QuoteIdentifier(versionSchema)))
				if err != nil {
					return fmt.Errorf("unable to drop active version schema before drain: %w", err)
				}
			}

			for _, dm := range queued {
				if err := m.drainDeferredMigration(ctx, dm); err != nil {
					return err
				}
			}
		}
	}

	// read the current schema (now reflecting every drained migration's
	// physical effects).
	currentSchema, err := m.state.ReadSchema(ctx, m.schema)
	if err != nil {
		return fmt.Errorf("unable to read schema: %w", err)
	}
	// Set the active migration's scope so its op.Complete naming helpers
	// match the identifiers installed by its earlier Start.
	currentSchema.MigrationScope = migrations.MigrationScopeFor(migration.Name)

	// run any BeforeCompleteDDL hooks
	if m.migrationHooks.BeforeCompleteDDL != nil {
		if err := m.migrationHooks.BeforeCompleteDDL(m); err != nil {
			return fmt.Errorf("failed to execute BeforeCompleteDDL hook: %w", err)
		}
	}

	// defer execution of any AfterCompleteDDL hooks
	if m.migrationHooks.AfterCompleteDDL != nil {
		defer m.migrationHooks.AfterCompleteDDL(m)
	}

	// execute the active migration's Complete operations
	refreshViews := false
	var actions []migrations.DBAction
	for _, op := range migration.Operations {
		opActions, err := op.Complete(m.logger, m.pgConn, currentSchema)
		if err != nil {
			return fmt.Errorf("unable to collect actions for complete operation: %w", err)
		}
		actions = append(actions, opActions...)

		if _, ok := op.(migrations.RequiresSchemaRefreshOperation); ok {
			refreshViews = true
		}
	}

	coordinator := migrations.NewCoordinator(actions)
	if err := coordinator.Execute(ctx); err != nil {
		return fmt.Errorf("unable to execute complete operation: %w", err)
	}

	// Recreate the active migration's version schema. Always when
	// !skipSchemaDrop, because the drain may have dropped it (above) to
	// unblock destructive DDL, OR the active migration's ops may have
	// included raw SQL that requires a refresh. The ensureViews call
	// rebuilds the version schema and its views over the post-drain
	// physical state, restoring the projection apps deploy against.
	if !o.skipSchemaDrop && !m.disableVersionSchemas {
		currentSchema, err = m.state.ReadSchema(ctx, m.schema)
		if err != nil {
			return fmt.Errorf("unable to read schema: %w", err)
		}

		err = m.ensureViews(ctx, currentSchema, migration)
		if err != nil {
			return err
		}
		// Suppress unused-var warning if refreshViews ends up unused in
		// some build paths.
		_ = refreshViews
	}

	// mark as completed
	err = m.state.Complete(ctx, m.schema, migration.Name)
	if err != nil {
		return fmt.Errorf("unable to complete migration: %w", err)
	}

	// A non-deferred Complete is a seal point: this migration's own
	// contraction ran, and the drain above flushed any queued deferred
	// completes. Everything done is now past the point of no return for
	// `pgroll revert`. Inline intermediates (WithSkipSchemaDrop) skip this —
	// their Completes touch only pgroll-internal artifacts and they remain
	// revertible until their train seals.
	if !o.skipSchemaDrop {
		if err := m.state.MarkSealed(ctx, m.schema); err != nil {
			return err
		}
	}

	m.logger.LogMigrationComplete(migration)

	return nil
}

// Rollback will revert the changes made by the migration
func (m *Roll) Rollback(ctx context.Context) error {
	// get current ongoing migration
	migration, err := m.state.GetActiveMigration(ctx, m.schema)
	if err != nil {
		return fmt.Errorf("unable to get active migration: %w", err)
	}

	m.logger.LogMigrationRollback(migration)

	// get the name of the previous migration
	previousMigration, err := m.state.PreviousMigration(ctx, m.schema)
	if err != nil {
		return fmt.Errorf("unable to get name of previous version: %w", err)
	}

	if err := m.rollbackExpandPhase(ctx, migration, previousMigration); err != nil {
		return err
	}

	// roll back the migration
	err = m.state.Rollback(ctx, m.schema, migration.Name)
	if err != nil {
		return fmt.Errorf("unable to rollback migration: %w", err)
	}

	m.logger.LogMigrationRollbackComplete(migration)

	return nil
}

// dropVersionSchema deletes the migration's version schema and its views, if
// they exist.
func (m *Roll) dropVersionSchema(ctx context.Context, migration *migrations.Migration) error {
	versionSchema := VersionedSchemaName(m.schema, migration.VersionSchemaName())
	_, err := m.pgConn.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", pq.QuoteIdentifier(versionSchema)))
	if err != nil {
		return err
	}

	m.logger.LogSchemaDeletion(migration.Name, versionSchema)
	return nil
}

// rollbackExpandPhase reverts a migration that is still in its expand phase
// (in-progress or complete-deferred): drops its version schema, rebuilds the
// schema state its Start ran against (parent snapshot + virtual replay of
// its own Start), and runs each operation's Rollback in reverse order.
func (m *Roll) rollbackExpandPhase(ctx context.Context, migration *migrations.Migration, parent *string) error {
	// delete the schema and views for the new version
	if err := m.dropVersionSchema(ctx, migration); err != nil {
		return err
	}

	// get the schema after the parent migration was applied
	schema := schema.New()
	if parent != nil {
		var err error
		schema, err = m.state.SchemaAfterMigration(ctx, m.schema, *parent)
		if err != nil {
			return fmt.Errorf("unable to read schema: %w", err)
		}
	}

	// Set the migration's scope so naming helpers in op.Start (replayed
	// here via UpdateVirtualSchema) and op.Rollback produce identifiers
	// matching the physical artifacts installed during Start.
	schema.MigrationScope = migrations.MigrationScopeFor(migration.Name)

	// update the in-memory schema with the results of applying the migration
	if err := migration.UpdateVirtualSchema(ctx, schema); err != nil {
		return fmt.Errorf("unable to replay changes to in-memory schema: %w", err)
	}

	return m.rollbackOperations(ctx, migration, schema)
}

// rollbackOperations runs each operation's Rollback in reverse order against
// the given schema.
func (m *Roll) rollbackOperations(ctx context.Context, migration *migrations.Migration, schema *schema.Schema) error {
	for i := len(migration.Operations) - 1; i >= 0; i-- {
		actions, err := migration.Operations[i].Rollback(m.logger, m.pgConn, schema)
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

// create view creates a view for the new version of the schema
func (m *Roll) ensureView(ctx context.Context, version, name string, table *schema.Table) error {
	columns := make([]string, 0, len(table.Columns))
	defaults := make(map[string]string, len(table.Columns))
	for k, v := range table.Columns {
		if v.Deleted {
			continue
		}
		// Skip columns whose user-facing name is a pgroll-internal artifact
		// — specifically the shared `_pgroll_needs_backfill` marker that
		// surfaces in read_schema while a migration with a backfill trigger
		// is in flight. Without this filter, a deferred migration's view
		// would project the marker and create a pg_rewrite dependency that
		// blocks the drained Complete from dropping the column.
		//
		// Only filter on the virtual key. A column with a temp physical
		// name (e.g. `_pgroll_new_<col>` from an in-flight OpAddColumn) is
		// still a valid user-facing column under its real virtual name —
		// the projection `_pgroll_new_X AS X` is correct and load-bearing.
		if strings.HasPrefix(k, "_pgroll_") {
			continue
		}
		columns = append(columns, fmt.Sprintf("%s AS %s", pq.QuoteIdentifier(v.Name), pq.QuoteIdentifier(k)))
		if v.Default != nil {
			defaults[k] = *v.Default
		}
	}

	// Create view with security_invoker option for PG 15+
	//
	// This ensures that any row level security permissions on the underlying
	// table are respected. `security_invoker` views are not supported in PG 14
	// and below.
	withOptions := ""
	if m.PGVersion() >= PGVersion15 {
		withOptions = "WITH (security_invoker = true)"
	}

	// Build the DROP + CREATE + per-column-default statements. Each is its
	// own statement (not chained with `;`) so the surrounding transaction is
	// owned by Go's `*sql.Tx` rather than an in-string BEGIN/COMMIT pair:
	// a `lock_timeout` (55P03) mid-string would otherwise abort the
	// implicit transaction and leave the pooled connection in
	// "transaction aborted" state, so RDB's retry would re-send the same
	// string into a poisoned session and get `25P02` on every subsequent
	// statement (which isn't 55P03, so the retry loop bails after one
	// attempt). WithRetryableTransaction rolls back on failure and opens a
	// fresh tx for each retry, letting the configured retry budget actually
	// work under view-projection lock contention.
	schemaName := VersionedSchemaName(m.schema, version)
	dropViewSQL := fmt.Sprintf("DROP VIEW IF EXISTS %s.%s",
		pq.QuoteIdentifier(schemaName),
		pq.QuoteIdentifier(name))
	//nolint:gosec // G201: identifiers are pq.QuoteIdentifier'd; withOptions is a fixed string; columns is built from `"<phys>" AS "<virt>"` pairs whose components are already QuoteIdentifier'd above.
	createViewSQL := fmt.Sprintf("CREATE VIEW %s.%s %s AS SELECT %s FROM %s",
		pq.QuoteIdentifier(schemaName),
		pq.QuoteIdentifier(name),
		withOptions,
		strings.Join(columns, ","),
		pq.QuoteIdentifier(table.Name))

	setDefaultSQLs := make([]string, 0, len(defaults))
	for column, defaultVal := range defaults {
		setDefaultSQLs = append(setDefaultSQLs, fmt.Sprintf(
			"ALTER VIEW %s.%s ALTER %s SET DEFAULT %s",
			pq.QuoteIdentifier(schemaName),
			pq.QuoteIdentifier(name),
			pq.QuoteIdentifier(column),
			defaultVal,
		))
	}

	return m.pgConn.WithRetryableTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, dropViewSQL); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, createViewSQL); err != nil {
			return err
		}
		for _, stmt := range setDefaultSQLs {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *Roll) performBackfills(ctx context.Context, job *backfill.Job, cfg *backfill.Config) error {
	// Inject the Roll's logger so backfill progress events surface to the
	// operator without requiring callers of NewConfig to opt in.
	backfill.WithLogger(m.logger)(cfg)

	bf := backfill.New(m.pgConn, cfg, job.MigrationScope())

	bf.CreateTriggers(ctx, job)

	for _, table := range job.Tables {
		m.logger.LogBackfillStart(table.Name)

		if err := bf.Start(ctx, table); err != nil {
			m.logger.LogRollbackOnFailure(fmt.Sprintf("backfill of table %q failed: %v", table.Name, err))
			cleanupCtx, cancel := m.rollbackContext(ctx)
			errRollback := m.Rollback(cleanupCtx)
			cancel()

			return errors.Join(
				fmt.Errorf("unable to backfill table %q: %w", table.Name, err),
				errRollback,
			)
		}

		m.logger.LogBackfillComplete(table.Name)
	}

	return nil
}

// drainDeferredMigration runs a single deferred migration's Complete
// actions through its own Coordinator and clears the complete_deferred
// flag on success. Uses raw physical schema state — no deferred-replay.
//
// With iterative drain, prior deferred migrations have *physically*
// applied their Completes by the time this migration's Complete is
// constructed, so reading the physical schema captures their effects
// directly. We deliberately don't replay later still-deferred
// migrations' Starts: those replays would clobber the schema this
// migration's `op.Complete` reads from (e.g. a later migration's
// AddColumn replay overwrites Columns[X].Name to the later migration's
// temp, hiding the user-facing physical name this Complete needs to
// drop).
//
// We also don't replay this migration's own Start: same reason —
// duplicator-pattern Completes resolve `column.Name` to derive the
// source column to drop, and the own-Start replay would install this
// migration's *own* temp name there, pointing the action at the wrong
// column.
//
// Sets the per-migration scope on the schema so naming helpers in
// `op.Complete` produce identifiers that match what's physically present
// (the temp/trigger/marker names installed by the same migration's
// earlier Start).
func (m *Roll) drainDeferredMigration(ctx context.Context, dm *migrations.Migration) error {
	if err := m.applyDeferredComplete(ctx, dm); err != nil {
		return err
	}

	if err := m.state.ClearCompleteDeferred(ctx, m.schema, dm.Name); err != nil {
		return fmt.Errorf("unable to clear complete_deferred for %q: %w", dm.Name, err)
	}
	m.logger.Info("drained deferred complete", "migration", dm.Name)
	return nil
}

// applyDeferredComplete constructs and executes a deferred migration's
// Complete actions against the current physical schema, without clearing
// its complete_deferred flag. Callers that need to order other durable
// steps before the flag clears (see SealDeferredCompletes) use this
// directly; everyone else uses drainDeferredMigration.
func (m *Roll) applyDeferredComplete(ctx context.Context, dm *migrations.Migration) error {
	drainSchema, err := m.state.ReadSchema(ctx, m.schema)
	if err != nil {
		return fmt.Errorf("unable to read schema for deferred complete %q: %w", dm.Name, err)
	}
	drainSchema.MigrationScope = migrations.MigrationScopeFor(dm.Name)

	var actions []migrations.DBAction
	for _, op := range dm.Operations {
		opActions, err := op.Complete(m.logger, m.pgConn, drainSchema)
		if err != nil {
			return fmt.Errorf("unable to collect actions for deferred complete %q: %w", dm.Name, err)
		}
		actions = append(actions, opActions...)
	}

	coord := migrations.NewCoordinator(actions)
	if err := coord.Execute(ctx); err != nil {
		return fmt.Errorf("unable to execute deferred complete %q: %w", dm.Name, err)
	}

	return nil
}

// ExistingVersionSchemas returns the names of all version schemas that
// currently exist for the Roll's underlying schema, ordered ascending by
// schema name. Version schemas are identified by their prefix
// (schema + "_"). Useful for pre-flight diagnostics — operators want to
// see exactly which schemas exist before a migrate run begins.
func (m *Roll) ExistingVersionSchemas(ctx context.Context) ([]string, error) {
	prefix := m.schema + "_"
	rows, err := m.pgConn.QueryContext(ctx,
		"SELECT schema_name FROM information_schema.schemata WHERE schema_name LIKE $1 ORDER BY schema_name",
		prefix+"%")
	if err != nil {
		return nil, fmt.Errorf("unable to list version schemas: %w", err)
	}
	defer rows.Close()

	var schemas []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("unable to scan schema name: %w", err)
		}
		schemas = append(schemas, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating version schemas: %w", err)
	}
	return schemas, nil
}

// DropVersionSchemasExcept drops all version schemas for the given schema
// except those whose version name is in the keep list. Version schemas are
// identified by their prefix (schema + "_") and cross-referenced with the
// migration history to avoid dropping unrelated schemas.
func (m *Roll) DropVersionSchemasExcept(ctx context.Context, keep ...string) error {
	keepSet := make(map[string]bool, len(keep))
	for _, k := range keep {
		keepSet[VersionedSchemaName(m.schema, k)] = true
	}

	prefix := m.schema + "_"
	rows, err := m.pgConn.QueryContext(ctx,
		"SELECT schema_name FROM information_schema.schemata WHERE schema_name LIKE $1",
		prefix+"%")
	if err != nil {
		return fmt.Errorf("unable to list version schemas: %w", err)
	}
	defer rows.Close()

	var toDrop []string
	for rows.Next() {
		var schemaName string
		if err := rows.Scan(&schemaName); err != nil {
			return fmt.Errorf("unable to scan schema name: %w", err)
		}
		if !keepSet[schemaName] {
			toDrop = append(toDrop, schemaName)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("error iterating version schemas: %w", err)
	}

	if len(toDrop) == 0 {
		m.logger.Info("deferred schema cleanup: no intermediate schemas to drop")
	} else {
		m.logger.Info("deferred schema cleanup: dropping intermediate schemas", "count", len(toDrop))
	}

	for _, s := range toDrop {
		m.logger.Info("deferred schema cleanup: dropping schema", "schema", s)
		_, err := m.pgConn.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", pq.QuoteIdentifier(s)))
		if err != nil {
			return fmt.Errorf("unable to drop version schema %q: %w", s, err)
		}
	}

	return nil
}

func VersionedSchemaName(schema string, version string) string {
	return schema + "_" + version
}
