package dbx

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/multitracer"
)

type stubQueryTracer struct{}

func (stubQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (stubQueryTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestCombinePGXTracersPreservesConfiguredHooksAndAddsTelemetry(t *testing.T) {
	configured := stubQueryTracer{}
	combined := combinePGXTracers(configured)
	multi, ok := combined.(*multitracer.Tracer)
	if !ok {
		t.Fatalf("combined tracer type = %T, want *multitracer.Tracer", combined)
	}
	if len(multi.QueryTracers) != 2 || multi.QueryTracers[0] != configured {
		t.Fatalf("query tracers did not preserve the configured tracer: %#v", multi.QueryTracers)
	}
	if len(multi.BatchTracers) != 1 || len(multi.CopyFromTracers) != 1 || len(multi.PrepareTracers) != 1 {
		t.Fatalf("database operation hooks missing: batch=%d copy=%d prepare=%d", len(multi.BatchTracers), len(multi.CopyFromTracers), len(multi.PrepareTracers))
	}
}

func TestCombinePGXTracersWithoutConfiguredTracer(t *testing.T) {
	combined := combinePGXTracers(nil)
	if _, ok := combined.(pgx.CopyFromTracer); !ok {
		t.Fatalf("default tracer %T does not trace COPY", combined)
	}
}
