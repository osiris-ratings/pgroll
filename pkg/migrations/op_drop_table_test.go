// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"database/sql"
	"testing"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/migrations"
)

func TestDropTable(t *testing.T) {
	t.Parallel()

	ExecuteTests(t, TestCases{
		{
			name: "drop table",
			migrations: []migrations.Migration{
				{
					Name:          "01_create_table",
					VersionSchema: "create_table",
					Operations: migrations.Operations{
						&migrations.OpCreateTable{
							Name: "users",
							Columns: []migrations.Column{
								{
									Name: "id",
									Type: "serial",
									Pk:   true,
								},
								{
									Name:   "name",
									Type:   "varchar(255)",
									Unique: true,
								},
							},
						},
					},
				},
				{
					Name:          "02_drop_table",
					VersionSchema: "drop_table",
					Operations: migrations.Operations{
						&migrations.OpDropTable{
							Name: "users",
						},
					},
				},
			},
			afterStart: func(t *testing.T, db *sql.DB, schema string) {
				// The view for the deleted table does not exist in the new version schema.
				ViewMustNotExist(t, db, schema, "drop_table", "users")

				// The underlying table has been soft-deleted (renamed).
				TableMustExist(t, db, schema, migrations.DeletionName(migrations.MigrationScopeFor("02_drop_table"), "users"))
			},
			afterRollback: func(t *testing.T, db *sql.DB, schema string) {
				// Rollback is a no-op.
			},
			afterComplete: func(t *testing.T, db *sql.DB, schema string) {
				// The underlying table has been deleted.
				TableMustNotExist(t, db, schema, "users")
			},
		},
	})
}

func TestDropTableInMultiOperationMigrations(t *testing.T) {
	t.Parallel()

	ExecuteTests(t, TestCases{
		{
			name: "create table, drop table",
			migrations: []migrations.Migration{
				{
					Name: "01_multi_operation",
					Operations: migrations.Operations{
						&migrations.OpCreateTable{
							Name: "items",
							Columns: []migrations.Column{
								{
									Name: "id",
									Type: "serial",
									Pk:   true,
								},
								{
									Name: "name",
									Type: "varchar(255)",
								},
							},
						},
						&migrations.OpDropTable{
							Name: "items",
						},
					},
				},
			},
			afterStart: func(t *testing.T, db *sql.DB, schema string) {
				// OpDropTable drops tables on migration completion, so the table
				// created by OpCreateTable is present after migration start but has
				// been soft-deleted (renamed).
				TableMustExist(t, db, schema, migrations.DeletionName(migrations.MigrationScopeFor("01_multi_operation"), "items"))

				// There is no view for the "items" table in the new schema
				ViewMustNotExist(t, db, schema, "01_multi_operation", "items")
			},
			afterRollback: func(t *testing.T, db *sql.DB, schema string) {
				// The table is not present
				TableMustNotExist(t, db, schema, "items")
			},
			afterComplete: func(t *testing.T, db *sql.DB, schema string) {
				// The table is not present
				TableMustNotExist(t, db, schema, "items")

				// There is no view for the "items" table in the new schema
				ViewMustNotExist(t, db, schema, "01_multi_operation", "items")
			},
		},
		{
			name: "create table, rename table, drop table",
			migrations: []migrations.Migration{
				{
					Name: "01_multi_operation",
					Operations: migrations.Operations{
						&migrations.OpCreateTable{
							Name: "items",
							Columns: []migrations.Column{
								{
									Name: "id",
									Type: "serial",
									Pk:   true,
								},
								{
									Name: "name",
									Type: "varchar(255)",
								},
							},
						},
						&migrations.OpRenameTable{
							From: "items",
							To:   "products",
						},
						&migrations.OpDropTable{
							Name: "products",
						},
					},
				},
			},
			afterStart: func(t *testing.T, db *sql.DB, schema string) {
				// OpDropTable drops tables on migration completion, so the table
				// created by OpCreateTable is present after migration start but has
				// been soft-deleted (renamed).
				TableMustExist(t, db, schema, migrations.DeletionName(migrations.MigrationScopeFor("01_multi_operation"), "items"))

				// There is no view for the "items" table in the new schema
				ViewMustNotExist(t, db, schema, "01_multi_operation", "items")

				// There is no view for the "products" table in the new schema
				ViewMustNotExist(t, db, schema, "01_multi_operation", "products")
			},
			afterRollback: func(t *testing.T, db *sql.DB, schema string) {
				// The table is not present
				TableMustNotExist(t, db, schema, "items")
			},
			afterComplete: func(t *testing.T, db *sql.DB, schema string) {
				// The table is not present
				TableMustNotExist(t, db, schema, "items")

				// There is no view for the "items" table in the new schema
				ViewMustNotExist(t, db, schema, "01_multi_operation", "items")

				// There is no view for the "products" table in the new schema
				ViewMustNotExist(t, db, schema, "01_multi_operation", "products")
			},
		},
		{
			name: "create table, drop table, create table",
			migrations: []migrations.Migration{
				{
					Name: "01_multi_operation",
					Operations: migrations.Operations{
						&migrations.OpCreateTable{
							Name: "items",
							Columns: []migrations.Column{
								{
									Name: "id",
									Type: "serial",
									Pk:   true,
								},
								{
									Name: "name",
									Type: "varchar(255)",
								},
							},
						},
						&migrations.OpDropTable{
							Name: "items",
						},
						&migrations.OpCreateTable{
							Name: "items",
							Columns: []migrations.Column{
								{
									Name: "id",
									Type: "serial",
									Pk:   true,
								},
								{
									Name: "name",
									Type: "varchar(255)",
								},
								{
									Name: "description",
									Type: "varchar(255)",
								},
							},
						},
					},
				},
			},
			afterStart: func(t *testing.T, db *sql.DB, schema string) {
				// Can insert into the items table, and it has a description column
				MustInsert(t, db, schema, "01_multi_operation", "items", map[string]string{
					"name":        "apples",
					"description": "amazing",
				})
			},
			afterRollback: func(t *testing.T, db *sql.DB, schema string) {
				// There are no tables, either original or soft-deleted
				TableMustNotExist(t, db, schema, "items")
				TableMustNotExist(t, db, schema, migrations.DeletionName(migrations.MigrationScopeFor("01_multi_operation"), "items"))
			},
			afterComplete: func(t *testing.T, db *sql.DB, schema string) {
				// Can insert into the items table, and it has a description column
				MustInsert(t, db, schema, "01_multi_operation", "items", map[string]string{
					"name":        "bananas",
					"description": "brilliant",
				})
			},
		},
		{
			name: "drop table, create table",
			migrations: []migrations.Migration{
				{
					Name: "01_create_table",
					Operations: migrations.Operations{
						&migrations.OpCreateTable{
							Name: "items",
							Columns: []migrations.Column{
								{
									Name: "id",
									Type: "serial",
									Pk:   true,
								},
								{
									Name: "name",
									Type: "varchar(255)",
								},
							},
						},
					},
				},
				{
					Name: "02_multi_operation",
					Operations: migrations.Operations{
						&migrations.OpDropTable{
							Name: "items",
						},
						&migrations.OpCreateTable{
							Name: "items",
							Columns: []migrations.Column{
								{
									Name: "id",
									Type: "serial",
									Pk:   true,
								},
								{
									Name: "name",
									Type: "varchar(255)",
								},
								{
									Name: "description",
									Type: "varchar(255)",
								},
							},
						},
					},
				},
			},
			afterStart: func(t *testing.T, db *sql.DB, schema string) {
				// Can insert into the items table, and it has a description column
				MustInsert(t, db, schema, "02_multi_operation", "items", map[string]string{
					"name":        "apples",
					"description": "amazing",
				})
			},
			afterRollback: func(t *testing.T, db *sql.DB, schema string) {
				// The table from the second migration has been dropped (the one
				// without the description column)
				MustNotInsert(t, db, schema, "01_create_table", "items", map[string]string{
					"name":        "apples",
					"description": "amazing",
				}, testutils.UndefinedColumnErrorCode)

				// The table from the first migration remains (the one with the
				// description column)
				MustInsert(t, db, schema, "01_create_table", "items", map[string]string{
					"name": "apples",
				})

				// There is no soft-deleted version of thte items table
				TableMustNotExist(t, db, schema, migrations.DeletionName(migrations.MigrationScopeFor("02_multi_operation"), "items"))
			},
			afterComplete: func(t *testing.T, db *sql.DB, schema string) {
				// Can insert into the items table, and it has a description column
				MustInsert(t, db, schema, "02_multi_operation", "items", map[string]string{
					"name":        "bananas",
					"description": "brilliant",
				})
			},
		},
		{
			name: "drop table, rename another table into its name",
			migrations: []migrations.Migration{
				{
					Name: "01_create_tables",
					Operations: migrations.Operations{
						&migrations.OpCreateTable{
							Name: "items",
							Columns: []migrations.Column{
								{Name: "id", Type: "serial", Pk: true},
								{Name: "name", Type: "varchar(255)"},
							},
						},
						&migrations.OpCreateTable{
							Name: "new_items",
							Columns: []migrations.Column{
								{Name: "id", Type: "serial", Pk: true},
								{Name: "name", Type: "varchar(255)"},
								{Name: "description", Type: "varchar(255)"},
							},
						},
					},
				},
				{
					Name: "02_multi_operation",
					Operations: migrations.Operations{
						&migrations.OpDropTable{
							Name: "items",
						},
						&migrations.OpRenameTable{
							From: "new_items",
							To:   "items",
						},
					},
				},
			},
			afterStart: func(t *testing.T, db *sql.DB, schema string) {
				// The new version's items table is the renamed one
				MustInsert(t, db, schema, "02_multi_operation", "items", map[string]string{
					"name":        "apples",
					"description": "amazing",
				})

				// The old version still sees both tables under their own names
				MustInsert(t, db, schema, "01_create_tables", "items", map[string]string{
					"name": "apples",
				})
				MustInsert(t, db, schema, "01_create_tables", "new_items", map[string]string{
					"name":        "apples",
					"description": "amazing",
				})
			},
			afterRollback: func(t *testing.T, db *sql.DB, schema string) {
				// The dropped items table is back (the one without the
				// description column)
				MustNotInsert(t, db, schema, "01_create_tables", "items", map[string]string{
					"name":        "apples",
					"description": "amazing",
				}, testutils.UndefinedColumnErrorCode)
				MustInsert(t, db, schema, "01_create_tables", "items", map[string]string{
					"name": "apples",
				})

				// The renamed table has its original name back
				MustInsert(t, db, schema, "01_create_tables", "new_items", map[string]string{
					"name":        "apples",
					"description": "amazing",
				})

				// There is no soft-deleted version of the items table
				TableMustNotExist(t, db, schema, migrations.DeletionName(migrations.MigrationScopeFor("02_multi_operation"), "items"))
			},
			afterComplete: func(t *testing.T, db *sql.DB, schema string) {
				// items is the renamed table, with its description column
				MustInsert(t, db, schema, "02_multi_operation", "items", map[string]string{
					"name":        "bananas",
					"description": "brilliant",
				})

				// The original items table is gone
				TableMustNotExist(t, db, schema, migrations.DeletionName(migrations.MigrationScopeFor("02_multi_operation"), "items"))
			},
		},
	})
}

func TestDropTableValidationInMultiOperationMigrations(t *testing.T) {
	t.Parallel()

	ExecuteTests(t, TestCases{
		{
			name: "drop table, drop table fails to validate",
			migrations: []migrations.Migration{
				{
					Name: "01_create_table",
					Operations: migrations.Operations{
						&migrations.OpCreateTable{
							Name: "items",
							Columns: []migrations.Column{
								{
									Name: "id",
									Type: "serial",
									Pk:   true,
								},
								{
									Name: "name",
									Type: "varchar(255)",
								},
							},
						},
					},
				},
				{
					Name: "02_multi_operation",
					Operations: migrations.Operations{
						&migrations.OpDropTable{
							Name: "items",
						},
						&migrations.OpDropTable{
							Name: "items",
						},
					},
				},
			},
			wantStartErr: migrations.TableDoesNotExistError{Name: "items"},
		},
		{
			name: "drop table, rename table fails to validate",
			migrations: []migrations.Migration{
				{
					Name: "01_create_table",
					Operations: migrations.Operations{
						&migrations.OpCreateTable{
							Name: "items",
							Columns: []migrations.Column{
								{
									Name: "id",
									Type: "serial",
									Pk:   true,
								},
								{
									Name: "name",
									Type: "varchar(255)",
								},
							},
						},
					},
				},
				{
					Name: "02_multi_operation",
					Operations: migrations.Operations{
						&migrations.OpDropTable{
							Name: "items",
						},
						&migrations.OpRenameTable{
							From: "items",
							To:   "products",
						},
					},
				},
			},
			wantStartErr: migrations.TableDoesNotExistError{Name: "items"},
		},
	})
}
