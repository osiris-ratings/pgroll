// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"testing"

	"github.com/oapi-codegen/nullable"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/pkg/schema"
)

func invertFixtureSchema() *schema.Schema {
	return &schema.Schema{
		Name: "public",
		Tables: map[string]*schema.Table{
			"users": {
				Name: "users",
				Columns: map[string]*schema.Column{
					"id":    {Name: "id", Type: "integer"},
					"email": {Name: "email", Type: "text", Nullable: true, Comment: "contact address"},
					"score": {Name: "score", Type: "integer", Nullable: true, Default: ptrString("0")},
				},
				PrimaryKey: []string{"id"},
				Indexes: map[string]*schema.Index{
					"idx_users_email": {
						Name:       "idx_users_email",
						Definition: `CREATE INDEX idx_users_email ON public.users USING btree (email)`,
					},
					"users_pkey": {
						Name:       "users_pkey",
						Unique:     true,
						Definition: `CREATE UNIQUE INDEX users_pkey ON public.users USING btree (id)`,
					},
				},
				CheckConstraints: map[string]*schema.CheckConstraint{
					"score_positive": {
						Name:       "score_positive",
						Columns:    []string{"score"},
						Definition: "CHECK ((score >= 0))",
					},
				},
				UniqueConstraints: map[string]*schema.UniqueConstraint{
					"users_email_key": {Name: "users_email_key", Columns: []string{"email"}},
				},
			},
		},
	}
}

func ptrString(s string) *string { return &s }

func singleInverse(t *testing.T, op Invertible, pre *schema.Schema) Operation {
	t.Helper()
	ops, err := op.Invert(pre)
	require.NoError(t, err)
	require.Len(t, ops, 1)
	return ops[0]
}

func TestInvertOperations(t *testing.T) {
	t.Parallel()
	pre := invertFixtureSchema()

	t.Run("rename column swaps direction", func(t *testing.T) {
		inv := singleInverse(t, &OpRenameColumn{Table: "users", From: "email", To: "contact"}, pre)
		assert.Equal(t, &OpRenameColumn{Table: "users", From: "contact", To: "email"}, inv)
	})

	t.Run("rename table swaps direction", func(t *testing.T) {
		inv := singleInverse(t, &OpRenameTable{From: "users", To: "people"}, pre)
		assert.Equal(t, &OpRenameTable{From: "people", To: "users"}, inv)
	})

	t.Run("create table inverts to drop", func(t *testing.T) {
		inv := singleInverse(t, &OpCreateTable{Name: "events"}, pre)
		assert.Equal(t, &OpDropTable{Name: "events"}, inv)
	})

	t.Run("create index inverts to drop", func(t *testing.T) {
		inv := singleInverse(t, &OpCreateIndex{Name: "idx_new", Table: "users"}, pre)
		assert.Equal(t, &OpDropIndex{Name: "idx_new"}, inv)
	})

	t.Run("add column inverts to drop with up as down", func(t *testing.T) {
		inv := singleInverse(t, &OpAddColumn{
			Table:  "users",
			Up:     "'unknown'",
			Column: Column{Name: "nick", Type: "text", Nullable: false},
		}, pre)
		assert.Equal(t, &OpDropColumn{Table: "users", Column: "nick", Down: "'unknown'"}, inv)
	})

	t.Run("add nullable column without up inverts with NULL down", func(t *testing.T) {
		inv := singleInverse(t, &OpAddColumn{
			Table:  "users",
			Column: Column{Name: "nick", Type: "text", Nullable: true},
		}, pre)
		assert.Equal(t, &OpDropColumn{Table: "users", Column: "nick", Down: "NULL"}, inv)
	})

	t.Run("add NOT NULL column without up refuses", func(t *testing.T) {
		_, err := (&OpAddColumn{
			Table:  "users",
			Column: Column{Name: "nick", Type: "text", Nullable: false},
		}).Invert(pre)
		require.Error(t, err)
	})

	t.Run("raw SQL inverts to deferred counter-statement", func(t *testing.T) {
		inv := singleInverse(t, &OpRawSQL{Up: "CREATE TYPE t AS ENUM ('a')", Down: "DROP TYPE t"}, pre)
		assert.Equal(t, &OpRawSQL{Up: "DROP TYPE t", OnComplete: true}, inv)
	})

	t.Run("raw SQL without down refuses", func(t *testing.T) {
		_, err := (&OpRawSQL{Up: "SELECT 1"}).Invert(pre)
		require.Error(t, err)
	})

	t.Run("drop index recreates from snapshot definition", func(t *testing.T) {
		inv := singleInverse(t, &OpDropIndex{Name: "idx_users_email"}, pre)
		raw, ok := inv.(*OpRawSQL)
		require.True(t, ok)
		assert.Contains(t, raw.Up, "CREATE INDEX idx_users_email")
		assert.Contains(t, raw.Down, "DROP INDEX")
	})

	t.Run("drop unknown index refuses", func(t *testing.T) {
		_, err := (&OpDropIndex{Name: "no_such_index"}).Invert(pre)
		require.Error(t, err)
	})

	t.Run("drop column re-adds from snapshot definition", func(t *testing.T) {
		inv := singleInverse(t, &OpDropColumn{Table: "users", Column: "email", Down: "'unknown@example.com'"}, pre)
		add, ok := inv.(*OpAddColumn)
		require.True(t, ok)
		assert.Equal(t, "users", add.Table)
		assert.Equal(t, "email", add.Column.Name)
		assert.Equal(t, "text", add.Column.Type)
		assert.True(t, add.Column.Nullable)
		require.NotNil(t, add.Column.Comment)
		assert.Equal(t, "contact address", *add.Column.Comment)
		assert.Equal(t, "'unknown@example.com'", add.Up)
	})

	t.Run("drop column without down refuses", func(t *testing.T) {
		_, err := (&OpDropColumn{Table: "users", Column: "email"}).Invert(pre)
		require.Error(t, err)
	})

	t.Run("alter column type restores prior type with swapped expressions", func(t *testing.T) {
		newType := "bigint"
		inv := singleInverse(t, &OpAlterColumn{
			Table:  "users",
			Column: "score",
			Type:   &newType,
			Up:     "score::bigint",
			Down:   "score::integer",
		}, pre)
		alter, ok := inv.(*OpAlterColumn)
		require.True(t, ok)
		require.NotNil(t, alter.Type)
		assert.Equal(t, "integer", *alter.Type)
		assert.Equal(t, "score::integer", alter.Up)
		assert.Equal(t, "score::bigint", alter.Down)
	})

	t.Run("alter column nullable restores prior nullability", func(t *testing.T) {
		notNull := false
		inv := singleInverse(t, &OpAlterColumn{
			Table:    "users",
			Column:   "score",
			Nullable: &notNull,
			Up:       "coalesce(score, 0)",
			Down:     "score",
		}, pre)
		alter, ok := inv.(*OpAlterColumn)
		require.True(t, ok)
		require.NotNil(t, alter.Nullable)
		assert.True(t, *alter.Nullable, "prior state was nullable")
	})

	t.Run("alter column default restores prior default", func(t *testing.T) {
		inv := singleInverse(t, &OpAlterColumn{
			Table:   "users",
			Column:  "score",
			Default: nullable.NewNullableWithValue("42"),
			Up:      "score",
			Down:    "score",
		}, pre)
		alter, ok := inv.(*OpAlterColumn)
		require.True(t, ok)
		require.True(t, alter.Default.IsSpecified())
		prior, err := alter.Default.Get()
		require.NoError(t, err)
		assert.Equal(t, "0", prior)
	})

	t.Run("alter column adding a constraint refuses", func(t *testing.T) {
		_, err := (&OpAlterColumn{
			Table:  "users",
			Column: "score",
			Unique: &UniqueConstraint{Name: "score_unique"},
			Up:     "score",
			Down:   "score",
		}).Invert(pre)
		require.ErrorContains(t, err, "constraint")
	})

	t.Run("drop table recreates definition and indexes from snapshot", func(t *testing.T) {
		ops, err := (&OpDropTable{Name: "users"}).Invert(pre)
		require.NoError(t, err)
		require.Len(t, ops, 2, "create_table plus the non-constraint index")

		create, ok := ops[0].(*OpCreateTable)
		require.True(t, ok)
		assert.Equal(t, "users", create.Name)
		require.Len(t, create.Columns, 3)
		byName := map[string]Column{}
		for _, c := range create.Columns {
			byName[c.Name] = c
		}
		assert.True(t, byName["id"].Pk)
		assert.Equal(t, "text", byName["email"].Type)
		require.NotNil(t, byName["score"].Default)
		assert.Equal(t, "0", *byName["score"].Default)

		var checkC, uniqueC *Constraint
		for i := range create.Constraints {
			switch create.Constraints[i].Type {
			case ConstraintTypeCheck:
				checkC = &create.Constraints[i]
			case ConstraintTypeUnique:
				uniqueC = &create.Constraints[i]
			}
		}
		require.NotNil(t, checkC)
		// pg_get_constraintdef double-wraps; one layer of parens may remain
		// and is semantically harmless inside CHECK (...).
		assert.Contains(t, checkC.Check, "score >= 0")
		assert.NotContains(t, checkC.Check, "CHECK")
		require.NotNil(t, uniqueC)
		assert.Equal(t, []string{"email"}, uniqueC.Columns)

		idx, ok := ops[1].(*OpRawSQL)
		require.True(t, ok)
		assert.Contains(t, idx.Up, "CREATE INDEX idx_users_email")
	})

	t.Run("drop unknown table refuses", func(t *testing.T) {
		_, err := (&OpDropTable{Name: "no_such_table"}).Invert(pre)
		require.Error(t, err)
	})
}

func TestMigrationInvert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("operations invert in reverse order against threaded state", func(t *testing.T) {
		// rename email -> contact, then drop contact: inverting must
		// resolve the dropped column's definition under its post-rename
		// virtual name, and emit [add contact, rename contact -> email].
		mig := &Migration{
			Name: "01_rename_and_drop",
			Operations: Operations{
				&OpRenameColumn{Table: "users", From: "email", To: "contact"},
				&OpDropColumn{Table: "users", Column: "contact", Down: "''"},
			},
		}

		inv, err := mig.Invert(ctx, invertFixtureSchema())
		require.NoError(t, err)
		assert.Equal(t, "revert_01_rename_and_drop", inv.Name)
		assert.Equal(t, "01_rename_and_drop", inv.RevertOf)
		require.Len(t, inv.Operations, 2)

		add, ok := inv.Operations[0].(*OpAddColumn)
		require.True(t, ok)
		assert.Equal(t, "contact", add.Column.Name)
		assert.Equal(t, "text", add.Column.Type, "definition must come from the pre-drop (post-rename) state")

		ren, ok := inv.Operations[1].(*OpRenameColumn)
		require.True(t, ok)
		assert.Equal(t, "contact", ren.From)
		assert.Equal(t, "email", ren.To)
	})

	t.Run("non-invertible operation refuses with the op named", func(t *testing.T) {
		mig := &Migration{
			Name: "02_replica",
			Operations: Operations{
				&OpSetReplicaIdentity{Table: "users", Identity: ReplicaIdentity{Type: "FULL"}},
			},
		}
		_, err := mig.Invert(ctx, invertFixtureSchema())
		require.ErrorContains(t, err, "does not support inversion")
	})
}
