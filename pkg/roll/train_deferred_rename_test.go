// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

// TestTrainDeferredRenameThenAddColumn reproduces the crash surfaced by the
// fresh ETL database seed (ENG-6556): a single delayed-contraction train that
// contains a view-backed rename_table followed, in a later migration, by an
// additive add_column on the renamed table.
//
// Under the train model rename_table's physical rename is deferred to seal, so
// the base relation is still named `onboarding_configurations` while the
// migration chain already refers to it logically as `onboarding_templates`.
// Start/Validate resolve that via readSchemaWithDeferred; Complete used to read
// the raw physical schema, so `s.GetTable("onboarding_templates")` returned nil
// and add_column's Complete dereferenced it (op_add_column.go:169 SIGSEGV).
//
// The additive add_column runs as an intermediate (WithSkipSchemaDrop), which
// skips the drain — so its Complete must itself resolve the logical name to the
// still-physical `onboarding_configurations`.
func TestTrainDeferredRenameThenAddColumn(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// Base table under its original physical name.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "00_create_onboarding_configurations",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE onboarding_configurations (id integer PRIMARY KEY, name text)`,
					Down: `DROP TABLE onboarding_configurations`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		// --- one delayed-contraction train ---
		//
		//   01 rename_table (deferred)   → physical rename queued for seal
		//   02 add_column   (intermediate, additive, WithSkipSchemaDrop)
		//                                → the crash path: its Complete runs
		//                                  inline while the rename is still
		//                                  deferred, so it must resolve the
		//                                  logical name to the physical base
		//                                  relation itself.
		//   03 add_column   (final, deferred) → keeps 02 a genuine intermediate
		//                                  and makes the train sealable.
		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{
				Name: "01_rename_to_templates",
				Operations: migrations.Operations{
					&migrations.OpRenameTable{From: "onboarding_configurations", To: "onboarding_templates"},
				},
			},
			{
				Name: "02_add_branding",
				Operations: migrations.Operations{
					&migrations.OpAddColumn{
						Table:  "onboarding_templates",
						Column: migrations.Column{Name: "branding", Type: "jsonb", Nullable: true},
					},
				},
			},
			{
				Name: "03_add_locale",
				Operations: migrations.Operations{
					&migrations.OpAddColumn{
						Table:  "onboarding_templates",
						Column: migrations.Column{Name: "locale", Type: "text", Nullable: true},
					},
				},
			},
		})

		// The rename is still deferred: the base relation is physically
		// onboarding_configurations, and it now carries the branding column
		// added inline mid-train.
		require.True(t, tableExists(t, db, cSchema, "onboarding_configurations"),
			"rename is deferred, base relation keeps its physical name")
		require.False(t, tableExists(t, db, cSchema, "onboarding_templates"),
			"the physical rename must not have happened before seal")
		require.Contains(t, columnNames(t, db, cSchema, "onboarding_configurations"), "branding",
			"the intermediate add_column must land on the still-physical base relation")

		// Contraction drains the deferred rename and the deferred final
		// add_column: onboarding_templates now exists physically and carries
		// both the inline (branding) and the drained (locale) columns.
		drained, _, err := mig.FinishContraction(ctx)
		require.NoError(t, err)
		require.Equal(t, 2, drained, "the deferred rename and the deferred final add_column drain at contraction")

		require.True(t, tableExists(t, db, cSchema, "onboarding_templates"),
			"seal applies the physical rename")
		require.False(t, tableExists(t, db, cSchema, "onboarding_configurations"))
		cols := columnNames(t, db, cSchema, "onboarding_templates")
		require.Contains(t, cols, "branding", "the inline branding column travels with the sealed rename")
		require.Contains(t, cols, "locale", "the deferred locale column drains onto the sealed table")
	})
}

// TestTrainDeferredRenameThenInlineDependents extends the reproduction across
// the inline-additive operations that run their Complete in the same train as
// a still-deferred rename (the WithSkipSchemaDrop path fixed here). Each op
// must resolve its logical table name to the still-physical base relation on
// its own; after seal the effect must travel with the physical rename.
func TestTrainDeferredRenameThenInlineDependents(t *testing.T) {
	t.Parallel()

	indexExists := func(t *testing.T, db *sql.DB, table, index string) bool {
		t.Helper()
		var ok bool
		require.NoError(t, db.QueryRowContext(context.Background(), `
			SELECT EXISTS (SELECT 1 FROM pg_indexes
			WHERE schemaname = $1 AND tablename = $2 AND indexname = $3)`,
			cSchema, table, index).Scan(&ok))
		return ok
	}
	constraintExists := func(t *testing.T, db *sql.DB, table, constraint string) bool {
		t.Helper()
		var ok bool
		require.NoError(t, db.QueryRowContext(context.Background(), `
			SELECT EXISTS (SELECT 1 FROM pg_constraint
			WHERE conrelid = ($1 || '.' || $2)::regclass AND conname = $3)`,
			cSchema, table, constraint).Scan(&ok))
		return ok
	}
	columnNotNull := func(t *testing.T, db *sql.DB, table, column string) bool {
		t.Helper()
		var notNull bool
		require.NoError(t, db.QueryRowContext(context.Background(), `
			SELECT is_nullable = 'NO' FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = $2 AND column_name = $3`,
			cSchema, table, column).Scan(&notNull))
		return notNull
	}
	replicaIdentity := func(t *testing.T, db *sql.DB, table string) string {
		t.Helper()
		var id string
		require.NoError(t, db.QueryRowContext(context.Background(), `
			SELECT c.relreplident FROM pg_class c
			JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relkind = 'r' AND n.nspname = $1 AND c.relname = $2`,
			cSchema, table).Scan(&id))
		return id
	}
	// namedNotNull reports whether this server records NOT NULL constraints as
	// catalog pg_constraint rows (Postgres 18+). On older versions NOT NULL is
	// only the attnotnull attribute, so a name-based constraint lookup would
	// find nothing — the canonical-name assertions below are gated on this.
	namedNotNull := func(t *testing.T, db *sql.DB) bool {
		t.Helper()
		var v int
		require.NoError(t, db.QueryRowContext(context.Background(),
			`SELECT current_setting('server_version_num')::int`).Scan(&v))
		return v >= 180000
	}

	cases := []struct {
		name       string
		op         migrations.Operation
		verifyPre  func(t *testing.T, db *sql.DB) // against physical onboarding_configurations
		verifyPost func(t *testing.T, db *sql.DB) // against onboarding_templates, post-seal
	}{
		{
			name: "create_index",
			op: &migrations.OpCreateIndex{
				Name:    "ix_onboarding_email",
				Table:   "onboarding_templates",
				Columns: []migrations.IndexField{{Column: "email"}},
			},
			verifyPre: func(t *testing.T, db *sql.DB) {
				require.True(t, indexExists(t, db, "onboarding_configurations", "ix_onboarding_email"),
					"the index must be created on the still-physical base relation")
			},
			verifyPost: func(t *testing.T, db *sql.DB) {
				require.True(t, indexExists(t, db, "onboarding_templates", "ix_onboarding_email"),
					"the index travels with the sealed rename")
			},
		},
		{
			name: "create_unique_constraint",
			op: &migrations.OpCreateConstraint{
				Name:    "uq_onboarding_email",
				Table:   "onboarding_templates",
				Type:    "unique",
				Columns: []string{"email"},
				Up:      map[string]string{"email": "email"},
				Down:    map[string]string{"email": "email"},
			},
			verifyPre: func(t *testing.T, db *sql.DB) {
				require.True(t, constraintExists(t, db, "onboarding_configurations", "uq_onboarding_email"),
					"the unique constraint (duplicator) must complete against the physical base relation")
			},
			verifyPost: func(t *testing.T, db *sql.DB) {
				require.True(t, constraintExists(t, db, "onboarding_templates", "uq_onboarding_email"))
			},
		},
		{
			name: "add_not_null_column",
			op: &migrations.OpAddColumn{
				Table:  "onboarding_templates",
				Up:     "'us'",
				Column: migrations.Column{Name: "region", Type: "text", Nullable: false},
			},
			verifyPre: func(t *testing.T, db *sql.DB) {
				require.Contains(t, columnNames(t, db, cSchema, "onboarding_configurations"), "region")
				require.True(t, columnNotNull(t, db, "onboarding_configurations", "region"),
					"the NOT NULL attribute must be upgraded on the physical base relation")
				// The canonical NOT NULL constraint name is derived from the
				// physical relation resolved at inline-Complete time
				// (table.Name == onboarding_configurations), not the logical
				// o.Table. This is what makes the inline DDL target a relation
				// that actually exists pre-seal. Only PG18+ records NOT NULL as
				// a named catalog constraint.
				if namedNotNull(t, db) {
					require.True(t, constraintExists(t, db, "onboarding_configurations", "onboarding_configurations_region_not_null"),
						"NOT NULL constraint must be named off the pre-rename physical relation")
				}
			},
			verifyPost: func(t *testing.T, db *sql.DB) {
				require.Contains(t, columnNames(t, db, cSchema, "onboarding_templates"), "region")
				require.True(t, columnNotNull(t, db, "onboarding_templates", "region"))
				// Postgres does not rename constraints when a table is renamed,
				// so the constraint keeps its pre-rename-derived name after the
				// seal applies the physical rename. Asserted to pin the naming
				// contract of CanonicalNotNullName(table.Name, …). PG18+ only.
				if namedNotNull(t, db) {
					require.True(t, constraintExists(t, db, "onboarding_templates", "onboarding_configurations_region_not_null"),
						"the NOT NULL constraint travels with the sealed rename, retaining its name")
				}
			},
		},
		{
			name: "set_replica_identity",
			op: &migrations.OpSetReplicaIdentity{
				Table:    "onboarding_templates",
				Identity: migrations.ReplicaIdentity{Type: "FULL"},
			},
			verifyPre: func(t *testing.T, db *sql.DB) {
				require.Equal(t, "f", replicaIdentity(t, db, "onboarding_configurations"),
					"REPLICA IDENTITY FULL must be applied to the still-physical base relation")
			},
			verifyPost: func(t *testing.T, db *sql.DB) {
				require.Equal(t, "f", replicaIdentity(t, db, "onboarding_templates"),
					"the replica identity travels with the sealed rename")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
				ctx := context.Background()

				// The whole history applies as one train — faithful to the fresh
				// ETL seed. CREATE TABLE is the first intermediate, so no version
				// schema is ever projected over the base relation to interfere
				// with duplicator column drops.
				applyDelayedContractionTrain(t, mig, []*migrations.Migration{
					{
						Name: "00_create_onboarding_configurations",
						Operations: migrations.Operations{
							&migrations.OpRawSQL{
								Up:   `CREATE TABLE onboarding_configurations (id integer PRIMARY KEY, name text NOT NULL, email text)`,
								Down: `DROP TABLE onboarding_configurations`,
							},
						},
					},
					{
						Name: "01_rename_to_templates",
						Operations: migrations.Operations{
							&migrations.OpRenameTable{From: "onboarding_configurations", To: "onboarding_templates"},
						},
					},
					{
						Name:       "02_" + tc.name,
						Operations: migrations.Operations{tc.op},
					},
					{
						Name: "03_final",
						Operations: migrations.Operations{
							&migrations.OpAddColumn{
								Table:  "onboarding_templates",
								Column: migrations.Column{Name: "trailer", Type: "text", Nullable: true},
							},
						},
					},
				})

				// Pre-seal: rename still deferred, dependent applied to the
				// still-physical base relation.
				require.True(t, tableExists(t, db, cSchema, "onboarding_configurations"))
				require.False(t, tableExists(t, db, cSchema, "onboarding_templates"))
				tc.verifyPre(t, db)

				// Contraction drains the rename (and the deferred final add_column).
				drained, _, err := mig.FinishContraction(ctx)
				require.NoError(t, err)
				require.GreaterOrEqual(t, drained, 1)

				require.True(t, tableExists(t, db, cSchema, "onboarding_templates"))
				require.False(t, tableExists(t, db, cSchema, "onboarding_configurations"))
				tc.verifyPost(t, db)
			})
		})
	}
}

// TestRevertTrainWithDeferredRename walks the reproduction backwards: an
// unsealed one-train ETL seed with a deferred rename_table followed by both
// inline-applied and deferred dependents on the renamed table, reverted before
// it seals. It exercises both revert twins of the forward fix in a single walk:
//
//   - RevertStateApplied (02_add_branding, 03_unique_email):
//     rollbackCompletedOperations reads via readSchemaWithDeferred, and each
//     op's Rollback / RollbackCompleted targets the resolved physical name.
//   - RevertStateDeferred on the renamed table (04_final): rollbackExpandPhase
//     re-keys the physical-only parent snapshot with the still-deferred
//     ancestor rename before replaying 04's Start, so add_column resolves
//     onboarding_templates instead of nil-erroring on the snapshot.
//
// On the unpatched code the RevertStateApplied step nil-derefs (like the
// forward add_column.Complete) and the RevertStateDeferred step fails with
// `table "onboarding_templates" does not exist`.
func TestRevertTrainWithDeferredRename(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// The whole history is one unsealed train (faithful to the fresh ETL
		// seed): CREATE TABLE first so no version schema is projected over the
		// base relation to block the duplicator's column drops.
		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{
				Name: "00_create_onboarding_configurations",
				Operations: migrations.Operations{
					&migrations.OpRawSQL{
						Up:   `CREATE TABLE onboarding_configurations (id integer PRIMARY KEY, name text NOT NULL, email text)`,
						Down: `DROP TABLE onboarding_configurations`,
					},
				},
			},
			{
				Name: "01_rename_to_templates",
				Operations: migrations.Operations{
					&migrations.OpRenameTable{From: "onboarding_configurations", To: "onboarding_templates"},
				},
			},
			{
				Name: "02_add_branding",
				Operations: migrations.Operations{
					&migrations.OpAddColumn{
						Table:  "onboarding_templates",
						Column: migrations.Column{Name: "branding", Type: "jsonb", Nullable: true},
					},
				},
			},
			{
				Name: "03_unique_email",
				Operations: migrations.Operations{
					&migrations.OpCreateConstraint{
						Name:    "uq_onboarding_email",
						Table:   "onboarding_templates",
						Type:    "unique",
						Columns: []string{"email"},
						Up:      map[string]string{"email": "email"},
						Down:    map[string]string{"email": "email"},
					},
				},
			},
			{
				// Trailing deferred add_column on the renamed table: keeps 02/03
				// genuine inline intermediates (RevertStateApplied) and is itself
				// reverted through rollbackExpandPhase against the deferred rename.
				Name: "04_final",
				Operations: migrations.Operations{
					&migrations.OpAddColumn{
						Table:  "onboarding_templates",
						Column: migrations.Column{Name: "locale", Type: "text", Nullable: true},
					},
				},
			},
		})

		// Pre-revert: rename still deferred, dependents applied inline to the
		// still-physical base relation.
		require.True(t, tableExists(t, db, cSchema, "onboarding_configurations"))
		require.False(t, tableExists(t, db, cSchema, "onboarding_templates"))
		require.Contains(t, columnNames(t, db, cSchema, "onboarding_configurations"), "branding")

		targets, err := mig.RevertTargets(ctx)
		require.NoError(t, err)
		require.Len(t, targets, 5, "the entire unsealed train is the revert window")

		reverted, err := mig.Revert(ctx)
		require.NoError(t, err)
		require.Len(t, reverted, 5)

		// The train never sealed and reverted cleanly: neither the physical
		// base relation nor its post-rename logical name survives.
		require.False(t, tableExists(t, db, cSchema, "onboarding_configurations"),
			"reverting 00_create drops the base relation")
		require.False(t, tableExists(t, db, cSchema, "onboarding_templates"),
			"the deferred rename never physically applied")
	})
}

// TestRevertTrainDeferredRenameThenDeferredDropColumn reverts a train whose
// deferred dependents on the renamed table are themselves deferred (drop_column
// + a trailing add_column). Every dependent is reverted through
// rollbackExpandPhase, which must re-key the physical parent snapshot with the
// ancestor rename so drop_column/add_column resolve onboarding_templates.
func TestRevertTrainDeferredRenameThenDeferredDropColumn(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{
				Name: "00_create_onboarding_configurations",
				Operations: migrations.Operations{
					&migrations.OpRawSQL{
						Up:   `CREATE TABLE onboarding_configurations (id integer PRIMARY KEY, name text NOT NULL, email text)`,
						Down: `DROP TABLE onboarding_configurations`,
					},
				},
			},
			{
				Name: "01_rename_to_templates",
				Operations: migrations.Operations{
					&migrations.OpRenameTable{From: "onboarding_configurations", To: "onboarding_templates"},
				},
			},
			{
				Name: "02_drop_email",
				Operations: migrations.Operations{
					&migrations.OpDropColumn{Table: "onboarding_templates", Column: "email", Down: "'unknown@example.com'"},
				},
			},
			{
				Name: "03_final",
				Operations: migrations.Operations{
					&migrations.OpAddColumn{
						Table:  "onboarding_templates",
						Column: migrations.Column{Name: "trailer", Type: "text", Nullable: true},
					},
				},
			},
		})

		// Pre-revert: rename + drop both deferred, so the base relation keeps
		// its physical name and still physically carries email.
		require.True(t, tableExists(t, db, cSchema, "onboarding_configurations"))
		require.False(t, tableExists(t, db, cSchema, "onboarding_templates"))
		require.Contains(t, columnNames(t, db, cSchema, "onboarding_configurations"), "email")

		reverted, err := mig.Revert(ctx)
		require.NoError(t, err)
		require.Len(t, reverted, 4)

		require.False(t, tableExists(t, db, cSchema, "onboarding_configurations"))
		require.False(t, tableExists(t, db, cSchema, "onboarding_templates"))
	})
}

// TestRevertTrainDeferredRenameTableAndColumn covers the OpRenameColumn arm of
// applyDeferredRenames and the ordered composition of a table rename followed
// by a column rename: 03_drop_contact references BOTH the renamed table
// (onboarding_templates) and the renamed column (contact), so reverting it
// through rollbackExpandPhase only resolves if the ancestor renames are
// replayed in apply order (table rename first, then column rename).
func TestRevertTrainDeferredRenameTableAndColumn(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{
				Name: "00_create_onboarding_configurations",
				Operations: migrations.Operations{
					&migrations.OpRawSQL{
						Up:   `CREATE TABLE onboarding_configurations (id integer PRIMARY KEY, name text NOT NULL, email text)`,
						Down: `DROP TABLE onboarding_configurations`,
					},
				},
			},
			{
				Name: "01_rename_to_templates",
				Operations: migrations.Operations{
					&migrations.OpRenameTable{From: "onboarding_configurations", To: "onboarding_templates"},
				},
			},
			{
				Name: "02_rename_email_to_contact",
				Operations: migrations.Operations{
					&migrations.OpRenameColumn{Table: "onboarding_templates", From: "email", To: "contact"},
				},
			},
			{
				Name: "03_drop_contact",
				Operations: migrations.Operations{
					&migrations.OpDropColumn{Table: "onboarding_templates", Column: "contact", Down: "'unknown'"},
				},
			},
			{
				Name: "04_final",
				Operations: migrations.Operations{
					&migrations.OpAddColumn{
						Table:  "onboarding_templates",
						Column: migrations.Column{Name: "trailer", Type: "text", Nullable: true},
					},
				},
			},
		})

		require.True(t, tableExists(t, db, cSchema, "onboarding_configurations"))
		// rename_column is deferred too, so the physical column is still email.
		require.Contains(t, columnNames(t, db, cSchema, "onboarding_configurations"), "email")

		reverted, err := mig.Revert(ctx)
		require.NoError(t, err)
		require.Len(t, reverted, 5)

		require.False(t, tableExists(t, db, cSchema, "onboarding_configurations"))
		require.False(t, tableExists(t, db, cSchema, "onboarding_templates"))
	})
}

// TestRevertTrainInProgressOpOnRenamedTable reverts an in-progress (Started but
// not Completed) op on a table renamed by a still-deferred ancestor. The
// in-progress migration takes the RevertStateInProgress → rollbackExpandPhase
// path, so its parent snapshot must likewise be re-keyed with the deferred
// rename. The base is sealed first so only the rename + in-progress op form the
// revert window, and the revert returns cleanly to the sealed base.
func TestRevertTrainInProgressOpOnRenamedTable(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// Sealed base deployment (out of the revert window).
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "00_create_onboarding_configurations",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE onboarding_configurations (id integer PRIMARY KEY, name text NOT NULL, email text)`,
					Down: `DROP TABLE onboarding_configurations`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		// Deferred rename, then an in-progress add_column on the renamed table
		// that is Started but never Completed.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "01_rename_to_templates",
			Operations: migrations.Operations{
				&migrations.OpRenameTable{From: "onboarding_configurations", To: "onboarding_templates"},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))
		require.NoError(t, mig.Complete(ctx, roll.WithDeferComplete()))

		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "02_add_branding",
			Operations: migrations.Operations{
				&migrations.OpAddColumn{
					Table:  "onboarding_templates",
					Column: migrations.Column{Name: "branding", Type: "jsonb", Nullable: true},
				},
			},
		}, backfill.NewConfig(), roll.WithoutVersionSchema()))

		targets, err := mig.RevertTargets(ctx)
		require.NoError(t, err)
		require.Len(t, targets, 2, "the rename and the in-progress add_column form the window")

		reverted, err := mig.Revert(ctx)
		require.NoError(t, err)
		require.Len(t, reverted, 2)

		// Back to the sealed base: the physical relation survives under its
		// original name with its original columns; the in-progress branding
		// column and the never-applied logical rename are gone.
		require.True(t, tableExists(t, db, cSchema, "onboarding_configurations"),
			"the sealed base relation is preserved")
		require.False(t, tableExists(t, db, cSchema, "onboarding_templates"))
		cols := columnNames(t, db, cSchema, "onboarding_configurations")
		require.NotContains(t, cols, "branding", "the in-progress add_column is rolled back")
		require.Contains(t, cols, "email")
	})
}

// TestRevertTrainChainedDeferredRenames covers ordered composition of chained
// table renames: a→b→c across two deferred migrations, then a deferred
// add_column on c. Reverting the add_column (and the second rename) through
// rollbackExpandPhase only resolves if both ancestor renames replay in apply
// order onto the physical snapshot.
func TestRevertTrainChainedDeferredRenames(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{
				Name: "00_create_a",
				Operations: migrations.Operations{
					&migrations.OpRawSQL{
						Up:   `CREATE TABLE a (id integer PRIMARY KEY)`,
						Down: `DROP TABLE a`,
					},
				},
			},
			{
				Name:       "01_rename_a_to_b",
				Operations: migrations.Operations{&migrations.OpRenameTable{From: "a", To: "b"}},
			},
			{
				Name:       "02_rename_b_to_c",
				Operations: migrations.Operations{&migrations.OpRenameTable{From: "b", To: "c"}},
			},
			{
				Name: "03_final",
				Operations: migrations.Operations{
					&migrations.OpAddColumn{
						Table:  "c",
						Column: migrations.Column{Name: "x", Type: "text", Nullable: true},
					},
				},
			},
		})

		// All renames deferred: the base relation is still physically `a`.
		require.True(t, tableExists(t, db, cSchema, "a"))
		require.False(t, tableExists(t, db, cSchema, "b"))
		require.False(t, tableExists(t, db, cSchema, "c"))

		reverted, err := mig.Revert(ctx)
		require.NoError(t, err)
		require.Len(t, reverted, 4)

		require.False(t, tableExists(t, db, cSchema, "a"))
		require.False(t, tableExists(t, db, cSchema, "b"))
		require.False(t, tableExists(t, db, cSchema, "c"))
	})
}
