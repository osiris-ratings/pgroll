// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

// TestRecoveryIsReachable drives the pre-flight classifier through its real
// caller rather than passing `stateInSync` straight into the pure function.
//
// The pure-function test could not have caught this: `stateInSync` was derived
// from state.LatestVersion, which resolves through find_version_schema, whose
// WHERE clause requires the schema to exist — so it was compared against a
// superset of itself and was always true. RECOVERY was unreachable and its
// unit test was green.
// The cmd package's other tests are pure functions over inputs; this one has
// to drive the real caller, so the package needs a database.
func TestMain(m *testing.M) {
	testutils.SharedTestMain(m)
}

func TestRecoveryIsReachable(t *testing.T) {
	t.Parallel()

	mig := &migrations.Migration{
		Name:       "01_create",
		Operations: migrations.Operations{&migrations.OpRawSQL{Up: "SELECT 1", Down: "SELECT 1"}},
	}

	// classifyCycle short-circuits to NO-OP when nothing is outstanding, and
	// that precedence is deliberate: `migrate` with no work to do reports no
	// work. RECOVERY is about a run that HAS work and finds the projection
	// missing, so every case here supplies one.
	pending := []*migrations.RawMigration{{Name: "02_next"}}

	t.Run("a deployed projection is in sync", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			require.NoError(t, m.Start(ctx, mig, backfill.NewConfig()))
			require.NoError(t, m.Complete(ctx))

			state, err := printMigratePreFlight(ctx, m, "", pending, io.Discard)
			require.NoError(t, err)
			require.NotEqual(t, cycleRecovery, state)
		})
	})

	t.Run("a dropped projection classifies as RECOVERY", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, db *sql.DB) {
			ctx := context.Background()
			require.NoError(t, m.Start(ctx, mig, backfill.NewConfig()))
			require.NoError(t, m.Complete(ctx))

			// History says 01_create; its projection is gone. This is exactly
			// what `pgroll materialize` repairs, and what the warning claims
			// to detect.
			_, err := db.ExecContext(ctx,
				fmt.Sprintf("DROP SCHEMA %s CASCADE", roll.VersionedSchemaName(m.Schema(), mig.Name)))
			require.NoError(t, err)

			state, err := printMigratePreFlight(ctx, m, "", pending, io.Discard)
			require.NoError(t, err)
			require.Equal(t, cycleRecovery, state)
		})
	})

	t.Run("version schemas disabled is always in sync", func(t *testing.T) {
		testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, "public",
			[]roll.Option{roll.WithLockTimeoutMs(500), roll.WithVersionSchema(false)},
			func(m *roll.Roll, _ *sql.DB) {
				ctx := context.Background()
				require.NoError(t, m.Start(ctx, mig, backfill.NewConfig()))
				require.NoError(t, m.Complete(ctx))

				// No projection exists by construction, so a missing one is not
				// evidence of anything.
				state, err := printMigratePreFlight(ctx, m, "", pending, io.Discard)
				require.NoError(t, err)
				require.NotEqual(t, cycleRecovery, state)
			})
	})
}

// TestInterruptedMessage covers the remedy an operator is handed mid-incident.
//
// The old text named only `pgroll rollback`, which undoes the ONE active
// migration. In a batch that is almost never the whole story: a run that dies
// partway leaves earlier migrations applied, unsealed, and carrying queued
// contraction, and the message said nothing about them.
func TestInterruptedMessage(t *testing.T) {
	t.Parallel()

	raw := func(name string) *migrations.Migration {
		return &migrations.Migration{
			Name:       name,
			Operations: migrations.Operations{&migrations.OpRawSQL{Up: "SELECT 1", Down: "SELECT 1"}},
		}
	}

	t.Run("names both verbs and counts the rest of the batch", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()

			// Two applied-and-uncontracted, then one left active: the shape a
			// batch interrupted partway through leaves behind.
			for _, name := range []string{"01_a", "02_b"} {
				require.NoError(t, m.Start(ctx, raw(name), backfill.NewConfig(),
					roll.WithoutVersionSchema()))
				require.NoError(t, m.Complete(ctx, roll.WithSkipSchemaDrop()))
			}
			require.NoError(t, m.Start(ctx, raw("03_c"), backfill.NewConfig()))

			msg := interruptedMessage(ctx, m, "03_c")
			require.Contains(t, msg, "pgroll rollback")
			require.Contains(t, msg, "pgroll revert",
				"the verb that walks back the whole window must be named")
			require.Contains(t, msg, "03_c")
		})
	})

	t.Run("says so when nothing else is uncontracted", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			require.NoError(t, m.Start(ctx, raw("01_only"), backfill.NewConfig()))

			msg := interruptedMessage(ctx, m, "01_only")
			require.Contains(t, msg, "pgroll rollback")
			require.Contains(t, msg, "Nothing else is applied-but-uncontracted")
		})
	})
}
