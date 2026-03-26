// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/schema"
)

func TestValidatePreconditions(t *testing.T) {
	t.Parallel()

	testSchema := &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id":    {Name: "id", Type: "integer"},
					"email": {Name: "email", Type: "text"},
					"name":  {Name: "name", Type: "varchar(255)"},
				},
				Indexes: map[string]*schema.Index{
					"users_email_idx": {Name: "users_email_idx"},
				},
				CheckConstraints: map[string]*schema.CheckConstraint{
					"users_email_check": {Name: "users_email_check"},
				},
				UniqueConstraints: map[string]*schema.UniqueConstraint{
					"users_email_unique": {Name: "users_email_unique"},
				},
				ForeignKeys:        map[string]*schema.ForeignKey{},
				ExcludeConstraints: map[string]*schema.ExcludeConstraint{},
			},
			"posts": {
				Name: "posts",
				Columns: map[string]*schema.Column{
					"id":      {Name: "id", Type: "integer"},
					"user_id": {Name: "user_id", Type: "integer"},
				},
				Indexes:           map[string]*schema.Index{},
				CheckConstraints:  map[string]*schema.CheckConstraint{},
				UniqueConstraints: map[string]*schema.UniqueConstraint{},
				ForeignKeys: map[string]*schema.ForeignKey{
					"posts_user_id_fkey": {Name: "posts_user_id_fkey"},
				},
				ExcludeConstraints: map[string]*schema.ExcludeConstraint{},
			},
		},
	}

	t.Run("empty preconditions pass", func(t *testing.T) {
		err := migrations.ValidatePreconditions(nil, testSchema)
		require.NoError(t, err)
	})

	t.Run("table_exists passes for existing table", func(t *testing.T) {
		tableName := "users"
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{TableExists: &tableName},
		}, testSchema)
		require.NoError(t, err)
	})

	t.Run("table_exists fails for missing table", func(t *testing.T) {
		tableName := "orders"
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{TableExists: &tableName},
		}, testSchema)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "orders")
		assert.Contains(t, err.Error(), "does not exist")
	})

	t.Run("table_not_exists passes for missing table", func(t *testing.T) {
		tableName := "orders"
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{TableNotExists: &tableName},
		}, testSchema)
		require.NoError(t, err)
	})

	t.Run("table_not_exists fails for existing table", func(t *testing.T) {
		tableName := "users"
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{TableNotExists: &tableName},
		}, testSchema)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "users")
		assert.Contains(t, err.Error(), "exists but should not")
	})

	t.Run("column_exists passes for existing column", func(t *testing.T) {
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{ColumnExists: &migrations.PreconditionColumnExists{
				Table:  "users",
				Column: "email",
			}},
		}, testSchema)
		require.NoError(t, err)
	})

	t.Run("column_exists with type check passes for matching type", func(t *testing.T) {
		colType := "text"
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{ColumnExists: &migrations.PreconditionColumnExists{
				Table:  "users",
				Column: "email",
				Type:   &colType,
			}},
		}, testSchema)
		require.NoError(t, err)
	})

	t.Run("column_exists with type check fails for wrong type", func(t *testing.T) {
		colType := "varchar(255)"
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{ColumnExists: &migrations.PreconditionColumnExists{
				Table:  "users",
				Column: "email",
				Type:   &colType,
			}},
		}, testSchema)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "text")
		assert.Contains(t, err.Error(), "varchar(255)")
	})

	t.Run("column_exists fails for missing column", func(t *testing.T) {
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{ColumnExists: &migrations.PreconditionColumnExists{
				Table:  "users",
				Column: "phone",
			}},
		}, testSchema)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "phone")
	})

	t.Run("column_exists fails for missing table", func(t *testing.T) {
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{ColumnExists: &migrations.PreconditionColumnExists{
				Table:  "orders",
				Column: "id",
			}},
		}, testSchema)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "orders")
	})

	t.Run("column_not_exists passes for missing column", func(t *testing.T) {
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{ColumnNotExists: &migrations.PreconditionColumnRef{
				Table:  "users",
				Column: "phone",
			}},
		}, testSchema)
		require.NoError(t, err)
	})

	t.Run("column_not_exists passes for missing table", func(t *testing.T) {
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{ColumnNotExists: &migrations.PreconditionColumnRef{
				Table:  "orders",
				Column: "id",
			}},
		}, testSchema)
		require.NoError(t, err)
	})

	t.Run("column_not_exists fails for existing column", func(t *testing.T) {
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{ColumnNotExists: &migrations.PreconditionColumnRef{
				Table:  "users",
				Column: "email",
			}},
		}, testSchema)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "email")
		assert.Contains(t, err.Error(), "exists")
	})

	t.Run("index_exists passes for existing index", func(t *testing.T) {
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{IndexExists: &migrations.PreconditionIndexRef{
				Table: "users",
				Index: "users_email_idx",
			}},
		}, testSchema)
		require.NoError(t, err)
	})

	t.Run("index_exists fails for missing index", func(t *testing.T) {
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{IndexExists: &migrations.PreconditionIndexRef{
				Table: "users",
				Index: "users_name_idx",
			}},
		}, testSchema)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "users_name_idx")
	})

	t.Run("constraint_exists passes for check constraint", func(t *testing.T) {
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{ConstraintExists: &migrations.PreconditionConstraintRef{
				Table:      "users",
				Constraint: "users_email_check",
			}},
		}, testSchema)
		require.NoError(t, err)
	})

	t.Run("constraint_exists passes for unique constraint", func(t *testing.T) {
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{ConstraintExists: &migrations.PreconditionConstraintRef{
				Table:      "users",
				Constraint: "users_email_unique",
			}},
		}, testSchema)
		require.NoError(t, err)
	})

	t.Run("constraint_exists passes for foreign key", func(t *testing.T) {
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{ConstraintExists: &migrations.PreconditionConstraintRef{
				Table:      "posts",
				Constraint: "posts_user_id_fkey",
			}},
		}, testSchema)
		require.NoError(t, err)
	})

	t.Run("constraint_exists fails for missing constraint", func(t *testing.T) {
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{ConstraintExists: &migrations.PreconditionConstraintRef{
				Table:      "users",
				Constraint: "nonexistent",
			}},
		}, testSchema)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nonexistent")
	})

	t.Run("multiple preconditions all must pass", func(t *testing.T) {
		tableName := "users"
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{TableExists: &tableName},
			{ColumnExists: &migrations.PreconditionColumnExists{
				Table:  "users",
				Column: "email",
			}},
			{IndexExists: &migrations.PreconditionIndexRef{
				Table: "users",
				Index: "users_email_idx",
			}},
		}, testSchema)
		require.NoError(t, err)
	})

	t.Run("multiple preconditions fail on first failure", func(t *testing.T) {
		tableName := "orders"
		err := migrations.ValidatePreconditions([]migrations.Precondition{
			{TableExists: &tableName}, // fails
			{ColumnExists: &migrations.PreconditionColumnExists{
				Table:  "users",
				Column: "email",
			}}, // would pass
		}, testSchema)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "orders")
	})
}
