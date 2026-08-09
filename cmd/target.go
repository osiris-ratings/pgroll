// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/xataio/pgroll/cmd/flags"
)

// refuseTarget rejects --target on a command that cannot act on it.
//
// --target is a persistent flag because most directory-reading commands do
// honour it, but a routing flag that is accepted and quietly ignored is the
// exact failure this design exists to prevent: the operator believes they
// scoped the command and they did not. Commands here either have no migration
// set to filter (they act on database state) or take a single explicit
// migration whose routing is checked where it is applied.
func refuseTarget(cmd *cobra.Command) error {
	if flags.Target() == "" {
		return nil
	}
	return fmt.Errorf("--target is not supported by `pgroll %s`: it does not select "+
		"migrations from a set, so the flag would have no effect", cmd.Name())
}
