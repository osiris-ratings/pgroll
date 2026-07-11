// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

// TestRevertSealed proves the inversion engine end to end: a sealed
// deployment (contraction drained, expand artifacts gone) is reverted by
// running synthesized inverse migrations forward through the engine, then
// pruning both directions from history — schema restored exactly, data
// re-derived best-effort, history identical to the boundary state.
func TestRevertSealed(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// Boundary deployment: users(id, name, email), sealed with a clean
		// snapshot.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "00_create_users",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE users (id integer PRIMARY KEY, name text, email text)`,
					Down: `DROP TABLE users`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		_, err := db.ExecContext(ctx,
			`INSERT INTO users (id, name, email) VALUES (1, 'ada', 'ada@example.com')`)
		require.NoError(t, err)

		// The deployment to be reverted: destructive + additive + raw SQL.
		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{
				Name: "01_drop_email",
				Operations: migrations.Operations{
					&migrations.OpDropColumn{Table: "users", Column: "email", Down: "'rederived@example.com'"},
				},
			},
			{
				Name: "02_add_age",
				Operations: migrations.Operations{
					&migrations.OpAddColumn{
						Table:  "users",
						Up:     "18",
						Column: migrations.Column{Name: "age", Type: "integer", Nullable: true},
					},
				},
			},
			{
				Name: "03_widen_name",
				Operations: migrations.Operations{
					&migrations.OpAlterColumn{
						Table:  "users",
						Column: "name",
						Type:   ptr("varchar(255)"),
						Up:     "name",
						Down:   "name",
					},
				},
			},
			{
				Name: "04_create_events",
				Operations: migrations.Operations{
					&migrations.OpRawSQL{
						Up:   `CREATE TABLE events (id integer PRIMARY KEY)`,
						Down: `DROP TABLE events`,
					},
				},
			},
		})

		// CONTRACT it: the drain runs, email's data is physically
		// destroyed. The drained count covers the deferred rows (01, 03,
		// 04); the inline 02 had no queued work but is stamped sealed too.
		drained, _, err := mig.FinishContraction(ctx)
		require.NoError(t, err)
		require.Equal(t, 3, drained)

		var emailExists bool
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'email'
			)`, cSchema).Scan(&emailExists))
		require.False(t, emailExists, "seal must have drained the drop")

		// The window is closed: a plain revert has nothing to do, and a
		// window-bounded --to refuses with the sealed message that points
		// here.
		targets, err := mig.RevertTargets(ctx)
		require.NoError(t, err)
		require.Empty(t, targets)
		_, err = mig.RevertPlan(ctx, roll.WithRevertTo("00_create_users"))
		require.ErrorContains(t, err, "sealed")

		// Sealed revert: plan first.
		plan, err := mig.PlanRevertSealed(ctx, "00_create_users")
		require.NoError(t, err)
		require.NotNil(t, plan)
		assert.Equal(t, []string{"04_create_events", "03_widen_name", "02_add_age", "01_drop_email"}, plan.Targets)
		require.Len(t, plan.Inverses, 4)
		assert.Equal(t, "revert_04_create_events", plan.Inverses[0].Name)
		assert.Equal(t, "00_create_users", plan.BoundaryVersionSchema)

		// Execute.
		result, err := mig.RevertSealed(ctx, "00_create_users", backfill.NewConfig())
		require.NoError(t, err)
		require.NotNil(t, result)

		// Schema restored exactly: events and age gone, email back.
		assert.False(t, tableExists(t, db, cSchema, "events"))
		var ageExists bool
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'age'
			)`, cSchema).Scan(&ageExists))
		assert.False(t, ageExists)
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'email'
			)`, cSchema).Scan(&emailExists))
		assert.True(t, emailExists, "sealed revert must restore the dropped column's shape")

		// The widened column is back at its prior type.
		var nameType string
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT data_type FROM information_schema.columns
			WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'name'
		`, cSchema).Scan(&nameType))
		assert.Equal(t, "text", nameType, "alter_column inverse must restore the prior type")

		// Data is best-effort by construction: the original values were
		// destroyed at the seal; the inverse re-derived them through the
		// forward migration's down expression.
		var email string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT email FROM users WHERE id = 1`).Scan(&email))
		assert.Equal(t, "rederived@example.com", email)

		// History rewound to the boundary with no inverse residue.
		latest, err := mig.State().LatestMigration(ctx, cSchema)
		require.NoError(t, err)
		require.NotNil(t, latest)
		assert.Equal(t, "00_create_users", *latest)
		var leftover int
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT count(*) FROM pgroll.migrations
			WHERE schema = $1 AND (name LIKE 'revert_%' OR name IN ('01_drop_email', '02_add_age', '03_widen_name', '04_create_events'))
		`, cSchema).Scan(&leftover))
		assert.Zero(t, leftover, "forward and inverse rows must both be pruned")

		// The boundary's version schema is live again.
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "00_create_users")))

		// The segment can be re-applied cleanly.
		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{
				Name: "01_drop_email",
				Operations: migrations.Operations{
					&migrations.OpDropColumn{Table: "users", Column: "email", Down: "''"},
				},
			},
		})
		targets, err = mig.RevertTargets(ctx)
		require.NoError(t, err)
		assert.Len(t, targets, 1)
	})
}

// TestRevertSealedGuards proves the refusal surface: open windows, unknown
// boundaries, and irreversible segments are refused with clear errors.
func TestRevertSealedGuards(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "00_create_users",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE users (id integer PRIMARY KEY)`,
					Down: `DROP TABLE users`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		// Open window: refuse.
		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{
				Name: "01_create_events",
				Operations: migrations.Operations{
					&migrations.OpRawSQL{
						Up:   `CREATE TABLE events (id integer PRIMARY KEY)`,
						Down: `DROP TABLE events`,
					},
				},
			},
		})
		_, err := mig.PlanRevertSealed(ctx, "00_create_users")
		require.ErrorContains(t, err, "revert window is open")

		// Contract, then: unknown boundary refused.
		_, _, err = mig.FinishContraction(ctx)
		require.NoError(t, err)
		_, err = mig.PlanRevertSealed(ctx, "no_such_migration")
		require.ErrorContains(t, err, "not found")

		// Irreversible migrations make the segment non-invertible.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name:         "02_irreversible",
			Irreversible: true,
			Operations: migrations.Operations{
				&migrations.OpRawSQL{Up: `CREATE TABLE audit (id integer)`},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		_, err = mig.PlanRevertSealed(ctx, "00_create_users")
		require.ErrorContains(t, err, "irreversible")
	})
}

// TestExpandOnlyRevertSplitFlow covers the zero-downtime revert choreography:
// `revert --to X --expand-only` applies the inverse train but leaves the
// final inverse ACTIVE, with the restored boundary projection existing
// alongside the current one so apps can repin; `pgroll complete` then
// contracts the inverses and FinishPendingSealedRevert prunes history back
// to the boundary.
func TestExpandOnlyRevertSplitFlow(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "00_create_users",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{
					Up:   `CREATE TABLE users (id integer PRIMARY KEY, email text)`,
					Down: `DROP TABLE users`,
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		applyDelayedContractionTrain(t, mig, []*migrations.Migration{
			{
				Name: "01_drop_email",
				Operations: migrations.Operations{
					&migrations.OpDropColumn{Table: "users", Column: "email", Down: "'x@example.com'"},
				},
			},
		})
		drained, _, err := mig.FinishContraction(ctx)
		require.NoError(t, err)
		require.Equal(t, 1, drained)

		// Expand-only: the inverse train applies but stops before its final
		// Complete — both projections exist side by side for the repin.
		plan, err := mig.RevertSealed(ctx, "00_create_users", nil, roll.WithExpandOnly())
		require.NoError(t, err)
		require.NotNil(t, plan)

		active, err := mig.State().GetActiveMigration(ctx, cSchema)
		require.NoError(t, err)
		assert.Equal(t, "revert_01_drop_email", active.Name)
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "00_create_users")),
			"the restored boundary projection must exist for the repin")
		assert.True(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "01_drop_email")),
			"the current projection must survive until the completing step")

		// `pgroll complete` contracts the inverse; FinishPendingSealedRevert
		// prunes history back to the boundary (the CLI runs both).
		require.NoError(t, mig.Complete(ctx))
		finished, err := mig.FinishPendingSealedRevert(ctx)
		require.NoError(t, err)
		require.NotNil(t, finished)
		assert.Equal(t, "00_create_users", finished.Boundary)

		latest, err := mig.State().LatestMigration(ctx, cSchema)
		require.NoError(t, err)
		require.NotNil(t, latest)
		assert.Equal(t, "00_create_users", *latest)

		// Schema restored: email is back (re-derived through the original
		// down), and only the boundary projection remains.
		var emailExists bool
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'users' AND column_name = 'email'
			)`, cSchema).Scan(&emailExists))
		assert.True(t, emailExists, "the reverted drop_column must be physically undone")
		assert.False(t, schemaExists(t, db, roll.VersionedSchemaName(cSchema, "01_drop_email")),
			"the reverted deployment's projection must be gone")

		// The forward migration is tombstoned against silent re-apply.
		tombstones, err := mig.State().RevertedMigrations(ctx, cSchema)
		require.NoError(t, err)
		_, ok := tombstones["01_drop_email"]
		assert.True(t, ok, "the pruned forward migration must be tombstoned")
	})
}
