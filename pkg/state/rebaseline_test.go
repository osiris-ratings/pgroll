// SPDX-License-Identifier: Apache-2.0

package state_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/state"
)

// histRow shapes one pgroll.migrations row for direct insertion. Tests here
// exercise ConvertMigrationToBaseline against precisely shaped histories, and
// created_at is the load-bearing column, so rows are inserted directly rather
// than through Start/Complete (which stamp CURRENT_TIMESTAMP).
type histRow struct {
	name             string
	parent           *string
	typ              string
	createdAt        string
	done             bool
	sealed           bool
	completeDeferred bool
	emptySchema      bool
}

func row(name string, parent *string, typ, createdAt string) histRow {
	return histRow{
		name:      name,
		parent:    parent,
		typ:       typ,
		createdAt: createdAt,
		done:      true,
		sealed:    true,
	}
}

func insertHistRows(t *testing.T, db *sql.DB, rows []histRow) {
	t.Helper()
	for _, r := range rows {
		resulting := `{"name": "public", "tables": {}}`
		if r.emptySchema {
			resulting = `{}`
		}
		_, err := db.Exec(`INSERT INTO pgroll.migrations
			(schema, name, migration, parent, done, sealed, complete_deferred,
			 resulting_schema, migration_type, created_at, updated_at)
			VALUES ('public', $1, '{"operations": []}'::jsonb, $2, $3, $4, $5,
			 $6::jsonb, $7, $8::timestamptz, $8::timestamptz)`,
			r.name, r.parent, r.done, r.sealed, r.completeDeferred, resulting, r.typ, r.createdAt)
		require.NoError(t, err)
	}
}

// seedChain inserts the canonical shape: a baseline followed by three applied,
// sealed, contracted migrations, in agreeing name and apply order.
//
//	00_baseline → 01_one → 02_two → 03_three
func seedChain(t *testing.T, db *sql.DB) {
	t.Helper()
	insertHistRows(t, db, []histRow{
		row("00_baseline", nil, "baseline", "2026-01-01 00:00:00Z"),
		row("01_one", ptr("00_baseline"), "pgroll", "2026-01-01 00:01:00Z"),
		row("02_two", ptr("01_one"), "pgroll", "2026-01-01 00:02:00Z"),
		row("03_three", ptr("02_two"), "pgroll", "2026-01-01 00:03:00Z"),
	})
}

func migrationTypeOf(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var typ string
	err := db.QueryRow(
		"SELECT migration_type FROM pgroll.migrations WHERE schema = 'public' AND name = $1",
		name,
	).Scan(&typ)
	require.NoError(t, err)
	return typ
}

func TestConvertMigrationToBaseline(t *testing.T) {
	t.Parallel()

	t.Run("converts in place, preserving created_at", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			seedChain(t, db)

			var before string
			require.NoError(t, db.QueryRow(
				"SELECT created_at::text FROM pgroll.migrations WHERE name = '02_two'",
			).Scan(&before))

			res, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two", nil, false)
			require.NoError(t, err)
			assert.False(t, res.AlreadyBaseline)
			assert.False(t, res.EmptyResultingSchema)

			assert.Equal(t, "baseline", migrationTypeOf(t, db, "02_two"))

			var after string
			require.NoError(t, db.QueryRow(
				"SELECT created_at::text FROM pgroll.migrations WHERE name = '02_two'",
			).Scan(&after))
			assert.Equal(t, before, after,
				"the conversion must preserve created_at; that is what positions the baseline correctly")

			// The converted row is now the latest baseline and hides everything
			// at or before it.
			baseline, err := st.LatestBaseline(ctx, "public")
			require.NoError(t, err)
			require.NotNil(t, baseline)
			assert.Equal(t, "02_two", baseline.Name)

			history, err := st.SchemaHistory(ctx, "public")
			require.NoError(t, err)
			require.Len(t, history, 1)
			assert.Equal(t, "03_three", history[0].Migration.Name)
		})
	})

	t.Run("idempotent when the row is already a baseline", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			seedChain(t, db)

			_, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two", nil, false)
			require.NoError(t, err)

			res, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two", nil, false)
			require.NoError(t, err)
			assert.True(t, res.AlreadyBaseline)
		})
	})

	t.Run("dry run audits without writing", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			seedChain(t, db)

			res, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two", nil, true)
			require.NoError(t, err)
			assert.False(t, res.AlreadyBaseline)
			assert.Equal(t, "pgroll", migrationTypeOf(t, db, "02_two"))
		})
	})

	t.Run("refuses when the row is absent", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			seedChain(t, db)

			_, err := st.ConvertMigrationToBaseline(ctx, "public", "09_missing", nil, false)
			require.ErrorIs(t, err, state.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "no row in the history")
		})
	})

	t.Run("refuses an inferred anchor", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			insertHistRows(t, db, []histRow{
				row("00_baseline", nil, "baseline", "2026-01-01 00:00:00Z"),
				row("01_inferred", ptr("00_baseline"), "inferred", "2026-01-01 00:01:00Z"),
			})

			_, err := st.ConvertMigrationToBaseline(ctx, "public", "01_inferred", nil, false)
			require.ErrorIs(t, err, state.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "inferred")
		})
	})

	t.Run("refuses an uncontracted anchor", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			unsealed := row("02_two", ptr("01_one"), "pgroll", "2026-01-01 00:02:00Z")
			unsealed.sealed = false
			insertHistRows(t, db, []histRow{
				row("00_baseline", nil, "baseline", "2026-01-01 00:00:00Z"),
				row("01_one", ptr("00_baseline"), "pgroll", "2026-01-01 00:01:00Z"),
				unsealed,
			})

			_, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two", nil, false)
			require.ErrorIs(t, err, state.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "not fully applied and contracted")
		})
	})

	t.Run("refuses while a migration is in progress", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			seedChain(t, db)
			active := row("04_active", ptr("03_three"), "pgroll", "2026-01-01 00:04:00Z")
			active.done = false
			active.sealed = false
			insertHistRows(t, db, []histRow{active})

			_, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two", nil, false)
			require.ErrorIs(t, err, state.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "in progress")
		})
	})

	t.Run("refuses a pending deferred complete below the anchor", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			deferred := row("01_one", ptr("00_baseline"), "pgroll", "2026-01-01 00:01:00Z")
			deferred.completeDeferred = true
			insertHistRows(t, db, []histRow{
				row("00_baseline", nil, "baseline", "2026-01-01 00:00:00Z"),
				deferred,
				row("02_two", ptr("01_one"), "pgroll", "2026-01-01 00:02:00Z"),
				row("03_three", ptr("02_two"), "pgroll", "2026-01-01 00:03:00Z"),
			})

			_, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two", nil, false)
			require.ErrorIs(t, err, state.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "01_one")
		})
	})

	t.Run("refuses when a later baseline exists", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			insertHistRows(t, db, []histRow{
				row("00_baseline", nil, "baseline", "2026-01-01 00:00:00Z"),
				row("01_one", ptr("00_baseline"), "pgroll", "2026-01-01 00:01:00Z"),
				row("02_two", ptr("01_one"), "pgroll", "2026-01-01 00:02:00Z"),
				row("03_three", ptr("02_two"), "baseline", "2026-01-01 00:03:00Z"),
			})

			_, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two", nil, false)
			require.ErrorIs(t, err, state.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "03_three")
		})
	})

	t.Run("without a fold set, refuses a row applied before the anchor that sorts after it", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			// 04_hotfix was applied BEFORE 02_two but its name sorts after.
			// Nothing says which migrations the baseline covers, so only a
			// database that applied them in name order may adopt it.
			insertHistRows(t, db, []histRow{
				row("00_baseline", nil, "baseline", "2026-01-01 00:00:00Z"),
				row("01_one", ptr("00_baseline"), "pgroll", "2026-01-01 00:01:00Z"),
				row("04_hotfix", ptr("01_one"), "pgroll", "2026-01-01 00:01:30Z"),
				row("02_two", ptr("04_hotfix"), "pgroll", "2026-01-01 00:02:00Z"),
				row("03_three", ptr("02_two"), "pgroll", "2026-01-01 00:03:00Z"),
			})

			_, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two", nil, false)
			require.ErrorIs(t, err, state.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "04_hotfix")
			assert.ErrorContains(t, err, "declares no fold set")
		})
	})

	t.Run("without a fold set, refuses a row applied after the anchor that sorts before it", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			// 01a_late was applied AFTER 02_two but its name sorts before.
			insertHistRows(t, db, []histRow{
				row("00_baseline", nil, "baseline", "2026-01-01 00:00:00Z"),
				row("01_one", ptr("00_baseline"), "pgroll", "2026-01-01 00:01:00Z"),
				row("02_two", ptr("01_one"), "pgroll", "2026-01-01 00:02:00Z"),
				row("01a_late", ptr("02_two"), "pgroll", "2026-01-01 00:03:00Z"),
			})

			_, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two", nil, false)
			require.ErrorIs(t, err, state.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "01a_late")
			assert.ErrorContains(t, err, "declares no fold set")
		})
	})

	t.Run("ignores inferred rows in the order audit", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			// Same shape as the refusal above, but the offending row is
			// inferred (out-of-band DDL): it never corresponds to a file, so
			// it must not block the conversion.
			insertHistRows(t, db, []histRow{
				row("00_baseline", nil, "baseline", "2026-01-01 00:00:00Z"),
				row("01_one", ptr("00_baseline"), "pgroll", "2026-01-01 00:01:00Z"),
				row("02_two", ptr("01_one"), "pgroll", "2026-01-01 00:02:00Z"),
				row("01a_inferred", ptr("02_two"), "inferred", "2026-01-01 00:03:00Z"),
			})

			res, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two", nil, false)
			require.NoError(t, err)
			assert.False(t, res.AlreadyBaseline)
			assert.Equal(t, "baseline", migrationTypeOf(t, db, "02_two"))
		})
	})

	t.Run("flags a placeholder resulting_schema", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			anchor := row("02_two", ptr("01_one"), "pgroll", "2026-01-01 00:02:00Z")
			anchor.emptySchema = true
			insertHistRows(t, db, []histRow{
				row("00_baseline", nil, "baseline", "2026-01-01 00:00:00Z"),
				row("01_one", ptr("00_baseline"), "pgroll", "2026-01-01 00:01:00Z"),
				anchor,
			})

			res, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two", nil, false)
			require.NoError(t, err)
			assert.True(t, res.EmptyResultingSchema)
			assert.Equal(t, "baseline", migrationTypeOf(t, db, "02_two"))
		})
	})
}

// seedOutOfOrder inserts a history applied in a different order from its
// names, the shape a database gets from back-dated migrations landing late:
//
//	applied:  00_baseline → 01_one → 04_four → 02_two → 01a_late → 03_three
//
// With 02_two as the anchor, 01_one and 01a_late fold in and 03_three and
// 04_four are kept, though 04_four was applied before the anchor and
// 01a_late after it.
func seedOutOfOrder(t *testing.T, db *sql.DB) {
	t.Helper()
	insertHistRows(t, db, []histRow{
		row("00_baseline", nil, "baseline", "2026-01-01 00:00:00Z"),
		row("01_one", ptr("00_baseline"), "pgroll", "2026-01-01 00:01:00Z"),
		row("04_four", ptr("01_one"), "pgroll", "2026-01-01 00:02:00Z"),
		row("02_two", ptr("04_four"), "pgroll", "2026-01-01 00:03:00Z"),
		row("01a_late", ptr("02_two"), "pgroll", "2026-01-01 00:04:00Z"),
		row("03_three", ptr("01a_late"), "pgroll", "2026-01-01 00:05:00Z"),
	})
}

func foldSet(names ...string) *state.FoldSet {
	return &state.FoldSet{All: names, Required: names}
}

func historyNames(t *testing.T, st *state.State) []string {
	t.Helper()
	history, err := st.SchemaHistory(context.Background(), "public")
	require.NoError(t, err)
	names := make([]string, len(history))
	for i, h := range history {
		names[i] = h.Migration.Name
	}
	return names
}

func TestConvertMigrationToBaselineWithFoldSet(t *testing.T) {
	t.Parallel()

	t.Run("converts a history applied out of name order", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			seedOutOfOrder(t, db)

			_, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two",
				foldSet("01_one", "01a_late", "02_two"), false)
			require.NoError(t, err)
			assert.Equal(t, "baseline", migrationTypeOf(t, db, "02_two"))

			// Exactly the rows named above the anchor stay visible: 04_four,
			// applied before it, and 03_three. 01a_late, applied after it, is
			// folded.
			assert.Equal(t, []string{"04_four", "03_three"}, historyNames(t, st))

			floor, err := st.BaselineFloor(ctx, "public")
			require.NoError(t, err)
			require.NotNil(t, floor)
			assert.Equal(t, "01a_late", floor.Name,
				"the floor is the folded row applied last, not the baseline row")
		})
	})

	t.Run("refuses when a folded migration was never applied", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			seedOutOfOrder(t, db)

			_, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two",
				foldSet("01_one", "01a_late", "01b_never", "02_two"), false)
			require.ErrorIs(t, err, state.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "01b_never")
			assert.ErrorContains(t, err, "never applied")
			assert.Equal(t, "pgroll", migrationTypeOf(t, db, "02_two"))
		})
	})

	t.Run("does not require a folded migration routed elsewhere", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			seedOutOfOrder(t, db)

			_, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two", &state.FoldSet{
				All:      []string{"01_one", "01a_late", "01b_other_target", "02_two"},
				Required: []string{"01_one", "01a_late", "02_two"},
			}, false)
			require.NoError(t, err)
		})
	})

	t.Run("refuses a row the fold set does not name", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			seedOutOfOrder(t, db)

			// 01a_late was applied here but is not among the files the
			// truncation folded: the baseline's snapshot does not describe it.
			_, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two",
				foldSet("01_one", "02_two"), false)
			require.ErrorIs(t, err, state.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "01a_late")
			assert.ErrorContains(t, err, "not among those the baseline folds in")
		})
	})

	t.Run("refuses an uncontracted folded row wherever it was applied", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			late := row("01a_late", ptr("02_two"), "pgroll", "2026-01-01 00:04:00Z")
			late.completeDeferred = true
			insertHistRows(t, db, []histRow{
				row("00_baseline", nil, "baseline", "2026-01-01 00:00:00Z"),
				row("01_one", ptr("00_baseline"), "pgroll", "2026-01-01 00:01:00Z"),
				row("02_two", ptr("01_one"), "pgroll", "2026-01-01 00:03:00Z"),
				late,
			})

			_, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two",
				foldSet("01_one", "01a_late", "02_two"), false)
			require.ErrorIs(t, err, state.ErrRebaselineRefused)
			assert.ErrorContains(t, err, "01a_late")
			assert.ErrorContains(t, err, "deferred complete")
		})
	})

	t.Run("an uncontracted row above the anchor does not block it", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			// 04_four is kept, not folded, so its open revert window is not
			// buried by the baseline even though it was applied first.
			early := row("04_four", ptr("01_one"), "pgroll", "2026-01-01 00:02:00Z")
			early.sealed = false
			early.completeDeferred = true
			insertHistRows(t, db, []histRow{
				row("00_baseline", nil, "baseline", "2026-01-01 00:00:00Z"),
				row("01_one", ptr("00_baseline"), "pgroll", "2026-01-01 00:01:00Z"),
				early,
				row("02_two", ptr("04_four"), "pgroll", "2026-01-01 00:03:00Z"),
			})

			_, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two",
				foldSet("01_one", "02_two"), false)
			require.NoError(t, err)
		})
	})

	t.Run("audits only above the database's own baseline", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			// This database was built from a baseline at 01_one: it has no
			// row for 00a_ancient, which a cumulative fold set still names.
			insertHistRows(t, db, []histRow{
				row("01_one", nil, "baseline", "2026-01-01 00:01:00Z"),
				row("02_two", ptr("01_one"), "pgroll", "2026-01-01 00:03:00Z"),
			})

			_, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two",
				foldSet("00a_ancient", "01_one", "02_two"), false)
			require.NoError(t, err)
		})
	})

	t.Run("a second conversion wins by name when its anchor was applied first", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			seedOutOfOrder(t, db)

			_, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two",
				foldSet("01_one", "01a_late", "02_two"), false)
			require.NoError(t, err)

			// 04_four was applied before 02_two. As the next anchor it must
			// still become the baseline in effect.
			_, err = st.ConvertMigrationToBaseline(ctx, "public", "04_four",
				foldSet("01_one", "01a_late", "02_two", "03_three", "04_four"), false)
			require.NoError(t, err)

			baseline, err := st.LatestBaseline(ctx, "public")
			require.NoError(t, err)
			require.NotNil(t, baseline)
			assert.Equal(t, "04_four", baseline.Name)
			assert.Empty(t, historyNames(t, st))

			floor, err := st.BaselineFloor(ctx, "public")
			require.NoError(t, err)
			assert.Equal(t, "03_three", floor.Name)
		})
	})

	t.Run("hides an inferred row captured beneath the floor, not one above it", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			ctx := context.Background()
			insertHistRows(t, db, []histRow{
				row("00_baseline", nil, "baseline", "2026-01-01 00:00:00Z"),
				row("01_one", ptr("00_baseline"), "pgroll", "2026-01-01 00:01:00Z"),
				row("02_two", ptr("01_one"), "pgroll", "2026-01-01 00:03:00Z"),
				row("sql_beneath", ptr("02_two"), "inferred", "2026-01-01 00:03:30Z"),
				row("01a_late", ptr("sql_beneath"), "pgroll", "2026-01-01 00:04:00Z"),
				row("sql_above", ptr("01a_late"), "inferred", "2026-01-01 00:04:30Z"),
			})

			_, err := st.ConvertMigrationToBaseline(ctx, "public", "02_two",
				foldSet("01_one", "01a_late", "02_two"), false)
			require.NoError(t, err)

			history, err := st.SchemaHistoryWithInferred(ctx, "public")
			require.NoError(t, err)
			require.Len(t, history, 1)
			assert.Equal(t, "sql_above", history[0].Migration.Name)
		})
	})
}

func TestBaselineFloor(t *testing.T) {
	t.Parallel()

	t.Run("nil without a baseline", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			insertHistRows(t, db, []histRow{
				row("01_one", nil, "pgroll", "2026-01-01 00:01:00Z"),
			})

			floor, err := st.BaselineFloor(context.Background(), "public")
			require.NoError(t, err)
			assert.Nil(t, floor)
		})
	})

	t.Run("the baseline row when nothing folded was applied after it", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
			seedChain(t, db)

			floor, err := st.BaselineFloor(context.Background(), "public")
			require.NoError(t, err)
			require.NotNil(t, floor)
			assert.Equal(t, "00_baseline", floor.Name)
		})
	})
}
