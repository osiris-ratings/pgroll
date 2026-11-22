// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/xataio/pgroll/pkg/schema"
)

func TestAddColumnStartedName(t *testing.T) {
	t.Parallel()

	const scope = "0123abcd"
	temporary := TemporaryName(scope, "a")
	volatile := "gen_random_uuid()"
	fills := &OpAddColumn{Table: "t", Up: "x + 1", Column: Column{Name: "a", Type: "integer"}}

	for name, tc := range map[string]struct {
		op      *OpAddColumn
		columns map[string]*schema.Column
		want    string
	}{
		"without up": {
			op:   &OpAddColumn{Table: "t", Column: Column{Name: "a", Type: "integer"}},
			want: temporary,
		},
		"up repeats the default": {
			op:   &OpAddColumn{Table: "t", Up: volatile, Column: Column{Name: "a", Type: "uuid", Default: &volatile}},
			want: temporary,
		},
		"up fills the column": {op: fills, want: "a"},
		"replayed over a schema read after Start": {
			op:      fills,
			columns: map[string]*schema.Column{"a": {Name: "a"}},
			want:    "a",
		},
		"the name belongs to a column dropped earlier in the train": {
			op:      fills,
			columns: map[string]*schema.Column{"a": {Name: "a", Deleted: true}},
			want:    temporary,
		},
		"the name belongs to a column renamed earlier in the train": {
			op:      fills,
			columns: map[string]*schema.Column{"b": {Name: "a"}},
			want:    temporary,
		},
		"started under the temporary name": {
			op:      fills,
			columns: map[string]*schema.Column{"a": {Name: "a", Deleted: true}, temporary: {Name: temporary}},
			want:    temporary,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			table := &schema.Table{Name: "t", Columns: tc.columns}
			assert.Equal(t, tc.want, tc.op.startedName(scope, table))
		})
	}
}
