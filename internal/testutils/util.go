// SPDX-License-Identifier: Apache-2.0

package testutils

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/xataio/pgroll/pkg/roll"
	"github.com/xataio/pgroll/pkg/state"
)

// The version of postgres against which the tests are run
// if the POSTGRES_VERSION environment variable is not set.
const defaultPostgresVersion = "18.0"

// tConnStr holds the connection string to the test container created in TestMain.
var tConnStr string

// SharedTestMain starts a postgres container to be used by all tests in a package. Each test then
// connects to the container and creates a new database. Optional functions that will run after all
// tests can be added and should return a nil error to indicate they ran successfully. If they return
// an error all subsequent functions will be skipped.
func SharedTestMain(m *testing.M, postRunHooks ...func() error) {
	ctx := context.Background()

	// The timeout is a ceiling, not a sleep: the wait returns the instant the
	// log line appears, so a generous value costs nothing when the machine is
	// idle and is the difference between green and red when it is not.
	//
	// Five seconds was too tight. `go test ./...` runs package binaries in
	// parallel and eight packages start their own Postgres, so on a busy CI
	// runner several containers pull and boot at once. Whichever one loses the
	// race blows the budget and every test in that package fails with
	// "database system is not yet accepting connections" — which reads like a
	// test failure and is really a scheduling one. It showed up as unrelated
	// packages (defaults, benchmarks, backfill) failing in different jobs of
	// the same run.
	waitForLogs := wait.
		ForLog("database system is ready to accept connections").
		WithOccurrence(2).
		WithStartupTimeout(60 * time.Second)

	pgVersion := os.Getenv("POSTGRES_VERSION")
	if pgVersion == "" {
		pgVersion = defaultPostgresVersion
	}

	ctr, err := postgres.Run(
		ctx,
		"postgres:"+pgVersion,
		testcontainers.WithWaitStrategy(waitForLogs),
	)
	if err != nil {
		os.Exit(1)
	}

	tConnStr, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		os.Exit(1)
	}

	db, err := sql.Open("postgres", tConnStr)
	if err != nil {
		os.Exit(1)
	}

	// create handy role for tests
	_, err = db.ExecContext(ctx, "CREATE ROLE pgroll")
	if err != nil {
		os.Exit(1)
	}

	exitCode := m.Run()

	if err := ctr.Terminate(ctx); err != nil {
		log.Printf("Failed to terminate container: %v", err)
	}

	if exitCode != 0 {
		log.Printf("Non zero exit code (%d), skipping post run hooks", exitCode)
		os.Exit(exitCode)
	}

	for _, hook := range postRunHooks {
		err := hook()
		if err != nil {
			log.Printf("Post-run hook failed: %v", err)
			os.Exit(1)
		}
	}

	os.Exit(0)
}

// TestSchema returns the schema in which migration tests apply migrations. By
// default, migrations will be applied to the "public" schema.
func TestSchema() string {
	testSchema := os.Getenv("PGROLL_TEST_SCHEMA")
	if testSchema != "" {
		return testSchema
	}
	return "public"
}

func WithStateInSchemaAndConnectionToContainer(t *testing.T, schema string, fn func(*state.State, *sql.DB)) {
	t.Helper()
	ctx := context.Background()

	db, connStr, _ := setupTestDatabase(t)

	st, err := state.New(ctx, connStr, schema)
	if err != nil {
		t.Fatal(err)
	}

	if err := st.Init(ctx); err != nil {
		t.Fatal(err)
	}

	fn(st, db)
}

func WithConnectionToContainer(t *testing.T, fn func(*sql.DB, string)) {
	t.Helper()

	db, connStr, _ := setupTestDatabase(t)

	fn(db, connStr)
}

func WithStateAndConnectionToContainer(t *testing.T, fn func(*state.State, *sql.DB)) {
	WithStateInSchemaAndConnectionToContainer(t, "pgroll", fn)
}

func WithStateAtVersionAndConnectionToContainer(t *testing.T, version string, fn func(*state.State, string, *sql.DB)) {
	t.Helper()
	ctx := context.Background()

	db, connStr, _ := setupTestDatabase(t)

	st, err := state.New(ctx, connStr, "pgroll", state.WithPgrollVersion(version))
	if err != nil {
		t.Fatal(err)
	}

	if err := st.Init(ctx); err != nil {
		t.Fatal(err)
	}

	fn(st, connStr, db)
}

func WithUninitializedState(t *testing.T, fn func(*state.State)) {
	t.Helper()
	ctx := context.Background()

	_, connStr, _ := setupTestDatabase(t)

	st, err := state.New(ctx, connStr, "pgroll")
	if err != nil {
		t.Fatal(err)
	}

	fn(st)
}

func WithUninitializedStateAndConnectionInfo(t *testing.T, fn func(*state.State, string, *sql.DB)) {
	t.Helper()
	ctx := context.Background()

	db, connStr, _ := setupTestDatabase(t)

	st, err := state.New(ctx, connStr, "pgroll")
	if err != nil {
		t.Fatal(err)
	}

	fn(st, connStr, db)
}

func WithMigratorInSchemaAndConnectionToContainerWithOptions(t testing.TB, schema string, opts []roll.Option, fn func(mig *roll.Roll, db *sql.DB)) {
	t.Helper()
	ctx := context.Background()

	db, connStr, dbName := setupTestDatabase(t)

	st, err := state.New(ctx, connStr, "pgroll")
	if err != nil {
		t.Fatal(err)
	}

	err = st.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}

	mig, err := roll.New(ctx, connStr, schema, st, opts...)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := mig.Close(); err != nil {
			t.Fatalf("Failed to close migrator connection: %v", err)
		}
	})

	_, err = db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", schema))
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.ExecContext(ctx, fmt.Sprintf("GRANT ALL PRIVILEGES ON SCHEMA %s TO pgroll", schema))
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.ExecContext(ctx, fmt.Sprintf("GRANT ALL PRIVILEGES ON DATABASE %s TO pgroll", dbName))
	if err != nil {
		t.Fatal(err)
	}

	fn(mig, db)
}

func WithMigratorAndStateAndConnectionToContainerWithOptions(t *testing.T, opts []roll.Option, fn func(*roll.Roll, *state.State, *sql.DB)) {
	t.Helper()
	ctx := context.Background()

	db, connStr, dbName := setupTestDatabase(t)

	st, err := state.New(ctx, connStr, "pgroll")
	if err != nil {
		t.Fatal(err)
	}

	err = st.Init(ctx)
	if err != nil {
		t.Fatal(err)
	}

	mig, err := roll.New(ctx, connStr, "public", st, opts...)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := mig.Close(); err != nil {
			t.Fatalf("Failed to close migrator connection: %v", err)
		}
	})

	_, err = db.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", "public"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.ExecContext(ctx, fmt.Sprintf("GRANT ALL PRIVILEGES ON SCHEMA %s TO pgroll", "public"))
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.ExecContext(ctx, fmt.Sprintf("GRANT ALL PRIVILEGES ON DATABASE %s TO pgroll", dbName))
	if err != nil {
		t.Fatal(err)
	}

	fn(mig, st, db)
}

func WithMigratorInSchemaAndConnectionToContainer(t *testing.T, schema string, fn func(mig *roll.Roll, db *sql.DB)) {
	WithMigratorInSchemaAndConnectionToContainerWithOptions(t, schema, []roll.Option{roll.WithLockTimeoutMs(500)}, fn)
}

func WithMigratorAndConnectionToContainer(t *testing.T, fn func(mig *roll.Roll, db *sql.DB)) {
	WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public", []roll.Option{roll.WithLockTimeoutMs(500)}, fn)
}

// WithMigratorAndConnStrToContainer is WithMigratorAndConnectionToContainer
// plus the connection string, so a test can open a second Roll against the
// SAME database with different options.
//
// One database, two Rolls — deliberately, and it is not the two-database
// harness. It models the *clone*: a database whose history was written by one
// configuration and is afterwards read by another, which is exactly an ETL host
// restored from an application-database volume and thereafter migrated with a
// --target. For two genuinely independent databases use WithTwoMigrators.
func WithMigratorAndConnStrToContainer(t *testing.T, opts []roll.Option, fn func(mig *roll.Roll, connStr string, db *sql.DB)) {
	t.Helper()
	ctx := context.Background()

	db, connStr, _ := setupTestDatabase(t)

	st, err := state.New(ctx, connStr, "pgroll")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Init(ctx); err != nil {
		t.Fatal(err)
	}

	mig, err := roll.New(ctx, connStr, "public", st, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mig.Close(); err != nil {
			t.Fatalf("Failed to close migrator connection: %v", err)
		}
	})

	fn(mig, connStr, db)
}

// WithTwoMigrators runs fn against two genuinely separate databases, each with
// its own pgroll state, so a test can prove that applying one target's
// migrations to one of them leaves the other untouched.
func WithTwoMigrators(t *testing.T, aOpts, bOpts []roll.Option, fn func(a, b *roll.Roll, aDB, bDB *sql.DB)) {
	t.Helper()
	ctx := context.Background()

	open := func(opts []roll.Option) (*roll.Roll, *sql.DB) {
		db, connStr, _ := setupTestDatabase(t)

		st, err := state.New(ctx, connStr, "pgroll")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Init(ctx); err != nil {
			t.Fatal(err)
		}

		mig, err := roll.New(ctx, connStr, "public", st, opts...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := mig.Close(); err != nil {
				t.Fatalf("Failed to close migrator connection: %v", err)
			}
		})
		return mig, db
	}

	a, aDB := open(aOpts)
	b, bDB := open(bOpts)
	fn(a, b, aDB, bDB)
}

// NewMigratorForConnStr opens an additional Roll against an already-initialized
// database. Pair it with WithMigratorAndConnStrToContainer.
func NewMigratorForConnStr(t *testing.T, connStr string, opts ...roll.Option) *roll.Roll {
	t.Helper()
	ctx := context.Background()

	st, err := state.New(ctx, connStr, "pgroll")
	if err != nil {
		t.Fatal(err)
	}

	mig, err := roll.New(ctx, connStr, "public", st, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := mig.Close(); err != nil {
			t.Fatalf("Failed to close migrator connection: %v", err)
		}
	})

	return mig
}

// setupTestDatabase creates a new database in the test container and returns:
// - a connection to the new database
// - the connection string to the new database
// - the name of the new database
func setupTestDatabase(t testing.TB) (*sql.DB, string, string) {
	t.Helper()
	ctx := context.Background()

	tDB, err := sql.Open("postgres", tConnStr)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := tDB.Close(); err != nil {
			t.Fatalf("Failed to close database connection: %v", err)
		}
	})

	dbName := randomDBName()

	_, err = tDB.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", pq.QuoteIdentifier(dbName)))
	if err != nil {
		t.Fatal(err)
	}

	u, err := url.Parse(tConnStr)
	if err != nil {
		t.Fatal(err)
	}

	u.Path = "/" + dbName
	connStr := u.String()

	db, err := sql.Open("postgres", connStr)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Fatalf("Failed to close database connection: %v", err)
		}
	})

	return db, connStr, dbName
}
