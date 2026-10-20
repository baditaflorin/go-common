package graph

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/baditaflorin/go-common/env"
)

// The environment variables graph reads, declared in the env registry. loadConfig
// and the helpers below still do the parsing (graph is lenient on purpose: a bad
// value falls back rather than failing a service); the declarations make the
// variables listed, documented and checked at start-up, so a mistyped one is
// reported instead of silently ignored.
var (
	envEnabled = env.Declare(env.Var{
		Name: "GRAPH_ENABLED", Kind: env.KindBool, Default: "false",
		Doc: "Opt-in switch for emitting service-to-service call events. Emission also needs GRAPH_COLLECTOR_URL and a writer key.",
	})
	envCollectorURL = env.Declare(env.Var{
		Name: "GRAPH_COLLECTOR_URL", Kind: env.KindString,
		Doc: "URL of the fleet-graph collector: the canonical https host or a loopback address.",
		Check: func(v string) error {
			_, err := normalizeCollectorURL(v)
			return err
		},
	})
	envAPIKey = env.Declare(env.Var{
		Name: "GRAPH_API_KEY", Kind: env.KindString, Secret: true,
		Doc: "Writer-only API key, the compatibility path for services not yet using GRAPH_API_KEY_FILE. Never falls back to FLEET_API_KEY.",
	})
	envAPIKeyFile = env.Declare(env.Var{
		Name: "GRAPH_API_KEY_FILE", Kind: env.KindString,
		Doc: "Path of the protected file holding the writer API key. It takes precedence over GRAPH_API_KEY, and an unreadable file fails closed.",
	})
	envSPIFFECollectorURL = env.Declare(env.Var{
		Name: "GRAPH_SPIFFE_COLLECTOR_URL", Kind: env.KindString,
		Doc: "HTTPS root of the collector's private SPIFFE mTLS listener, e.g. https://go-fleet-graph:8443. With GRAPH_SPIFFE_COLLECTOR_ID and SPIFFE_ENDPOINT_SOCKET, batches go there with the workload's X.509-SVID and no writer key; a configured writer key stays the fallback.",
		Check: func(v string) error {
			_, err := normalizeSPIFFECollectorURL(v)
			return err
		},
	})
	envSPIFFECollectorID = env.Declare(env.Var{
		Name: "GRAPH_SPIFFE_COLLECTOR_ID", Kind: env.KindString,
		Doc: "Exact SPIFFE ID the collector's mTLS listener must prove, e.g. spiffe://0exec.com/ns/fleet/sa/fleet-graph. Required by GRAPH_SPIFFE_COLLECTOR_URL.",
		Check: func(v string) error {
			if !validSPIFFEIDShape(strings.TrimSpace(v)) {
				return fmt.Errorf("not a spiffe:// ID")
			}
			return nil
		},
	})
	envSPIFFESocket = env.Declare(env.Var{
		Name: "SPIFFE_ENDPOINT_SOCKET", Kind: env.KindString,
		Doc: "SPIFFE Workload API socket (unix:///run/spire/sockets/agent.sock), set by the fleet's Compose SPIFFE overlay. graph reads it only for GRAPH_SPIFFE_COLLECTOR_URL.",
	})
	envReaderAPIKey = env.Declare(env.Var{
		Name: "GRAPH_READER_API_KEY", Kind: env.KindString, Secret: true,
		Doc: "Read-side key for graph lookups, kept separate from the writer key.",
	})
	envServiceID = env.Declare(env.Var{
		Name: "GRAPH_SERVICE_ID", Kind: env.KindString,
		Doc: "Canonical registry id used as the identity of batches and events: a lower-case DNS label. An invalid value fails closed.",
		Check: func(v string) error {
			if !validGraphLabel(strings.ToLower(strings.TrimSpace(v))) {
				return fmt.Errorf("not a valid service id (lower-case letters, digits and '-', at most 63 characters)")
			}
			return nil
		},
	})
	envTargetAliases = env.Declare(env.Var{
		Name: "GRAPH_TARGET_ALIASES", Kind: env.KindList,
		Doc: "Comma-separated host=canonical-service-id pairs that map an outbound host to a service id.",
	})
	envTrustedCallerIPs = env.Declare(env.Var{
		Name: "GRAPH_TRUSTED_CALLER_IPS", Kind: env.KindList,
		Doc: "Comma-separated literal IP addresses of gateways trusted to stamp X-Auth-User. Entries that are not literal IPs are ignored.",
		Check: func(v string) error {
			for _, part := range strings.Split(v, ",") {
				if part = strings.TrimSpace(part); part == "" {
					continue
				}
				if _, err := netip.ParseAddr(part); err != nil {
					return fmt.Errorf("%q is not a literal IP address and would be ignored", part)
				}
			}
			return nil
		},
	})
	envSampleRate = env.Declare(env.Var{
		Name: "GRAPH_SAMPLE_RATE", Kind: env.KindFloat, Default: "1",
		Doc: "Fraction of events kept, clamped to the range 0 to 1.",
	})
	envBufferSize = env.Declare(env.Var{
		Name: "GRAPH_BUFFER_SIZE", Kind: env.KindInt, Default: "10000",
		Doc: "Events buffered before the oldest are dropped; at least 64.",
	})
	envFlushInterval = env.Declare(env.Var{
		Name: "GRAPH_FLUSH_INTERVAL", Kind: env.KindInt, Default: "10",
		Doc: "Seconds between flushes of buffered events (an integer number of seconds, not a duration); at least 1.",
	})
	envFlushBatch = env.Declare(env.Var{
		Name: "GRAPH_FLUSH_BATCH", Kind: env.KindInt, Default: "500",
		Doc: "Events per flush request; at least 1.",
	})
	envNode = env.Declare(env.Var{
		Name: "FLEET_NODE", Kind: env.KindString,
		Doc: "Logical name of the host (VM) this process runs on, sent as the X-Fleet-Node header: a lower-case DNS label. Anything else is ignored with one log line.",
		Check: func(v string) error {
			if !validGraphLabel(strings.ToLower(strings.TrimSpace(v))) {
				return fmt.Errorf("not a lower-case DNS label, so it is ignored and batches carry no node")
			}
			return nil
		},
	})
)

// GRAPH_USE_EGRESS_PROXY was added in v0.102.59 and removed in v0.102.60. A service
// that still sets it is told at start-up, not left wondering why nothing changed.
var _ = env.Removed("GRAPH_USE_EGRESS_PROXY", "v0.102.60",
	"graph always connects to the collector directly; delete the variable")
