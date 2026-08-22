// SPDX-License-Identifier: Apache-2.0

package migrations_test

import (
	"context"
	"encoding/json"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/xataio/pgroll/pkg/migrations"
	"github.com/xataio/pgroll/pkg/schema"
)

func TestMigrationsIsolated(t *testing.T) {
	t.Parallel()

	migration := migrations.Migration{
		Name: "sql",
		Operations: migrations.Operations{
			&migrations.OpRawSQL{
				Up: `foo`,
			},
			&migrations.OpCreateTable{Name: "foo"},
		},
	}

	err := migration.Validate(context.TODO(), schema.New())
	var wantErr migrations.InvalidMigrationError
	assert.ErrorAs(t, err, &wantErr)
}

func TestMigrationsIsolatedValid(t *testing.T) {
	t.Parallel()

	migration := migrations.Migration{
		Name: "sql",
		Operations: migrations.Operations{
			&migrations.OpRawSQL{
				Up: `foo`,
			},
		},
	}
	err := migration.Validate(context.TODO(), schema.New())
	assert.NoError(t, err)
}

func TestOnCompleteSQLMigrationsAreNotIsolated(t *testing.T) {
	t.Parallel()

	migration := migrations.Migration{
		Name: "sql",
		Operations: migrations.Operations{
			&migrations.OpRawSQL{
				Up:         `foo`,
				OnComplete: true,
			},
			&migrations.OpCreateTable{Name: "foo"},
		},
	}
	err := migration.Validate(context.TODO(), schema.New())
	assert.NoError(t, err)
}

func TestCompleteMustBeDeferred(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		ops    migrations.Operations
		expect bool
	}{
		"add column is inline-safe": {
			ops:    migrations.Operations{&migrations.OpAddColumn{Table: "t", Column: migrations.Column{Name: "c", Type: "text", Nullable: true}}},
			expect: false,
		},
		"alter column needs deferral (duplicator pattern)": {
			ops:    migrations.Operations{&migrations.OpAlterColumn{Table: "t", Column: "c", Up: "c", Down: "c"}},
			expect: true,
		},
		"create table is inline-safe": {
			ops:    migrations.Operations{&migrations.OpCreateTable{Name: "t"}},
			expect: false,
		},
		"raw SQL without OnComplete is inline-safe": {
			ops:    migrations.Operations{&migrations.OpRawSQL{Up: "SELECT 1"}},
			expect: false,
		},
		"drop column needs deferral": {
			ops:    migrations.Operations{&migrations.OpDropColumn{Table: "t", Column: "c"}},
			expect: true,
		},
		"drop table needs deferral": {
			ops:    migrations.Operations{&migrations.OpDropTable{Name: "t"}},
			expect: true,
		},
		"rename column needs deferral": {
			ops:    migrations.Operations{&migrations.OpRenameColumn{Table: "t", From: "a", To: "b"}},
			expect: true,
		},
		"rename table needs deferral": {
			ops:    migrations.Operations{&migrations.OpRenameTable{From: "a", To: "b"}},
			expect: true,
		},
		"drop constraint needs deferral": {
			ops:    migrations.Operations{&migrations.OpDropConstraint{Name: "c", Table: "t", Up: "x", Down: "x"}},
			expect: true,
		},
		"drop index needs deferral": {
			ops:    migrations.Operations{&migrations.OpDropIndex{Name: "idx"}},
			expect: true,
		},
		"OnComplete raw SQL needs deferral": {
			ops:    migrations.Operations{&migrations.OpRawSQL{Up: "ALTER TABLE t DROP COLUMN c", OnComplete: true}},
			expect: true,
		},
		"mixed additive + drop column needs deferral": {
			ops: migrations.Operations{
				&migrations.OpAddColumn{Table: "t", Column: migrations.Column{Name: "c", Type: "text", Nullable: true}},
				&migrations.OpDropColumn{Table: "t", Column: "old"},
			},
			expect: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := &migrations.Migration{Name: "x", Operations: tc.ops}
			assert.Equal(t, tc.expect, m.CompleteMustBeDeferred())
		})
	}
}

func TestCompleteRequiresLiveSchemaDrop(t *testing.T) {
	t.Parallel()

	// Only onComplete raw SQL (opaque) forces the live-schema drop. Every typed
	// contraction leaves the live views valid (Postgres auto-follows the
	// underlying renames; the view never references dropped originals), so the
	// drop is unnecessary — proven per op by the seal tests.
	cases := map[string]struct {
		ops    migrations.Operations
		expect bool
	}{
		"add column": {
			ops:    migrations.Operations{&migrations.OpAddColumn{Table: "t", Column: migrations.Column{Name: "c", Type: "text", Nullable: true}}},
			expect: false,
		},
		"create table": {
			ops:    migrations.Operations{&migrations.OpCreateTable{Name: "t"}},
			expect: false,
		},
		"drop column": {
			ops:    migrations.Operations{&migrations.OpDropColumn{Table: "t", Column: "c"}},
			expect: false,
		},
		"rename column": {
			ops:    migrations.Operations{&migrations.OpRenameColumn{Table: "t", From: "a", To: "b"}},
			expect: false,
		},
		"rename table": {
			ops:    migrations.Operations{&migrations.OpRenameTable{From: "a", To: "b"}},
			expect: false,
		},
		"drop table": {
			ops:    migrations.Operations{&migrations.OpDropTable{Name: "t"}},
			expect: false,
		},
		"alter column": {
			ops:    migrations.Operations{&migrations.OpAlterColumn{Table: "t", Column: "c", Up: "c", Down: "c"}},
			expect: false,
		},
		"create constraint": {
			ops:    migrations.Operations{&migrations.OpCreateConstraint{}},
			expect: false,
		},
		"drop index": {
			ops:    migrations.Operations{&migrations.OpDropIndex{Name: "idx"}},
			expect: false,
		},
		"raw SQL without OnComplete": {
			ops:    migrations.Operations{&migrations.OpRawSQL{Up: "SELECT 1"}},
			expect: false,
		},
		"onComplete raw SQL forces the drop": {
			ops:    migrations.Operations{&migrations.OpRawSQL{Up: "ALTER TABLE t DROP COLUMN c", OnComplete: true}},
			expect: true,
		},
		"onComplete raw SQL mixed with typed ops still forces the drop": {
			ops: migrations.Operations{
				&migrations.OpAddColumn{Table: "t", Column: migrations.Column{Name: "c", Type: "text", Nullable: true}},
				&migrations.OpRawSQL{Up: "ALTER TABLE t DROP COLUMN old", OnComplete: true},
			},
			expect: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			m := &migrations.Migration{Name: "x", Operations: tc.ops}
			assert.Equal(t, tc.expect, m.CompleteRequiresLiveSchemaDrop())
		})
	}
}

func TestCollectFilesFromDir(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		dir           fstest.MapFS
		expectedFiles []string
	}{
		"find all json files": {
			dir: fstest.MapFS{
				"01_migration_1.json": &fstest.MapFile{},
				"03_migration_3.json": &fstest.MapFile{},
				"02_migration_2.json": &fstest.MapFile{},
			},
			expectedFiles: []string{"01_migration_1.json", "02_migration_2.json", "03_migration_3.json"},
		},
		"find all yaml and yml files": {
			dir: fstest.MapFS{
				"01_migration_1.yaml": &fstest.MapFile{},
				"03_migration_3.yaml": &fstest.MapFile{},
				"02_migration_2.yml":  &fstest.MapFile{},
			},
			expectedFiles: []string{"01_migration_1.yaml", "02_migration_2.yml", "03_migration_3.yaml"},
		},
		"find all files": {
			dir: fstest.MapFS{
				"01_migration_1.json": &fstest.MapFile{},
				"03_migration_3.yaml": &fstest.MapFile{},
				"02_migration_2.yml":  &fstest.MapFile{},
			},
			expectedFiles: []string{"01_migration_1.json", "02_migration_2.yml", "03_migration_3.yaml"},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			files, err := migrations.CollectFilesFromDir(test.dir)
			assert.NoError(t, err)
			assert.Equal(t, test.expectedFiles, files)
		})
	}
}

func TestMigrationNamesAreSetCorrectlyWhenReadingFromFile(t *testing.T) {
	t.Parallel()

	// Set up a directory with multiple migration files
	dir := fstest.MapFS{
		"01_migration_1.json": &fstest.MapFile{Data: exampleMigration(t)},
		"02_migration_2.yml":  &fstest.MapFile{Data: exampleMigration(t)},
		"03.migration.3.yml":  &fstest.MapFile{Data: exampleMigration(t)},
	}

	testcases := []struct {
		FileName              string
		ExpectedMigrationName string
	}{
		{
			FileName:              "01_migration_1.json",
			ExpectedMigrationName: "01_migration_1",
		},
		{
			FileName:              "02_migration_2.yml",
			ExpectedMigrationName: "02_migration_2",
		},
		{
			FileName:              "03.migration.3.yml",
			ExpectedMigrationName: "03.migration.3",
		},
	}

	for _, tc := range testcases {
		t.Run(tc.FileName, func(t *testing.T) {
			mig, err := migrations.ReadMigration(dir, tc.FileName)
			require.NoError(t, err)

			// Ensure that the migration name is set correctly from the filename
			assert.Equal(t, tc.ExpectedMigrationName, mig.Name)
		})
	}
}

func TestAllNonDeprecatedOperationsAreCreateable(t *testing.T) {
	for _, opName := range migrations.AllNonDeprecatedOperations {
		t.Run(opName, func(t *testing.T) {
			op, err := migrations.OperationFromName(migrations.OpName(opName))
			assert.NoError(t, err)
			_, ok := op.(migrations.Createable)
			assert.True(t, ok, "operation %q must have a Create function", opName)
		})
	}
}

func exampleMigration(t *testing.T) []byte {
	t.Helper()

	mig := &migrations.Migration{
		Operations: migrations.Operations{
			&migrations.OpRawSQL{Up: "SELECT 1"},
		},
	}

	bytes, err := json.Marshal(mig)
	require.NoError(t, err)

	return bytes
}

// TestContentHashIgnoresTargets pins the backward-compatibility invariant that
// makes re-application tombstones survive the introduction of `targets`.
//
// It holds today only because the field is omitempty and nil on both sides, so
// the existing assertions pass identically with the exclusion deleted. These
// two do not.
func TestContentHashIgnoresTargets(t *testing.T) {
	t.Parallel()

	base := migrations.Migration{
		Name:       "01_migration",
		Operations: migrations.Operations{&migrations.OpRawSQL{Up: "SELECT 1"}},
	}
	tagged := base
	tagged.Targets = []string{"app", "etl"}
	rerouted := base
	rerouted.Targets = []string{"etl"}

	baseHash, err := base.ContentHash()
	require.NoError(t, err)
	taggedHash, err := tagged.ContentHash()
	require.NoError(t, err)
	reroutedHash, err := rerouted.ContentHash()
	require.NoError(t, err)

	require.Equal(t, baseHash, taggedHash,
		"adding targets must not change the hash, or every tombstone recorded before "+
			"the field existed silently stops matching")
	require.Equal(t, taggedHash, reroutedHash,
		"re-routing a migration must not clear its tombstone: routing records which "+
			"databases it reaches, not what it does")

	// A real content change still moves the hash.
	edited := tagged
	edited.Operations = migrations.Operations{&migrations.OpRawSQL{Up: "SELECT 2"}}
	editedHash, err := edited.ContentHash()
	require.NoError(t, err)
	require.NotEqual(t, taggedHash, editedHash)
}

// TestTargetsRoundTrip covers the path `pgroll update` and `pgroll pull` take.
// Without Targets on both RawMigration and Migration, ParseMigration drops it
// and those commands rewrite every file with its routing stripped.
func TestTargetsRoundTrip(t *testing.T) {
	t.Parallel()

	for _, ext := range []string{"json", "yaml"} {
		t.Run(ext, func(t *testing.T) {
			body := `{"targets":["etl"],"operations":[{"sql":{"up":"SELECT 1"}}]}`
			fsys := fstest.MapFS{"01_m." + ext: &fstest.MapFile{Data: []byte(body)}}

			raw, err := migrations.ReadRawMigration(fsys, "01_m."+ext)
			require.NoError(t, err)
			require.Equal(t, []string{"etl"}, raw.Targets)

			parsed, err := migrations.ParseMigration(raw)
			require.NoError(t, err)
			require.Equal(t, []string{"etl"}, parsed.Targets,
				"ParseMigration must carry targets, or pgroll update strips them")
		})
	}
}

// TestBaselineMarkerRoundTrip covers the same path as TestTargetsRoundTrip:
// without Baseline on both RawMigration and Migration, ParseMigration drops
// the marker and `pgroll update` rewrites the baseline file as an ordinary
// migration — un-anchoring the whole directory.
func TestBaselineMarkerRoundTrip(t *testing.T) {
	t.Parallel()

	for _, ext := range []string{"json", "yaml"} {
		t.Run(ext, func(t *testing.T) {
			body := `{"baseline":true,"irreversible":true,"operations":[{"sql":{"up":"SELECT 1"}}]}`
			fsys := fstest.MapFS{"01_m." + ext: &fstest.MapFile{Data: []byte(body)}}

			raw, err := migrations.ReadRawMigration(fsys, "01_m."+ext)
			require.NoError(t, err)
			require.True(t, raw.Baseline)

			parsed, err := migrations.ParseMigration(raw)
			require.NoError(t, err)
			require.True(t, parsed.Baseline,
				"ParseMigration must carry the baseline marker, or pgroll update strips it")
		})
	}
}
