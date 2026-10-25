// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/xataio/pgroll/pkg/db"
)

func TestIndexBuildProgressArgsOmitCountersWithoutATotal(t *testing.T) {
	t.Parallel()

	args := indexBuildProgressArgs("idx_users_name", db.IndexBuildProgress{
		Phase:   "building index",
		Elapsed: 100 * time.Second,
	})

	assert.Equal(t, []any{
		"index", "idx_users_name",
		"phase", "building index",
		"elapsed", "1m40s",
	}, args)
}

func TestIndexBuildProgressArgsRenderCountersAndTheBlocker(t *testing.T) {
	t.Parallel()

	args := indexBuildProgressArgs("idx_users_name", db.IndexBuildProgress{
		Phase:        "waiting for old snapshots",
		Elapsed:      754 * time.Second,
		BlocksDone:   250,
		BlocksTotal:  1000,
		LockersDone:  1,
		LockersTotal: 4,
		Blocker: &db.Blocker{
			PID:             2725393,
			ApplicationName: "osiris-typesense-job",
			State:           "active",
			TransactionAge:  "00:12:31.5",
			Query:           "SELECT CAST(row_to_json(t) AS VARCHAR) FROM (SELECT ...) AS t",
		},
	})

	assert.Equal(t, []any{
		"index", "idx_users_name",
		"phase", "waiting for old snapshots",
		"elapsed", "12m34s",
		"blocks", "250/1000 (25.0%)",
		"lockers_done", "1/4",
		"waiting_on", `pid=2725393 source=osiris-typesense-job state=active xact_age=00:12:31.5 query="SELECT CAST(row_to_json(t) AS VARCHAR) FROM (SELECT ...) AS t"`,
	}, args)
}
