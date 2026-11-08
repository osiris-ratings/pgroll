// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"

	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
	"github.com/xataio/pgroll/pkg/state"
)

// The invariant these tests lock: the live version schema, the one apps are
// pinned to, is always readable. It exists, and every view in it answers a
// query, whatever statement a contraction is stopped at, and at every moment
// of a contraction that succeeds.

// fixtureSlots caps how many of these tests hold a database at once: each
// fixture opens several connections, and the shared container's
// max_connections is Postgres' default 100.
var fixtureSlots = make(chan struct{}, 6)

// withFixture is testutils.WithMigratorAndConnectionToContainer inside a
// fixture slot.
func withFixture(t *testing.T, fn func(mig *roll.Roll, db *sql.DB)) {
	t.Helper()
	fixtureSlots <- struct{}{}
	defer func() { <-fixtureSlots }()
	testutils.WithMigratorAndConnectionToContainer(t, fn)
}

// contractionCase is one train to contract: the deferred intermediates, the
// leaf, and the physical state a finished contraction must leave.
type contractionCase struct {
	name  string
	setup string // SQL run on the base deployment before the train
	train []*migrations.Migration
	leaf  *migrations.Migration
	check func(t *testing.T, db *sql.DB)
	// exhaustive: inject a fault at every statement; otherwise sample.
	exhaustive bool
	// examples: the train is every migration in examples/ (what `make
	// examples` applies), in place of base, setup, train and leaf.
	examples bool
}

// examplesDir is the repository's examples, relative to this package.
const examplesDir = "../../examples"

func contractionCorpus() []contractionCase {
	events := func() *migrations.Migration { return leafCreatingEvents("09_leaf") }
	return []contractionCase{
		{
			// The incident: two onComplete raw SQL migrations on one table,
			// then a typed destructive leaf.
			name: "incident_add_then_validate_check",
			train: []*migrations.Migration{
				rawOnComplete("01_add_check",
					"ALTER TABLE t ADD CONSTRAINT t_value_present CHECK (value IS NOT NULL) NOT VALID"),
				rawOnComplete("02_validate_check", "ALTER TABLE t VALIDATE CONSTRAINT t_value_present"),
			},
			leaf: &migrations.Migration{Name: "09_leaf", Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "hot", Column: "name", Down: "''"},
			}},
			check: func(t *testing.T, db *sql.DB) {
				assert.True(t, constraintValidated(t, db, "t_value_present"), "the CHECK must exist, validated")
				assert.False(t, columnExists(t, db, cSchema, "hot", "name"), "the leaf's column must be gone")
			},
			exhaustive: true,
		},
		{
			name: "drop_column",
			train: []*migrations.Migration{{Name: "01_drop_value", Operations: migrations.Operations{
				&migrations.OpDropColumn{Table: "t", Column: "value", Down: "''"},
			}}},
			leaf: events(),
			check: func(t *testing.T, db *sql.DB) {
				assert.False(t, columnExists(t, db, cSchema, "t", "value"))
			},
		},
		{
			name: "drop_table",
			train: []*migrations.Migration{{Name: "01_drop_t", Operations: migrations.Operations{
				&migrations.OpDropTable{Name: "t"},
			}}},
			leaf: events(),
			check: func(t *testing.T, db *sql.DB) {
				assert.False(t, tableExists(t, db, cSchema, "t"))
			},
		},
		{
			name: "rename_column",
			train: []*migrations.Migration{{Name: "01_rename_value", Operations: migrations.Operations{
				&migrations.OpRenameColumn{Table: "t", From: "value", To: "label"},
			}}},
			leaf: events(),
			check: func(t *testing.T, db *sql.DB) {
				assert.True(t, columnExists(t, db, cSchema, "t", "label"))
				assert.False(t, columnExists(t, db, cSchema, "t", "value"))
			},
		},
		{
			name: "rename_table",
			train: []*migrations.Migration{{Name: "01_rename_t", Operations: migrations.Operations{
				&migrations.OpRenameTable{From: "t", To: "t_renamed"},
			}}},
			leaf: events(),
			check: func(t *testing.T, db *sql.DB) {
				assert.True(t, tableExists(t, db, cSchema, "t_renamed"))
				assert.False(t, tableExists(t, db, cSchema, "t"))
			},
		},
		{
			name: "alter_column_type",
			train: []*migrations.Migration{{Name: "01_value_to_text", Operations: migrations.Operations{
				&migrations.OpAlterColumn{Table: "t", Column: "value", Type: ptr("text"), Up: "value", Down: "value"},
			}}},
			leaf: events(),
			check: func(t *testing.T, db *sql.DB) {
				assert.Equal(t, "text", columnType(t, db, "t", "value"))
			},
		},
		{
			name:  "drop_constraint",
			setup: "ALTER TABLE t ADD CONSTRAINT t_value_short CHECK (length(value) < 100)",
			train: []*migrations.Migration{{Name: "01_drop_short", Operations: migrations.Operations{
				&migrations.OpDropConstraint{Table: "t", Name: "t_value_short", Up: "value", Down: "value"},
			}}},
			leaf: events(),
			check: func(t *testing.T, db *sql.DB) {
				assert.False(t, constraintExists(t, db, "t_value_short"))
			},
		},
		{
			name:  "drop_index_concurrently",
			setup: "CREATE INDEX t_value_idx ON t (value)",
			train: []*migrations.Migration{{Name: "01_drop_idx", Operations: migrations.Operations{
				&migrations.OpDropIndex{Name: "t_value_idx"},
			}}},
			leaf: events(),
			check: func(t *testing.T, db *sql.DB) {
				assert.False(t, indexExists(t, db, "t_value_idx"))
			},
		},
		{
			// A duplicator on the primary key column: the tail drops the
			// original, primary key with it, and re-adds the key.
			name: "alter_column_primary_key",
			train: []*migrations.Migration{{Name: "01_id_to_bigint", Operations: migrations.Operations{
				&migrations.OpAlterColumn{Table: "t", Column: "id", Type: ptr("bigint"), Up: "id", Down: "id"},
			}}},
			leaf: events(),
			check: func(t *testing.T, db *sql.DB) {
				assert.Equal(t, "bigint", columnType(t, db, "t", "id"))
				assert.Equal(t, "id", primaryKeyColumns(t, db, "t"))
			},
		},
		{
			name:  "drop_multicolumn_constraint",
			setup: "ALTER TABLE t ADD CONSTRAINT t_id_value_check CHECK (id > 0 AND length(value) < 100)",
			train: []*migrations.Migration{{Name: "01_drop_pair_check", Operations: migrations.Operations{
				&migrations.OpDropMultiColumnConstraint{
					Table: "t", Name: "t_id_value_check",
					Up:   migrations.MultiColumnUpSQL{"id": "id", "value": "value"},
					Down: migrations.MultiColumnDownSQL{"id": "id", "value": "value"},
				},
			}}},
			leaf: events(),
			check: func(t *testing.T, db *sql.DB) {
				assert.False(t, constraintExists(t, db, "t_id_value_check"))
				assert.Equal(t, "id", primaryKeyColumns(t, db, "t"))
			},
		},
		{
			// create_constraint completes inline mid-train, so it is the
			// leaf here: its tail runs at contraction.
			name: "create_constraint_leaf",
			leaf: &migrations.Migration{Name: "09_leaf", Operations: migrations.Operations{
				&migrations.OpCreateConstraint{
					Table: "t", Name: "t_value_unique", Type: migrations.OpCreateConstraintTypeUnique,
					Columns: []string{"value"},
					Up:      migrations.MultiColumnUpSQL{"value": "value"},
					Down:    migrations.MultiColumnDownSQL{"value": "value"},
				},
			}},
			check: func(t *testing.T, db *sql.DB) {
				assert.True(t, constraintExists(t, db, "t_value_unique"))
			},
		},
		{
			// Every example, one train, as `make examples` applies it. It
			// holds chained duplicators on one column (posts.user_id gets a
			// foreign key, then loses it), which must not bind the live view
			// to a duplicate the contraction renames away.
			name:     "examples",
			examples: true,
			check: func(t *testing.T, db *sql.DB) {
				var fks int
				require.NoError(t, db.QueryRow(`SELECT count(*) FROM pg_constraint
					WHERE conrelid = 'public.posts'::regclass AND contype = 'f'`).Scan(&fks))
				assert.Zero(t, fks, "24 dropped posts' foreign key")
				assert.True(t, columnExists(t, db, cSchema, "posts", "user_id"))
			},
		},
		{
			// Raw SQL a live view really depends on: it takes the
			// transactional fallback.
			name:  "raw_sql_conflicting_with_view",
			train: []*migrations.Migration{rawOnComplete("01_raw_drop_value", "ALTER TABLE t DROP COLUMN value")},
			leaf:  events(),
			check: func(t *testing.T, db *sql.DB) {
				assert.False(t, columnExists(t, db, cSchema, "t", "value"))
			},
		},
	}
}

// contractionPath is how a train is left for contraction and contracted:
// with its leaf active (`pgroll migrate` on a shared fleet, then `pgroll
// complete`), or with its leaf queued (FinishContraction).
type contractionPath struct {
	name     string
	apply    func(t *testing.T, mig *roll.Roll, train []*migrations.Migration)
	contract func(ctx context.Context, mig *roll.Roll) error
}

var contractionPaths = []contractionPath{
	{
		name:     "complete",
		apply:    applyAdditiveTrain,
		contract: func(ctx context.Context, mig *roll.Roll) error { return mig.Complete(ctx) },
	},
	{
		name:  "finish_contraction",
		apply: applyDelayedContractionTrain,
		contract: func(ctx context.Context, mig *roll.Roll) error {
			_, _, err := mig.FinishContraction(ctx)
			return err
		},
	},
}

// pgrollComplete is what `pgroll complete` does: complete the active
// migration, or contract what is queued when none is active.
func pgrollComplete(ctx context.Context, mig *roll.Roll) error {
	err := mig.Complete(ctx)
	if errors.Is(err, state.ErrNoActiveMigration) {
		_, _, err = mig.FinishContraction(ctx)
	}
	return err
}

// applyCase builds the base deployment and leaves the case's train ready to
// contract. Returns the live schema and the views it holds.
func applyCase(t *testing.T, mig *roll.Roll, db *sql.DB, c contractionCase, p contractionPath) (string, []string) {
	t.Helper()
	var train []*migrations.Migration
	if c.examples {
		raw, err := mig.UnappliedMigrations(context.Background(), os.DirFS(examplesDir))
		require.NoError(t, err)
		for _, r := range raw {
			m, err := migrations.ParseMigration(r)
			require.NoError(t, err)
			train = append(train, m)
		}
	} else {
		createBase(t, mig, db)
		if c.setup != "" {
			_, err := db.Exec(c.setup)
			require.NoError(t, err)
		}
		train = append(append([]*migrations.Migration{}, c.train...), c.leaf)
	}
	p.apply(t, mig, train)
	live := roll.VersionedSchemaName(cSchema, train[len(train)-1].VersionSchemaName())
	views := liveViews(t, db, live)
	require.NotEmpty(t, views)
	return live, views
}

// faultConn counts the statements pgroll runs on its DDL connection and
// fails the failAt-th: with context.Canceled, or, when cancel is set, by
// cancelling the caller's context and letting the call fail on it.
type faultConn struct {
	db.DB
	mu     sync.Mutex
	calls  int
	failAt int
	cancel context.CancelFunc
	fired  string
}

func (f *faultConn) trip(what string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls != f.failAt {
		return nil
	}
	f.fired = what
	if f.cancel != nil {
		f.cancel()
		return nil
	}
	return context.Canceled
}

func (f *faultConn) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *faultConn) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	if err := f.trip("exec: " + query); err != nil {
		return nil, err
	}
	return f.DB.ExecContext(ctx, query, args...)
}

func (f *faultConn) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	if err := f.trip("query: " + query); err != nil {
		return nil, err
	}
	return f.DB.QueryContext(ctx, query, args...)
}

func (f *faultConn) WithRetryableTransaction(ctx context.Context, fn func(context.Context, *sql.Tx) error) error {
	if err := f.trip("transaction"); err != nil {
		return err
	}
	return f.DB.WithRetryableTransaction(ctx, fn)
}

// countContractionCalls contracts the case once, fault-free, and returns how
// many statements it ran on the DDL connection.
//
// It runs as a subtest of its own so the fixture is closed before the fault
// subtests start: a parent's cleanup waits for all its parallel children.
func countContractionCalls(t *testing.T, c contractionCase, p contractionPath) int {
	t.Helper()
	var calls int
	t.Run("count_statements", func(t *testing.T) {
		withFixture(t, func(mig *roll.Roll, db *sql.DB) {
			applyCase(t, mig, db, c, p)
			fc := &faultConn{}
			fc.DB = roll.SwapConnForTest(mig, fc)
			require.NoError(t, p.contract(context.Background(), mig))
			calls = fc.count()
		})
	})
	require.Positive(t, calls)
	return calls
}

// faultPoints is every statement for an exhaustive case, else a sample that
// keeps both ends and a spread between them. PGROLL_EXHAUSTIVE_FAULTS=1 walks
// every statement of every case, the large ones included.
func faultPoints(k int, exhaustive bool) []int {
	if exhaustive || k <= 12 || os.Getenv("PGROLL_EXHAUSTIVE_FAULTS") == "1" {
		points := make([]int, k)
		for i := range points {
			points[i] = i + 1
		}
		return points
	}
	set := map[int]bool{}
	for _, n := range []int{1, 2, 3, k - 2, k - 1, k} {
		set[n] = true
	}
	for i := 1; i <= 6; i++ {
		set[1+i*(k-1)/7] = true
	}
	points := make([]int, 0, len(set))
	for n := range set {
		points = append(points, n)
	}
	sort.Ints(points)
	return points
}

// TestContractionLiveSchemaSurvivesEveryFault walks every statement boundary
// of a contraction: for each N it fails the Nth statement (or cancels the
// context there), asserts the live schema is still readable, then contracts
// again fault-free and asserts the result. Any code path that removes the
// live schema and returns before restoring it fails here, whichever statement
// it is.
func TestContractionLiveSchemaSurvivesEveryFault(t *testing.T) {
	t.Parallel()

	for _, c := range contractionCorpus() {
		for _, p := range contractionPaths {
			for _, cancelCtx := range []bool{false, true} {
				variant := "error"
				if cancelCtx {
					variant = "cancel"
				}
				t.Run(c.name+"/"+p.name+"/"+variant, func(t *testing.T) {
					t.Parallel()
					k := countContractionCalls(t, c, p)
					for _, n := range faultPoints(k, c.exhaustive) {
						t.Run(fmt.Sprintf("fault_%03d_of_%03d", n, k), func(t *testing.T) {
							t.Parallel()
							withFixture(t, func(mig *roll.Roll, db *sql.DB) {
								live, views := applyCase(t, mig, db, c, p)

								ctx, cancel := context.WithCancel(context.Background())
								defer cancel()
								fc := &faultConn{failAt: n}
								if cancelCtx {
									fc.cancel = cancel
								}
								fc.DB = roll.SwapConnForTest(mig, fc)
								faultErr := p.contract(ctx, mig)
								roll.SwapConnForTest(mig, fc.DB)

								assertLiveReadable(t, db, live, views, fmt.Sprintf(
									"after a fault at statement %d of %d (%s); contraction returned %v",
									n, k, fc.fired, faultErr,
								))
								assert.Empty(t, orphanedDuplicates(t, db), "a duplicator tail left half done "+
									"after a fault at statement %d of %d (%s)", n, k, fc.fired)

								require.NoError(t, pgrollComplete(context.Background(), mig),
									"the fault-free retry must contract (fault was at %d: %s)", n, fc.fired)
								assertLiveReadable(t, db, live, views, "after the retry")
								c.check(t, db)
							})
						})
					}
				})
			}
		}
	}
}

// TestContractionHasNoReadGap runs a successful contraction while a reader,
// pinned to the live schema the way an app is (search_path set to it alone),
// reads every live view in a loop. Waiting on a lock is allowed; an error
// saying the relation or schema is missing is not. A contraction that
// succeeds but leaves even a short gap fails here.
func TestContractionHasNoReadGap(t *testing.T) {
	t.Parallel()

	for _, c := range contractionCorpus() {
		for _, p := range contractionPaths {
			t.Run(c.name+"/"+p.name, func(t *testing.T) {
				t.Parallel()
				withFixture(t, func(mig *roll.Roll, db *sql.DB) {
					live, views := applyCase(t, mig, db, c, p)

					probe := startLiveReader(t, db, live, views)
					require.NoError(t, p.contract(context.Background(), mig))
					reads, missing := probe()

					assert.Positive(t, reads, "the reader must have run during the contraction")
					assert.Empty(t, missing, "the live schema must never be missing a relation")
					assertLiveReadable(t, db, live, views, "after the contraction")
					c.check(t, db)
				})
			})
		}
	}
}

// TestContractionRefusesNonTransactionalViewConflict: raw SQL that a live
// view blocks and that cannot run inside a transaction (here a procedure
// that COMMITs) fails the contraction with the live schema untouched.
func TestContractionRefusesNonTransactionalViewConflict(t *testing.T) {
	t.Parallel()

	for _, p := range contractionPaths {
		t.Run(p.name, func(t *testing.T) {
			t.Parallel()
			withFixture(t, func(mig *roll.Roll, db *sql.DB) {
				c := contractionCase{
					name: "procedure_that_commits",
					setup: `CREATE PROCEDURE drop_value_and_commit() LANGUAGE plpgsql AS $$
						BEGIN ALTER TABLE public.t DROP COLUMN value; COMMIT; END $$`,
					train: []*migrations.Migration{rawOnComplete("01_call", "CALL drop_value_and_commit()")},
					leaf:  leafCreatingEvents("09_leaf"),
				}
				live, views := applyCase(t, mig, db, c, p)

				probe := startLiveReader(t, db, live, views)
				err := p.contract(context.Background(), mig)
				_, missing := probe()

				require.Error(t, err)
				assert.Contains(t, err.Error(), "cannot run inside a transaction")
				assert.Empty(t, missing, "the live schema must never be missing a relation")
				assertLiveReadable(t, db, live, views, "after the refused contraction")
				assert.True(t, viewHasColumn(t, db, live, "t", "value"), "nothing was changed")
				assert.True(t, columnExists(t, db, cSchema, "t", "value"), "nothing was changed")
			})
		})
	}
}

// startLiveReader reads every live view in a loop on one connection whose
// search_path is the live schema alone, until the returned stop is called.
// stop returns the number of reads and the errors that said a relation or
// schema was missing (42P01, 3F000).
func startLiveReader(t *testing.T, db *sql.DB, live string, views []string) (stop func() (int, []string)) {
	t.Helper()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	require.NoError(t, err)
	_, err = conn.ExecContext(ctx, "SET search_path TO "+pq.QuoteIdentifier(live))
	require.NoError(t, err)

	done := make(chan struct{})
	finished := make(chan struct{})
	var reads int
	var missing []string
	go func() {
		defer close(finished)
		for {
			for _, view := range views {
				select {
				case <-done:
					return
				default:
				}
				_, err := conn.ExecContext(ctx, "SELECT 1 FROM "+pq.QuoteIdentifier(view)+" LIMIT 0")
				reads++
				var pqErr *pq.Error
				if errors.As(err, &pqErr) && (pqErr.Code == "42P01" || pqErr.Code == "3F000") {
					missing = append(missing, fmt.Sprintf("%s: %s", view, pqErr.Message))
				}
			}
		}
	}()
	return func() (int, []string) {
		close(done)
		<-finished
		require.NoError(t, conn.Close())
		return reads, missing
	}
}

// assertLiveReadable: the live schema exists, holds exactly the views it
// held before the contraction, and every one answers a query.
func assertLiveReadable(t *testing.T, db *sql.DB, live string, wantViews []string, when string) {
	t.Helper()
	require.True(t, schemaExists(t, db, live), "live schema %q missing %s", live, when)
	require.Equal(t, wantViews, liveViews(t, db, live), "live views changed %s", when)
	for _, view := range wantViews {
		_, err := db.Exec(fmt.Sprintf("SELECT 1 FROM %s.%s LIMIT 0", pq.QuoteIdentifier(live), pq.QuoteIdentifier(view)))
		require.NoError(t, err, "live view %s.%s unreadable %s", live, view, when)
	}
}

func liveViews(t *testing.T, db *sql.DB, live string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT viewname FROM pg_catalog.pg_views WHERE schemaname = $1 ORDER BY viewname`, live)
	require.NoError(t, err)
	defer rows.Close()
	var views []string
	for rows.Next() {
		var v string
		require.NoError(t, rows.Scan(&v))
		views = append(views, v)
	}
	require.NoError(t, rows.Err())
	return views
}

func columnExists(t *testing.T, db *sql.DB, schemaName, table, column string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS(SELECT 1 FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2 AND column_name = $3)`,
		schemaName, table, column).Scan(&exists))
	return exists
}

func columnType(t *testing.T, db *sql.DB, table, column string) string {
	t.Helper()
	var typ string
	require.NoError(t, db.QueryRow(`SELECT data_type FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2 AND column_name = $3`,
		cSchema, table, column).Scan(&typ))
	return typ
}

func constraintExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_constraint WHERE conname = $1)`,
		name).Scan(&exists))
	return exists
}

func constraintValidated(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var validated bool
	err := db.QueryRow(`SELECT convalidated FROM pg_catalog.pg_constraint WHERE conname = $1`, name).Scan(&validated)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	require.NoError(t, err)
	return validated
}

func indexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, cSchema+"."+name).Scan(&exists))
	return exists
}

// orphanedDuplicates lists duplicate columns whose original is gone while the
// duplicate has not been renamed into its place: the state between a
// duplicator tail's drop and its rename.
func orphanedDuplicates(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`
		SELECT c.table_name || '.' || c.column_name
		FROM information_schema.columns c
		WHERE c.table_schema = $1
		  AND c.column_name ~ '^_pgroll_new_.+_[0-9a-f]{8}$'
		  AND NOT EXISTS (
		    SELECT 1 FROM information_schema.columns o
		    WHERE o.table_schema = c.table_schema AND o.table_name = c.table_name
		      AND o.column_name = regexp_replace(c.column_name, '^_pgroll_new_(.+)_[0-9a-f]{8}$', '\1'))`,
		cSchema)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		out = append(out, c)
	}
	require.NoError(t, rows.Err())
	return out
}

func primaryKeyColumns(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	var columns sql.NullString
	require.NoError(t, db.QueryRow(`
		SELECT string_agg(a.attname, ',' ORDER BY a.attnum)
		FROM pg_constraint c
		JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = ANY (c.conkey)
		WHERE c.conrelid = to_regclass($1) AND c.contype = 'p'`, cSchema+"."+table).Scan(&columns))
	return columns.String
}
