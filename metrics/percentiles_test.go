package metrics

import (
	"slices"
	"testing"
	"time"
)

func TestPercentileHelperPreservesInput(t *testing.T) {
	input := []time.Duration{9 * time.Millisecond, time.Millisecond, 5 * time.Millisecond}
	original := slices.Clone(input)
	p50, p95, p99 := computePercentiles(input)
	if !slices.Equal(input, original) {
		t.Fatal("pure percentile helper reordered caller samples")
	}
	if p50 != "1ms" || p95 != "5ms" || p99 != "5ms" {
		t.Fatalf("percentiles = %s/%s/%s", p50, p95, p99)
	}
	q50, q95, q99 := computePercentilesInPlace(input)
	if q50 != p50 || q95 != p95 || q99 != p99 || !slices.IsSorted(input) {
		t.Fatal("private in-place helper changed percentile semantics")
	}
}
