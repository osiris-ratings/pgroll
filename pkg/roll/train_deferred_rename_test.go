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

		// Sealing drains the deferred rename and the deferred final add_column:
		// onboarding_templates now exists physically and carries both the
		// inline (branding) and the drained (locale) columns.
		sealed, err := mig.SealDeferredCompletes(ctx)
		require.NoError(t, err)
		require.Equal(t, 2, sealed, "the deferred rename and the deferred final add_column drain at seal")

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
			},
			verifyPost: func(t *testing.T, db *sql.DB) {
				require.Contains(t, columnNames(t, db, cSchema, "onboarding_templates"), "region")
				require.True(t, columnNotNull(t, db, "onboarding_templates", "region"))
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

				// Seal drains the rename (and the deferred final add_column).
				sealed, err := mig.SealDeferredCompletes(ctx)
				require.NoError(t, err)
				require.GreaterOrEqual(t, sealed, 1)

				require.True(t, tableExists(t, db, cSchema, "onboarding_templates"))
				require.False(t, tableExists(t, db, cSchema, "onboarding_configurations"))
				tc.verifyPost(t, db)
			})
		})
	}
}
