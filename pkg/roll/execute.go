// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
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
	s, err := m.state.ReadSchema(ctx, m.schema)
	if err != nil {
		return nil, err
	}

	deferred, err := m.state.DeferredCompletes(ctx, m.schema)
	if err != nil {
		return nil, fmt.Errorf("unable to query deferred completes: %w", err)
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

	// Collect deferred Completes from prior intermediates. Their actions get
	// merged with the active migration's actions and run through a single
	// Coordinator below. The Coordinator's dedup-and-move-later behavior is
	// load-bearing here: shared cleanups (e.g. drop_column on the per-table
	// `_pgroll_needs_backfill` marker) appear in every contributing
	// migration's action list, and the merged ordering pushes that drop
	// after every contributing migration's trigger-function drop, so the
	// column drop doesn't fail on lingering trigger dependencies.
	//
	// Skip drain under WithSkipSchemaDrop — that path keeps the
	// prev-production schema around, which would block destructive drained
	// DDL with the same dependency error the deferral was set up to avoid.
	// Leave the queue for whoever runs the next non-deferred Complete.
	var drainedMigrations []string
	var deferredActions []migrations.DBAction
	if !o.skipSchemaDrop {
		queued, err := m.state.DeferredCompletes(ctx, m.schema)
		if err != nil {
			return fmt.Errorf("unable to query deferred completes: %w", err)
		}
		for _, dm := range queued {
			drainedMigrations = append(drainedMigrations, dm.Name)
			drainSchema, err := m.state.ReadSchema(ctx, m.schema)
			if err != nil {
				return fmt.Errorf("unable to read schema for deferred complete %q: %w", dm.Name, err)
			}
			// Set this deferred migration's scope on the schema so its
			// op.Complete naming helpers produce identifiers matching
			// what's physically present (the temp/trigger/marker names
			// installed by the same migration's earlier Start).
			drainSchema.MigrationScope = migrations.MigrationScopeFor(dm.Name)
			for _, op := range dm.Operations {
				opActions, err := op.Complete(m.logger, m.pgConn, drainSchema)
				if err != nil {
					return fmt.Errorf("unable to collect actions for deferred complete %q: %w", dm.Name, err)
				}
				deferredActions = append(deferredActions, opActions...)
			}
		}
		if len(drainedMigrations) > 0 {
			m.logger.Info("merging deferred completes into final action set", "count", len(drainedMigrations))
		}
	}

	// read the current schema
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

	// execute operations
	refreshViews := false
	actions := deferredActions
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

	// Coordinator succeeded — clear complete_deferred for the drained
	// migrations now that their actions have physically applied. Done after
	// successful execution so a Coordinator failure leaves the queue
	// intact: an operator can fix the underlying issue and re-run
	// `pgroll complete` to retry. Idempotent DDL (DROP COLUMN IF EXISTS,
	// DROP FUNCTION IF EXISTS CASCADE) makes the retry safe for
	// already-executed actions.
	for _, name := range drainedMigrations {
		if err := m.state.ClearCompleteDeferred(ctx, m.schema, name); err != nil {
			return fmt.Errorf("unable to clear complete_deferred for %q: %w", name, err)
		}
	}

	// Recreate views for the new version (if some operations require it, ie
	// SQL). Skipped under WithSkipSchemaDrop because that flag signals an
	// intermediate migration in a `pgroll migrate` batch — those don't
	// project a version schema in Start, and shouldn't materialize one
	// here in Complete either.
	if refreshViews && !o.skipSchemaDrop && !m.disableVersionSchemas {
		currentSchema, err = m.state.ReadSchema(ctx, m.schema)
		if err != nil {
			return fmt.Errorf("unable to read schema: %w", err)
		}

		err = m.ensureViews(ctx, currentSchema, migration)
		if err != nil {
			return err
		}
	}

	// mark as completed
	err = m.state.Complete(ctx, m.schema, migration.Name)
	if err != nil {
		return fmt.Errorf("unable to complete migration: %w", err)
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

	// delete the schema and views for the new version
	versionSchema := VersionedSchemaName(m.schema, migration.VersionSchemaName())
	_, err = m.pgConn.ExecContext(ctx, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", pq.QuoteIdentifier(versionSchema)))
	if err != nil {
		return err
	}

	m.logger.LogSchemaDeletion(migration.Name, versionSchema)

	// get the name of the previous migration
	previousMigration, err := m.state.PreviousMigration(ctx, m.schema)
	if err != nil {
		return fmt.Errorf("unable to get name of previous version: %w", err)
	}

	// get the schema after the previous migration was applied
	schema := schema.New()
	if previousMigration != nil {
		schema, err = m.state.SchemaAfterMigration(ctx, m.schema, *previousMigration)
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

	// roll back operations in reverse order
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

	// roll back the migration
	err = m.state.Rollback(ctx, m.schema, migration.Name)
	if err != nil {
		return fmt.Errorf("unable to rollback migration: %w", err)
	}

	m.logger.LogMigrationRollbackComplete(migration)

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

	// We must set column default values for the views directly, as the
	// values are not kept from the underlying tables.
	var addDefaultsToView string
	for column, defaultVal := range defaults {
		addDefaultsToView += fmt.Sprintf("ALTER VIEW %s.%s ALTER %s SET DEFAULT %s; ",
			pq.QuoteIdentifier(VersionedSchemaName(m.schema, version)),
			pq.QuoteIdentifier(name),
			pq.QuoteIdentifier(column),
			defaultVal)
	}
	_, err := m.pgConn.ExecContext(ctx,
		fmt.Sprintf("BEGIN; DROP VIEW IF EXISTS %s.%s; CREATE VIEW %s.%s %s AS SELECT %s FROM %s; %s COMMIT",
			pq.QuoteIdentifier(VersionedSchemaName(m.schema, version)),
			pq.QuoteIdentifier(name),
			pq.QuoteIdentifier(VersionedSchemaName(m.schema, version)),
			pq.QuoteIdentifier(name),
			withOptions,
			strings.Join(columns, ","),
			pq.QuoteIdentifier(table.Name),
			addDefaultsToView))
	if err != nil {
		return err
	}
	return nil
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
