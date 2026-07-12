// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/pkg/schema"
)

// recordingDB is a no-op db.DB that records the queries it is asked to
// execute, in order, so a test can assert the order statements are emitted.
type recordingDB struct{ queries []string }

func (r *recordingDB) ExecContext(_ context.Context, query string, _ ...interface{}) (sql.Result, error) {
	r.queries = append(r.queries, query)
	return nil, nil
}

func (r *recordingDB) QueryContext(_ context.Context, _ string, _ ...interface{}) (*sql.Rows, error) {
	return nil, nil
}

func (r *recordingDB) WithRetryableTransaction(_ context.Context, _ func(context.Context, *sql.Tx) error) error {
	return nil
}
func (r *recordingDB) Close() error { return nil }

// TestColumnDuplicationOrderIsDeterministic guards against non-deterministic
// recreated column ordering.
//
// Adding a UNIQUE constraint over existing columns makes pgroll recreate
// ("duplicate") those columns. The order in which the duplicate
// `_pgroll_new_*` columns are ADDed fixes their physical position (attnum) in
// the completed table — completion only drops the originals and renames the
// duplicates, which never changes attnum. So that add order alone decides the
// recreated table's column order.
//
// The duplicator used to emit those ADD COLUMNs by ranging a Go map, whose
// iteration order is randomized per process, so the recreated column order
// varied across applications — the same migration could be recreated as
// `name, id` on one run and `id, name` on another.
//
// Execute must instead emit ADD COLUMN in the operation's declared column
// order on every application. We assert that over many runs: a regression
// (ranging the map) makes the emitted order vary run-to-run and diverge from
// the declared order, failing this almost immediately; the fix emits the
// declared order every time.
func TestColumnDuplicationOrderIsDeterministic(t *testing.T) {
	// Several columns so a regressed (map-ranged) emit order is very unlikely
	// to match the declared order by chance.
	declared := []string{"name", "person_id", "count", "encrypted_details", "extra"}

	cols := make([]*schema.Column, len(declared))
	tableColumns := make(map[string]*schema.Column, len(declared))
	for i, name := range declared {
		c := &schema.Column{Name: name, Type: "text", Nullable: true}
		cols[i] = c
		tableColumns[name] = c
	}
	// A bare table (no constraints/indexes) so Execute emits exactly one
	// ADD COLUMN per duplicated column and nothing else.
	tbl := &schema.Table{Name: "items", Columns: tableColumns}

	want := make([]string, len(declared))
	for i, name := range declared {
		want[i] = TemporaryName(name)
	}

	// Each application builds a fresh map and ranges it once; many independent
	// applications make a regression's run-to-run variation a certainty to catch.
	const applications = 1000
	for i := range applications {
		rec := &recordingDB{}
		d := NewColumnDuplicator(rec, tbl, cols...)
		require.NoError(t, d.Execute(context.Background()))

		require.Equal(t, want, addColumnTargets(rec.queries),
			"duplicate ADD COLUMN order must follow the declared column order on every "+
				"application; ranging the duplicator's map made it depend on Go map "+
				"iteration order (application %d)", i)
	}
}

// addColumnTargets extracts, in order, the column added by each
// `ALTER TABLE ... ADD COLUMN IF NOT EXISTS "<name>" ...` statement.
func addColumnTargets(queries []string) []string {
	const marker = `ADD COLUMN IF NOT EXISTS "`
	targets := make([]string, 0, len(queries))
	for _, q := range queries {
		idx := strings.Index(q, marker)
		if idx == -1 {
			continue
		}
		rest := q[idx+len(marker):]
		if end := strings.IndexByte(rest, '"'); end != -1 {
			targets = append(targets, rest[:end])
		}
	}
	return targets
}
