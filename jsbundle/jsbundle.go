// Package jsbundle recovers original JavaScript source from minified production
// bundles using source maps, then scans the recovered source. It is the shared
// pipeline used by go_jsbundle_secrets, go_jsbundle_route_extractor,
// go_postmessage_listener_finder, and go_prototype_pollution_static.
//
// Pipeline:
//
//  1. Parse the target HTML.
//  2. Collect every <script src=...> URL (absolute + same-origin relative).
//  3. For each script, GET it (subject to the caller's *http.Client).
//  4. Look for a sourceMappingURL comment, or probe "<bundle>.map" as fallback.
//  5. If a v3 source map is found, decode the "sourcesContent" array and use
//     that as the recovered source (one entry per original file).
//  6. If no map exists, fall back to the minified bundle body as the "source"
//     so callers can still grep for patterns.
//
// The package is intentionally allocation-conservative: each recovered source
// chunk is returned with its origin URL and file path so callers can cite
// findings back to a specific source.
package jsbundle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
)

const (
	// DefaultMaxScriptBytes caps a single bundle body. Production bundles
	// over a few MB are exceptional.
	DefaultMaxScriptBytes int64 = 8 * 1024 * 1024
	// DefaultMaxScripts caps how many <script src> tags we follow per page.
	DefaultMaxScripts = 32
	// DefaultMaxConcurrency caps parallel script fetches.
	DefaultMaxConcurrency = 4
)

// Source is one chunk of recovered source code.
type Source struct {
	// BundleURL is the URL of the minified bundle this came from.
	BundleURL string `json:"bundle_url"`
	// FilePath is the "sources[i]" entry from the source map (e.g.
	// "webpack:///./src/foo.js"). Empty when the map was unavailable.
	FilePath string `json:"file_path,omitempty"`
	// FromMap is true when this chunk came from a sourcesContent entry.
	// False means the chunk is the raw (likely minified) bundle body.
	FromMap bool `json:"from_map"`
	// Content is the recovered source text.
	Content string `json:"-"`
	// SizeBytes is len(Content). Exposed for telemetry without dumping body.
	SizeBytes int `json:"size_bytes"`
}

// RecoverOptions tunes the pipeline.
type RecoverOptions struct {
	Client         *http.Client
	UserAgent      string
	MaxScriptBytes int64
	MaxScripts     int
	MaxConcurrency int
}

func (o *RecoverOptions) normalize() {
	if o.Client == nil {
		o.Client = http.DefaultClient
	}
	if o.MaxScriptBytes <= 0 {
		o.MaxScriptBytes = DefaultMaxScriptBytes
	}
	if o.MaxScripts <= 0 {
		o.MaxScripts = DefaultMaxScripts
	}
	if o.MaxConcurrency <= 0 {
		o.MaxConcurrency = DefaultMaxConcurrency
	}
	if o.UserAgent == "" {
		o.UserAgent = "go-common-jsbundle/0.1"
	}
}

// scriptSrcRe matches <script ... src="..."> attributes case-insensitively.
// Permissive on purpose: handles single/double quotes and attribute order.
var scriptSrcRe = regexp.MustCompile(`(?is)<script\b[^>]*\bsrc\s*=\s*["']([^"']+)["']`)

// sourceMappingURLRe matches both //# and //@ forms of the sourceMappingURL
// trailing comment that production bundlers append.
var sourceMappingURLRe = regexp.MustCompile(`(?m)//[#@]\s*sourceMappingURL\s*=\s*(\S+)\s*$`)

// ExtractScriptURLs returns absolute script URLs found in html, resolved
// against base. Duplicates removed in encounter order.
func ExtractScriptURLs(html string, base *url.URL) []string {
	matches := scriptSrcRe.FindAllStringSubmatch(html, -1)
	seen := make(map[string]bool, len(matches))
	var out []string
	for _, m := range matches {
		raw := strings.TrimSpace(m[1])
		if raw == "" || strings.HasPrefix(raw, "data:") {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		abs := base.ResolveReference(u).String()
		// Drop fragments; some bundlers append a build hash as #.
		if i := strings.Index(abs, "#"); i >= 0 {
			abs = abs[:i]
		}
		if !seen[abs] {
			seen[abs] = true
			out = append(out, abs)
		}
	}
	return out
}

// sourceMap is the v3 source-map shape we care about. Only sourcesContent
// is required; the other fields are accepted but unused.
type sourceMap struct {
	Version        int      `json:"version"`
	Sources        []string `json:"sources"`
	SourcesContent []string `json:"sourcesContent"`
}

// Recover downloads bundleURL and any associated .map, returning one Source
// per original file when a source map is available, or a single Source with
// the raw bundle body otherwise.
func Recover(ctx context.Context, bundleURL string, opt RecoverOptions) ([]Source, error) {
	opt.normalize()
	body, err := fetchBody(ctx, opt.Client, opt.UserAgent, bundleURL, opt.MaxScriptBytes)
	if err != nil {
		return nil, fmt.Errorf("fetch bundle: %w", err)
	}

	mapURL := findMapURL(body, bundleURL)
	if mapURL == "" {
		// fall back to bundle body
		return []Source{{
			BundleURL: bundleURL,
			FromMap:   false,
			Content:   body,
			SizeBytes: len(body),
		}}, nil
	}

	mapBody, err := fetchBody(ctx, opt.Client, opt.UserAgent, mapURL, opt.MaxScriptBytes)
	if err != nil {
		// map advertised but unreachable — degrade gracefully.
		return []Source{{
			BundleURL: bundleURL,
			FromMap:   false,
			Content:   body,
			SizeBytes: len(body),
		}}, nil
	}

	// Some maps start with the )]}' XSSI prefix; strip it.
	mapBody = strings.TrimPrefix(strings.TrimSpace(mapBody), ")]}'")

	var sm sourceMap
	if err := json.Unmarshal([]byte(mapBody), &sm); err != nil {
		return []Source{{
			BundleURL: bundleURL,
			FromMap:   false,
			Content:   body,
			SizeBytes: len(body),
		}}, nil
	}

	if len(sm.SourcesContent) == 0 {
		return []Source{{
			BundleURL: bundleURL,
			FromMap:   false,
			Content:   body,
			SizeBytes: len(body),
		}}, nil
	}

	out := make([]Source, 0, len(sm.SourcesContent))
	for i, content := range sm.SourcesContent {
		if content == "" {
			continue
		}
		path := ""
		if i < len(sm.Sources) {
			path = sm.Sources[i]
		}
		out = append(out, Source{
			BundleURL: bundleURL,
			FilePath:  path,
			FromMap:   true,
			Content:   content,
			SizeBytes: len(content),
		})
	}
	if len(out) == 0 {
		out = append(out, Source{
			BundleURL: bundleURL,
			FromMap:   false,
			Content:   body,
			SizeBytes: len(body),
		})
	}
	return out, nil
}

// RecoverAll runs Recover across every bundleURL with bounded concurrency.
// Failed bundles are silently dropped — partial recovery is the common case.
func RecoverAll(ctx context.Context, bundleURLs []string, opt RecoverOptions) []Source {
	opt.normalize()
	if len(bundleURLs) > opt.MaxScripts {
		bundleURLs = bundleURLs[:opt.MaxScripts]
	}

	sem := make(chan struct{}, opt.MaxConcurrency)
	var (
		mu  sync.Mutex
		all []Source
		wg  sync.WaitGroup
	)
	for _, b := range bundleURLs {
		b := b
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			srcs, err := Recover(ctx, b, opt)
			if err != nil {
				return
			}
			mu.Lock()
			all = append(all, srcs...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	return all
}

// RecoverFromPage convenience wrapper: fetches html (caller may have it
// already and pass via Source.Content workaround), but this variant takes a
// pre-fetched body and a base URL.
func RecoverFromPage(ctx context.Context, pageHTML string, pageURL *url.URL, opt RecoverOptions) []Source {
	bundles := ExtractScriptURLs(pageHTML, pageURL)
	return RecoverAll(ctx, bundles, opt)
}

func fetchBody(ctx context.Context, client *http.Client, ua, target string, cap int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return "", err
	}
	if ua != "" {
		req.Header.Set("User-Agent", ua)
	}
	req.Header.Set("Accept", "application/javascript, application/json, */*;q=0.5")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, cap+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > cap {
		data = data[:cap]
	}
	return string(data), nil
}

// findMapURL inspects the trailing sourceMappingURL comment. Returns "" if
// none was advertised. We do NOT probe "<bundle>.map" speculatively to avoid
// unnecessary 404 traffic.
func findMapURL(body, bundleURL string) string {
	// Walk backwards through the last 4KB — the comment is at the tail.
	tail := body
	if len(tail) > 4096 {
		tail = tail[len(tail)-4096:]
	}
	m := sourceMappingURLRe.FindStringSubmatch(tail)
	if len(m) < 2 {
		return ""
	}
	mapRef := strings.TrimSpace(m[1])
	if mapRef == "" || strings.HasPrefix(mapRef, "data:") {
		return ""
	}
	base, err := url.Parse(bundleURL)
	if err != nil {
		return ""
	}
	ref, err := url.Parse(mapRef)
	if err != nil {
		return ""
	}
	return base.ResolveReference(ref).String()
}

// ErrNotJS is returned when the response body does not look like JavaScript
// or JSON. Currently unused but reserved for stricter callers.
var ErrNotJS = errors.New("response body is not JavaScript or JSON")
