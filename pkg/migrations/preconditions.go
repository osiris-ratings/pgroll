// SPDX-License-Identifier: Apache-2.0

package migrations

import (
	"fmt"

	"github.com/xataio/pgroll/pkg/schema"
)

// ValidatePreconditions checks that all preconditions hold against the given schema.
// Returns an error describing the first precondition that fails.
func ValidatePreconditions(preconditions []Precondition, s *schema.Schema) error {
	for _, p := range preconditions {
		if err := validatePrecondition(p, s); err != nil {
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
	default:
		return fmt.Errorf("precondition has no assertion specified")
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
