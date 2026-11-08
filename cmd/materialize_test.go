// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

// TestResolveMaterializeVersionUsesVersionSchema: with nothing active and no
// flags, materialize targets the latest migration's version schema, which a
// migration may name itself.
func TestResolveMaterializeVersionUsesVersionSchema(t *testing.T) {
	testutils.WithMigratorAndConnectionToContainer(t, func(m *roll.Roll, _ *sql.DB) {
		ctx := context.Background()
		require.NoError(t, m.Start(ctx, &migrations.Migration{
			Name:          "01_create_widgets",
			VersionSchema: "widgets_v1",
			Operations: migrations.Operations{&migrations.OpCreateTable{
				Name:    "widgets",
				Columns: []migrations.Column{{Name: "id", Type: "integer", Pk: true}},
			}},
		}, backfill.NewConfig()))
		require.NoError(t, m.Complete(ctx))

		version, err := resolveMaterializeVersion(ctx, m, nil, "")
		require.NoError(t, err)
		require.Equal(t, "widgets_v1", version)
	})
}
