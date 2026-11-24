package capturestore

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/baditaflorin/go-common/credfile"
	"github.com/baditaflorin/go-common/graph"
	"github.com/baditaflorin/go-common/header"
	"github.com/baditaflorin/go-common/internal/tracehttp"
)

const (
	MaxBodyBytes     = 8 << 20
	MaxMetadataBytes = 16 << 10
	DefaultTimeout   = 5 * time.Second
	maxResponseBytes = 2 << 20
	capturesPath     = "/v1/captures"
)

var idPattern = regexp.MustCompile("^[A-Za-z0-9_-]{1,128}$")
var shaPattern = regexp.MustCompile("^[a-fA-F0-9]{64}$")

// Config identifies a capture-store HTTPS origin and a scoped service credential.
// Transport is intended for tests and does not relax URL validation.
type Config struct {
	URL, Token, TokenFile string
	Timeout               time.Duration
	Transport             http.RoundTripper
}
type Client struct {
	base        *url.URL
	token       string
	tokenSource *credfile.Source
	http        *http.Client
}

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return nil, errors.New("capturestore: URL must be an HTTPS origin without path, credentials, query, or fragment")
	}
	if cfg.Token != "" && cfg.TokenFile != "" {
		return nil, errors.New("capturestore: configure Token or TokenFile, not both")
	}
	if strings.ContainsAny(cfg.Token, "\r\n\x00") {
		return nil, errors.New("capturestore: invalid token")
	}
	var source *credfile.Source
	if cfg.TokenFile != "" {
		source, err = credfile.Open("capture_store_api_key", cfg.TokenFile, credfile.Options{Protected: true, MaxBytes: 8 << 10})
		if err != nil {
			return nil, errors.New("capturestore: could not read token file")
		}
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	rt := cfg.Transport
	if rt == nil {
		tr := &http.Transport{DialContext: (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: timeout, IdleConnTimeout: 60 * time.Second, MaxIdleConnsPerHost: 16, ForceAttemptHTTP2: true}
		rt = tracehttp.NewTransport(graph.RoundTripper(tr))
	}
	return &Client{base: u, token: strings.TrimSpace(cfg.Token), tokenSource: source, http: &http.Client{Timeout: timeout, Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Metadata is the project-neutral, service-validated capture envelope.
type Metadata struct {
	IdempotencyKey string            `json:"idempotency_key"`
	Namespace      string            `json:"namespace"`
	SourceService  string            `json:"source_service"`
	SourceVersion  string            `json:"source_version"`
	RequestID      string            `json:"request_id"`
	URL            string            `json:"url"`
	FetchedAt      string            `json:"fetched_at"`
	HTTPStatus     int               `json:"http_status"`
	ContentType    string            `json:"content_type"`
	LatencyMS      int64             `json:"latency_ms"`
	Recipe         string            `json:"recipe"`
	Settings       map[string]string `json:"settings"`
	RequestedRoute RequestedRoute    `json:"requested_route"`
	RouteVerified  bool              `json:"route_verified"`
	Outcome        string            `json:"outcome"`
	Attempts       []RouteAttempt    `json:"attempts"`
	BodyKind       string            `json:"body_kind"`
}
type RequestedRoute struct {
	PoolID     string `json:"pool_id"`
	ProviderID string `json:"provider_id"`
	ProfileID  string `json:"profile_id"`
	Class      string `json:"class"`
}
type RouteAttempt struct {
	ProviderID string `json:"provider_id"`
	AccountID  string `json:"account_id"`
	PoolID     string `json:"pool_id"`
	Class      string `json:"class"`
	Outcome    string `json:"outcome"`
}
type Capture struct {
	ID string `json:"id"`
	Metadata
	BodyBytes int64  `json:"body_bytes"`
	SHA256    string `json:"sha256"`
	GzipBytes int64  `json:"gzip_bytes"`
	CreatedAt string `json:"created_at"`
}
type CreateResult struct {
	Capture Capture `json:"capture"`
	Reused  bool    `json:"reused"`
}
type ListOptions struct {
	Namespace, Domain, Provider, Account, Pool, Class, SourceService, Status string
	Limit, Offset                                                            int
}
type ListResult struct {
	Captures []Capture `json:"captures"`
	Total    int64     `json:"total"`
	Limit    int       `json:"limit"`
	Offset   int       `json:"offset"`
}
type Storage struct {
	Observations        int64 `json:"observations"`
	UniqueObjects       int64 `json:"unique_objects"`
	LogicalBodyBytes    int64 `json:"logical_body_bytes"`
	ReferencedGzipBytes int64 `json:"referenced_gzip_bytes"`
	UniqueGzipBytes     int64 `json:"unique_gzip_bytes"`
}
type TextDiff struct {
	Equal        bool   `json:"equal"`
	Truncated    bool   `json:"truncated"`
	LeftExcerpt  string `json:"left_excerpt"`
	RightExcerpt string `json:"right_excerpt"`
}
type Comparison struct {
	Left           Capture   `json:"left"`
	Right          Capture   `json:"right"`
	SameBody       bool      `json:"same_body"`
	BodyBytesDelta int64     `json:"body_bytes_delta"`
	LatencyMSDelta int64     `json:"latency_ms_delta"`
	Text           *TextDiff `json:"text,omitempty"`
}

func (c *Client) tokenValue() string {
	if c.tokenSource != nil {
		return strings.TrimSpace(c.tokenSource.Value())
	}
	return c.token
}
func (c *Client) endpoint(p string) string {
	u := *c.base
	u.Path = path.Clean("/" + strings.TrimPrefix(p, "/"))
	return u.String()
}
func (c *Client) do(ctx context.Context, method, p string, q url.Values, body []byte, contentType string, headers http.Header, out any) (*http.Response, error) {
	u, err := url.Parse(c.endpoint(p))
	if err != nil {
		return nil, errors.New("capturestore: invalid endpoint")
	}
	if q != nil {
		u.RawQuery = q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("capturestore: could not build request")
	}
	token := c.tokenValue()
	if token == "" {
		return nil, errors.New("capturestore: token is not configured")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(header.FleetCaller, "go-common/capturestore")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for k, vs := range headers {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("capturestore: request failed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, fmt.Errorf("capturestore: server returned HTTP %d", resp.StatusCode)
	}
	if out == nil {
		return resp, nil
	}
	data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	resp.Body.Close()
	if readErr != nil || len(data) > maxResponseBytes || json.Unmarshal(data, out) != nil {
		return nil, errors.New("capturestore: invalid response")
	}
	return nil, nil
}

func (c *Client) Create(ctx context.Context, m Metadata, body []byte) (CreateResult, error) {
	return c.create(ctx, m, body, "")
}

// CreateWithDigest carries a SHA-256 already computed over these immutable
// bytes. The service verifies it independently and the receipt must agree.
func (c *Client) CreateWithDigest(ctx context.Context, m Metadata, body []byte, digest string) (CreateResult, error) {
	if !shaPattern.MatchString(digest) || digest != strings.ToLower(digest) {
		return CreateResult{}, errors.New("capturestore: invalid source digest")
	}
	return c.create(ctx, m, body, digest)
}

func (c *Client) create(ctx context.Context, m Metadata, body []byte, digest string) (CreateResult, error) {
	var out CreateResult
	if len(body) > MaxBodyBytes {
		return out, errors.New("capturestore: body exceeds 8 MiB limit")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return out, errors.New("capturestore: invalid metadata")
	}
	encoded := base64.RawURLEncoding.EncodeToString(raw)
	if len(encoded) > MaxMetadataBytes {
		return out, errors.New("capturestore: metadata header exceeds 16 KiB limit")
	}
	if m.IdempotencyKey == "" || m.Namespace == "" || m.SourceService == "" || m.URL == "" || (m.BodyKind != "website_response" && m.BodyKind != "proxy_error_response") {
		return out, errors.New("capturestore: required metadata is missing or invalid")
	}
	h := make(http.Header)
	h.Set("X-Capture-Metadata", encoded)
	if digest != "" {
		h.Set("X-Source-SHA256", digest)
	}
	_, err = c.do(ctx, http.MethodPost, capturesPath, nil, body, "application/octet-stream", h, &out)
	if err != nil {
		return out, err
	}
	if digest == "" {
		sum := sha256.Sum256(body)
		digest = hex.EncodeToString(sum[:])
	}
	if !validID(out.Capture.ID) || out.Capture.BodyBytes != int64(len(body)) || out.Capture.SHA256 != digest || out.Capture.GzipBytes <= 0 || out.Capture.Namespace != m.Namespace || out.Capture.IdempotencyKey != m.IdempotencyKey || out.Capture.SourceService != m.SourceService || out.Capture.SourceVersion != m.SourceVersion {
		return CreateResult{}, errors.New("capturestore: invalid create receipt")
	}
	return out, nil
}
func validID(id string) bool { return idPattern.MatchString(id) }
func (c *Client) Get(ctx context.Context, id string) (Capture, error) {
	var out Capture
	if !validID(id) {
		return out, errors.New("capturestore: invalid capture ID")
	}
	_, err := c.do(ctx, http.MethodGet, capturesPath+"/"+url.PathEscape(id), nil, nil, "", nil, &out)
	return out, err
}
func (c *Client) List(ctx context.Context, o ListOptions) (ListResult, error) {
	var out ListResult
	if o.Limit < 0 || o.Limit > 100 || o.Offset < 0 {
		return out, errors.New("capturestore: invalid list pagination")
	}
	q := url.Values{}
	for k, v := range map[string]string{"namespace": o.Namespace, "domain": o.Domain, "provider": o.Provider, "account": o.Account, "pool": o.Pool, "class": o.Class, "source_service": o.SourceService, "status": o.Status} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if o.Limit > 0 {
		q.Set("limit", fmt.Sprint(o.Limit))
	}
	if o.Offset > 0 {
		q.Set("offset", fmt.Sprint(o.Offset))
	}
	_, err := c.do(ctx, http.MethodGet, capturesPath, q, nil, "", nil, &out)
	return out, err
}
func (c *Client) Storage(ctx context.Context) (Storage, error) {
	var out Storage
	_, err := c.do(ctx, http.MethodGet, "/v1/storage", nil, nil, "", nil, &out)
	return out, err
}
func (c *Client) Compare(ctx context.Context, left, right string) (Comparison, error) {
	var out Comparison
	if !validID(left) || !validID(right) {
		return out, errors.New("capturestore: invalid capture ID")
	}
	q := url.Values{"left": []string{left}, "right": []string{right}}
	_, err := c.do(ctx, http.MethodGet, "/v1/compare", q, nil, "", nil, &out)
	return out, err
}

// BodyResult is a gzip stream and its uncompressed SHA-256 checksum.
type BodyResult struct {
	Body          io.ReadCloser
	SHA256        string
	ContentLength int64
}
type limitedReadCloser struct {
	io.Reader
	io.Closer
}

func (c *Client) OpenBody(ctx context.Context, id string) (BodyResult, error) {
	if !validID(id) {
		return BodyResult{}, errors.New("capturestore: invalid capture ID")
	}
	headers := make(http.Header)
	headers.Set("Accept-Encoding", "identity")
	resp, err := c.do(ctx, http.MethodGet, capturesPath+"/"+url.PathEscape(id)+"/body", nil, nil, "", headers, nil)
	if err != nil {
		return BodyResult{}, err
	}
	if resp.Header.Get("Content-Type") != "application/gzip" || resp.Header.Get("Content-Encoding") != "" {
		resp.Body.Close()
		return BodyResult{}, errors.New("capturestore: invalid body response")
	}
	disposition, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if err != nil || disposition != "attachment" || params["filename"] != id+".response.gz" {
		resp.Body.Close()
		return BodyResult{}, errors.New("capturestore: invalid body response")
	}
	sum := resp.Header.Get("X-Capture-SHA256")
	if !shaPattern.MatchString(sum) {
		resp.Body.Close()
		return BodyResult{}, errors.New("capturestore: invalid body checksum")
	}
	n := resp.ContentLength
	if n < 0 || n > MaxBodyBytes+(64<<10) {
		resp.Body.Close()
		return BodyResult{}, errors.New("capturestore: invalid body length")
	}
	return BodyResult{Body: &limitedReadCloser{io.LimitReader(resp.Body, MaxBodyBytes+(64<<10)), resp.Body}, SHA256: strings.ToLower(sum), ContentLength: n}, nil
}
func (c *Client) Download(ctx context.Context, id string) ([]byte, error) {
	return c.download(ctx, id, "", 0)
}

// DownloadCapture verifies the downloaded body against its previously read
// index metadata in the same pass as verifying the download receipt.
func (c *Client) DownloadCapture(ctx context.Context, capture Capture) ([]byte, error) {
	if !shaPattern.MatchString(capture.SHA256) || capture.SHA256 != strings.ToLower(capture.SHA256) ||
		capture.BodyBytes < 0 || capture.BodyBytes > MaxBodyBytes {
		return nil, errors.New("capturestore: invalid capture body metadata")
	}
	return c.download(ctx, capture.ID, capture.SHA256, capture.BodyBytes)
}

func (c *Client) download(ctx context.Context, id, expectedDigest string, expectedBytes int64) ([]byte, error) {
	r, err := c.OpenBody(ctx, id)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	if expectedDigest != "" && expectedDigest != r.SHA256 {
		return nil, errors.New("capturestore: body receipt does not match capture metadata")
	}
	gz, err := gzip.NewReader(r.Body)
	if err != nil {
		return nil, errors.New("capturestore: invalid gzip body")
	}
	defer gz.Close()
	body, err := io.ReadAll(io.LimitReader(gz, MaxBodyBytes+1))
	if err != nil {
		return nil, errors.New("capturestore: body read failed")
	}
	if len(body) > MaxBodyBytes {
		return nil, errors.New("capturestore: body exceeds 8 MiB limit")
	}
	if expectedDigest != "" && int64(len(body)) != expectedBytes {
		return nil, errors.New("capturestore: body size does not match capture metadata")
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != r.SHA256 {
		return nil, errors.New("capturestore: body checksum mismatch")
	}
	return body, nil
}
