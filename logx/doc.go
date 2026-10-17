// Package logx is the fleet's structured logger: slog JSON on stdout, with the
// service, version and node on every line, the trace id when the call is inside
// a traced request, and credentials redacted before they are written.
//
//	logx.Install(logx.Options{Service: "go_example", Version: "1.2.3"})
//	slog.InfoContext(ctx, "charged", "account", id)   // {"service":"go_example", "trace_id":"…", …}
//	log.Printf("legacy line %d", n)                   // the same JSON shape, level INFO
//
// # Why one call is enough
//
// slog.SetDefault, which Install calls, also redirects the standard library's log
// package to the new handler. Code written with log.Printf (this repository has
// dozens of such calls, and so do the services) therefore becomes structured
// without being edited. Those records have no context, so they carry the service
// fields but no trace id; use slog.InfoContext or middleware.LoggerFromContext
// where the trace matters.
//
// # What it redacts
//
//   - an attribute whose key looks like a credential (token, secret, password,
//     authorization, api_key, cookie, credential) and whose value is a string;
//     numbers are left alone, so "tokens": 52 stays;
//   - in any string value, including the message: "Bearer <token>", the user and
//     password of a URL (scheme://user:pass@host), and token=, password=, secret=
//     or api_key= in a query string or key=value text.
//
// Redaction is a net under the careless log line, not permission to log secrets.
//
// # Adopting it
//
// It is opt-in per service: server.New installs it when LOG_FORMAT=json (or the
// service passes server.WithStructuredLogs). The default output is unchanged,
// because moving every service to JSON at once would break anything that greps
// or alerts on the old text.
package logx
