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

// These tests apply migrations in an order that differs from their names,
// which is what a live database does when a back-dated migration lands late,
// and then adopt a baseline that folds some of them in.
//
// The directory throughout: 10_anchor is the marked baseline, 05_backdated's
// file is deleted (it sorts below the anchor), and the 20s and 30s are kept.

func truncatedAt10(t *testing.T, folds string, kept ...string) fstest.MapFS {
	t.Helper()
	dir := fstest.MapFS{
		"10_anchor.json": &fstest.MapFile{Data: baselineMarkedJSON()},
	}
	if folds != "" {
		dir["10_anchor.folds"] = &fstest.MapFile{Data: []byte(folds)}
	}
	for _, name := range kept {
		dir[name+".json"] = &fstest.MapFile{Data: exampleMigration(t, name)}
	}
	return dir
}

func leafOf(t *testing.T, m *roll.Roll) string {
	t.Helper()
	latest, err := m.State().LatestMigration(context.Background(), cSchema)
	require.NoError(t, err)
	require.NotNil(t, latest)
	return *latest
}

func TestReadFoldSet(t *testing.T) {
	t.Parallel()

	t.Run("nil when the directory declares none", func(t *testing.T) {
		folds, err := roll.ReadFoldSet(fstest.MapFS{}, "10_anchor")
		require.NoError(t, err)
		assert.Nil(t, folds)
	})

	t.Run("reads names and targets, skipping comments and blank lines", func(t *testing.T) {
		dir := fstest.MapFS{"10_anchor.folds": &fstest.MapFile{Data: []byte(
			"# Migrations folded into 10_anchor\n\n01_one app,etl\n05_backdated\t app\n10_anchor\n",
		)}}

		folds, err := roll.ReadFoldSet(dir, "10_anchor")
		require.NoError(t, err)
		assert.Equal(t, []roll.FoldedMigration{
			{Name: "01_one", Targets: []string{"app", "etl"}},
			{Name: "05_backdated", Targets: []string{"app"}},
			{Name: "10_anchor"},
		}, folds)
	})

	t.Run("an empty file is a declared empty set", func(t *testing.T) {
		dir := fstest.MapFS{"10_anchor.folds": &fstest.MapFile{Data: []byte("# nothing\n")}}

		folds, err := roll.ReadFoldSet(dir, "10_anchor")
		require.NoError(t, err)
		assert.NotNil(t, folds)
		assert.Empty(t, folds)
	})

	for name, content := range map[string]string{
		"a name sorting after the baseline": "20_v1\n",
		"a name listed twice":               "01_one\n01_one app\n",
		"a third field":                     "01_one app etl\n",
		"an empty target":                   "01_one app,,etl\n",
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			dir := fstest.MapFS{"10_anchor.folds": &fstest.MapFile{Data: []byte(content)}}

			_, err := roll.ReadFoldSet(dir, "10_anchor")
			require.ErrorIs(t, err, roll.ErrRebaselineRefused)
		})
	}
}

func TestRebaselineWithFoldSet(t *testing.T) {
	t.Parallel()

	t.Run("adopts a baseline over a history applied out of name order", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			// 20_v1 lands before the anchor; 05_backdated after it.
			applyChain(t, m, "01_one", "20_v1", "10_anchor", "05_backdated", "30_v2")
			dir := truncatedAt10(t, "01_one\n05_backdated\n10_anchor\n", "20_v1", "30_v2", "40_new")

			res, err := m.Rebaseline(ctx, dir, false)
			require.NoError(t, err)
			assert.Equal(t, roll.RebaselineConverted, res.Outcome)
			assert.True(t, res.FoldSetDeclared)

			// Nothing folded is re-applied, nothing kept is reported
			// missing: only the new file is pending.
			migs, err := m.UnappliedMigrations(ctx, dir)
			require.NoError(t, err)
			require.Len(t, migs, 1)
			assert.Equal(t, "40_new", migs[0].Name)

			res, err = m.Rebaseline(ctx, dir, false)
			require.NoError(t, err)
			assert.Equal(t, roll.RebaselineAlreadyBaseline, res.Outcome)
		})
	})

	t.Run("the same history is refused without a fold set", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			applyChain(t, m, "01_one", "20_v1", "10_anchor", "05_backdated", "30_v2")

			_, err := m.Rebaseline(context.Background(), truncatedAt10(t, "", "20_v1", "30_v2"), false)
			require.ErrorIs(t, err, roll.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "declares no fold set")
		})
	})

	t.Run("refuses a database that never applied a folded migration", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			// This database deployed the anchor but not the back-dated
			// migration that landed later. Its file is gone, so adopting the
			// baseline would skip it for good.
			applyChain(t, m, "01_one", "10_anchor", "20_v1")
			dir := truncatedAt10(t, "01_one\n05_backdated\n10_anchor\n", "20_v1")

			_, err := m.Rebaseline(ctx, dir, true)
			require.ErrorIs(t, err, roll.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "05_backdated")
			assert.ErrorContains(t, err, "never applied")
		})
	})

	t.Run("refuses a database holding a row the fold set does not name", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			applyChain(t, m, "01_one", "07_from_a_branch", "10_anchor", "20_v1")
			dir := truncatedAt10(t, "01_one\n10_anchor\n", "20_v1")

			_, err := m.Rebaseline(ctx, dir, true)
			require.ErrorIs(t, err, roll.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "07_from_a_branch")
		})
	})

	t.Run("a database built from the truncated directory adopts its own baseline", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			dir := truncatedAt10(t, "01_one\n05_backdated\n10_anchor\n", "20_v1")

			// A fresh database runs the baseline file as an ordinary
			// migration. The folded migrations reached it through that
			// snapshot, so it has no rows for them and is not asked for any.
			baseline, err := migrations.ReadRawMigration(dir, "10_anchor.json")
			require.NoError(t, err)
			parsed, err := migrations.ParseMigration(baseline)
			require.NoError(t, err)
			require.NoError(t, m.Start(ctx, parsed, backfill.NewConfig()))
			require.NoError(t, m.Complete(ctx))
			applyChain(t, m, "20_v1")

			res, err := m.Rebaseline(ctx, dir, false)
			require.NoError(t, err)
			assert.Equal(t, roll.RebaselineConverted, res.Outcome)
		})
	})

	t.Run("refuses a fold set named for another migration", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			applyChain(t, m, "01_one", "10_anchor")
			dir := truncatedAt10(t, "")
			dir["05_previous.folds"] = &fstest.MapFile{Data: []byte("01_one\n")}

			_, err := m.Rebaseline(context.Background(), dir, true)
			require.ErrorIs(t, err, roll.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "05_previous.folds")
		})
	})

	t.Run("a migration named at or below the baseline cannot be started", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			foldAt10(t, m, createTable("05_backdated", "t_b"), createTable("10_anchor", "t_anchor"))

			err := m.Start(ctx, createTable("07_hotfix", "t_hotfix"), backfill.NewConfig())
			require.ErrorIs(t, err, roll.ErrMigrationBelowBaseline)
		})
	})

	t.Run("a target is held only to the folded migrations routed to it", func(t *testing.T) {
		const folds = "01_one app,etl\n03_etl_only etl\n10_anchor app,etl\n"

		testutils.WithMigratorAndConnStrToContainer(t, []roll.Option{roll.WithLockTimeoutMs(500)},
			func(seed *roll.Roll, connStr string, _ *sql.DB) {
				ctx := context.Background()
				applyChain(t, seed, "01_one", "10_anchor", "20_v1")

				app := testutils.NewMigratorForConnStr(t, connStr,
					roll.WithLockTimeoutMs(500), roll.WithTarget("app"))

				res, err := app.Rebaseline(ctx, truncatedAt10(t, folds), false)
				require.NoError(t, err)
				assert.Equal(t, roll.RebaselineConverted, res.Outcome)
			})

		// With no target the database is expected to have applied every
		// migration, so the same history is refused.
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			applyChain(t, m, "01_one", "10_anchor", "20_v1")

			_, err := m.Rebaseline(ctx, truncatedAt10(t, folds), false)
			require.ErrorIs(t, err, roll.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "03_etl_only")
		})
	})
}

// foldAt10 applies the given migrations in order and converts 10_anchor to
// the baseline, folding in 05_backdated.
func foldAt10(t *testing.T, m *roll.Roll, apply ...*migrations.Migration) {
	t.Helper()
	for _, mig := range apply {
		applyAndSeal(t, m, mig)
	}
	_, err := m.Rebaseline(context.Background(),
		truncatedAt10(t, "05_backdated\n10_anchor\n"), false)
	require.NoError(t, err)
}

func TestRevertAcrossAFoldedMigration(t *testing.T) {
	t.Parallel()

	t.Run("refuses a boundary applied before a folded migration", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			foldAt10(
				t, m,
				createTable("10_anchor", "t_anchor"),
				createTable("20_v1", "t_v1"),
				createTable("05_backdated", "t_b"), // folded, applied after 20_v1
				createTable("30_v2", "t_v2"),
			)

			// Reverting to 20_v1 would have to undo 05_backdated too, and its
			// snapshot is not the schema beneath 30_v2.
			_, err := m.PlanRevertSealed(ctx, "20_v1")
			require.Error(t, err)
			assert.ErrorContains(t, err, "not contiguous")
			assert.ErrorContains(t, err, "05_backdated")

			_, err = m.RevertSealed(ctx, "20_v1", backfill.NewConfig())
			require.Error(t, err)
			assert.True(t, tableExists(t, db, cSchema, "t_v2"), "nothing was reverted")
			assert.Equal(t, "30_v2", leafOf(t, m))
		})
	})

	t.Run("reverting to the baseline stops at the floor", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			foldAt10(
				t, m,
				createTable("20_v1", "t_v1"), // kept, applied before the anchor
				createTable("10_anchor", "t_anchor"),
				createTable("05_backdated", "t_b"), // folded, applied after it
				createTable("30_v2", "t_v2"),
			)

			plan, err := m.RevertSealed(ctx, "10_anchor", backfill.NewConfig())
			require.NoError(t, err)
			require.NotNil(t, plan)
			assert.Equal(t, []string{"30_v2"}, plan.Targets)
			assert.Equal(t, "05_backdated", plan.Boundary)

			// Only what was applied on top of the folded set is undone.
			assert.False(t, tableExists(t, db, cSchema, "t_v2"))
			assert.True(t, tableExists(t, db, cSchema, "t_v1"))
			assert.True(t, tableExists(t, db, cSchema, "t_b"))
			assert.Equal(t, "05_backdated", leafOf(t, m))

			history, err := m.State().SchemaHistory(ctx, cSchema)
			require.NoError(t, err)
			require.Len(t, history, 1)
			assert.Equal(t, "20_v1", history[0].Migration.Name)
		})
	})

	t.Run("a revert above the floor is unaffected", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			foldAt10(
				t, m,
				createTable("10_anchor", "t_anchor"),
				createTable("05_backdated", "t_b"),
				createTable("20_v1", "t_v1"),
				createTable("30_v2", "t_v2"),
			)

			plan, err := m.RevertSealed(ctx, "20_v1", backfill.NewConfig())
			require.NoError(t, err)
			assert.Equal(t, []string{"30_v2"}, plan.Targets)
			assert.False(t, tableExists(t, db, cSchema, "t_v2"))
			assert.Equal(t, "20_v1", leafOf(t, m))
		})
	})
}

// A sealed revert names its inverse migrations `revert_<name>`, which sorts
// below a baseline named with a later letter. They must stay visible, or the
// revert that created them can never find them to finish.
func TestSealedRevertUnderABaselineSortingAboveItsInverses(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
		ctx := context.Background()
		require.NoError(t, m.CreateBaseline(ctx, "v1_base"))
		applyAndSeal(t, m, createTable("v2_a", "t_a"))
		applyAndSeal(t, m, createTable("v3_b", "t_b"))

		_, err := m.RevertSealed(ctx, "v2_a", backfill.NewConfig(), roll.WithExpandOnly())
		require.NoError(t, err)
		require.NoError(t, m.Complete(ctx))
		plan, err := m.FinishPendingSealedRevert(ctx)
		require.NoError(t, err)
		require.NotNil(t, plan, "the inverse row was hidden from the revert that created it")

		assert.False(t, tableExists(t, db, cSchema, "t_b"))
		assert.Equal(t, "v2_a", leafOf(t, m))
		history, err := m.State().SchemaHistory(ctx, cSchema)
		require.NoError(t, err)
		require.Len(t, history, 1)
		assert.Equal(t, "v2_a", history[0].Migration.Name)
	})
}

func TestPlanWithAFoldedLeaf(t *testing.T) {
	t.Parallel()

	t.Run("a pending migration is a plain apply, not a divergence", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			foldAt10(
				t, m,
				createTable("10_anchor", "t_anchor"),
				createTable("20_v1", "t_v1"),
				createTable("05_backdated", "t_b"), // the leaf, and folded
			)

			plan, err := m.Plan(ctx, truncatedAt10(t, "", "20_v1", "30_new"))
			require.NoError(t, err)
			assert.False(t, plan.Diverged)
			assert.False(t, plan.InSync)
			assert.Equal(t, []string{"30_new"}, plan.Apply.Migrations)
			assert.Zero(t, plan.Revert.Count)
			assert.Zero(t, plan.Blocked.Count)

			plan, err = m.Plan(ctx, truncatedAt10(t, "", "20_v1"))
			require.NoError(t, err)
			assert.True(t, plan.InSync, "everything on disk is applied")
			assert.False(t, plan.Diverged)
		})
	})

	t.Run("an in-flight train on a folded parent reverts onto the floor", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			foldAt10(
				t, m,
				createTable("10_anchor", "t_anchor"),
				createTable("20_v1", "t_v1"),
				createTable("05_backdated", "t_b"),
			)
			// A train this checkout does not have, left in its revert window.
			require.NoError(t, m.Start(ctx, createTable("40_foreign", "t_foreign"), backfill.NewConfig()))
			require.NoError(t, m.Complete(ctx, roll.WithDeferComplete()))

			plan, err := m.Plan(ctx, truncatedAt10(t, "", "20_v1"))
			require.NoError(t, err)
			assert.Zero(t, plan.Blocked.Count, "blocked: %+v", plan.Blocked)
			assert.Equal(t, []string{"40_foreign"}, plan.Revert.Migrations)
			require.NotNil(t, plan.Revert.To)
			assert.Equal(t, "05_backdated", *plan.Revert.To)
			require.NotNil(t, plan.Revert.ToSchema)
			assert.Equal(t, vs("05_backdated"), *plan.Revert.ToSchema)

			// The plan's target is one `revert --to` accepts.
			reverted, err := m.Revert(ctx, roll.WithRevertTo(*plan.Revert.To))
			require.NoError(t, err)
			require.Len(t, reverted, 1)
			assert.Equal(t, "05_backdated", leafOf(t, m))
		})
	})

	t.Run("a kept migration absent from the checkout beneath the floor is blocked", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			foldAt10(
				t, m,
				createTable("10_anchor", "t_anchor"),
				createTable("20_v1", "t_v1"),
				createTable("05_backdated", "t_b"),
				createTable("30_v2", "t_v2"),
			)

			// The checkout has neither 20_v1 nor 30_v2. 30_v2 can be
			// reverted; 20_v1 sits beneath a folded migration and cannot.
			plan, err := m.Plan(ctx, truncatedAt10(t, ""))
			require.NoError(t, err)
			assert.Equal(t, []string{"30_v2"}, plan.Revert.Migrations)
			require.NotNil(t, plan.Revert.To)
			assert.Equal(t, "05_backdated", *plan.Revert.To)
			require.NotNil(t, plan.Blocked.Reason)
			assert.Equal(t, "non-contiguous", *plan.Blocked.Reason)
			assert.Equal(t, []string{"20_v1"}, plan.Blocked.Migrations)
		})
	})
}
