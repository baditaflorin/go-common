package bootgate

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeClock replaces now/sleep/readLoad/hostname for a test. sleep advances
// virtual time instantly and records the requested durations.
type fakeClock struct {
	t      time.Time
	sleeps []time.Duration
	loads  []float64 // consumed one per readLoad; last value repeats
	known  bool
	reads  int
}

func install(t *testing.T, fc *fakeClock) {
	t.Helper()
	oldNow, oldSleep, oldRead, oldHost := now, sleep, readLoad, hostname
	t.Cleanup(func() { now, sleep, readLoad, hostname = oldNow, oldSleep, oldRead, oldHost })
	fc.t = time.Unix(1_000_000, 0)
	now = func() time.Time { return fc.t }
	sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		fc.sleeps = append(fc.sleeps, d)
		fc.t = fc.t.Add(d)
		return nil
	}
	readLoad = func() (float64, bool) {
		if !fc.known || len(fc.loads) == 0 {
			return 0, false
		}
		i := fc.reads
		if i >= len(fc.loads) {
			i = len(fc.loads) - 1
		}
		fc.reads++
		return fc.loads[i], true
	}
	hostname = func() (string, error) { return "host-a", nil }
}

func TestZeroConfigReturnsImmediately(t *testing.T) {
	fc := &fakeClock{known: true, loads: []float64{999}}
	install(t, fc)
	res, err := Wait(context.Background(), Config{})
	if err != nil || res != (Result{}) {
		t.Fatalf("got %+v, %v; want zero result", res, err)
	}
	if len(fc.sleeps) != 0 || fc.reads != 0 {
		t.Fatalf("zero config slept %v / read load %d times", fc.sleeps, fc.reads)
	}
}

func TestBaseDelayAndJitter(t *testing.T) {
	fc := &fakeClock{}
	install(t, fc)
	cfg := Config{BaseDelay: 2 * time.Second, JitterMax: 60 * time.Second, Seed: "x"}
	res, err := Wait(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := cfg.BaseDelay + jitterFor(cfg)
	if res.Jitter != jitterFor(cfg) || res.Waited != want {
		t.Fatalf("waited %s jitter %s, want waited %s", res.Waited, res.Jitter, want)
	}
}

func TestJitterDeterministicAndBounded(t *testing.T) {
	fc := &fakeClock{}
	install(t, fc)
	max := 90 * time.Second
	a1 := jitterFor(Config{JitterMax: max, Seed: "alpha"})
	a2 := jitterFor(Config{JitterMax: max, Seed: "alpha"})
	b := jitterFor(Config{JitterMax: max, Seed: "beta"})
	if a1 != a2 {
		t.Fatalf("same seed gave %s and %s", a1, a2)
	}
	if a1 == b {
		t.Fatalf("different seeds gave same jitter %s", a1)
	}
	for i := 0; i < 5000; i++ {
		j := jitterFor(Config{JitterMax: max, Seed: fmt.Sprintf("s%d", i)})
		if j < 0 || j >= max {
			t.Fatalf("jitter %s outside [0,%s)", j, max)
		}
	}
}

func TestJitterSeedFallbacks(t *testing.T) {
	fc := &fakeClock{}
	install(t, fc)
	max := time.Hour
	if got, want := jitterFor(Config{JitterMax: max, Name: "svc"}), jitterFor(Config{JitterMax: max, Seed: "host-a"}); got != want {
		t.Errorf("hostname fallback: got %s want %s", got, want)
	}
	hostname = func() (string, error) { return "", errors.New("no host") }
	if got, want := jitterFor(Config{JitterMax: max, Name: "svc"}), jitterFor(Config{JitterMax: max, Seed: "svc"}); got != want {
		t.Errorf("name fallback: got %s want %s", got, want)
	}
}

func TestJitterEvenSpread(t *testing.T) {
	fc := &fakeClock{}
	install(t, fc)
	for _, pattern := range []string{"container-%03d", "%012x"} {
		max := 300 * time.Second
		const n, buckets = 300, 10
		var count [buckets]int
		for i := 0; i < n; i++ {
			j := jitterFor(Config{JitterMax: max, Seed: fmt.Sprintf(pattern, i)})
			count[int(int64(j)*buckets/int64(max))]++
		}
		mean := float64(n) / buckets
		for b, c := range count {
			if c == 0 {
				t.Errorf("%s: bucket %d empty: %v", pattern, b, count)
			}
			if float64(c) >= 2*mean {
				t.Errorf("%s: bucket %d has %d >= 2x mean %.0f: %v", pattern, b, c, mean, count)
			}
		}
	}
}

func TestLoadGate(t *testing.T) {
	tests := []struct {
		name         string
		known        bool
		loads        []float64
		cfg          Config
		wantSleeps   int
		wantTimedOut bool
		wantKnown    bool
		wantStart    float64
		wantEnd      float64
	}{
		{"load low proceeds at once", true, []float64{1}, Config{MaxLoad: 4}, 0, false, true, 1, 1},
		{"waits while high then proceeds", true, []float64{9, 8, 3}, Config{MaxLoad: 4, PollEvery: time.Second}, 2, false, true, 9, 3},
		{"equal to max keeps waiting", true, []float64{4, 3.99}, Config{MaxLoad: 4, PollEvery: time.Second}, 1, false, true, 4, 3.99},
		{"timeout is not an error", true, []float64{50}, Config{MaxLoad: 4, PollEvery: time.Second, MaxWait: 10 * time.Second}, 10, true, true, 50, 50},
		{"default MaxWait is 5m", true, []float64{50}, Config{MaxLoad: 4, PollEvery: time.Minute}, 5, true, true, 50, 50},
		{"unknown load does not block", false, nil, Config{MaxLoad: 4}, 0, false, false, 0, 0},
		{"gate off ignores load", true, []float64{999}, Config{}, 0, false, false, 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fc := &fakeClock{known: tc.known, loads: tc.loads}
			install(t, fc)
			res, err := Wait(context.Background(), tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			if len(fc.sleeps) != tc.wantSleeps || res.TimedOut != tc.wantTimedOut || res.LoadKnown != tc.wantKnown ||
				res.LoadAtStart != tc.wantStart || res.LoadAtEnd != tc.wantEnd {
				t.Fatalf("sleeps=%d res=%+v", len(fc.sleeps), res)
			}
			if tc.wantTimedOut {
				limit := tc.cfg.MaxWait
				if limit == 0 {
					limit = DefaultMaxWait
				}
				if res.Waited != limit {
					t.Errorf("Waited=%s want %s", res.Waited, limit)
				}
			}
		})
	}
}

func TestPollNeverOvershootsMaxWait(t *testing.T) {
	fc := &fakeClock{known: true, loads: []float64{50}}
	install(t, fc)
	res, _ := Wait(context.Background(), Config{MaxLoad: 1, PollEvery: 7 * time.Second, MaxWait: 10 * time.Second})
	if !res.TimedOut || res.Waited != 10*time.Second {
		t.Fatalf("%+v", res)
	}
}

func TestContextCancel(t *testing.T) {
	t.Run("already cancelled", func(t *testing.T) {
		fc := &fakeClock{known: true, loads: []float64{50}}
		install(t, fc)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := Wait(ctx, Config{BaseDelay: time.Hour}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("during load gate", func(t *testing.T) {
		fc := &fakeClock{known: true, loads: []float64{50}}
		install(t, fc)
		ctx, cancel := context.WithCancel(context.Background())
		sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
		if _, err := Wait(ctx, Config{MaxLoad: 1}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("real sleep returns promptly", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() { time.Sleep(20 * time.Millisecond); cancel() }()
		start := time.Now()
		_, err := Wait(ctx, Config{BaseDelay: time.Hour})
		if !errors.Is(err, context.Canceled) || time.Since(start) > 2*time.Second {
			t.Fatalf("err=%v after %s", err, time.Since(start))
		}
	})
}

func TestParseLoadavg(t *testing.T) {
	tests := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"0.52 0.58 0.59 1/467 12345\n", 0.52, true},
		{"468.10 300.00 100.00 400/900 1\n", 468.10, true},
		{"", 0, false},
		{"   \n", 0, false},
		{"garbage 1 2", 0, false},
		{"-1 0 0", 0, false},
		{"NaN 0 0", 0, false},
		{"Inf 0 0", 0, false},
	}
	for _, tc := range tests {
		got, ok := parseLoadavg(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseLoadavg(%q)=%v,%v want %v,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestReadHostLoadDoesNotPanic(t *testing.T) {
	// On non-Linux this must report unknown; on Linux it may be either.
	_, _ = readHostLoad()
}

func TestFromEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want Config
	}{
		{"unset", nil, Config{Name: "svc"}},
		{"all set", map[string]string{
			EnvBaseDelay: "5s", EnvJitterMax: "3m", EnvMaxLoad: "6.5", EnvMaxWait: "10m", EnvPollEvery: "2s",
		}, Config{Name: "svc", BaseDelay: 5 * time.Second, JitterMax: 3 * time.Minute, MaxLoad: 6.5, MaxWait: 10 * time.Minute, PollEvery: 2 * time.Second}},
		{"invalid falls back", map[string]string{
			EnvBaseDelay: "soon", EnvJitterMax: "10", EnvMaxLoad: "lots", EnvMaxWait: "x", EnvPollEvery: "?",
		}, Config{Name: "svc"}},
		{"negative and NaN disabled", map[string]string{
			EnvBaseDelay: "-5s", EnvJitterMax: "-1m", EnvMaxLoad: "-3", EnvMaxWait: "-1s", EnvPollEvery: "-1s",
		}, Config{Name: "svc"}},
		{"NaN load disabled", map[string]string{EnvMaxLoad: "NaN"}, Config{Name: "svc"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{EnvBaseDelay, EnvJitterMax, EnvMaxLoad, EnvMaxWait, EnvPollEvery} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := FromEnv("svc"); got != tc.want {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

func TestGoRunsFnAfterWait(t *testing.T) {
	fc := &fakeClock{}
	install(t, fc)
	ran := false
	done := goDone(context.Background(), Config{Name: "svc", BaseDelay: time.Second}, func(context.Context) { ran = true })
	<-done
	if !ran {
		t.Fatal("fn not run")
	}
	ch := make(chan struct{})
	Go(context.Background(), Config{}, func(context.Context) { close(ch) }) // public wrapper
	<-ch
}

func TestGoSkipsFnOnCancel(t *testing.T) {
	fc := &fakeClock{}
	install(t, fc)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	<-goDone(ctx, Config{Name: "svc"}, func(context.Context) { ran = true })
	if ran {
		t.Fatal("fn ran despite cancelled ctx")
	}
}
