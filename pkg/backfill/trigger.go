// SPDX-License-Identifier: Apache-2.0

package backfill

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
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
	return executeTemplate("function", templates.Function, cfg)
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
func TriggerFunctionName(scope, tableName, columnName string) string {
	base := "_pgroll_trigger_" + tableName + "_" + columnName
	if scope == "" {
		return base
	}
	return base + "_" + scope
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
func OrderedTriggerName(direction TriggerDirection, position int, functionName string) string {
	const last = 9999
	position = min(max(position, 0), last)
	key := fmt.Sprintf("a%04d", position)
	if direction == TriggerDirectionDown {
		key = fmt.Sprintf("z%04d", last-position)
	}
	sum := sha256.Sum256([]byte(functionName))
	return "_pgroll_trigger_" + key + "_" + hex.EncodeToString(sum[:8])
}

// RetargetTrigger renames the column a trigger fills and rebuilds the trigger
// function to write it under its new name, in one transaction, so no write
// sees the function and the column disagree. trigger.PhysicalColumn is the
// new name. A re-run after the rename has landed only rebuilds the function.
// The rebuilt function follows the train's target.
func RetargetTrigger(ctx context.Context, conn db.DB, schemaName, scope string, trigger OperationTrigger, from string) error {
	cfg := triggerConfig{
		Name:                trigger.Name,
		Direction:           trigger.Direction,
		Columns:             trigger.Columns,
		SchemaName:          schemaName,
		TableName:           trigger.TableName,
		PhysicalColumn:      trigger.PhysicalColumn,
		TargetFunction:      TrainTargetFunction,
		SQL:                 []string{trigger.SQL},
		NeedsBackfillColumn: NeedsBackfillColumnName(scope),
		MigrationScope:      scope,
	}
	if sql := cfg.SQL[0]; len(sql) > 0 && sql[0] != '(' {
		cfg.SQL[0] = "(" + sql + ")"
	}
	funcSQL, err := buildFunction(cfg)
	if err != nil {
		return err
	}

	return conn.WithRetryableTransaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var pending bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_attribute
				WHERE attrelid = (quote_ident($1) || '.' || quote_ident($2))::regclass
				AND attname = $3 AND NOT attisdropped)`,
			schemaName, trigger.TableName, from).Scan(&pending); err != nil {
			return err
		}
		if pending {
			if _, err := tx.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s",
				pq.QuoteIdentifier(trigger.TableName),
				pq.QuoteIdentifier(from),
				pq.QuoteIdentifier(trigger.PhysicalColumn))); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, funcSQL)
		return err
	})
}
