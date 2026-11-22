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
// SchemaWithDeferred returns the schema as the migrations applied so far
// leave it, with what the ones still waiting for a contraction changed.
func (m *Roll) SchemaWithDeferred(ctx context.Context) (*schema.Schema, error) {
	return m.readSchemaWithDeferred(ctx)
}

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

	// A fleet can now pin the leaf of a train that stopped part-way, so the
	// train's triggers follow it.
	latest, err := m.state.LatestMigration(ctx, m.schema)
	if err != nil {
		return fmt.Errorf("materialize: %w", err)
	}
	if latest == nil {
		return nil
	}
	leaf, err := m.state.GetMigration(ctx, m.schema, *latest)
	if err != nil {
		return fmt.Errorf("materialize: %w", err)
	}
	if leaf.VersionSchemaName() != version {
		return nil
	}
	if _, err := m.retargetTrain(ctx, VersionedSchemaName(m.schema, version)); err != nil {
		return fmt.Errorf("materialize: %w", err)
	}
	return nil
}
