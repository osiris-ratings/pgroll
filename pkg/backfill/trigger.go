// SPDX-License-Identifier: Apache-2.0

package backfill

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"text/template"

	"github.com/lib/pq"

	"github.com/xataio/pgroll/pkg/backfill/templates"
	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

type TriggerDirection string

const (
	TriggerDirectionUp   TriggerDirection = "up"
	TriggerDirectionDown TriggerDirection = "down"
)

type triggerConfig struct {
	// Name is the trigger function's name; TriggerName the trigger's.
	Name           string
	TriggerName    string
	Direction      TriggerDirection
	Columns        map[string]*schema.Column
	SchemaName     string
	TableName      string
	PhysicalColumn string
	LatestSchema   string
	// TargetFunction, when set, names the function the trigger asks for
	// the latest schema instead of comparing against LatestSchema.
	TargetFunction      string
	SQL                 []string
	NeedsBackfillColumn string
	// KeepsUnpinnedWrites: see OperationTrigger.
	KeepsUnpinnedWrites bool
	// MigrationScope is the per-migration suffix used when naming the
	// marker column. Empty scope produces the legacy unscoped marker.
	MigrationScope string
}

type OperationTrigger struct {
	Name           string
	Direction      TriggerDirection
	Columns        map[string]*schema.Column
	TableName      string
	PhysicalColumn string
	SQL            string
	// KeepsUnpinnedWrites is for a column that sessions on the migrated
	// schema itself can already name: pgroll's own, running a later
	// migration, or anything else not pinned to a version schema. Their
	// write keeps the value it gave the column. The expression still fills
	// an insert that gave none and a row the backfill has not reached.
	KeepsUnpinnedWrites bool
}

type createTriggerAction struct {
	conn db.DB
	cfg  triggerConfig
}

func (a *createTriggerAction) execute(ctx context.Context) error {
	// Parenthesize the up/down SQL if it's not parenthesized already
	for i, sql := range a.cfg.SQL {
		if len(sql) > 0 && sql[0] != '(' {
			a.cfg.SQL[i] = "(" + sql + ")"
		}
	}

	a.cfg.NeedsBackfillColumn = NeedsBackfillColumnName(a.cfg.MigrationScope)

	funcSQL, err := buildFunction(a.cfg)
	if err != nil {
		return err
	}

	triggerSQL, err := buildTrigger(a.cfg)
	if err != nil {
		return err
	}

	return a.conn.WithRetryableTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := a.conn.ExecContext(ctx,
			fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s boolean DEFAULT true",
				pq.QuoteIdentifier(a.cfg.TableName),
				pq.QuoteIdentifier(a.cfg.NeedsBackfillColumn)))
		if err != nil {
			return err
		}

		_, err = a.conn.ExecContext(ctx, funcSQL)
		if err != nil {
			return err
		}

		_, err = a.conn.ExecContext(ctx, triggerSQL)
		return err
	})
}

func buildFunction(cfg triggerConfig) (string, error) {
	cfg.Columns = namedColumns(cfg.Columns, cfg.SQL)
	return executeTemplate("function", templates.Function, cfg)
}

// PinnedPattern is the LIKE pattern of a search_path pinned to one of the
// schema's version schemas.
func (c triggerConfig) PinnedPattern() string {
	return strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`).Replace(c.SchemaName) + `\_%`
}

// namedColumns returns the columns whose name appears in one of the
// expressions. The function declares a variable for each, read from the row,
// so one it never reads must not be declared: another migration may rename
// or drop that column while the function is live. The match is by text, so
// it can keep a column the expressions do not read, never drop one they do.
func namedColumns(columns map[string]*schema.Column, expressions []string) map[string]*schema.Column {
	named := make(map[string]*schema.Column, len(columns))
	for name, column := range columns {
		if slices.ContainsFunc(expressions, func(e string) bool { return namesIdentifier(e, name) }) {
			named[name] = column
		}
	}
	return named
}

// namesIdentifier reports whether expression contains name as a whole
// identifier, in any letter case.
func namesIdentifier(expression, name string) bool {
	if name == "" {
		return false
	}
	expression, name = strings.ToLower(expression), strings.ToLower(name)
	for from := 0; ; {
		i := strings.Index(expression[from:], name)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(name)
		if (start == 0 || !isIdentifierByte(expression[start-1])) &&
			(end == len(expression) || !isIdentifierByte(expression[end])) {
			return true
		}
		from = start + 1
	}
}

func isIdentifierByte(b byte) bool {
	return b == '_' || b == '$' || b >= 0x80 ||
		(b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func buildTrigger(cfg triggerConfig) (string, error) {
	return executeTemplate("trigger", templates.Trigger, cfg)
}

func executeTemplate(name, content string, cfg triggerConfig) (string, error) {
	tmpl := template.Must(template.
		New(name).
		Funcs(template.FuncMap{
			"ql": pq.QuoteLiteral,
			"qi": pq.QuoteIdentifier,
		}).
		Parse(content))

	buf := bytes.Buffer{}
	if err := tmpl.Execute(&buf, cfg); err != nil {
		return "", err
	}

	return buf.String(), nil
}

// TriggerFunctionName returns the per-migration name of the trigger
// function for a given table and column. The scope suffix prevents two
// concurrently-deferred migrations operating on the same (table, col)
// from clobbering each other's trigger functions, which is what would
// happen with the legacy unscoped name.
//
// Postgres cuts an identifier at 63 bytes, which would drop the scope and
// part of the column from a long name, so a name that long carries a digest
// of the table and column instead.
func TriggerFunctionName(scope, tableName, columnName string) string {
	const prefix, maxIdentifier = "_pgroll_trigger_", 63
	suffix := ""
	if scope != "" {
		suffix = "_" + scope
	}
	if name := prefix + tableName + "_" + columnName + suffix; len(name) <= maxIdentifier {
		return name
	}
	sum := sha256.Sum256([]byte(tableName + "\x00" + columnName))
	return prefix + hex.EncodeToString(sum[:12]) + suffix
}

// TriggerName returns the per-migration name of the trigger for a given
// table and column. Mirrors TriggerFunctionName so the trigger and its
// underlying function share an identifier shape.
func TriggerName(scope, tableName, columnName string) string {
	return TriggerFunctionName(scope, tableName, columnName)
}

// TrainTargetFunction is the function, in the migrated schema, that returns
// the version schema a train's new revision pins. Every trigger of the train
// reads it, so redefining it re-keys them all at once.
const TrainTargetFunction = "_pgroll_train_target"

// OrderedTriggerName returns the name of the trigger that runs the function
// functionName. Postgres fires a table's triggers in name order, so `up`
// triggers sort by ascending position and `down` triggers by descending:
// a write crosses a train's migrations in the order its revision needs.
// index orders the triggers of one migration among themselves.
func OrderedTriggerName(direction TriggerDirection, position, index int, functionName string) string {
	const last = 9999
	position = min(max(position, 0), last)
	key := fmt.Sprintf("a%04d", position)
	if direction == TriggerDirectionDown {
		key = fmt.Sprintf("z%04d", last-position)
	}
	sum := sha256.Sum256([]byte(functionName))
	return fmt.Sprintf("_pgroll_trigger_%s_%03d_%s", key, min(max(index, 0), 999), hex.EncodeToString(sum[:8]))
}
