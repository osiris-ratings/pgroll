// SPDX-License-Identifier: Apache-2.0

package templates

const Function = `CREATE OR REPLACE FUNCTION {{ .Name | qi }}()
    RETURNS TRIGGER
    LANGUAGE PLPGSQL
    AS $$
    DECLARE
      {{- $schemaName := .SchemaName  }}
      {{- $tableName := .TableName  }}
      {{ range $name, $col := .Columns }} 
      {{- $name | qi }} {{ $schemaName | qi }}.{{ $tableName | qi}}.{{ $col.Name | qi }}%TYPE := NEW.{{ $col.Name | qi }};
      {{ end -}}
      latest_schema text;
      search_path text;
      {{- if .KeepsMigrationWrites }}
      _pgroll_fill boolean;
      {{- end }}
    BEGIN
      SELECT current_setting
        INTO search_path
        FROM current_setting('search_path');

      IF search_path {{- if eq .Direction "up" }} != {{- else }} = {{- end }} {{ if .TargetFunction }}{{ .SchemaName | qi }}.{{ .TargetFunction | qi }}(){{ else }}{{ .LatestSchema | ql }}{{ end }} THEN
      {{- $physicalColumn := .PhysicalColumn | qi  }}
      {{- if .KeepsMigrationWrites }}
        IF TG_OP = 'UPDATE' THEN
          _pgroll_fill := NEW.{{ $physicalColumn }} IS NOT DISTINCT FROM OLD.{{ $physicalColumn }};
        ELSE
          _pgroll_fill := {{ if .ColumnHasDefault }}true{{ else }}NEW.{{ $physicalColumn }} IS NULL{{ end }};
        END IF;
        IF current_setting('application_name') = {{ .MigrationSession | ql }} THEN
          IF TG_OP = 'UPDATE' THEN
            _pgroll_fill := _pgroll_fill AND OLD.{{ .NeedsBackfillColumn | qi }} IS NOT FALSE;
          ELSE
            _pgroll_fill := NEW.{{ $physicalColumn }} IS NULL;
          END IF;
        END IF;
        IF _pgroll_fill THEN
        {{- range $s := .SQL }}
          NEW.{{ $physicalColumn  }} = {{ $s }};
        {{- end }}
        END IF;
      {{- else if .OnlyFillsNull }}
        IF NEW.{{ $physicalColumn }} IS NULL THEN
        {{- range $s := .SQL }}
          NEW.{{ $physicalColumn  }} = {{ $s }};
        {{- end }}
        END IF;
      {{- else }}{{ range $s := .SQL }}
        NEW.{{ $physicalColumn  }} = {{ $s }};
      {{- end }}{{ end }}
        NEW.{{ .NeedsBackfillColumn | qi }} = false;
      {{- if .KeepsMigrationWrites }}
      ELSE
        NEW.{{ .NeedsBackfillColumn | qi }} = false;
      {{- end }}
      END IF;

      RETURN NEW;
    END; $$
`
