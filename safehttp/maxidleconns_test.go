package safehttp

import "testing"

func TestMaxIdleConnsPerHost_Default(t *testing.T) {
	// Zero value (caller didn't set the option) must resolve to the
	// safehttp default, NOT the Go std library's restrictive 2.
	got := newBaseTransport(&options{}, nil).MaxIdleConnsPerHost
	if got != defaultMaxIdleConnsPerHost {
		t.Errorf("default MaxIdleConnsPerHost = %d, want %d", got, defaultMaxIdleConnsPerHost)
	}
	if defaultMaxIdleConnsPerHost <= 2 {
		t.Errorf("default %d should beat the std lib default of 2", defaultMaxIdleConnsPerHost)
	}
}

func TestMaxIdleConnsPerHost_Explicit(t *testing.T) {
	got := newBaseTransport(&options{maxIdleConnsPerHost: 50}, nil).MaxIdleConnsPerHost
	if got != 50 {
		t.Errorf("explicit MaxIdleConnsPerHost = %d, want 50", got)
	}
}

func TestMaxIdleConnsPerHost_NonPositiveFallsBack(t *testing.T) {
	for _, n := range []int{0, -1} {
		if got := resolveMaxIdleConnsPerHost(n); got != defaultMaxIdleConnsPerHost {
			t.Errorf("resolveMaxIdleConnsPerHost(%d) = %d, want default %d", n, got, defaultMaxIdleConnsPerHost)
		}
	}
}

func TestWithMaxIdleConnsPerHost_SetsOption(t *testing.T) {
	o := &options{}
	WithMaxIdleConnsPerHost(33)(o)
	if o.maxIdleConnsPerHost != 33 {
		t.Errorf("WithMaxIdleConnsPerHost(33) set %d", o.maxIdleConnsPerHost)
	}
}
