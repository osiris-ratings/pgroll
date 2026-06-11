// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"

	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/schema"
)

//go:embed init.sql
var sqlInit string

// applicationName is the Postgres application_name set on the connection used
// by the the State instance
const applicationName = "pgroll-state"

type State struct {
	pgConn        *sql.DB
	pgrollVersion string
	schema        string
}

func New(ctx context.Context, pgURL, stateSchema string, opts ...StateOpt) (*State, error) {
	// pq.ParseURL is marked deprecated, but the deprecation notice ("just pass
	// the URL to sql.Open") doesn't help here: we need to *augment* the DSN
	// with `search_path` and `application_name` keyword pairs, which only
	// works on the keyword-form DSN that ParseURL produces. See the matching
	// comment in pkg/roll/roll.go::setupConn.
	//nolint:staticcheck // SA1019: deprecation suggestion does not match this use.
	dsn, err := pq.ParseURL(pgURL)
	if err != nil {
		dsn = pgURL
	}

	dsn += fmt.Sprintf(" search_path=%s application_name=%s", stateSchema, applicationName)

	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}

	if err := conn.PingContext(ctx); err != nil {
		return nil, err
	}

	_, err = conn.ExecContext(ctx, "SET pgroll.no_inferred_migrations TO 'TRUE'")
	if err != nil {
		return nil, fmt.Errorf("unable to set pgroll.no_inferred_migrations to true: %w", err)
	}

	st := &State{
		pgConn:        conn,
		pgrollVersion: "development",
		schema:        stateSchema,
	}

	// Apply options to the State instance
	for _, opt := range opts {
		opt(st)
	}

	// Check version compatibility between the pgroll version and the version of
	// the pgroll state schema.
	compat, err := st.VersionCompatibility(ctx)
	if err != nil {
		return nil, err
	}

	// If the state schema is newer than the pgroll version, return an error
	if compat == VersionCompatVersionSchemaNewer {
		schemaVersion := "unknown"
		if v, err := st.SchemaVersion(ctx); err == nil {
			schemaVersion = v
		}
		return nil, fmt.Errorf("%w: binary: %s vs schema: %s", ErrNewPgrollSchema, st.pgrollVersion, schemaVersion)
	}

	// if the state schema is older than the pgroll version, re-initialize the
	// state schema
	if compat == VersionCompatVersionSchemaOlder {
		if err := st.Init(ctx); err != nil {
			return nil, err
		}
	}

	return st, nil
}

// Init initializes the required pg_roll schema to store the state
func (s *State) Init(ctx context.Context) error {
	tx, err := s.pgConn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Try to obtain an advisory lock.
	// The key is an arbitrary number, used to distinguish the lock from other locks.
	// The lock is automatically released when the transaction is committed or rolled back.
	const key int64 = 0x2c03057fb9525b
	_, err = tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock($1)", key)
	if err != nil {
		return err
	}

	// Perform pgroll state initialization
	q := strings.ReplaceAll(sqlInit, "placeholder", pq.QuoteIdentifier(s.schema))
	_, err = tx.ExecContext(ctx, q)
	if err != nil {
		return err
	}

	// Clear the pgroll_version table
	_, err = tx.ExecContext(ctx, fmt.Sprintf("TRUNCATE TABLE %s.pgroll_version",
		pq.QuoteIdentifier(s.schema)))
	if err != nil {
		return err
	}

	// Insert the version of `pgroll` that is being initialized into the
	// pgroll_version table
	_, err = tx.ExecContext(ctx, fmt.Sprintf("INSERT INTO %s.pgroll_version (version) VALUES ($1)",
		pq.QuoteIdentifier(s.schema)),
		s.pgrollVersion)
	if err != nil {
		return err
	}

	return tx.Commit()
}

func (s *State) PgConn() *sql.DB {
	return s.pgConn
}

// IsInitialized checks if the pgroll state schema is initialized.
func (s *State) IsInitialized(ctx context.Context) (bool, error) {
	var isInitialized bool
	err := s.pgConn.QueryRowContext(ctx,
		"SELECT EXISTS (SELECT 1 from pg_catalog.pg_namespace WHERE nspname = $1)",
		s.schema).Scan(&isInitialized)
	if err != nil {
		return false, err
	}

	return isInitialized, nil
}

func (s *State) Close() error {
	return s.pgConn.Close()
}

// Schema returns the schema name
func (s *State) Schema() string {
	return s.schema
}

// HasExistingSchemaWithoutHistory checks if there's an existing schema with
// tables but no migration history. Returns true if the schema exists, has
// tables, but has no pgroll migration history
func (s *State) HasExistingSchemaWithoutHistory(ctx context.Context, schemaName string) (bool, error) {
	// Check if pgroll is initialized
	ok, err := s.IsInitialized(ctx)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	// Check if there's any migration history for this schema
	var migrationCount int
	err = s.pgConn.QueryRowContext(ctx,
		fmt.Sprintf("SELECT COUNT(*) FROM %s.migrations WHERE schema=$1", pq.QuoteIdentifier(s.schema)),
		schemaName).Scan(&migrationCount)
	if err != nil {
		return false, fmt.Errorf("failed to check migration history: %w", err)
	}

	// If there's migration history, return false
	if migrationCount > 0 {
		return false, nil
	}

	// Check if the schema is empty or not, as determined by ReadSchema
	schema, err := s.ReadSchema(ctx, schemaName)
	if err != nil {
		return false, fmt.Errorf("failed to read schema: %w", err)
	}

	// Return true if there are tables but no migration history
	return len(schema.Tables) > 0, nil
}

// IsActiveMigrationPeriod returns true if there is an active migration
func (s *State) IsActiveMigrationPeriod(ctx context.Context, schema string) (bool, error) {
	var isActive bool
	err := s.pgConn.QueryRowContext(ctx, fmt.Sprintf("SELECT %s.is_active_migration_period($1)", pq.QuoteIdentifier(s.schema)), schema).Scan(&isActive)
	if err != nil {
		return false, err
	}

	return isActive, nil
}

// BackendPID returns the pg_backend_pid of this State's connection. Used by
// callers that need to exclude their own backends when probing
// pg_stat_activity for other live pgroll processes.
func (s *State) BackendPID(ctx context.Context) (int, error) {
	var pid int
	if err := s.pgConn.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		return 0, fmt.Errorf("reading pg_backend_pid: %w", err)
	}
	return pid, nil
}

// OtherPgrollBackends returns the count of backends connected to the current
// database with application_name set to 'pgroll' or 'pgroll-state',
// excluding the supplied PIDs (typically the caller's own state and DDL
// connections).
//
// `pgroll migrate` uses this to distinguish a migration that's currently
// being executed by a live pgroll process (IN-PROGRESS) from one whose
// owning process has died and left a done=FALSE row behind (INTERRUPTED).
//
// The application_name strings here mirror the package-private constants in
// pkg/state and pkg/roll; if those ever change, this list must change with
// them.
func (s *State) OtherPgrollBackends(ctx context.Context, excludePIDs []int) (int, error) {
	pids := make([]int64, len(excludePIDs))
	for i, p := range excludePIDs {
		pids[i] = int64(p)
	}
	const q = `
		SELECT count(*)
		FROM pg_stat_activity
		WHERE application_name IN ('pgroll', 'pgroll-state')
		  AND datname = current_database()
		  AND pid <> ALL($1)`
	var count int
	if err := s.pgConn.QueryRowContext(ctx, q, pq.Array(pids)).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting pgroll backends: %w", err)
	}
	return count, nil
}

// GetActiveMigration returns the name & raw content of the active migration (if any), errors out otherwise
func (s *State) GetActiveMigration(ctx context.Context, schema string) (*migrations.Migration, error) {
	var name, rawMigration string
	err := s.pgConn.QueryRowContext(ctx, fmt.Sprintf("SELECT name, migration FROM %s.migrations WHERE schema=$1 AND done=false", pq.QuoteIdentifier(s.schema)), schema).Scan(&name, &rawMigration)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNoActiveMigration
		}
		return nil, err
	}

	var migration migrations.Migration
	err = json.Unmarshal([]byte(rawMigration), &migration)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal migration: %w", err)
	}
	migration.Name = name

	return &migration, nil
}

// Start creates a new migration, storing its name and raw content
// this will effectively activate a new migration period, so `IsActiveMigrationPeriod` will return true
// until the migration is completed
// This method will return the current schema (before the migration is applied)
func (s *State) Start(ctx context.Context, schemaname string, migration *migrations.Migration) error {
	rawMigration, err := json.Marshal(migration)
	if err != nil {
		return fmt.Errorf("unable to marshal migration: %w", err)
	}

	// create a new migration object and return the previous known schema
	// if there is no previous migration, read the schema from postgres
	stmt := fmt.Sprintf(`INSERT INTO %[1]s.migrations (schema, name, parent, migration) VALUES ($1, $2, %[1]s.latest_migration($1), $3)`,
		pq.QuoteIdentifier(s.schema))

	_, err = s.pgConn.ExecContext(ctx, stmt, schemaname, migration.Name, rawMigration)
	return err
}

// Complete marks a migration as completed
func (s *State) Complete(ctx context.Context, schema, name string) error {
	res, err := s.pgConn.ExecContext(ctx, fmt.Sprintf("UPDATE %[1]s.migrations SET done=$1, resulting_schema=(SELECT %[1]s.read_schema($2)) WHERE schema=$2 AND name=$3 AND done=$4", pq.QuoteIdentifier(s.schema)), true, schema, name, false)
	if err != nil {
		return err
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if rows == 0 {
		return fmt.Errorf("no migration found with name %s", name)
	}

	return err
}

// MarkCompleteDeferred marks a migration as logically done while flagging
// its Complete operations for replay during the next non-deferred Complete.
// Captures resulting_schema as a normal Complete would, so subsequent reads
// see the schema state implied by the migration even though the DDL hasn't
// physically run yet.
func (s *State) MarkCompleteDeferred(ctx context.Context, schema, name string) error {
	res, err := s.pgConn.ExecContext(ctx, fmt.Sprintf(
		"UPDATE %[1]s.migrations SET done=TRUE, complete_deferred=TRUE, resulting_schema=(SELECT %[1]s.read_schema($1)) WHERE schema=$1 AND name=$2 AND done=FALSE",
		pq.QuoteIdentifier(s.schema),
	), schema, name)
	if err != nil {
		return err
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if rows == 0 {
		return fmt.Errorf("no active migration found with name %s", name)
	}

	return nil
}

// DeferredCompletes returns the migrations whose Complete operations are
// queued for replay, in the order they were applied (which equals
// parent-chain order across all chains).
//
// We sort by created_at because pgroll.migrations rows are inserted
// in-order as each migration's Start runs, so created_at is monotonic
// along the parent chain. A naïve recursive-CTE walk that walks each
// disjoint chain separately and orders by depth would interleave
// chain-roots with chain-children incorrectly: when a deferred chain has
// a non-deferred migration in the middle, both segments form separate
// roots and depth=0 of all roots comes before depth=1 of any root —
// leaving a later chain-root drained before an earlier chain's children.
// Order by created_at sidesteps the multiple-chain shape entirely.
func (s *State) DeferredCompletes(ctx context.Context, schema string) ([]*migrations.Migration, error) {
	q := fmt.Sprintf(`
		SELECT name, migration FROM %[1]s.migrations
		WHERE schema = $1 AND complete_deferred = TRUE
		ORDER BY created_at ASC
	`, pq.QuoteIdentifier(s.schema))

	rows, err := s.pgConn.QueryContext(ctx, q, schema)
	if err != nil {
		return nil, fmt.Errorf("unable to query deferred completes: %w", err)
	}
	defer rows.Close()

	var out []*migrations.Migration
	for rows.Next() {
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, fmt.Errorf("unable to scan deferred complete row: %w", err)
		}
		var mig migrations.Migration
		if err := json.Unmarshal([]byte(raw), &mig); err != nil {
			return nil, fmt.Errorf("unable to unmarshal deferred migration %q: %w", name, err)
		}
		mig.Name = name
		out = append(out, &mig)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating deferred completes: %w", err)
	}
	return out, nil
}

// HasSealedDeferred reports whether any migration is both sealed and still
// queued (complete_deferred). Because the seal stamps before draining, this
// is the durable signature of an interrupted seal: the drain must be resumed
// before anything else touches the queue.
func (s *State) HasSealedDeferred(ctx context.Context, schema string) (bool, error) {
	var exists bool
	err := s.pgConn.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT EXISTS(SELECT 1 FROM %s.migrations WHERE schema=$1 AND complete_deferred AND sealed)",
		pq.QuoteIdentifier(s.schema),
	), schema).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("unable to check for sealed deferred migrations: %w", err)
	}
	return exists, nil
}

// ClearCompleteDeferred clears the complete_deferred flag on a migration
// after its queued operations have successfully replayed.
func (s *State) ClearCompleteDeferred(ctx context.Context, schema, name string) error {
	_, err := s.pgConn.ExecContext(ctx, fmt.Sprintf(
		"UPDATE %s.migrations SET complete_deferred=FALSE WHERE schema=$1 AND name=$2",
		pq.QuoteIdentifier(s.schema),
	), schema, name)
	return err
}

// GetMigration returns the named migration from the history, parsed from its
// stored JSON definition.
func (s *State) GetMigration(ctx context.Context, schema, name string) (*migrations.Migration, error) {
	var rawMigration string
	err := s.pgConn.QueryRowContext(ctx, fmt.Sprintf(
		"SELECT migration FROM %s.migrations WHERE schema=$1 AND name=$2",
		pq.QuoteIdentifier(s.schema),
	), schema, name).Scan(&rawMigration)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("migration %q not found in history", name)
		}
		return nil, err
	}

	var migration migrations.Migration
	if err := json.Unmarshal([]byte(rawMigration), &migration); err != nil {
		return nil, fmt.Errorf("unable to unmarshal migration %q: %w", name, err)
	}
	migration.Name = name

	return &migration, nil
}

// RefreshResultingSchema re-captures the resulting_schema of the named
// migration from the live physical schema. MarkCompleteDeferred snapshots
// the schema mid-flight, while pgroll temp artifacts from the migration's
// own (and sibling) expand phases are still physically present; once the
// deferred queue has fully drained, the physical schema is the clean
// post-contraction state and the boundary row's snapshot must reflect it —
// SchemaAfterMigration consumers (rollback, revert) depend on this.
func (s *State) RefreshResultingSchema(ctx context.Context, schema, name string) error {
	res, err := s.pgConn.ExecContext(ctx, fmt.Sprintf(
		"UPDATE %[1]s.migrations SET resulting_schema=(SELECT %[1]s.read_schema($1)) WHERE schema=$1 AND name=$2",
		pq.QuoteIdentifier(s.schema),
	), schema, name)
	if err != nil {
		return err
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("no migration found with name %s", name)
	}

	return nil
}

// ReadSchema reads the schema for the specified schema name
func (s *State) ReadSchema(ctx context.Context, schemaName string) (*schema.Schema, error) {
	var rawSchema []byte
	err := s.pgConn.QueryRowContext(ctx, fmt.Sprintf("SELECT %s.read_schema($1)", pq.QuoteIdentifier(s.schema)), schemaName).Scan(&rawSchema)
	if err != nil {
		return nil, err
	}

	var sc schema.Schema
	err = json.Unmarshal(rawSchema, &sc)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal schema: %w", err)
	}

	return &sc, nil
}

// SchemaAfterMigration reads the schema after the migration `version` was
// applied to `schemaName`
func (s *State) SchemaAfterMigration(ctx context.Context, schemaName, version string) (*schema.Schema, error) {
	sql := fmt.Sprintf("SELECT resulting_schema FROM %s.migrations WHERE schema=$1 AND name=$2", pq.QuoteIdentifier(s.schema))

	var rawSchema []byte
	err := s.pgConn.QueryRowContext(ctx, sql, schemaName, version).Scan(&rawSchema)
	if err != nil {
		return nil, err
	}

	var sc schema.Schema
	err = json.Unmarshal(rawSchema, &sc)
	if err != nil {
		return nil, fmt.Errorf("unable to unmarshal schema: %w", err)
	}

	return &sc, nil
}

// Rollback removes a migration from the state (we consider it rolled back, as if it never started)
func (s *State) Rollback(ctx context.Context, schema, name string) error {
	res, err := s.pgConn.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s.migrations WHERE schema=$1 AND name=$2 AND done=$3", pq.QuoteIdentifier(s.schema)), schema, name, false)
	if err != nil {
		return err
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}

	if rows == 0 {
		return fmt.Errorf("no migration found with name %s", name)
	}

	return nil
}

// CreateBaseline creates a baseline migration that captures the current state of the schema.
// It marks the migration as 'baseline' type and completed (done=true).
// This is used when you want to start using pgroll with an existing database.
func (s *State) CreateBaseline(ctx context.Context, schemaName, baselineVersion string) error {
	// Check if baseline can be created (no active migrations, etc)
	isActive, err := s.IsActiveMigrationPeriod(ctx, schemaName)
	if err != nil {
		return fmt.Errorf("failed to check for active migrations: %w", err)
	}
	if isActive {
		return fmt.Errorf("cannot create baseline while a migration is in progress")
	}

	// Read the current schema
	schema, err := s.ReadSchema(ctx, schemaName)
	if err != nil {
		return fmt.Errorf("failed to read schema: %w", err)
	}

	// Create an empty migration with just a name
	emptyMigration := migrations.Migration{
		Name:       baselineVersion,
		Operations: migrations.Operations{},
	}

	rawMigration, err := json.Marshal(emptyMigration)
	if err != nil {
		return fmt.Errorf("unable to marshal migration: %w", err)
	}

	rawSchema, err := json.Marshal(schema)
	if err != nil {
		return fmt.Errorf("unable to marshal schema: %w", err)
	}

	// Insert a baseline migration record. Baselines are sealed at insert:
	// they capture pre-existing state pgroll knows nothing about, so there
	// is nothing to revert to behind them.
	stmt := fmt.Sprintf(`
		INSERT INTO %[1]s.migrations
		(schema, name, migration, resulting_schema, done, sealed, parent, migration_type, created_at, updated_at)
		VALUES ($1, $2, $3, $4, TRUE, TRUE, %[1]s.latest_migration($1), 'baseline', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		pq.QuoteIdentifier(s.schema))

	_, err = s.pgConn.ExecContext(ctx, stmt, schemaName, baselineVersion, rawMigration, rawSchema)
	if err != nil {
		return fmt.Errorf("failed to insert baseline migration: %w", err)
	}

	return nil
}

// MigrationExists reports whether a migration with the given name is already
// recorded for the schema. Used by callers that need to skip already-recorded
// migrations regardless of baseline boundary (which SchemaHistory respects).
func (s *State) MigrationExists(ctx context.Context, schemaName, name string) (bool, error) {
	var exists bool
	err := s.pgConn.QueryRowContext(ctx,
		fmt.Sprintf("SELECT EXISTS (SELECT 1 FROM %s.migrations WHERE schema=$1 AND name=$2)",
			pq.QuoteIdentifier(s.schema)),
		schemaName, name).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking migration existence: %w", err)
	}
	return exists, nil
}

// Stamp records a single migration in the state as already-applied without
// executing any DDL. Inserts one row into the migrations table with done=TRUE
// and the supplied body, resulting_schema, parent, and migration_type. Used by
// `pgroll stamp` to formalize alembic-style state stamping after loading a
// SQL dump (or recovering from missing state).
//
// migration:        marshalled migration body to store. May be a real
//
//	parsed migration JSON or an empty `{}` placeholder.
//
// resultingSchema:  marshalled schema.Schema to store as the post-migration
//
//	state. nil falls back to the SQL default '{}'.
//
// parent:           explicit parent name. nil resolves to latest_migration()
//
//	at insert time, mirroring State.Start.
//
// migrationType:    one of "pgroll", "baseline", "inferred". Empty string
//
//	falls back to the migrations table default ("pgroll").
func (s *State) Stamp(
	ctx context.Context,
	schemaName, name string,
	migration []byte,
	resultingSchema []byte,
	parent *string,
	migrationType string,
) error {
	parentClause := fmt.Sprintf("%s.latest_migration($1)", pq.QuoteIdentifier(s.schema))
	args := []any{schemaName, name, migration}
	if parent != nil {
		parentClause = fmt.Sprintf("$%d", len(args)+1)
		args = append(args, *parent)
	}

	resultingClause := "'{}'::jsonb"
	if resultingSchema != nil {
		resultingClause = fmt.Sprintf("$%d", len(args)+1)
		args = append(args, resultingSchema)
	}

	typeClause := "DEFAULT"
	if migrationType != "" {
		typeClause = fmt.Sprintf("$%d", len(args)+1)
		args = append(args, migrationType)
	}

	// Schema identifier passes through pq.QuoteIdentifier; the other
	// formatted segments are fixed strings (parameter placeholders or
	// literal `DEFAULT`/`'{}'::jsonb`) chosen by call-site logic above.
	// Same shape as State.CreateBaseline / State.Start.
	//
	// Stamped rows are sealed at insert: the recorded DDL already happened
	// outside pgroll (dump load, state recovery), so there is no expand
	// state to revert to.
	//nolint:gosec // G201: schema is identifier-quoted; segments are fixed strings.
	stmt := fmt.Sprintf(`
		INSERT INTO %[1]s.migrations
		(schema, name, migration, resulting_schema, done, sealed, parent, migration_type, created_at, updated_at)
		VALUES ($1, $2, $3, %[2]s, TRUE, TRUE, %[3]s, %[4]s, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		pq.QuoteIdentifier(s.schema), resultingClause, parentClause, typeClause)

	if _, err := s.pgConn.ExecContext(ctx, stmt, args...); err != nil {
		return fmt.Errorf("failed to stamp migration %q: %w", name, err)
	}
	return nil
}
