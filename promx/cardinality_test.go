package promx

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/fleetfetch"
	"github.com/baditaflorin/go-common/safehttp"
	"github.com/prometheus/client_golang/prometheus"
)

// crawlStream calls emit once per request of a synthetic crawler run and
// returns how many it made. The shape follows what the Metrics Hub Prometheus
// showed on 2026-10-19 for the processes at the old 256-host cap: most domains
// are fetched a handful of times (median 1-2, p90 11-15), a few hundred times
// for a deep crawl, and a few upstreams (an API, the fetch cache, the selftest
// target) are called all the time. Here: 20,000 domains fetched 1-10 times
// each in a burst, every 100th of them 100 times, and three upstreams taking
// every 10th request.
func crawlStream(emit func(host string, i int)) (requests int) {
	upstreams := []string{"api.upstream.example", "cache.upstream.example", "example.com"}
	for d := 0; d < 20000; d++ {
		host := fmt.Sprintf("d%05d.crawl.example", d)
		n := 1 + d%10
		if d%100 == 0 {
			n = 100
		}
		for k := 0; k < n; k++ {
			if requests%10 == 0 {
				emit(upstreams[(requests/10)%len(upstreams)], requests)
				requests++
			}
			emit(host, requests)
			requests++
		}
	}
	emit("", requests) // an event with no host (a blocked URL with none)
	return requests + 1
}

// seriesStats is what a registry would cost Prometheus: series per metric
// family (a histogram costs one per bucket, +Inf, _sum and _count) and the
// distinct "host" values per family.
type seriesStats struct {
	series map[string]int
	hosts  map[string]map[string]bool
	total  int
}

func gatherSeries(t *testing.T, reg *prometheus.Registry) seriesStats {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	st := seriesStats{series: map[string]int{}, hosts: map[string]map[string]bool{}}
	for _, mf := range mfs {
		name := mf.GetName()
		for _, m := range mf.GetMetric() {
			n := 1
			if h := m.GetHistogram(); h != nil {
				b := h.GetBucket()
				n = len(b) + 2 // _sum, _count
				if len(b) == 0 || !math.IsInf(b[len(b)-1].GetUpperBound(), 1) {
					n++ // the +Inf bucket the exposition adds
				}
			}
			st.series[name] += n
			st.total += n
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "host" {
					if st.hosts[name] == nil {
						st.hosts[name] = map[string]bool{}
					}
					st.hosts[name][lp.GetValue()] = true
				}
			}
		}
	}
	return st
}

func (st seriesStats) log(t *testing.T, requests int) {
	t.Helper()
	names := make([]string, 0, len(st.series))
	for n := range st.series {
		names = append(names, n)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		fmt.Fprintf(&b, "\n  %-40s %6d series, %5d host values", n, st.series[n], len(st.hosts[n]))
	}
	t.Logf("%d requests, %d series in all:%s", requests, st.total, b.String())
}

// TestEgressSeriesBound measures what one crawler process costs Prometheus
// through the default egress collectors, and pins the bound: whatever hosts it
// fetches, "host" takes at most 32 named values plus "_other" and "_unknown",
// the duration histogram has no "host", and sums over "host" stay exact.
//
// Measured with this stream (go test -run TestEgressSeriesBound -v): 0.104.1
// (first 256 hosts, host on the histogram) exported 7,960 series with up to 258
// host values per family; 0.105.0 exports 219 series. The three upstreams keep their
// names, and the slots left go to the deep-crawled domains (100 fetches each),
// none to the domains fetched 1-10 times.
func TestEgressSeriesBound(t *testing.T) {
	reg := prometheus.NewRegistry()
	coll := NewEgressCollectors(reg)
	requests := crawlStream(func(host string, i int) {
		ev := safehttp.EgressEvent{Host: host, Scheme: "https", ViaProxy: i%2 == 0, Duration: 100 * time.Millisecond, Outcome: safehttp.OutcomeSuccess, Bytes: 1000}
		if i%7 == 0 {
			ev.Outcome, ev.Bytes = safehttp.OutcomeTimeout, 0
		}
		coll.ObserveEgress(ev)
	})
	st := gatherSeries(t, reg)
	st.log(t, requests)

	for _, name := range []string{"safehttp_egress_requests_total", "safehttp_egress_response_bytes_total"} {
		hosts := st.hosts[name]
		if len(hosts) > defaultHostLimit+2 {
			t.Errorf("%s: %d host values, want at most %d", name, len(hosts), defaultHostLimit+2)
		}
		for _, h := range []string{"api.upstream.example", "cache.upstream.example", "example.com", "_other"} {
			if !hosts[h] {
				t.Errorf("%s: host %q missing; the upstreams called all the time must keep their names", name, h)
			}
		}
		for h := range hosts {
			if strings.HasPrefix(h, "d") && !strings.HasSuffix(strings.TrimSuffix(h, ".crawl.example"), "00") {
				t.Errorf("%s: shallow-crawled domain %q got a slot", name, h)
			}
		}
	}
	if hosts := st.hosts["safehttp_egress_duration_seconds"]; len(hosts) != 0 {
		t.Errorf("safehttp_egress_duration_seconds carries %d host values, want none", len(hosts))
	}
	// One duration histogram per via_proxy value: 2 x (11 default buckets + 3).
	if got, want := st.series["safehttp_egress_duration_seconds"], 2*(len(prometheus.DefBuckets)+3); got != want {
		t.Errorf("safehttp_egress_duration_seconds: %d series, want %d", got, want)
	}
	// Folding into "_other" moves requests, it never drops them.
	var sum float64
	mfs, _ := reg.Gather()
	for _, mf := range mfs {
		if mf.GetName() == "safehttp_egress_requests_total" {
			for _, m := range mf.GetMetric() {
				sum += m.GetCounter().GetValue()
			}
		}
	}
	if int(sum) != requests {
		t.Errorf("sum of safehttp_egress_requests_total = %v, want %d (every request counted once)", sum, requests)
	}
	// 34 host values x 2 via_proxy x 2 outcomes for the counter, 34 x 2 channels
	// for bytes (every request is "direct" or "proxy" here), the histograms.
	if limit := (defaultHostLimit+2)*2*2 + (defaultHostLimit+2)*2 + 2*(len(prometheus.DefBuckets)+3); st.total > limit {
		t.Errorf("%d series in all, want at most %d", st.total, limit)
	}
}

// TestFleetFetchSeriesBound is TestEgressSeriesBound for the fleetfetch
// collectors: "host" on fleet_fetch_total takes at most 34 values, and the
// cache-age histogram has no "host".
//
// Measured with this stream: 0.104.1 exported 3,256 series (fleet_fetch_cache_age_seconds
// alone 2,761, with 251 host values); 0.105.0 exports 101.
func TestFleetFetchSeriesBound(t *testing.T) {
	reg := prometheus.NewRegistry()
	coll := NewFleetFetchCollectors(reg)
	requests := crawlStream(func(host string, i int) {
		ev := fleetfetch.Event{Host: host, Result: "hit", Status: 200, AgeSeconds: 120, Duration: 5 * time.Millisecond}
		if i%3 == 0 {
			ev.Result, ev.AgeSeconds = "miss", 0
		}
		coll.ObserveFleetFetch(ev)
	})
	st := gatherSeries(t, reg)
	st.log(t, requests)

	if hosts := st.hosts["fleet_fetch_total"]; len(hosts) > defaultHostLimit+2 {
		t.Errorf("fleet_fetch_total: %d host values, want at most %d", len(hosts), defaultHostLimit+2)
	}
	if hosts := st.hosts["fleet_fetch_cache_age_seconds"]; len(hosts) != 0 {
		t.Errorf("fleet_fetch_cache_age_seconds carries %d host values, want none", len(hosts))
	}
	if got, want := st.series["fleet_fetch_cache_age_seconds"], 8+3; got != want {
		t.Errorf("fleet_fetch_cache_age_seconds: %d series, want %d (one histogram)", got, want)
	}
}
