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

// TestContractionStampsBeforeDDL covers the seal-at-intent crash contract: a
// contraction interrupted after stamping but before (or during) the drain
// leaves sealed-but-queued rows. Revert must refuse (the window is closed —
// no crash state may present a drained row as revertible), and re-running
// the contraction must resume the drain to completion.
func TestContractionStampsBeforeDDL(t *testing.T) {
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

		// Simulate the crash window immediately after the stamp: every done
		// row sealed, the deferred flags still set, no contraction DDL run
		// yet.
		_, err := db.ExecContext(ctx,
			`UPDATE pgroll.migrations SET sealed = TRUE WHERE schema = $1 AND done`, cSchema)
		require.NoError(t, err)

		// The window is closed: revert refuses to touch the half-sealed
		// batch rather than running expand-phase rollbacks over rows whose
		// contraction may have partially run.
		targets, err := mig.RevertTargets(ctx)
		require.NoError(t, err)
		require.Empty(t, targets, "sealed rows must not be revertible, even while still queued")

		// Re-running the contraction resumes forward: the sealed-but-queued
		// rows are the durable signature of an interrupted run.
		drained, _, err := mig.FinishContraction(ctx)
		require.NoError(t, err)
		require.Equal(t, 2, drained, "both deferred rows drain on resume")

		var emailExists bool
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'email'
			)`, cSchema).Scan(&emailExists))
		assert.False(t, emailExists, "the resumed contraction must finish the drain")

		var queued int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*) FROM pgroll.migrations WHERE schema = $1 AND complete_deferred`,
			cSchema).Scan(&queued))
		assert.Zero(t, queued, "the resumed contraction must clear the queue")

		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "02_add_age")),
			"the live projection must survive the resumed contraction")
	})
}

// TestContractionKeepsLiveSchemaForAdditiveDrain proves the live-schema drop
// is skipped when the drain has nothing to contract. A create-table batch
// left deferred is projection-preserving — its Complete does not drop or
// rename anything the live views project — so the live version schema need
// not be dropped. The contraction must succeed even while a backend holds
// the live schema open with the AccessShare lock that would otherwise wedge
// the (unnecessary) DROP SCHEMA, with retries disabled so a drop attempt
// would fail outright.
func TestContractionKeepsLiveSchemaForAdditiveDrain(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
		[]roll.Option{roll.WithLockTimeoutMs(50), roll.WithLockRetryTimeout(-1)},
		func(mig *roll.Roll, db *sql.DB) {
			ctx := context.Background()

			// Base deployment: the users table becomes the live projection.
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

			// An additive batch awaiting contraction: a new table whose
			// Complete touches neither users nor any identifier the live
			// views project.
			applyDelayedContractionTrain(t, mig, []*migrations.Migration{
				{Name: "01_create_sessions", Operations: migrations.Operations{createTableOp("sessions")}},
			})

			live := roll.VersionedSchemaName(cSchema, "01_create_sessions")

			// A live backend holds the live schema's users view open.
			// Dropping the live schema would need AccessExclusive on this
			// view and fail immediately (retries disabled); the drain itself
			// never touches users, so nothing legitimately blocks.
			release := holdAccessShareOnView(t, db, live+".users")
			defer release()

			// The contraction must still succeed: the additive drain needs
			// no live-schema drop, so the schema is left in place.
			drained, _, err := mig.FinishContraction(ctx)
			require.NoError(t, err)
			require.Equal(t, 1, drained)

			assert.True(t, schemaExists(t, db, live),
				"a projection-preserving drain must keep the live schema in place")

			var queued int
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT count(*) FROM pgroll.migrations WHERE schema = $1 AND complete_deferred`,
				cSchema).Scan(&queued))
			assert.Zero(t, queued, "the drain must clear the deferred queue")
		})
}

// TestContractionLiveSchemaHandlingPerOp is the empirical gate for skipping
// the live-schema drop. Each case applies a deferred batch that contracts
// table `t`, while a backend holds the live view of an UNRELATED table `hot`
// (which the migration never touches) with retries disabled. For every typed
// contraction the drain must succeed WITHOUT dropping the live schema — so
// the held `hot` view never blocks it. Only opaque onComplete raw SQL forces
// the whole-schema drop, which the held view then blocks.
func TestContractionLiveSchemaHandlingPerOp(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		op        migrations.Operation
		wantErr   bool // opaque raw SQL: full-schema drop blocked by the held view
		tViewGone bool // drop_table removes t's own view
	}{
		{
			name: "rename_column",
			op:   &migrations.OpRenameColumn{Table: "t", From: "value", To: "label"},
		},
		{
			name: "drop_column",
			op:   &migrations.OpDropColumn{Table: "t", Column: "value", Down: "''"},
		},
		{
			name: "alter_column",
			op:   &migrations.OpAlterColumn{Table: "t", Column: "value", Type: ptr("text"), Up: "value", Down: "value"},
		},
		{
			name: "create_constraint",
			op: &migrations.OpCreateConstraint{
				Table:   "t",
				Name:    "t_value_unique",
				Type:    migrations.OpCreateConstraintTypeUnique,
				Columns: []string{"value"},
				Up:      migrations.MultiColumnUpSQL{"value": "value"},
				Down:    migrations.MultiColumnDownSQL{"value": "value"},
			},
		},
		{
			name:      "drop_table",
			op:        &migrations.OpDropTable{Name: "t"},
			tViewGone: true,
		},
		{
			name:    "opaque_oncomplete_raw_sql",
			op:      &migrations.OpRawSQL{Up: "SELECT 1", OnComplete: true},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
				[]roll.Option{roll.WithLockTimeoutMs(50), roll.WithLockRetryTimeout(-1)},
				func(mig *roll.Roll, db *sql.DB) {
					ctx := context.Background()

					// Base deployment: `t` (to be contracted) and an
					// unrelated hot table `hot`.
					require.NoError(t, mig.Start(ctx, &migrations.Migration{
						Name: "00_base",
						Operations: migrations.Operations{
							&migrations.OpCreateTable{Name: "t", Columns: []migrations.Column{
								{Name: "id", Type: "integer", Pk: true},
								{Name: "value", Type: "varchar(255)", Nullable: true},
							}},
							createTableOp("hot"),
						},
					}, backfill.NewConfig()))
					require.NoError(t, mig.Complete(ctx))

					// The deferred contraction batch.
					version := "01_" + tc.name
					applyDelayedContractionTrain(t, mig, []*migrations.Migration{
						{Name: version, Operations: migrations.Operations{tc.op}},
					})
					live := roll.VersionedSchemaName(cSchema, version)

					// A live backend holds the unrelated `hot` view open.
					// Dropping the live SCHEMA needs AccessExclusive on it
					// and would fail immediately (retries disabled); the
					// contraction touches only `t`, so nothing legitimately
					// blocks.
					release := holdAccessShareOnView(t, db, live+".hot")
					defer release()

					_, _, err := mig.FinishContraction(ctx)
					if tc.wantErr {
						require.Error(t, err,
							"opaque drain must take the full-schema drop, which the held view blocks")
						return
					}
					require.NoError(t, err,
						"a typed contraction must drain without dropping the live schema")

					assert.True(t, schemaExists(t, db, live), "live schema must remain")
					assert.True(t, viewExists(t, db, live, "hot"),
						"the unrelated view held open must be untouched")
					if tc.tViewGone {
						assert.False(t, viewExists(t, db, live, "t"),
							"a dropped table's view must be gone")
					} else {
						assert.True(t, viewExists(t, db, live, "t"),
							"the contracted table's view must remain (auto-maintained)")
						// The view is still valid/queryable post-contraction.
						MustSelect(t, db, cSchema, version, "t")
					}
				})
		})
	}
}

// TestContractionRefusesUnfinishedBatch covers the mid-batch-crash recovery
// contract: a `pgroll migrate` that died mid-batch leaves the applied
// prefix's defer-class rows queued with a version-schema-less intermediate
// as the history leaf. The contraction must REFUSE — draining would fire the
// point of no return mid-batch and drop the previous deployment's
// (production-pinned) schema — so the operator resumes or aborts the batch.
func TestContractionRefusesUnfinishedBatch(t *testing.T) {
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

		// A batch that crashed after its first (destructive, deferred)
		// intermediate: no version schema, queue non-empty, batch-final
		// never applied.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "01_drop_email",
			Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "users", Column: "email", Down: "''"},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))

		_, _, err := mig.FinishContraction(ctx)
		require.ErrorContains(t, err, "unfinished", "the contraction must refuse an unfinished batch")

		// Nothing was sealed or drained, and the previous deployment's
		// schema survives — production apps stay pinned to it.
		var queued int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*) FROM pgroll.migrations WHERE schema = $1 AND complete_deferred AND NOT sealed`,
			cSchema).Scan(&queued))
		assert.Equal(t, 1, queued, "the crashed batch's queue must stay intact and unsealed")
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "00_create_users")),
			"the previous deployment's version schema must survive the refused contraction")

		// The partial batch remains losslessly revertible: the documented
		// alternative to resuming it.
		targets, err := mig.RevertTargets(ctx)
		require.NoError(t, err)
		require.Len(t, targets, 1)
		assert.Equal(t, "01_drop_email", targets[0].Name)
		assert.Equal(t, roll.RevertStateDeferred, targets[0].State)
	})
}

// TestContractionSealsStrandedCompletes: a crash between an older binary's
// drain and its (post-drain) seal stamp left done/drained/unsealed
// defer-class rows with an empty queue — a state the window guard refuses
// and no drain would ever stamp. The empty-queue contraction must heal it.
func TestContractionSealsStrandedCompletes(t *testing.T) {
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

		// ...and the empty-queue contraction heals it, exactly as the
		// error's advice (re-run migrate or complete) promises.
		drained, stamped, err := mig.FinishContraction(ctx)
		require.NoError(t, err)
		assert.Zero(t, drained, "nothing drains; the heal only stamps")
		assert.Equal(t, int64(1), stamped, "the stranded row is stamped sealed")

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

// TestFinishContractionClosesInlineOnlyWindow: a bounded revert can re-open
// a batch whose remaining rows are all inline-completed — nothing queued, so
// the drain path has nothing to do. `pgroll complete` (FinishContraction)
// must still close that window and converge the version schemas.
func TestFinishContractionClosesInlineOnlyWindow(t *testing.T) {
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

		// Bounded revert removes the (deferred) batch-final, leaving an
		// inline-only window with an empty queue.
		reverted, err := mig.Revert(ctx, roll.WithRevertSteps(1))
		require.NoError(t, err)
		require.Len(t, reverted, 1)

		// The explicit contraction stamps the inline row and converges the
		// version schemas onto the new leaf's.
		drained, stamped, err := mig.FinishContraction(ctx)
		require.NoError(t, err)
		assert.Zero(t, drained)
		assert.Equal(t, int64(1), stamped)

		targets, err := mig.RevertTargets(ctx)
		require.NoError(t, err)
		assert.Empty(t, targets)

		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "01_add_age")),
			"the leaf's projection must survive")
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "00_create_users")),
			"older version schemas must be dropped by the contraction")
	})
}

// TestRevertInlineCompletedRenameConstraint: rename_constraint completes
// inline as a batch intermediate (its Complete physically renames the
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

// TestInversionRevertRecordsTombstones: an inversion revert must leave
// tombstones for its targets so an unchanged re-apply is refused (the
// convergent-deploy re-application hazard), and clearing them must work.
func TestInversionRevertRecordsTombstones(t *testing.T) {
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

		drained, _, err := mig.FinishContraction(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, drained)

		result, err := mig.RevertSealed(ctx, "00_create_users", nil)
		require.NoError(t, err)
		require.NotNil(t, result)
		require.Equal(t, []string{"01_drop_email"}, result.Targets)

		tombstones, err := mig.State().RevertedMigrations(ctx, cSchema)
		require.NoError(t, err)
		hash, ok := tombstones["01_drop_email"]
		require.True(t, ok, "the inversion revert must record a tombstone for its target")
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

// TestBatchedMigrateCompleteConverges is the end-state invariant for the
// two-phase deploy: after a batch is applied expand-only (final ACTIVE) and
// `pgroll complete` contracts it, exactly one version schema exists, every
// row is sealed with an empty queue, and the leaf's snapshot is a clean
// anchor for inversion reverts.
func TestBatchedMigrateCompleteConverges(t *testing.T) {
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

		// The batch, as `pgroll migrate` (no --complete) applies it:
		// defer-class intermediate, inline intermediate, final left ACTIVE.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "01_drop_email",
			Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "users", Column: "email", Down: "''"},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))

		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "02_add_age",
			Operations: migrations.Operations{
				&migrations.OpAddColumn{
					Table:  "users",
					Up:     "18",
					Column: migrations.Column{Name: "age", Type: "integer", Nullable: true},
				},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithSkipSchemaDrop()))

		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name:       "03_create_sessions",
			Operations: migrations.Operations{createTableOp("sessions")},
		}, backfill.NewConfig()))

		// The fleet repins here; then the deploy's contraction step runs.
		require.NoError(t, mig.Complete(ctx))

		// Exactly one version schema: the leaf's.
		schemas, err := mig.ExistingVersionSchemas(ctx)
		require.NoError(t, err)
		assert.Equal(t, []string{roll.VersionedSchemaName(cSchema, "03_create_sessions")}, schemas)

		// Every row sealed; queue empty.
		var unsealed, queued int
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT count(*) FILTER (WHERE NOT sealed), count(*) FILTER (WHERE complete_deferred)
			 FROM pgroll.migrations WHERE schema = $1`, cSchema).Scan(&unsealed, &queued))
		assert.Zero(t, unsealed, "every row must be sealed after the deploy's complete")
		assert.Zero(t, queued, "the intra-deploy queue must drain at complete")

		// The drain physically ran.
		var emailExists bool
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'email'
			)`, cSchema).Scan(&emailExists))
		assert.False(t, emailExists, "the deferred contraction must run at complete")

		// The leaf snapshot is a clean inversion anchor.
		var snapshot string
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT resulting_schema::text FROM pgroll.migrations WHERE schema = $1 AND name = '03_create_sessions'`,
			cSchema).Scan(&snapshot))
		assert.NotContains(t, snapshot, "_pgroll_", "the deploy-boundary snapshot must be clean")
	})
}
