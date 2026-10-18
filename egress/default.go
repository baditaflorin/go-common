package egress

import (
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/baditaflorin/go-common/env"
)

// DefaultTransportEnv switches InstallDefaultTransport off for one process:
// any value other than "", "1", "true", "yes", "on" or "enabled" leaves
// http.DefaultTransport as it was. It exists so that one service can go back to
// the previous behaviour with an environment change, without a release.
const DefaultTransportEnv = "FLEET_EGRESS_DEFAULT_TRANSPORT"

var (
	envDefaultTransport = env.Declare(env.Var{
		Name: DefaultTransportEnv, Kind: env.KindString, Default: "enabled",
		Doc: "Set to off, false, 0 or no to keep http.DefaultTransport on the plain environment proxy rule (NO_PROXY only), so that plain http.Clients send requests to " +
			"the fleet's own hosts through HTTP_PROXY/HTTPS_PROXY again. By default server.New makes them go direct (package egress). Anything unrecognised also turns it off.",
		Check: func(v string) error {
			if _, ok := defaultTransportEnabled(v); !ok {
				return errUnknownSwitch
			}
			return nil
		},
	})
	_ = env.Declare(env.Var{
		Name: ExtraDomainsEnv, Kind: env.KindList,
		Doc: "Comma-separated apex domains, each with at least one dot, that egress treats as the fleet's own in addition to the built-in ones: requests to them and to their subdomains never use the egress proxy.",
	})
)

type switchError string

func (e switchError) Error() string { return string(e) }

const errUnknownSwitch = switchError("not one of enabled, 1, true, yes, on, off, 0, false, no; an unrecognised value turns the egress rule on the default transport off")

// defaultTransportEnabled reads the switch. known is false for a value that is
// neither an on nor an off spelling; such a value reads as off.
func defaultTransportEnabled(v string) (enabled, known bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "1", "true", "yes", "on", "enabled":
		return true, true
	case "0", "false", "no", "off", "disabled":
		return false, true
	}
	return false, false
}

// InstallStatus says what InstallDefaultTransport did.
type InstallStatus int

const (
	// DefaultInstalled: this call put the egress rule on http.DefaultTransport.
	DefaultInstalled InstallStatus = iota + 1
	// DefaultAlready: the rule was already on http.DefaultTransport.
	DefaultAlready
	// DefaultDisabled: DefaultTransportEnv switched it off; nothing changed.
	DefaultDisabled
	// DefaultCustom: the program had set its own proxy function on
	// http.DefaultTransport (nil, a fixed proxy, a wrapper); it is left alone.
	DefaultCustom
	// DefaultForeign: http.DefaultTransport is not an *http.Transport (the
	// program replaced it); it is left alone.
	DefaultForeign
)

// Active reports whether the default transport follows the egress rule.
func (s InstallStatus) Active() bool { return s == DefaultInstalled || s == DefaultAlready }

func (s InstallStatus) String() string {
	switch s {
	case DefaultInstalled:
		return "installed"
	case DefaultAlready:
		return "already installed"
	case DefaultDisabled:
		return "disabled by " + DefaultTransportEnv
	case DefaultCustom:
		return "left alone: the program set its own proxy function on the default transport"
	case DefaultForeign:
		return "left alone: http.DefaultTransport is not an *http.Transport"
	}
	return "unknown"
}

var (
	installMu sync.Mutex
	// installed is true once the rule is on http.DefaultTransport. It lets every
	// call after the first return without reading the transport.
	installed atomic.Bool
)

// InstallDefaultTransport makes the process's plain HTTP clients follow Proxy:
// http.DefaultClient, an http.Client with no Transport, http.Get, a dependency
// probe, and a library that builds its client the standard way. They used the
// stock rule, which is NO_PROXY alone, so a request to one of our own hosts left
// through the egress proxy unless NO_PROXY happened to list that exact host. A bare
// container name reached the proxy, which cannot resolve it (502), and a public
// fleet name reached the gateway from the proxy's address, which a vhost's source
// allowlist refuses or which flaps when the proxy rotates its exits. With the rule
// installed a fleet destination is direct and everything else still follows the
// environment (HTTP_PROXY, HTTPS_PROXY, NO_PROXY), as before.
//
// It changes the field of the existing transport and does not replace it, so a
// reference taken earlier (graph.RoundTripper and telemetry.NewTransport default to
// it) sees the change too, and code that type-asserts http.DefaultTransport keeps
// working. A transport cloned before the call keeps the rule it was cloned with.
//
// It touches the proxy function only when it is still the standard one: a program
// that set its own (nil to force direct, a fixed proxy, a wrapper) keeps it,
// because replacing a deliberate choice, above all one that is more direct than
// ours, would widen the use of the proxy. safehttp does not read the default
// transport (it builds its own, with its own proxy policy) and is not affected.
//
// Call it once at start-up, before the program starts requests of its own:
// server.New does. It is idempotent and safe to call concurrently, but it writes
// a field that net/http reads without a lock, so a first call that races with a
// request in flight on the default transport is a data race. The return value
// says what happened; DefaultTransportEnv switches it off for one process.
func InstallDefaultTransport() InstallStatus {
	if installed.Load() {
		return DefaultAlready
	}
	installMu.Lock()
	defer installMu.Unlock()
	if installed.Load() {
		return DefaultAlready
	}
	if enabled, _ := defaultTransportEnabled(envDefaultTransport.Value()); !enabled {
		return DefaultDisabled
	}
	st := installOn(http.DefaultTransport)
	if st.Active() {
		installed.Store(true)
	}
	return st
}

// DefaultTransportInstalled reports whether http.DefaultTransport currently
// follows Proxy, however it got there.
func DefaultTransportInstalled() bool {
	t, ok := http.DefaultTransport.(*http.Transport)
	return ok && sameProxyFunc(t.Proxy, Proxy)
}

// installOn puts Proxy on rt when rt is an *http.Transport that still has the
// standard environment proxy function. Callers hold installMu.
func installOn(rt http.RoundTripper) InstallStatus {
	t, ok := rt.(*http.Transport)
	if !ok || t == nil {
		return DefaultForeign
	}
	switch {
	case sameProxyFunc(t.Proxy, Proxy):
		return DefaultAlready
	case sameProxyFunc(t.Proxy, http.ProxyFromEnvironment):
		t.Proxy = Proxy
		return DefaultInstalled
	}
	return DefaultCustom
}

// sameProxyFunc reports whether a and b are the same top-level function. Func
// values are not comparable with ==, and reflect's code pointer is what tells two
// references to one declared function apart from a closure or a different one.
func sameProxyFunc(a, b func(*http.Request) (*url.URL, error)) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}
