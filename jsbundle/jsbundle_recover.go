package jsbundle

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
)

// Recover downloads bundleURL and any associated .map, returning one Source
// per original file when a source map is available, or a single Source with
// the raw bundle body otherwise. With no opt.Client it uses a shared safehttp
// client; if that cannot be built (FLEET_REQUIRE_PROXY=1 with no proxy
// variable set) it returns the error and fetches nothing.
func Recover(ctx context.Context, bundleURL string, opt RecoverOptions) ([]Source, error) {
	if err := opt.normalize(); err != nil {
		return nil, err
	}
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
// If the default client cannot be built (see Recover) it logs the reason and
// returns nil.
func RecoverAll(ctx context.Context, bundleURLs []string, opt RecoverOptions) []Source {
	if err := opt.normalize(); err != nil {
		// No client, so nothing can be fetched. The signature has no error
		// to return, so say why the result is empty.
		log.Printf("jsbundle: RecoverAll: %v", err)
		return nil
	}
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
