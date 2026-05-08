// SPDX-License-Identifier: Apache-2.0

package backfill

import (
	"bytes"
	"context"
	"database/sql"
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
	Name                string
	Direction           TriggerDirection
	Columns             map[string]*schema.Column
	SchemaName          string
	TableName           string
	PhysicalColumn      string
	LatestSchema        string
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
