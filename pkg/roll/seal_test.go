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

// TestSealStampsBeforeContraction covers the seal-at-intent crash contract:
// a seal interrupted after stamping but before (or during) the drain leaves
// sealed-but-queued rows. Revert must refuse (the window is closed — no
// crash state may present a drained row as revertible), and re-running the
// seal must resume the drain to completion.
func TestSealStampsBeforeContraction(t *testing.T) {
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
				Name: "02_add_age",
				Operations: migrations.Operations{
					&migrations.OpAddColumn{
						Table:  "users",
						Up:     "18",
						Column: migrations.Column{Name: "age", Type: "integer", Nullable: true},
					},
				},
			},
		})

		// Simulate the crash window immediately after the seal's stamp:
		// every done row sealed, the deferred flags still set, no
		// contraction DDL run yet.
		_, err := db.ExecContext(ctx,
			`UPDATE pgroll.migrations SET sealed = TRUE WHERE schema = $1 AND done`, cSchema)
		require.NoError(t, err)

		// The window is closed: revert refuses to touch the half-sealed
		// train rather than running expand-phase rollbacks over rows whose
		// contraction may have partially run.
		targets, err := mig.RevertTargets(ctx)
		require.NoError(t, err)
		require.Empty(t, targets, "sealed rows must not be revertible, even while still queued")

		// Re-running the seal resumes forward: the sealed-but-queued rows
		// are the durable signature of an interrupted seal.
		sealed, err := mig.SealDeferredCompletes(ctx)
		require.NoError(t, err)
		require.Equal(t, 2, sealed, "both deferred rows drain on resume")

		var emailExists bool
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'email'
			)`, cSchema).Scan(&emailExists))
		assert.False(t, emailExists, "the resumed seal must finish the contraction")

		var queued int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*) FROM pgroll.migrations WHERE schema = $1 AND complete_deferred`,
			cSchema).Scan(&queued))
		assert.Zero(t, queued, "the resumed seal must clear the queue")

		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "02_add_age")),
			"the live projection must be recreated by the resumed seal")
	})
}

// TestSealSkipsUnfinishedTrain covers the mid-train-crash recovery contract:
// a `pgroll migrate` that died mid-batch leaves the applied prefix's
// defer-class rows queued with a version-schema-less intermediate as the
// history leaf. The seal must NOT fire — sealing would close the window
// mid-train and drop the previous deployment's (production-pinned) schema —
// so the retry resumes the train instead.
func TestSealSkipsUnfinishedTrain(t *testing.T) {
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

		// A train that crashed after its first (destructive, deferred)
		// intermediate: no version schema, queue non-empty, train-final
		// never applied.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "01_drop_email",
			Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "users", Column: "email", Down: "''"},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))

		sealed, err := mig.SealDeferredCompletes(ctx)
		require.NoError(t, err)
		assert.Zero(t, sealed, "the seal must skip an unfinished train")

		// Nothing was sealed or drained, and the previous deployment's
		// schema survives — production apps stay pinned to it.
		var queued int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*) FROM pgroll.migrations WHERE schema = $1 AND complete_deferred AND NOT sealed`,
			cSchema).Scan(&queued))
		assert.Equal(t, 1, queued, "the crashed train's queue must stay intact and unsealed")
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "00_create_users")),
			"the previous deployment's version schema must survive the skipped seal")

		// The partial train remains losslessly revertible: the documented
		// alternative to resuming it.
		targets, err := mig.RevertTargets(ctx)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		assert.Equal(t, "01_drop_email", targets[0].Name)
		assert.Equal(t, roll.RevertStateDeferred, targets[0].State)
	})
}

// TestSealHealsStrandedCompletes: a crash between an older binary's drain
// and its (post-drain) seal stamp left done/drained/unsealed defer-class
// rows with an empty queue — a state the window guard refuses and no drain
// would ever stamp. The empty-queue seal path must heal it.
func TestSealHealsStrandedCompletes(t *testing.T) {
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

		// Simulate the old-binary strand: drained (flag cleared) but never
		// sealed; the queue is empty.
		_, err := db.ExecContext(ctx,
			`UPDATE pgroll.migrations SET complete_deferred = FALSE WHERE schema = $1 AND name = '01_drop_email'`,
			cSchema)
		require.NoError(t, err)

		// The window guard refuses the stranded row...
		_, err = mig.RevertTargets(ctx)
		require.ErrorContains(t, err, "not sealed")

		// ...and the empty-queue seal heals it, exactly as the error's
		// advice (re-run migrate or complete) promises.
		sealed, err := mig.SealDeferredCompletes(ctx)
		require.NoError(t, err)
		assert.Zero(t, sealed, "nothing drains; the heal only stamps")

		var isSealed bool
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT sealed FROM pgroll.migrations WHERE schema = $1 AND name = '01_drop_email'`,
			cSchema).Scan(&isSealed))
		assert.True(t, isSealed, "the stranded row must be stamped sealed")

		targets, err := mig.RevertTargets(ctx)
		require.NoError(t, err)
		assert.Empty(t, targets, "revert works again (window closed)")
	})
}

// TestRevertRefusesNonContiguousWindow: a sealed row landing on top of an
// open window (an inferred DDL capture or a stamp) means the unsealed rows
// are no longer the history suffix. The walk must refuse up front — not
// execute destructive rollback DDL against a non-leaf and then fail on the
// parent FK at the history delete.
func TestRevertRefusesNonContiguousWindow(t *testing.T) {
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

		// Simulate an inferred DDL capture landing on the open window:
		// inserted done AND sealed, parented on the unsealed leaf.
		_, err := db.ExecContext(ctx, `
			INSERT INTO pgroll.migrations (schema, name, migration, resulting_schema, done, sealed, parent, migration_type)
			VALUES ($1, 'sql_hotfix', '{"operations": [{"sql": {"up": "CREATE INDEX users_email_idx ON users (email)"}}]}', '{}', TRUE, TRUE, '01_drop_email', 'inferred')`,
			cSchema)
		require.NoError(t, err)

		_, err = mig.RevertTargets(ctx)
		require.ErrorContains(t, err, "history has advanced past the revert window")

		_, err = mig.Revert(ctx)
		require.Error(t, err, "the walk must refuse before any DDL runs")
	})
}

// TestRevertRefusesIrreversibleMigration: irreversible raw SQL has no down,
// and OpRawSQL.Rollback with no down returns zero actions — without the
// guard, reverting it "succeeds" as a silent no-op while the migration's
// effects persist.
func TestRevertRefusesIrreversibleMigration(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{
				Name:         "01_legacy_backfill",
				Irreversible: true,
				Operations: migrations.Operations{
					&migrations.OpRawSQL{
						Up: `CREATE TABLE audit_log (id integer PRIMARY KEY)`,
					},
				},
			},
		})

		_, err := mig.RevertTargets(ctx)
		require.ErrorContains(t, err, "irreversible")
	})
}

// TestSealWindowClosesInlineOnlyWindow: a bounded revert can re-open a train
// whose remaining rows are all inline-completed — nothing queued, so the
// drain path has nothing to do. The explicit SealWindow (manual `pgroll
// complete`) must still close that window on demand.
func TestSealWindowClosesInlineOnlyWindow(t *testing.T) {
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

		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{
				Name: "01_add_age",
				Operations: migrations.Operations{
					&migrations.OpAddColumn{
						Table:  "users",
						Up:     "18",
						Column: migrations.Column{Name: "age", Type: "integer", Nullable: true},
					},
				},
			},
			{
				Name: "02_add_score",
				Operations: migrations.Operations{
					&migrations.OpAddColumn{
						Table:  "users",
						Up:     "0",
						Column: migrations.Column{Name: "score", Type: "integer", Nullable: true},
					},
				},
			},
		})

		// Bounded revert removes the (deferred) train-final, leaving an
		// inline-only window with an empty queue.
		reverted, err := mig.Revert(ctx, roll.WithRevertSteps(1))
		require.NoError(t, err)
		require.Len(t, reverted, 1)

		// The drain path has nothing to do and must NOT close the window:
		// the re-applied train-final should join it.
		sealed, err := mig.SealDeferredCompletes(ctx)
		require.NoError(t, err)
		assert.Zero(t, sealed)
		targets, err := mig.RevertTargets(ctx)
		require.NoError(t, err)
		require.Len(t, targets, 1, "the inline row stays revertible after a drain-only seal")

		// The explicit window close stamps it.
		stamped, err := mig.SealWindow(ctx)
		require.NoError(t, err)
		assert.Equal(t, int64(1), stamped)

		targets, err = mig.RevertTargets(ctx)
		require.NoError(t, err)
		assert.Empty(t, targets)
	})
}

// TestRevertInlineCompletedRenameConstraint: rename_constraint completes
// inline as a train intermediate (its Complete physically renames the
// constraint). Reverting it must rename the constraint back — previously
// its no-op Rollback deleted the history row while the rename persisted.
func TestRevertInlineCompletedRenameConstraint(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "00_create_users",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE users (id integer PRIMARY KEY, score integer, CONSTRAINT score_positive CHECK (score >= 0))`,
					Down: `DROP TABLE users`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{
				Name: "01_rename_constraint",
				Operations: migrations.Operations{
					&migrations.OpRenameConstraint{
						Table: "users",
						From:  "score_positive",
						To:    "score_nonnegative",
					},
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
		})

		constraintName := func() string {
			var name string
			require.NoError(t, db.QueryRowContext(ctx, `
				SELECT conname FROM pg_constraint
				WHERE conrelid = 'users'::regclass AND contype = 'c'`).Scan(&name))
			return name
		}
		require.Equal(t, "score_nonnegative", constraintName(), "inline complete renamed the constraint")

		reverted, err := mig.Revert(ctx)
		require.NoError(t, err)
		require.Len(t, reverted, 2)

		assert.Equal(t, "score_positive", constraintName(), "revert must rename the constraint back")
	})
}

// TestSealedRevertRecordsTombstones: a sealed revert must leave tombstones
// for its targets so an unchanged re-apply is refused (the convergent-deploy
// re-application hazard), and clearing them must work.
func TestSealedRevertRecordsTombstones(t *testing.T) {
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
					&migrations.OpDropColumn{Table: "users", Column: "email", Down: "'x@example.com'"},
				},
			},
		})

		sealed, err := mig.SealDeferredCompletes(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, sealed)

		result, err := mig.RevertSealed(ctx, "00_create_users", nil)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, []string{"01_drop_email"}, result.Targets)

		tombstones, err := mig.State().RevertedMigrations(ctx, cSchema)
		require.NoError(t, err)
		hash, ok := tombstones["01_drop_email"]
		require.True(t, ok, "the sealed revert must record a tombstone for its target")
		assert.NotEmpty(t, hash)

		// The hash matches the reverted content, so an unchanged re-apply
		// would be refused; an edited migration hashes differently.
		same, err := (&migrations.Migration{
			Name: "01_drop_email",
			Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "users", Column: "email", Down: "'x@example.com'"},
			},
		}).ContentHash()
		require.NoError(t, err)
		assert.Equal(t, hash, same)

		changed, err := (&migrations.Migration{
			Name: "01_drop_email",
			Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "users", Column: "email", Down: "'y@example.com'"},
			},
		}).ContentHash()
		require.NoError(t, err)
		assert.NotEqual(t, hash, changed)

		require.NoError(t, mig.State().ClearRevertedMigrations(ctx, cSchema, []string{"01_drop_email"}))
		tombstones, err = mig.State().RevertedMigrations(ctx, cSchema)
		require.NoError(t, err)
		assert.Empty(t, tombstones)
	})
}
