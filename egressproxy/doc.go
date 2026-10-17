// Package egressproxy is a credential-injecting forward proxy that listens on
// loopback only, so a browser that cannot authenticate to an upstream proxy
// can still be forced through one.
//
// # Why this exists
//
// Chromium cannot log in to an upstream proxy from a URL or a flag
// (--proxy-server ignores credentials, and a 407 is unanswerable in headless
// mode). The fleet's egress proxy (Webshare, picked per request by
// proxysupplier) needs a Proxy-Authorization header. This package sits in
// between: the browser is started with --proxy-server pointing at the local
// listener, the listener asks a proxysupplier.Supplier for an upstream, adds
// the header and relays. Every browser isolation mode (shared tab, isolated
// context, process per render) inherits it because the proxy is a
// browser-level flag.
//
// # What it serves
//
//   - CONNECT host:port: a tunnel. The proxy sends CONNECT to the upstream with
//     Proxy-Authorization, relays success as 200 and splices the two sockets.
//   - Absolute-URI plain HTTP (GET http://host/path): forwarded to the upstream
//     with Proxy-Authorization; hop-by-hop and Proxy-* headers from the browser
//     are removed. WebSocket upgrades over plain HTTP are spliced after the
//     upstream's 101.
//
// # Fail closed
//
// There is no direct path to a target. The only direct dial is to the upstream
// proxy itself. When the supplier returns "" (nothing configured, every exit
// IP cooling down) or an address that loops back to this listener, the request
// is answered 503 and the target is never contacted. Config.RequireUpstream
// only makes Start refuse a nil Supplier; it does not open a direct path.
// Destinations that must be reached without the proxy (fleet-internal hosts,
// see egress.Direct) belong in the browser's --proxy-bypass-list, not here.
//
// # SSRF guard
//
// Before the upstream is dialed the target host goes through Config.Guard
// (DefaultGuard: safehttp.GuardHost, so loopback, private, link-local and
// metadata addresses, hostnames resolving to them, and the compliance denylist
// are answered 403, honouring SAFEHTTP_ALLOW_PRIVATE_IPS and
// SAFEHTTP_ALLOW_PRIVATE_HOSTS). Verdicts are cached per host:port for 30 s in
// a bounded cache. The upstream resolves the target name itself, so the guard
// cannot close a DNS-rebinding gap; it is defence in depth against pages that
// steer the browser at internal address literals.
//
// # Retry and result feedback
//
// A CONNECT is attempted on at most two upstreams inside DialTimeout, retrying
// only while nothing has been relayed to the browser. If the supplier
// implements proxysupplier.ResultReporter the proxy calls MarkResult(addr,
// false) on a dial or handshake failure, a 407 and a 5xx answer to CONNECT,
// and MarkResult(addr, true) on an established tunnel. A plain HTTP request is
// retried only when it is safe to repeat (a dial failure, or a body-less
// GET/HEAD/OPTIONS). Set Config.IgnoreUpstream5xx when the upstream answers
// 5xx for a dead target rather than a bad exit IP, so good IPs are not put in
// cooldown.
//
// # Limits
//
// MaxConns (256) concurrent client connections, the excess answered 503 at
// once; DialTimeout (10 s); IdleTimeout (60 s, nothing moved in either
// direction); ReadHeaderTimeout (15 s); MaxTunnelBytes (64 MiB per direction of
// one tunnel, 0 = unlimited). ConfigFromEnv reads EGRESSPROXY_* overrides.
//
// # Metrics
//
// Registered on Config.Registerer (default promx.Registry(), so call it after
// promx.Init / telemetry.Init): egressproxy_requests_total{kind,result},
// egressproxy_bytes_total{direction}, egressproxy_active_tunnels,
// egressproxy_upstream_attempts_total{result} and
// egressproxy_request_duration_seconds{kind}. Labels are fixed strings: no
// host, no upstream address, no credential.
//
// # Logging
//
// Logs carry the upstream's host:port, its scheme and a result, never the
// credentialed URL, and are rate limited per cause.
//
// # Usage
//
//	cfg := egressproxy.ConfigFromEnv()
//	cfg.Supplier = proxysupplier.New() // PROXY_SUPPLIER=webshare_direct|env|multi
//	cfg.Registerer = promx.Registry()
//	p, err := egressproxy.Start(ctx, cfg)
//	if err != nil {
//	    return err
//	}
//	defer p.Close()
//
//	opts = append(opts,
//	    chromedp.Flag("proxy-server", p.URL()),
//	    // Send loopback through the proxy too, so the guard refuses it.
//	    chromedp.Flag("proxy-bypass-list", "<-loopback>"),
//	    // Chromium's own background traffic (component updater, time sync,
//	    // Google endpoints) also goes through the proxy: switch it off so it
//	    // does not spend upstream bandwidth or put exit IPs in cooldown.
//	    chromedp.Flag("disable-background-networking", true),
//	    chromedp.Flag("disable-component-update", true),
//	    chromedp.Flag("disable-sync", true),
//	)
//
// To keep one exit IP per site (a proxysupplier.KeyedSupplier such as
// webshare_direct), give the proxy a key:
//
//	cfg.KeyFunc = func(t egressproxy.Target) string { return t.Host }
package egressproxy
