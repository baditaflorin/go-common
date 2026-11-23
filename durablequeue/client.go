// Package durablequeue provides a scoped client for the durable work queue.
// A 204 claim response means no task is claimable now, not that the run ended.
package durablequeue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/baditaflorin/go-common/credfile"
)

var ErrInvalid = errors.New("durablequeue: invalid request")
var ErrConflict = errors.New("durablequeue: conflict or stale lease")
var ErrUnauthorized = errors.New("durablequeue: unauthorized")
var ErrUnavailable = errors.New("durablequeue: service unavailable")

type Config struct {
	URL, Token, TokenFile string
	Timeout               time.Duration
	Transport             http.RoundTripper
}
type Client struct {
	base   string
	token  string
	source *credfile.Source
	http   *http.Client
}

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return nil, ErrInvalid
	}
	if cfg.Token != "" && cfg.TokenFile != "" || strings.ContainsAny(cfg.Token, "\r\n\x00") {
		return nil, ErrInvalid
	}
	var source *credfile.Source
	if cfg.TokenFile != "" {
		source, err = credfile.Open("durable_queue_api_key", cfg.TokenFile, credfile.Options{Protected: true, MaxBytes: 8 << 10})
		if err != nil {
			return nil, ErrInvalid
		}
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	rt := cfg.Transport
	if rt == nil {
		rt = &http.Transport{Proxy: nil, MaxIdleConns: 64, MaxIdleConnsPerHost: 16, IdleConnTimeout: 60 * time.Second, ForceAttemptHTTP2: true}
	}
	return &Client{base: u.String(), token: strings.TrimSpace(cfg.Token), source: source, http: &http.Client{Timeout: timeout, Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

type Task struct {
	Key         string          `json:"key"`
	Payload     json.RawMessage `json:"payload"`
	MaxAttempts int             `json:"max_attempts"`
}
type Lease struct {
	Key      string          `json:"key"`
	Worker   string          `json:"worker"`
	Version  int64           `json:"lease_version"`
	Attempts int             `json:"attempts"`
	Until    time.Time       `json:"lease_until"`
	Payload  json.RawMessage `json:"payload"`
}
type Stats struct {
	State    string `json:"state"`
	Sealed   bool   `json:"sealed"`
	Total    int64  `json:"total"`
	Pending  int64  `json:"pending"`
	Leased   int64  `json:"leased"`
	Done     int64  `json:"done"`
	Failed   int64  `json:"failed"`
	Complete bool   `json:"complete"`
}

func valid(s string, max int) bool {
	return len(s) > 0 && len(s) <= max && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n\t")
}
func runPath(run string) (string, error) {
	if !valid(run, 128) {
		return "", ErrInvalid
	}
	return "/v1/runs/" + url.PathEscape(run), nil
}
func (c *Client) call(ctx context.Context, method, path string, input, output any) (int, error) {
	var body io.Reader
	if input != nil {
		raw, err := json.Marshal(input)
		if err != nil || len(raw) > 1<<20 {
			return 0, ErrInvalid
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, ErrInvalid
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	token := c.token
	if c.source != nil {
		token = c.source.Value()
	}
	if token == "" || strings.ContainsAny(token, "\r\n\x00") {
		return 0, ErrUnauthorized
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, ErrUnavailable
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 200, 204:
	case 400:
		return resp.StatusCode, ErrInvalid
	case 401, 403:
		return resp.StatusCode, ErrUnauthorized
	case 409:
		return resp.StatusCode, ErrConflict
	default:
		return resp.StatusCode, ErrUnavailable
	}
	if output != nil && resp.StatusCode == 200 {
		b, err := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
		if err != nil || len(b) > 1<<20 || json.Unmarshal(b, output) != nil {
			return resp.StatusCode, ErrUnavailable
		}
	}
	return resp.StatusCode, nil
}
func (c *Client) postAck(ctx context.Context, path string, input any, key string, want bool) error {
	var out map[string]bool
	status, err := c.call(ctx, http.MethodPost, path, input, &out)
	if err != nil {
		return err
	}
	got, ok := out[key]
	if status != http.StatusOK || !ok || got != want {
		return ErrUnavailable
	}
	return nil
}
func (c *Client) CreateRun(ctx context.Context, run string) error {
	p, e := runPath(run)
	if e != nil {
		return e
	}
	return c.postAck(ctx, p, struct{}{}, "created", true)
}
func (c *Client) Enqueue(ctx context.Context, run string, tasks []Task) error {
	p, e := runPath(run)
	if e != nil {
		return e
	}
	if len(tasks) < 1 || len(tasks) > 1000 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, t := range tasks {
		if !valid(t.Key, 256) || seen[t.Key] || !json.Valid(t.Payload) || len(t.Payload) > 65536 || t.MaxAttempts < 1 || t.MaxAttempts > 10 {
			return ErrInvalid
		}
		seen[t.Key] = true
	}
	var out struct {
		Accepted int `json:"accepted"`
	}
	status, e := c.call(ctx, http.MethodPost, p+"/tasks", struct {
		Tasks []Task `json:"tasks"`
	}{tasks}, &out)
	if e != nil {
		return e
	}
	if status != http.StatusOK || out.Accepted != len(tasks) {
		return ErrUnavailable
	}
	return nil
}
func (c *Client) Seal(ctx context.Context, run string, expected int64) error {
	p, e := runPath(run)
	if e != nil || expected < 0 {
		return ErrInvalid
	}
	return c.postAck(ctx, p+"/seal", struct {
		Expected int64 `json:"expected"`
	}{expected}, "sealed", true)
}
func (c *Client) Stats(ctx context.Context, run string) (Stats, error) {
	var s Stats
	p, e := runPath(run)
	if e != nil {
		return s, e
	}
	status, e := c.call(ctx, http.MethodGet, p, nil, &s)
	if e == nil && status != http.StatusOK {
		e = ErrUnavailable
	}
	if e != nil {
		return Stats{}, e
	}
	if (s.State != "running" && s.State != "paused") || s.Total < 0 || s.Pending < 0 || s.Leased < 0 || s.Done < 0 || s.Failed < 0 || s.Pending+s.Leased+s.Done+s.Failed != s.Total || s.Complete && (!s.Sealed || s.Pending != 0 || s.Leased != 0) {
		return Stats{}, ErrUnavailable
	}
	return s, nil
}
func (c *Client) Claim(ctx context.Context, run, worker string, lease time.Duration) (Lease, bool, error) {
	var l Lease
	p, e := runPath(run)
	if e != nil || !valid(worker, 128) || lease < time.Second || lease > time.Hour || lease%time.Second != 0 {
		return l, false, ErrInvalid
	}
	status, e := c.call(ctx, http.MethodPost, p+"/claim", struct {
		Worker  string `json:"worker"`
		Seconds int64  `json:"lease_seconds"`
	}{worker, int64(lease / time.Second)}, &l)
	if e != nil {
		return Lease{}, false, e
	}
	if status == 204 {
		return Lease{}, false, nil
	}
	if !valid(l.Key, 256) || l.Worker != worker || l.Version < 1 || l.Attempts < 1 || l.Until.IsZero() || !json.Valid(l.Payload) {
		return Lease{}, false, ErrUnavailable
	}
	return l, true, nil
}
func taskPath(run string, l Lease, action string) (string, error) {
	p, e := runPath(run)
	if e != nil || !valid(l.Key, 256) || !valid(l.Worker, 128) || l.Version < 1 {
		return "", ErrInvalid
	}
	return p + "/tasks/" + url.PathEscape(l.Key) + "/" + action, nil
}
func (c *Client) Complete(ctx context.Context, run string, l Lease, result json.RawMessage) error {
	p, e := taskPath(run, l, "complete")
	if e != nil || !json.Valid(result) || len(result) > 65536 {
		return ErrInvalid
	}
	return c.postAck(ctx, p, struct {
		Worker  string          `json:"worker"`
		Version int64           `json:"lease_version"`
		Result  json.RawMessage `json:"result"`
	}{l.Worker, l.Version, result}, "completed", true)
}
func (c *Client) Fail(ctx context.Context, run string, l Lease, code string, retry bool, backoff time.Duration) error {
	p, e := taskPath(run, l, "fail")
	if e != nil || !valid(code, 128) || backoff < 0 || backoff > time.Hour || backoff%time.Millisecond != 0 {
		return ErrInvalid
	}
	return c.postAck(ctx, p, struct {
		Worker  string `json:"worker"`
		Version int64  `json:"lease_version"`
		Code    string `json:"error_code"`
		Retry   bool   `json:"retry"`
		Backoff int64  `json:"backoff_ms"`
	}{l.Worker, l.Version, code, retry, backoff.Milliseconds()}, "recorded", true)
}
func (c *Client) Renew(ctx context.Context, run string, l Lease, lease time.Duration) error {
	p, e := taskPath(run, l, "renew")
	if e != nil || lease < time.Second || lease > time.Hour || lease%time.Second != 0 {
		return ErrInvalid
	}
	return c.postAck(ctx, p, struct {
		Worker  string `json:"worker"`
		Version int64  `json:"lease_version"`
		Seconds int64  `json:"lease_seconds"`
	}{l.Worker, l.Version, int64(lease / time.Second)}, "renewed", true)
}
func (c *Client) SetPaused(ctx context.Context, run string, paused bool) error {
	p, e := runPath(run)
	if e != nil {
		return e
	}
	action := "resume"
	if paused {
		action = "pause"
	}
	return c.postAck(ctx, p+"/"+action, struct{}{}, "paused", paused)
}

// TerminalTask is an owner-scoped completed or permanently failed task receipt.
// Large results should live in the owning storage service, referenced here.
type TerminalTask struct {
	Key       string          `json:"key"`
	State     string          `json:"state"`
	Attempts  int             `json:"attempts"`
	Result    json.RawMessage `json:"result,omitempty"`
	ErrorCode string          `json:"error_code,omitempty"`
}
type TerminalPage struct {
	Tasks []TerminalTask `json:"tasks"`
	Next  string         `json:"next,omitempty"`
}

// TerminalResults returns one byte-bounded keyset page. For an exhaustive
// export, wait until Stats reports Complete: tasks can become terminal behind
// a cursor while a run is still active.
func (c *Client) TerminalResults(ctx context.Context, run, after string, limit int) (TerminalPage, error) {
	var page TerminalPage
	path, err := runPath(run)
	if err != nil || (after != "" && !valid(after, 256)) || limit < 1 || limit > 100 {
		return page, ErrInvalid
	}
	query := url.Values{}
	query.Set("limit", strconv.Itoa(limit))
	if after != "" {
		query.Set("after", after)
	}
	status, err := c.call(ctx, http.MethodGet, path+"/tasks?"+query.Encode(), nil, &page)
	if err != nil {
		return TerminalPage{}, err
	}
	if status != http.StatusOK || len(page.Tasks) > limit || page.Tasks == nil {
		return TerminalPage{}, ErrUnavailable
	}
	seen := make(map[string]bool, len(page.Tasks))
	for _, task := range page.Tasks {
		if !valid(task.Key, 256) || task.Key == after || seen[task.Key] || task.Attempts < 1 {
			return TerminalPage{}, ErrUnavailable
		}
		seen[task.Key] = true
		switch task.State {
		case "done":
			if !json.Valid(task.Result) || task.ErrorCode != "" {
				return TerminalPage{}, ErrUnavailable
			}
		case "failed":
			if len(task.Result) != 0 || !valid(task.ErrorCode, 128) {
				return TerminalPage{}, ErrUnavailable
			}
		default:
			return TerminalPage{}, ErrUnavailable
		}
	}
	if page.Next != "" && (len(page.Tasks) == 0 || page.Next != page.Tasks[len(page.Tasks)-1].Key) {
		return TerminalPage{}, ErrUnavailable
	}
	return page, nil
}

// Completion is a lightweight indexed completion check, without exact counts.
type Completion struct {
	Sealed   bool
	Complete bool
}

func (c *Client) RunCompletion(ctx context.Context, run string) (Completion, error) {
	var out Completion
	path, err := runPath(run)
	if err != nil {
		return out, err
	}
	var response struct {
		Sealed   *bool `json:"sealed"`
		Complete *bool `json:"complete"`
	}
	status, err := c.call(ctx, http.MethodGet, path+"/completion", nil, &response)
	if err != nil {
		return out, err
	}
	if status != http.StatusOK || response.Sealed == nil || response.Complete == nil || *response.Complete && !*response.Sealed {
		return out, ErrUnavailable
	}
	out.Sealed = *response.Sealed
	out.Complete = *response.Complete
	return out, nil
}

// RecordAttempt writes one bounded immutable receipt under the current lease.
// Retrying the same receipt after an ambiguous acknowledgement is safe.
func (c *Client) RecordAttempt(ctx context.Context, run string, l Lease, receipt json.RawMessage) error {
	p, e := taskPath(run, l, "attempt")
	var object map[string]json.RawMessage
	if e != nil || len(receipt) > 16384 || !json.Valid(receipt) || json.Unmarshal(receipt, &object) != nil || object == nil {
		return ErrInvalid
	}
	return c.postAck(ctx, p, struct {
		Worker  string          `json:"worker"`
		Version int64           `json:"lease_version"`
		Receipt json.RawMessage `json:"receipt"`
	}{l.Worker, l.Version, receipt}, "recorded", true)
}

type AttemptReceipt struct {
	LeaseVersion int64           `json:"lease_version"`
	Attempt      int             `json:"attempt"`
	Payload      json.RawMessage `json:"payload"`
	CreatedAt    time.Time       `json:"created_at"`
}

func (c *Client) AttemptReceipts(ctx context.Context, run, key string) ([]AttemptReceipt, error) {
	p, e := runPath(run)
	if e != nil || !valid(key, 256) {
		return nil, ErrInvalid
	}
	var out struct {
		Attempts []AttemptReceipt `json:"attempts"`
	}
	status, e := c.call(ctx, http.MethodGet, p+"/tasks/"+url.PathEscape(key)+"/attempts", nil, &out)
	if e != nil {
		return nil, e
	}
	if status != http.StatusOK || out.Attempts == nil || len(out.Attempts) > 10 {
		return nil, ErrUnavailable
	}
	var last int64
	for _, r := range out.Attempts {
		var object map[string]json.RawMessage
		if r.LeaseVersion <= last || r.Attempt < 1 || r.Attempt > 10 || r.CreatedAt.IsZero() || len(r.Payload) > 16384 || json.Unmarshal(r.Payload, &object) != nil || object == nil {
			return nil, ErrUnavailable
		}
		last = r.LeaseVersion
	}
	return out.Attempts, nil
}
