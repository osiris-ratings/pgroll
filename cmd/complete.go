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
				sealed, sealErr := m.SealDeferredCompletes(cmd.Context())
				if sealErr != nil {
					sp.Fail(fmt.Sprintf("Failed to drain deferred completions: %s", sealErr))
					return sealErr
				}
				if sealed > 0 {
					sp.Success(fmt.Sprintf("No active migration; drained %d deferred completion(s) from the previous deployment. The revert window is now closed.", sealed))
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
