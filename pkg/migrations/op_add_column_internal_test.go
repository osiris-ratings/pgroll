// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAddColumnUpRepeatsDefault(t *testing.T) {
	t.Parallel()

	ptr := func(s string) *string { return &s }
	for name, tc := range map[string]struct {
		up       string
		fallback *string
		want     bool
	}{
		"no default":                   {up: "x + 1", want: false},
		"the default itself":           {up: "gen_random_uuid()", fallback: ptr("gen_random_uuid()"), want: true},
		"the default, spaced":          {up: " 'liens' ", fallback: ptr("'liens'"), want: true},
		"the default, cast and all":    {up: "'liens'::text", fallback: ptr("'liens'::text"), want: true},
		"the default with a cast":      {up: "'liens'::text", fallback: ptr("'liens'"), want: false},
		"a cast that changes it":       {up: "1.7::int", fallback: ptr("1.7"), want: false},
		"the default in parentheses":   {up: "('liens')", fallback: ptr("'liens'"), want: false},
		"parentheses that do not pair": {up: "COALESCE(n, 1) + (2)", fallback: ptr("(1) + (2)"), want: false},
		"coalesce of the column":       {up: "COALESCE(kind, 'liens')", fallback: ptr("'liens'"), want: false},
		"derived from the row":         {up: "tin IS NOT NULL", fallback: ptr("false"), want: false},
		"a different constant":         {up: "'ucc'", fallback: ptr("'liens'"), want: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			op := &OpAddColumn{Table: "t", Up: tc.up, Column: Column{Name: "kind", Type: "text", Default: tc.fallback}}
			assert.Equal(t, tc.want, op.upRepeatsDefault())
		})
	}
}
