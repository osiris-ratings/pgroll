// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
	"github.com/xataio/pgroll/pkg/state"
)

const (
	cSchema = "public"
)

func TestMain(m *testing.M) {
	testutils.SharedTestMain(m)
}

func TestSchemaIsCreatedAfterMigrationStart(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		version := "1_create_table"

		if err := mig.Start(ctx, &migrations.Migration{Name: version, Operations: migrations.Operations{createTableOp("table1")}}, backfill.NewConfig()); err != nil {
			t.Fatalf("Failed to start migration: %v", err)
		}

		//
		// Check that the schema exists
		//
		if !schemaExists(t, db, roll.VersionedSchemaName(cSchema, version)) {
			t.Errorf("Expected schema %q to exist", version)
		}
	})
}

func TestWithUseVersionSchemaOption(t *testing.T) {
	t.Parallel()

	opts := []roll.Option{roll.WithVersionSchema(false)}

	t.Run("can start, rollback and complete a migration without using version schema", func(t *testing.T) {
		testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public", opts, func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			version := "1_create_table"

			m := &migrations.Migration{Name: version, Operations: migrations.Operations{createTableOp("table1")}}

			// Start the migration
			err := mig.Start(ctx, m, backfill.NewConfig())
			require.NoError(t, err)

			// Check that the version schema doesn't get created
			exists := schemaExists(t, db, roll.VersionedSchemaName(cSchema, version))
			require.False(t, exists)

			// Rollback the migration
			err = mig.Rollback(ctx)
			require.NoError(t, err)

			// Restart the migration
			err = mig.Start(ctx, m, backfill.NewConfig())
			require.NoError(t, err)

			// complete the migration
			err = mig.Complete(ctx)
			require.NoError(t, err)

			// Check that no version schema exists for the migration
			exists = schemaExists(t, db, roll.VersionedSchemaName(cSchema, version))
			require.False(t, exists)
		})
	})

	t.Run("roll instance reports that it does not use version schema", func(t *testing.T) {
		testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public", opts, func(mig *roll.Roll, db *sql.DB) {
			// The roll instance correctly reports tht it does not use version schema
			require.False(t, mig.UseVersionSchema())
		})
	})

	t.Run("roll instance reports that it does use version schema", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
			// The roll instance correctly reports that it uses version schema
			require.True(t, mig.UseVersionSchema())
		})
	})
}

func TestPreviousVersionIsDroppedAfterMigrationCompletion(t *testing.T) {
	t.Parallel()

	t.Run("when the previous version is a pgroll migration", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			const (
				firstVersion  = "1_create_table"
				secondVersion = "2_create_table"
			)

			if err := mig.Start(ctx, &migrations.Migration{Name: firstVersion, Operations: migrations.Operations{createTableOp("table1")}}, backfill.NewConfig()); err != nil {
				t.Fatalf("Failed to start first migration: %v", err)
			}
			if err := mig.Complete(ctx); err != nil {
				t.Fatalf("Failed to complete first migration: %v", err)
			}
			if err := mig.Start(ctx, &migrations.Migration{Name: secondVersion, Operations: migrations.Operations{createTableOp("table2")}}, backfill.NewConfig()); err != nil {
				t.Fatalf("Failed to start second migration: %v", err)
			}
			if err := mig.Complete(ctx); err != nil {
				t.Fatalf("Failed to complete second migration: %v", err)
			}

			//
			// Check that the schema for the first version has been dropped
			//
			if schemaExists(t, db, roll.VersionedSchemaName(cSchema, firstVersion)) {
				t.Errorf("Expected schema %q to not exist", firstVersion)
			}
		})
	})

	t.Run("when the previous version is an inferred DDL migration", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			const (
				firstVersion  = "1_create_table"
				secondVersion = "2_create_table"
			)

			// Run the first pgroll migration
			if err := mig.Start(ctx, &migrations.Migration{Name: firstVersion, Operations: migrations.Operations{createTableOp("table1")}}, backfill.NewConfig()); err != nil {
				t.Fatalf("Failed to start first migration: %v", err)
			}
			if err := mig.Complete(ctx); err != nil {
				t.Fatalf("Failed to complete first migration: %v", err)
			}

			// Run a manual DDL migration
			_, err := db.ExecContext(ctx, "CREATE TABLE foo (id integer)")
			if err != nil {
				t.Fatalf("Failed to create table: %v", err)
			}

			// Run the second pgroll migration
			if err := mig.Start(ctx, &migrations.Migration{Name: secondVersion, Operations: migrations.Operations{createTableOp("table2")}}, backfill.NewConfig()); err != nil {
				t.Fatalf("Failed to start second migration: %v", err)
			}
			if err := mig.Complete(ctx); err != nil {
				t.Fatalf("Failed to complete second migration: %v", err)
			}

			//
			// Check that the schema for the first version has been dropped
			//
			if schemaExists(t, db, roll.VersionedSchemaName(cSchema, firstVersion)) {
				t.Errorf("Expected schema %q to not exist", firstVersion)
			}
		})
	})

	t.Run("when the previous version sets a non-default version schema name", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
			ctx := context.Background()

			migs := []migrations.Migration{
				{
					Name:          "01_create_table",
					VersionSchema: "01_foo",
					Operations: migrations.Operations{
						&migrations.OpCreateTable{
							Name: "users",
							Columns: []migrations.Column{
								{Name: "id", Type: "serial", Pk: true},
							},
						},
					},
				},
				{
					Name: "02_create_another_table",
					Operations: migrations.Operations{
						&migrations.OpCreateTable{
							Name: "items",
							Columns: []migrations.Column{
								{Name: "id", Type: "serial", Pk: true},
							},
						},
					},
				},
			}

			// Start and complete both migrations
			for _, mig := range migs {
				err := m.Start(ctx, &mig, backfill.NewConfig())
				require.NoError(t, err)
				err = m.Complete(ctx)
				require.NoError(t, err)
			}

			// Ensure that the schema for the first migration has been dropped
			require.False(t, schemaExists(t, db, roll.VersionedSchemaName("public", "01_foo")))
		})
	})
}

func TestSchemaIsDroppedAfterMigrationRollback(t *testing.T) {
	t.Parallel()

	t.Run("when the migration does not set an explicit version schema name ", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			version := "1_create_table"

			if err := mig.Start(ctx, &migrations.Migration{
				Name:       version,
				Operations: migrations.Operations{createTableOp("table1")},
			}, backfill.NewConfig()); err != nil {
				t.Fatalf("Failed to start migration: %v", err)
			}
			if err := mig.Rollback(ctx); err != nil {
				t.Fatalf("Failed to rollback migration: %v", err)
			}

			// Check that the schema has been dropped
			if schemaExists(t, db, roll.VersionedSchemaName(cSchema, version)) {
				t.Errorf("Expected schema %q to not exist", version)
			}
		})
	})

	t.Run("when the migration does set an explicit version schema name ", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()

			if err := mig.Start(ctx, &migrations.Migration{
				Name:          "1_create_table",
				VersionSchema: "1_foo",
				Operations:    migrations.Operations{createTableOp("table1")},
			}, backfill.NewConfig()); err != nil {
				t.Fatalf("Failed to start migration: %v", err)
			}
			if err := mig.Rollback(ctx); err != nil {
				t.Fatalf("Failed to rollback migration: %v", err)
			}

			// Check that the schema has been dropped
			if schemaExists(t, db, roll.VersionedSchemaName(cSchema, "1_foo")) {
				t.Errorf("Expected schema %q to not exist", "1_foo")
			}
		})
	})
}

func TestRollbackOnMigrationStartFailure(t *testing.T) {
	t.Parallel()

	t.Run("when the DDL phase fails", func(t *testing.T) {
		t.Parallel()

		testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()

			// start a migration that will fail during the DDL phase
			err := mig.Start(ctx, &migrations.Migration{
				Name: "01_create_table",
				Operations: migrations.Operations{
					&migrations.OpCreateTable{
						Name: "table1",
						Columns: []migrations.Column{
							{
								Name: "id",
								Type: "invalid",
							},
						},
					},
				},
			}, backfill.NewConfig())
			assert.Error(t, err)

			// ensure that there is no active migration
			status, err := mig.Status(ctx, "public")
			assert.NoError(t, err)
			assert.Equal(t, roll.NoneMigrationStatus, status.Status)
		})
	})

	t.Run("when the backfill phase fails", func(t *testing.T) {
		t.Parallel()

		testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()

			// run an initial migration to create the table
			err := mig.Start(ctx, &migrations.Migration{
				Name:       "01_create_table",
				Operations: migrations.Operations{createTableOp("table1")},
			}, backfill.NewConfig())
			assert.NoError(t, err)

			// complete the migration
			err = mig.Complete(ctx)
			assert.NoError(t, err)

			// insert some data into the table
			_, err = db.ExecContext(ctx, "INSERT INTO table1 (id, name) VALUES (1, 'alice'), (2, 'bob')")
			assert.NoError(t, err)

			// Start a migration that will fail during the backfill phase
			// Change the type of the `name` column but provide invalid up and down SQL
			err = mig.Start(ctx, &migrations.Migration{
				Name: "02_add_column",
				Operations: migrations.Operations{
					&migrations.OpAlterColumn{
						Table:  "table1",
						Column: "name",
						Type:   ptr("text"),
						Up:     "invalid",
						Down:   "invalid",
					},
				},
			}, backfill.NewConfig())
			assert.Error(t, err)

			// Ensure that there is no active migration
			status, err := mig.Status(ctx, "public")
			assert.NoError(t, err)
			assert.Equal(t, "01_create_table", status.Version)
			assert.Equal(t, roll.CompleteMigrationStatus, status.Status)
		})
	})
}

func TestSchemaOptionIsRespected(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorInSchemaAndConnectionToContainer(t, "schema1", func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		const version1 = "1_create_table"
		const version2 = "2_create_another_table"

		if err := mig.Start(
			ctx,
			&migrations.Migration{
				Name:       version1,
				Operations: migrations.Operations{createTableOp("table1")},
			},
			backfill.NewConfig(),
		); err != nil {
			t.Fatalf("Failed to start migration: %v", err)
		}
		if err := mig.Complete(ctx); err != nil {
			t.Fatalf("Failed to complete migration: %v", err)
		}

		//
		// Check that the table exists in the correct schema
		//
		var exists bool
		err := db.QueryRow(`
    SELECT EXISTS(
      SELECT 1
      FROM pg_catalog.pg_tables
      WHERE tablename = $1
      AND schemaname = $2
    )`, "table1", "schema1").Scan(&exists)
		if err != nil {
			t.Fatal(err)
		}

		if !exists {
			t.Errorf("Expected table %q to exist in schema %q", "table1", "schema1")
		}

		// Apply another migration to the same schema
		if err := mig.Start(
			ctx, &migrations.Migration{
				Name:       version2,
				Operations: migrations.Operations{createTableOp("table2")},
			},
			backfill.NewConfig(),
		); err != nil {
			t.Fatalf("Failed to start migration: %v", err)
		}
		if err := mig.Complete(ctx); err != nil {
			t.Fatalf("Failed to complete migration: %v", err)
		}

		// Ensure that the versioned schema for the first migration has been dropped
		if schemaExists(t, db, roll.VersionedSchemaName("schema1", version1)) {
			t.Errorf("Expected schema %q to not exist", version1)
		}
	})
}

func TestMigrationDDLIsRetriedOnLockTimeouts(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public", []roll.Option{roll.WithLockTimeoutMs(50)}, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// Create a table
		_, err := db.ExecContext(ctx, "CREATE TABLE table1 (id integer, name text)")
		require.NoError(t, err)

		// Start a goroutine which takes an ACCESS_EXCLUSIVE lock on the table for
		// two seconds
		errCh := make(chan error)
		go func() {
			tx, err := db.Begin()
			if err != nil {
				errCh <- err
			}

			if _, err := tx.ExecContext(ctx, "LOCK TABLE table1 IN ACCESS EXCLUSIVE MODE"); err != nil {
				errCh <- err
			}
			errCh <- nil

			// Sleep for two seconds to hold the lock
			time.Sleep(2 * time.Second)

			// Commit the transaction
			tx.Commit()
		}()

		// Wait for lock to be taken
		err = <-errCh
		require.NoError(t, err)

		// Attempt to start a second migration on the table while the lock is held.
		// The migration should eventually succeed after the lock is released
		err = mig.Start(ctx, &migrations.Migration{
			Name:       "01_add_column",
			Operations: migrations.Operations{addColumnOp("table1")},
		}, backfill.NewConfig())
		require.NoError(t, err)
	})
}

// TestCompleteRetriesViewProjectionOnLockTimeout exercises the fix for the
// retry-into-aborted-tx bug in ensureView. The Complete-phase view
// re-projection used to send `BEGIN; DROP VIEW; CREATE VIEW; ALTER VIEW
// SET DEFAULT…; COMMIT` as a single string via ExecContext. When
// lock_timeout (55P03) fired on the DROP, the implicit transaction was
// aborted and the pooled connection was left in "transaction aborted"
// state; the retry re-sent the same string, the leading BEGIN became a
// notice, the next statement returned 25P02, and the retry loop saw a
// non-55P03 error and bailed after one attempt. With the fix (using
// WithRetryableTransaction so the transaction is owned by `*sql.Tx`),
// each retry opens a fresh transaction and the configured budget
// actually runs to completion.
func TestCompleteRetriesViewProjectionOnLockTimeout(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
		[]roll.Option{roll.WithLockTimeoutMs(50)},
		func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			version := "01_create_table"

			// Start a migration so the version schema and its view exist.
			if err := mig.Start(ctx,
				&migrations.Migration{Name: version, Operations: migrations.Operations{createTableOp("table1")}},
				backfill.NewConfig()); err != nil {
				t.Fatalf("start: %v", err)
			}

			// Hold an AccessShare lock on the versioned view for two seconds —
			// long enough to force at least one lock_timeout (50ms) inside the
			// Complete-phase view re-projection.
			errCh := make(chan error, 1)
			go func() {
				tx, err := db.Begin()
				if err != nil {
					errCh <- err
					return
				}
				defer tx.Commit()
				if _, err := tx.ExecContext(ctx,
					fmt.Sprintf("LOCK TABLE %s.table1 IN ACCESS SHARE MODE",
						roll.VersionedSchemaName(cSchema, version))); err != nil {
					errCh <- err
					return
				}
				errCh <- nil
				time.Sleep(2 * time.Second)
			}()
			require.NoError(t, <-errCh)

			// Complete must eventually succeed once the reader releases.
			// Pre-fix this returned `25P02` after one retry; post-fix the
			// retry budget produces fresh transactions until DROP VIEW gets
			// AccessExclusive.
			require.NoError(t, mig.Complete(ctx))
		})
}

func TestViewsAreCreatedWithSecurityInvokerTrue(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		version := "1_create_table"

		if mig.PGVersion() < roll.PGVersion15 {
			t.Skip("Skipping test for postgres < 15 as `security_invoker` views are not supported")
		}

		// Start and complete a migration to create a simple `users` table
		if err := mig.Start(ctx, &migrations.Migration{Name: version, Operations: migrations.Operations{createTableOp("users")}}, backfill.NewConfig()); err != nil {
			t.Fatalf("Failed to start migration: %v", err)
		}
		if err := mig.Complete(ctx); err != nil {
			t.Fatalf("Failed to complete migration: %v", err)
		}

		// Insert two rows into the underlying table
		_, err := db.ExecContext(ctx, "INSERT INTO users (id, name) VALUES (1, 'alice'), (2, 'bob')")
		if err != nil {
			t.Fatalf("Failed to insert rows into table: %v", err)
		}

		// Enable row level security on the underlying table
		_, err = db.ExecContext(ctx, "ALTER TABLE users ENABLE ROW LEVEL SECURITY")
		if err != nil {
			t.Fatalf("Failed to enable row level security: %v", err)
		}

		// Add a security policy to the underlying table
		_, err = db.ExecContext(ctx, "CREATE POLICY user_policy ON users USING (name = current_user)")
		if err != nil {
			t.Fatalf("Failed to create security policy: %v", err)
		}

		// Create user 'alice'
		_, err = db.ExecContext(ctx, "CREATE USER alice")
		if err != nil {
			t.Fatalf("Failed to create user: %v", err)
		}

		// Grant access to the underlying table to user 'alice'
		_, err = db.ExecContext(ctx, "GRANT SELECT ON users TO alice")
		if err != nil {
			t.Fatalf("Failed to grant access to user: %v", err)
		}

		// Grant access to the versioned schema to user 'alice'
		_, err = db.ExecContext(ctx, "GRANT USAGE ON SCHEMA public_1_create_table TO alice")
		if err != nil {
			t.Fatalf("Failed to grant usage on schema to user: %v", err)
		}

		// Grant access to the versioned view to user 'alice'
		_, err = db.ExecContext(ctx, "GRANT SELECT ON public_1_create_table.users TO alice")
		if err != nil {
			t.Fatalf("Failed to grant select on view to user: %v", err)
		}

		// Ensure that the superuser can see all rows
		rows := MustSelect(t, db, "public", "1_create_table", "users")
		assert.Equal(t, []map[string]any{
			{"id": 1, "name": "alice"},
			{"id": 2, "name": "bob"},
		}, rows)

		// Switch roles to 'alice'
		_, err = db.ExecContext(ctx, "SET ROLE alice")
		if err != nil {
			t.Fatalf("Failed to switch roles: %v", err)
		}

		// Ensure that 'alice' can only see her own row
		rows = MustSelect(t, db, "public", "1_create_table", "users")
		assert.Equal(t, []map[string]any{
			{"id": 1, "name": "alice"},
		}, rows)
	})
}

func TestStatusMethodReturnsCorrectStatus(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// Get the initial migration status before any migrations are run
		status, err := mig.Status(ctx, "public")
		assert.NoError(t, err)

		// Ensure that the status shows "No migrations"
		assert.Equal(t, &roll.Status{
			Schema:  "public",
			Version: "",
			Status:  roll.NoneMigrationStatus,
		}, status)

		// Start a migration
		err = mig.Start(ctx, &migrations.Migration{
			Name:       "01_create_table",
			Operations: []migrations.Operation{createTableOp("table1")},
		}, backfill.NewConfig())
		assert.NoError(t, err)

		// Get the migration status
		status, err = mig.Status(ctx, "public")
		assert.NoError(t, err)

		// Ensure that the status shows "In progress"
		assert.Equal(t, &roll.Status{
			Schema:  "public",
			Version: "01_create_table",
			Status:  roll.InProgressMigrationStatus,
		}, status)

		// Rollback the migration
		err = mig.Rollback(ctx)
		assert.NoError(t, err)

		// Get the migration status
		status, err = mig.Status(ctx, "public")
		assert.NoError(t, err)

		// Ensure that the status shows "No migrations"
		assert.Equal(t, &roll.Status{
			Schema:  "public",
			Version: "",
			Status:  roll.NoneMigrationStatus,
		}, status)

		// Start and complete a migration
		err = mig.Start(ctx, &migrations.Migration{
			Name:       "01_create_table",
			Operations: []migrations.Operation{createTableOp("table1")},
		}, backfill.NewConfig())
		assert.NoError(t, err)
		err = mig.Complete(ctx)
		assert.NoError(t, err)

		// Get the migration status
		status, err = mig.Status(ctx, "public")
		assert.NoError(t, err)

		// Ensure that the status shows "Complete"
		assert.Equal(t, &roll.Status{
			Schema:  "public",
			Version: "01_create_table",
			Status:  roll.CompleteMigrationStatus,
		}, status)
	})
}

func TestRoleIsRespected(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public", []roll.Option{roll.WithRole("pgroll")}, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// Start a create table migration
		err := mig.Start(ctx, &migrations.Migration{
			Name:       "01_create_table",
			Operations: migrations.Operations{createTableOp("table1")},
		}, backfill.NewConfig())
		assert.NoError(t, err)

		// Complete the create table migration
		err = mig.Complete(ctx)
		assert.NoError(t, err)

		// Ensure that the table exists in the correct schema and owned by the correct role
		var exists bool
		err = db.QueryRow(`
			SELECT EXISTS(
				SELECT 1
				FROM pg_catalog.pg_tables
				WHERE tablename = $1
					AND schemaname = $2
					AND tableowner = $3
		)`, "table1", "public", "pgroll").Scan(&exists)
		assert.NoError(t, err)
		assert.True(t, exists)
	})
}

func TestMigrationHooksAreInvoked(t *testing.T) {
	t.Parallel()

	options := []roll.Option{roll.WithMigrationHooks(roll.MigrationHooks{
		BeforeStartDDL: func(m *roll.Roll) error {
			_, err := m.PgConn().ExecContext(context.Background(), "CREATE TABLE before_start_ddl (id integer)")
			return err
		},
		AfterStartDDL: func(m *roll.Roll) error {
			_, err := m.PgConn().ExecContext(context.Background(), "CREATE TABLE after_start_ddl (id integer)")
			return err
		},
		BeforeCompleteDDL: func(m *roll.Roll) error {
			_, err := m.PgConn().ExecContext(context.Background(), "CREATE TABLE before_complete_ddl (id integer)")
			return err
		},
		AfterCompleteDDL: func(m *roll.Roll) error {
			_, err := m.PgConn().ExecContext(context.Background(), "CREATE TABLE after_complete_ddl (id integer)")
			return err
		},
	})}

	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public", options, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// Start a create table migration
		err := mig.Start(ctx, &migrations.Migration{
			Name:       "01_create_table",
			Operations: migrations.Operations{createTableOp("table1")},
		}, backfill.NewConfig())
		assert.NoError(t, err)

		// Ensure that both the before_start_ddl and after_start_ddl tables were created
		assert.True(t, tableExists(t, db, "public", "before_start_ddl"))
		assert.True(t, tableExists(t, db, "public", "after_start_ddl"))

		// Complete the migration
		err = mig.Complete(ctx)
		assert.NoError(t, err)

		// Ensure that both the before_complete_ddl and after_complete_ddl tables were created
		assert.True(t, tableExists(t, db, "public", "before_complete_ddl"))
		assert.True(t, tableExists(t, db, "public", "after_complete_ddl"))
	})
}

func TestCallbacksAreInvokedOnMigrationStart(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// Create a table
		_, err := db.ExecContext(ctx, "CREATE TABLE users (id SERIAL PRIMARY KEY, name text)")
		require.NoError(t, err)

		// Insert some data
		_, err = db.ExecContext(ctx,
			"INSERT INTO users (id, name) VALUES (1, 'alice'), (2, 'bob')")
		require.NoError(t, err)

		// Define a mock callback
		invoked := false
		cb := func(n, total int64) { invoked = true }

		backfillConfig := backfill.NewConfig()
		backfillConfig.AddCallback(cb)

		// Start a migration that requires a backfill
		err = mig.Start(ctx, &migrations.Migration{
			Name: "02_change_type",
			Operations: migrations.Operations{
				&migrations.OpAlterColumn{
					Table:  "users",
					Column: "name",
					Type:   ptr("varchar(255)"),
					Up:     "name",
					Down:   "name",
				},
			},
		}, backfillConfig)
		require.NoError(t, err)

		// Ensure that the callback was invoked
		assert.True(t, invoked)
	})
}

func TestRollSchemaMethodReturnsCorrectSchema(t *testing.T) {
	t.Parallel()

	t.Run("when the schema is public", func(t *testing.T) {
		testutils.WithMigratorInSchemaAndConnectionToContainer(t, "public", func(mig *roll.Roll, _ *sql.DB) {
			assert.Equal(t, "public", mig.Schema())
		})
	})

	t.Run("when the schema is non-public", func(t *testing.T) {
		testutils.WithMigratorInSchemaAndConnectionToContainer(t, "apples", func(mig *roll.Roll, _ *sql.DB) {
			assert.Equal(t, "apples", mig.Schema())
		})
	})
}

func TestLatestVersionAndLatestMigrationMethodsRespectVersionSchemaAndName(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(r *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// Create a migration with an explicit version schema
		mig := &migrations.Migration{
			Name:          "01_create_table",
			VersionSchema: "01_foo",
			Operations:    migrations.Operations{createTableOp("table1")},
		}

		// Start and complete a migration
		err := r.Start(ctx, mig, backfill.NewConfig())
		require.NoError(t, err)
		err = r.Complete(ctx)
		require.NoError(t, err)

		// Get the latest version
		latestVersion, err := r.State().LatestVersion(ctx, "public")
		require.NoError(t, err)

		// Get the latest migration name
		latestMigration, err := r.State().LatestMigration(ctx, "public")
		require.NoError(t, err)

		// Assert that the latest version is correct
		require.NotNil(t, latestVersion)
		require.Equal(t, "01_foo", *latestVersion)

		// Assert that the latest migration name is correct
		require.NotNil(t, latestMigration)
		require.Equal(t, "01_create_table", *latestMigration)
	})
}

func TestWithSearchPathOptionIsRespected(t *testing.T) {
	t.Parallel()

	opts := []roll.Option{roll.WithSearchPath("public")}

	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "foo", opts, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// Create a function in the public schema
		_, err := db.ExecContext(ctx, `CREATE OR REPLACE FUNCTION say_hello()
      RETURNS TEXT AS $$
        SELECT 'hello world';
      $$ LANGUAGE sql;
    `)
		require.NoError(t, err)

		// Apply a migration in the foo schema that references the function in the public schema
		err = mig.Start(ctx, &migrations.Migration{
			Name: "01_raw_sql",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up: "SELECT say_hello()",
				},
			},
		}, backfill.NewConfig())
		require.NoError(t, err)

		// Complete the migration
		err = mig.Complete(ctx)
		require.NoError(t, err)

		// No assertions required as the migration would have failed if the
		// function reference was not found
	})
}

func createTableOp(tableName string) *migrations.OpCreateTable {
	return &migrations.OpCreateTable{
		Name: tableName,
		Columns: []migrations.Column{
			{
				Name: "id",
				Type: "integer",
				Pk:   true,
			},
			{
				Name:   "name",
				Type:   "varchar(255)",
				Unique: true,
			},
		},
	}
}

// pgroll uses two Postgres connections:
// - one for the migrator (used for DDL operations on the target schema)
// - one for the state (used to update pgroll's internal state)
// Both connections should have their application_name set to a specific value for easy identification in pg_stat_activity.
func TestConnectionsSetPostgresApplicationName(t *testing.T) {
	t.Parallel()

	// Define an interface common to
	// - *sql.DB (used by the state connection)
	// - db.DB (used by the migrator connection)
	type Execer interface {
		ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	}

	testCases := []struct {
		name            string
		connFn          func(mig *roll.Roll) Execer
		query           string
		expectedAppName string
	}{
		{
			name: "migrator sets application name correctly",
			connFn: func(mig *roll.Roll) Execer {
				return mig.PgConn()
			},
			query:           "SELECT pg_sleep(2) -- migrator connection",
			expectedAppName: "pgroll",
		},
		{
			name: "state sets application name correctly",
			connFn: func(mig *roll.Roll) Execer {
				return mig.State().PgConn()
			},
			query:           "SELECT pg_sleep(2) -- state connection",
			expectedAppName: "pgroll-state",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			testutils.WithMigratorAndConnectionToContainer(t, func(roll *roll.Roll, db *sql.DB) {
				ctx := context.Background()

				// Get the connection under test; either the migrator or the state connection
				conn := tc.connFn(roll)

				// Start a long running query on the connection under test
				// Use a buffered channel to ensure the goroutine can always signal completion
				errCh := make(chan error, 1)
				go func() {
					_, err := conn.ExecContext(ctx, tc.query)
					errCh <- err
				}()

				// Wait for the query, which must have the expected application_name, to appear in pg_stat_activity
				require.Eventually(t, func() bool {
					// Fail the test if the query under test has failed for any reason
					select {
					case err := <-errCh:
						require.NoError(t, err, "query %q failed: %v", tc.query, err)
					default:
					}

					// Query pg_stat_activity for queries with the expected application_name
					rows, err := db.QueryContext(ctx,
						"SELECT query FROM pg_stat_activity WHERE application_name = $1", tc.expectedAppName)
					require.NoError(t, err)

					defer rows.Close()

					// Check if the query executed by this testcase is present in the result set
					for rows.Next() {
						var query string
						require.NoError(t, rows.Scan(&query))
						if query == tc.query {
							return true
						}
					}
					require.NoError(t, rows.Err())
					return false
				}, 3*time.Second, 100*time.Millisecond,
					"expected query %q with application_name %q to be found in pg_stat_activity", tc.query, tc.expectedAppName)
			})
		})
	}
}

func TestStartFailsWithExistingSchemaWithoutHistory(t *testing.T) {
	t.Parallel()

	testutils.WithUninitializedStateAndConnectionInfo(t, func(st *state.State, connStr string, db *sql.DB) {
		ctx := context.Background()

		// Create a table to before initializing `pgroll`
		_, err := db.ExecContext(ctx, "CREATE TABLE existing_table (id int)")
		require.NoError(t, err)

		// Initialize `pgroll`
		err = st.Init(ctx)
		require.NoError(t, err)

		// Create a Roll instance
		m, err := roll.New(ctx, connStr, "public", st)
		require.NoError(t, err)

		// Attempt to start a migration
		err = m.Start(ctx, &migrations.Migration{
			Name:       "01_create_table",
			Operations: migrations.Operations{createTableOp("new_table")},
		}, backfill.NewConfig())

		// Verify that the error is ErrExistingSchemaWithoutHistory
		assert.ErrorIs(t, err, roll.ErrExistingSchemaWithoutHistory)
	})
}

func TestStartRejectsMigrationsWithOverlongVersionSchemaName(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// "public_" is 7 bytes; pick a name that pushes the combined length
		// past the 63-byte Postgres identifier limit.
		longName := strings.Repeat("x", migrations.MaxIdentifierLength-len(cSchema))
		require.Greater(t, len(cSchema)+1+len(longName), migrations.MaxIdentifierLength)

		err := m.Start(ctx, &migrations.Migration{
			Name:       longName,
			Operations: migrations.Operations{createTableOp("new_table")},
		}, backfill.NewConfig())

		var tooLong migrations.VersionSchemaNameTooLongError
		require.ErrorAs(t, err, &tooLong)
		assert.Equal(t, cSchema, tooLong.Schema)
		assert.Equal(t, longName, tooLong.VersionName)
		assert.Equal(t, migrations.MaxIdentifierLength, tooLong.Max)

		// The truncated schema must not have been created.
		truncated := roll.VersionedSchemaName(cSchema, longName)[:migrations.MaxIdentifierLength]
		assert.False(t, schemaExists(t, db, truncated),
			"expected truncated schema %q not to have been created", truncated)

		// And the migration must not have been recorded in pgroll state.
		var count int
		require.NoError(t, db.QueryRowContext(ctx,
			"SELECT count(*) FROM pgroll.migrations WHERE schema = $1 AND name = $2",
			cSchema, longName).Scan(&count))
		assert.Equal(t, 0, count, "migration row should not have been inserted")
	})
}

func TestStartAcceptsMigrationAtVersionSchemaNameLimit(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// Construct a name that hits the 63-byte limit exactly:
		// len("public") + 1 ("_") + len(name) == 63.
		name := strings.Repeat("x", migrations.MaxIdentifierLength-len(cSchema)-1)
		require.Equal(t, migrations.MaxIdentifierLength, len(cSchema)+1+len(name))

		err := m.Start(ctx, &migrations.Migration{
			Name:       name,
			Operations: migrations.Operations{createTableOp("new_table")},
		}, backfill.NewConfig())
		require.NoError(t, err)

		// And the version schema actually exists at the full computed name.
		full := roll.VersionedSchemaName(cSchema, name)
		assert.True(t, schemaExists(t, db, full))
	})
}

func TestVersionSchemaCreationIsNotCapturedAsAnInferredMigration(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// Apply a migration
		err := m.Start(ctx, &migrations.Migration{
			Name:       "01_create_table",
			Operations: migrations.Operations{createTableOp("new_table")},
		}, backfill.NewConfig())
		require.NoError(t, err)
		err = m.Complete(ctx)
		require.NoError(t, err)

		// Ensure that the version schema has been created
		versionSchema := roll.VersionedSchemaName("public", "01_create_table")
		require.True(t, schemaExists(t, db, versionSchema))

		// Get the schema history **for the version schema**
		hist, err := m.State().SchemaHistory(ctx, versionSchema)
		require.NoError(t, err)

		// Ensure that there are no inferred migrations recorded for the version
		// schema; the DDL statements that `pgroll` executes to create the version
		// schema and the views inside it should be ignored by the event trigger
		// that captures inferred migrations.
		require.Len(t, hist, 0)
	})
}

func TestCompleteWithSkipSchemaDropPreservesPreviousVersionSchema(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		const (
			version1 = "1_create_table"
			version2 = "2_create_another_table"
		)

		// Start and complete the first migration
		err := mig.Start(ctx, &migrations.Migration{
			Name:       version1,
			Operations: migrations.Operations{createTableOp("table1")},
		}, backfill.NewConfig())
		require.NoError(t, err)
		err = mig.Complete(ctx)
		require.NoError(t, err)

		// Start and complete the second migration with WithSkipSchemaDrop
		err = mig.Start(ctx, &migrations.Migration{
			Name:       version2,
			Operations: migrations.Operations{createTableOp("table2")},
		}, backfill.NewConfig())
		require.NoError(t, err)
		err = mig.Complete(ctx, roll.WithSkipSchemaDrop())
		require.NoError(t, err)

		// The first version schema should still exist because we skipped the drop
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, version1)),
			"Expected schema %q to still exist after Complete with WithSkipSchemaDrop", version1)

		// The second version schema should also exist
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, version2)),
			"Expected schema %q to exist", version2)
	})
}

func TestMultiMigrationBatchPreservesOriginalSchema(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		const (
			version1 = "1_create_table"
			version2 = "2_create_table2"
			version3 = "3_create_table3"
			version4 = "4_create_table4"
		)

		// Apply the first migration normally (this is the "app's current version").
		err := mig.Start(ctx, &migrations.Migration{
			Name:       version1,
			Operations: migrations.Operations{createTableOp("table1")},
		}, backfill.NewConfig())
		require.NoError(t, err)
		err = mig.Complete(ctx)
		require.NoError(t, err)

		// Verify the original schema exists.
		require.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, version1)))

		// Simulate a multi-migration batch: apply intermediates with both
		// WithoutVersionSchema (Start) and WithSkipSchemaDrop (Complete).
		// This is what `pgroll migrate` now does in its loop.
		for _, v := range []struct {
			name  string
			table string
		}{
			{version2, "table2"},
			{version3, "table3"},
		} {
			err = mig.Start(ctx, &migrations.Migration{
				Name:       v.name,
				Operations: migrations.Operations{createTableOp(v.table)},
			}, backfill.NewConfig(), roll.WithoutVersionSchema())
			require.NoError(t, err)
			err = mig.Complete(ctx, roll.WithSkipSchemaDrop())
			require.NoError(t, err)

			// Original schema should still exist after each intermediate.
			assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, version1)),
				"Original schema should persist during intermediate migration %s", v.name)

			// Intermediates do not project a version schema — this is the
			// load-bearing assertion of the new model. No orphan schemas
			// can accumulate during the batch.
			assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, v.name)),
				"intermediate %s must not have a version schema", v.name)
		}

		// Apply the final migration normally (Start projects the new target).
		err = mig.Start(ctx, &migrations.Migration{
			Name:       version4,
			Operations: migrations.Operations{createTableOp("table4")},
		}, backfill.NewConfig())
		require.NoError(t, err)

		// At this moment exactly two version schemas exist: the original
		// (V1, apps' active version) and the final target (V4).
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, version1)))
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, version4)))
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, version2)))
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, version3)))

		// Run final Complete (the standalone `pgroll complete` step). This
		// reaps the original V1 schema and leaves only V4 standing.
		require.NoError(t, mig.Complete(ctx))

		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, version1)),
			"original schema reaped by final Complete after deploy")
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, version4)),
			"final target schema remains")
	})
}

func TestDropVersionSchemasExceptKeepsSpecifiedSchemas(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		versions := []string{"v1", "v2", "v3", "v4"}
		tables := []string{"t1", "t2", "t3", "t4"}

		// Create 4 migrations, skipping schema drops for all
		for i, v := range versions {
			err := mig.Start(ctx, &migrations.Migration{
				Name:       v,
				Operations: migrations.Operations{createTableOp(tables[i])},
			}, backfill.NewConfig())
			require.NoError(t, err)
			err = mig.Complete(ctx, roll.WithSkipSchemaDrop())
			require.NoError(t, err)
		}

		// All version schemas should exist
		for _, v := range versions {
			require.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, v)),
				"Expected schema for %s to exist before cleanup", v)
		}

		// Drop all except v1 and v4
		err := mig.DropVersionSchemasExcept(ctx, "v1", "v4")
		require.NoError(t, err)

		// v1 and v4 should still exist
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "v1")))
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "v4")))

		// v2 and v3 should be dropped
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "v2")))
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "v3")))
	})
}

// holdAccessShareOnView holds an AccessShare lock on the given relation until
// the returned release func is called, simulating a live backend still reading
// through a (dead) version schema's view. release blocks until the holding
// transaction has committed and the lock is gone.
func holdAccessShareOnView(t *testing.T, db *sql.DB, qualifiedView string) (release func()) {
	t.Helper()
	ctx := context.Background()
	acquired := make(chan error, 1)
	unblock := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		tx, err := db.Begin()
		if err != nil {
			acquired <- err
			return
		}
		if _, err := tx.ExecContext(ctx,
			fmt.Sprintf("LOCK TABLE %s IN ACCESS SHARE MODE", qualifiedView)); err != nil {
			_ = tx.Rollback()
			acquired <- err
			return
		}
		acquired <- nil
		<-unblock
		_ = tx.Commit()
	}()
	require.NoError(t, <-acquired)
	return func() {
		close(unblock)
		<-done
	}
}

// TestReapVersionSchemasExceptDefersLockedSchema verifies the best-effort reap
// path the seal uses: a dead version schema that a live backend is still
// reading through is carried forward instead of failing the caller, while
// unlocked dead schemas are dropped and kept schemas are untouched. This is the
// fix for a seal/GC drop wedging an entire deployment behind one straggler
// connection (e.g. an idle-in-transaction worker still pinned to an old schema).
func TestReapVersionSchemasExceptDefersLockedSchema(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
		// Short lock_timeout so the blocked DROP fails fast; disabled retry
		// budget (negative) so the 55P03 is surfaced immediately and deferred
		// rather than retried for minutes.
		[]roll.Option{roll.WithLockTimeoutMs(50), roll.WithLockRetryTimeout(-1)},
		func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()

			versions := []string{"v1", "v2", "v3", "v4"}
			tables := []string{"t1", "t2", "t3", "t4"}
			for i, v := range versions {
				require.NoError(t, mig.Start(ctx, &migrations.Migration{
					Name:       v,
					Operations: migrations.Operations{createTableOp(tables[i])},
				}, backfill.NewConfig()))
				require.NoError(t, mig.Complete(ctx, roll.WithSkipSchemaDrop()))
			}

			// A live backend still reads through v2's view.
			release := holdAccessShareOnView(t, db,
				roll.VersionedSchemaName(cSchema, "v2")+".t2")

			// Reap everything except v1 and v4. v2 is locked → deferred (not
			// fatal); v3 is free → dropped.
			deferred, err := mig.ReapVersionSchemasExcept(ctx, "v1", "v4")
			require.NoError(t, err)
			assert.Equal(t, []string{roll.VersionedSchemaName(cSchema, "v2")}, deferred)

			assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "v1")))
			assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "v4")))
			assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "v2")),
				"locked schema must be deferred, not dropped")
			assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "v3")),
				"unlocked dead schema must be dropped")

			// Once the reader releases, a follow-up reap collects the
			// previously-deferred schema — it is retried, not lost.
			release()
			deferred, err = mig.ReapVersionSchemasExcept(ctx, "v1", "v4")
			require.NoError(t, err)
			assert.Empty(t, deferred)
			assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "v2")),
				"previously-deferred schema reaped once the reader released")
		})
}

// TestDropVersionSchemasExceptIsStrictWhenLocked locks in the safety boundary:
// the (non-best-effort) DropVersionSchemasExcept — used by the Complete path
// and, via the live-schema drop, by the seal — still aborts when a drop is
// blocked, rather than silently skipping.
func TestDropVersionSchemasExceptIsStrictWhenLocked(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
		[]roll.Option{roll.WithLockTimeoutMs(50), roll.WithLockRetryTimeout(-1)},
		func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()

			for i, v := range []string{"v1", "v2"} {
				require.NoError(t, mig.Start(ctx, &migrations.Migration{
					Name:       v,
					Operations: migrations.Operations{createTableOp([]string{"t1", "t2"}[i])},
				}, backfill.NewConfig()))
				require.NoError(t, mig.Complete(ctx, roll.WithSkipSchemaDrop()))
			}

			release := holdAccessShareOnView(t, db,
				roll.VersionedSchemaName(cSchema, "v1")+".t1")
			defer release()

			// v1 is locked; the strict path must surface the error.
			err := mig.DropVersionSchemasExcept(ctx, "v2")
			require.Error(t, err)
			assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "v1")),
				"strict drop must leave the locked schema in place after failing")
		})
}

// TestDependentMigrationsResolveWithDeferredCleanup mirrors the production
// pgroll-migrate flow: a stack of additive migrations where each one uses
// objects created by previous migrations. It proves that intermediate
// completion (running operations + state.Complete) and schema cleanup are
// severable for additive operations — every migration's effects are visible
// to the next even though no version schemas are dropped until the final
// Complete.
//
// Migration chain:
//
//	A: CREATE TABLE products (id, name)
//	B: CREATE FUNCTION greet(text) -> text  (depends on nothing in pgroll, just exists in public)
//	C: ALTER TABLE products ADD COLUMN greeting text DEFAULT greet('default')
//	   (depends on A's table + B's function)
func TestDependentMigrationsResolveWithDeferredCleanup(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		const (
			vA = "01_create_products_table"
			vB = "02_create_greet_function"
			vC = "03_add_greeting_column"
		)

		// Migration A: create the products table.
		migA := &migrations.Migration{
			Name: vA,
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE products (id integer PRIMARY KEY, name text NOT NULL)`,
					Down: `DROP TABLE products`,
				},
			},
		}
		require.NoError(t, mig.Start(ctx, migA, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithSkipSchemaDrop()))

		// Intermediate migrations don't project version schemas.
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, vA)),
			"intermediate A should not have a version schema")

		// Migration B: create a function. Depends on B running in a context
		// where A has already physically applied its DDL.
		migB := &migrations.Migration{
			Name: vB,
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up: `CREATE FUNCTION greet(name text) RETURNS text
						   LANGUAGE sql IMMUTABLE
						   AS $$ SELECT 'Hello, ' || name $$`,
					Down: `DROP FUNCTION greet(text)`,
				},
			},
		}
		require.NoError(t, mig.Start(ctx, migB, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithSkipSchemaDrop()))

		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, vB)),
			"intermediate B should not have a version schema")

		// Migration C: add a column whose DEFAULT calls greet() and references
		// the products table. This will fail unless both A and B have been
		// effectively applied (their operations actually executed against the
		// underlying schema), so this is the load-bearing assertion.
		migC := &migrations.Migration{
			Name: vC,
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up: `ALTER TABLE products
						   ADD COLUMN greeting text NOT NULL DEFAULT greet('world')`,
					Down: `ALTER TABLE products DROP COLUMN greeting`,
				},
			},
		}
		// Final migration projects the new target schema.
		require.NoError(t, mig.Start(ctx, migC, backfill.NewConfig()))

		// Final completion (no skip).
		require.NoError(t, mig.Complete(ctx))

		// Underlying objects all exist and the dependency chain resolved.
		var greeting string
		_, err := db.ExecContext(ctx, `INSERT INTO products(id, name) VALUES (1, 'pgroll')`)
		require.NoError(t, err, "insert into products should succeed (table from A exists, default from B was applied during C)")

		err = db.QueryRowContext(ctx, `SELECT greeting FROM products WHERE id = 1`).Scan(&greeting)
		require.NoError(t, err)
		assert.Equal(t, "Hello, world", greeting,
			"greet() default applied to new column proves B's function is callable from C's DDL")

		// Only C's version schema remains.
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, vA)))
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, vB)))
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, vC)),
			"version schema for C should remain after final Complete")
	})
}

// TestDestructiveContractOpFailsAsIntermediate pins down the safety
// *boundary* of the deferred-cleanup model. A migration whose contract
// (OnComplete) operation drops or alters something referenced by views in
// prior version schemas cannot run as an intermediate (with
// WithSkipSchemaDrop) — Postgres rejects the DDL with "other objects depend
// on it" because the prev-version views haven't been reaped.
//
// This is a fundamental property of pgroll's multi-migration batch model,
// not a quirk of the current cleanup design. The same failure occurs under
// any cleanup scheme that keeps WithSkipSchemaDrop's contract: don't drop
// prev-version mid-batch (so apps still connected to it aren't rugged).
// Both the "--protect-version + end-of-migrate cleanup" alternative and
// the cleanup-on-Complete design preserve that contract, so both produce
// this failure for destructive intermediates.
//
// Operational pattern: destructive migrations must be the *final*
// migration of a batch (where they run without WithSkipSchemaDrop), or be
// applied as a standalone `pgroll start`/`pgroll complete` cycle between
// deploy windows.
func TestDestructiveContractOpFailsAsIntermediate(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, _ *sql.DB) {
		ctx := context.Background()

		// A: create users(id, email). Completed normally (would have been
		// the previous batch's end-state in production).
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "01_create_users",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE users (id integer PRIMARY KEY, email text)`,
					Down: `DROP TABLE users`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		// B: destructive contract — DROP COLUMN runs at Complete time
		// (OnComplete: true). The view in public_01_create_users still
		// projects users.email, so the DDL must fail unless we drop that
		// schema first.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "02_drop_email",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:         `ALTER TABLE users DROP COLUMN email`,
					OnComplete: true,
				},
			},
		}, backfill.NewConfig()))

		// Intermediate Complete (the migrate-loop position): WithSkipSchemaDrop
		// preserves prev-version → DROP COLUMN fails on the dependent view.
		err := mig.Complete(ctx, roll.WithSkipSchemaDrop())
		require.Error(t, err, "destructive intermediate must fail; prev-version views block the drop")
		assert.Contains(t, err.Error(), "depend on it",
			"expected Postgres dependency error, got: %v", err)
	})
}

// TestAbortedBatchLeavesNoOrphanSchemas proves the recovery property of
// the no-intermediate-schemas model: when `pgroll migrate` aborts partway
// through a batch, the database is left with exactly the production-active
// version schema and nothing else. Operators can identify the active
// schema from `\dn` alone — no reasoning across pgroll.migrations state +
// orphan schemas + DB_SEARCH_PATH config.
func TestAbortedBatchLeavesNoOrphanSchemas(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		const (
			vProd = "00_create_users"
			vMid  = "01_add_email"
			vBad  = "02_invalid_sql"
		)

		// Production-active version: completed normally with a version schema.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: vProd,
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE users (id integer PRIMARY KEY, name text)`,
					Down: `DROP TABLE users`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		// Successful intermediate (no version schema projected).
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: vMid,
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `ALTER TABLE users ADD COLUMN email text`,
					Down: `ALTER TABLE users DROP COLUMN email`,
				},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithSkipSchemaDrop()))

		// Failing intermediate: invalid SQL aborts Start. Rollback runs.
		err := mig.Start(ctx, &migrations.Migration{
			Name: vBad,
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE this_is_invalid_sql (`,
					Down: `DROP TABLE this_is_invalid_sql`,
				},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema())
		require.Error(t, err, "Start of invalid SQL must fail")

		// After the abort: only the production-active version schema exists.
		// The successful intermediate left no schema (WithoutVersionSchema)
		// and the failing intermediate's Rollback cleaned up its own row.
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, vProd)),
			"production-active version schema must survive an aborted batch")
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, vMid)),
			"successful intermediate must leave no version schema")
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, vBad)),
			"failing intermediate must leave no version schema")
	})
}

// TestDestructiveOpAgainstNonV0ColumnSucceedsAsIntermediate proves the
// destructive-case lift: when intermediate migrations don't project version
// schemas, a destructive contract op against a column the production-active
// schema doesn't reference now succeeds — even mid-batch. Under the prior
// model (PR #13's deferred-cleanup), the intermediate-V1 schema's view
// would have referenced the column and blocked the drop. With no
// intermediate schemas to project, the only remaining blocker is V0 — and
// V0's view never knew about the column being dropped.
func TestDestructiveOpAgainstNonV0ColumnSucceedsAsIntermediate(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// V0: production-active. View projects (id, name) — no email column.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "00_create_users",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE users (id integer PRIMARY KEY, name text)`,
					Down: `DROP TABLE users`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		// V1 (intermediate): adds an email column. No version schema; no view
		// in any pgroll-managed schema references email.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "01_add_email",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `ALTER TABLE users ADD COLUMN email text`,
					Down: `ALTER TABLE users DROP COLUMN email`,
				},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithSkipSchemaDrop()))

		// V2 (intermediate, destructive contract): DROP COLUMN email at
		// Complete time. V0's view doesn't reference email; V1 has no
		// schema. Nothing blocks the drop.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "02_drop_email",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:         `ALTER TABLE users DROP COLUMN email`,
					OnComplete: true,
				},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))

		err := mig.Complete(ctx, roll.WithSkipSchemaDrop())
		require.NoError(t, err,
			"destructive intermediate must succeed when V0's view doesn't reference the dropped column")

		// Verify the column is actually gone from the underlying table.
		var exists bool
		err = db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_name = 'users' AND column_name = 'email'
			)`).Scan(&exists)
		require.NoError(t, err)
		assert.False(t, exists, "email column should be dropped")
	})
}

// TestExistingVersionSchemas covers the helper used by the migrate command's
// pre-flight summary: it must list every `<schema>_*` schema that exists,
// in stable order, regardless of whether they correspond to the current
// pgroll state.
func TestExistingVersionSchemas(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, _ *sql.DB) {
		ctx := context.Background()

		// Empty initial state.
		schemas, err := mig.ExistingVersionSchemas(ctx)
		require.NoError(t, err)
		assert.Empty(t, schemas)

		// One schema after applying a migration.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name:       "01_first",
			Operations: migrations.Operations{createTableOp("t1")},
		}, backfill.NewConfig()))

		schemas, err = mig.ExistingVersionSchemas(ctx)
		require.NoError(t, err)
		assert.Equal(t, []string{roll.VersionedSchemaName(cSchema, "01_first")}, schemas)

		require.NoError(t, mig.Complete(ctx))

		// Two schemas exist briefly between Start and Complete of the second.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name:       "02_second",
			Operations: migrations.Operations{createTableOp("t2")},
		}, backfill.NewConfig()))

		schemas, err = mig.ExistingVersionSchemas(ctx)
		require.NoError(t, err)
		assert.Equal(t, []string{
			roll.VersionedSchemaName(cSchema, "01_first"),
			roll.VersionedSchemaName(cSchema, "02_second"),
		}, schemas)
	})
}

// TestDeferCompleteAllowsMidBatchDestructiveOp is the load-bearing scenario
// for WithDeferComplete: a weekly batched release where a destructive
// migration sits between additive ones. With WithDeferComplete on every
// intermediate, the destructive DROP COLUMN no longer fails mid-batch — its
// physical execution slides into the drain step at final Complete, *after*
// the previous-production version schema has been dropped (so its view no
// longer projects the column being dropped).
func TestDeferCompleteAllowsMidBatchDestructiveOp(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// V0 (production-active): users table with id, email. Completed
		// normally — its version schema's view projects email and is what
		// apps are still connected to throughout the batch.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "00_create_users",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE users (id integer PRIMARY KEY, email text, name text)`,
					Down: `DROP TABLE users`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		// V1 (intermediate, additive): add column 'role'.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "01_add_role",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `ALTER TABLE users ADD COLUMN role text`,
					Down: `ALTER TABLE users DROP COLUMN role`,
				},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))

		// V2 (intermediate, destructive): drop column 'email'. Uses typed
		// OpDropColumn (not OpRawSQL OnComplete) so its Start mutation —
		// marking the column Deleted in the virtual schema — replays
		// during V4's Start, keeping V4's projected view from referencing
		// email and unblocking the eventual DROP COLUMN at drain time.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "02_drop_email",
			Operations: migrations.Operations{
				&migrations.OpDropColumn{
					Table:  "users",
					Column: "email",
				},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()),
			"WithDeferComplete must not run the destructive DDL — it just queues it")

		// V3 (intermediate, additive): add column 'phone'.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "03_add_phone",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `ALTER TABLE users ADD COLUMN phone text`,
					Down: `ALTER TABLE users DROP COLUMN phone`,
				},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))

		// V4 (final, additive): create a new table. Projects the new target
		// version schema as today.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "04_create_events",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE events (id integer PRIMARY KEY, kind text)`,
					Down: `DROP TABLE events`,
				},
			},
		}, backfill.NewConfig()))

		// At this point V0 (prod) and V4 (target) coexist. V1, V2, V3 have
		// no version schemas (intermediates).
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "00_create_users")))
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "04_create_events")))
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "01_add_role")))
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "02_drop_email")))
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "03_add_phone")))

		// Final Complete: drops V0, drains V1/V2/V3 (DROP COLUMN now
		// succeeds because V0's view is gone), runs V4's ops, marks V4 done.
		require.NoError(t, mig.Complete(ctx),
			"final Complete must drain deferred destructive ops without dependency errors")

		// V0 reaped, V4 remains, intermediates have no schemas.
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "00_create_users")))
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "04_create_events")))

		// email is physically gone from the underlying table — proves
		// V2's deferred Complete actually replayed.
		var emailExists bool
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'email'
			)`, cSchema).Scan(&emailExists))
		assert.False(t, emailExists, "email column must be dropped by V2's drained Complete")

		// role and phone are physically present (proves additive
		// intermediates' Completes also drained successfully).
		var roleExists, phoneExists bool
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'role'
			)`, cSchema).Scan(&roleExists))
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'phone'
			)`, cSchema).Scan(&phoneExists))
		assert.True(t, roleExists, "role column should exist after drain")
		assert.True(t, phoneExists, "phone column should exist after drain")

		// Deferred queue is empty after a successful drain.
		remaining, err := mig.State().DeferredCompletes(ctx, cSchema)
		require.NoError(t, err)
		assert.Empty(t, remaining, "drain must clear the deferred queue on success")
	})
}

// TestDeferCompleteAllowsNextMigrationToStart proves that a deferred
// intermediate frees the active-migration slot immediately. Without this
// property the migrate batch loop would block at the second migration
// because IsActiveMigrationPeriod would still report true.
func TestDeferCompleteAllowsNextMigrationToStart(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, _ *sql.DB) {
		ctx := context.Background()

		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "01_create_users",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE users (id integer PRIMARY KEY)`,
					Down: `DROP TABLE users`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "02_add_email",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `ALTER TABLE users ADD COLUMN email text`,
					Down: `ALTER TABLE users DROP COLUMN email`,
				},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))

		// Active migration slot must be free even though the deferred
		// migration's operations haven't replayed yet.
		active, err := mig.State().IsActiveMigrationPeriod(ctx, cSchema)
		require.NoError(t, err)
		assert.False(t, active, "deferred Complete must release the active-migration slot")

		// The next migration's Start should succeed.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "03_add_phone",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `ALTER TABLE users ADD COLUMN phone text`,
					Down: `ALTER TABLE users DROP COLUMN phone`,
				},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()),
			"Start of subsequent migration must not be blocked by a deferred intermediate")
	})
}

// TestDeferCompleteDrainFailureIsResumable proves the idempotent-drain
// property: when one deferred Complete fails mid-drain, the queue is left
// with the failing migration (and any remaining tail) so that a subsequent
// `pgroll complete` after operator intervention can resume.
func TestDeferCompleteDrainFailureIsResumable(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// V0 prod: simple table.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "00_create_users",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE users (id integer PRIMARY KEY, email text)`,
					Down: `DROP TABLE users`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		// V1 (deferred): OpRawSQL OnComplete that will fail at drain time
		// because the column doesn't exist.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "01_drop_nonexistent",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:         `ALTER TABLE users DROP COLUMN nonexistent`,
					OnComplete: true,
				},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))

		// V2 (final).
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "02_create_events",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE events (id integer PRIMARY KEY)`,
					Down: `DROP TABLE events`,
				},
			},
		}, backfill.NewConfig()))

		// Final Complete merges deferred + active actions into one
		// Coordinator. V1's bad SQL fails partway through; the error
		// surfaces the underlying Postgres failure.
		err := mig.Complete(ctx)
		require.Error(t, err, "merged drain must surface the underlying SQL error")
		assert.Contains(t, err.Error(), "nonexistent",
			"error must identify the failing column reference")

		// All drained migrations remain in the deferred queue. The clear
		// happens only after the merged Coordinator succeeds end-to-end,
		// so a partial failure leaves the queue intact for resumable
		// retry. (Already-executed actions are idempotent under retry —
		// DROP COLUMN IF EXISTS, DROP FUNCTION IF EXISTS CASCADE.)
		remaining, err := mig.State().DeferredCompletes(ctx, cSchema)
		require.NoError(t, err)
		require.Len(t, remaining, 1, "failing batch leaves the queue intact for retry")
		assert.Equal(t, "01_drop_nonexistent", remaining[0].Name)

		// Operator intervention: clear V1 manually to simulate "the
		// operator fixed it" and re-run Complete.
		require.NoError(t, mig.State().ClearCompleteDeferred(ctx, cSchema, "01_drop_nonexistent"))

		// Retry Complete — drain queue is now empty, V2's ops run cleanly,
		// V2 is marked done.
		require.NoError(t, mig.Complete(ctx),
			"resumed Complete must succeed once the failing intermediate has been cleared")

		// events table exists from V2's drained operations.
		assert.True(t, tableExists(t, db, cSchema, "events"))
	})
}

// TestDeferCompleteMergesMultipleDestructiveDrains is the load-bearing case
// for merged-Coordinator drain. Two destructive intermediates each create a
// per-table backfill trigger plus the shared `_pgroll_needs_backfill` marker
// column. Per-migration draining would deadlock: V1's Complete tries to drop
// `_pgroll_needs_backfill` while V2's still-installed trigger references it.
//
// The fix: merge every drained Complete's actions and the active migration's
// actions into one Coordinator before executing. The Coordinator dedupes by
// action ID and moves duplicates to the latest position, so the shared
// `drop_column_<table>__pgroll_needs_backfill` action lands after every
// contributing migration's `drop_function_*` (which CASCADE-removes the
// triggers). Order: drop col(email), drop func(email_trigger), drop
// col(phone), drop func(phone_trigger), drop col(_pgroll_needs_backfill).
func TestDeferCompleteMergesMultipleDestructiveDrains(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// V0 prod: users with two columns, both with data so backfill
		// triggers actually do something on each subsequent drop.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "00_create_users",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE users (id integer PRIMARY KEY, email text NOT NULL, phone text NOT NULL)`,
					Down: `DROP TABLE users`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		// V1 (deferred destructive): drop email with Down SQL, which
		// installs a backfill trigger and the _pgroll_needs_backfill
		// marker column.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "01_drop_email",
			Operations: migrations.Operations{
				&migrations.OpDropColumn{
					Table:  "users",
					Column: "email",
					Down:   "''",
				},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))

		// V2 (deferred destructive): drop phone with Down SQL, installs
		// its own trigger (different name) but ADDs IF NOT EXISTS the
		// same _pgroll_needs_backfill column (no-op, V1 added it).
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "02_drop_phone",
			Operations: migrations.Operations{
				&migrations.OpDropColumn{
					Table:  "users",
					Column: "phone",
					Down:   "''",
				},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))

		// V3 (final): unrelated additive migration to give us a target
		// version schema. The final Complete must drain both destructives
		// without the per-migration deadlock that per-migration draining
		// would produce.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "03_create_events",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE events (id integer PRIMARY KEY)`,
					Down: `DROP TABLE events`,
				},
			},
		}, backfill.NewConfig()))

		require.NoError(t, mig.Complete(ctx),
			"merged-Coordinator drain must order shared cleanups (drop _pgroll_needs_backfill) after every contributing trigger drop")

		// Both destructive drops physically applied.
		for _, col := range []string{"email", "phone"} {
			var exists bool
			require.NoError(t, db.QueryRowContext(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM information_schema.columns
					WHERE table_schema=$1 AND table_name='users' AND column_name=$2
				)`, cSchema, col).Scan(&exists))
			assert.False(t, exists, "%q must be dropped after merged drain", col)
		}

		// _pgroll_needs_backfill is gone too (single drop succeeded
		// because both contributing triggers were dropped first).
		var marker bool
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema=$1 AND table_name='users' AND column_name='_pgroll_needs_backfill'
			)`, cSchema).Scan(&marker))
		assert.False(t, marker, "shared _pgroll_needs_backfill marker must be gone after drain")

		// Deferred queue is drained.
		remaining, err := mig.State().DeferredCompletes(ctx, cSchema)
		require.NoError(t, err)
		assert.Empty(t, remaining, "merged drain clears the queue on success")
	})
}

func TestSingleMigrationCompleteStillDropsPreviousSchema(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		const (
			version1 = "1_create_table"
			version2 = "2_create_table2"
		)

		// Apply two migrations normally (no WithSkipSchemaDrop)
		err := mig.Start(ctx, &migrations.Migration{
			Name:       version1,
			Operations: migrations.Operations{createTableOp("table1")},
		}, backfill.NewConfig())
		require.NoError(t, err)
		err = mig.Complete(ctx)
		require.NoError(t, err)

		err = mig.Start(ctx, &migrations.Migration{
			Name:       version2,
			Operations: migrations.Operations{createTableOp("table2")},
		}, backfill.NewConfig())
		require.NoError(t, err)
		err = mig.Complete(ctx)
		require.NoError(t, err)

		// Previous version schema should have been dropped (normal behavior unchanged)
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, version1)),
			"Previous schema should be dropped during normal Complete")
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, version2)),
			"Current schema should exist")
	})
}

func addColumnOp(tableName string) *migrations.OpAddColumn {
	return &migrations.OpAddColumn{
		Table: tableName,
		Column: migrations.Column{
			Name:     "age",
			Type:     "integer",
			Nullable: true,
		},
	}
}

func MustSelect(t *testing.T, db *sql.DB, schema, version, table string) []map[string]any {
	t.Helper()
	versionSchema := roll.VersionedSchemaName(schema, version)

	selectStmt := fmt.Sprintf("SELECT * FROM %s.%s", versionSchema, table)

	q, err := db.Query(selectStmt)
	if err != nil {
		t.Fatal(err)
	}

	res := make([]map[string]any, 0)

	for q.Next() {
		cols, err := q.Columns()
		if err != nil {
			t.Fatal(err)
		}
		values := make([]any, len(cols))
		valuesPtr := make([]any, len(cols))
		for i := range values {
			valuesPtr[i] = &values[i]
		}
		if err := q.Scan(valuesPtr...); err != nil {
			t.Fatal(err)
		}

		row := map[string]any{}
		for i, col := range cols {
			// avoid having to cast int literals to int64 in tests
			if v, ok := values[i].(int64); ok {
				values[i] = int(v)
			}
			row[col] = values[i]
		}

		res = append(res, row)
	}
	assert.NoError(t, q.Err())

	return res
}

func schemaExists(t *testing.T, db *sql.DB, schema string) bool {
	t.Helper()
	var exists bool
	err := db.QueryRow(`
	SELECT EXISTS(
		SELECT 1
		FROM pg_catalog.pg_namespace
		WHERE nspname = $1
	)`, schema).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}
	return exists
}

func tableExists(t *testing.T, db *sql.DB, schema, table string) bool {
	t.Helper()

	var exists bool
	err := db.QueryRow(`
		SELECT EXISTS(
      SELECT 1
			FROM pg_catalog.pg_tables
			WHERE schemaname = $1
      AND tablename = $2
    )`,
		schema, table).Scan(&exists)
	if err != nil {
		t.Fatal(err)
	}

	return exists
}

func ptr[T any](v T) *T {
	return &v
}

// simulateMigrate emulates `pgroll migrate <dir> --complete` end to end:
// every intermediate runs without projecting a version schema and is
// completed via the migration's classifier-picked CompleteOption (deferred
// for the destructive-but-replay-safe set, inline-via-skip-schema-drop for
// everything else); the final migration projects normally and runs the
// drain. Used by stress tests to exercise the same code paths an operator
// would hit running the migrate command against a Postgres host.
func simulateMigrate(t *testing.T, mig *roll.Roll, ms []*migrations.Migration) {
	t.Helper()
	require.NotEmpty(t, ms, "simulateMigrate needs at least one migration")
	ctx := context.Background()
	cfg := backfill.NewConfig()

	for i, m := range ms[:len(ms)-1] {
		require.NoError(t, mig.Start(ctx, m, cfg, roll.WithoutVersionSchema()),
			"intermediate %d %q Start", i, m.Name)
		opt := roll.WithSkipSchemaDrop()
		if m.CompleteMustBeDeferred() {
			opt = roll.WithDeferComplete()
		}
		require.NoError(t, mig.Complete(ctx, opt),
			"intermediate %d %q Complete", i, m.Name)
	}

	final := ms[len(ms)-1]
	require.NoError(t, mig.Start(ctx, final, cfg), "final %q Start", final.Name)
	require.NoError(t, mig.Complete(ctx), "final %q Complete", final.Name)
}

// columnNames returns the sorted physical column names of a table in the
// given schema. Used as a stable signature for end-state assertions.
func columnNames(t *testing.T, db *sql.DB, schema, table string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `
		SELECT column_name FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2`, schema, table)
	require.NoError(t, err)
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		cols = append(cols, c)
	}
	require.NoError(t, rows.Err())
	sort.Strings(cols)
	return cols
}

func tableNames(t *testing.T, db *sql.DB, schema string) []string {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = $1 AND table_type = 'BASE TABLE'`, schema)
	require.NoError(t, err)
	defer rows.Close()
	var ts []string
	for rows.Next() {
		var n string
		require.NoError(t, rows.Scan(&n))
		ts = append(ts, n)
	}
	require.NoError(t, rows.Err())
	sort.Strings(ts)
	return ts
}

// TestTortureMixedBatchAllOpTypes throws one of each major op into a single
// batch and verifies the final state. The chain interleaves additive ops
// (run inline by the migrate classifier) with deferred destructive ops
// (drop_column, drop_table, OnComplete raw SQL) so the merged-Coordinator
// drain has to handle them all together at final Complete.
func TestTortureMixedBatchAllOpTypes(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// V0 prod: a couple of base tables apps are already connected to.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "00_baseline",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up: `CREATE TABLE accounts (id integer PRIMARY KEY, email text NOT NULL);
					     CREATE TABLE legacy_logs (id integer PRIMARY KEY, body text);
					     INSERT INTO accounts(id, email) VALUES (1, 'a@x'), (2, 'b@x');`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		batch := []*migrations.Migration{
			// Owen — create a new table.
			{Name: "10_owen_create_orders", Operations: migrations.Operations{
				&migrations.OpCreateTable{
					Name: "orders",
					Columns: []migrations.Column{
						{Name: "id", Type: "integer", Pk: true},
						{Name: "account_id", Type: "integer", Nullable: false},
						{Name: "total_cents", Type: "integer", Nullable: false},
					},
				},
			}},
			// Klemen — add an index.
			{Name: "11_klemen_index_orders_account", Operations: migrations.Operations{
				&migrations.OpCreateIndex{
					Name:    "idx_orders_account_id",
					Table:   "orders",
					Columns: []migrations.IndexField{{Column: "account_id"}},
				},
			}},
			// Kerlous — add a column on the prod table.
			{Name: "12_kerlous_add_signup_at", Operations: migrations.Operations{
				&migrations.OpAddColumn{
					Table: "accounts",
					Column: migrations.Column{
						Name: "signup_at", Type: "timestamptz", Nullable: true,
					},
				},
			}},
			// Tim — drop a prod column. THIS is the case PR #18 was about.
			{Name: "13_tim_drop_email", Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "accounts", Column: "email", Down: "''"},
			}},
			// Blake — add another new table.
			{Name: "14_blake_create_audit", Operations: migrations.Operations{
				&migrations.OpCreateTable{
					Name: "audit_events",
					Columns: []migrations.Column{
						{Name: "id", Type: "integer", Pk: true},
						{Name: "kind", Type: "text", Nullable: false},
					},
				},
			}},
			// Ehab — drop a legacy table outright.
			{Name: "15_ehab_drop_legacy_logs", Operations: migrations.Operations{
				&migrations.OpDropTable{Name: "legacy_logs"},
			}},
			// Jess — alter a column type. Inline (duplicator pattern).
			{Name: "16_jess_widen_total", Operations: migrations.Operations{
				&migrations.OpAlterColumn{
					Table:  "orders",
					Column: "total_cents",
					Type:   ptr("bigint"),
					Up:     "total_cents",
					Down:   "total_cents",
				},
			}},
			// Final — additive cap to project the new version schema.
			{Name: "17_final_add_phone", Operations: migrations.Operations{
				&migrations.OpAddColumn{
					Table: "accounts",
					Column: migrations.Column{
						Name: "phone", Type: "text", Nullable: true,
					},
				},
			}},
		}

		simulateMigrate(t, mig, batch)

		// End-state assertions.
		assert.ElementsMatch(t,
			[]string{"accounts", "orders", "audit_events"},
			tableNames(t, db, cSchema),
			"legacy_logs must be gone, new tables present")
		assert.ElementsMatch(t,
			[]string{"id", "signup_at", "phone"},
			columnNames(t, db, cSchema, "accounts"),
			"accounts must have lost email and gained signup_at + phone (no _pgroll_* leftovers)")

		// Verify deferred queue is fully drained.
		remaining, err := mig.State().DeferredCompletes(ctx, cSchema)
		require.NoError(t, err)
		assert.Empty(t, remaining, "queue must be drained after final Complete")

		// Final version schema is what apps will connect to next.
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "17_final_add_phone")))
	})
}

// TestTortureManyDestructivesShareBackfillMarker pushes the multi-deferred
// case the user surfaced: a wave of typed OpDropColumns each installing a
// down-trigger that touches the shared `_pgroll_needs_backfill` marker,
// followed by an OnComplete-true raw SQL drop. All deferrals merge into
// one Coordinator at final Complete; the shared marker drop has to land
// after every contributing trigger drop.
func TestTortureManyDestructivesShareBackfillMarker(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// V0 prod: wide table with several columns we'll drop.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "00_create_wide",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up: `CREATE TABLE wide (
					        id integer PRIMARY KEY,
					        a text NOT NULL,
					        b text NOT NULL,
					        c text NOT NULL,
					        d text NOT NULL,
					        e text NOT NULL
					     );
					     INSERT INTO wide VALUES (1, 'a', 'b', 'c', 'd', 'e');`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		batch := []*migrations.Migration{
			{Name: "01_drop_a", Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "wide", Column: "a", Down: "''"},
			}},
			{Name: "02_drop_b", Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "wide", Column: "b", Down: "''"},
			}},
			{Name: "03_drop_c", Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "wide", Column: "c", Down: "''"},
			}},
			{Name: "04_drop_d", Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "wide", Column: "d", Down: "''"},
			}},
			{Name: "05_drop_e", Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "wide", Column: "e", Down: "''"},
			}},
			// Final additive.
			{Name: "06_final_add_z", Operations: migrations.Operations{
				&migrations.OpAddColumn{
					Table:  "wide",
					Column: migrations.Column{Name: "z", Type: "text", Nullable: true},
				},
			}},
		}

		simulateMigrate(t, mig, batch)

		assert.ElementsMatch(t,
			[]string{"id", "z"},
			columnNames(t, db, cSchema, "wide"),
			"all five drops must land; no leftover _pgroll_needs_backfill")

		remaining, err := mig.State().DeferredCompletes(ctx, cSchema)
		require.NoError(t, err)
		assert.Empty(t, remaining)
	})
}

// TestTortureSevenDevsRandomOrder is the load-bearing claim: a 7-developer
// week where each contributor commits 1-3 migrations of varying op type,
// the chain order is shuffled by PRNG, and the whole thing applies in one
// pgroll migrate. We run multiple deterministic seeds; every seed must
// produce the same final schema signature, proving the batch model is
// robust to the order migrations happen to land on the chain.
//
// Op mix is intentionally constrained to types we currently support
// reliably under the classifier+drain model: creates, additive column ops,
// drops (column/table), renames (inline), and additive raw SQL. Alter
// column type appears once per shuffle to exercise the duplicator path
// running inline alongside deferrals.
func TestTortureSevenDevsRandomOrder(t *testing.T) {
	t.Parallel()

	// Each entry is one developer's contribution: a list of migrations
	// they wrote during the week. Order WITHIN a developer's list is
	// preserved (a developer's drop_column comes after their create_table
	// for the same column). Order ACROSS developers is shuffled by seed.
	type contributor struct {
		name string
		mig  []*migrations.Migration
	}
	contributors := []contributor{
		{name: "owen", mig: []*migrations.Migration{
			{Name: "owen_01_create_orders", Operations: migrations.Operations{
				&migrations.OpCreateTable{Name: "orders", Columns: []migrations.Column{
					{Name: "id", Type: "integer", Pk: true},
					{Name: "amount", Type: "integer", Nullable: false},
				}},
			}},
		}},
		{name: "klemen", mig: []*migrations.Migration{
			{Name: "klemen_01_index_accounts_email", Operations: migrations.Operations{
				&migrations.OpCreateIndex{
					Name:    "idx_accounts_email",
					Table:   "accounts",
					Columns: []migrations.IndexField{{Column: "email"}},
				},
			}},
		}},
		{name: "kerlous", mig: []*migrations.Migration{
			{Name: "kerlous_01_add_kind", Operations: migrations.Operations{
				&migrations.OpAddColumn{
					Table:  "accounts",
					Column: migrations.Column{Name: "kind", Type: "text", Nullable: true},
				},
			}},
			{Name: "kerlous_02_add_kind_default", Operations: migrations.Operations{
				&migrations.OpRawSQL{Up: "ALTER TABLE accounts ALTER COLUMN kind SET DEFAULT 'standard'"},
			}},
		}},
		{name: "tim", mig: []*migrations.Migration{
			{Name: "tim_01_drop_legacy_flag", Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "accounts", Column: "legacy_flag", Down: "FALSE"},
			}},
		}},
		{name: "blake", mig: []*migrations.Migration{
			{Name: "blake_01_create_audit", Operations: migrations.Operations{
				&migrations.OpCreateTable{Name: "audit_events", Columns: []migrations.Column{
					{Name: "id", Type: "integer", Pk: true},
					{Name: "msg", Type: "text", Nullable: false},
				}},
			}},
		}},
		{name: "ehab", mig: []*migrations.Migration{
			{Name: "ehab_01_drop_legacy_logs", Operations: migrations.Operations{
				&migrations.OpDropTable{Name: "legacy_logs"},
			}},
		}},
		{name: "jess", mig: []*migrations.Migration{
			// Jess adds a comment via raw SQL (additive, no shuffle deps).
			// Stresses a different op shape than the other contributors.
			{Name: "jess_01_comment_email", Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up: "COMMENT ON COLUMN accounts.email IS 'normalized lowercase'",
				},
			}},
		}},
	}

	// Compute the dependency-respecting partial order: each contributor's
	// migrations stay in their original sequence. shuffleByCommitTime
	// produces a topologically valid linearization for a given PRNG seed
	// by repeatedly picking a random contributor whose head hasn't been
	// emitted yet. That mimics how migration files arrive in a shared
	// directory ordered by commit timestamp.
	shuffleByCommitTime := func(seed uint64) []*migrations.Migration {
		r := rand.New(rand.NewPCG(seed, seed^0x9E3779B97F4A7C15))
		queues := make([][]*migrations.Migration, len(contributors))
		for i, c := range contributors {
			queues[i] = append([]*migrations.Migration(nil), c.mig...)
		}
		var out []*migrations.Migration
		for {
			active := []int{}
			for i, q := range queues {
				if len(q) > 0 {
					active = append(active, i)
				}
			}
			if len(active) == 0 {
				break
			}
			pick := active[r.IntN(len(active))]
			out = append(out, queues[pick][0])
			queues[pick] = queues[pick][1:]
		}
		return out
	}

	// Many seeds because the chain order is the variable being stressed —
	// each seed produces a different topologically-valid linearization of
	// the seven contributors' commits. All must converge to the same end
	// state. Pinned seeds (deterministic) so failures reproduce; we don't
	// rely on testing.Short to skip slow runs because the suite is fast.
	seeds := []uint64{
		1, 2, 3, 7, 13, 42, 100, 256, 999, 1337,
		20240101, 20251225, 0xCAFEBABE, 0xDEADBEEF, 0xFEEDFACE,
	}
	for _, seed := range seeds {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()

				// V0 prod: the schema state production ran on Sunday.
				require.NoError(t, mig.Start(ctx, &migrations.Migration{
					Name: "00_baseline",
					Operations: migrations.Operations{
						&migrations.OpRawSQL{
							Up: `CREATE TABLE accounts (
							        id integer PRIMARY KEY,
							        email text NOT NULL,
							        legacy_flag boolean DEFAULT false
							     );
							     CREATE TABLE legacy_logs (id integer PRIMARY KEY, body text);
							     INSERT INTO accounts(id, email) VALUES (1, 'a@x'), (2, 'b@x');`,
						},
					},
				}, backfill.NewConfig()))
				require.NoError(t, mig.Complete(ctx))

				// Apply this seed's shuffled batch as one pgroll migrate run.
				simulateMigrate(t, mig, shuffleByCommitTime(seed))

				// Final schema signature — must be the same across seeds.
				assert.ElementsMatch(t,
					[]string{"accounts", "orders", "audit_events"},
					tableNames(t, db, cSchema),
					"legacy_logs dropped, new tables present")
				assert.ElementsMatch(t,
					[]string{"id", "email", "kind"},
					columnNames(t, db, cSchema, "accounts"),
					"accounts: legacy_flag dropped (Tim), kind added (Kerlous)")
				assert.ElementsMatch(t,
					[]string{"id", "amount"},
					columnNames(t, db, cSchema, "orders"))

				// Verify Jess's comment landed.
				var emailComment sql.NullString
				require.NoError(t, db.QueryRowContext(ctx, `
					SELECT col_description(c.oid, a.attnum)
					FROM pg_class c
					JOIN pg_namespace n ON n.oid = c.relnamespace
					JOIN pg_attribute a ON a.attrelid = c.oid
					WHERE n.nspname=$1 AND c.relname='accounts' AND a.attname='email'`,
					cSchema).Scan(&emailComment))
				assert.True(t, emailComment.Valid && emailComment.String == "normalized lowercase",
					"Jess's comment must apply (got %q)", emailComment.String)

				// Email default lookup — Kerlous's raw SQL ran.
				var kindDefault sql.NullString
				require.NoError(t, db.QueryRowContext(ctx, `
					SELECT column_default FROM information_schema.columns
					WHERE table_schema=$1 AND table_name='accounts' AND column_name='kind'`,
					cSchema).Scan(&kindDefault))
				assert.True(t, kindDefault.Valid && strings.Contains(kindDefault.String, "standard"),
					"Kerlous's default-set raw SQL must have applied (got %q)", kindDefault.String)

				// Index from Klemen.
				var indexExists bool
				require.NoError(t, db.QueryRowContext(ctx, `
					SELECT EXISTS (SELECT 1 FROM pg_indexes
					WHERE schemaname=$1 AND indexname='idx_accounts_email')`,
					cSchema).Scan(&indexExists))
				assert.True(t, indexExists, "Klemen's index must exist")

				// Drained queue.
				remaining, err := mig.State().DeferredCompletes(ctx, cSchema)
				require.NoError(t, err)
				assert.Empty(t, remaining, "no leftover deferreds after final complete")

				// No _pgroll_* artifacts on user tables.
				rows, err := db.QueryContext(ctx, `
					SELECT table_name, column_name FROM information_schema.columns
					WHERE table_schema=$1 AND column_name LIKE '_pgroll_%'`, cSchema)
				require.NoError(t, err)
				defer rows.Close()
				var leaked []string
				for rows.Next() {
					var tn, cn string
					require.NoError(t, rows.Scan(&tn, &cn))
					leaked = append(leaked, tn+"."+cn)
				}
				assert.Empty(t, leaked, "no pgroll-internal columns may leak into the final user schema")
			})
		})
	}
}

// TestTortureBackToBackBatches exercises the "weekly release" cadence:
// after one batch is fully completed, a second batch on top must apply
// cleanly and observe the first batch's effects. Catches regressions where
// state from a completed batch (drained queue, dropped schemas) interferes
// with the next pgroll migrate run.
func TestTortureBackToBackBatches(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "00_baseline",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up: `CREATE TABLE t (id integer PRIMARY KEY, a text, b text, c text);
					     INSERT INTO t VALUES (1, 'a', 'b', 'c');`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		// Week 1: drop a, add d.
		simulateMigrate(t, mig, []*migrations.Migration{
			{Name: "w1_drop_a", Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "t", Column: "a", Down: "''"},
			}},
			{Name: "w1_add_d", Operations: migrations.Operations{
				&migrations.OpAddColumn{
					Table:  "t",
					Column: migrations.Column{Name: "d", Type: "text", Nullable: true},
				},
			}},
		})
		assert.ElementsMatch(t, []string{"id", "b", "c", "d"}, columnNames(t, db, cSchema, "t"),
			"week 1 end state")

		// Week 2: drop b, drop c, add e — multiple destructives from
		// week-2 contributors landing on the post-week-1 state.
		simulateMigrate(t, mig, []*migrations.Migration{
			{Name: "w2_drop_b", Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "t", Column: "b", Down: "''"},
			}},
			{Name: "w2_drop_c", Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "t", Column: "c", Down: "''"},
			}},
			{Name: "w2_add_e", Operations: migrations.Operations{
				&migrations.OpAddColumn{
					Table:  "t",
					Column: migrations.Column{Name: "e", Type: "text", Nullable: true},
				},
			}},
		})
		assert.ElementsMatch(t, []string{"id", "d", "e"}, columnNames(t, db, cSchema, "t"),
			"week 2 end state")

		remaining, err := mig.State().DeferredCompletes(ctx, cSchema)
		require.NoError(t, err)
		assert.Empty(t, remaining)
	})
}
