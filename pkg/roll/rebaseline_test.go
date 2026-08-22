// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

// baselineMarkedJSON is a migration file carrying the `baseline: true` marker:
// an irreversible schema snapshot, as the truncation workflow writes it.
func baselineMarkedJSON() []byte {
	return []byte(`{"baseline":true,"irreversible":true,"operations":[{"sql":{"up":"SELECT 1"}}]}`)
}

// applyChain applies each named migration for real (Start + Complete), so the
// history rows carry genuine created_at ordering and resulting_schema.
func applyChain(t *testing.T, m *roll.Roll, names ...string) {
	t.Helper()
	ctx := context.Background()
	for _, name := range names {
		mig := &migrations.Migration{
			Name:       name,
			Operations: migrations.Operations{&migrations.OpRawSQL{Up: "SELECT 1"}},
		}
		require.NoError(t, m.Start(ctx, mig, backfill.NewConfig()))
		require.NoError(t, m.Complete(ctx))
	}
}

func TestRebaseline(t *testing.T) {
	t.Parallel()

	// The truncated-directory shape used throughout: 01_one's file has been
	// deleted, 02_two has been rewritten as the marked baseline, and 05_five
	// is new work on top of the retained tail.
	truncatedDir := fstest.MapFS{
		"02_two.json":   &fstest.MapFile{Data: baselineMarkedJSON()},
		"03_three.json": &fstest.MapFile{Data: exampleMigration(t, "03_three")},
		"04_four.json":  &fstest.MapFile{Data: exampleMigration(t, "04_four")},
		"05_five.json":  &fstest.MapFile{Data: exampleMigration(t, "05_five")},
	}

	t.Run("converts an applied anchor and resolves only the tail", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			applyChain(t, m, "01_one", "02_two", "03_three", "04_four")

			// Pre-conversion, the truncated directory is unusable: 01_one is
			// applied but its file is gone.
			_, err := m.UnappliedMigrations(ctx, truncatedDir)
			require.ErrorIs(t, err, roll.ErrMismatchedMigration)

			res, err := m.Rebaseline(ctx, truncatedDir, false)
			require.NoError(t, err)
			assert.Equal(t, roll.RebaselineConverted, res.Outcome)
			assert.Equal(t, "02_two", res.BaselineName)
			assert.Empty(t, res.DBBaseline)

			// Post-conversion, resolution accepts the truncated directory and
			// selects exactly the new work.
			migs, err := m.UnappliedMigrations(ctx, truncatedDir)
			require.NoError(t, err)
			require.Len(t, migs, 1)
			assert.Equal(t, "05_five", migs[0].Name)
		})
	})

	t.Run("idempotent: a second run is a no-op", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			applyChain(t, m, "01_one", "02_two", "03_three", "04_four")

			_, err := m.Rebaseline(ctx, truncatedDir, false)
			require.NoError(t, err)

			res, err := m.Rebaseline(ctx, truncatedDir, false)
			require.NoError(t, err)
			assert.Equal(t, roll.RebaselineAlreadyBaseline, res.Outcome)
			assert.Equal(t, "02_two", res.DBBaseline)
		})
	})

	t.Run("no-op when the database baseline is newer than the directory's", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			applyChain(t, m, "01_one", "02_two")
			// A later rebaseline already happened on this database; an old
			// checkout reconciling against it must do nothing.
			require.NoError(t, m.CreateBaseline(ctx, "09_newer"))

			res, err := m.Rebaseline(ctx, truncatedDir, false)
			require.NoError(t, err)
			assert.Equal(t, roll.RebaselineDBBaselineNewer, res.Outcome)
			assert.Equal(t, "09_newer", res.DBBaseline)
		})
	})

	t.Run("no-op on a database with no history", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			res, err := m.Rebaseline(ctx, truncatedDir, false)
			require.NoError(t, err)
			assert.Equal(t, roll.RebaselineEmptyHistory, res.Outcome)
		})
	})

	t.Run("check mode reports pending without writing", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			applyChain(t, m, "01_one", "02_two", "03_three", "04_four")

			res, err := m.Rebaseline(ctx, truncatedDir, true)
			require.NoError(t, err)
			assert.Equal(t, roll.RebaselinePending, res.Outcome)

			// Nothing was written: the truncated directory still fails
			// resolution.
			_, err = m.UnappliedMigrations(ctx, truncatedDir)
			require.ErrorIs(t, err, roll.ErrMismatchedMigration)
		})
	})

	t.Run("hard error when the anchor was never applied", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			// History exists but predates the anchor entirely — the stale
			// environment that must catch up or be rebuilt. Running migrate
			// here would execute the baseline's snapshot; rebaseline must
			// refuse loudly instead of no-opping.
			applyChain(t, m, "01_one")

			_, err := m.Rebaseline(ctx, truncatedDir, false)
			require.ErrorIs(t, err, roll.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "02_two")
			assert.ErrorContains(t, err, "pre-truncation")
		})
	})

	t.Run("audit refusals surface as rebaseline refusals", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			// 04_four applied BEFORE the anchor 02_two: order disagreement
			// across the boundary, detected by the state-layer audit and
			// wrapped as a roll-layer refusal.
			applyChain(t, m, "01_one", "04_four", "02_two", "03_three")

			_, err := m.Rebaseline(ctx, truncatedDir, false)
			require.ErrorIs(t, err, roll.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "04_four")
		})
	})
}

func TestFindBaselineMigration(t *testing.T) {
	t.Parallel()

	t.Run("finds the single marked baseline", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_base.json": &fstest.MapFile{Data: baselineMarkedJSON()},
			"02_next.json": &fstest.MapFile{Data: exampleMigration(t, "02_next")},
		}
		raw, err := roll.FindBaselineMigration(fs)
		require.NoError(t, err)
		assert.Equal(t, "01_base", raw.Name)
		assert.True(t, raw.Baseline)
	})

	t.Run("refuses when no file is marked", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_base.json": &fstest.MapFile{Data: exampleMigration(t, "01_base")},
		}
		_, err := roll.FindBaselineMigration(fs)
		require.ErrorIs(t, err, roll.ErrRebaselineRefused)
		assert.ErrorContains(t, err, "no migration declares")
	})

	t.Run("refuses multiple marked files", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_base.json":  &fstest.MapFile{Data: baselineMarkedJSON()},
			"02_other.json": &fstest.MapFile{Data: baselineMarkedJSON()},
		}
		_, err := roll.FindBaselineMigration(fs)
		require.ErrorIs(t, err, roll.ErrRebaselineRefused)
		assert.ErrorContains(t, err, "at most one baseline")
	})

	t.Run("refuses a marked file that is not first", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_first.json": &fstest.MapFile{Data: exampleMigration(t, "01_first")},
			"02_base.json":  &fstest.MapFile{Data: baselineMarkedJSON()},
		}
		_, err := roll.FindBaselineMigration(fs)
		require.ErrorIs(t, err, roll.ErrRebaselineRefused)
		assert.ErrorContains(t, err, "not the first migration")
	})
}

func TestRefuseBaselineExecution(t *testing.T) {
	t.Parallel()

	plain := &migrations.RawMigration{Name: "01_plain"}
	marked := &migrations.RawMigration{Name: "02_base", Baseline: true}

	require.NoError(t, roll.RefuseBaselineExecution([]*migrations.RawMigration{plain}))

	err := roll.RefuseBaselineExecution([]*migrations.RawMigration{plain, marked})
	require.Error(t, err)
	assert.ErrorContains(t, err, "02_base")
	assert.ErrorContains(t, err, "baseline")
}
