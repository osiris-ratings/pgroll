// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/roll"
)

// TestStartRefusesBaselineExecutionIntoHistory drives runMigrationFromFile —
// the `pgroll start` execution path — against a real database to prove the
// baseline execute-guard: a `baseline: true` file may bootstrap a database
// with no history, and must be refused once history exists (the
// truncated-history trap, where executing it would replay the whole schema).
func TestStartRefusesBaselineExecutionIntoHistory(t *testing.T) {
	t.Parallel()

	const markedBaseline = `{"baseline": true, "irreversible": true, "operations": [{"sql": {"up": "SELECT 1"}}]}`

	writeFile := func(t *testing.T, dir, name, content string) string {
		t.Helper()
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}

	t.Run("a fresh database may bootstrap by executing the baseline", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			path := writeFile(t, t.TempDir(), "01_base.json", markedBaseline)

			err := runMigrationFromFile(ctx, m, path, true, backfill.NewConfig())
			require.NoError(t, err)
		})
	})

	t.Run("a database with history refuses to execute a baseline", func(t *testing.T) {
		testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
			ctx := context.Background()
			dir := t.TempDir()

			plain := writeFile(t, dir, "01_plain.json",
				`{"operations": [{"sql": {"up": "SELECT 1", "down": "SELECT 1"}}]}`)
			require.NoError(t, runMigrationFromFile(ctx, m, plain, true, backfill.NewConfig()))

			marked := writeFile(t, dir, "02_base.json", markedBaseline)
			err := runMigrationFromFile(ctx, m, marked, true, backfill.NewConfig())
			require.Error(t, err)
			require.ErrorContains(t, err, "refusing to execute baseline migration")
			require.ErrorContains(t, err, "02_base")
		})
	})
}
