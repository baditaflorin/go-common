package meshclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/baditaflorin/go-common/degraded"
	"github.com/baditaflorin/go-common/graph"
	"github.com/baditaflorin/go-common/resilience"
	"github.com/baditaflorin/go-common/telemetry"
)

// Mode says what a caller does when the sibling is unavailable. It has no default
// on purpose: a client that quietly fails open is an authorisation bypass waiting
// to happen, and one that quietly fails closed is an outage amplifier.
type Mode int

const (
	_ Mode = iota // the zero value is not a Mode: New rejects a Config that leaves it out
	// FailOpen: the caller carries on without the answer. Unavailability is
	// reported in the context's degraded.Sink.
	FailOpen
	// FailClosed: the caller stops. Unavailability is only an error.
	FailClosed
)

func (m Mode) String() string {
	switch m {
	case FailOpen:
		return "fail_open"
	case FailClosed:
		return "fail_closed"
	}
	return "unset"
}

// Defaults.
const (
	DefaultTimeout      = 3 * time.Second
	DefaultMaxBodyBytes = 1 << 20
	DefaultUserAgent    = "go-common/meshclient"
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

// Config describes one sibling service.
type Config struct {
	// Name is the sibling's slug ("ledger"). It labels errors and becomes the
	// prefix of degraded tokens ("ledger-down"), so it is lower case with digits,
	// '-' and '_' only.
	Name string
	// BaseURL is the sibling's address, http or https, without credentials, query
	// or fragment.
	BaseURL string
	// Mode is required: FailOpen or FailClosed.
	Mode Mode
	// Timeout bounds each attempt. Default DefaultTimeout.
	Timeout time.Duration
	// MaxBodyBytes caps how much of a reply is read. Default DefaultMaxBodyBytes.
	MaxBodyBytes int64
	// UserAgent defaults to DefaultUserAgent.
	UserAgent string
	// RejectUnknownFields makes decoding fail on a JSON field that out does not
	// have. Off by default so a sibling can add fields without breaking callers.
	RejectUnknownFields bool
	// Policy adds retries (with a budget), a circuit breaker and load shedding.
	// Nil makes exactly one attempt.
	Policy *resilience.Policy
	// Transport replaces the default (direct connections, tuned timeouts, trace
	// propagation and fleet-graph events). Tests pass httptest's.
	Transport http.RoundTripper
}

// Client talks to one sibling service. It is safe for concurrent use.
type Client struct {
	name    string
	base    *url.URL
	mode    Mode
	hc      *http.Client
	maxBody int64
	ua      string
	strict  bool
	policy  *resilience.Policy
}

// New validates cfg and returns a Client.
func New(cfg Config) (*Client, error) {
	if !nameRe.MatchString(cfg.Name) {
		return nil, fmt.Errorf("meshclient: Name %q must be a slug: lower-case letters, digits, '-' and '_'", cfg.Name)
	}
	if cfg.Mode != FailOpen && cfg.Mode != FailClosed {
		return nil, fmt.Errorf("meshclient: %s: Mode is required (FailOpen or FailClosed)", cfg.Name)
	}
	base, err := url.Parse(cfg.BaseURL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" ||
		base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("meshclient: %s: BaseURL must be an http(s) URL without credentials, query or fragment", cfg.Name)
	}
	c := &Client{
		name: cfg.Name, base: base, mode: cfg.Mode, policy: cfg.Policy,
		maxBody: cfg.MaxBodyBytes, ua: cfg.UserAgent, strict: cfg.RejectUnknownFields,
	}
	if c.maxBody <= 0 {
		c.maxBody = DefaultMaxBodyBytes
	}
	if c.ua == "" {
		c.ua = DefaultUserAgent
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	rt := cfg.Transport
	if rt == nil {
		rt = defaultTransport()
	}
	c.hc = &http.Client{
		Timeout:       timeout,
		Transport:     rt,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c, nil
}

// defaultTransport dials directly (mesh addresses are private and need no proxy),
// with timeouts on every phase, and carries the fleet's trace context and graph
// events.
func defaultTransport() http.RoundTripper {
	t := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   3 * time.Second,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConnsPerHost:   16,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     true,
	}
	return telemetry.NewTransport(graph.RoundTripper(t))
}

// Name returns the sibling's slug.
func (c *Client) Name() string { return c.name }

// Mode returns the failure mode the client was built with.
func (c *Client) Mode() Mode { return c.mode }

// Request is one call.
type Request struct {
	// Method defaults to GET.
	Method string
	// Path is appended to the base URL's path and must start with "/".
	Path  string
	Query url.Values
	// Header is added to the request (credentials, idempotency keys).
	Header http.Header
	// Body is sent as JSON: a []byte or json.RawMessage as is, anything else
	// marshalled. Nil sends no body.
	Body any
}

// Do makes the request and, on a 2xx answer, decodes the JSON body into out (nil
// to ignore it). See the package comment for how failures are reported.
func (c *Client) Do(ctx context.Context, req Request, out any) error {
	body, err := encodeBody(req.Body)
	if err != nil {
		return fmt.Errorf("meshclient: %s: encode request: %w", c.name, err)
	}
	if !strings.HasPrefix(req.Path, "/") {
		return fmt.Errorf("meshclient: %s: Path %q must start with '/'", c.name, req.Path)
	}

	attempt := func(ctx context.Context) error { return c.once(ctx, req, body, out) }
	if c.policy != nil {
		err = c.policy.Do(ctx, attempt)
	} else {
		err = attempt(ctx)
	}
	if err == nil {
		return nil
	}
	return c.report(ctx, err)
}

// Get is Do for a GET with no body.
func (c *Client) Get(ctx context.Context, path string, out any) error {
	return c.Do(ctx, Request{Path: path}, out)
}

// PostJSON is Do for a POST of in.
func (c *Client) PostJSON(ctx context.Context, path string, in, out any) error {
	return c.Do(ctx, Request{Method: http.MethodPost, Path: path, Body: in}, out)
}

func encodeBody(v any) ([]byte, error) {
	switch b := v.(type) {
	case nil:
		return nil, nil
	case []byte:
		return b, nil
	case json.RawMessage:
		return b, nil
	}
	return json.Marshal(v)
}

// once makes one attempt and returns the raw error, unclassified.
func (c *Client) once(ctx context.Context, req Request, body []byte, out any) error {
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	u := c.base.JoinPath(req.Path)
	if len(req.Query) > 0 {
		u.RawQuery = req.Query.Encode()
	}
	var rd io.Reader
	if len(body) > 0 {
		rd = bytes.NewReader(body)
	}
	hreq, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return fmt.Errorf("meshclient: %s: build request: %w", c.name, err)
	}
	for k, vs := range req.Header {
		for _, v := range vs {
			hreq.Header.Add(k, v)
		}
	}
	hreq.Header.Set("User-Agent", c.ua)
	hreq.Header.Set("Accept", "application/json")
	if len(body) > 0 && hreq.Header.Get("Content-Type") == "" {
		hreq.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(hreq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, c.maxBody+1))
	if err != nil {
		return &decodeError{c.name, fmt.Errorf("read body: %w", err)}
	}
	if int64(len(data)) > c.maxBody {
		return &decodeError{c.name, fmt.Errorf("response is larger than %d bytes", c.maxBody)}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &StatusError{Name: c.name, Method: method, Path: req.Path, Status: resp.StatusCode, Body: data}
	}
	if out == nil {
		return nil
	}
	if err := c.decode(data, out); err != nil {
		return &decodeError{c.name, err}
	}
	return nil
}

// decode reads exactly one JSON value into out and rejects anything after it.
func (c *Client) decode(data []byte, out any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return errors.New("empty body")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if c.strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(out); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the JSON value")
	}
	return nil
}

// report turns the final raw error into what the caller sees: an
// *UnavailableError when the sibling cannot be used right now (also noting it in
// the request's degraded list in FailOpen mode), or the error unchanged when the
// request itself was wrong.
func (c *Client) report(ctx context.Context, err error) error {
	class, unavailable := classify(ctx, err)
	if !unavailable {
		return err
	}
	if c.mode == FailOpen {
		degraded.FromContext(ctx).Append(c.name + "-" + class)
	}
	return &UnavailableError{Name: c.name, Class: class, Cause: err}
}

// classify decides whether err means "unavailable" and which degraded suffix fits.
func classify(ctx context.Context, err error) (class string, unavailable bool) {
	var se *StatusError
	var de *decodeError
	var ne net.Error
	var ue *url.Error
	switch {
	case errors.Is(err, resilience.ErrShed):
		return "degraded", true
	case errors.Is(err, resilience.ErrCircuitOpen):
		return "down", true
	case errors.As(err, &se):
		switch {
		case se.Status == http.StatusTooManyRequests:
			return "rate_limited", true
		case se.Status == http.StatusRequestTimeout || se.Status == http.StatusGatewayTimeout:
			return "timeout", true
		case resilience.RetryableStatus(se.Status):
			return "down", true
		}
		return "", false
	case errors.As(err, &de):
		return "down", true
	case errors.Is(err, context.Canceled) && ctx.Err() != nil:
		return "", false // the caller gave up; the sibling did nothing wrong
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", true
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout", true
	case errors.As(err, &ne), errors.As(err, &ue), errors.Is(err, io.ErrUnexpectedEOF):
		return "down", true
	}
	return "", false
}
