// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"errors"
	"testing"
)

// TestClassifyBlockedReason locks the Blocked.Reason contract: every planner
// refusal maps to one of the fixed tokens, and an unrecognized error falls
// through to the stable "unavailable" catch-all rather than leaking raw text.
func TestClassifyBlockedReason(t *testing.T) {
	tests := []struct {
		name string
		err  string
		want string
	}{
		{"not found", `migration "x" not found in history`, "target not found"},
		{"irreversible", `migration "x" is marked irreversible and cannot be inverted`, "inverse unavailable"},
		{"inferred DDL", `migration "x" was captured from DDL run outside pgroll and cannot be reverted`, "inverse unavailable"},
		{"no operations", `migration "x" has no operations (inferred or stamped DDL?) and cannot be inverted`, "inverse unavailable"},
		{"polluted boundary", `migration "x" is not a clean train boundary (its snapshot contains in-flight pgroll artifacts)`, "inverse unavailable"},
		{"non-contiguous window", `the revert window is not contiguous: unsealed migration(s) [x] are not ancestors of the leaf "y"`, "non-contiguous"},
		{"advanced past window", `history has advanced past the revert window: the leaf "y" is sealed`, "non-contiguous"},
		{"window open", `the revert window is open (2 unsealed migration(s)); revert it first with a plain revert`, "window open"},
		{"active migration", `a migration is in progress; complete or roll it back before reverting sealed history`, "window open"},
		{"unsealed destructive", `migration "x" has completed destructive operations but is not sealed`, "window open"},
		{"unrecognized", `some brand new error nobody classified yet`, "unavailable"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyBlockedReason(errors.New(tt.err))
			if got != tt.want {
				t.Errorf("classifyBlockedReason(%q) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
