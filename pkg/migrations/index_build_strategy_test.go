// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/schema"
)

// A table the migration itself created cannot be visible to any older
// transaction, so its indexes are built plainly; a pre-existing table keeps
// the concurrent build. The action ID carries the choice.
func TestIndexBuildStrategyFollowsTableAge(t *testing.T) {
	t.Parallel()

	newSchema := func() *schema.Schema {
		return &schema.Schema{
			Name: "public",
			Tables: map[string]*schema.Table{
				"fresh": {
					Name:               "fresh",
					CreatedInMigration: true,
					Columns:            map[string]*schema.Column{"id": {Name: "id", Type: "integer"}},
				},
				"old": {
					Name:    "old",
					Columns: map[string]*schema.Column{"id": {Name: "id", Type: "integer"}},
				},
			},
		}
	}

	t.Run("create_index", func(t *testing.T) {
		t.Parallel()
		for table, wantID := range map[string]string{
			"fresh": "create_index_fresh_idx",
			"old":   "create_index_concurrently_old_idx",
		} {
			op := &migrations.OpCreateIndex{
				Name:    "idx",
				Table:   table,
				Columns: []migrations.IndexField{{Column: "id"}},
			}
			res, err := op.Start(context.Background(), migrations.NewNoopLogger(), &db.FakeDB{}, newSchema())
			require.NoError(t, err)
			require.Len(t, res.Actions, 1)
			assert.Equal(t, wantID, res.Actions[0].ID())
		}
	})

	t.Run("add_column unique", func(t *testing.T) {
		t.Parallel()
		for table, wantPrefix := range map[string]string{
			"fresh": "create_unique_index__pgroll_uniq_email",
			"old":   "create_unique_index_concurrently__pgroll_uniq_email",
		} {
			op := &migrations.OpAddColumn{
				Table:  table,
				Column: migrations.Column{Name: "email", Type: "text", Unique: true, Nullable: true},
			}
			res, err := op.Start(context.Background(), migrations.NewNoopLogger(), &db.FakeDB{}, newSchema())
			require.NoError(t, err)
			ids := make([]string, 0, len(res.Actions))
			for _, a := range res.Actions {
				ids = append(ids, a.ID())
			}
			assert.True(t, hasPrefixed(ids, wantPrefix), "want an action with prefix %q in %v", wantPrefix, ids)
		}
	})
}

func hasPrefixed(ids []string, prefix string) bool {
	for _, id := range ids {
		if strings.HasPrefix(id, prefix) {
			return true
		}
	}
	return false
}
