// SPDX-License-Identifier: Apache-2.0

package roll_test

// Build-specific half of train_cases_test.go: how this build's `pgroll migrate`
// applies a train without --complete.

import (
	"context"

	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
	"github.com/xataio/pgroll/pkg/schema"
)

func r2MidOpts(mig *roll.Roll, final *migrations.Migration) []roll.StartOption {
	return append([]roll.StartOption{roll.WithoutVersionSchema()}, r2LeafOpts(mig, final)...)
}

func r2LeafOpts(mig *roll.Roll, final *migrations.Migration) []roll.StartOption {
	if !mig.UseVersionSchema() {
		return nil
	}
	return []roll.StartOption{roll.WithTrainTarget(final)}
}

func r2Defers(mig *roll.Roll, m *migrations.Migration) bool {
	return roll.DefersCompleteInTrain(m, mig.UseVersionSchema())
}

// r2MaterializeSchema is the schema `pgroll materialize --version <version>`
// projects when no migration is in progress (cmd/materialize.go).
func r2MaterializeSchema(ctx context.Context, mig *roll.Roll, version string) (*schema.Schema, error) {
	leaf, err := mig.LeafVersionSchema(ctx)
	if err != nil {
		return nil, err
	}
	if version != leaf {
		return mig.State().ReadSchema(ctx, mig.Schema())
	}
	return mig.SchemaWithDeferred(ctx)
}
