// SPDX-License-Identifier: Apache-2.0

package state_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/schema"
	"github.com/xataio/pgroll/pkg/state"
)

func TestMain(m *testing.M) {
	testutils.SharedTestMain(m)
}

func TestSchemaOptionIsRespected(t *testing.T) {
	t.Parallel()

	testutils.WithStateAndConnectionToContainer(t, func(state *state.State, db *sql.DB) {
		ctx := context.Background()

		// create a table in the public schema
		if _, err := db.ExecContext(ctx, "CREATE TABLE public.table1 (id int)"); err != nil {
			t.Fatal(err)
		}

		// check that we can retrieve the already existing table
		currentSchema, err := state.ReadSchema(ctx, "public")
		assert.NoError(t, err)

		assert.Equal(t, 1, len(currentSchema.Tables))
		assert.Equal(t, "public", currentSchema.Name)

		// check that we can start the migration
		err = state.Start(ctx, "public", &migrations.Migration{
			Name: "1_add_column",
			Operations: migrations.Operations{
				&migrations.OpAddColumn{
					Table: "table1",
					Column: migrations.Column{
						Name: "test",
						Type: "text",
					},
				},
			},
		})
		assert.NoError(t, err)
	})
}

func TestInitDoesNotInstallDDLCaptureTriggers(t *testing.T) {
	t.Parallel()

	testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
		ctx := context.Background()

		var triggers int
		err := db.QueryRowContext(ctx,
			"SELECT count(*) FROM pg_catalog.pg_event_trigger WHERE evtname LIKE 'pg_roll_%'").
			Scan(&triggers)
		require.NoError(t, err)
		assert.Equal(t, 0, triggers, "pgroll must not install DDL-capture event triggers")

		var fns int
		err = db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_catalog.pg_proc p
			 JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
			 WHERE n.nspname = $1 AND p.proname = 'raw_migration'`, st.Schema()).
			Scan(&fns)
		require.NoError(t, err)
		assert.Equal(t, 0, fns, "the DDL-capture function must not exist")
	})
}

func TestOutOfBandDDLIsNotRecordedInHistory(t *testing.T) {
	t.Parallel()

	testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
		ctx := context.Background()

		// DDL run outside pgroll, on a connection that sets no guard at all.
		_, err := db.ExecContext(ctx, "CREATE TABLE public.table1 (id int)")
		require.NoError(t, err)
		_, err = db.ExecContext(ctx, "DROP TABLE public.table1")
		require.NoError(t, err)

		var rows int
		err = db.QueryRowContext(ctx,
			fmt.Sprintf("SELECT count(*) FROM %s.migrations", pq.QuoteIdentifier(st.Schema()))).
			Scan(&rows)
		require.NoError(t, err)
		assert.Equal(t, 0, rows, "out-of-band DDL must not be recorded as a migration")

		history, err := st.SchemaHistory(ctx, "public")
		require.NoError(t, err)
		assert.Empty(t, history)
	})
}

// A database initialized by a pgroll old enough to install the capture
// triggers must be cleaned up by the next Init, not left capturing forever.
func TestInitRemovesLegacyDDLCaptureTriggers(t *testing.T) {
	t.Parallel()

	testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
		ctx := context.Background()
		schema := pq.QuoteIdentifier(st.Schema())

		// Recreate the pre-removal shape by hand.
		_, err := db.ExecContext(ctx, fmt.Sprintf(`
			CREATE FUNCTION %s.raw_migration() RETURNS event_trigger
			LANGUAGE plpgsql AS $$ BEGIN RETURN; END; $$;
			CREATE EVENT TRIGGER pg_roll_handle_ddl ON ddl_command_end
				EXECUTE FUNCTION %s.raw_migration();
			CREATE EVENT TRIGGER pg_roll_handle_drop ON sql_drop
				EXECUTE FUNCTION %s.raw_migration();`, schema, schema, schema))
		require.NoError(t, err)

		countObjects := func() (int, int) {
			t.Helper()
			var triggers, fns int
			require.NoError(t, db.QueryRowContext(ctx,
				"SELECT count(*) FROM pg_catalog.pg_event_trigger WHERE evtname LIKE 'pg_roll_%'").
				Scan(&triggers))
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT count(*) FROM pg_catalog.pg_proc p
				 JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
				 WHERE n.nspname = $1 AND p.proname = 'raw_migration'`, st.Schema()).
				Scan(&fns))
			return triggers, fns
		}

		triggers, fns := countObjects()
		require.Equal(t, 2, triggers)
		require.Equal(t, 1, fns)

		require.NoError(t, st.Init(ctx))

		triggers, fns = countObjects()
		assert.Equal(t, 0, triggers, "Init must drop the legacy capture triggers")
		assert.Equal(t, 0, fns, "Init must drop the legacy capture function")
	})
}

func TestPgRollInitializationInANonDefaultSchema(t *testing.T) {
	t.Parallel()

	testutils.WithStateInSchemaAndConnectionToContainer(t, "pgroll_foo", func(state *state.State, _ *sql.DB) {
		ctx := context.Background()

		// Ensure that pgroll state has been correctly initialized in the
		// non-default schema `pgroll_foo` by performing a basic operation on the
		// state
		migrationActive, err := state.IsActiveMigrationPeriod(ctx, "public")
		if err != nil {
			t.Fatal(err)
		}

		assert.False(t, migrationActive)
	})
}

func TestIsInitializedMethodReturnsTrueAfterInitialization(t *testing.T) {
	t.Parallel()

	testutils.WithUninitializedState(t, func(state *state.State) {
		ctx := context.Background()

		// Get whether the state is initialized
		ok, err := state.IsInitialized(ctx)
		require.NoError(t, err)

		// Assert that the state is not initialized as `Init` has not been called
		require.False(t, ok)

		// Invoke `Init` to initialize the state
		err = state.Init(ctx)
		require.NoError(t, err)

		// Get whether the state is initialized
		ok, err = state.IsInitialized(ctx)
		require.NoError(t, err)

		// Assert that the state is initialized
		require.True(t, ok)
	})
}

func TestConcurrentInitialization(t *testing.T) {
	t.Parallel()

	testutils.WithUninitializedState(t, func(state *state.State) {
		ctx := context.Background()
		numGoroutines := 10

		wg := sync.WaitGroup{}
		wg.Add(numGoroutines)
		for i := 0; i < numGoroutines; i++ {
			go func() {
				defer wg.Done()

				if err := state.Init(ctx); err != nil {
					t.Error(err)
				}
			}()
		}

		wg.Wait()
	})
}

func TestReadSchema(t *testing.T) {
	t.Parallel()

	testutils.WithStateAndConnectionToContainer(t, func(state *state.State, db *sql.DB) {
		ctx := context.Background()

		tests := []struct {
			name       string
			createStmt string
			wantSchema *schema.Schema
		}{
			{
				name:       "empty schema",
				createStmt: "",
				wantSchema: &schema.Schema{
					Name:   "public",
					Tables: map[string]*schema.Table{},
				},
			},
			{
				name:       "one table without columns",
				createStmt: "CREATE TABLE public.table1 ()",
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"table1": {
							Name: "table1",
						},
					},
				},
			},
			{
				name:       "one table with columns",
				createStmt: "CREATE TABLE public.table1 (id int)",
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"table1": {
							Name: "table1",
							Columns: map[string]*schema.Column{
								"id": {
									Name:         "id",
									Type:         "integer",
									Nullable:     true,
									PostgresType: "base",
								},
							},
						},
					},
				},
			},
			{
				name:       "unique, not null",
				createStmt: "CREATE TABLE public.table1 (id int NOT NULL, CONSTRAINT id_unique UNIQUE(id))",
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"table1": {
							Name: "table1",
							Columns: map[string]*schema.Column{
								"id": {
									Name:         "id",
									Type:         "integer",
									Nullable:     false,
									Unique:       true,
									PostgresType: "base",
								},
							},
							Indexes: map[string]*schema.Index{
								"id_unique": {
									Name:       "id_unique",
									Unique:     true,
									Columns:    []string{"id"},
									Method:     string(migrations.OpCreateIndexMethodBtree),
									Definition: "CREATE UNIQUE INDEX id_unique ON public.table1 USING btree (id)",
								},
							},
							UniqueConstraints: map[string]*schema.UniqueConstraint{
								"id_unique": {
									Name:    "id_unique",
									Columns: []string{"id"},
								},
							},
						},
					},
				},
			},
			{
				name:       "non-unique index",
				createStmt: "CREATE TABLE public.table1 (id int, name text); CREATE INDEX idx_name ON public.table1 (name)",
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"table1": {
							Name: "table1",
							Columns: map[string]*schema.Column{
								"id": {
									Name:         "id",
									Type:         "integer",
									Nullable:     true,
									PostgresType: "base",
								},
								"name": {
									Name:         "name",
									Type:         "text",
									Nullable:     true,
									PostgresType: "base",
								},
							},
							Indexes: map[string]*schema.Index{
								"idx_name": {
									Name:       "idx_name",
									Unique:     false,
									Columns:    []string{"name"},
									Method:     string(migrations.OpCreateIndexMethodBtree),
									Definition: "CREATE INDEX idx_name ON public.table1 USING btree (name)",
								},
							},
						},
					},
				},
			},
			{
				name:       "foreign key",
				createStmt: "CREATE TABLE public.table1 (id int PRIMARY KEY); CREATE TABLE public.table2 (fk int NOT NULL, CONSTRAINT fk_fkey FOREIGN KEY (fk) REFERENCES public.table1 (id))",
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"table1": {
							Name: "table1",
							Columns: map[string]*schema.Column{
								"id": {
									Name:         "id",
									Type:         "integer",
									Nullable:     false,
									Unique:       true,
									PostgresType: "base",
								},
							},
							PrimaryKey: []string{"id"},
							Indexes: map[string]*schema.Index{
								"table1_pkey": {
									Name:       "table1_pkey",
									Unique:     true,
									Columns:    []string{"id"},
									Method:     string(migrations.OpCreateIndexMethodBtree),
									Definition: "CREATE UNIQUE INDEX table1_pkey ON public.table1 USING btree (id)",
								},
							},
						},
						"table2": {
							Name: "table2",
							Columns: map[string]*schema.Column{
								"fk": {
									Name:         "fk",
									Type:         "integer",
									Nullable:     false,
									PostgresType: "base",
								},
							},
							ForeignKeys: map[string]*schema.ForeignKey{
								"fk_fkey": {
									Name:              "fk_fkey",
									Columns:           []string{"fk"},
									ReferencedTable:   "table1",
									ReferencedColumns: []string{"id"},
									MatchType:         "SIMPLE",
									OnDelete:          "NO ACTION",
									OnUpdate:          "NO ACTION",
								},
							},
						},
					},
				},
			},
			{
				name:       "foreign key with ON DELETE CASCADE",
				createStmt: "CREATE TABLE public.table1 (id int PRIMARY KEY); CREATE TABLE public.table2 (fk int NOT NULL, CONSTRAINT fk_fkey FOREIGN KEY (fk) REFERENCES public.table1 (id) ON DELETE CASCADE)",
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"table1": {
							Name: "table1",
							Columns: map[string]*schema.Column{
								"id": {
									Name:         "id",
									Type:         "integer",
									Nullable:     false,
									Unique:       true,
									PostgresType: "base",
								},
							},
							PrimaryKey: []string{"id"},
							Indexes: map[string]*schema.Index{
								"table1_pkey": {
									Name:       "table1_pkey",
									Unique:     true,
									Columns:    []string{"id"},
									Method:     string(migrations.OpCreateIndexMethodBtree),
									Definition: "CREATE UNIQUE INDEX table1_pkey ON public.table1 USING btree (id)",
								},
							},
						},
						"table2": {
							Name: "table2",
							Columns: map[string]*schema.Column{
								"fk": {
									Name:         "fk",
									Type:         "integer",
									Nullable:     false,
									PostgresType: "base",
								},
							},
							ForeignKeys: map[string]*schema.ForeignKey{
								"fk_fkey": {
									Name:              "fk_fkey",
									Columns:           []string{"fk"},
									ReferencedTable:   "table1",
									ReferencedColumns: []string{"id"},
									MatchType:         "SIMPLE",
									OnDelete:          "CASCADE",
									OnUpdate:          "NO ACTION",
								},
							},
						},
					},
				},
			},
			{
				name:       "foreign key with ON DELETE CASCADE ON UPDATE CASCADE",
				createStmt: "CREATE TABLE public.table1 (id int PRIMARY KEY); CREATE TABLE public.table2 (fk int NOT NULL, CONSTRAINT fk_fkey FOREIGN KEY (fk) REFERENCES public.table1 (id) ON DELETE CASCADE ON UPDATE CASCADE)",
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"table1": {
							Name: "table1",
							Columns: map[string]*schema.Column{
								"id": {
									Name:         "id",
									Type:         "integer",
									Nullable:     false,
									Unique:       true,
									PostgresType: "base",
								},
							},
							PrimaryKey: []string{"id"},
							Indexes: map[string]*schema.Index{
								"table1_pkey": {
									Name:       "table1_pkey",
									Unique:     true,
									Columns:    []string{"id"},
									Method:     string(migrations.OpCreateIndexMethodBtree),
									Definition: "CREATE UNIQUE INDEX table1_pkey ON public.table1 USING btree (id)",
								},
							},
						},
						"table2": {
							Name: "table2",
							Columns: map[string]*schema.Column{
								"fk": {
									Name:         "fk",
									Type:         "integer",
									Nullable:     false,
									PostgresType: "base",
								},
							},
							ForeignKeys: map[string]*schema.ForeignKey{
								"fk_fkey": {
									Name:              "fk_fkey",
									Columns:           []string{"fk"},
									ReferencedTable:   "table1",
									ReferencedColumns: []string{"id"},
									MatchType:         "SIMPLE",
									OnDelete:          "CASCADE",
									OnUpdate:          "CASCADE",
								},
							},
						},
					},
				},
			},
			{
				name:       "foreign key with MATCH full ON DELETE CASCADE",
				createStmt: "CREATE TABLE public.table1 (id int PRIMARY KEY); CREATE TABLE public.table2 (fk int NOT NULL, CONSTRAINT fk_fkey FOREIGN KEY (fk) REFERENCES public.table1 (id) MATCH FULL ON DELETE CASCADE)",
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"table1": {
							Name: "table1",
							Columns: map[string]*schema.Column{
								"id": {
									Name:         "id",
									Type:         "integer",
									Nullable:     false,
									Unique:       true,
									PostgresType: "base",
								},
							},
							PrimaryKey: []string{"id"},
							Indexes: map[string]*schema.Index{
								"table1_pkey": {
									Name:       "table1_pkey",
									Unique:     true,
									Columns:    []string{"id"},
									Method:     string(migrations.OpCreateIndexMethodBtree),
									Definition: "CREATE UNIQUE INDEX table1_pkey ON public.table1 USING btree (id)",
								},
							},
						},
						"table2": {
							Name: "table2",
							Columns: map[string]*schema.Column{
								"fk": {
									Name:         "fk",
									Type:         "integer",
									Nullable:     false,
									PostgresType: "base",
								},
							},
							ForeignKeys: map[string]*schema.ForeignKey{
								"fk_fkey": {
									Name:              "fk_fkey",
									Columns:           []string{"fk"},
									ReferencedTable:   "table1",
									ReferencedColumns: []string{"id"},
									MatchType:         "FULL",
									OnDelete:          "CASCADE",
									OnUpdate:          "NO ACTION",
								},
							},
						},
					},
				},
			},
			{
				name:       "check constraint",
				createStmt: "CREATE TABLE public.table1 (id int PRIMARY KEY, age INTEGER, CONSTRAINT age_check CHECK (age > 18));",
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"table1": {
							Name: "table1",
							Columns: map[string]*schema.Column{
								"id": {
									Name:         "id",
									Type:         "integer",
									Nullable:     false,
									Unique:       true,
									PostgresType: "base",
								},
								"age": {
									Name:         "age",
									Type:         "integer",
									Nullable:     true,
									PostgresType: "base",
								},
							},
							PrimaryKey: []string{"id"},
							Indexes: map[string]*schema.Index{
								"table1_pkey": {
									Name:       "table1_pkey",
									Unique:     true,
									Columns:    []string{"id"},
									Method:     string(migrations.OpCreateIndexMethodBtree),
									Definition: "CREATE UNIQUE INDEX table1_pkey ON public.table1 USING btree (id)",
								},
							},
							CheckConstraints: map[string]*schema.CheckConstraint{
								"age_check": {
									Name:       "age_check",
									Columns:    []string{"age"},
									Definition: "CHECK ((age > 18))",
									NoInherit:  false,
								},
							},
						},
					},
				},
			},
			{
				name:       "check constraint with no inherit",
				createStmt: "CREATE TABLE public.table1 (age INTEGER, CONSTRAINT age_check CHECK (age > 18) NO INHERIT);",
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"table1": {
							Name: "table1",
							Columns: map[string]*schema.Column{
								"age": {
									Name:         "age",
									Type:         "integer",
									Nullable:     true,
									PostgresType: "base",
								},
							},
							CheckConstraints: map[string]*schema.CheckConstraint{
								"age_check": {
									Name:       "age_check",
									Columns:    []string{"age"},
									Definition: "CHECK ((age > 18)) NO INHERIT",
									NoInherit:  true,
								},
							},
						},
					},
				},
			},
			{
				name:       "unique constraint",
				createStmt: "CREATE TABLE public.table1 (id int PRIMARY KEY, name TEXT, CONSTRAINT name_unique UNIQUE(name) );",
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"table1": {
							Name: "table1",
							Columns: map[string]*schema.Column{
								"id": {
									Name:         "id",
									Type:         "integer",
									Nullable:     false,
									Unique:       true,
									PostgresType: "base",
								},
								"name": {
									Name:         "name",
									Type:         "text",
									Unique:       true,
									Nullable:     true,
									PostgresType: "base",
								},
							},
							PrimaryKey: []string{"id"},
							Indexes: map[string]*schema.Index{
								"table1_pkey": {
									Name:       "table1_pkey",
									Unique:     true,
									Columns:    []string{"id"},
									Method:     string(migrations.OpCreateIndexMethodBtree),
									Definition: "CREATE UNIQUE INDEX table1_pkey ON public.table1 USING btree (id)",
								},
								"name_unique": {
									Name:       "name_unique",
									Unique:     true,
									Columns:    []string{"name"},
									Method:     string(migrations.OpCreateIndexMethodBtree),
									Definition: "CREATE UNIQUE INDEX name_unique ON public.table1 USING btree (name)",
								},
							},
							UniqueConstraints: map[string]*schema.UniqueConstraint{
								"name_unique": {
									Name:    "name_unique",
									Columns: []string{"name"},
								},
							},
						},
					},
				},
			},
			{
				name:       "multicolumn unique constraint",
				createStmt: "CREATE TABLE public.table1 (id int PRIMARY KEY, name TEXT, CONSTRAINT name_id_unique UNIQUE(id, name));",
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"table1": {
							Name: "table1",
							Columns: map[string]*schema.Column{
								"id": {
									Name:         "id",
									Type:         "integer",
									Nullable:     false,
									Unique:       true,
									PostgresType: "base",
								},
								"name": {
									Name:         "name",
									Type:         "text",
									Nullable:     true,
									Unique:       false,
									PostgresType: "base",
								},
							},
							PrimaryKey: []string{"id"},
							Indexes: map[string]*schema.Index{
								"table1_pkey": {
									Name:       "table1_pkey",
									Unique:     true,
									Columns:    []string{"id"},
									Method:     string(migrations.OpCreateIndexMethodBtree),
									Definition: "CREATE UNIQUE INDEX table1_pkey ON public.table1 USING btree (id)",
								},
								"name_id_unique": {
									Name:       "name_id_unique",
									Unique:     true,
									Columns:    []string{"id", "name"},
									Method:     string(migrations.OpCreateIndexMethodBtree),
									Definition: "CREATE UNIQUE INDEX name_id_unique ON public.table1 USING btree (id, name)",
								},
							},
							UniqueConstraints: map[string]*schema.UniqueConstraint{
								"name_id_unique": {
									Name:    "name_id_unique",
									Columns: []string{"id", "name"},
								},
							},
						},
					},
				},
			},
			{
				name:       "exclusion constraint",
				createStmt: "CREATE TABLE public.table1 (name TEXT, CONSTRAINT name_unique EXCLUDE USING btree (name WITH =));",
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"table1": {
							Name: "table1",
							Columns: map[string]*schema.Column{
								"name": {
									Name:         "name",
									Type:         "text",
									Nullable:     true,
									PostgresType: "base",
								},
							},
							Indexes: map[string]*schema.Index{
								"name_unique": {
									Name:       "name_unique",
									Exclusion:  true,
									Unique:     false,
									Columns:    []string{"name"},
									Method:     string(migrations.OpCreateIndexMethodBtree),
									Definition: "CREATE INDEX name_unique ON public.table1 USING btree (name)",
								},
							},
							ExcludeConstraints: map[string]*schema.ExcludeConstraint{
								"name_unique": {
									Name:       "name_unique",
									Columns:    []string{"name"},
									Method:     string(migrations.OpCreateIndexMethodBtree),
									Definition: "EXCLUDE USING btree (name WITH =)",
								},
							},
						},
					},
				},
			},
			{
				name: "multicolumn foreign key constraint",
				createStmt: `CREATE TABLE products(
          customer_id INT NOT NULL,
          product_id INT NOT NULL,
          PRIMARY KEY(customer_id, product_id));

          CREATE TABLE orders(
            customer_id INT NOT NULL,
            product_id INT NOT NULL,
            CONSTRAINT fk_customer_product FOREIGN KEY (customer_id, product_id) REFERENCES products (customer_id, product_id));`,
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"products": {
							Name: "products",
							Columns: map[string]*schema.Column{
								"customer_id": {
									Name:         "customer_id",
									Type:         "integer",
									Nullable:     false,
									PostgresType: "base",
								},
								"product_id": {
									Name:         "product_id",
									Type:         "integer",
									Nullable:     false,
									PostgresType: "base",
								},
							},
							PrimaryKey: []string{"customer_id", "product_id"},
							Indexes: map[string]*schema.Index{
								"products_pkey": {
									Name:       "products_pkey",
									Unique:     true,
									Columns:    []string{"customer_id", "product_id"},
									Method:     string(migrations.OpCreateIndexMethodBtree),
									Definition: "CREATE UNIQUE INDEX products_pkey ON public.products USING btree (customer_id, product_id)",
								},
							},
						},
						"orders": {
							Name: "orders",
							Columns: map[string]*schema.Column{
								"customer_id": {
									Name:         "customer_id",
									Type:         "integer",
									Nullable:     false,
									PostgresType: "base",
								},
								"product_id": {
									Name:         "product_id",
									Type:         "integer",
									Nullable:     false,
									PostgresType: "base",
								},
							},
							ForeignKeys: map[string]*schema.ForeignKey{
								"fk_customer_product": {
									Name:              "fk_customer_product",
									Columns:           []string{"customer_id", "product_id"},
									ReferencedTable:   "products",
									ReferencedColumns: []string{"customer_id", "product_id"},
									MatchType:         "SIMPLE",
									OnDelete:          "NO ACTION",
									OnUpdate:          "NO ACTION",
								},
							},
						},
					},
				},
			},
			{
				name: "multicolumn foreign key constraint with on update action",
				createStmt: `CREATE TABLE products(
          customer_id INT NOT NULL,
          product_id INT NOT NULL,
          PRIMARY KEY(customer_id, product_id));

          CREATE TABLE orders(
            customer_id INT NOT NULL,
            product_id INT NOT NULL,
            CONSTRAINT fk_customer_product FOREIGN KEY (customer_id, product_id) REFERENCES products (customer_id, product_id) ON UPDATE CASCADE);`,
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"products": {
							Name: "products",
							Columns: map[string]*schema.Column{
								"customer_id": {
									Name:         "customer_id",
									Type:         "integer",
									Nullable:     false,
									PostgresType: "base",
								},
								"product_id": {
									Name:         "product_id",
									Type:         "integer",
									Nullable:     false,
									PostgresType: "base",
								},
							},
							PrimaryKey: []string{"customer_id", "product_id"},
							Indexes: map[string]*schema.Index{
								"products_pkey": {
									Name:       "products_pkey",
									Unique:     true,
									Columns:    []string{"customer_id", "product_id"},
									Method:     string(migrations.OpCreateIndexMethodBtree),
									Definition: "CREATE UNIQUE INDEX products_pkey ON public.products USING btree (customer_id, product_id)",
								},
							},
						},
						"orders": {
							Name: "orders",
							Columns: map[string]*schema.Column{
								"customer_id": {
									Name:         "customer_id",
									Type:         "integer",
									Nullable:     false,
									PostgresType: "base",
								},
								"product_id": {
									Name:         "product_id",
									Type:         "integer",
									Nullable:     false,
									PostgresType: "base",
								},
							},
							ForeignKeys: map[string]*schema.ForeignKey{
								"fk_customer_product": {
									Name:              "fk_customer_product",
									Columns:           []string{"customer_id", "product_id"},
									ReferencedTable:   "products",
									ReferencedColumns: []string{"customer_id", "product_id"},
									MatchType:         "SIMPLE",
									OnDelete:          "NO ACTION",
									OnUpdate:          "CASCADE",
								},
							},
						},
					},
				},
			},
			{
				name:       "multi-column index",
				createStmt: "CREATE TABLE public.table1 (a text, b text); CREATE INDEX idx_ab ON public.table1 (a, b);",
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"table1": {
							Name: "table1",
							Columns: map[string]*schema.Column{
								"a": {
									Name:         "a",
									Type:         "text",
									Nullable:     true,
									PostgresType: "base",
								},
								"b": {
									Name:         "b",
									Type:         "text",
									Nullable:     true,
									PostgresType: "base",
								},
							},
							Indexes: map[string]*schema.Index{
								"idx_ab": {
									Name:       "idx_ab",
									Unique:     false,
									Columns:    []string{"a", "b"},
									Method:     string(migrations.OpCreateIndexMethodBtree),
									Definition: "CREATE INDEX idx_ab ON public.table1 USING btree (a, b)",
								},
							},
						},
					},
				},
			},
			{
				name:       "column whose type is a UDT in another schema should have the type prefixed with the schema",
				createStmt: "CREATE DOMAIN email_type AS varchar(255); CREATE TABLE public.table1 (a email_type);",
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"table1": {
							Name: "table1",
							Columns: map[string]*schema.Column{
								"a": {
									Name:         "a",
									Type:         "public.email_type",
									Nullable:     true,
									PostgresType: "domain",
								},
							},
						},
					},
				},
			},
			{
				name:       "custom enum types",
				createStmt: "CREATE TYPE review AS ENUM ('good', 'bad', 'ugly'); CREATE TABLE public.table1 (name text, review review);",
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"table1": {
							Name: "table1",
							Columns: map[string]*schema.Column{
								"name": {
									Name:         "name",
									Type:         "text",
									Nullable:     true,
									PostgresType: "base",
								},
								"review": {
									Name:         "review",
									Type:         "public.review",
									Nullable:     true,
									EnumValues:   []string{"good", "bad", "ugly"},
									PostgresType: "enum",
								},
							},
						},
					},
				},
			},
			{
				name: "postgres type types",
				createStmt: `
					CREATE TYPE comptype AS (f1 int, f2 text);
					CREATE TYPE review AS ENUM ('good', 'bad', 'ugly');
					CREATE TYPE float8_range AS RANGE (subtype = float8, subtype_diff = float8mi);
					CREATE DOMAIN us_postal_code AS TEXT
						CHECK(
							VALUE ~ '^\d{5}$'
							OR VALUE ~ '^\d{5}-\d{4}$'
						);
					CREATE TABLE public.table1 (id bigint, comp_col comptype, enum_col review, range_col float8_range, domain_col us_postal_code);`,
				wantSchema: &schema.Schema{
					Name: "public",
					Tables: map[string]*schema.Table{
						"table1": {
							Name: "table1",
							Columns: map[string]*schema.Column{
								"id": {
									Name:         "id",
									Type:         "bigint",
									Nullable:     true,
									PostgresType: "base",
								},
								"comp_col": {
									Name:         "comp_col",
									Type:         "public.comptype",
									Nullable:     true,
									PostgresType: "composite",
								},
								"enum_col": {
									Name:         "enum_col",
									Type:         "public.review",
									Nullable:     true,
									PostgresType: "enum",
									EnumValues:   []string{"good", "bad", "ugly"},
								},
								"range_col": {
									Name:         "range_col",
									Type:         "public.float8_range",
									Nullable:     true,
									PostgresType: "range",
								},
								"domain_col": {
									Name:         "domain_col",
									Type:         "public.us_postal_code",
									Nullable:     true,
									PostgresType: "domain",
								},
							},
						},
					},
				},
			},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if _, err := db.ExecContext(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
					t.Fatal(err)
				}

				if _, err := db.ExecContext(ctx, tt.createStmt); err != nil {
					t.Fatal(err)
				}

				gotSchema, err := state.ReadSchema(ctx, "public")
				if err != nil {
					t.Fatal(err)
				}
				clearOIDS(gotSchema)
				assert.Equal(t, tt.wantSchema, gotSchema)
			})
		}
	})
}

func TestPgrollSchemaVersionUpgrades(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	tests := []struct {
		name                  string
		initialSchemaVersion  string
		pgrollVersion         string
		expectedSchemaVersion string
		expectedError         error
	}{
		{
			name:                  "pgroll schema is older than the pgroll version - pgroll schema is updated",
			initialSchemaVersion:  "0.13.0",
			pgrollVersion:         "0.14.0",
			expectedSchemaVersion: "0.14.0",
		},
		{
			name:                 "pgroll schema is newer than the pgroll version - state initialization fails",
			initialSchemaVersion: "0.15.0",
			pgrollVersion:        "0.14.0",
			expectedError:        state.ErrNewPgrollSchema,
		},
		{
			name:                  "pgroll schema is the same as the pgroll version - pgroll schema is not updated",
			initialSchemaVersion:  "0.13.0",
			pgrollVersion:         "0.13.0",
			expectedSchemaVersion: "0.13.0",
		},
		{
			name:                  "development versions of pgroll never cause a pgroll schema update",
			initialSchemaVersion:  "0.13.0",
			pgrollVersion:         "development",
			expectedSchemaVersion: "0.13.0",
		},
		{
			name:                  "development versions of the pgroll schema are never upgraded",
			initialSchemaVersion:  "development",
			pgrollVersion:         "0.13.0",
			expectedSchemaVersion: "development",
		},
		{
			name:                  "invalid pgroll version - pgroll schema is not updated",
			initialSchemaVersion:  "0.14.0",
			pgrollVersion:         "banana",
			expectedSchemaVersion: "0.14.0",
		},
		{
			name:                  "invalid pgroll schema version - pgroll schema is not updated",
			initialSchemaVersion:  "banana",
			pgrollVersion:         "0.14.0",
			expectedSchemaVersion: "banana",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testutils.WithStateAtVersionAndConnectionToContainer(t, tt.initialSchemaVersion, func(st *state.State, connStr string, _ *sql.DB) {
				// Create a new state instance with the specified pgroll version. This
				// will upgrade the pgroll schema if necessary.
				s, err := state.New(ctx, connStr, "pgroll", state.WithPgrollVersion(tt.pgrollVersion))

				if tt.expectedError != nil {
					require.ErrorIs(t, err, tt.expectedError)
				} else {
					require.NoError(t, err)
					// Get the version of the pgroll schema
					schemaVersion, err := s.SchemaVersion(ctx)
					require.NoError(t, err)

					// Ensure the expected pgroll schema version
					require.Equal(t, tt.expectedSchemaVersion, schemaVersion)
				}
			})
		})
	}
}

func TestSchemaAfterMigration(t *testing.T) {
	t.Parallel()

	t.Run("returns error for non-existent migration", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()

			_, err := st.SchemaAfterMigration(ctx, "public", "non_existent_migration")

			require.ErrorIs(t, err, sql.ErrNoRows)
		})
	})

	t.Run("returns the schemas after the two most recent migrations", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()

			// Apply some SQL and record it, snapshotting the resulting schema
			// the way State.Complete does
			applyAndRecord(ctx, t, st, db, "01_add_items",
				"CREATE TABLE items (id int NOT NULL)")
			applyAndRecord(ctx, t, st, db, "02_add_items_name",
				"ALTER TABLE items ADD COLUMN name text NOT NULL")

			// Get the schema history
			hist, err := st.SchemaHistory(ctx, "public")
			require.NoError(t, err)

			// Assert that the schema history has the expected length
			require.Len(t, hist, 2)

			// Get the schema after the first migration
			sc, err := st.SchemaAfterMigration(ctx, "public", hist[0].Migration.Name)
			require.NoError(t, err)

			// Assert the schema after the first migration
			expectedTable := &schema.Table{
				Name: "items",
				Columns: map[string]*schema.Column{
					"id": {
						Name:         "id",
						Type:         "integer",
						PostgresType: "base",
					},
				},
			}
			clearOIDS(sc)
			require.Len(t, sc.Tables, 1)
			require.Equal(t, expectedTable, sc.Tables["items"])

			// Get the schema after the second migration
			sc, err = st.SchemaAfterMigration(ctx, "public", hist[1].Migration.Name)
			require.NoError(t, err)

			// Assert the schema after the second migration
			expectedTable = &schema.Table{
				Name: "items",
				Columns: map[string]*schema.Column{
					"id": {
						Name:         "id",
						Type:         "integer",
						PostgresType: "base",
					},
					"name": {
						Name:         "name",
						Type:         "text",
						PostgresType: "base",
					},
				},
			}
			clearOIDS(sc)
			require.Len(t, sc.Tables, 1)
			require.Equal(t, expectedTable, sc.Tables["items"])
		})
	})
}

// applyAndRecord runs DDL and records it as a history row carrying a snapshot
// of the resulting schema — the pairing State.Complete performs, and the one
// the removed DDL-capture trigger used to perform for out-of-band statements.
func applyAndRecord(ctx context.Context, t *testing.T, st *state.State, db *sql.DB, name, ddl string) {
	t.Helper()

	_, err := db.ExecContext(ctx, ddl)
	require.NoError(t, err)

	sc, err := st.ReadSchema(ctx, "public")
	require.NoError(t, err)
	resulting, err := json.Marshal(sc)
	require.NoError(t, err)

	body, err := json.Marshal(map[string]any{
		"operations": []any{map[string]any{"sql": map[string]any{"up": ddl}}},
	})
	require.NoError(t, err)

	require.NoError(t, st.Stamp(ctx, "public", name, body, resulting, nil, ""))
}

func clearOIDS(s *schema.Schema) {
	for k := range s.Tables {
		c := s.Tables[k]
		c.OID = ""
		s.Tables[k] = c
	}
}

func TestStamp(t *testing.T) {
	t.Parallel()

	t.Run("inserts a row with parent auto-resolved when no prior history", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(s *state.State, db *sql.DB) {
			ctx := context.Background()
			require.NoError(t, s.Stamp(ctx, "public", "01_first", []byte(`{}`), nil, nil, "pgroll"))

			var parent *string
			var migType string
			var done bool
			require.NoError(t, db.QueryRowContext(ctx,
				"SELECT parent, migration_type, done FROM pgroll.migrations WHERE schema=$1 AND name=$2",
				"public", "01_first").Scan(&parent, &migType, &done))
			assert.Nil(t, parent, "first stamp with no history must have NULL parent")
			assert.Equal(t, "pgroll", migType)
			assert.True(t, done)
		})
	})

	t.Run("auto-resolves parent from latest_migration() when prior history exists", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(s *state.State, db *sql.DB) {
			ctx := context.Background()
			require.NoError(t, s.Stamp(ctx, "public", "01_a", []byte(`{}`), nil, nil, "pgroll"))
			require.NoError(t, s.Stamp(ctx, "public", "02_b", []byte(`{}`), nil, nil, "pgroll"))

			var parent *string
			require.NoError(t, db.QueryRowContext(ctx,
				"SELECT parent FROM pgroll.migrations WHERE schema=$1 AND name=$2",
				"public", "02_b").Scan(&parent))
			require.NotNil(t, parent)
			assert.Equal(t, "01_a", *parent)
		})
	})

	t.Run("honors explicit parent over latest_migration()", func(t *testing.T) {
		// Roll.Stamp builds the chain explicitly to avoid relying on
		// latest_migration() resolving correctly mid-batch — verify an
		// explicit parent is what gets persisted. Linear-history
		// constraint forces the explicit parent to match the leaf.
		testutils.WithStateAndConnectionToContainer(t, func(s *state.State, db *sql.DB) {
			ctx := context.Background()
			require.NoError(t, s.Stamp(ctx, "public", "01_a", []byte(`{}`), nil, nil, "pgroll"))

			explicit := "01_a"
			require.NoError(t, s.Stamp(ctx, "public", "02_b", []byte(`{}`), nil, &explicit, "pgroll"))

			var parent *string
			require.NoError(t, db.QueryRowContext(ctx,
				"SELECT parent FROM pgroll.migrations WHERE schema=$1 AND name=$2",
				"public", "02_b").Scan(&parent))
			require.NotNil(t, parent)
			assert.Equal(t, "01_a", *parent)
		})
	})

	t.Run("stores supplied resulting_schema verbatim", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(s *state.State, db *sql.DB) {
			ctx := context.Background()
			payload := []byte(`{"name":"public","tables":{}}`)
			require.NoError(t, s.Stamp(ctx, "public", "01_x", []byte(`{}`), payload, nil, "pgroll"))

			var stored []byte
			require.NoError(t, db.QueryRowContext(ctx,
				"SELECT resulting_schema FROM pgroll.migrations WHERE schema=$1 AND name=$2",
				"public", "01_x").Scan(&stored))
			assert.JSONEq(t, string(payload), string(stored))
		})
	})

	t.Run("rejects duplicate (schema, name)", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(s *state.State, _ *sql.DB) {
			ctx := context.Background()
			require.NoError(t, s.Stamp(ctx, "public", "01_dup", []byte(`{}`), nil, nil, "pgroll"))
			err := s.Stamp(ctx, "public", "01_dup", []byte(`{}`), nil, nil, "pgroll")
			require.Error(t, err)
		})
	})

	t.Run("MigrationExists tracks stamped names", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(s *state.State, _ *sql.DB) {
			ctx := context.Background()
			exists, err := s.MigrationExists(ctx, "public", "01_x")
			require.NoError(t, err)
			assert.False(t, exists)

			require.NoError(t, s.Stamp(ctx, "public", "01_x", []byte(`{}`), nil, nil, "pgroll"))

			exists, err = s.MigrationExists(ctx, "public", "01_x")
			require.NoError(t, err)
			assert.True(t, exists)
		})
	})
}
