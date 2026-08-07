// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xataio/pgroll/pkg/migrations"
)

func TestTopoSortMigrations(t *testing.T) {
	t.Parallel()

	t.Run("no dependencies preserves filesystem order", func(t *testing.T) {
		migs := []*migrations.RawMigration{
			rawMig("01_first"),
			rawMig("02_second"),
			rawMig("03_third"),
		}

		sorted, err := TopoSortMigrations(migs, nil, nil)
		require.NoError(t, err)
		require.Len(t, sorted, 3)
		assert.Equal(t, "01_first", sorted[0].Name)
		assert.Equal(t, "02_second", sorted[1].Name)
		assert.Equal(t, "03_third", sorted[2].Name)
	})

	t.Run("single dependency reorders correctly", func(t *testing.T) {
		// Filesystem order: A, B, C — but A depends on B
		migs := []*migrations.RawMigration{
			rawMigWithDeps("01_A", "02_B"),
			rawMig("02_B"),
			rawMig("03_C"),
		}

		sorted, err := TopoSortMigrations(migs, nil, nil)
		require.NoError(t, err)
		require.Len(t, sorted, 3)
		// B must come before A; C has no constraints
		assert.Equal(t, "02_B", sorted[0].Name)
		assert.Equal(t, "01_A", sorted[1].Name)
		assert.Equal(t, "03_C", sorted[2].Name)
	})

	t.Run("chain of dependencies", func(t *testing.T) {
		// C depends on B, B depends on A — filesystem order is reversed
		migs := []*migrations.RawMigration{
			rawMigWithDeps("03_C", "02_B"),
			rawMigWithDeps("02_B", "01_A"),
			rawMig("01_A"),
		}

		sorted, err := TopoSortMigrations(migs, nil, nil)
		require.NoError(t, err)
		require.Len(t, sorted, 3)
		assert.Equal(t, "01_A", sorted[0].Name)
		assert.Equal(t, "02_B", sorted[1].Name)
		assert.Equal(t, "03_C", sorted[2].Name)
	})

	t.Run("dependency on already-applied migration is satisfied", func(t *testing.T) {
		// B depends on A, but A is already applied
		applied := map[string]struct{}{
			"01_A": {},
		}
		migs := []*migrations.RawMigration{
			rawMigWithDeps("02_B", "01_A"),
			rawMig("03_C"),
		}

		sorted, err := TopoSortMigrations(migs, applied, nil)
		require.NoError(t, err)
		require.Len(t, sorted, 2)
		assert.Equal(t, "02_B", sorted[0].Name)
		assert.Equal(t, "03_C", sorted[1].Name)
	})

	t.Run("dependency on unknown migration returns error", func(t *testing.T) {
		migs := []*migrations.RawMigration{
			rawMigWithDeps("01_A", "nonexistent"),
		}

		_, err := TopoSortMigrations(migs, nil, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrMismatchedMigration)
		assert.Contains(t, err.Error(), "nonexistent")
	})

	t.Run("cycle detection", func(t *testing.T) {
		migs := []*migrations.RawMigration{
			rawMigWithDeps("01_A", "02_B"),
			rawMigWithDeps("02_B", "01_A"),
		}

		_, err := TopoSortMigrations(migs, nil, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrDependencyCycle)
	})

	t.Run("three-way cycle detection", func(t *testing.T) {
		migs := []*migrations.RawMigration{
			rawMigWithDeps("01_A", "03_C"),
			rawMigWithDeps("02_B", "01_A"),
			rawMigWithDeps("03_C", "02_B"),
		}

		_, err := TopoSortMigrations(migs, nil, nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrDependencyCycle)
	})

	t.Run("filesystem position is tiebreaker", func(t *testing.T) {
		// D and E both depend on A; no relation between D and E
		// Filesystem order should determine D vs E ordering
		migs := []*migrations.RawMigration{
			rawMig("01_A"),
			rawMigWithDeps("02_D", "01_A"),
			rawMigWithDeps("03_E", "01_A"),
		}

		sorted, err := TopoSortMigrations(migs, nil, nil)
		require.NoError(t, err)
		require.Len(t, sorted, 3)
		assert.Equal(t, "01_A", sorted[0].Name)
		assert.Equal(t, "02_D", sorted[1].Name)
		assert.Equal(t, "03_E", sorted[2].Name)
	})

	t.Run("single migration returns as-is", func(t *testing.T) {
		migs := []*migrations.RawMigration{
			rawMig("01_only"),
		}

		sorted, err := TopoSortMigrations(migs, nil, nil)
		require.NoError(t, err)
		require.Len(t, sorted, 1)
		assert.Equal(t, "01_only", sorted[0].Name)
	})

	t.Run("empty list returns empty", func(t *testing.T) {
		sorted, err := TopoSortMigrations(nil, nil, nil)
		require.NoError(t, err)
		assert.Empty(t, sorted)
	})

	t.Run("multiple dependencies on same migration", func(t *testing.T) {
		// B and C both depend on A; D depends on both B and C
		migs := []*migrations.RawMigration{
			rawMig("01_A"),
			rawMigWithDeps("02_B", "01_A"),
			rawMigWithDeps("03_C", "01_A"),
			rawMigWithDeps("04_D", "02_B", "03_C"),
		}

		sorted, err := TopoSortMigrations(migs, nil, nil)
		require.NoError(t, err)
		require.Len(t, sorted, 4)
		assert.Equal(t, "01_A", sorted[0].Name)
		assert.Equal(t, "02_B", sorted[1].Name)
		assert.Equal(t, "03_C", sorted[2].Name)
		assert.Equal(t, "04_D", sorted[3].Name)
	})
}

func rawMig(name string) *migrations.RawMigration {
	ops, _ := json.Marshal([]map[string]interface{}{
		{"sql": map[string]interface{}{"up": "SELECT 1"}},
	})
	return &migrations.RawMigration{
		Name:       name,
		Operations: ops,
	}
}

func rawMigWithDeps(name string, deps ...string) *migrations.RawMigration {
	m := rawMig(name)
	m.DependsOn = deps
	return m
}
