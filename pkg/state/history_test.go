// SPDX-License-Identifier: Apache-2.0

package state_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/state"
)

func TestSchemaHistoryReturnsFullSchemaHistory(t *testing.T) {
	t.Parallel()

	testutils.WithStateAndConnectionToContainer(t, func(state *state.State, db *sql.DB) {
		ctx := context.Background()
		migs := []migrations.Migration{
			{
				Name: "01_add_table",
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
								Name:     "username",
								Type:     "text",
								Nullable: false,
							},
						},
					},
				},
			},
			{
				Name: "02_set_nullable",
				Operations: migrations.Operations{
					&migrations.OpAlterColumn{
						Table:    "users",
						Column:   "username",
						Nullable: ptr(false),
						Up:       "username",
					},
				},
			},
		}

		// Start and complete both migrations
		for _, mig := range migs {
			err := state.Start(ctx, "public", &mig)
			require.NoError(t, err)
			err = state.Complete(ctx, "public", mig.Name)
			require.NoError(t, err)
		}

		// Get the schema history
		res, err := state.SchemaHistory(ctx, "public")
		require.NoError(t, err)

		// Parse the raw migrations from the schema history into actual migrations
		actualMigs := make([]migrations.Migration, len(migs))
		for i := range res {
			m, err := migrations.ParseMigration(&res[i].Migration)
			require.NoError(t, err)
			actualMigs[i] = *m
		}

		// Assert that the schema history is correct
		assert.Equal(t, 2, len(res))
		assert.Equal(t, migs, actualMigs)
	})
}

func TestSchemaHistoryDoesNotReturnBaselineMigrations(t *testing.T) {
	t.Parallel()

	t.Run("baseline migration does not appear in schema history", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(state *state.State, db *sql.DB) {
			ctx := context.Background()

			// Create a baseline migration
			err := state.CreateBaseline(ctx, "public", "01_initial_version")
			require.NoError(t, err)

			// Get the schema history
			res, err := state.SchemaHistory(ctx, "public")
			require.NoError(t, err)

			// Assert that the schema history is empty
			assert.Equal(t, 0, len(res))
		})
	})

	t.Run("migrations before the most recent baseline do not appear in the schema history", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(state *state.State, db *sql.DB) {
			ctx := context.Background()

			// Record a migration ahead of the baseline
			stampHistory(ctx, t, state, "public", "00_add_users", "CREATE TABLE users (id int)")

			// Create a baseline migration
			err := state.CreateBaseline(ctx, "public", "01_initial_version")
			require.NoError(t, err)

			// Get the schema history
			res, err := state.SchemaHistory(ctx, "public")
			require.NoError(t, err)

			// Assert that the schema history is empty
			assert.Equal(t, 0, len(res))
		})
	})

	t.Run("migrations after the most recent baseline are included in the history", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(state *state.State, db *sql.DB) {
			ctx := context.Background()

			// Record a migration ahead of the baseline
			stampHistory(ctx, t, state, "public", "00_add_users", "CREATE TABLE users (id int)")

			// Create a baseline migration
			err := state.CreateBaseline(ctx, "public", "01_initial_version")
			require.NoError(t, err)

			// Record a migration after the baseline
			stampHistory(ctx, t, state, "public", "02_add_fruits", "CREATE TABLE fruits (id int)")

			// Get the schema history
			res, err := state.SchemaHistory(ctx, "public")
			require.NoError(t, err)

			// Assert that one migration is present in the schema history
			require.Equal(t, 1, len(res))

			// Deserialize the migration from the history.
			mig, err := migrations.ParseMigration(&res[0].Migration)
			require.NoError(t, err)

			// Assert that the migration is the one that was created after the baseline
			expectedOperations := migrations.Operations{
				&migrations.OpRawSQL{
					Up: "CREATE TABLE fruits (id int)",
				},
			}
			assert.Equal(t, expectedOperations, mig.Operations)
		})
	})
}

func TestLatestBaseline(t *testing.T) {
	t.Parallel()

	t.Run("baseline is the only item in the history", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(state *state.State, db *sql.DB) {
			ctx := context.Background()

			// Create a baseline migration
			err := state.CreateBaseline(ctx, "public", "01_initial_version")
			require.NoError(t, err)

			// Get the latest baseline migration
			baseline, err := state.LatestBaseline(ctx, "public")
			require.NoError(t, err)

			// Assert that the baseline migration is returned
			assert.Equal(t, "01_initial_version", baseline.Name)
		})
	})

	t.Run("several migrations precede the baseline", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(state *state.State, db *sql.DB) {
			ctx := context.Background()

			// Record two migrations ahead of the baseline
			stampHistory(ctx, t, state, "public", "00_add_users", "CREATE TABLE users (id int)")
			stampHistory(ctx, t, state, "public", "00_add_fruits", "CREATE TABLE fruits (id int)")

			// Create a baseline migration
			err := state.CreateBaseline(ctx, "public", "01_initial_version")
			require.NoError(t, err)

			// Get the latest baseline migration
			baseline, err := state.LatestBaseline(ctx, "public")
			require.NoError(t, err)

			// Assert that the baseline migration is returned
			assert.Equal(t, "01_initial_version", baseline.Name)
		})
	})

	t.Run("several migrations precede and come after the baseline", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(state *state.State, db *sql.DB) {
			ctx := context.Background()

			// Record two migrations ahead of the baseline
			stampHistory(ctx, t, state, "public", "00_add_users", "CREATE TABLE users (id int)")
			stampHistory(ctx, t, state, "public", "00_add_fruits", "CREATE TABLE fruits (id int)")

			// Create a baseline migration
			err := state.CreateBaseline(ctx, "public", "01_initial_version")
			require.NoError(t, err)

			// Record a migration after the baseline
			stampHistory(ctx, t, state, "public", "02_add_people", "CREATE TABLE people (id int)")

			// Get the latest baseline migration
			baseline, err := state.LatestBaseline(ctx, "public")
			require.NoError(t, err)

			// Assert that the baseline migration is returned
			assert.Equal(t, "01_initial_version", baseline.Name)
		})
	})

	t.Run("multiple baselines in the history", func(t *testing.T) {
		testutils.WithStateAndConnectionToContainer(t, func(state *state.State, db *sql.DB) {
			ctx := context.Background()

			// Craete a baseline migration
			err := state.CreateBaseline(ctx, "public", "01_initial_version")
			require.NoError(t, err)

			// Record two migrations between the baselines
			stampHistory(ctx, t, state, "public", "01_add_users", "CREATE TABLE users (id int)")
			stampHistory(ctx, t, state, "public", "01_add_fruits", "CREATE TABLE fruits (id int)")

			// Create a baseline migration
			err = state.CreateBaseline(ctx, "public", "02_another_baseline")
			require.NoError(t, err)

			// Record a migration after the second baseline
			stampHistory(ctx, t, state, "public", "03_add_people", "CREATE TABLE people (id int)")

			// Get the latest baseline migration
			baseline, err := state.LatestBaseline(ctx, "public")
			require.NoError(t, err)

			// Assert that the baseline migration is returned
			assert.Equal(t, "02_another_baseline", baseline.Name)
		})
	})
}

// stampHistory records an ordinary pgroll history row carrying a single raw
// SQL operation. Tests below only need *a* history entry with a known body;
// before the DDL-capture triggers were removed they got one by running the
// statement out of band and letting the trigger record it.
func stampHistory(ctx context.Context, t *testing.T, st *state.State, schemaName, name, upSQL string) {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"name":       name,
		"operations": []any{map[string]any{"sql": map[string]any{"up": upSQL}}},
	})
	require.NoError(t, err)
	require.NoError(t, st.Stamp(ctx, schemaName, name, body, nil, nil, ""))
}

// legacyInferredName is the shape the removed DDL-capture trigger gave its
// rows: the latest non-inferred migration name plus a microsecond timestamp.
const legacyInferredName = "01_initial_migration_20260101120000000001"

// stampInferred records a row of the kind the removed DDL-capture trigger used
// to insert. Databases initialized by an older pgroll still hold these, so the
// behaviour they produce is still under test even though nothing creates them
// any more.
func stampInferred(ctx context.Context, t *testing.T, st *state.State, schemaName, name, upSQL string) {
	t.Helper()

	body, err := json.Marshal(map[string]any{
		"version_schema": "sql_0a1b2c3d",
		"operations":     []any{map[string]any{"sql": map[string]any{"up": upSQL}}},
	})
	require.NoError(t, err)
	require.NoError(t, st.Stamp(ctx, schemaName, name, body, nil, nil, "inferred"))
}

func ptr[T any](v T) *T {
	return &v
}
