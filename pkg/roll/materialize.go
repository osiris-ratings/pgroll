// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"context"
	"fmt"

	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/schema"
)

// Materialize re-creates the version schema <Roll.schema>_<version> and
// projects views over the supplied schema snapshot. It is a recovery tool:
// it does NOT execute any operation Start/Complete logic, does NOT touch
// pgroll's migrations table, and does NOT alter user tables. It writes only
// the version schema and its views.
//
// Use it when the live tables exist but the version schema apps connect to
// is missing — e.g. a batched migrate was interrupted before its final Start
// projected a version schema, or the version schema was dropped manually.
//
// Re-running with the same version is safe: ensureViews uses
// CREATE SCHEMA IF NOT EXISTS and CREATE OR REPLACE VIEW.
func (m *Roll) Materialize(ctx context.Context, version string, sc *schema.Schema) error {
	if version == "" {
		return fmt.Errorf("materialize: version name must not be empty")
	}
	if sc == nil {
		return fmt.Errorf("materialize: schema must not be nil")
	}

	m.logger.Info("materializing version schema", "version", version, "schema", m.schema)

	mig := &migrations.Migration{Name: version}
	if err := m.ensureViews(ctx, sc, mig); err != nil {
		return fmt.Errorf("materialize: %w", err)
	}
	return nil
}
