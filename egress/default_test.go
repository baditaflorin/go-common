package egress

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// stockTransport is a transport as net/http builds the default one: the standard
// environment proxy function and nothing else.
func stockTransport() *http.Transport {
	return &http.Transport{Proxy: http.ProxyFromEnvironment}
}

func TestInstallOnReplacesTheStandardProxyFunction(t *testing.T) {
	tr := stockTransport()
	if got := installOn(tr); got != DefaultInstalled {
		t.Fatalf("installOn(stock) = %v, want installed", got)
	}
	if !sameProxyFunc(tr.Proxy, Proxy) {
		t.Fatal("the transport's proxy function is not egress.Proxy after install")
	}
	if got := installOn(tr); got != DefaultAlready {
		t.Fatalf("second installOn = %v, want already installed", got)
	}
}

// A program that chose its own proxy function made a decision; nil in
// particular is more direct than ours, and replacing it would widen the use of
// the proxy.
func TestInstallOnLeavesADeliberateChoiceAlone(t *testing.T) {
	fixed, _ := url.Parse("http://proxy.example.test:3128")
	for name, proxy := range map[string]func(*http.Request) (*url.URL, error){
		"nil (force direct)": nil,
		"fixed proxy":        http.ProxyURL(fixed),
		"wrapper around the standard function": func(r *http.Request) (*url.URL, error) {
			return http.ProxyFromEnvironment(r)
		},
	} {
		t.Run(name, func(t *testing.T) {
			tr := &http.Transport{Proxy: proxy}
			if got := installOn(tr); got != DefaultCustom {
				t.Fatalf("installOn = %v, want left alone as custom", got)
			}
			if (proxy == nil) != (tr.Proxy == nil) {
				t.Fatal("the program's proxy function was replaced")
			}
			if name == "fixed proxy" {
				req, _ := http.NewRequest(http.MethodGet, "https://fleet-graph.0exec.com/", nil)
				if got, _ := tr.Proxy(req); got == nil || got.Host != fixed.Host {
					t.Fatalf("the fixed proxy now answers %v", got)
				}
			}
		})
	}
}

func TestInstallOnIgnoresAForeignRoundTripper(t *testing.T) {
	if got := installOn(roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, nil })); got != DefaultForeign {
		t.Fatalf("installOn(foreign) = %v, want foreign", got)
	}
	var nilTransport *http.Transport
	if got := installOn(nilTransport); got != DefaultForeign {
		t.Fatalf("installOn(nil *Transport) = %v, want foreign", got)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSameProxyFunc(t *testing.T) {
	if !sameProxyFunc(Proxy, Proxy) || !sameProxyFunc(http.ProxyFromEnvironment, http.ProxyFromEnvironment) {
		t.Error("a function is not the same as itself")
	}
	if sameProxyFunc(Proxy, http.ProxyFromEnvironment) {
		t.Error("egress.Proxy and http.ProxyFromEnvironment compare equal")
	}
	if !sameProxyFunc(nil, nil) {
		t.Error("nil and nil must compare equal")
	}
	if sameProxyFunc(nil, Proxy) || sameProxyFunc(Proxy, nil) {
		t.Error("nil equals a function")
	}
}

func TestDefaultTransportEnabledSpellings(t *testing.T) {
	for v, want := range map[string]bool{
		"": true, "1": true, "true": true, "TRUE": true, " yes ": true, "on": true, "enabled": true,
		"0": false, "false": false, "No": false, "off": false, "OFF": false, "disabled": false,
		"of": false, "disable": false, "maybe": false, // unrecognised reads as off
	} {
		if got, _ := defaultTransportEnabled(v); got != want {
			t.Errorf("defaultTransportEnabled(%q) = %v, want %v", v, got, want)
		}
	}
	for _, v := range []string{"", "on", "off", "0", "1"} {
		if _, known := defaultTransportEnabled(v); !known {
			t.Errorf("%q should be a known spelling", v)
		}
	}
	if _, known := defaultTransportEnabled("of"); known {
		t.Error(`"of" should be reported as unknown so the start-up check can name the typo`)
	}
	if err := envDefaultTransport.Validate("of"); err == nil {
		t.Error("the declared variable accepts a value that is neither on nor off")
	}
	if err := envDefaultTransport.Validate("off"); err != nil {
		t.Errorf("the declared variable rejects off: %v", err)
	}
}

// restoreDefault puts the real default transport and the install state back, so
// a test that calls InstallDefaultTransport leaves nothing behind for the next.
func restoreDefault(t *testing.T) {
	t.Helper()
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	prev := tr.Proxy
	installMu.Lock()
	wasInstalled := installed.Load()
	installMu.Unlock()
	t.Cleanup(func() {
		installMu.Lock()
		defer installMu.Unlock()
		tr.Proxy = prev
		installed.Store(wasInstalled)
	})
	installMu.Lock()
	installed.Store(false)
	tr.Proxy = http.ProxyFromEnvironment
	installMu.Unlock()
}

func TestInstallDefaultTransportOnTheRealDefault(t *testing.T) {
	restoreDefault(t)
	if DefaultTransportInstalled() {
		t.Fatal("the fixture should start with the standard function")
	}
	if got := InstallDefaultTransport(); got != DefaultInstalled || !got.Active() {
		t.Fatalf("first call = %v, want installed and active", got)
	}
	if !DefaultTransportInstalled() {
		t.Fatal("DefaultTransportInstalled() is false after a successful install")
	}
	if got := InstallDefaultTransport(); got != DefaultAlready || !got.Active() {
		t.Fatalf("second call = %v, want already installed and active", got)
	}
	// A clone taken now inherits the rule; secrets.DirectTransport and Transport
	// both clone the default and set their own function afterwards.
	clone := http.DefaultTransport.(*http.Transport).Clone()
	if !sameProxyFunc(clone.Proxy, Proxy) {
		t.Error("a clone of the default transport does not carry the egress rule")
	}
}

func TestInstallDefaultTransportHonoursTheSwitch(t *testing.T) {
	for _, v := range []string{"off", "false", "0", "no", "disabled", "of"} {
		t.Run(v, func(t *testing.T) {
			restoreDefault(t)
			t.Setenv(DefaultTransportEnv, v)
			if got := InstallDefaultTransport(); got != DefaultDisabled || got.Active() {
				t.Fatalf("with %s=%q: %v, want disabled and not active", DefaultTransportEnv, v, got)
			}
			if DefaultTransportInstalled() {
				t.Fatal("the default transport was changed although the switch is off")
			}
			if !sameProxyFunc(http.DefaultTransport.(*http.Transport).Proxy, http.ProxyFromEnvironment) {
				t.Fatal("the default transport's proxy function is no longer the standard one")
			}
		})
	}
	t.Run("on again", func(t *testing.T) {
		restoreDefault(t)
		t.Setenv(DefaultTransportEnv, "on")
		if got := InstallDefaultTransport(); got != DefaultInstalled {
			t.Fatalf("with the switch on: %v, want installed", got)
		}
	})
}

// A disabled call must not be remembered: the next process (or test) with the
// switch on installs normally, and a later call with the switch still off does not
// either.
func TestDisabledCallIsNotRemembered(t *testing.T) {
	restoreDefault(t)
	t.Setenv(DefaultTransportEnv, "off")
	if got := InstallDefaultTransport(); got != DefaultDisabled {
		t.Fatalf("got %v", got)
	}
	t.Setenv(DefaultTransportEnv, "")
	if got := InstallDefaultTransport(); got != DefaultInstalled {
		t.Fatalf("after the switch is cleared: %v, want installed", got)
	}
}

func TestInstallDefaultTransportKeepsACustomProxy(t *testing.T) {
	restoreDefault(t)
	tr := http.DefaultTransport.(*http.Transport)
	fixed, _ := url.Parse("http://proxy.example.test:3128")
	tr.Proxy = http.ProxyURL(fixed)
	if got := InstallDefaultTransport(); got != DefaultCustom || got.Active() {
		t.Fatalf("got %v, want left alone", got)
	}
	if DefaultTransportInstalled() {
		t.Fatal("the program's own proxy function was replaced")
	}
	// Not remembered as installed: a later call, after the program restores the
	// standard function, still installs.
	tr.Proxy = http.ProxyFromEnvironment
	if got := InstallDefaultTransport(); got != DefaultInstalled {
		t.Fatalf("after the standard function is back: %v, want installed", got)
	}
}

func TestInstallDefaultTransportIsSafeUnderConcurrentCallers(t *testing.T) {
	restoreDefault(t)
	var wg sync.WaitGroup
	results := make(chan InstallStatus, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- InstallDefaultTransport()
		}()
	}
	wg.Wait()
	close(results)
	fresh := 0
	for r := range results {
		if !r.Active() {
			t.Fatalf("a concurrent call got %v", r)
		}
		if r == DefaultInstalled {
			fresh++
		}
	}
	if fresh != 1 {
		t.Fatalf("%d calls claimed to install the rule, want exactly 1", fresh)
	}
}

func TestInstallStatusStrings(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range []InstallStatus{DefaultInstalled, DefaultAlready, DefaultDisabled, DefaultCustom, DefaultForeign, 0} {
		str := s.String()
		if str == "" || seen[str] {
			t.Errorf("status %d has an empty or repeated description %q", s, str)
		}
		seen[str] = true
	}
	if !strings.Contains(DefaultDisabled.String(), DefaultTransportEnv) {
		t.Error("the disabled description should name the variable that did it")
	}
	if InstallStatus(0).Active() || DefaultCustom.Active() || DefaultDisabled.Active() || DefaultForeign.Active() {
		t.Error("only installed and already-installed are active")
	}
}

// The behaviour that matters, end to end: a plain client with an installed
// transport sends a request for a fleet name straight to the destination and one
// for a third-party name to the egress proxy. The dialer records where each
// connection went, so no real DNS or network is involved.
func TestInstalledTransportRoutesByDestination(t *testing.T) {
	var mu sync.Mutex
	var viaProxy, direct []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		direct = append(direct, r.Host)
		mu.Unlock()
		io.WriteString(w, "origin")
	}))
	defer origin.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		viaProxy = append(viaProxy, r.Host)
		mu.Unlock()
		io.WriteString(w, "proxy")
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)

	prev := fromEnvironment
	fromEnvironment = func(*http.Request) (*url.URL, error) { return proxyURL, nil }
	defer func() { fromEnvironment = prev }()

	tr := stockTransport()
	// Every connection goes to a loopback server whatever name was asked for, and
	// the address the transport tried to reach is what we assert on.
	var dialed []string
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		mu.Lock()
		dialed = append(dialed, addr)
		mu.Unlock()
		if addr == proxyURL.Host {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(origin.URL, "http://"))
	}
	if installOn(tr) != DefaultInstalled {
		t.Fatal("install failed")
	}
	client := &http.Client{Transport: tr}

	get := func(target string) string {
		resp, err := client.Get(target)
		if err != nil {
			t.Fatalf("GET %s: %v", target, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	for _, target := range []string{
		"http://fleet-payload-corpus.0exec.com/health", // a fleet name the stock rule sends to the proxy
		"http://go-fleet-payload-corpus:18156/health",  // a bare container name: the proxy cannot resolve it
		"http://10.70.10.30:8080/health",               // a private address
		"http://100.92.211.122:8080/health",            // a tailnet address
	} {
		if body := get(target); body != "origin" {
			t.Errorf("%s was answered by %q, want a direct connection to the origin", target, body)
		}
	}
	for _, target := range []string{"http://example.com/", "http://api.github.com/x", "http://evil0exec.com/"} {
		if body := get(target); body != "proxy" {
			t.Errorf("%s was answered by %q, want the egress proxy", target, body)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(direct) != 4 || len(viaProxy) != 3 {
		t.Fatalf("direct=%v viaProxy=%v, want 4 direct and 3 through the proxy; dialed %v", direct, viaProxy, dialed)
	}
	for _, host := range viaProxy {
		if Direct(strings.Split(host, ":")[0]) {
			t.Errorf("the proxy received a request for the fleet destination %q", host)
		}
	}
}

// The stock transport, for contrast: the same fleet name goes to the proxy. This
// is the behaviour the install exists to change, pinned so a test failure above
// cannot be a fixture that never exercised the proxy.
func TestStockTransportSendsAFleetNameToTheProxy(t *testing.T) {
	var proxied bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = true
		io.WriteString(w, "proxy")
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)

	tr := &http.Transport{Proxy: func(*http.Request) (*url.URL, error) { return proxyURL, nil }}
	resp, err := (&http.Client{Transport: tr}).Get("http://fleet-payload-corpus.0exec.com/health")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !proxied {
		t.Fatal("the fixture's proxy never saw the request")
	}
}
