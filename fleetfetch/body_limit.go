package fleetfetch

import (
	"errors"
	"io"
	"log"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/baditaflorin/go-common/env"
)

// DefaultMaxBodyBytes is the most of a response body a Client keeps unless it is
// told otherwise (WithMaxBodyBytes, WithUnboundedBody, or EnvMaxBodyBytes): 32 MiB.
// Bytes past it are dropped and Response.Truncated is set; the fetch is not an
// error.
//
// Why 32 MiB: it is what this module already applies to fetch-proxy responses
// (client.DefaultBodyCap), and it is above every limit that a fleet service chose
// for a page or a JSON reply when it set one with WithMaxBodyBytes (1 to 5 MiB for
// almost all of the 50 or so call sites, 8 MiB for three; one image fetcher takes
// 12) and above jsbundle.DefaultMaxScriptBytes (8 MiB). A lower figure would cut
// documents that are legitimately large and are read by callers that do not look at
// Truncated: the sitemap protocol allows 50 MiB uncompressed, and a silent cut at
// 8 MiB breaks sitemap and large-feed fetchers. A response of any size now costs at
// most this much memory (the read buffer can grow to about twice that while it
// fills). It is a ceiling for memory, not a promise about content: a service that
// parses larger documents says so with WithMaxBodyBytes (a sitemap fetcher that
// wants the full 50 MiB: 50<<20) or WithUnboundedBody.
const DefaultMaxBodyBytes int64 = 32 << 20

// EnvMaxBodyBytes overrides DefaultMaxBodyBytes for every Client that does not set
// its own limit with an option: a positive number of bytes is the limit, and 0
// means no limit at all, which is discouraged (a single response from a hostile or
// broken site can then take all of a worker's memory). An unparsable or negative
// value is ignored, with one log line.
const EnvMaxBodyBytes = "FLEET_FETCH_MAX_BODY_BYTES"

var _ = env.Declare(env.Var{
	Name: EnvMaxBodyBytes, Kind: env.KindInt, Default: "33554432",
	Doc: "Response-body limit in bytes for every fleetfetch client that does not set its own with WithMaxBodyBytes or WithUnboundedBody: a longer body is cut " +
		"there and Response.Truncated is set. 0 removes the limit, which is discouraged: one oversized response can take all of a worker's memory. " +
		"An unparsable or negative value is ignored.",
	Check: func(v string) error {
		if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err != nil || n < 0 {
			return errors.New("not a non-negative integer; the default limit is used")
		}
		return nil
	},
})

// unboundedBody is Client.maxBodyBytes for "no limit". Zero is "not set": the
// environment, then DefaultMaxBodyBytes, decide.
const unboundedBody int64 = -1

var badBodyLimitEnvOnce sync.Once

// bodyLimitFromEnv resolves the limit of a Client that set none: the environment
// variable when it holds a usable value, else DefaultMaxBodyBytes.
func bodyLimitFromEnv() int64 {
	raw := strings.TrimSpace(os.Getenv(EnvMaxBodyBytes))
	if raw == "" {
		return DefaultMaxBodyBytes
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		badBodyLimitEnvOnce.Do(func() {
			log.Printf("fleetfetch: ignoring %s=%q (not a non-negative integer); using the default limit of %d bytes", EnvMaxBodyBytes, raw, DefaultMaxBodyBytes)
		})
		return DefaultMaxBodyBytes
	}
	if n == 0 {
		return unboundedBody
	}
	return n
}

// bodyLimit is the limit readBody applies. A Client not built by NewClient has
// none set and gets the default, so the default holds there too.
func (c *Client) bodyLimit() int64 {
	if c.maxBodyBytes == 0 {
		return DefaultMaxBodyBytes
	}
	return c.maxBodyBytes
}

// readBody reads a response body up to the client's limit. truncated reports that
// the body was longer: data then holds exactly the first limit bytes. It reads one
// byte past the limit to know, and never more, so the memory a response can take
// is bounded by the limit, whatever the size the origin claims.
func (c *Client) readBody(body io.Reader) (data []byte, truncated bool, err error) {
	limit := c.bodyLimit()
	if limit < 0 || limit == math.MaxInt64 {
		data, err = io.ReadAll(body)
		return data, false, err
	}
	data, err = io.ReadAll(io.LimitReader(body, limit+1))
	if int64(len(data)) > limit {
		return data[:limit], true, err
	}
	return data, false, err
}

var firstTruncationOnce sync.Once

// noteTruncated says, once per process, that a body was cut: the first time is
// when an operator who just took a release with the default limit needs to know.
// The counts are in Stats().Truncated, Event.Truncated and the metric built on it.
func noteTruncated(host string, limit int64) {
	firstTruncationOnce.Do(func() {
		log.Printf("fleetfetch: a response body from %q was cut at %d bytes (Response.Truncated is set). A client that needs more sets WithMaxBodyBytes or WithUnboundedBody; "+
			"%s changes the default for clients that set neither", host, limit, EnvMaxBodyBytes)
	})
}
