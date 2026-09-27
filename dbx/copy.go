package dbx

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// CopyFromExecutor is implemented by pgx connections, transactions, and pools.
type CopyFromExecutor interface {
	CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error)
}

// CopyFrom performs PostgreSQL binary COPY using validated identifiers and
// pgx's typed row source. Open the pool with OpenPGXPool so the operation is
// traced. Row values travel through pgx's COPY protocol; they are never
// interpolated into SQL. Resolve external names through a fixed allowlist
// before calling Ident, because syntactic validation is not authorization.
func CopyFrom(ctx context.Context, executor CopyFromExecutor, table Identifier, columns []Identifier, source pgx.CopyFromSource) (int64, error) {
	if executor == nil {
		return 0, errors.New("dbx: copy executor is required")
	}
	if !table.valid() {
		return 0, errors.New("dbx: invalid copy table identifier")
	}
	if len(columns) == 0 {
		return 0, errors.New("dbx: copy requires at least one column")
	}
	if source == nil {
		return 0, errors.New("dbx: copy source is required")
	}

	tableName := make(pgx.Identifier, len(table.parts))
	copy(tableName, table.parts)
	columnNames := make([]string, len(columns))
	for i, column := range columns {
		if !column.valid() || len(column.parts) != 1 {
			return 0, errors.New("dbx: copy column must be a single valid identifier")
		}
		columnNames[i] = column.parts[0]
	}
	return executor.CopyFrom(ctx, tableName, columnNames, source)
}
