package dbx

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var identifierPart = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Identifier is a validated and quoted PostgreSQL identifier. Construct it
// from trusted application schema names, never directly from request input.
type Identifier struct{ parts []string }

// Ident validates one identifier or a schema-qualified identifier such as
// "public.users". Each part is quoted separately in generated SQL.
func Ident(name string) (Identifier, error) {
	parts := strings.Split(name, ".")
	if len(parts) == 0 || len(parts) > 2 {
		return Identifier{}, errors.New("dbx: identifier must have one or two parts")
	}
	for _, part := range parts {
		if !identifierPart.MatchString(part) {
			return Identifier{}, fmt.Errorf("dbx: invalid identifier %q", name)
		}
	}
	return Identifier{parts: parts}, nil
}

func (i Identifier) valid() bool {
	if len(i.parts) == 0 || len(i.parts) > 2 {
		return false
	}
	for _, part := range i.parts {
		if !identifierPart.MatchString(part) {
			return false
		}
	}
	return true
}

func (i Identifier) sql() string {
	quoted := make([]string, len(i.parts))
	for n, part := range i.parts {
		quoted[n] = `"` + part + `"`
	}
	return strings.Join(quoted, ".")
}

// Assignment binds a value to a trusted column identifier.
type Assignment struct {
	Column Identifier
	Value  any
}

// Eq creates a parameterized equality predicate. A nil value is rendered as
// IS NULL, avoiding PostgreSQL's three-valued NULL comparison behavior.
type Predicate struct {
	column Identifier
	value  any
}

func Eq(column Identifier, value any) Predicate { return Predicate{column: column, value: value} }

// BuildSelect creates a parameterized SELECT. A non-positive limit means no
// LIMIT clause; negative limits are rejected.
func BuildSelect(table Identifier, columns []Identifier, where []Predicate, limit int) (string, []any, error) {
	if !table.valid() {
		return "", nil, errors.New("dbx: invalid table identifier")
	}
	if len(columns) == 0 {
		return "", nil, errors.New("dbx: select requires at least one column")
	}
	if limit < 0 {
		return "", nil, errors.New("dbx: limit cannot be negative")
	}
	quoted := make([]string, len(columns))
	for n, column := range columns {
		if !column.valid() {
			return "", nil, errors.New("dbx: invalid select column")
		}
		quoted[n] = column.sql()
	}
	query, args, err := whereClause("SELECT "+strings.Join(quoted, ", ")+" FROM "+table.sql(), where, 1)
	if err != nil {
		return "", nil, err
	}
	if limit > 0 {
		args = append(args, limit)
		query += fmt.Sprintf(" LIMIT $%d", len(args))
	}
	return query, args, nil
}

// BuildInsert creates a parameterized INSERT with a RETURNING-free statement.
// Use QueryRowContext with an explicit RETURNING query when the caller needs
// returned values, composing only validated identifiers and placeholders.
func BuildInsert(table Identifier, values []Assignment) (string, []any, error) {
	if !table.valid() {
		return "", nil, errors.New("dbx: invalid table identifier")
	}
	if len(values) == 0 {
		return "", nil, errors.New("dbx: insert requires values")
	}
	cols, placeholders, args, err := assignments(values, 1)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", table.sql(), strings.Join(cols, ", "), strings.Join(placeholders, ", ")), args, nil
}

// BuildUpdate creates a parameterized UPDATE and requires at least one
// predicate, preventing accidental table-wide updates.
func BuildUpdate(table Identifier, values []Assignment, where []Predicate) (string, []any, error) {
	if !table.valid() {
		return "", nil, errors.New("dbx: invalid table identifier")
	}
	if len(values) == 0 {
		return "", nil, errors.New("dbx: update requires values")
	}
	if len(where) == 0 {
		return "", nil, errors.New("dbx: update requires at least one predicate")
	}
	cols, placeholders, args, err := assignments(values, 1)
	if err != nil {
		return "", nil, err
	}
	sets := make([]string, len(cols))
	for n := range cols {
		sets[n] = cols[n] + " = " + placeholders[n]
	}
	query, args, err := whereClause("UPDATE "+table.sql()+" SET "+strings.Join(sets, ", "), where, len(args)+1, args...)
	return query, args, err
}

// BuildDelete creates a parameterized DELETE and requires at least one
// predicate, preventing accidental table-wide deletion.
func BuildDelete(table Identifier, where []Predicate) (string, []any, error) {
	if !table.valid() {
		return "", nil, errors.New("dbx: invalid table identifier")
	}
	if len(where) == 0 {
		return "", nil, errors.New("dbx: delete requires at least one predicate")
	}
	return whereClause("DELETE FROM "+table.sql(), where, 1)
}

func assignments(values []Assignment, firstArg int) ([]string, []string, []any, error) {
	cols := make([]string, len(values))
	placeholders := make([]string, len(values))
	args := make([]any, len(values))
	seen := make(map[string]struct{}, len(values))
	for i, assignment := range values {
		if !assignment.Column.valid() {
			return nil, nil, nil, errors.New("dbx: invalid assignment column")
		}
		col := assignment.Column.sql()
		if _, ok := seen[col]; ok {
			return nil, nil, nil, fmt.Errorf("dbx: duplicate assignment column %s", col)
		}
		seen[col] = struct{}{}
		cols[i] = col
		placeholders[i] = fmt.Sprintf("$%d", firstArg+i)
		args[i] = assignment.Value
	}
	return cols, placeholders, args, nil
}

func whereClause(prefix string, where []Predicate, firstArg int, args ...any) (string, []any, error) {
	if len(where) == 0 {
		return prefix, args, nil
	}
	conditions := make([]string, 0, len(where))
	seen := make(map[string]struct{}, len(where))
	nextArg := firstArg
	for _, predicate := range where {
		if !predicate.column.valid() {
			return "", nil, errors.New("dbx: invalid predicate column")
		}
		column := predicate.column.sql()
		if _, ok := seen[column]; ok {
			return "", nil, fmt.Errorf("dbx: duplicate predicate column %s", column)
		}
		seen[column] = struct{}{}
		if predicate.value == nil {
			conditions = append(conditions, column+" IS NULL")
			continue
		}
		args = append(args, predicate.value)
		conditions = append(conditions, fmt.Sprintf("%s = $%d", column, nextArg))
		nextArg++
	}
	return prefix + " WHERE " + strings.Join(conditions, " AND "), args, nil
}
