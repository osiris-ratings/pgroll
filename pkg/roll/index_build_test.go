// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

// A concurrent index build must out-wait every transaction in the database
// holding an older snapshot, whatever table it reads. An index on a table the
// same migration created has no such wait, and an index on a pre-existing
// table reports the session it is stuck behind while it waits.
func TestIndexBuildsOnNewTablesSkipTheSnapshotWaitAndOldTablesReportIt(t *testing.T) {
	t.Parallel()

	rec := &recordingLogger{Logger: migrations.NewNoopLogger()}
	opts := []roll.Option{
		roll.WithLockTimeoutMs(500),
		roll.WithLockRetryTimeout(3 * time.Second),
		roll.WithIndexProgressInterval(200 * time.Millisecond),
		roll.WithLogger(rec),
	}
	testutils.WithMigratorInSchemaAndConnectionToContainerWithOptions(t, cSchema, opts, func(mig *roll.Roll, conn *sql.DB) {
		ctx := context.Background()

		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name:       "01_tables",
			Operations: migrations.Operations{createTableOp("anchor"), createTableOp("bystander")},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))

		// A transaction holding a snapshot on a table none of the migrations
		// below touch.
		holder, err := conn.Conn(ctx)
		require.NoError(t, err)
		defer holder.Close()
		_, err = holder.ExecContext(ctx, "BEGIN ISOLATION LEVEL REPEATABLE READ")
		require.NoError(t, err)
		var holderPID, rows int
		require.NoError(t, holder.QueryRowContext(ctx, "SELECT pg_backend_pid()").Scan(&holderPID))
		require.NoError(t, holder.QueryRowContext(ctx, "SELECT count(*) FROM bystander").Scan(&rows))

		// A table created in this migration gets its index without waiting on
		// the holder; a concurrent build would exhaust the 3s lock budget.
		require.NoError(t, mig.Start(ctx, &migrations.Migration{
			Name: "02_new_table_with_index",
			Operations: migrations.Operations{
				createTableOp("fresh"),
				&migrations.OpCreateIndex{
					Name:    "idx_fresh_name",
					Table:   "fresh",
					Columns: []migrations.IndexField{{Column: "name"}},
				},
			},
		}, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))
		indexMustExist(t, conn, cSchema, "fresh", "idx_fresh_name")
		assert.Empty(t, rec.events(), "a plain build has no progress to report")

		// An index on a pre-existing table waits on the holder's snapshot
		// until the lock budget runs out, and names the holder while it waits.
		indexExisting := &migrations.Migration{
			Name: "03_index_existing",
			Operations: migrations.Operations{
				&migrations.OpCreateIndex{
					Name:    "idx_anchor_name",
					Table:   "anchor",
					Columns: []migrations.IndexField{{Column: "name"}},
				},
			},
		}
		err = mig.Start(ctx, indexExisting, backfill.NewConfig())
		require.Error(t, err)
		assert.Contains(t, err.Error(), "older snapshot")

		var reported bool
		for _, e := range rec.events() {
			if e.Phase == "waiting for old snapshots" && e.Blocker != nil && e.Blocker.PID == holderPID {
				reported = true
				assert.Equal(t, "idle in transaction", e.Blocker.State)
				break
			}
		}
		assert.True(t, reported, "expected a progress report waiting on pid %d, got %+v", holderPID, rec.events())

		// Once the holder is gone the same build goes through.
		_, err = holder.ExecContext(ctx, "ROLLBACK")
		require.NoError(t, err)
		require.NoError(t, mig.Start(ctx, indexExisting, backfill.NewConfig()))
		require.NoError(t, mig.Complete(ctx))
		indexMustExist(t, conn, cSchema, "anchor", "idx_anchor_name")
	})
}

// recordingLogger keeps every index build progress report; everything else is
// discarded.
type recordingLogger struct {
	migrations.Logger

	mu       sync.Mutex
	progress []db.IndexBuildProgress
}

func (l *recordingLogger) LogIndexBuildProgress(_ string, progress db.IndexBuildProgress) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.progress = append(l.progress, progress)
}

func (l *recordingLogger) events() []db.IndexBuildProgress {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]db.IndexBuildProgress(nil), l.progress...)
}

func indexMustExist(t *testing.T, conn *sql.DB, schema, table, index string) {
	t.Helper()
	var exists bool
	err := conn.QueryRow(`SELECT EXISTS (
		SELECT 1 FROM pg_catalog.pg_indexes
		WHERE schemaname = $1 AND tablename = $2 AND indexname = $3)`,
		schema, table, index).Scan(&exists)
	require.NoError(t, err)
	assert.True(t, exists, "expected index %s on %s.%s", index, schema, table)
}
