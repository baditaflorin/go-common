package graph

import (
	"testing"
	"time"
)

func TestFlushDelayStaysWithinTenPercentOfTheInterval(t *testing.T) {
	interval := 30 * time.Second
	for _, r := range []float64{0, 0.25, 0.5, 0.999999} {
		d := jitteredFlushDelay(interval, r)
		if d < 27*time.Second || d > 33*time.Second {
			t.Errorf("r=%v: delay %v is outside 27s..33s", r, d)
		}
	}
	if got := jitteredFlushDelay(interval, 0.5); got != interval {
		t.Errorf("the middle of the range must be the interval itself, got %v", got)
	}
}

func TestWritersFlushEveryThirtySecondsByDefault(t *testing.T) {
	t.Setenv("GRAPH_FLUSH_INTERVAL", "")
	if got := loadConfig().flushInterval; got != 30*time.Second {
		t.Errorf("default flush interval = %v, want 30s", got)
	}
}
