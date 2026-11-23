// SPDX-License-Identifier: Apache-2.0

package state

import "errors"

var ErrNoActiveMigration = errors.New("no active migration")

// ErrRebaselineRefused wraps every audit refusal from
// ConvertMigrationToBaseline, so callers can distinguish "this history is not
// shaped for an in-place baseline conversion" from transient database errors.
var ErrRebaselineRefused = errors.New("refusing to convert migration to baseline")

// ErrBaselineSortsBelowHistory is returned when a new baseline's name sorts
// before a migration the schema has already applied.
var ErrBaselineSortsBelowHistory = errors.New("baseline sorts before applied migrations")
