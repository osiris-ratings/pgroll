// SPDX-License-Identifier: Apache-2.0

package state_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/state"
)

// TestOnlyOneActiveIsEnforced pins the constraint the index is named for.
//
// It used to be indexed on (schema, name, done) WHERE done = FALSE, which
// enforced nothing: (schema, name) is already the PRIMARY KEY, so at most one
// row exists per pair regardless and the partial index was trivially
// satisfied. Two *different* migrations could both sit done = FALSE, and
// GetActiveMigration would return whichever one Postgres yielded first.
//
// This test fails against that index and passes against (schema) WHERE
// done = FALSE.
func TestOnlyOneActiveIsEnforced(t *testing.T) {
	t.Parallel()

	testutils.WithStateAndConnectionToContainer(t, func(st *state.State, db *sql.DB) {
		ctx := context.Background()

		insert := func(name, parent string) error {
			var parentArg any
			if parent != "" {
				parentArg = parent
			}
			_, err := db.ExecContext(ctx,
				`INSERT INTO pgroll.migrations (schema, name, parent, migration, done)
				 VALUES ('public', $1, $2, '{"operations":[]}'::jsonb, FALSE)`,
				name, parentArg)
			return err
		}

		require.NoError(t, insert("01_first", ""))

		// A second active migration, chained off the first so history_is_linear
		// cannot be what rejects it. Only only_one_active can.
		err := insert("02_second", "01_first")
		require.Error(t, err, "a second active migration must be rejected")
		require.Contains(t, err.Error(), "only_one_active")

		_ = st
	})
}
