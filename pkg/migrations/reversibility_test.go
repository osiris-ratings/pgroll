// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateReversibility(t *testing.T) {
	t.Parallel()

	ptr := func(s string) *string { return &s }

	tests := []struct {
		name      string
		migration Migration
		wantErr   string
	}{
		{
			name: "raw SQL with down passes",
			migration: Migration{
				Operations: Operations{
					&OpRawSQL{Up: "CREATE TABLE t(a int)", Down: "DROP TABLE t"},
				},
			},
		},
		{
			name: "raw SQL without down fails",
			migration: Migration{
				Operations: Operations{
					&OpRawSQL{Up: "CREATE TABLE t(a int)"},
				},
			},
			wantErr: "down",
		},
		{
			name: "raw SQL without down marked irreversible passes",
			migration: Migration{
				Irreversible: true,
				Operations: Operations{
					&OpRawSQL{Up: "CREATE TABLE t(a int)"},
				},
			},
		},
		{
			name: "onComplete raw SQL without down passes",
			migration: Migration{
				Operations: Operations{
					&OpRawSQL{Up: "GRANT SELECT ON t TO app", OnComplete: true},
				},
			},
		},
		{
			name: "drop_column without down fails",
			migration: Migration{
				Operations: Operations{
					&OpDropColumn{Table: "users", Column: "email"},
				},
			},
			wantErr: "users.email",
		},
		{
			name: "drop_column with down passes",
			migration: Migration{
				Operations: Operations{
					&OpDropColumn{Table: "users", Column: "email", Down: "''"},
				},
			},
		},
		{
			name: "drop_column without down marked irreversible passes",
			migration: Migration{
				Irreversible: true,
				Operations: Operations{
					&OpDropColumn{Table: "users", Column: "email"},
				},
			},
		},
		{
			name: "structured ops without down requirements pass",
			migration: Migration{
				Operations: Operations{
					&OpAddColumn{Table: "users", Column: Column{Name: "age", Type: "int", Nullable: true}},
					&OpRenameColumn{Table: "users", From: "name", To: "full_name"},
					&OpDropTable{Name: "legacy"},
				},
			},
		},
		{
			name: "violation reported among multiple ops",
			migration: Migration{
				Operations: Operations{
					&OpAddColumn{Table: "users", Column: Column{Name: "age", Type: "int", Nullable: true}},
					&OpRawSQL{Up: "UPDATE users SET age = 0"},
				},
			},
			wantErr: "irreversible",
		},
		{
			name: "alter_column carries its own down requirement upstream",
			migration: Migration{
				Operations: Operations{
					&OpAlterColumn{Table: "users", Column: "age", Type: ptr("bigint"), Up: "age", Down: "age"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.migration.ValidateReversibility()
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}
