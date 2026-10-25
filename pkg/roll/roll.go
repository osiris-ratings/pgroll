// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/lib/pq"

	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/state"
)

type PGVersion int

const (
	PGVersion15 PGVersion = 15

	// applicationName is the Postgres application_name set on the connection
	// used by the the Roll instance
	applicationName = "pgroll"
)

var (
	ErrMismatchedMigration          = fmt.Errorf("remote migration does not match local migration")
	ErrExistingSchemaWithoutHistory = fmt.Errorf("schema has existing tables but no migration history - baseline required")
)

type Roll struct {
	pgConn db.DB

	// monitor observes pgConn's session while it is busy; nil when
	// unavailable. Owned here so Close can release it.
	monitor *sql.DB

	logger migrations.Logger

	// schema we are acting on
	schema string

	// disable pgroll version schemas creation and deletion
	disableVersionSchemas bool

	// deployment target migrations are filtered to; empty means no filtering
	target string

	migrationHooks    MigrationHooks
	state             *state.State
	pgVersion         PGVersion
	skipValidation    bool
	requireReversible bool
}

// New creates a new Roll instance
func New(ctx context.Context, pgURL, schema string, state *state.State, opts ...Option) (*Roll, error) {
	rollOpts := &options{}
	for _, o := range opts {
		o(rollOpts)
	}

	conn, err := setupConn(ctx, pgURL, schema, *rollOpts)
	if err != nil {
		return nil, err
	}

	// Always use the real logger so operators see migration progress and
	// lock_timeout/retry events without needing to opt in. The --verbose flag
	// is reserved for future debug-level output and currently has no effect.
	_ = rollOpts.verbose
	logger := rollOpts.logger
	if logger == nil {
		logger = migrations.NewLogger()
	}

	// A second pool observes the migration session while it is busy (index
	// build progress). Never drawn from conn: that pool carries session-level
	// SETs and is used as one session. Observation is best effort, so a
	// failure here only disables it.
	monitor, err := setupMonitorConn(ctx, pgURL, schema)
	if err != nil {
		logger.Warn("index build progress reporting disabled", "error", err.Error())
		monitor = nil
	}

	var pgMajorVersion PGVersion
	err = conn.QueryRowContext(ctx, "SELECT substring(split_part(version(), ' ', 2) from '^[0-9]+')").Scan(&pgMajorVersion)
	if err != nil {
		return nil, fmt.Errorf("unable to retrieve postgres version: %w", err)
	}

	return &Roll{
		pgConn: &db.RDB{
			DB:                    conn,
			LockRetryTimeout:      rollOpts.lockRetryTimeout,
			Logger:                logger,
			Monitor:               monitor,
			IndexProgressInterval: rollOpts.indexProgressInterval,
		},
		monitor:               monitor,
		logger:                logger,
		schema:                schema,
		state:                 state,
		pgVersion:             pgMajorVersion,
		disableVersionSchemas: rollOpts.disableVersionSchemas,
		target:                rollOpts.target,
		migrationHooks:        rollOpts.migrationHooks,
		skipValidation:        rollOpts.skipValidation,
		requireReversible:     rollOpts.requireReversible,
	}, nil
}

func setupConn(ctx context.Context, pgURL, schema string, options options) (*sql.DB, error) {
	// pq.ParseURL is marked deprecated, but the deprecation notice ("just pass
	// the URL to sql.Open") doesn't help here: we need to *augment* the
	// resulting DSN with `search_path` and `application_name` keyword pairs,
	// which only works on the keyword-form DSN that ParseURL produces. The
	// fallback to the raw input on parse error preserves keyword-form inputs.
	//nolint:staticcheck // SA1019: deprecation suggestion does not match this use.
	dsn, err := pq.ParseURL(pgURL)
	if err != nil {
		dsn = pgURL
	}

	searchPath := append([]string{schema}, options.searchPath...)
	dsn += fmt.Sprintf(" search_path=%s application_name=%s",
		strings.Join(searchPath, ","), applicationName)

	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}

	if err := conn.PingContext(ctx); err != nil {
		return nil, err
	}

	// pgroll no longer installs the DDL-capture event triggers, and `Init`
	// drops them from databases that still carry them. This remains as a guard
	// for the one case that cleanup cannot reach: a database whose triggers are
	// owned by a role pgroll does not connect as. Cheap, and it keeps pgroll's
	// own DDL out of the capture path there.
	_, err = conn.ExecContext(ctx, "SET pgroll.no_inferred_migrations TO 'TRUE'")
	if err != nil {
		return nil, fmt.Errorf("unable to set pgroll.no_inferred_migrations to true: %w", err)
	}

	if options.lockTimeoutMs > 0 {
		_, err = conn.ExecContext(ctx, fmt.Sprintf("SET lock_timeout to '%dms'", options.lockTimeoutMs))
		if err != nil {
			return nil, fmt.Errorf("unable to set lock_timeout: %w", err)
		}
	}

	if options.role != "" {
		_, err = conn.ExecContext(ctx, fmt.Sprintf("SET ROLE %s", options.role))
		if err != nil {
			return nil, fmt.Errorf("unable to set role to '%s': %w", options.role, err)
		}
	}

	return conn, nil
}

// setupMonitorConn opens the one-connection pool that observes the migration
// session. It reads only pg_stat views, so it needs none of the session SETs
// setupConn applies; it does carry its own application_name so the extra
// backend is attributable in pg_stat_activity.
func setupMonitorConn(ctx context.Context, pgURL, schema string) (*sql.DB, error) {
	//nolint:staticcheck // SA1019: see setupConn.
	dsn, err := pq.ParseURL(pgURL)
	if err != nil {
		dsn = pgURL
	}
	dsn += fmt.Sprintf(" search_path=%s application_name=%s-monitor", schema, applicationName)

	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	if err := conn.PingContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// Init initializes the Roll instance
func (m *Roll) Init(ctx context.Context) error {
	return m.state.Init(ctx)
}

// PGVersion returns the postgres version
func (m *Roll) PGVersion() PGVersion {
	return m.pgVersion
}

// PgConn returns the underlying database connection
func (m *Roll) PgConn() db.DB {
	return m.pgConn
}

// BackendPID returns the pg_backend_pid of this Roll's DDL connection.
// Used by `pgroll migrate` pre-flight to exclude its own backend when
// probing pg_stat_activity for other live pgroll processes.
func (m *Roll) BackendPID(ctx context.Context) (int, error) {
	rows, err := m.pgConn.QueryContext(ctx, "SELECT pg_backend_pid()")
	if err != nil {
		return 0, fmt.Errorf("reading pg_backend_pid: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("reading pg_backend_pid: %w", err)
		}
		return 0, fmt.Errorf("reading pg_backend_pid: no rows")
	}
	var pid int
	if err := rows.Scan(&pid); err != nil {
		return 0, fmt.Errorf("scanning pg_backend_pid: %w", err)
	}
	return pid, nil
}

// State returns the state instance the Roll instance is acting on
func (m *Roll) State() *state.State {
	return m.state
}

// Schema returns the schema the Roll instance is acting on
func (m *Roll) Schema() string {
	return m.schema
}

func (m *Roll) UseVersionSchema() bool {
	return !m.disableVersionSchemas
}

// Target returns the deployment target migrations are filtered to, or the
// empty string when no filtering is in effect.
func (m *Roll) Target() string {
	return m.target
}

func (m *Roll) Close() error {
	err := m.state.Close()
	if err != nil {
		return err
	}

	if m.monitor != nil {
		if err := m.monitor.Close(); err != nil {
			return err
		}
	}

	return m.pgConn.Close()
}
