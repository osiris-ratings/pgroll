// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"fmt"

	"github.com/xataio/pgroll/pkg/schema"
)

// Materialize creates the version schema <Roll.schema>_<version>, if needed,
// and in it a view over each table of the supplied schema snapshot that has
// none. It is a recovery tool: it does NOT execute any operation
// Start/Complete logic, does NOT touch pgroll's migrations table, and does NOT
// alter user tables. It never drops or rebuilds a view that exists, so
// re-running it is safe.
//
// Use it when the live tables exist but the version schema apps connect to,
// or a view in it, is missing — e.g. a batched migrate was interrupted before
// its final Start projected a version schema, or the version schema was
// dropped manually.
func (m *Roll) Materialize(ctx context.Context, version string, sc *schema.Schema) error {
	if version == "" {
		return fmt.Errorf("materialize: version name must not be empty")
	}
	if sc == nil {
		return fmt.Errorf("materialize: schema must not be nil")
	}

	m.logger.Info("materializing version schema", "version", version, "schema", m.schema)

	if _, err := m.createMissingViews(ctx, version, sc); err != nil {
		return fmt.Errorf("materialize: %w", err)
	}
	return nil
}
