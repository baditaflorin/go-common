package selftest

// live.go — opt-in "live" mode for /selftest.
//
// Background: every fleet service ships a /selftest endpoint that
// runs cheap, mostly-self-contained checks. The aggregator polls
// every service hourly, the deploy smoke gate hits it once per
// roll. Both need /selftest to be fast (sub-second) and safe to
// invoke from any caller (no side effects on real upstreams), so by
// default checks must use stubs or in-process fixtures.
//
// Live mode (/selftest?live=1) opt-in for deeper probes that hit
// real upstreams — vault round-trip with the real FleetAPIKey,
// keystore DB write+read+delete, real DNS resolve. Useful for:
//
//   * Manual debugging when /selftest passes but production is
//     visibly broken (the stub didn't exercise the actual auth path).
//   * fleet-runner deploy's smoke probe once N services adopt the
//     convention — catches "vault scope is wrong" failures within
//     30s instead of next-hour-aggregator-poll.
//
// Wire-up at the check level looks like:
//
//	checks.Check("vault", func(ctx context.Context) error {
//	    if !selftest.IsLive(ctx) {
//	        return checkVaultStub()   // in-process fixture
//	    }
//	    return checkVaultReal(ctx)    // real https://go-fleet-secrets…
//	})
//
// Render propagates live into the check ctx — checks never see the
// raw *http.Request. This keeps the CheckFunc signature stable and
// avoids passing a request into pure-functional check bodies that
// don't need anything but a context.
//
// Status semantics are unchanged: 200 if every check (stub or live)
// passes, 503 if any fail. A failing live check IS a failed
// /selftest — that's the point of having a deeper probe gated by
// the query param. The opt-in is on the *caller's* side; once they
// pass ?live=1 they get the deeper signal.

import "context"

// liveKey is the context value key for the live flag. Unexported —
// callers go through IsLive() to read it.
type liveKey struct{}

// IsLive reports whether the current /selftest run was invoked with
// ?live=1. Checks should branch on this to decide whether to hit
// real upstreams or in-process stubs.
//
// Returns false on nil contexts and on contexts that don't carry
// the flag (i.e. unit-test invocations that called Suite.run
// directly without going through Render).
func IsLive(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(liveKey{}).(bool)
	return v
}

// withLive returns a derived context flagged as live. Internal —
// Render calls this when the request carries ?live=1; callers
// reach the bit via IsLive().
func withLive(ctx context.Context, live bool) context.Context {
	if !live {
		return ctx
	}
	return context.WithValue(ctx, liveKey{}, true)
}
