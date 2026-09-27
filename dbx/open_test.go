package dbx

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

var registerTestDriver sync.Once

func TestOpenSQLCreatesRedactedSpans(t *testing.T) {
	recorder, cleanup := testProvider(t)
	registerTestDriver.Do(func() { sql.Register("dbx-test-driver", testDriver{}) })
	db, err := OpenSQL(context.Background(), "dbx-test-driver", "opaque-dsn-secret", "testdb", SQLConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := db.Stats().MaxOpenConnections; got != DefaultMaxOpenConnections {
		t.Fatalf("default max open connections = %d, want %d", got, DefaultMaxOpenConnections)
	}
	if _, err := db.ExecContext(context.Background(), "INSERT INTO t (v) VALUES ('literal-secret')", "bind-secret"); err != nil {
		t.Fatal(err)
	}
	cleanup()
	spans := recorder.Ended()
	if len(spans) == 0 {
		t.Fatal("expected SQL operation span")
	}
	for _, span := range spans {
		if strings.Contains(span.Name(), "literal-secret") || strings.Contains(span.Name(), "bind-secret") {
			t.Fatalf("span name leaks SQL data: %q", span.Name())
		}
		for _, attr := range span.Attributes() {
			value := attr.Value.AsString()
			if strings.Contains(value, "literal-secret") || strings.Contains(value, "bind-secret") || strings.Contains(value, "opaque-dsn-secret") {
				t.Fatalf("span leaks database data in %s=%q", attr.Key, value)
			}
		}
	}
}

func TestOpenSQLHonorsBoundedPoolOverrides(t *testing.T) {
	registerTestDriver.Do(func() { sql.Register("dbx-test-driver", testDriver{}) })
	db, err := OpenSQL(context.Background(), "dbx-test-driver", "opaque-dsn-secret", "testdb", SQLConfig{
		MaxOpenConnections: 3,
		MaxIdleConnections: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if got := db.Stats().MaxOpenConnections; got != 3 {
		t.Fatalf("max open connections = %d, want 3", got)
	}
}

func TestPGXTracerOmitsSQLAndErrorText(t *testing.T) {
	recorder, cleanup := testProvider(t)
	tracer := NewPGXQueryTracer().(pgx.QueryTracer)
	rootCtx, root := otel.Tracer("dbx-test").Start(context.Background(), "root")
	ctx := tracer.TraceQueryStart(rootCtx, nil, pgx.TraceQueryStartData{SQL: "UPDATE users SET token='sql-secret'", Args: []any{"bind-secret"}})
	tracer.TraceQueryEnd(ctx, nil, pgx.TraceQueryEndData{Err: errors.New("constraint detail contains error-secret")})
	root.End()
	cleanup()
	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want root + query", len(spans))
	}
	var span sdktrace.ReadOnlySpan
	for _, current := range spans {
		if current.Name() != "root" {
			span = current
		}
	}
	if span == nil {
		t.Fatal("missing query span")
	}
	if strings.Contains(span.Name(), "sql-secret") || strings.Contains(span.Name(), "error-secret") {
		t.Fatalf("span name leaked sensitive data: %q", span.Name())
	}
	for _, attr := range span.Attributes() {
		if strings.Contains(attr.Value.AsString(), "sql-secret") || strings.Contains(attr.Value.AsString(), "error-secret") {
			t.Fatalf("span leaked sensitive data in %s", attr.Key)
		}
	}
	for _, event := range span.Events() {
		for _, attr := range event.Attributes {
			if strings.Contains(attr.Value.AsString(), "error-secret") {
				t.Fatalf("span event leaked sensitive error text in %s", attr.Key)
			}
		}
	}
	if span.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Errorf("query span parent %s != root span %s", span.Parent().SpanID(), root.SpanContext().SpanID())
	}
}

func testProvider(t *testing.T) (*tracetest.SpanRecorder, func()) {
	t.Helper()
	previous := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(tp)
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			otel.SetTracerProvider(previous)
			_ = tp.Shutdown(context.Background())
		})
	}
	t.Cleanup(cleanup)
	return recorder, cleanup
}

type testDriver struct{}

func (testDriver) Open(string) (driver.Conn, error) { return testConn{}, nil }

type testConn struct{}

func (testConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("not supported") }
func (testConn) Close() error                        { return nil }
func (testConn) Begin() (driver.Tx, error)           { return testTx{}, nil }
func (testConn) Ping(context.Context) error          { return nil }
func (testConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}

type testTx struct{}

func (testTx) Commit() error   { return nil }
func (testTx) Rollback() error { return nil }

var _ driver.Driver = testDriver{}
var _ driver.Conn = testConn{}
var _ driver.Pinger = testConn{}
var _ driver.ExecerContext = testConn{}
var _ driver.Tx = testTx{}
