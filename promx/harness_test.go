package promx

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/apikey"
	"github.com/baditaflorin/go-common/backoffcoord"
	"github.com/baditaflorin/go-common/cgroup"
	"github.com/baditaflorin/go-common/circuitbreaker"
	"github.com/baditaflorin/go-common/credfile"
	"github.com/baditaflorin/go-common/degraded"
	"github.com/baditaflorin/go-common/depcheck"
	"github.com/baditaflorin/go-common/fleetfetch"
	"github.com/baditaflorin/go-common/loadshed"
	"github.com/baditaflorin/go-common/middleware"
	"github.com/baditaflorin/go-common/policyeval"
	"github.com/baditaflorin/go-common/ratecoord"
	"github.com/baditaflorin/go-common/response"
	"github.com/baditaflorin/go-common/safehttp"
	"github.com/baditaflorin/go-common/selftest"
	"github.com/baditaflorin/go-common/testhelpers/promxtest"
	"github.com/baditaflorin/go-common/workpool"
	"github.com/prometheus/client_golang/prometheus"
)

// configuredNames is how many names the probes declare for a label that names
// something a service creates once at start-up (a pool, a gate, a dependency, a
// check, a ruleset): such a label is configuration, not input, and its series count
// is the number of names declared. The series budgets in docs/observability.md
// assume this many.
const configuredNames = 16

// harnessCase is one collector set with the hostile probes that attack it.
type harnessCase struct {
	name    string
	ctors   []string // the constructors it covers, see TestEveryCollectorHasAHarnessCase
	metrics []string // the metric families the set registers
	// bounded runs promxtest.AssertBounded with a budget and then check.
	bounded func(t testing.TB, maxSeries int)
	// into registers the set on reg and drives its probes, to measure every set in one registry.
	into func(t testing.TB, reg prometheus.Registerer, in *promxtest.Inputs)
}

func newCase[C any](name string, ctors, metrics []string, register func(prometheus.Registerer) C,
	check func(t testing.TB, r promxtest.Report), probes ...promxtest.Probe[C]) harnessCase {
	return harnessCase{
		name: name, ctors: ctors, metrics: metrics,
		bounded: func(t testing.TB, maxSeries int) {
			t.Helper()
			rep := promxtest.AssertBounded(t, register, probes, maxSeries)
			if check != nil {
				check(t, rep)
			}
		},
		into: func(t testing.TB, reg prometheus.Registerer, in *promxtest.Inputs) {
			t.Helper()
			promxtest.Run(t, reg, register, probes, in)
		},
	}
}

func probe[C any](name string, drive func(c C, in *promxtest.Inputs)) promxtest.Probe[C] {
	return promxtest.Probe[C]{Name: name, Drive: drive}
}

func family(t testing.TB, r promxtest.Report, name string) promxtest.Metric {
	t.Helper()
	m, ok := r.Metric(name)
	if !ok {
		t.Fatalf("no series for %s: the probes never reached it", name)
	}
	return m
}

// wantValues fails unless label of metric took exactly want distinct values: the
// bound the collector claims, reached (so the probes saturate it) and not passed.
func wantValues(t testing.TB, r promxtest.Report, metric, label string, want int) {
	t.Helper()
	if got := family(t, r, metric).Distinct(label); got != want {
		t.Errorf("%s: label %q took %d distinct values, want %d", metric, label, got, want)
	}
}

func names(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%d", prefix, i)
	}
	return out
}

func harnessCases() []harnessCase {
	return []harnessCase{
		httpCase(), egressCase(), fleetFetchCase(), backoffCase(), backoffCoordCase(), rateCoordCase(),
		degradedCase(), authCase(), adminCase(), selftestCase(), depCase(), policyCase(), envelopeCase(),
		circuitCase(), workpoolCase(), loadshedCase(), webshareCase(), cgroupCase(), graphCase(), credfileCase(), buildInfoCase(),
	}
}

// ---- request-derived labels: hostile probes ----

type httpHarness struct {
	h      http.Handler
	status int
}

func newHTTPHarness(reg prometheus.Registerer) *httpHarness {
	hh := &httpHarness{}
	hh.h = NewHTTPCollectors(reg).Middleware()(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(hh.status)
		_, _ = w.Write([]byte("body"))
	}))
	return hh
}

// serve hands the middleware a request the way net/http's server does: the method
// token and the percent-decoded path of the request line.
func (hh *httpHarness) serve(method, path string, status int, ua string) {
	hh.status = status
	req := &http.Request{Method: method, URL: &url.URL{Path: path}, Header: http.Header{}}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	hh.h.ServeHTTP(httptest.NewRecorder(), req)
}

func httpCase() harnessCase {
	return newCase("http", []string{"NewHTTPCollectors"},
		[]string{"http_requests_total", "http_request_duration_seconds", "http_response_size_bytes", "http_requests_in_flight"},
		newHTTPHarness,
		func(t testing.TB, r promxtest.Report) {
			t.Helper()
			statuses := map[string]bool{}
			for code := 100; code <= 999; code++ {
				statuses[statusLabel(code)] = true
			}
			wantValues(t, r, "http_requests_total", "method", 10)   // nine standard, OTHER
			wantValues(t, r, "http_requests_total", "route", 512+2) // the limit, _other, _unknown
			wantValues(t, r, "http_requests_total", "status", len(statuses))
		},
		probe("paths", func(c *httpHarness, in *promxtest.Inputs) {
			in.Each(in.Paths(), func(p string) { c.serve("GET", p, 200, "") })
		}),
		probe("methods", func(c *httpHarness, in *promxtest.Inputs) {
			in.Each(in.Methods(), func(m string) { c.serve(m, "/x", 200, "") })
		}),
		probe("statuses", func(c *httpHarness, in *promxtest.Inputs) {
			for range 3 {
				for _, s := range in.Statuses() {
					c.serve("GET", "/x", s, "")
				}
			}
		}),
		probe("user agents", func(c *httpHarness, in *promxtest.Inputs) {
			for _, ua := range in.UserAgents() {
				c.serve("GET", "/x", 200, ua)
			}
		}),
		probe("all at once", func(c *httpHarness, in *promxtest.Inputs) {
			// The same 2000 requests at every scale: only the label values may grow with it.
			paths, methods, statuses := in.Paths()[:2000], in.Methods()[:2000], in.Statuses()
			for i, p := range paths {
				c.serve(methods[i*7%len(methods)], p, statuses[i*13%len(statuses)], "")
			}
		}),
	)
}

var egressOutcomes = []safehttp.EgressOutcome{
	safehttp.OutcomeSuccess, safehttp.OutcomeRedirect, safehttp.OutcomeClientError, safehttp.OutcomeServerError,
	safehttp.OutcomeBlocked, safehttp.OutcomeDNSFail, safehttp.OutcomeTimeout, safehttp.OutcomeTLSFail, safehttp.OutcomeNetError,
}

func egressEvent(host, scheme string) safehttp.EgressEvent {
	return safehttp.EgressEvent{Host: host, Scheme: scheme, Status: 200, Duration: time.Millisecond, Bytes: 10, Channel: "direct", Outcome: safehttp.OutcomeSuccess}
}

func egressCase() harnessCase {
	return newCase("egress", []string{"NewEgressCollectors"},
		[]string{"safehttp_egress_requests_total", "safehttp_egress_duration_seconds", "safehttp_egress_response_bytes_total", "safehttp_egress_blocked_total"},
		func(reg prometheus.Registerer) *EgressCollectors { return NewEgressCollectors(reg) },
		func(t testing.TB, r promxtest.Report) {
			t.Helper()
			for _, m := range []string{"safehttp_egress_requests_total", "safehttp_egress_response_bytes_total"} {
				wantValues(t, r, m, "host", defaultHostLimit+2)
			}
			wantValues(t, r, "safehttp_egress_requests_total", "scheme", 4) // http, https, _other, _unknown
			wantValues(t, r, "safehttp_egress_requests_total", "outcome", len(egressOutcomes))
			wantValues(t, r, "safehttp_egress_response_bytes_total", "channel", 3)
			wantValues(t, r, "safehttp_egress_blocked_total", "reason", 6)
		},
		probe("hosts", func(c *EgressCollectors, in *promxtest.Inputs) {
			in.Each(in.Hosts(), func(h string) { c.ObserveEgress(egressEvent(h, "https")) })
		}),
		probe("schemes", func(c *EgressCollectors, in *promxtest.Inputs) {
			in.Each(in.Schemes(), func(s string) { c.ObserveEgress(egressEvent("api.example", s)) })
		}),
		probe("outcomes, proxy and channels", func(c *EgressCollectors, _ *promxtest.Inputs) {
			for _, o := range egressOutcomes {
				for _, proxy := range []bool{false, true} {
					for _, ch := range []string{"direct", "proxy", "cache_hit", ""} {
						ev := egressEvent("api.example", "https")
						ev.Outcome, ev.ViaProxy, ev.Channel = o, proxy, ch
						c.ObserveEgress(ev)
					}
				}
			}
		}),
		probe("blocked reasons", func(c *EgressCollectors, in *promxtest.Inputs) {
			errs := []error{safehttp.ErrBlocked, safehttp.ErrInvalidScheme, safehttp.ErrMissingHost, errors.New("dial: blocked port 25"), errors.New("else"), nil}
			for _, n := range in.Names()[:300] {
				errs = append(errs, errors.New(n))
			}
			for _, err := range errs {
				ev := egressEvent("api.example", "https")
				ev.Outcome, ev.Err = safehttp.OutcomeBlocked, err
				c.ObserveEgress(ev)
			}
		}),
	)
}

var fleetFetchResults = []string{"hit", "miss", "fallback", "timeout", "busy", "error", "direct", "direct_error"}

func fleetFetchCase() harnessCase {
	return newCase("fleetfetch", []string{"NewFleetFetchCollectors"},
		[]string{"fleet_fetch_total", "fleet_fetch_duration_seconds", "fleet_fetch_cache_age_seconds"},
		func(reg prometheus.Registerer) *FleetFetchCollectors { return NewFleetFetchCollectors(reg) },
		func(t testing.TB, r promxtest.Report) {
			t.Helper()
			wantValues(t, r, "fleet_fetch_total", "host", defaultHostLimit+2)
			wantValues(t, r, "fleet_fetch_total", "result", len(fleetFetchResults))
		},
		probe("hosts", func(c *FleetFetchCollectors, in *promxtest.Inputs) {
			in.Each(in.Hosts(), func(h string) {
				c.ObserveFleetFetch(fleetfetch.Event{Host: h, Result: "hit", Status: 200, AgeSeconds: 120, Duration: time.Millisecond})
			})
		}),
		probe("results", func(c *FleetFetchCollectors, _ *promxtest.Inputs) {
			for _, r := range fleetFetchResults {
				c.ObserveFleetFetch(fleetfetch.Event{Host: "api.example", Result: r, Status: 200, AgeSeconds: 5, Duration: time.Millisecond})
			}
		}),
	)
}

func backoffCase() harnessCase {
	outcomes := []string{"consulted_no_wait", "consulted_waited", "unreachable"}
	return newCase("backoff", []string{"NewBackoffCollectors"},
		[]string{"safehttp_backoff_consults_total", "safehttp_backoff_consult_latency_seconds", "safehttp_backoff_waited_seconds"},
		func(reg prometheus.Registerer) *BackoffCollectors { return NewBackoffCollectors(reg) },
		func(t testing.TB, r promxtest.Report) {
			t.Helper()
			wantValues(t, r, "safehttp_backoff_consults_total", "host", 256+2)
			wantValues(t, r, "safehttp_backoff_consults_total", "outcome", len(outcomes))
		},
		probe("hosts", func(c *BackoffCollectors, in *promxtest.Inputs) {
			in.Each(in.Hosts(), func(h string) {
				c.ObserveBackoff(safehttp.BackoffEvent{Host: h, Outcome: "consulted_waited", ConsultLatency: time.Millisecond, Waited: time.Second})
			})
		}),
		probe("outcomes", func(c *BackoffCollectors, _ *promxtest.Inputs) {
			for _, o := range outcomes {
				c.ObserveBackoff(safehttp.BackoffEvent{Host: "api.example", Outcome: o, ConsultLatency: time.Millisecond})
			}
		}),
	)
}

func backoffCoordCase() harnessCase {
	outcomes := []string{"no_wait", "waited", "unreachable"}
	return newCase("backoffcoord", []string{"NewBackoffCoordCollectors"},
		[]string{"backoffcoord_consults_total", "backoffcoord_consult_latency_seconds", "backoffcoord_wait_seconds"},
		func(reg prometheus.Registerer) *BackoffCoordCollectors { return NewBackoffCoordCollectors(reg) },
		func(t testing.TB, r promxtest.Report) {
			t.Helper()
			wantValues(t, r, "backoffcoord_consults_total", "host", 256+2)
			wantValues(t, r, "backoffcoord_consults_total", "outcome", len(outcomes))
		},
		probe("hosts", func(c *BackoffCoordCollectors, in *promxtest.Inputs) {
			in.Each(in.Hosts(), func(h string) {
				c.ObserveBackoffCoord(backoffcoord.Event{Host: h, Outcome: "waited", ConsultLatency: time.Millisecond, Waited: time.Second})
			})
		}),
		probe("outcomes", func(c *BackoffCoordCollectors, _ *promxtest.Inputs) {
			for _, o := range outcomes {
				c.ObserveBackoffCoord(backoffcoord.Event{Host: "api.example", Outcome: o, ConsultLatency: time.Millisecond})
			}
		}),
	)
}

func rateCoordCase() harnessCase {
	return newCase("ratecoord", []string{"NewRateCoordCollectors"},
		[]string{"ratecoord_decisions_total", "ratecoord_fallback_total", "ratecoord_wait_seconds"},
		func(reg prometheus.Registerer) *RateCoordCollectors { return NewRateCoordCollectors(reg) },
		func(t testing.TB, r promxtest.Report) {
			t.Helper()
			wantValues(t, r, "ratecoord_decisions_total", "host", 256+2)
			wantValues(t, r, "ratecoord_decisions_total", "outcome", 3)
		},
		probe("hosts", func(c *RateCoordCollectors, in *promxtest.Inputs) {
			in.Each(in.Hosts(), func(h string) { c.ObserveRate(ratecoord.Event{Host: h, Allowed: true, Waited: time.Millisecond}) })
		}),
		probe("outcomes", func(c *RateCoordCollectors, _ *promxtest.Inputs) {
			for _, fellBack := range []bool{false, true} {
				for _, allowed := range []bool{false, true} {
					c.ObserveRate(ratecoord.Event{Host: "api.example", Allowed: allowed, FellBack: fellBack, Waited: time.Millisecond})
				}
			}
		}),
	)
}

func degradedCase() harnessCase {
	return newCase("degraded", []string{"NewDegradedCollectors"}, []string{"fleet_degraded_total"},
		func(reg prometheus.Registerer) *DegradedCollectors { return NewDegradedCollectors(reg) },
		func(t testing.TB, r promxtest.Report) {
			t.Helper()
			wantValues(t, r, "fleet_degraded_total", "primitive", degradedPrimitiveLimit+2)
			wantValues(t, r, "fleet_degraded_total", "suffix", degradedSuffixLimit+1) // _other; no _unknown: an empty suffix is "_none"
		},
		probe("conventional suffixes", func(c *DegradedCollectors, _ *promxtest.Inputs) {
			for _, s := range []string{"down", "degraded", "timeout", "rate_limited", ""} {
				c.ObserveDegraded(degraded.Event{Token: "html-proxy-" + s, Primitive: "html-proxy", Suffix: s})
			}
		}),
		probe("primitives", func(c *DegradedCollectors, in *promxtest.Inputs) {
			in.Each(in.Names(), func(n string) { c.ObserveDegraded(degraded.Event{Token: "t-down", Primitive: n, Suffix: "down"}) })
		}),
		probe("suffixes", func(c *DegradedCollectors, in *promxtest.Inputs) {
			in.Each(in.Names(), func(n string) { c.ObserveDegraded(degraded.Event{Token: "t-" + "x", Primitive: "p", Suffix: n}) })
		}),
	)
}

// ---- labels from closed lists and from names declared at start-up: every value used ----

func authCase() harnessCase {
	sources := []middleware.AuthSource{middleware.AuthSourceBypass, middleware.AuthSourceGateway, middleware.AuthSourceLocal,
		middleware.AuthSourceKeystore, middleware.AuthSourceMissing, middleware.AuthSourcePrivateMesh}
	results := []middleware.AuthResult{middleware.AuthResultAllow, middleware.AuthResultDeny, middleware.AuthResultUnavailable,
		middleware.AuthResultTierDenied, middleware.AuthResultTierShadowDenied}
	cache := []apikey.CacheResult{apikey.CacheResultFresh, apikey.CacheResultStale, apikey.CacheResultInnerOK,
		apikey.CacheResultInnerInvalid, apikey.CacheResultInnerUnavailable}
	return newCase("auth", []string{"NewAuthCollectors"},
		[]string{"apikey_auth_total", "apikey_keystore_call_duration_seconds", "apikey_cache_total", "apikey_cache_stale_age_seconds", "apikey_keystore_inner_duration_seconds"},
		func(reg prometheus.Registerer) *AuthCollectors { return NewAuthCollectors(reg) }, nil,
		probe("every source and result", func(c *AuthCollectors, _ *promxtest.Inputs) {
			for _, s := range sources {
				for _, r := range results {
					c.ObserveAuth(middleware.AuthEvent{Source: s, Result: r, Duration: time.Millisecond})
				}
			}
			for _, r := range cache {
				c.ObserveCache(apikey.CacheEvent{Result: r, Age: time.Second, Duration: time.Millisecond})
			}
		}),
	)
}

func adminCase() harnessCase {
	return newCase("admin", []string{"NewAdminCollectors"}, []string{"apikey_admin_total", "apikey_admin_duration_seconds"},
		func(reg prometheus.Registerer) *AdminCollectors { return NewAdminCollectors(reg) }, nil,
		probe("every op and result", func(c *AdminCollectors, _ *promxtest.Inputs) {
			for _, op := range []string{"issue", "revoke", "list", "purge", "_other"} {
				for _, r := range []string{"ok", "unauthorized", "unavailable", "client_error", "transport_error"} {
					c.ObserveAdmin(apikey.AdminEvent{Op: op, Result: r, Duration: time.Millisecond})
				}
			}
		}),
	)
}

func selftestCase() harnessCase {
	return newCase("selftest", []string{"NewSelftestCollectors"},
		[]string{"fleet_selftest_check_pass", "fleet_selftest_check_runs_total", "fleet_selftest_runs_total", "fleet_selftest_run_duration_seconds"},
		func(reg prometheus.Registerer) *SelftestCollectors { return NewSelftestCollectors(reg) }, nil,
		probe("configured checks", func(c *SelftestCollectors, _ *promxtest.Inputs) {
			for _, ok := range []bool{true, false} {
				ev := selftest.Event{Service: "svc", OK: ok, Duration: time.Second}
				for _, n := range names("check", configuredNames) {
					ev.Checks = append(ev.Checks, selftest.CheckResult{Name: n, Pass: ok})
				}
				c.ObserveSelftest(ev)
			}
		}),
	)
}

func depCase() harnessCase {
	return newCase("depcheck", []string{"NewDepCollectors"},
		[]string{"fleet_dep_probe_ok", "fleet_dep_probe_runs_total", "fleet_dep_probe_latency_seconds"},
		func(reg prometheus.Registerer) *DepCollectors { return NewDepCollectors(reg) }, nil,
		probe("configured deps", func(c *DepCollectors, _ *promxtest.Inputs) {
			for _, n := range names("dep", configuredNames) {
				for _, ok := range []bool{true, false} {
					c.ObserveDep(depcheck.Event{Dep: n, OK: ok, Latency: time.Millisecond})
				}
			}
		}),
	)
}

func policyCase() harnessCase {
	return newCase("policyeval", []string{"NewPolicyCollectors"},
		[]string{"policyeval_evaluations_total", "policyeval_rule_fires_total", "policyeval_errors_total"},
		func(reg prometheus.Registerer) *PolicyCollectors { return NewPolicyCollectors(reg) }, nil,
		probe("configured rulesets and rules", func(c *PolicyCollectors, _ *promxtest.Inputs) {
			rules := names("rule", configuredNames)
			for _, rs := range append(names("ruleset", configuredNames), "") {
				c.ObservePolicy(policyeval.Event{Ruleset: rs, Matched: rules, Winner: rules[0]})
				c.ObservePolicy(policyeval.Event{Ruleset: rs})
				c.ObservePolicy(policyeval.Event{Ruleset: rs, Err: errors.New("bad regex")})
			}
		}),
	)
}

func envelopeCase() harnessCase {
	return newCase("envelope", []string{"NewEnvelopeCollectors"}, []string{"fleet_envelope_emitted_total", "fleet_envelope_warnings_total"},
		func(reg prometheus.Registerer) *EnvelopeCollectors { return NewEnvelopeCollectors(reg) }, nil,
		probe("schema versions and warnings", func(c *EnvelopeCollectors, _ *promxtest.Inputs) {
			for v := 0; v < 8; v++ {
				for _, w := range []string{"", "conflict__service", "conflict__schema_version", "conflict__emitted_at", "marshal_failed"} {
					c.ObserveEnvelope(response.Event{Service: "svc", SchemaVersion: v, Warning: w})
				}
			}
		}),
	)
}

func circuitCase() harnessCase {
	states := []circuitbreaker.State{circuitbreaker.StateClosed, circuitbreaker.StateOpen, circuitbreaker.StateHalfOpen}
	reasons := []string{"probe_in_flight", "open_window_elapsed", "probe_ok", "threshold_reached", "probe_failed"}
	return newCase("circuitbreaker", []string{"NewCircuitCollectors"},
		[]string{"fleet_circuit_state", "fleet_circuit_transitions_total", "fleet_circuit_trips_total"},
		func(reg prometheus.Registerer) *CircuitCollectors { return NewCircuitCollectors(reg) }, nil,
		probe("configured upstreams, every transition", func(c *CircuitCollectors, _ *promxtest.Inputs) {
			for _, u := range names("upstream", configuredNames) {
				for _, from := range states {
					for _, to := range states {
						for _, r := range reasons {
							c.ObserveCircuit(circuitbreaker.Event{Upstream: u, From: from, To: to, Reason: r})
						}
					}
				}
			}
		}),
	)
}

func workpoolCase() harnessCase {
	phases := []workpool.Phase{workpool.PhaseQueued, workpool.PhaseStarted, workpool.PhaseFinished, workpool.PhaseCanceled, workpool.PhaseShed}
	return newCase("workpool", []string{"NewWorkpoolCollectors"}, []string{"pool_inflight", "pool_queue_depth", "pool_phase_total"},
		func(reg prometheus.Registerer) *WorkpoolCollectors { return NewWorkpoolCollectors(reg) }, nil,
		probe("configured pools", func(c *WorkpoolCollectors, _ *promxtest.Inputs) {
			for _, p := range names("pool", configuredNames) {
				for _, ph := range phases {
					c.ObserveWorkpool(workpool.Event{Pool: p, Phase: ph, InFlight: 1, QueueDepth: 1})
				}
			}
		}),
	)
}

func loadshedCase() harnessCase {
	phases := []loadshed.Phase{loadshed.PhaseAdmitted, loadshed.PhaseReleased, loadshed.PhaseShed}
	return newCase("loadshed", []string{"NewLoadshedCollectors"}, []string{"loadshed_in_flight", "loadshed_admitted_total", "loadshed_shed_total"},
		func(reg prometheus.Registerer) *LoadshedCollectors { return NewLoadshedCollectors(reg) }, nil,
		probe("configured gates", func(c *LoadshedCollectors, _ *promxtest.Inputs) {
			for _, g := range names("gate", configuredNames) {
				for _, ph := range phases {
					c.ObserveLoadshed(loadshed.Event{Gate: g, Phase: ph, Limit: 4, InFlight: 1})
				}
			}
		}),
	)
}

// ---- scrape-time collectors and constants: nothing to drive ----

func webshareCase() harnessCase {
	return newCase("webshare_direct", []string{"NewWebshareDirectCollectors"},
		[]string{
			"proxysupplier_webshare_direct_pool_size", "proxysupplier_webshare_direct_pool_in_cooldown", "proxysupplier_webshare_direct_pool_eligible",
			"proxysupplier_webshare_direct_refresh_total", "proxysupplier_webshare_direct_refresh_consecutive_failures", "proxysupplier_webshare_direct_last_refresh_age_seconds",
		},
		func(reg prometheus.Registerer) *WebshareDirectCollectors { return NewWebshareDirectCollectors(reg) }, nil)
}

func fullCgroupSnapshot() (cgroup.Snapshot, error) {
	p := &cgroup.Pressure{Avg10Ratio: f64(0.1), Avg60Ratio: f64(0.1), Avg300Ratio: f64(0.1), TotalSeconds: f64(1)}
	return cgroup.Snapshot{
		MemoryUsageBytes: u64(1), MemoryLimitBytes: u64(2), CPUUsageSeconds: f64(1), CPUQuotaCores: f64(1),
		CPUThrottledPeriods: u64(1), CPUThrottledSeconds: f64(1), CPUPressureSome: p, MemoryPressureSome: p, MemoryPressureFull: p,
	}, nil
}

func cgroupCase() harnessCase {
	var metrics []string
	for name := range newCgroupCollectors("svc", fullCgroupSnapshot).desc {
		metrics = append(metrics, "go_container_"+name)
	}
	return newCase("cgroup", []string{"NewCgroupCollectors"}, metrics,
		func(reg prometheus.Registerer) *CgroupCollectors {
			c := newCgroupCollectors("svc", fullCgroupSnapshot)
			reg.MustRegister(c)
			return c
		}, nil)
}

// credfileHarness feeds the collector a snapshot the probes build, instead of
// the process-wide credfile registry.
type credfileHarness struct {
	mu    sync.Mutex
	stats []credfile.Stats
}

func (h *credfileHarness) snapshot() []credfile.Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]credfile.Stats(nil), h.stats...)
}

func (h *credfileHarness) add(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	errs := map[string]uint64{}
	for _, r := range credfileReasons {
		errs[r] = 1
	}
	h.stats = append(h.stats, credfile.Stats{Name: name, Expiry: time.Unix(1800000000, 0), ReloadOK: 1, ReloadError: 1, StaleUses: 1, Errors: errs})
}

func credfileCase() harnessCase {
	return newCase("credfile", []string{"NewCredfileCollectors"},
		[]string{"fleet_credential_expiry_timestamp_seconds", "fleet_credential_reload_total", "fleet_credential_reload_errors_total", "fleet_credential_stale_use_total"},
		func(reg prometheus.Registerer) *credfileHarness {
			h := &credfileHarness{}
			reg.MustRegister(newCredfileCollectors("svc", h.snapshot))
			return h
		}, nil,
		probe("configured credentials", func(h *credfileHarness, _ *promxtest.Inputs) {
			for _, n := range names("credential", configuredNames) {
				h.add(n)
			}
		}),
		// Names are code, not input, but a credential opened with a name built
		// from data must still not grow the series: everything past the first
		// 16 names, and anything that is not a plain name, is "_other".
		probe("hostile names", func(h *credfileHarness, in *promxtest.Inputs) {
			in.Each(in.Names(), h.add)
		}),
	)
}

func graphCase() harnessCase {
	return newCase("graph", []string{"NewGraphCollectors"},
		[]string{
			"fleet_graph_events_recorded_total", "fleet_graph_events_dropped_total", "fleet_graph_batches_sent_total", "fleet_graph_batches_failed_total",
			"fleet_graph_batches_rejected_total", "fleet_graph_events_dropped_invalid_identity_total",
			"fleet_graph_batches_sent_spiffe_total", "fleet_graph_batches_spiffe_fallback_total",
		},
		func(reg prometheus.Registerer) *GraphCollectors { return NewGraphCollectors(reg, "svc") }, nil)
}

func buildInfoCase() harnessCase {
	return newCase("build_info", []string{"registerBuildInfo"}, []string{"build_info"},
		func(reg prometheus.Registerer) struct{} { registerBuildInfo(reg, "svc", "v1.2.3"); return struct{}{} }, nil)
}
