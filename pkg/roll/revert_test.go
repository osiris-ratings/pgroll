// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

// applyTrain applies a delayed-contraction train the way `pgroll migrate
// --complete` does: intermediates without version schemas (deferred when
// destructive, inline otherwise), the final migration with a version schema
// and a deferred complete.
func applyDelayedContractionTrain(t *testing.T, mig *roll.Roll, train []*migrations.Migration) {
	t.Helper()
	ctx := context.Background()

	for i, m := range train {
		final := i == len(train)-1
		if final {
			require.NoError(t, mig.Start(ctx, m, backfill.NewConfig()))
			require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))
			continue
		}
		require.NoError(t, mig.Start(ctx, m, backfill.NewConfig(), roll.WithoutVersionSchema()))
		if m.CompleteMustBeDeferred() {
			require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))
		} else {
			require.NoError(t, mig.Complete(ctx, roll.WithSkipSchemaDrop()))
		}
	}
}

// TestRevertRestoresPreviousDeployment is the incident scenario: a train
// containing destructive, additive, and raw-SQL migrations is applied with
// delayed contraction; `Revert` walks it back out, restoring schema, data,
// and history to the pre-train state.
func TestRevertRestoresPreviousDeployment(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// Train A (previous, known-good deployment): users table.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "00_create_users",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE users (id integer PRIMARY KEY, name text, email text)`,
					Down: `DROP TABLE users`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		// Pre-train data: must survive the revert byte-for-byte.
		_, err := db.ExecContext(ctx,
			`INSERT INTO users (id, name, email) VALUES (1, 'ada', 'ada@example.com'), (2, 'alan', 'alan@example.com')`)
		require.NoError(t, err)

		// Train B (the bad deployment): destructive + inline additive + raw SQL.
		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{
				Name: "01_drop_email",
				Operations: migrations.Operations{
					&migrations.OpDropColumn{Table: "users", Column: "email", Down: "'unknown@example.com'"},
				},
			},
			{
				Name: "02_add_age",
				Operations: migrations.Operations{
					&migrations.OpAddColumn{
						Table:  "users",
						Up:     "18",
						Column: migrations.Column{Name: "age", Type: "integer", Nullable: true},
					},
				},
			},
			{
				Name: "03_create_events",
				Operations: migrations.Operations{
					&migrations.OpRawSQL{
						Up:   `CREATE TABLE events (id integer PRIMARY KEY, kind text)`,
						Down: `DROP TABLE events`,
					},
				},
			},
		})

		// Train B is live: its version schema exists alongside train A's
		// (delayed contraction never drops the previous deployment's schema).
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "00_create_users")))
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "03_create_events")))

		// The window covers the whole train, newest first, with the right
		// per-row states.
		targets, err := mig.RevertTargets(ctx)
		require.NoError(t, err)
		require.Len(t, targets, 3)
		assert.Equal(t, "03_create_events", targets[0].Name)
		assert.Equal(t, roll.RevertStateDeferred, targets[0].State)
		assert.Equal(t, "02_add_age", targets[1].Name)
		assert.Equal(t, roll.RevertStateApplied, targets[1].State)
		assert.Equal(t, "01_drop_email", targets[2].Name)
		assert.Equal(t, roll.RevertStateDeferred, targets[2].State)

		// Revert the train.
		reverted, err := mig.Revert(ctx)
		require.NoError(t, err)
		require.Len(t, reverted, 3)

		// Schema restored: events gone, age gone, email still present.
		assert.False(t, tableExists(t, db, cSchema, "events"), "events table must be dropped by revert")
		var ageExists, emailExists bool
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'age'
			)`, cSchema).Scan(&ageExists))
		assert.False(t, ageExists, "age column (inline-completed add_column) must be dropped by revert")
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'email'
			)`, cSchema).Scan(&emailExists))
		assert.True(t, emailExists, "email column must survive — its drop was deferred, never drained")

		// Data lossless: the deferred drop never contracted, so pre-train
		// email values are intact.
		var email string
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT email FROM users WHERE id = 1`).Scan(&email))
		assert.Equal(t, "ada@example.com", email)

		// History rewound: train A's final migration is the leaf again and
		// train B's version schema is gone while train A's survives.
		latest, err := mig.State().LatestMigration(ctx, cSchema)
		require.NoError(t, err)
		require.NotNil(t, latest)
		assert.Equal(t, "00_create_users", *latest)
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "03_create_events")))
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "00_create_users")))

		// The fixed train can be re-applied cleanly.
		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{
				Name: "01_drop_email_v2",
				Operations: migrations.Operations{
					&migrations.OpDropColumn{Table: "users", Column: "email", Down: "'unknown@example.com'"},
				},
			},
		})
		targets, err = mig.RevertTargets(ctx)
		require.NoError(t, err)
		require.Len(t, targets, 1)
	})
}

// TestSealClosesRevertWindow proves the seal is the point of no return: it
// drains the queued contraction DDL, refreshes the boundary snapshot, keeps
// the live version schema available, and empties the revert window.
func TestSealClosesRevertWindow(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

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

		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{
				Name: "01_drop_email",
				Operations: migrations.Operations{
					&migrations.OpDropColumn{Table: "users", Column: "email", Down: "''"},
				},
			},
			{
				Name: "02_create_events",
				Operations: migrations.Operations{
					&migrations.OpRawSQL{
						Up:   `CREATE TABLE events (id integer PRIMARY KEY)`,
						Down: `DROP TABLE events`,
					},
				},
			},
		})

		// Before the seal: contraction has not run.
		var emailExists bool
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'email'
			)`, cSchema).Scan(&emailExists))
		assert.True(t, emailExists, "deferred drop must not run before the seal")

		sealed, err := mig.SealDeferredCompletes(ctx)
		require.NoError(t, err)
		assert.Equal(t, 2, sealed)

		// Contraction drained: email physically gone.
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'email'
			)`, cSchema).Scan(&emailExists))
		assert.False(t, emailExists, "seal must drain the deferred drop")

		// Live version schema survives the seal (recreated post-drain).
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "02_create_events")))

		// Boundary snapshot refreshed: no pgroll-internal artifacts in the
		// train-final's resulting_schema.
		var snapshot string
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT resulting_schema::text FROM pgroll.migrations WHERE schema = $1 AND name = '02_create_events'`,
			cSchema).Scan(&snapshot))
		assert.NotContains(t, snapshot, "_pgroll_", "boundary snapshot must be refreshed to the clean post-drain state")

		// The window is closed.
		targets, err := mig.RevertTargets(ctx)
		require.NoError(t, err)
		assert.Empty(t, targets, "seal must close the revert window")

		reverted, err := mig.Revert(ctx)
		require.NoError(t, err)
		assert.Empty(t, reverted, "revert after seal must be a no-op")

		// Sealing again is a no-op.
		sealed, err = mig.SealDeferredCompletes(ctx)
		require.NoError(t, err)
		assert.Zero(t, sealed)
	})
}

// TestNonDeferredCompleteIsASealPoint proves a plain start+complete (the
// non-train flow) seals itself: its own contraction ran, so it must not be
// revertible.
func TestNonDeferredCompleteIsASealPoint(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "00_create_users",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE users (id integer PRIMARY KEY)`,
					Down: `DROP TABLE users`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		targets, err := mig.RevertTargets(ctx)
		require.NoError(t, err)
		assert.Empty(t, targets, "a non-deferred Complete must seal the migration")
	})
}

// TestRevertIncludesInProgressMigration proves the walk covers an active
// (started, not completed) migration on top of deferred rows.
func TestRevertIncludesInProgressMigration(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

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

		// Deferred intermediate.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "01_drop_email",
			Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "users", Column: "email", Down: "''"},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))

		// Active migration, left in progress.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "02_create_events",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE events (id integer PRIMARY KEY)`,
					Down: `DROP TABLE events`,
				},
			},
		}, backfill.NewConfig()))

		targets, err := mig.RevertTargets(ctx)
		require.NoError(t, err)
		require.Len(t, targets, 2)
		assert.Equal(t, roll.RevertStateInProgress, targets[0].State)
		assert.Equal(t, roll.RevertStateDeferred, targets[1].State)

		reverted, err := mig.Revert(ctx)
		require.NoError(t, err)
		require.Len(t, reverted, 2)

		assert.False(t, tableExists(t, db, cSchema, "events"))
		var emailExists bool
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'email'
			)`, cSchema).Scan(&emailExists))
		assert.True(t, emailExists)

		latest, err := mig.State().LatestMigration(ctx, cSchema)
		require.NoError(t, err)
		require.NotNil(t, latest)
		assert.Equal(t, "00_create_users", *latest)
	})
}

// TestRevertInlineCompletedCreateConstraint proves the RollbackCompleted
// path: a create_constraint that completed inline mid-train (its Complete
// swaps the original columns for the backfilled duplicates and attaches the
// constraint) is reverted by dropping the constraint.
//
// The train's first migration runs without a version schema so no views
// project the constrained column — inline completion of an intermediate
// create_constraint against a live version schema hits a pre-existing
// pg_depend limitation that exists independently of revert.
func TestRevertInlineCompletedCreateConstraint(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{
				Name: "00_create_users",
				Operations: migrations.Operations{
					&migrations.OpRawSQL{
						Up:   `CREATE TABLE users (id integer PRIMARY KEY, name text)`,
						Down: `DROP TABLE users`,
					},
				},
			},
			{
				Name: "01_unique_name",
				Operations: migrations.Operations{
					&migrations.OpCreateConstraint{
						Table:   "users",
						Name:    "users_name_unique",
						Type:    migrations.OpCreateConstraintTypeUnique,
						Columns: []string{"name"},
						Up:      migrations.MultiColumnUpSQL{"name": "name"},
						Down:    migrations.MultiColumnDownSQL{"name": "name"},
					},
				},
			},
			{
				Name: "02_create_events",
				Operations: migrations.Operations{
					&migrations.OpRawSQL{
						Up:   `CREATE TABLE events (id integer PRIMARY KEY)`,
						Down: `DROP TABLE events`,
					},
				},
			},
		})

		// The inline create_constraint completed: the constraint physically
		// exists on the swapped-in column.
		var uniqueExists bool
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_constraint
				WHERE conname = 'users_name_unique' AND conrelid = to_regclass('users')
			)`).Scan(&uniqueExists))
		require.True(t, uniqueExists, "inline create_constraint must have completed")

		targets, err := mig.RevertTargets(ctx)
		require.NoError(t, err)
		require.Len(t, targets, 3)
		assert.Equal(t, roll.RevertStateApplied, targets[1].State)

		reverted, err := mig.Revert(ctx)
		require.NoError(t, err)
		require.Len(t, reverted, 3)

		// The whole train is gone, including the constraint's table; history
		// is empty again.
		assert.False(t, tableExists(t, db, cSchema, "users"))
		assert.False(t, tableExists(t, db, cSchema, "events"))
		latest, err := mig.State().LatestMigration(ctx, cSchema)
		require.NoError(t, err)
		assert.Nil(t, latest)
	})
}

// TestRevertRefusesAfterInterruptedSeal proves the defensive guard: a
// completed destructive migration left unsealed (a seal that crashed between
// draining and stamping) must not be revertible — the seal must be resumed
// instead.
func TestRevertRefusesAfterInterruptedSeal(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

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

		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{
				Name: "01_drop_email",
				Operations: migrations.Operations{
					&migrations.OpDropColumn{Table: "users", Column: "email", Down: "''"},
				},
			},
		})

		// Simulate a seal interrupted after draining this row but before
		// stamping: flag cleared, row unsealed.
		_, err := db.ExecContext(ctx,
			`UPDATE pgroll.migrations SET complete_deferred = FALSE WHERE schema = $1 AND name = '01_drop_email'`,
			cSchema)
		require.NoError(t, err)

		_, err = mig.RevertTargets(ctx)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not sealed")
	})
}
