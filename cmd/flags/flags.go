// SPDX-License-Identifier: Apache-2.0

package flags

import (
	"time"

	"github.com/spf13/viper"
)

func PostgresURL() string {
	return viper.GetString("PG_URL")
}

func Schema() string {
	return viper.GetString("SCHEMA")
}

func StateSchema() string {
	return viper.GetString("STATE_SCHEMA")
}

func LockTimeout() int {
	return viper.GetInt("LOCK_TIMEOUT")
}

func LockRetryTimeout() time.Duration {
	return viper.GetDuration("LOCK_RETRY_TIMEOUT")
}

func SkipValidation() bool { return viper.GetBool("SKIP_VALIDATION") }

func Role() string {
	return viper.GetString("ROLE")
}

func Verbose() bool { return viper.GetBool("VERBOSE") }

// IndexProgressInterval is how often a concurrent index build reports
// progress. The flag's 0 means "off", which the Roll option spells as a
// negative duration (its zero means "default").
func IndexProgressInterval() time.Duration {
	d := viper.GetDuration("INDEX_PROGRESS_INTERVAL")
	if d <= 0 {
		return -1
	}
	return d
}

func UseVersionSchema() bool {
	return viper.GetBool("USE_VERSION_SCHEMA")
}

// Target is the deployment target migrations are filtered to. Empty means no
// filtering was requested — every migration applies, which is single-database
// mode.
func Target() string {
	return viper.GetString("TARGET")
}
