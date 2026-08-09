// SPDX-License-Identifier: Apache-2.0

package roll

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Test helpers — RawMigration uses json:"-" for Name (inferred from filename),
// so the JSON must not include a "name" field.

func validMigrationJSON(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"operations": []map[string]any{{"sql": map[string]any{"up": "SELECT 1", "down": "SELECT 1"}}},
	})
	require.NoError(t, err)
	return data
}

func migrationWithDependsOn(t *testing.T, deps []string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"operations": []map[string]any{{"sql": map[string]any{"up": "SELECT 1", "down": "SELECT 1"}}},
		"depends_on": deps,
	})
	require.NoError(t, err)
	return data
}

func migrationWithPreconditions(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"operations": []map[string]any{{"sql": map[string]any{"up": "SELECT 1", "down": "SELECT 1"}}},
		"preconditions": []map[string]any{
			{"table_exists": "users"},
		},
	})
	require.NoError(t, err)
	return data
}

func structuredMigrationJSON(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"operations": []map[string]any{
			{"add_column": map[string]any{
				"table":  "users",
				"column": map[string]any{"name": "email", "type": "text"},
			}},
		},
	})
	require.NoError(t, err)
	return data
}

func TestCheckMigrationsDir(t *testing.T) {
	t.Parallel()

	t.Run("valid files pass all checks", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_create_users.json": &fstest.MapFile{Data: validMigrationJSON(t)},
			"02_add_email.json":    &fstest.MapFile{Data: validMigrationJSON(t)},
			"03_create_posts.json": &fstest.MapFile{Data: validMigrationJSON(t)},
		}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)
		assert.Empty(t, result.Errors)
	})

	t.Run("empty directory passes", func(t *testing.T) {
		fs := fstest.MapFS{}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)
		assert.Empty(t, result.Errors)
		assert.Empty(t, result.Warnings)
	})

	t.Run("invalid YAML/JSON is an error", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_bad.json": &fstest.MapFile{Data: []byte(`{invalid json`)},
		}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)
		require.Len(t, result.Errors, 1)
		assert.Contains(t, result.Errors[0].Message, "invalid syntax")
		assert.Equal(t, "01_bad.json", result.Errors[0].File)
	})

	t.Run("missing operations is an error", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_empty.json": &fstest.MapFile{Data: []byte(`{}`)},
		}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)
		require.Len(t, result.Errors, 1)
		assert.Contains(t, result.Errors[0].Message, "operations")
	})

	t.Run("empty operations array is an error", func(t *testing.T) {
		data, err := json.Marshal(map[string]any{
			"operations": []any{},
		})
		require.NoError(t, err)

		fs := fstest.MapFS{
			"01_empty_ops.json": &fstest.MapFile{Data: data},
		}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)
		require.Len(t, result.Errors, 1)
		assert.Contains(t, result.Errors[0].Message, "operations")
	})

	t.Run("name too long is an error", func(t *testing.T) {
		longName := "20260101000000_this_is_a_very_long_migration_name_that_exceeds_the_limit"

		fs := fstest.MapFS{
			longName + ".json": &fstest.MapFile{Data: validMigrationJSON(t)},
		}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)

		schemaName := "public_" + longName
		if len(schemaName) > 63 {
			require.Len(t, result.Errors, 1)
			assert.Contains(t, result.Errors[0].Message, "too long")
		}
	})

	t.Run("depends_on missing target is an error", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_base.json":      &fstest.MapFile{Data: validMigrationJSON(t)},
			"02_dependent.json": &fstest.MapFile{Data: migrationWithDependsOn(t, []string{"nonexistent"})},
		}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)
		// Produces 2 errors: explicit depends_on check + TopoSortMigrations unknown dep
		require.GreaterOrEqual(t, len(result.Errors), 1)
		assert.Contains(t, result.Errors[0].Message, "nonexistent")
	})

	t.Run("depends_on valid target passes", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_base.json":      &fstest.MapFile{Data: validMigrationJSON(t)},
			"02_dependent.json": &fstest.MapFile{Data: migrationWithDependsOn(t, []string{"01_base"})},
		}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)
		assert.Empty(t, result.Errors)
	})

	t.Run("dependency cycle is an error", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_a.json": &fstest.MapFile{Data: migrationWithDependsOn(t, []string{"02_b"})},
			"02_b.json": &fstest.MapFile{Data: migrationWithDependsOn(t, []string{"01_a"})},
		}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)
		require.Len(t, result.Errors, 1)
		assert.Contains(t, result.Errors[0].Message, "dependency")
	})

	t.Run("raw SQL without preconditions is a warning", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_raw_sql.json": &fstest.MapFile{Data: validMigrationJSON(t)},
		}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)
		assert.Empty(t, result.Errors)
		require.Len(t, result.Warnings, 1)
		assert.Contains(t, result.Warnings[0].Message, "raw SQL")
		assert.Contains(t, result.Warnings[0].Message, "preconditions")
	})

	t.Run("raw SQL with preconditions has no warning", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_raw_sql.json": &fstest.MapFile{Data: migrationWithPreconditions(t)},
		}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)
		assert.Empty(t, result.Errors)
		assert.Empty(t, result.Warnings)
	})

	t.Run("structured operations without preconditions have no warning", func(t *testing.T) {
		fs := fstest.MapFS{
			"01_structured.json": &fstest.MapFile{Data: structuredMigrationJSON(t)},
		}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)
		assert.Empty(t, result.Errors)
		assert.Empty(t, result.Warnings)
	})

	t.Run("raw SQL without down is an error", func(t *testing.T) {
		data, err := json.Marshal(map[string]any{
			"operations": []map[string]any{{"sql": map[string]any{"up": "SELECT 1"}}},
		})
		require.NoError(t, err)

		fs := fstest.MapFS{
			"01_raw_sql.json": &fstest.MapFile{Data: data},
		}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)
		require.Len(t, result.Errors, 1)
		assert.Contains(t, result.Errors[0].Message, "down")
		assert.Contains(t, result.Errors[0].Message, "irreversible")
	})

	t.Run("raw SQL without down marked irreversible passes", func(t *testing.T) {
		data, err := json.Marshal(map[string]any{
			"operations":   []map[string]any{{"sql": map[string]any{"up": "SELECT 1"}}},
			"irreversible": true,
		})
		require.NoError(t, err)

		fs := fstest.MapFS{
			"01_raw_sql.json": &fstest.MapFile{Data: data},
		}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)
		assert.Empty(t, result.Errors)
	})

	t.Run("onComplete raw SQL without down passes", func(t *testing.T) {
		data, err := json.Marshal(map[string]any{
			"operations": []map[string]any{{"sql": map[string]any{"up": "SELECT 1", "onComplete": true}}},
		})
		require.NoError(t, err)

		fs := fstest.MapFS{
			"01_on_complete.json": &fstest.MapFile{Data: data},
		}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)
		assert.Empty(t, result.Errors)
	})

	t.Run("drop_column without down is an error", func(t *testing.T) {
		data, err := json.Marshal(map[string]any{
			"operations": []map[string]any{
				{"drop_column": map[string]any{"table": "users", "column": "email"}},
			},
		})
		require.NoError(t, err)

		fs := fstest.MapFS{
			"01_drop_email.json": &fstest.MapFile{Data: data},
		}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)
		require.Len(t, result.Errors, 1)
		assert.Contains(t, result.Errors[0].Message, "down")
		assert.Contains(t, result.Errors[0].Message, "users.email")
	})

	t.Run("drop_column with down passes", func(t *testing.T) {
		data, err := json.Marshal(map[string]any{
			"operations": []map[string]any{
				{"drop_column": map[string]any{"table": "users", "column": "email", "down": "''"}},
			},
		})
		require.NoError(t, err)

		fs := fstest.MapFS{
			"01_drop_email.json": &fstest.MapFile{Data: data},
		}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)
		assert.Empty(t, result.Errors)
	})

	t.Run("unparseable operations are an error", func(t *testing.T) {
		data, err := json.Marshal(map[string]any{
			"operations": []map[string]any{{"no_such_op": map[string]any{}}},
		})
		require.NoError(t, err)

		fs := fstest.MapFS{
			"01_unknown_op.json": &fstest.MapFile{Data: data},
		}

		result, err := CheckMigrationsDir(fs, false)
		require.NoError(t, err)
		require.Len(t, result.Errors, 1)
		assert.Contains(t, result.Errors[0].Message, "invalid operations")
	})
}

func TestCheckTargets(t *testing.T) {
	t.Parallel()

	mig := func(body string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(body)}
	}
	// irreversible so the fork's reversibility check does not fire; these
	// fixtures exist to exercise `targets`, not revertibility.
	ops := "irreversible: true\npreconditions: []\noperations:\n  - sql:\n      up: SELECT 1\n"

	t.Run("an empty targets list is an error", func(t *testing.T) {
		res, err := CheckMigrationsDir(fstest.MapFS{
			"01_a.yaml": mig("targets: []\n" + ops),
		}, false)
		require.NoError(t, err)
		require.True(t, res.HasErrors())
		require.Contains(t, res.Errors[0].Message, "empty 'targets' list")
	})

	t.Run("an empty entry is an error", func(t *testing.T) {
		res, err := CheckMigrationsDir(fstest.MapFS{
			"01_a.yaml": mig("targets:\n  - \"\"\n" + ops),
		}, false)
		require.NoError(t, err)
		require.True(t, res.HasErrors())
		require.Contains(t, res.Errors[0].Message, "empty entry")
	})

	t.Run("a duplicate entry is an error", func(t *testing.T) {
		res, err := CheckMigrationsDir(fstest.MapFS{
			"01_a.yaml": mig("targets:\n  - etl\n  - etl\n" + ops),
		}, false)
		require.NoError(t, err)
		require.True(t, res.HasErrors())
		require.Contains(t, res.Errors[0].Message, "more than once")
	})

	t.Run("targets are not required by default", func(t *testing.T) {
		res, err := CheckMigrationsDir(fstest.MapFS{"01_a.yaml": mig(ops)}, false)
		require.NoError(t, err)
		require.False(t, res.HasErrors())
	})

	t.Run("--require-targets makes an undeclared migration an error", func(t *testing.T) {
		res, err := CheckMigrationsDir(fstest.MapFS{"01_a.yaml": mig(ops)}, true)
		require.NoError(t, err)
		require.True(t, res.HasErrors())
		require.Contains(t, res.Errors[0].Message, "declares no `targets`")
	})

	// The one shape where treating an excluded dependency as satisfied can
	// actually break a database, so it must fail the check rather than warn:
	// cmd/check.go only exits non-zero on errors.
	t.Run("a depends_on that does not cover the dependent is an error", func(t *testing.T) {
		res, err := CheckMigrationsDir(fstest.MapFS{
			"01_a.yaml": mig("targets:\n  - app\n" + ops),
			"02_b.yaml": mig("targets:\n  - app\n  - etl\ndepends_on:\n  - 01_a\n" + ops),
		}, false)
		require.NoError(t, err)
		require.True(t, res.HasErrors())
		require.Contains(t, res.Errors[0].Message, "[etl]")
	})

	t.Run("a depends_on that covers the dependent is fine", func(t *testing.T) {
		res, err := CheckMigrationsDir(fstest.MapFS{
			"01_a.yaml": mig("targets:\n  - app\n  - etl\n" + ops),
			"02_b.yaml": mig("targets:\n  - etl\ndepends_on:\n  - 01_a\n" + ops),
		}, false)
		require.NoError(t, err)
		require.False(t, res.HasErrors())
	})

	t.Run("case-variant target names are flagged", func(t *testing.T) {
		res, err := CheckMigrationsDir(fstest.MapFS{
			"01_a.yaml": mig("targets:\n  - etl\n" + ops),
			"02_b.yaml": mig("targets:\n  - ETL\n" + ops),
		}, false)
		require.NoError(t, err)
		require.False(t, res.HasErrors(), "spelling is advisory, not fatal")
		var found bool
		for _, w := range res.Warnings {
			if strings.Contains(w.Message, "verbatim") {
				found = true
			}
		}
		require.True(t, found, "expected a case-variant warning, got %v", res.Warnings)
	})
}
