// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"encoding/json"
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
					"id":     {Name: "id", Type: "integer"},
					"email":  {Name: "email", Type: "text", Nullable: true, Comment: "contact address"},
					"score":  {Name: "score", Type: "integer", Nullable: true, Default: ptrString("0")},
					"org_id": {Name: "org_id", Type: "integer", Nullable: true},
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
				ForeignKeys: map[string]*schema.ForeignKey{
					"users_org_id_fkey": {
						Name:              "users_org_id_fkey",
						Columns:           []string{"org_id"},
						ReferencedTable:   "orgs",
						ReferencedColumns: []string{"id"},
						OnDelete:          "CASCADE",
						OnUpdate:          "NO ACTION",
						MatchType:         "SIMPLE",
					},
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
		// OnComplete: a bare raw-SQL op IsIsolated and would be rejected by
		// Migration.Validate when combined with other inverse operations
		// (e.g. inside a drop_table inverse); onComplete forbids down.
		assert.True(t, raw.OnComplete)
		assert.Empty(t, raw.Down)
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

	t.Run("alter column adding a constraint inverts to a constraint drop", func(t *testing.T) {
		inv := singleInverse(t, &OpAlterColumn{
			Table:  "users",
			Column: "score",
			Unique: &UniqueConstraint{Name: "score_unique"},
			Up:     "score",
			Down:   "score",
		}, pre)
		drop, ok := inv.(*OpDropMultiColumnConstraint)
		require.True(t, ok)
		assert.Equal(t, "users", drop.Table)
		assert.Equal(t, "score_unique", drop.Name)
		assert.Equal(t, MultiColumnUpSQL{"score": "score"}, drop.Up)
		assert.Equal(t, MultiColumnDownSQL{"score": "score"}, drop.Down)
	})

	t.Run("alter column combining a constraint with other changes refuses", func(t *testing.T) {
		newType := "bigint"
		_, err := (&OpAlterColumn{
			Table:  "users",
			Column: "score",
			Type:   &newType,
			Unique: &UniqueConstraint{Name: "score_unique"},
			Up:     "score::bigint",
			Down:   "score::integer",
		}).Invert(pre)
		require.ErrorContains(t, err, "combines a constraint")
	})

	t.Run("create constraint inverts to a constraint drop with swapped expressions", func(t *testing.T) {
		inv := singleInverse(t, &OpCreateConstraint{
			Table:   "users",
			Name:    "email_score_unique",
			Type:    OpCreateConstraintTypeUnique,
			Columns: []string{"email", "score"},
			Up:      MultiColumnUpSQL{"email": "lower(email)", "score": "score"},
			Down:    MultiColumnDownSQL{"email": "email", "score": "score"},
		}, pre)
		drop, ok := inv.(*OpDropMultiColumnConstraint)
		require.True(t, ok)
		assert.Equal(t, "email_score_unique", drop.Name)
		assert.Equal(t, MultiColumnUpSQL{"email": "email", "score": "score"}, drop.Up,
			"the drop's up is the create's down")
		assert.Equal(t, MultiColumnDownSQL{"email": "lower(email)", "score": "score"}, drop.Down,
			"the drop's down is the create's up")
	})

	t.Run("create primary key constraint refuses", func(t *testing.T) {
		_, err := (&OpCreateConstraint{
			Table:   "users",
			Name:    "users_pk",
			Type:    OpCreateConstraintTypePrimaryKey,
			Columns: []string{"id"},
			Up:      MultiColumnUpSQL{"id": "id"},
			Down:    MultiColumnDownSQL{"id": "id"},
		}).Invert(pre)
		require.ErrorContains(t, err, "primary key")
	})

	t.Run("drop check constraint inverts to its recreation from the snapshot", func(t *testing.T) {
		inv := singleInverse(t, &OpDropMultiColumnConstraint{
			Table: "users",
			Name:  "score_positive",
			Up:    MultiColumnUpSQL{"score": "greatest(score, 0)"},
			Down:  MultiColumnDownSQL{"score": "score"},
		}, pre)
		create, ok := inv.(*OpCreateConstraint)
		require.True(t, ok)
		assert.Equal(t, OpCreateConstraintTypeCheck, create.Type)
		assert.Equal(t, "score_positive", create.Name)
		assert.Equal(t, []string{"score"}, create.Columns)
		require.NotNil(t, create.Check)
		// pg_get_constraintdef double-wraps; one layer of parens may remain
		// and is semantically harmless inside CHECK (...).
		assert.Equal(t, "(score >= 0)", *create.Check)
		assert.Equal(t, MultiColumnUpSQL{"score": "score"}, create.Up,
			"the create's up is the drop's down")
		assert.Equal(t, MultiColumnDownSQL{"score": "greatest(score, 0)"}, create.Down,
			"the create's down is the drop's up")
	})

	t.Run("drop unique constraint without up falls back to identity", func(t *testing.T) {
		inv := singleInverse(t, &OpDropMultiColumnConstraint{
			Table: "users",
			Name:  "users_email_key",
			Down:  MultiColumnDownSQL{"email": "email"},
		}, pre)
		create, ok := inv.(*OpCreateConstraint)
		require.True(t, ok)
		assert.Equal(t, OpCreateConstraintTypeUnique, create.Type)
		assert.Equal(t, []string{"email"}, create.Columns)
		assert.Equal(t, MultiColumnUpSQL{"email": "email"}, create.Up)
		assert.Equal(t, MultiColumnDownSQL{"email": `"email"`}, create.Down,
			"missing drop up falls back to the identity projection")
	})

	t.Run("drop foreign key inverts to its recreation from the snapshot", func(t *testing.T) {
		inv := singleInverse(t, &OpDropMultiColumnConstraint{
			Table: "users",
			Name:  "users_org_id_fkey",
			Up:    MultiColumnUpSQL{"org_id": "org_id"},
			Down:  MultiColumnDownSQL{"org_id": "org_id"},
		}, pre)
		create, ok := inv.(*OpCreateConstraint)
		require.True(t, ok)
		assert.Equal(t, OpCreateConstraintTypeForeignKey, create.Type)
		assert.Equal(t, []string{"org_id"}, create.Columns)
		require.NotNil(t, create.References)
		assert.Equal(t, "orgs", create.References.Table)
		assert.Equal(t, []string{"id"}, create.References.Columns)
		assert.Equal(t, ForeignKeyActionCASCADE, create.References.OnDelete)
		assert.Equal(t, ForeignKeyActionNOACTION, create.References.OnUpdate)
		assert.Empty(t, create.References.MatchType, "SIMPLE match is the default and stays unset")
	})

	t.Run("drop of an unknown constraint refuses", func(t *testing.T) {
		_, err := (&OpDropMultiColumnConstraint{
			Table: "users",
			Name:  "no_such_constraint",
			Down:  MultiColumnDownSQL{"score": "score"},
		}).Invert(pre)
		require.ErrorContains(t, err, "not found")
	})

	t.Run("deprecated drop_constraint inverts via the snapshot for a single column", func(t *testing.T) {
		inv := singleInverse(t, &OpDropConstraint{
			Table: "users",
			Name:  "score_positive",
			Up:    "score",
			Down:  "greatest(score, 0)",
		}, pre)
		create, ok := inv.(*OpCreateConstraint)
		require.True(t, ok)
		assert.Equal(t, OpCreateConstraintTypeCheck, create.Type)
		assert.Equal(t, MultiColumnUpSQL{"score": "greatest(score, 0)"}, create.Up)
		assert.Equal(t, MultiColumnDownSQL{"score": "score"}, create.Down)
	})

	t.Run("drop table recreates definition and indexes from snapshot", func(t *testing.T) {
		ops, err := (&OpDropTable{Name: "users"}).Invert(pre)
		require.NoError(t, err)
		require.Len(t, ops, 2, "create_table plus the non-constraint index")

		create, ok := ops[0].(*OpCreateTable)
		require.True(t, ok)
		assert.Equal(t, "users", create.Name)
		require.Len(t, create.Columns, 4)
		byName := map[string]Column{}
		for _, c := range create.Columns {
			byName[c.Name] = c
		}
		assert.True(t, byName["id"].Pk)
		assert.Equal(t, "text", byName["email"].Type)
		require.NotNil(t, byName["score"].Default)
		assert.Equal(t, "0", *byName["score"].Default)

		var checkC, uniqueC, fkC *Constraint
		for i := range create.Constraints {
			switch create.Constraints[i].Type {
			case ConstraintTypeCheck:
				checkC = &create.Constraints[i]
			case ConstraintTypeUnique:
				uniqueC = &create.Constraints[i]
			case ConstraintTypeForeignKey:
				fkC = &create.Constraints[i]
			}
		}
		require.NotNil(t, checkC)
		// pg_get_constraintdef double-wraps; one layer of parens may remain
		// and is semantically harmless inside CHECK (...).
		assert.Contains(t, checkC.Check, "score >= 0")
		assert.NotContains(t, checkC.Check, "CHECK")
		require.NotNil(t, uniqueC)
		assert.Equal(t, []string{"email"}, uniqueC.Columns)
		require.NotNil(t, fkC)
		assert.Equal(t, []string{"org_id"}, fkC.Columns)
		require.NotNil(t, fkC.References)
		assert.Equal(t, "orgs", fkC.References.Table)

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

	t.Run("drop_table inverse combines no isolated operations", func(t *testing.T) {
		// The synthesized index restore must be onComplete raw SQL: a bare
		// raw-SQL op IsIsolated, and Migration.Validate rejects isolated ops
		// combined with the create_table — making every drop_table with a
		// secondary index uninvertible mid-train.
		mig := &Migration{
			Name:       "03_drop_users",
			Operations: Operations{&OpDropTable{Name: "users"}},
		}
		inv, err := mig.Invert(ctx, invertFixtureSchema())
		require.NoError(t, err)
		require.Greater(t, len(inv.Operations), 1)
		for _, op := range inv.Operations {
			if iso, ok := op.(IsolatedOperation); ok {
				assert.False(t, iso.IsIsolated(), "synthesized inverse op %q is isolated", OperationName(op))
			}
		}
	})
}

func TestInvertSegmentFidelity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("deleted columns stay deleted across pre-state snapshots", func(t *testing.T) {
		// drop_column email, then drop_table users: the table recreation in
		// the drop_table inverse must NOT resurrect email — its Deleted
		// marker is json:"-" and a naive JSON deep-copy of the work schema
		// silently loses it, synthesizing an add_column collision mid-train.
		segment := []*Migration{
			{Name: "01_drop_email", Operations: Operations{
				&OpDropColumn{Table: "users", Column: "email", Down: "''"},
			}},
			{Name: "02_drop_users", Operations: Operations{
				&OpDropTable{Name: "users"},
			}},
		}
		inverses, err := InvertSegment(ctx, segment, invertFixtureSchema())
		require.NoError(t, err)
		require.Len(t, inverses, 2)

		create, ok := inverses[0].Operations[0].(*OpCreateTable)
		require.True(t, ok, "newest-first: the drop_table inverse comes first")
		for _, c := range create.Columns {
			assert.NotEqual(t, "email", c.Name, "deleted column resurrected in the recreated table")
		}
	})

	t.Run("raw SQL taints pre-states for snapshot-reading inverses", func(t *testing.T) {
		segment := []*Migration{
			{Name: "01_raw", Operations: Operations{
				&OpRawSQL{Up: "ALTER TABLE users ADD COLUMN extra text", Down: "ALTER TABLE users DROP COLUMN extra"},
			}},
			{Name: "02_drop_email", Operations: Operations{
				&OpDropColumn{Table: "users", Column: "email", Down: "''"},
			}},
		}
		_, err := InvertSegment(ctx, segment, invertFixtureSchema())
		require.ErrorContains(t, err, "untrustworthy")
	})

	t.Run("raw SQL does not taint inverses that ignore pre-state", func(t *testing.T) {
		segment := []*Migration{
			{Name: "01_raw", Operations: Operations{
				&OpRawSQL{Up: "UPDATE users SET score = 0 WHERE score IS NULL", Down: "SELECT 1"},
			}},
			{Name: "02_rename", Operations: Operations{
				&OpRenameColumn{Table: "users", From: "email", To: "contact"},
			}},
		}
		inverses, err := InvertSegment(ctx, segment, invertFixtureSchema())
		require.NoError(t, err)
		require.Len(t, inverses, 2)
	})

	t.Run("replaying a typed op against raw-SQL-created objects errors instead of panicking", func(t *testing.T) {
		segment := []*Migration{
			{Name: "01_raw_create", Operations: Operations{
				&OpRawSQL{Up: "CREATE TABLE widgets (id int)", Down: "DROP TABLE widgets"},
			}},
			{Name: "02_rename_widgets", Operations: Operations{
				&OpRenameColumn{Table: "widgets", From: "id", To: "widget_id"},
			}},
		}
		_, err := InvertSegment(ctx, segment, invertFixtureSchema())
		require.Error(t, err)
		require.ErrorContains(t, err, "replay")
	})
}

func TestInvertSequenceAndIdentityColumns(t *testing.T) {
	t.Parallel()

	serialFixture := func() *schema.Schema {
		return &schema.Schema{
			Name: "public",
			Tables: map[string]*schema.Table{
				"orders": {
					Name: "orders",
					Columns: map[string]*schema.Column{
						"id":   {Name: "id", Type: "integer", Default: ptrString("nextval('orders_id_seq'::regclass)")},
						"note": {Name: "note", Type: "text", Nullable: true},
					},
					PrimaryKey: []string{"id"},
				},
			},
		}
	}

	t.Run("drop_table recreates serial columns via the serial pseudo-type", func(t *testing.T) {
		// The owned sequence was dropped with the table; carrying the
		// recorded nextval default verbatim references a nonexistent
		// regclass and the CREATE TABLE fails mid-inverse-train.
		ops, err := (&OpDropTable{Name: "orders"}).Invert(serialFixture())
		require.NoError(t, err)
		create, ok := ops[0].(*OpCreateTable)
		require.True(t, ok)
		byName := map[string]Column{}
		for _, c := range create.Columns {
			byName[c.Name] = c
		}
		assert.Equal(t, "serial", byName["id"].Type)
		assert.Nil(t, byName["id"].Default)
	})

	t.Run("drop_column of a serial column re-adds via the serial pseudo-type", func(t *testing.T) {
		inv, err := (&OpDropColumn{Table: "orders", Column: "id", Down: "0"}).Invert(serialFixture())
		require.NoError(t, err)
		add, ok := inv[0].(*OpAddColumn)
		require.True(t, ok)
		assert.Equal(t, "serial", add.Column.Type)
		assert.Nil(t, add.Column.Default)
	})

	t.Run("shared (non-owned) sequence defaults are carried verbatim", func(t *testing.T) {
		fix := serialFixture()
		fix.Tables["orders"].Columns["id"].Default = ptrString("nextval('global_seq'::regclass)")
		ops, err := (&OpDropTable{Name: "orders"}).Invert(fix)
		require.NoError(t, err)
		create, ok := ops[0].(*OpCreateTable)
		require.True(t, ok)
		for _, c := range create.Columns {
			if c.Name == "id" {
				require.NotNil(t, c.Default)
				assert.Contains(t, *c.Default, "global_seq")
			}
		}
	})

	t.Run("identity columns refuse inversion loudly", func(t *testing.T) {
		fix := serialFixture()
		fix.Tables["orders"].Columns["id"].Identity = "a"
		fix.Tables["orders"].Columns["id"].Default = nil

		_, err := (&OpDropTable{Name: "orders"}).Invert(fix)
		require.ErrorContains(t, err, "IDENTITY")

		_, err = (&OpDropColumn{Table: "orders", Column: "id", Down: "0"}).Invert(fix)
		require.ErrorContains(t, err, "IDENTITY")
	})
}

func TestInvertAddColumnDefaultFallback(t *testing.T) {
	t.Parallel()
	pre := invertFixtureSchema()

	t.Run("NOT NULL column with default and no up falls back to the default", func(t *testing.T) {
		inv := singleInverse(t, &OpAddColumn{
			Table:  "users",
			Column: Column{Name: "flag", Type: "boolean", Nullable: false, Default: ptrString("false")},
		}, pre)
		drop, ok := inv.(*OpDropColumn)
		require.True(t, ok)
		assert.Equal(t, "false", drop.Down)
	})
}

func TestRevertOfSurvivesHistoryRoundTrip(t *testing.T) {
	t.Parallel()

	// The sealed-revert orchestrator's crash recovery identifies leftover
	// inverse rows in history by RevertOf; it must survive the stored-JSON →
	// RawMigration → ParseMigration round-trip or the resume becomes a
	// double-inverse re-execution.
	stored, err := json.Marshal(&Migration{
		Name:       "revert_01_drop_email",
		Operations: Operations{&OpRawSQL{Up: "SELECT 1", OnComplete: true}},
		RevertOf:   "01_drop_email",
	})
	require.NoError(t, err)

	var raw RawMigration
	require.NoError(t, json.Unmarshal(stored, &raw))
	raw.Name = "revert_01_drop_email"

	parsed, err := ParseMigration(&raw)
	require.NoError(t, err)
	assert.Equal(t, "01_drop_email", parsed.RevertOf)
}
