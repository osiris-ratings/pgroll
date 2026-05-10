// SPDX-License-Identifier: Apache-2.0

package roll_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xataio/pgroll/internal/testutils"
	"github.com/xataio/pgroll/pkg/backfill"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/roll"
)

func TestFunctionExistsPrecondition(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// Create a test function
		_, err := db.ExecContext(ctx, `
			CREATE FUNCTION test_normalize(input text) RETURNS text
			LANGUAGE sql IMMUTABLE AS $$
				SELECT lower(trim(input));
			$$;
		`)
		require.NoError(t, err)

		t.Run("function exists passes", func(t *testing.T) {
			migration := &migrations.Migration{
				Name: "01_test",
				Preconditions: []migrations.Precondition{
					{FunctionExists: &migrations.PreconditionFunctionRef{
						Name: "test_normalize",
					}},
				},
				Operations: migrations.Operations{
					&migrations.OpRawSQL{Up: "SELECT 1"},
				},
			}
			err := mig.Validate(ctx, migration)
			require.NoError(t, err)
		})

		t.Run("function exists with matching signature passes", func(t *testing.T) {
			sig := "input text -> text"
			migration := &migrations.Migration{
				Name: "01_test",
				Preconditions: []migrations.Precondition{
					{FunctionExists: &migrations.PreconditionFunctionRef{
						Name:      "test_normalize",
						Signature: &sig,
					}},
				},
				Operations: migrations.Operations{
					&migrations.OpRawSQL{Up: "SELECT 1"},
				},
			}
			err := mig.Validate(ctx, migration)
			require.NoError(t, err)
		})

		t.Run("function exists with wrong signature fails", func(t *testing.T) {
			sig := "integer -> text"
			migration := &migrations.Migration{
				Name: "01_test",
				Preconditions: []migrations.Precondition{
					{FunctionExists: &migrations.PreconditionFunctionRef{
						Name:      "test_normalize",
						Signature: &sig,
					}},
				},
				Operations: migrations.Operations{
					&migrations.OpRawSQL{Up: "SELECT 1"},
				},
			}
			err := mig.Validate(ctx, migration)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "no overload matches signature")
		})

		t.Run("function exists with matching body hash passes", func(t *testing.T) {
			body := "\n\t\t\t\tSELECT lower(trim(input));\n\t\t\t"
			hash := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(body)))
			migration := &migrations.Migration{
				Name: "01_test",
				Preconditions: []migrations.Precondition{
					{FunctionExists: &migrations.PreconditionFunctionRef{
						Name:     "test_normalize",
						BodyHash: &hash,
					}},
				},
				Operations: migrations.Operations{
					&migrations.OpRawSQL{Up: "SELECT 1"},
				},
			}
			err := mig.Validate(ctx, migration)
			require.NoError(t, err)
		})

		t.Run("function exists with wrong body hash fails", func(t *testing.T) {
			hash := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
			migration := &migrations.Migration{
				Name: "01_test",
				Preconditions: []migrations.Precondition{
					{FunctionExists: &migrations.PreconditionFunctionRef{
						Name:     "test_normalize",
						BodyHash: &hash,
					}},
				},
				Operations: migrations.Operations{
					&migrations.OpRawSQL{Up: "SELECT 1"},
				},
			}
			err := mig.Validate(ctx, migration)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "body hash mismatch")
		})

		t.Run("missing function fails", func(t *testing.T) {
			migration := &migrations.Migration{
				Name: "01_test",
				Preconditions: []migrations.Precondition{
					{FunctionExists: &migrations.PreconditionFunctionRef{
						Name: "nonexistent_function",
					}},
				},
				Operations: migrations.Operations{
					&migrations.OpRawSQL{Up: "SELECT 1"},
				},
			}
			err := mig.Validate(ctx, migration)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "does not exist")
		})
	})
}

func TestTypeExistsPrecondition(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// Create a test enum type
		_, err := db.ExecContext(ctx, `
			CREATE TYPE test_status AS ENUM ('active', 'inactive', 'pending');
		`)
		require.NoError(t, err)

		t.Run("type exists passes", func(t *testing.T) {
			migration := &migrations.Migration{
				Name: "01_test",
				Preconditions: []migrations.Precondition{
					{TypeExists: &migrations.PreconditionTypeRef{
						Name: "test_status",
					}},
				},
				Operations: migrations.Operations{
					&migrations.OpRawSQL{Up: "SELECT 1"},
				},
			}
			err := mig.Validate(ctx, migration)
			require.NoError(t, err)
		})

		t.Run("type exists with matching values hash passes", func(t *testing.T) {
			// Hash of sorted values: "active,inactive,pending"
			hash := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("active,inactive,pending")))
			migration := &migrations.Migration{
				Name: "01_test",
				Preconditions: []migrations.Precondition{
					{TypeExists: &migrations.PreconditionTypeRef{
						Name:       "test_status",
						ValuesHash: &hash,
					}},
				},
				Operations: migrations.Operations{
					&migrations.OpRawSQL{Up: "SELECT 1"},
				},
			}
			err := mig.Validate(ctx, migration)
			require.NoError(t, err)
		})

		t.Run("type exists with wrong values hash fails", func(t *testing.T) {
			hash := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
			migration := &migrations.Migration{
				Name: "01_test",
				Preconditions: []migrations.Precondition{
					{TypeExists: &migrations.PreconditionTypeRef{
						Name:       "test_status",
						ValuesHash: &hash,
					}},
				},
				Operations: migrations.Operations{
					&migrations.OpRawSQL{Up: "SELECT 1"},
				},
			}
			err := mig.Validate(ctx, migration)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "values hash mismatch")
		})

		t.Run("missing type fails", func(t *testing.T) {
			migration := &migrations.Migration{
				Name: "01_test",
				Preconditions: []migrations.Precondition{
					{TypeExists: &migrations.PreconditionTypeRef{
						Name: "nonexistent_type",
					}},
				},
				Operations: migrations.Operations{
					&migrations.OpRawSQL{Up: "SELECT 1"},
				},
			}
			err := mig.Validate(ctx, migration)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "does not exist")
		})
	})
}

func TestDBPreconditionsWithMigrate(t *testing.T) {
	t.Parallel()

	testutils.WithMigratorAndConnectionToContainer(t, func(mig *roll.Roll, db *sql.DB) {
		ctx := context.Background()

		// Apply a migration that creates a function
		createFn := &migrations.Migration{
			Name: "01_create_fn",
			Operations: migrations.Operations{
				&migrations.OpRawSQL{Up: `
					CREATE FUNCTION normalize_name(input text) RETURNS text
					LANGUAGE sql IMMUTABLE AS $$
						SELECT lower(trim(input));
					$$;
				`},
			},
		}
		err := mig.Start(ctx, createFn, backfill.NewConfig())
		require.NoError(t, err)
		err = mig.Complete(ctx)
		require.NoError(t, err)

		// Apply a migration that depends on the function with a precondition
		useFn := &migrations.Migration{
			Name:      "02_use_fn",
			DependsOn: []string{"01_create_fn"},
			Preconditions: []migrations.Precondition{
				{FunctionExists: &migrations.PreconditionFunctionRef{
					Name: "normalize_name",
				}},
			},
			Operations: migrations.Operations{
				&migrations.OpRawSQL{Up: "SELECT normalize_name('  Test  ')"},
			},
		}
		err = mig.Start(ctx, useFn, backfill.NewConfig())
		require.NoError(t, err)
		err = mig.Complete(ctx)
		require.NoError(t, err)
	})
}
