// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/xataio/pgroll/pkg/migrations"
)

func TestRefuseEarlyCompleteOfAFillFollowsTheTablesSinceTheLastContraction(t *testing.T) {
	t.Parallel()

	mig := func(name string, ops ...migrations.Operation) *migrations.Migration {
		return &migrations.Migration{Name: name, Operations: ops}
	}
	create := func(table string) *migrations.OpCreateTable {
		return &migrations.OpCreateTable{Name: table, Columns: []migrations.Column{{Name: "id", Type: "integer", Pk: true}}}
	}
	fill := func(table string) *migrations.Migration {
		return mig("50_fill", &migrations.OpAddColumn{
			Table: table, Up: "id * 2",
			Column: migrations.Column{Name: "twice", Type: "integer", Nullable: true},
		})
	}
	later := mig("90_later", &migrations.OpRawSQL{Up: "SELECT 1"})

	for name, tc := range map[string]struct {
		uncontracted, run []*migrations.Migration
		refused           bool
	}{
		"a table the previous revision writes": {
			run: []*migrations.Migration{fill("orders"), later}, refused: true,
		},
		"a table the run creates": {
			run: []*migrations.Migration{mig("01_create", create("fresh")), fill("fresh"), later},
		},
		"a table an uncontracted migration created": {
			uncontracted: []*migrations.Migration{mig("01_create", create("fresh"))},
			run:          []*migrations.Migration{fill("fresh"), later},
		},
		"a created table renamed in the run": {
			run: []*migrations.Migration{
				mig("01_create", create("fresh")),
				mig("02_rename", &migrations.OpRenameTable{From: "fresh", To: "fresher"}),
				fill("fresher"), later,
			},
		},
		"the old name of a created table that was renamed": {
			run: []*migrations.Migration{
				mig("01_create", create("fresh")),
				mig("02_rename", &migrations.OpRenameTable{From: "fresh", To: "fresher"}),
				fill("fresh"), later,
			},
			refused: true,
		},
		"an existing table renamed to a created and dropped name": {
			run: []*migrations.Migration{
				mig("01_create", create("staging")),
				mig("02_drop", &migrations.OpDropTable{Name: "staging"}),
				mig("03_rename", &migrations.OpRenameTable{From: "orders", To: "staging"}),
				fill("staging"), later,
			},
			refused: true,
		},
		"an existing table renamed over a created name": {
			uncontracted: []*migrations.Migration{mig("01_create", create("staging"))},
			run: []*migrations.Migration{
				mig("03_rename", &migrations.OpRenameTable{From: "orders", To: "staging"}),
				fill("staging"), later,
			},
			refused: true,
		},
		"the fill is the last migration": {
			run: []*migrations.Migration{later, fill("orders")},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := refuseEarlyCompleteOfAFill(tc.uncontracted, tc.run)
			if tc.refused {
				assert.ErrorContains(t, err, "50_fill")
				return
			}
			assert.NoError(t, err)
		})
	}
}
