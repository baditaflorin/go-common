package dbx

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type fakeCopyExecutor struct {
	called  bool
	table   pgx.Identifier
	columns []string
	source  pgx.CopyFromSource
}

func (f *fakeCopyExecutor) CopyFrom(_ context.Context, table pgx.Identifier, columns []string, source pgx.CopyFromSource) (int64, error) {
	f.called = true
	f.table = table
	f.columns = columns
	f.source = source
	return 2, nil
}

func TestCopyFromUsesValidatedIdentifiersAndTypedRows(t *testing.T) {
	table, err := Ident("public.events")
	if err != nil {
		t.Fatal(err)
	}
	id, err := Ident("id")
	if err != nil {
		t.Fatal(err)
	}
	payload, err := Ident("payload")
	if err != nil {
		t.Fatal(err)
	}
	executor := &fakeCopyExecutor{}
	source := pgx.CopyFromRows([][]any{{int64(1), "value; DROP TABLE events;"}})
	rows, err := CopyFrom(context.Background(), executor, table, []Identifier{id, payload}, source)
	if err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("rows = %d, want 2", rows)
	}
	if !executor.called {
		t.Fatal("executor was not called")
	}
	if got := executor.table.Sanitize(); got != "\"public\".\"events\"" {
		t.Fatalf("table = %s", got)
	}
	if len(executor.columns) != 2 || executor.columns[0] != "id" || executor.columns[1] != "payload" {
		t.Fatalf("columns = %#v", executor.columns)
	}
	if executor.source != source {
		t.Fatal("typed row source was not passed through")
	}
}

func TestCopyFromRejectsUnsafeIdentifiersBeforeDatabaseCall(t *testing.T) {
	executor := &fakeCopyExecutor{}
	validColumn, _ := Ident("id")
	source := pgx.CopyFromRows([][]any{{1}})
	unsafe := Identifier{parts: []string{"events; DROP TABLE users"}}
	if _, err := CopyFrom(context.Background(), executor, unsafe, []Identifier{validColumn}, source); err == nil {
		t.Fatal("expected unsafe table to be rejected")
	}
	qualifiedColumn, _ := Ident("public.id")
	validTable, _ := Ident("public.events")
	if _, err := CopyFrom(context.Background(), executor, validTable, []Identifier{qualifiedColumn}, source); err == nil {
		t.Fatal("expected schema-qualified column to be rejected")
	}
	if executor.called {
		t.Fatal("database call occurred before identifier validation")
	}
}

func TestCopyFromRequiresColumnsAndSource(t *testing.T) {
	executor := &fakeCopyExecutor{}
	table, _ := Ident("events")
	if _, err := CopyFrom(context.Background(), executor, table, nil, pgx.CopyFromRows(nil)); err == nil {
		t.Fatal("expected missing columns to be rejected")
	}
	column, _ := Ident("id")
	if _, err := CopyFrom(context.Background(), executor, table, []Identifier{column}, nil); err == nil {
		t.Fatal("expected missing source to be rejected")
	}
	if executor.called {
		t.Fatal("database call occurred for invalid input")
	}
}

func TestPGXBatchTracerOmitsSQLAndRecordsErrorType(t *testing.T) {
	recorder, cleanup := testProvider(t)
	tracer, ok := NewPGXQueryTracer().(pgx.BatchTracer)
	if !ok {
		t.Fatal("pgx tracer does not implement BatchTracer")
	}
	rootCtx, root := otel.Tracer("dbx-test").Start(context.Background(), "root")
	ctx := tracer.TraceBatchStart(rootCtx, nil, pgx.TraceBatchStartData{})
	tracer.TraceBatchQuery(ctx, nil, pgx.TraceBatchQueryData{
		SQL:  "INSERT INTO tenant_secret (token) VALUES ($1)",
		Args: []any{"value_secret"},
		Err:  errors.New("row payload secret"),
	})
	tracer.TraceBatchEnd(ctx, nil, pgx.TraceBatchEndData{})
	root.End()
	cleanup()

	span := findSpan(t, recorder.Ended(), "postgres batch")
	if span.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Fatal("batch span is not a child of the request span")
	}
	if span.Status().Code.String() != "Error" {
		t.Fatalf("batch status = %s, want Error", span.Status().Code)
	}
	assertSpanOmits(t, span, "tenant_secret", "value_secret", "row payload secret")
}

func TestPGXPrepareTracerOmitsStatementDetails(t *testing.T) {
	recorder, cleanup := testProvider(t)
	tracer, ok := NewPGXQueryTracer().(pgx.PrepareTracer)
	if !ok {
		t.Fatal("pgx tracer does not implement PrepareTracer")
	}
	rootCtx, root := otel.Tracer("dbx-test").Start(context.Background(), "root")
	ctx := tracer.TracePrepareStart(rootCtx, nil, pgx.TracePrepareStartData{
		Name: "sensitive_statement_name",
		SQL:  "SELECT secret_column FROM tenant_secret",
	})
	tracer.TracePrepareEnd(ctx, nil, pgx.TracePrepareEndData{})
	root.End()
	cleanup()

	span := findSpan(t, recorder.Ended(), "postgres prepare")
	if span.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Fatal("prepare span is not a child of the request span")
	}
	assertSpanOmits(t, span, "sensitive_statement_name", "secret_column", "tenant_secret")
}

func findSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("span %q not found", name)
	return nil
}

func assertSpanOmits(t *testing.T, span sdktrace.ReadOnlySpan, values ...string) {
	t.Helper()
	for _, forbidden := range values {
		if strings.Contains(span.Name(), forbidden) {
			t.Fatalf("span name leaked %q", forbidden)
		}
		for _, attr := range span.Attributes() {
			if strings.Contains(attr.Value.AsString(), forbidden) {
				t.Fatalf("attribute %s leaked %q", attr.Key, forbidden)
			}
		}
		for _, event := range span.Events() {
			for _, attr := range event.Attributes {
				if strings.Contains(attr.Value.AsString(), forbidden) {
					t.Fatalf("event attribute %s leaked %q", attr.Key, forbidden)
				}
			}
		}
	}
}

func TestPGXCopyTracerOmitsIdentifiersAndRawError(t *testing.T) {
	recorder, cleanup := testProvider(t)
	tracer, ok := NewPGXQueryTracer().(pgx.CopyFromTracer)
	if !ok {
		t.Fatal("pgx tracer does not implement CopyFromTracer")
	}
	rootCtx, root := otel.Tracer("dbx-test").Start(context.Background(), "root")
	ctx := tracer.TraceCopyFromStart(rootCtx, nil, pgx.TraceCopyFromStartData{
		TableName:   pgx.Identifier{"tenant_secret", "records"},
		ColumnNames: []string{"token_secret"},
	})
	tracer.TraceCopyFromEnd(ctx, nil, pgx.TraceCopyFromEndData{Err: errors.New("row payload secret")})
	root.End()
	cleanup()

	spans := recorder.Ended()
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want root + copy", len(spans))
	}
	var copySpan = spans[0]
	if copySpan.Name() == "root" {
		copySpan = spans[1]
	}
	if copySpan.Name() != "postgres copy" {
		t.Fatalf("span name = %q", copySpan.Name())
	}
	if copySpan.Parent().SpanID() != root.SpanContext().SpanID() {
		t.Fatal("copy span is not a child of the request span")
	}
	for _, secret := range []string{"tenant_secret", "token_secret", "row payload secret"} {
		if strings.Contains(copySpan.Name(), secret) {
			t.Fatalf("span name leaked %q", secret)
		}
		for _, attr := range copySpan.Attributes() {
			if strings.Contains(attr.Value.AsString(), secret) {
				t.Fatalf("attribute %s leaked %q", attr.Key, secret)
			}
		}
		for _, event := range copySpan.Events() {
			for _, attr := range event.Attributes {
				if strings.Contains(attr.Value.AsString(), secret) {
					t.Fatalf("event attribute %s leaked %q", attr.Key, secret)
				}
			}
		}
	}
}
