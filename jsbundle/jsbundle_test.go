package jsbundle

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestExtractScriptURLs_AbsoluteAndRelative(t *testing.T) {
	base, _ := url.Parse("https://example.com/page")
	html := `
	<html><body>
	  <script src="/static/app.js"></script>
	  <script src="https://cdn.example.com/lib.js"></script>
	  <script src='./inline.js'></script>
	  <script>console.log('inline-no-src')</script>
	  <script src="data:text/javascript;base64,Zm9v"></script>
	  <script SRC="/UPPER.js"></script>
	</body></html>`
	got := ExtractScriptURLs(html, base)
	want := []string{
		"https://example.com/static/app.js",
		"https://cdn.example.com/lib.js",
		"https://example.com/inline.js",
		"https://example.com/UPPER.js",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d urls, want %d: %v", len(got), len(want), got)
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("url[%d] = %s, want %s", i, got[i], w)
		}
	}
}

func TestExtractScriptURLs_Dedup(t *testing.T) {
	base, _ := url.Parse("https://example.com/")
	html := `<script src="/a.js"></script><script src="/a.js"></script>`
	got := ExtractScriptURLs(html, base)
	if len(got) != 1 {
		t.Fatalf("expected 1 deduped url, got %d", len(got))
	}
}

func TestFindMapURL(t *testing.T) {
	body := "function x(){return 1;}\n//# sourceMappingURL=app.js.map"
	got := findMapURL(body, "https://example.com/static/app.js")
	want := "https://example.com/static/app.js.map"
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestFindMapURL_AtForm(t *testing.T) {
	body := "var a=1;\n//@ sourceMappingURL=/maps/x.map"
	got := findMapURL(body, "https://example.com/static/app.js")
	want := "https://example.com/maps/x.map"
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestFindMapURL_None(t *testing.T) {
	body := "function x(){return 1;}"
	got := findMapURL(body, "https://example.com/static/app.js")
	if got != "" {
		t.Errorf("expected empty, got %s", got)
	}
}

func TestRecover_WithSourceMap(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/app.js", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("var x=1;\n//# sourceMappingURL=app.js.map\n"))
	})
	mux.HandleFunc("/app.js.map", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "version":3,
		  "sources":["webpack:///./src/index.js","webpack:///./src/secret.js"],
		  "sourcesContent":["console.log('index')","const API_KEY='sk_live_TOTALLYREAL';"]
		}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	srcs, err := Recover(context.Background(), srv.URL+"/app.js", RecoverOptions{Client: srv.Client()})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(srcs) != 2 {
		t.Fatalf("expected 2 sources, got %d", len(srcs))
	}
	if !srcs[0].FromMap || !srcs[1].FromMap {
		t.Errorf("expected FromMap=true for both")
	}
	if !strings.Contains(srcs[1].Content, "sk_live_TOTALLYREAL") {
		t.Errorf("missing recovered content: %q", srcs[1].Content)
	}
	if srcs[1].FilePath != "webpack:///./src/secret.js" {
		t.Errorf("wrong filepath: %s", srcs[1].FilePath)
	}
}

func TestRecover_NoMapFalsbackToBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/x.js", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("var s='AKIA12345678901234XX';"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srcs, err := Recover(context.Background(), srv.URL+"/x.js", RecoverOptions{Client: srv.Client()})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(srcs) != 1 {
		t.Fatalf("expected 1 source, got %d", len(srcs))
	}
	if srcs[0].FromMap {
		t.Errorf("expected FromMap=false fallback")
	}
	if !strings.Contains(srcs[0].Content, "AKIA") {
		t.Errorf("expected fallback body content")
	}
}

func TestRecover_MapAdvertisedButMissing(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/app.js", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("var a=1;\n//# sourceMappingURL=app.js.map\n"))
	})
	// no /app.js.map handler — will 404
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srcs, err := Recover(context.Background(), srv.URL+"/app.js", RecoverOptions{Client: srv.Client()})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if len(srcs) != 1 || srcs[0].FromMap {
		t.Errorf("expected fallback to bundle body, got %+v", srcs)
	}
}

func TestRecoverAll_ConcurrencyCap(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("var a=1;"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	urls := []string{srv.URL + "/a.js", srv.URL + "/b.js", srv.URL + "/c.js"}
	srcs := RecoverAll(context.Background(), urls, RecoverOptions{Client: srv.Client(), MaxConcurrency: 2})
	if len(srcs) != 3 {
		t.Fatalf("expected 3 sources, got %d", len(srcs))
	}
}

func TestRecoverFromPage(t *testing.T) {
	jsmux := http.NewServeMux()
	jsmux.HandleFunc("/main.js", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("var leaked='ghp_FAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKEFAKE';"))
	})
	jsSrv := httptest.NewServer(jsmux)
	defer jsSrv.Close()

	pageURL, _ := url.Parse(jsSrv.URL + "/")
	html := `<html><body><script src="/main.js"></script></body></html>`

	srcs := RecoverFromPage(context.Background(), html, pageURL, RecoverOptions{Client: jsSrv.Client()})
	if len(srcs) != 1 {
		t.Fatalf("expected 1 source, got %d", len(srcs))
	}
	if !strings.Contains(srcs[0].Content, "ghp_FAKE") {
		t.Errorf("missing expected content")
	}
}
