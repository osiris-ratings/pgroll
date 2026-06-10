// SPDX-License-Identifier: Apache-2.0

package roll

import "time"

type options struct {
	// lock timeout in milliseconds for pgroll DDL operations
	lockTimeoutMs int

	// total wall-clock budget for retrying lock_timeout errors before giving up
	lockRetryTimeout time.Duration

	// optional role to set before executing migrations
	role string

	// disable pgroll version schemas creation and deletion
	disableVersionSchemas bool

	// additional entries to add to the search_path during migration execution
	searchPath []string

	// whether to skip validation
	skipValidation bool

	// require every migration to be revertible (or explicitly marked
	// `irreversible: true`)
	requireReversible bool

	migrationHooks MigrationHooks

	verbose bool
}

// MigrationHooks defines hooks that can be set to be called at various points
// during the migration process
type MigrationHooks struct {
	// BeforeStartDDL is called before the DDL phase of migration start
	BeforeStartDDL func(*Roll) error
	// AfterStartDDL is called after the DDL phase of migration start is complete
	AfterStartDDL func(*Roll) error
	// BeforeCompleteDDL is called before the DDL phase of migration complete
	BeforeCompleteDDL func(*Roll) error
	// AfterCompleteDDL is called after the DDL phase of migration complete is complete
	AfterCompleteDDL func(*Roll) error
}

type Option func(*options)

// WithLockTimeoutMs sets the lock timeout in milliseconds for pgroll DDL operations
func WithLockTimeoutMs(lockTimeoutMs int) Option {
	return func(o *options) {
		o.lockTimeoutMs = lockTimeoutMs
	}
}

// WithLockRetryTimeout sets the total wall-clock budget for retrying queries
// that fail with a lock_timeout error (SQLSTATE 55P03). When the budget is
// exhausted the underlying lock_timeout error is returned so the caller can
// run cleanup. Zero uses the default (5 minutes); a negative value disables
// retries entirely.
func WithLockRetryTimeout(d time.Duration) Option {
	return func(o *options) {
		o.lockRetryTimeout = d
	}
}

// WithRole sets the role to set before executing migrations
func WithRole(role string) Option {
	return func(o *options) {
		o.role = role
	}
}

// WithVersionSchema enables or disables the creation of version schema for
// migrations.
func WithVersionSchema(enabled bool) Option {
	return func(o *options) {
		o.disableVersionSchemas = !enabled
	}
}

// WithMigrationHooks sets the migration hooks for the Roll instance
// Migration hooks are called at various points during the migration process
// to allow for custom behavior to be injected
func WithMigrationHooks(hooks MigrationHooks) Option {
	return func(o *options) {
		o.migrationHooks = hooks
	}
}

// WithSearchPath sets the search_path to use during migration execution. The
// schema in which the migration is run is always included in the search path,
// regardless of this setting.
func WithSearchPath(schemas ...string) Option {
	return func(o *options) {
		o.searchPath = schemas
	}
}

// WithSkipValidation controls whether or not to perform validation on
// migrations. If set to true, validation will be skipped.
func WithSkipValidation(skip bool) Option {
	return func(o *options) {
		o.skipValidation = skip
	}
}

// WithRequireReversible requires every migration to pass
// Migration.ValidateReversibility before it can be started: operations that
// need a 'down' expression to be revertible must declare one, unless the
// migration is explicitly marked `irreversible: true`. The pgroll CLI always
// sets this; it ensures `pgroll revert` can walk back out of any applied
// migration.
func WithRequireReversible() Option {
	return func(o *options) {
		o.requireReversible = true
	}
}

// WithLogging enables verbose logging for the Roll instance
func WithLogging(enabled bool) Option {
	return func(o *options) {
		if enabled {
			o.verbose = enabled
		}
	}
}

// startOptions holds options for a single Start invocation.
type startOptions struct {
	skipVersionSchema bool
}

// StartOption is a functional option for the Start method.
type StartOption func(*startOptions)

// WithoutVersionSchema returns a StartOption that disables version schema
// creation for this Start call. Used by `pgroll migrate` for intermediate
// migrations in a batch — no apps will ever connect to an intermediate
// version, so projecting it wastes a schema and (more importantly) creates
// view dependencies that block destructive operations later in the batch.
func WithoutVersionSchema() StartOption {
	return func(o *startOptions) { o.skipVersionSchema = true }
}

// StartOptionsSkipVersionSchema reports whether the given StartOption set
// would suppress version schema creation. Used by callers (e.g. the
// `pgroll migrate` command) to tailor user-facing output to whether a
// schema was actually projected.
func StartOptionsSkipVersionSchema(opts ...StartOption) bool {
	var o startOptions
	for _, opt := range opts {
		opt(&o)
	}
	return o.skipVersionSchema
}
