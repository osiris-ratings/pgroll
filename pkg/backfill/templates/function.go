// SPDX-License-Identifier: Apache-2.0

package templates

const Function = `CREATE OR REPLACE FUNCTION {{ .Name | qi }}()
    RETURNS TRIGGER
    LANGUAGE PLPGSQL
    AS $$
    DECLARE
      {{- $schemaName := .SchemaName  }}
      {{- $tableName := .TableName  }}
      {{ range $name, $col := .Columns }}{{ if not (internal $name) }} 
      {{- $name | qi }} {{ $schemaName | qi }}.{{ $tableName | qi}}.{{ $col.Name | qi }}%TYPE := NEW.{{ $col.Name | qi }};
      {{ end }}{{ end -}}
      latest_schema text;
      search_path text;
    BEGIN
      SELECT current_setting
        INTO search_path
        FROM current_setting('search_path');

      IF search_path {{- if eq .Direction "up" }} != {{- else }} = {{- end }} {{ if .TargetFunction }}{{ .SchemaName | qi }}.{{ .TargetFunction | qi }}(){{ else }}{{ .LatestSchema | ql }}{{ end }} THEN
      {{- $physicalColumn := .PhysicalColumn | qi  }}{{ range $s := .SQL }}
        NEW.{{ $physicalColumn  }} = {{ $s }};
      {{- end }}
      {{- if .NeedsBackfillColumn }}
        NEW.{{ .NeedsBackfillColumn | qi }} = false;
      {{- end }}
      END IF;

      RETURN NEW;
    END; $$
`
