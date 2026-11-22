// SPDX-License-Identifier: Apache-2.0

package backfill

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/xataio/pgroll/pkg/schema"
)

func TestBuildFunction(t *testing.T) {
	testCases := []struct {
		name     string
		config   triggerConfig
		expected string
	}{
		{
			name: "simple up trigger",
			config: triggerConfig{
				Name:      "triggerName",
				Direction: TriggerDirectionUp,
				Columns: map[string]*schema.Column{
					"id":       {Name: "id", Type: "int"},
					"username": {Name: "username", Type: "text"},
					"product":  {Name: "product", Type: "text"},
					"review":   {Name: "review", Type: "text"},
				},
				SchemaName:          "public",
				LatestSchema:        "public_01_migration_name",
				TableName:           "reviews",
				PhysicalColumn:      "_pgroll_new_review",
				NeedsBackfillColumn: NeedsBackfillColumnName(""),
				SQL:                 []string{"product || 'is good'"},
			},
			expected: `CREATE OR REPLACE FUNCTION "triggerName"()
    RETURNS TRIGGER
    LANGUAGE PLPGSQL
    AS $$
    DECLARE
      "product" "public"."reviews"."product"%TYPE := NEW."product";
      latest_schema text;
      search_path text;
    BEGIN
      SELECT current_setting
        INTO search_path
        FROM current_setting('search_path');

      IF search_path != 'public_01_migration_name' THEN
        NEW."_pgroll_new_review" = product || 'is good';
        NEW."_pgroll_needs_backfill" = false;
      END IF;

      RETURN NEW;
    END; $$
`,
		},
		{
			name: "multiple up trigger",
			config: triggerConfig{
				Name:      "triggerName",
				Direction: TriggerDirectionUp,
				Columns: map[string]*schema.Column{
					"id":       {Name: "id", Type: "int"},
					"username": {Name: "username", Type: "text"},
					"product":  {Name: "product", Type: "text"},
					"review":   {Name: "review", Type: "text"},
				},
				SchemaName:          "public",
				LatestSchema:        "public_01_migration_name",
				TableName:           "reviews",
				PhysicalColumn:      "_pgroll_new_review",
				NeedsBackfillColumn: NeedsBackfillColumnName(""),
				SQL: []string{
					"product || 'is good'",
					"CASE WHEN NEW.\"_pgroll_new_review\" = 'bad' THEN 'bad review' ELSE 'good review' END",
				},
			},
			expected: `CREATE OR REPLACE FUNCTION "triggerName"()
    RETURNS TRIGGER
    LANGUAGE PLPGSQL
    AS $$
    DECLARE
      "product" "public"."reviews"."product"%TYPE := NEW."product";
      "review" "public"."reviews"."review"%TYPE := NEW."review";
      latest_schema text;
      search_path text;
    BEGIN
      SELECT current_setting
        INTO search_path
        FROM current_setting('search_path');

      IF search_path != 'public_01_migration_name' THEN
        NEW."_pgroll_new_review" = product || 'is good';
        NEW."_pgroll_new_review" = CASE WHEN NEW."_pgroll_new_review" = 'bad' THEN 'bad review' ELSE 'good review' END;
        NEW."_pgroll_needs_backfill" = false;
      END IF;

      RETURN NEW;
    END; $$
`,
		},
		{
			name: "simple down trigger",
			config: triggerConfig{
				Name:      "triggerName",
				Direction: TriggerDirectionDown,
				Columns: map[string]*schema.Column{
					"id":       {Name: "id", Type: "int"},
					"username": {Name: "username", Type: "text"},
					"product":  {Name: "product", Type: "text"},
					"review":   {Name: "review", Type: "text"},
				},
				SchemaName:          "public",
				LatestSchema:        "public_01_migration_name",
				TableName:           "reviews",
				PhysicalColumn:      "review",
				NeedsBackfillColumn: NeedsBackfillColumnName(""),
				SQL:                 []string{`NEW."_pgroll_new_review"`},
			},
			expected: `CREATE OR REPLACE FUNCTION "triggerName"()
    RETURNS TRIGGER
    LANGUAGE PLPGSQL
    AS $$
    DECLARE
      latest_schema text;
      search_path text;
    BEGIN
      SELECT current_setting
        INTO search_path
        FROM current_setting('search_path');

      IF search_path = 'public_01_migration_name' THEN
        NEW."review" = NEW."_pgroll_new_review";
        NEW."_pgroll_needs_backfill" = false;
      END IF;

      RETURN NEW;
    END; $$
`,
		},
		{
			name: "down trigger with aliased column",
			config: triggerConfig{
				Name:      "triggerName",
				Direction: TriggerDirectionDown,
				Columns: map[string]*schema.Column{
					"id":       {Name: "id", Type: "int"},
					"username": {Name: "username", Type: "text"},
					"product":  {Name: "product", Type: "text"},
					"review":   {Name: "review", Type: "text"},
					"rating":   {Name: "_pgroll_new_rating", Type: "integer"},
				},
				SchemaName:          "public",
				LatestSchema:        "public_01_migration_name",
				TableName:           "reviews",
				PhysicalColumn:      "rating",
				NeedsBackfillColumn: NeedsBackfillColumnName(""),
				SQL:                 []string{`CAST(rating as text)`},
			},
			expected: `CREATE OR REPLACE FUNCTION "triggerName"()
    RETURNS TRIGGER
    LANGUAGE PLPGSQL
    AS $$
    DECLARE
      "rating" "public"."reviews"."_pgroll_new_rating"%TYPE := NEW."_pgroll_new_rating";
      latest_schema text;
      search_path text;
    BEGIN
      SELECT current_setting
        INTO search_path
        FROM current_setting('search_path');

      IF search_path = 'public_01_migration_name' THEN
        NEW."rating" = CAST(rating as text);
        NEW."_pgroll_needs_backfill" = false;
      END IF;

      RETURN NEW;
    END; $$
`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sql, err := buildFunction(tc.config)
			assert.NoError(t, err)
			assert.Equal(t, tc.expected, sql)
		})
	}
}

func TestBuildTrigger(t *testing.T) {
	testCases := []struct {
		name     string
		config   triggerConfig
		expected string
	}{
		{
			name: "trigger",
			config: triggerConfig{
				Name:        "functionName",
				TriggerName: "triggerName",
				TableName:   "reviews",
			},
			expected: `CREATE OR REPLACE TRIGGER "triggerName"
    BEFORE UPDATE OR INSERT
    ON "reviews"
    FOR EACH ROW
    EXECUTE PROCEDURE "functionName"();
`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sql, err := buildTrigger(tc.config)
			assert.NoError(t, err)
			assert.Equal(t, tc.expected, sql)
		})
	}
}

func TestNamesIdentifier(t *testing.T) {
	for _, tc := range []struct {
		expression, name string
		want             bool
	}{
		{"tin IS NOT NULL", "tin", true},
		{"TIN IS NOT NULL", "tin", true},
		{`COALESCE("Status", 'x')`, "Status", true},
		{"(SELECT s.id FROM s WHERE s.id = session_id)", "session_id", true},
		{"(SELECT s.id FROM s WHERE s.id = session_id)", "session", false},
		{`NEW."_pgroll_new_review"`, "review", false},
		{"x + 1", "_pgroll_needs_backfill_abc", false},
		{"lower(x)", "", false},
	} {
		assert.Equal(t, tc.want, namesIdentifier(tc.expression, tc.name), "%q in %q", tc.name, tc.expression)
	}
}

func TestBuildFunctionKeepingMigrationWrites(t *testing.T) {
	sql, err := buildFunction(triggerConfig{
		Name:                 "triggerName",
		Direction:            TriggerDirectionUp,
		Columns:              map[string]*schema.Column{"x": {Name: "x"}, "a": {Name: "a"}},
		SchemaName:           "my_app",
		TargetFunction:       TrainTargetFunction,
		TableName:            "w",
		PhysicalColumn:       "a",
		NeedsBackfillColumn:  NeedsBackfillColumnName("abc"),
		KeepsMigrationWrites: true,
		SQL:                  []string{"(x + 1)"},
	})
	assert.NoError(t, err)
	assert.Equal(t, `CREATE OR REPLACE FUNCTION "triggerName"()
    RETURNS TRIGGER
    LANGUAGE PLPGSQL
    AS $$
    DECLARE
      "x" "my_app"."w"."x"%TYPE := NEW."x";
      latest_schema text;
      search_path text;
      _pgroll_fill boolean;
    BEGIN
      SELECT current_setting
        INTO search_path
        FROM current_setting('search_path');

      IF search_path != "my_app"."_pgroll_train_target"() THEN
        IF TG_OP = 'UPDATE' THEN
          _pgroll_fill := NEW."a" IS NOT DISTINCT FROM OLD."a";
        ELSE
          _pgroll_fill := true;
        END IF;
        IF current_setting('application_name') = 'pgroll' THEN
          IF TG_OP = 'UPDATE' THEN
            _pgroll_fill := _pgroll_fill AND OLD."_pgroll_needs_backfill_abc" IS NOT FALSE;
          ELSE
            _pgroll_fill := NEW."a" IS NULL;
          END IF;
        END IF;
        IF _pgroll_fill THEN
          NEW."a" = (x + 1);
        END IF;
        NEW."_pgroll_needs_backfill_abc" = false;
      ELSE
        NEW."_pgroll_needs_backfill_abc" = false;
      END IF;

      RETURN NEW;
    END; $$
`, sql)
}
