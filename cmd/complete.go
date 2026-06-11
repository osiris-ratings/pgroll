// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"fmt"

	"github.com/pterm/pterm"
	"github.com/spf13/cobra"

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
			// No active migration: under delayed contraction, completed
			// deployments leave their destructive DDL queued so they stay
			// revertible. Treat a bare `pgroll complete` as the manual
			// contraction trigger — drain the queue and close the revert
			// window on demand.
			if errors.Is(err, state.ErrNoActiveMigration) {
				drained, sealErr := m.SealDeferredCompletes(cmd.Context())
				if sealErr != nil {
					sp.Fail(fmt.Sprintf("Failed to drain deferred completions: %s", sealErr))
					return sealErr
				}
				// A manual seal closes the window completely: also stamp
				// unsealed rows with nothing queued (inline-only windows,
				// e.g. a train re-opened by a bounded revert).
				stamped, sealErr := m.SealWindow(cmd.Context())
				if sealErr != nil {
					sp.Fail(fmt.Sprintf("Failed to seal the revert window: %s", sealErr))
					return sealErr
				}
				if drained > 0 || stamped > 0 {
					sp.Success(fmt.Sprintf("No active migration; drained %d deferred completion(s) and sealed %d migration(s). The revert window is now closed.", drained, stamped))
					return nil
				}
			}
			sp.Fail(fmt.Sprintf("Failed to complete migration: %s", err))
			return err
		}

		sp.Success("Migration successful!")
		return nil
	},
}
