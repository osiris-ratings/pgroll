// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"

	"github.com/lib/pq"

	"github.com/xataio/pgroll/pkg/db"
	"github.com/xataio/pgroll/pkg/schema"
)

// ValidatePreconditions checks schema-level preconditions against the given schema.
// Database-level preconditions (function_exists, type_exists) are skipped here
// and must be validated separately via ValidateDBPreconditions.
func ValidatePreconditions(preconditions []Precondition, s *schema.Schema) error {
	for _, p := range preconditions {
		if err := validatePrecondition(p, s); err != nil {
			return err
		}
	}
	return nil
}

// ValidateDBPreconditions checks preconditions that require database access
// (function_exists, type_exists). Schema-level preconditions are skipped.
func ValidateDBPreconditions(ctx context.Context, preconditions []Precondition, conn db.DB, schemaName string) error {
	for _, p := range preconditions {
		if err := validateDBPrecondition(ctx, p, conn, schemaName); err != nil {
			return err
		}
	}
	return nil
}

func validatePrecondition(p Precondition, s *schema.Schema) error {
	switch {
	case p.TableExists != nil:
		return validateTableExists(*p.TableExists, s)
	case p.TableNotExists != nil:
		return validateTableNotExists(*p.TableNotExists, s)
	case p.ColumnExists != nil:
		return validateColumnExists(p.ColumnExists, s)
	case p.ColumnNotExists != nil:
		return validateColumnNotExists(p.ColumnNotExists, s)
	case p.IndexExists != nil:
		return validateIndexExists(p.IndexExists, s)
	case p.ConstraintExists != nil:
		return validateConstraintExists(p.ConstraintExists, s)
	case p.FunctionExists != nil, p.TypeExists != nil:
		// Validated in ValidateDBPreconditions
		return nil
	default:
		return fmt.Errorf("precondition has no assertion specified")
	}
}

func validateDBPrecondition(ctx context.Context, p Precondition, conn db.DB, schemaName string) error {
	switch {
	case p.FunctionExists != nil:
		return validateFunctionExists(ctx, p.FunctionExists, conn, schemaName)
	case p.TypeExists != nil:
		return validateTypeExists(ctx, p.TypeExists, conn, schemaName)
	default:
		// Schema-level preconditions handled elsewhere
		return nil
	}
}

func validateTableExists(tableName string, s *schema.Schema) error {
	table := s.GetTable(tableName)
	if table == nil {
		return fmt.Errorf("precondition failed: table %q does not exist", tableName)
	}
	return nil
}

func validateTableNotExists(tableName string, s *schema.Schema) error {
	table := s.GetTable(tableName)
	if table != nil {
		return fmt.Errorf("precondition failed: table %q exists but should not", tableName)
	}
	return nil
}

func validateColumnExists(ref *PreconditionColumnExists, s *schema.Schema) error {
	table := s.GetTable(ref.Table)
	if table == nil {
		return fmt.Errorf("precondition failed: table %q does not exist (checking for column %q)", ref.Table, ref.Column)
	}
	col, ok := table.Columns[ref.Column]
	if !ok {
		return fmt.Errorf("precondition failed: column %q does not exist on table %q", ref.Column, ref.Table)
	}
	if ref.Type != nil && col.Type != *ref.Type {
		return fmt.Errorf("precondition failed: column %q on table %q has type %q but expected %q",
			ref.Column, ref.Table, col.Type, *ref.Type)
	}
	return nil
}

func validateColumnNotExists(ref *PreconditionColumnRef, s *schema.Schema) error {
	table := s.GetTable(ref.Table)
	if table == nil {
		// Table doesn't exist, so column can't exist either
		return nil
	}
	if _, ok := table.Columns[ref.Column]; ok {
		return fmt.Errorf("precondition failed: column %q exists on table %q but should not", ref.Column, ref.Table)
	}
	return nil
}

func validateIndexExists(ref *PreconditionIndexRef, s *schema.Schema) error {
	table := s.GetTable(ref.Table)
	if table == nil {
		return fmt.Errorf("precondition failed: table %q does not exist (checking for index %q)", ref.Table, ref.Index)
	}
	if _, ok := table.Indexes[ref.Index]; !ok {
		return fmt.Errorf("precondition failed: index %q does not exist on table %q", ref.Index, ref.Table)
	}
	return nil
}

func validateConstraintExists(ref *PreconditionConstraintRef, s *schema.Schema) error {
	table := s.GetTable(ref.Table)
	if table == nil {
		return fmt.Errorf("precondition failed: table %q does not exist (checking for constraint %q)", ref.Table, ref.Constraint)
	}

	// Check across all constraint types
	if _, ok := table.CheckConstraints[ref.Constraint]; ok {
		return nil
	}
	if _, ok := table.UniqueConstraints[ref.Constraint]; ok {
		return nil
	}
	if _, ok := table.ForeignKeys[ref.Constraint]; ok {
		return nil
	}
	if _, ok := table.ExcludeConstraints[ref.Constraint]; ok {
		return nil
	}

	return fmt.Errorf("precondition failed: constraint %q does not exist on table %q", ref.Constraint, ref.Table)
}

func validateFunctionExists(ctx context.Context, ref *PreconditionFunctionRef, conn db.DB, defaultSchema string) error {
	fnSchema := ref.Schema
	if fnSchema == "" {
		fnSchema = defaultSchema
	}

	// Query pg_proc for the function
	query := `SELECT p.prosrc,
		pg_get_function_arguments(p.oid) || ' -> ' || pg_get_function_result(p.oid) AS signature
		FROM pg_proc p
		JOIN pg_namespace n ON p.pronamespace = n.oid
		WHERE n.nspname = $1 AND p.proname = $2`

	rows, err := conn.QueryContext(ctx, query, fnSchema, ref.Name)
	if err != nil {
		return fmt.Errorf("precondition check failed: querying function %q.%q: %w", fnSchema, ref.Name, err)
	}
	defer rows.Close()

	var found bool
	for rows.Next() {
		var body, signature string
		if err := rows.Scan(&body, &signature); err != nil {
			return fmt.Errorf("precondition check failed: scanning function %q.%q: %w", fnSchema, ref.Name, err)
		}
		found = true

		// Check signature if specified
		if ref.Signature != nil && signature != *ref.Signature {
			// Try next overload
			continue
		}

		// Check body hash if specified
		if ref.BodyHash != nil {
			actualHash := "sha256:" + fmt.Sprintf("%x", sha256.Sum256([]byte(body)))
			if actualHash != *ref.BodyHash {
				return fmt.Errorf("precondition failed: function %q.%q body hash mismatch: expected %q, got %q",
					fnSchema, ref.Name, *ref.BodyHash, actualHash)
			}
		}

		// Signature and hash both match (or weren't specified)
		return nil
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("precondition check failed: iterating function %q.%q results: %w", fnSchema, ref.Name, err)
	}

	if !found {
		return fmt.Errorf("precondition failed: function %q.%q does not exist", fnSchema, ref.Name)
	}

	// Found overloads but none matched the signature
	if ref.Signature != nil {
		return fmt.Errorf("precondition failed: function %q.%q exists but no overload matches signature %q",
			fnSchema, ref.Name, *ref.Signature)
	}

	return nil
}

func validateTypeExists(ctx context.Context, ref *PreconditionTypeRef, conn db.DB, defaultSchema string) error {
	typeSchema := ref.Schema
	if typeSchema == "" {
		typeSchema = defaultSchema
	}

	// First check the type exists
	existsQuery := `SELECT t.typtype
		FROM pg_type t
		JOIN pg_namespace n ON t.typnamespace = n.oid
		WHERE n.nspname = $1 AND t.typname = $2`

	rows, err := conn.QueryContext(ctx, existsQuery, typeSchema, ref.Name)
	if err != nil {
		return fmt.Errorf("precondition check failed: querying type %q.%q: %w", typeSchema, ref.Name, err)
	}
	defer rows.Close()

	if !rows.Next() {
		return fmt.Errorf("precondition failed: type %q.%q does not exist", typeSchema, ref.Name)
	}
	var typType string
	if err := rows.Scan(&typType); err != nil {
		return fmt.Errorf("precondition check failed: scanning type %q.%q: %w", typeSchema, ref.Name, err)
	}
	rows.Close()

	// Check values hash if specified
	if ref.ValuesHash != nil {
		if typType != "e" {
			return fmt.Errorf("precondition failed: type %q.%q is not an enum (cannot check values_hash)", typeSchema, ref.Name)
		}

		enumQuery := `SELECT array_agg(e.enumlabel ORDER BY e.enumsortorder)
			FROM pg_enum e
			JOIN pg_type t ON e.enumtypid = t.oid
			JOIN pg_namespace n ON t.typnamespace = n.oid
			WHERE n.nspname = $1 AND t.typname = $2`

		enumRows, err := conn.QueryContext(ctx, enumQuery, typeSchema, ref.Name)
		if err != nil {
			return fmt.Errorf("precondition check failed: querying enum values for %q.%q: %w", typeSchema, ref.Name, err)
		}
		defer enumRows.Close()

		var values []string
		if enumRows.Next() {
			if err := enumRows.Scan(pq.Array(&values)); err != nil {
				return fmt.Errorf("precondition check failed: scanning enum values for %q.%q: %w", typeSchema, ref.Name, err)
			}
		}

		sort.Strings(values)
		joined := strings.Join(values, ",")
		actualHash := "sha256:" + fmt.Sprintf("%x", sha256.Sum256([]byte(joined)))

		if actualHash != *ref.ValuesHash {
			return fmt.Errorf("precondition failed: enum %q.%q values hash mismatch: expected %q, got %q (values: [%s])",
				typeSchema, ref.Name, *ref.ValuesHash, actualHash, joined)
		}
	}

	return nil
}
