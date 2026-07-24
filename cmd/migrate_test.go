// SPDX-License-Identifier: Apache-2.0

package cmd

import "testing"

func TestClassifyCycle(t *testing.T) {
	tests := []struct {
		name           string
		activePeriod   bool
		otherBackends  int
		remaining      int
		applied        int
		stateInSync    bool
		expandComplete bool
		want           cycleState
	}{
		{
			name:         "active period with no other backend is interrupted",
			activePeriod: true,
			remaining:    1,
			applied:      0,
			stateInSync:  true,
			want:         cycleInterrupted,
		},
		{
			name:           "active period with completed expand and nothing remaining is awaiting complete",
			activePeriod:   true,
			remaining:      0,
			applied:        5,
			stateInSync:    true,
			expandComplete: true,
			want:           cycleAwaitingComplete,
		},
		{
			name:           "active period with completed expand but migrations remaining is interrupted",
			activePeriod:   true,
			remaining:      1,
			applied:        5,
			stateInSync:    true,
			expandComplete: true,
			want:           cycleInterrupted,
		},
		{
			name:         "active period with no projected schema and nothing remaining is interrupted",
			activePeriod: true,
			remaining:    0,
			applied:      5,
			stateInSync:  true,
			want:         cycleInterrupted,
		},
		{
			name:           "another live backend beats awaiting complete",
			activePeriod:   true,
			otherBackends:  1,
			remaining:      0,
			applied:        5,
			stateInSync:    true,
			expandComplete: true,
			want:           cycleInProgress,
		},
		{
			name:          "active period with another live pgroll backend is in-progress",
			activePeriod:  true,
			otherBackends: 1,
			remaining:     1,
			applied:       0,
			stateInSync:   true,
			want:          cycleInProgress,
		},
		{
			name:          "in-progress beats interrupted regardless of state-in-sync",
			activePeriod:  true,
			otherBackends: 2,
			remaining:     0,
			applied:       3,
			stateInSync:   false,
			want:          cycleInProgress,
		},
		{
			name:        "no remaining migrations is no-op",
			remaining:   0,
			applied:     5,
			stateInSync: true,
			want:        cycleNoOp,
		},
		{
			name:        "first run is fresh",
			remaining:   3,
			applied:     0,
			stateInSync: true,
			want:        cycleFresh,
		},
		{
			name:        "applied history with state in sync is incremental",
			remaining:   1,
			applied:     2,
			stateInSync: true,
			want:        cycleIncremental,
		},
		{
			name:        "state ahead of live schema is recovery",
			remaining:   1,
			applied:     2,
			stateInSync: false,
			want:        cycleRecovery,
		},
		{
			name:          "other backends only matter when activePeriod is true",
			otherBackends: 5,
			remaining:     1,
			applied:       0,
			stateInSync:   true,
			want:          cycleFresh,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyCycle(tt.activePeriod, tt.otherBackends, tt.remaining, tt.applied, tt.stateInSync, tt.expandComplete)
			if got != tt.want {
				t.Errorf("classifyCycle(active=%v, others=%d, remaining=%d, applied=%d, inSync=%v, expandComplete=%v) = %q, want %q",
					tt.activePeriod, tt.otherBackends, tt.remaining, tt.applied, tt.stateInSync, tt.expandComplete, got, tt.want)
			}
		})
	}
}
