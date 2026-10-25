// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/xataio/pgroll/pkg/db"
)

// indexProgressPollInterval is how often the build's progress row is read.
// Phase changes are reported as soon as a poll sees them; a phase that stays
// put is reported once per the configured interval.
const indexProgressPollInterval = 2 * time.Second

// startIndexBuildProgress reports the progress of an index build running on
// conn until the returned stop function is called. It is a no-op unless conn
// is an RDB with a Monitor pool and reporting enabled.
//
// The build runs on the migration session, so it is observed from the
// monitor pool: pg_stat_progress_create_index for the phase and counters,
// joined with pg_stat_activity for the session it is waiting on. That join is
// the point: an operator watching a concurrent build otherwise has no way to
// tell "building" from "blocked by a 30-minute snapshot" without querying the
// catalog by hand.
func startIndexBuildProgress(ctx context.Context, conn db.DB, indexName string) (stop func()) {
	noop := func() {}
	rdb, ok := conn.(*db.RDB)
	if !ok || rdb.Monitor == nil {
		return noop
	}
	interval := rdb.EffectiveIndexProgressInterval()
	if interval <= 0 {
		return noop
	}
	pid, ok := backendPID(ctx, conn)
	if !ok {
		return noop
	}

	pollCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		pollIndexBuildProgress(pollCtx, rdb.Monitor, rdb.EffectiveLogger(), indexName, pid, interval)
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

// backendPID returns the pid of the session conn's next statement runs on.
// The migration pool is used as one session, so the pid read here is the one
// the build will run under. False when the connection cannot answer (FakeDB).
func backendPID(ctx context.Context, conn db.DB) (int, bool) {
	rows, err := conn.QueryContext(ctx, "SELECT pg_catalog.pg_backend_pid()")
	if err != nil || rows == nil {
		return 0, false
	}
	defer rows.Close()
	var pid int
	if err := db.ScanFirstValue(rows, &pid); err != nil {
		return 0, false
	}
	return pid, true
}

func pollIndexBuildProgress(ctx context.Context, monitor *sql.DB, logger db.Logger, indexName string, pid int, interval time.Duration) {
	ticker := time.NewTicker(min(indexProgressPollInterval, interval))
	defer ticker.Stop()

	var (
		lastPhase    string
		lastReported time.Time
	)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		progress, found := readIndexBuildProgress(ctx, monitor, pid)
		if !found {
			continue
		}
		phaseChanged := progress.Phase != lastPhase
		if !phaseChanged && time.Since(lastReported) < interval {
			continue
		}
		logger.LogIndexBuildProgress(indexName, progress)
		lastPhase = progress.Phase
		lastReported = time.Now()
	}
}

// readIndexBuildProgress samples the progress view for the build running on
// pid. found is false when the session is between attempts, or the view is
// unavailable; a failed read is treated the same way, since progress reporting
// must never fail a migration.
func readIndexBuildProgress(ctx context.Context, monitor *sql.DB, pid int) (progress db.IndexBuildProgress, found bool) {
	row := monitor.QueryRowContext(ctx, `SELECT
			p.phase,
			coalesce(extract(epoch from clock_timestamp() - a.query_start), 0),
			p.blocks_done, p.blocks_total,
			p.tuples_done, p.tuples_total,
			p.lockers_done, p.lockers_total,
			coalesce(l.pid, 0),
			coalesce(nullif(l.application_name, ''), l.backend_type, ''),
			coalesce(l.state, ''),
			coalesce((clock_timestamp() - l.xact_start)::text, ''),
			left(regexp_replace(coalesce(l.query, ''), '\s+', ' ', 'g'), 120)
		FROM pg_catalog.pg_stat_progress_create_index p
		JOIN pg_catalog.pg_stat_activity a ON a.pid = p.pid
		LEFT JOIN pg_catalog.pg_stat_activity l
			ON p.current_locker_pid <> 0 AND l.pid = p.current_locker_pid
		WHERE p.pid = $1`, pid)

	var (
		elapsedSeconds float64
		blocker        db.Blocker
	)
	err := row.Scan(
		&progress.Phase,
		&elapsedSeconds,
		&progress.BlocksDone, &progress.BlocksTotal,
		&progress.TuplesDone, &progress.TuplesTotal,
		&progress.LockersDone, &progress.LockersTotal,
		&blocker.PID,
		&blocker.ApplicationName,
		&blocker.State,
		&blocker.TransactionAge,
		&blocker.Query,
	)
	if err != nil {
		return db.IndexBuildProgress{}, false
	}
	progress.Elapsed = time.Duration(elapsedSeconds * float64(time.Second))
	if blocker.PID != 0 {
		progress.Blocker = &blocker
	}
	return progress, true
}

// indexBuildProgressArgs renders one progress sample as logger key/value
// pairs. Counters are included only when Postgres reports a total for the
// current phase, so a line never shows a meaningless 0/0.
func indexBuildProgressArgs(index string, p db.IndexBuildProgress) []any {
	args := []any{
		"index", index,
		"phase", p.Phase,
		"elapsed", p.Elapsed.Round(time.Second).String(),
	}
	if p.BlocksTotal > 0 {
		args = append(args, "blocks", ratio(p.BlocksDone, p.BlocksTotal))
	}
	if p.TuplesTotal > 0 {
		args = append(args, "tuples", ratio(p.TuplesDone, p.TuplesTotal))
	}
	if p.LockersTotal > 0 {
		args = append(args, "lockers_done", fmt.Sprintf("%d/%d", p.LockersDone, p.LockersTotal))
	}
	if b := p.Blocker; b != nil {
		args = append(args, "waiting_on", fmt.Sprintf("pid=%d source=%s state=%s xact_age=%s query=%q",
			b.PID, b.ApplicationName, b.State, b.TransactionAge, b.Query))
	}
	return args
}

func ratio(done, total int64) string {
	return fmt.Sprintf("%d/%d (%.1f%%)", done, total, 100*float64(done)/float64(total))
}
