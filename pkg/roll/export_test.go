// SPDX-License-Identifier: Apache-2.0

package roll

import "github.com/xataio/pgroll/pkg/db"

// SwapConnForTest replaces the connection pgroll runs its DDL through and
// returns the one it replaced, so tests can inject faults into every
// statement. It lives in an internal test file because the tests that use it
// are external: they need internal/testutils, which imports this package.
func SwapConnForTest(m *Roll, conn db.DB) db.DB {
	old := m.pgConn
	m.pgConn = conn
	return old
}
