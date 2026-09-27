// Package dbx provides context-aware SQL/PostgreSQL connections with
// OpenTelemetry spans and parameterized CRUD query builders.
package dbx

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/XSAM/otelsql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	DefaultMaxOpenConnections = 10
	DefaultMaxIdleConnections = 5
	DefaultConnMaxLifetime    = 30 * time.Minute
	DefaultConnMaxIdleTime    = 5 * time.Minute
	DefaultPingTimeout        = 5 * time.Second
)

// SQLConfig controls database/sql pool behavior. Zero values use bounded
// defaults: 10 open / 5 idle connections, 30-minute max lifetime, 5-minute
// idle timeout, and a 5-second initial ping. A negative MaxIdleConnections
// disables idle connections.
type SQLConfig struct {
	MaxOpenConnections int
	MaxIdleConnections int
	ConnMaxLifetime    time.Duration
	ConnMaxIdleTime    time.Duration
	PingTimeout        time.Duration
}

// OpenSQL opens a database/sql connection using an already-registered driver.
// It instruments operations without exporting SQL text, bind arguments, DSNs,
// or raw driver error messages. The caller owns db.Close().
func OpenSQL(ctx context.Context, driverName, dataSourceName, dbSystem string, cfg SQLConfig) (*sql.DB, error) {
	if driverName == "" {
		return nil, errors.New("dbx: driver name is required")
	}
	if dbSystem == "" {
		return nil, errors.New("dbx: database system name is required")
	}
	options := []otelsql.Option{
		otelsql.WithAttributes(attribute.String("db.system.name", dbSystem)),
		otelsql.WithSpanOptions(otelsql.SpanOptions{
			DisableQuery: true,
			RecordError:  func(error) bool { return false },
		}),
	}
	db, err := otelsql.Open(driverName, dataSourceName, options...)
	if err != nil {
		return nil, fmt.Errorf("dbx: open %s database: %w", dbSystem, err)
	}
	maxOpen := cfg.MaxOpenConnections
	if maxOpen <= 0 {
		maxOpen = DefaultMaxOpenConnections
	}
	db.SetMaxOpenConns(maxOpen)
	if cfg.MaxIdleConnections > 0 {
		maxIdle := cfg.MaxIdleConnections
		if maxIdle > maxOpen {
			maxIdle = maxOpen
		}
		db.SetMaxIdleConns(maxIdle)
	} else if cfg.MaxIdleConnections < 0 {
		db.SetMaxIdleConns(0)
	} else {
		maxIdle := DefaultMaxIdleConnections
		if maxIdle > maxOpen {
			maxIdle = maxOpen
		}
		db.SetMaxIdleConns(maxIdle)
	}
	if cfg.ConnMaxLifetime > 0 {
		db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	} else {
		db.SetConnMaxLifetime(DefaultConnMaxLifetime)
	}
	if cfg.ConnMaxIdleTime > 0 {
		db.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)
	} else {
		db.SetConnMaxIdleTime(DefaultConnMaxIdleTime)
	}
	pingTimeout := cfg.PingTimeout
	if pingTimeout <= 0 {
		pingTimeout = DefaultPingTimeout
	}
	pingCtx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("dbx: ping %s database: %w", dbSystem, err)
	}
	return db, nil
}

// OpenPostgres opens a PostgreSQL database/sql pool using pgx's stdlib driver.
// SQL values remain bind parameters; do not interpolate untrusted values into
// SQL strings. For SQL injection-resistant CRUD, use the builders in builder.go.
func OpenPostgres(ctx context.Context, dataSourceName string, cfg SQLConfig) (*sql.DB, error) {
	_ = stdlib.GetDefaultDriver() // ensure the pgx database/sql driver is linked
	return OpenSQL(ctx, "pgx", dataSourceName, "postgresql", cfg)
}

// OpenPGXPool opens a native pgx pool and installs a query tracer which records
// operation kind, duration, and success status only. SQL text, parameters,
// connection details, and raw error messages are never added to spans.
func OpenPGXPool(ctx context.Context, dataSourceName string, configure func(*pgxpool.Config)) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dataSourceName)
	if err != nil {
		return nil, fmt.Errorf("dbx: parse PostgreSQL pool configuration: %w", err)
	}
	if configure != nil {
		configure(cfg)
	}
	cfg.ConnConfig.Tracer = combinePGXTracers(cfg.ConnConfig.Tracer)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("dbx: create PostgreSQL pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("dbx: ping PostgreSQL pool: %w", err)
	}
	return pool, nil
}

func combinePGXTracers(configured pgx.QueryTracer) pgx.QueryTracer {
	if configured == nil {
		return NewPGXQueryTracer()
	}
	return multitracer.New(configured, NewPGXQueryTracer())
}

type pgxSpanKey string

const (
	querySpanKey   pgxSpanKey = "query"
	batchSpanKey   pgxSpanKey = "batch"
	copySpanKey    pgxSpanKey = "copy"
	prepareSpanKey pgxSpanKey = "prepare"
)

type pgxQueryTracer struct{ tracer trace.Tracer }

// NewPGXQueryTracer returns a privacy-conscious pgx QueryTracer. It can be
// assigned to pgx.ConnConfig.Tracer when an application owns pool creation.
func NewPGXQueryTracer() pgx.QueryTracer {
	return &pgxQueryTracer{tracer: otel.Tracer("github.com/baditaflorin/go-common/dbx")}
}

func (t *pgxQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	operation := safeOperation(data.SQL)
	ctx, span := t.start(ctx, operation)
	return context.WithValue(ctx, querySpanKey, span)
}

func (t *pgxQueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	t.finish(ctx, querySpanKey, data.Err)
}

// TraceBatchStart records a single span for a pgx batch. Query text and
// parameter values are intentionally omitted.
func (t *pgxQueryTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceBatchStartData) context.Context {
	ctx, span := t.start(ctx, "batch")
	return context.WithValue(ctx, batchSpanKey, span)
}

func (t *pgxQueryTracer) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if data.Err == nil {
		return
	}
	if span, ok := ctx.Value(batchSpanKey).(trace.Span); ok {
		t.recordError(span, data.Err)
	}
}

func (t *pgxQueryTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	t.finish(ctx, batchSpanKey, data.Err)
}

// TraceCopyFrom records pgx binary COPY operations without table, column, or
// row data. The span remains attached to the request trace context.
func (t *pgxQueryTracer) TraceCopyFromStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceCopyFromStartData) context.Context {
	ctx, span := t.start(ctx, "copy")
	return context.WithValue(ctx, copySpanKey, span)
}

func (t *pgxQueryTracer) TraceCopyFromEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceCopyFromEndData) {
	t.finish(ctx, copySpanKey, data.Err)
}

// TracePrepare records the prepare operation kind only; statement names and
// SQL text are omitted from trace data.
func (t *pgxQueryTracer) TracePrepareStart(ctx context.Context, _ *pgx.Conn, _ pgx.TracePrepareStartData) context.Context {
	ctx, span := t.start(ctx, "prepare")
	return context.WithValue(ctx, prepareSpanKey, span)
}

func (t *pgxQueryTracer) TracePrepareEnd(ctx context.Context, _ *pgx.Conn, data pgx.TracePrepareEndData) {
	t.finish(ctx, prepareSpanKey, data.Err)
}

func (t *pgxQueryTracer) start(ctx context.Context, operation string) (context.Context, trace.Span) {
	return t.tracer.Start(ctx, "postgres "+operation,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("db.system.name", "postgresql"),
			attribute.String("db.operation.name", operation),
		),
	)
}

func (t *pgxQueryTracer) finish(ctx context.Context, key pgxSpanKey, err error) {
	span, ok := ctx.Value(key).(trace.Span)
	if !ok {
		return
	}
	if err != nil {
		t.recordError(span, err)
	}
	span.End()
}

func (t *pgxQueryTracer) recordError(span trace.Span, err error) {
	span.SetStatus(codes.Error, "database error")
	span.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", err)))
}

func safeOperation(query string) string {
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return "other"
	}
	switch strings.ToUpper(fields[0]) {
	case "SELECT", "INSERT", "UPDATE", "DELETE", "BEGIN", "COMMIT", "ROLLBACK", "WITH":
		return strings.ToLower(fields[0])
	default:
		return "other"
	}
}
