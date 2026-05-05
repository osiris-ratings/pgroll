// SPDX-License-Identifier: Apache-2.0

package backfill

import (
	"time"
)

type Config struct {
	batchSize       int
	batchDelay      time.Duration
	callbacks       []CallbackFn
	logger          Logger
	progressEvery   int
	progressMinTime time.Duration
}

const (
	DefaultBatchSize int           = 1000
	DefaultDelay     time.Duration = 0

	// defaultProgressEvery is how many batches between progress log lines.
	defaultProgressEvery = 50
	// defaultProgressMinTime is the minimum time between progress log lines.
	// Used to keep small/fast tables quiet while still emitting progress for
	// long-running backfills.
	defaultProgressMinTime = 30 * time.Second
)

// Logger is the minimal interface backfill needs to report progress. Any
// logger that implements LogBackfillProgress satisfies it (including
// migrations.Logger). Defining it here avoids a pkg/backfill -> pkg/migrations
// dependency cycle.
type Logger interface {
	LogBackfillProgress(table string, rowsProcessed, total int64, elapsed time.Duration)
}

type noopLogger struct{}

func (noopLogger) LogBackfillProgress(string, int64, int64, time.Duration) {}

type OptionFn func(*Config)

func NewConfig(opts ...OptionFn) *Config {
	c := &Config{
		batchSize:       DefaultBatchSize,
		batchDelay:      DefaultDelay,
		callbacks:       make([]CallbackFn, 0),
		logger:          noopLogger{},
		progressEvery:   defaultProgressEvery,
		progressMinTime: defaultProgressMinTime,
	}

	for _, opt := range opts {
		opt(c)
	}
	return c
}

// WithLogger sets the logger used to emit periodic backfill progress events.
func WithLogger(l Logger) OptionFn {
	return func(o *Config) {
		if l != nil {
			o.logger = l
		}
	}
}

// WithBatchSize sets the batch size for the backfill operation.
func WithBatchSize(batchSize int) OptionFn {
	return func(o *Config) {
		o.batchSize = batchSize
	}
}

// WithBatchDelay sets the delay between batches for the backfill operation.
func WithBatchDelay(delay time.Duration) OptionFn {
	return func(o *Config) {
		o.batchDelay = delay
	}
}

// AddCallback adds a callback to the backfill operation.
// Callbacks are invoked after each batch is processed.
func (c *Config) AddCallback(fn CallbackFn) {
	c.callbacks = append(c.callbacks, fn)
}
