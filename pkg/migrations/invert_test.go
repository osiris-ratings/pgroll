// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"testing"

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
				},
				Indexes: map[string]*schema.Index{
					"idx_users_email": {
						Name:       "idx_users_email",
						Definition: `CREATE INDEX idx_users_email ON public.users USING btree (email)`,
					},
				},
			},
		},
	}
}

func TestInvertOperations(t *testing.T) {
	t.Parallel()
	pre := invertFixtureSchema()

	t.Run("rename column swaps direction", func(t *testing.T) {
		inv, err := (&OpRenameColumn{Table: "users", From: "email", To: "contact"}).Invert(pre)
		require.NoError(t, err)
		assert.Equal(t, &OpRenameColumn{Table: "users", From: "contact", To: "email"}, inv)
	})

	t.Run("rename table swaps direction", func(t *testing.T) {
		inv, err := (&OpRenameTable{From: "users", To: "people"}).Invert(pre)
		require.NoError(t, err)
		assert.Equal(t, &OpRenameTable{From: "people", To: "users"}, inv)
	})

	t.Run("create table inverts to drop", func(t *testing.T) {
		inv, err := (&OpCreateTable{Name: "events"}).Invert(pre)
		require.NoError(t, err)
		assert.Equal(t, &OpDropTable{Name: "events"}, inv)
	})

	t.Run("create index inverts to drop", func(t *testing.T) {
		inv, err := (&OpCreateIndex{Name: "idx_new", Table: "users"}).Invert(pre)
		require.NoError(t, err)
		assert.Equal(t, &OpDropIndex{Name: "idx_new"}, inv)
	})

	t.Run("add column inverts to drop with up as down", func(t *testing.T) {
		inv, err := (&OpAddColumn{
			Table:  "users",
			Up:     "'unknown'",
			Column: Column{Name: "nick", Type: "text", Nullable: false},
		}).Invert(pre)
		require.NoError(t, err)
		assert.Equal(t, &OpDropColumn{Table: "users", Column: "nick", Down: "'unknown'"}, inv)
	})

	t.Run("add nullable column without up inverts with NULL down", func(t *testing.T) {
		inv, err := (&OpAddColumn{
			Table:  "users",
			Column: Column{Name: "nick", Type: "text", Nullable: true},
		}).Invert(pre)
		require.NoError(t, err)
		assert.Equal(t, &OpDropColumn{Table: "users", Column: "nick", Down: "NULL"}, inv)
	})

	t.Run("add NOT NULL column without up refuses", func(t *testing.T) {
		_, err := (&OpAddColumn{
			Table:  "users",
			Column: Column{Name: "nick", Type: "text", Nullable: false},
		}).Invert(pre)
		require.Error(t, err)
	})

	t.Run("raw SQL swaps up and down", func(t *testing.T) {
		inv, err := (&OpRawSQL{Up: "CREATE TYPE t AS ENUM ('a')", Down: "DROP TYPE t"}).Invert(pre)
		require.NoError(t, err)
		assert.Equal(t, &OpRawSQL{Up: "DROP TYPE t", Down: "CREATE TYPE t AS ENUM ('a')"}, inv)
	})

	t.Run("raw SQL without down refuses", func(t *testing.T) {
		_, err := (&OpRawSQL{Up: "SELECT 1"}).Invert(pre)
		require.Error(t, err)
	})

	t.Run("drop index recreates from snapshot definition", func(t *testing.T) {
		inv, err := (&OpDropIndex{Name: "idx_users_email"}).Invert(pre)
		require.NoError(t, err)
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
		inv, err := (&OpDropColumn{Table: "users", Column: "email", Down: "'unknown@example.com'"}).Invert(pre)
		require.NoError(t, err)
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
