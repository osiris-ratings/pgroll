// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/cloudflare/backoff"
	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
)

const (
	lockNotAvailableErrorCode pqerror.Code = "55P03"
	maxBackoffDuration                     = 1 * time.Minute
	backoffInterval                        = 1 * time.Second

	// DefaultLockRetryTimeout is the total wall-clock budget spent retrying
	// lock_timeout errors before giving up. Used when RDB.LockRetryTimeout is
	// zero so library callers get sensible behavior without configuration.
	DefaultLockRetryTimeout = 5 * time.Minute

	// DefaultIndexProgressInterval is how often a concurrent index build
	// reports its progress when RDB.IndexProgressInterval is zero.
	DefaultIndexProgressInterval = 10 * time.Second
)

type DB interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	WithRetryableTransaction(ctx context.Context, f func(context.Context, *sql.Tx) error) error
	Close() error
}

// Logger is the minimal logging interface needed by RDB to emit lock_timeout
// retry events. It is satisfied structurally by any logger that implements
// these methods (including pkg/migrations.Logger).
type Logger interface {
	LogLockTimeoutRetry(query string, attempt int, sleep, elapsed, budget time.Duration)
	LogLockTimeoutGiveUp(attempts int, elapsed time.Duration)
	LogLockTimeoutInterrupted(attempts int, elapsed time.Duration)
	LogIndexBuildProgress(index string, progress IndexBuildProgress)
}

// IndexBuildProgress is one sample of pg_stat_progress_create_index for a
// build pgroll is running, joined with the session it is currently waiting
// on, if any.
type IndexBuildProgress struct {
	Phase   string
	Elapsed time.Duration

	BlocksDone, BlocksTotal   int64
	TuplesDone, TuplesTotal   int64
	LockersDone, LockersTotal int64

	// Blocker is the session the build is waiting on right now, when the
	// phase is a wait and pg_stat_activity still knows the pid.
	Blocker *Blocker
}

// Blocker describes a session an index build is waiting on.
type Blocker struct {
	PID             int
	ApplicationName string
	State           string
	TransactionAge  string
	Query           string
}

// noopLogger is used when no logger is configured on RDB.
type noopLogger struct{}

func (noopLogger) LogLockTimeoutRetry(string, int, time.Duration, time.Duration, time.Duration) {
}
func (noopLogger) LogLockTimeoutGiveUp(int, time.Duration)          {}
func (noopLogger) LogLockTimeoutInterrupted(int, time.Duration)     {}
func (noopLogger) LogIndexBuildProgress(string, IndexBuildProgress) {}

// RDB wraps a *sql.DB and retries queries using an exponential backoff (with
// jitter) on lock_timeout errors, up to a configurable wall-clock budget.
type RDB struct {
	DB *sql.DB

	// LockRetryTimeout caps the total wall-clock time spent retrying a single
	// call when Postgres returns SQLSTATE 55P03 (lock_not_available). Zero
	// uses DefaultLockRetryTimeout. Negative disables retries entirely.
	LockRetryTimeout time.Duration

	// Logger receives lock_timeout retry events. May be nil.
	Logger Logger

	// Monitor is a second, independent pool used to observe the migration
	// session while it is busy (progress views, blocker lookups). It must
	// not be DB: that pool is treated as one session carrying session-level
	// SETs, and a second connection drawn from it would be handed back to the
	// next statement without them. May be nil, which disables observation.
	Monitor *sql.DB

	// IndexProgressInterval is how often a concurrent index build reports
	// progress through Logger. Zero uses DefaultIndexProgressInterval;
	// negative disables reporting.
	IndexProgressInterval time.Duration
}

// IndexProgressInterval returns the effective progress-reporting interval, or
// a value <= 0 when reporting is disabled.
func (db *RDB) EffectiveIndexProgressInterval() time.Duration {
	if db.IndexProgressInterval == 0 {
		return DefaultIndexProgressInterval
	}
	return db.IndexProgressInterval
}

func (db *RDB) logger() Logger {
	if db.Logger == nil {
		return noopLogger{}
	}
	return db.Logger
}

// EffectiveLogger returns the configured Logger, or a no-op one.
func (db *RDB) EffectiveLogger() Logger { return db.logger() }

func (db *RDB) retryBudget() time.Duration {
	if db.LockRetryTimeout == 0 {
		return DefaultLockRetryTimeout
	}
	return db.LockRetryTimeout
}

// RetryBudget returns the effective wall-clock budget for lock_timeout
// retries (negative when retries are disabled). Exposed so long-running
// operations like concurrent index builds can align their own deadlines
// with the retry layer instead of inventing a second budget.
func (db *RDB) RetryBudget() time.Duration {
	return db.retryBudget()
}

// ExecContext wraps sql.DB.ExecContext, retrying queries on lock_timeout
// errors up to the configured retry budget.
func (db *RDB) ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error) {
	b := backoff.New(maxBackoffDuration, backoffInterval)
	budget := db.retryBudget()
	start := time.Now()
	deadline := start.Add(budget)
	attempt := 0
	log := db.logger()

	for {
		res, err := db.DB.ExecContext(ctx, query, args...)
		if err == nil {
			return res, nil
		}

		if !isLockNotAvailable(err) {
			return nil, err
		}

		attempt++
		if budget < 0 {
			log.LogLockTimeoutGiveUp(attempt, time.Since(start))
			return nil, err
		}

		sleep := b.Duration()
		if remaining := time.Until(deadline); remaining <= 0 {
			log.LogLockTimeoutGiveUp(attempt, time.Since(start))
			return nil, err
		} else if sleep > remaining {
			sleep = remaining
		}

		log.LogLockTimeoutRetry(query, attempt, sleep, time.Since(start), budget)

		if err := sleepCtx(ctx, sleep); err != nil {
			log.LogLockTimeoutInterrupted(attempt, time.Since(start))
			return nil, err
		}
	}
}

// QueryContext wraps sql.DB.QueryContext, retrying queries on lock_timeout
// errors up to the configured retry budget.
func (db *RDB) QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error) {
	b := backoff.New(maxBackoffDuration, backoffInterval)
	budget := db.retryBudget()
	start := time.Now()
	deadline := start.Add(budget)
	attempt := 0
	log := db.logger()

	for {
		rows, err := db.DB.QueryContext(ctx, query, args...)
		if err == nil {
			return rows, nil
		}

		if !isLockNotAvailable(err) {
			return nil, err
		}

		attempt++
		if budget < 0 {
			log.LogLockTimeoutGiveUp(attempt, time.Since(start))
			return nil, err
		}

		sleep := b.Duration()
		if remaining := time.Until(deadline); remaining <= 0 {
			log.LogLockTimeoutGiveUp(attempt, time.Since(start))
			return nil, err
		} else if sleep > remaining {
			sleep = remaining
		}

		log.LogLockTimeoutRetry(query, attempt, sleep, time.Since(start), budget)

		if err := sleepCtx(ctx, sleep); err != nil {
			log.LogLockTimeoutInterrupted(attempt, time.Since(start))
			return nil, err
		}
	}
}

// WithRetryableTransaction runs `f` in a transaction, retrying on lock_timeout
// errors up to the configured retry budget.
func (db *RDB) WithRetryableTransaction(ctx context.Context, f func(context.Context, *sql.Tx) error) error {
	b := backoff.New(maxBackoffDuration, backoffInterval)
	budget := db.retryBudget()
	start := time.Now()
	deadline := start.Add(budget)
	attempt := 0
	log := db.logger()

	for {
		tx, err := db.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}

		err = f(ctx, tx)
		if err == nil {
			return tx.Commit()
		}

		if errRollback := tx.Rollback(); errRollback != nil {
			return errRollback
		}

		if !isLockNotAvailable(err) {
			return err
		}

		attempt++
		if budget < 0 {
			log.LogLockTimeoutGiveUp(attempt, time.Since(start))
			return err
		}

		sleep := b.Duration()
		if remaining := time.Until(deadline); remaining <= 0 {
			log.LogLockTimeoutGiveUp(attempt, time.Since(start))
			return err
		} else if sleep > remaining {
			sleep = remaining
		}

		log.LogLockTimeoutRetry("(transaction)", attempt, sleep, time.Since(start), budget)

		if err := sleepCtx(ctx, sleep); err != nil {
			log.LogLockTimeoutInterrupted(attempt, time.Since(start))
			return err
		}
	}
}

func (db *RDB) Close() error {
	return db.DB.Close()
}

func isLockNotAvailable(err error) bool {
	pqErr := &pq.Error{}
	return errors.As(err, &pqErr) && pqErr.Code == lockNotAvailableErrorCode
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// ScanFirstValue is a helper function to scan the first value with the assumption that Rows contains
// a single row with a single value.
func ScanFirstValue[T any](rows *sql.Rows, dest *T) error {
	if rows.Next() {
		if err := rows.Scan(dest); err != nil {
			return err
		}
	}
	return rows.Err()
}
