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
      {{- if .KeepsUnpinnedWrites }}
      _pgroll_fill boolean;
      {{- end }}
    BEGIN
      SELECT current_setting
        INTO search_path
        FROM current_setting('search_path');

      IF search_path {{- if eq .Direction "up" }} != {{- else }} = {{- end }} {{ if .TargetFunction }}{{ .SchemaName | qi }}.{{ .TargetFunction | qi }}(){{ else }}{{ .LatestSchema | ql }}{{ end }} THEN
      {{- $physicalColumn := .PhysicalColumn | qi  }}
      {{- if .KeepsUnpinnedWrites }}
        IF search_path LIKE {{ .PinnedPattern | ql }} THEN
          _pgroll_fill := true;
        ELSIF TG_OP = 'INSERT' THEN
          _pgroll_fill := NEW.{{ $physicalColumn }} IS NULL;
        ELSE
          _pgroll_fill := OLD.{{ .NeedsBackfillColumn | qi }} IS NOT FALSE;
        END IF;
        IF _pgroll_fill THEN
        {{- range $s := .SQL }}
          NEW.{{ $physicalColumn  }} = {{ $s }};
        {{- end }}
        END IF;
      {{- else }}{{ range $s := .SQL }}
        NEW.{{ $physicalColumn  }} = {{ $s }};
      {{- end }}{{ end }}
        NEW.{{ .NeedsBackfillColumn | qi }} = false;
      END IF;

      RETURN NEW;
    END; $$
`
