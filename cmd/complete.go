// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"

	"github.com/pterm/pterm"
	"github.com/spf13/cobra"

	"github.com/xataio/pgroll/pkg/roll"
	"github.com/xataio/pgroll/pkg/state"
)

var completeCmd = &cobra.Command{
	Use:   "complete <file>",
	Short: "Complete an ongoing migration with the operations present in the given file",
	RunE: func(cmd *cobra.Command, args []string) error {
		// Create a roll instance and check if pgroll is initialized
		m, err := NewRollWithInitCheck(cmd.Context())
		if err != nil {
			return err
		}
		defer m.Close()

		sp, _ := pterm.DefaultSpinner.WithText("Completing migration...").Start()
		err = m.Complete(cmd.Context())
		if err != nil {
			// No active migration: contract whatever the deployment left
			// pending — drain the deferred queue (a resumed batch, or a
			// database upgraded with its delayed-contraction window still
			// open) and stamp the deployment sealed.
			if errors.Is(err, state.ErrNoActiveMigration) {
				drained, stamped, finishErr := m.FinishContraction(cmd.Context())
				if finishErr != nil {
					sp.Fail(fmt.Sprintf("Failed to contract the deployment: %s", finishErr))
					return finishErr
				}
				if drained > 0 || stamped > 0 {
					sp.Success(fmt.Sprintf("No active migration; drained %d deferred completion(s) and sealed %d migration(s).", drained, stamped))
				} else {
					sp.Success("Nothing to complete: no active migration and no pending contraction.")
				}
				return finishPendingRevert(cmd.Context(), m)
			}
			sp.Fail(fmt.Sprintf("Failed to complete migration: %s", err))
			return err
		}

		sp.Success("Migration successful!")
		return finishPendingRevert(cmd.Context(), m)
	},
}

// finishPendingRevert finishes an applied-but-unpruned inverse train, if the
// history leaf is one: it prunes the reverted forward migrations and their
// inverses from history and records re-application tombstones. This is the
// second half of a split (`revert --expand-only`) revert — the operator
// repins the fleet to the restored schema between the expand and this
// complete — and also the resume path for a revert interrupted between its
// final Complete and its prune. A no-op for ordinary migrations.
func finishPendingRevert(ctx context.Context, m *roll.Roll) error {
	plan, err := m.FinishPendingSealedRevert(ctx)
	if err != nil {
		return fmt.Errorf("failed to finish the pending revert: %w", err)
	}
	if plan == nil {
		return nil
	}
	pterm.Success.Printfln("Finished the pending revert: %d migration(s) unwound; history restored to %q.",
		len(plan.Targets), plan.Boundary)
	return nil
}
