package botwall

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// detectPlain is Detect before the prefix check: the definition the faster form must match.
func detectPlain(html string) string {
	if html == "" || len(html) > MaxBody {
		return ""
	}
	l := strings.ToLower(html)
	vis := visibleTextLen(l)
	if vis > MaxVisibleText {
		return ""
	}
	has := func(subs ...string) bool {
		for _, s := range subs {
			if !strings.Contains(l, s) {
				return false
			}
		}
		return true
	}
	switch {
	case has("captcha-delivery.com", "var dd="):
		return "datadome"
	case cloudflareChallenge(l):
		return "cloudflare"
	case strings.Contains(l, "px-captcha") || strings.Contains(l, "captcha.px-cdn.net"):
		return "perimeterx"
	case has("<title>access denied</title>", "edgesuite"):
		return "akamai"
	case impervaChallenge(l, vis):
		return "imperva"
	case awsWAFChallenge(l):
		return "aws_waf"
	case has("sucuri website firewall", "access denied"):
		return "sucuri"
	case strings.Contains(l, "our systems have detected unusual traffic from your computer network"):
		return "google_sorry"
	}
	return ""
}

// pieces are fragments that stress the scanner at a cut: tags and script
// bodies that can straddle it, runes whose lower case has another length,
// runes that lower-case to ASCII, and invalid bytes.
var pieces = []string{
	"word ", "Text ", "<p>", "</p>", "<div class=x>", "<script>var a=1;</script>", "<SCRIPT>x</SCRIPT>", "<style>a{}</style>",
	"<script>", "</script>", "<style>", "<", ">", "<scrİpt>", "</scrİpt>", "K", "İ", "Ω", "Å",
	"é", "ß", "ș", "中文", "\xff", "\xc3", "\x80\x80\x80\x80\x80", "\xe2\x82", " \t\n", "<!-- c -->", "&nbsp;",
	strings.Repeat("x", 300), strings.Repeat("<script>", 3), "<style>" + strings.Repeat("y", 500), strings.Repeat(" ", 200),
	fixCloudflareWait, fixDataDome, fixImperva, fixAWSWAF,
}

func generated(r *rand.Rand, size int) string {
	var b strings.Builder
	for b.Len() < size {
		b.WriteString(pieces[r.Intn(len(pieces))])
	}
	return b.String()
}

// The prefix count may fall short of the whole document's, never exceed it.
func TestPrefixCountNeverExceedsTheDocuments(t *testing.T) {
	r := rand.New(rand.NewSource(20261125))
	for i := 0; i < 3000; i++ {
		doc := generated(r, 1+r.Intn(20000))
		full := visibleTextLen(strings.ToLower(doc))
		for k := 0; k < 8; k++ {
			cut := r.Intn(len(doc) + 1)
			for cut > 0 && cut < len(doc) && doc[cut]&0xC0 == 0x80 {
				cut--
			}
			if got := visibleTextLen(strings.ToLower(doc[:cut])); got > full {
				t.Fatalf("doc %d cut %d: prefix counts %d, whole document %d", i, cut, got, full)
			}
		}
	}
}

func TestDetectMatchesThePlainForm(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	var docs []string
	for _, f := range []string{fixDataDome, fixCloudflareWait, fixCloudflareBlock, fixPerimeterX, fixAkamai, fixImperva, fixAWSWAF, fixSucuri, fixGoogleSorry} {
		docs = append(docs, f)
		// A wall behind a long head: the probes see no visible text and must not decide.
		docs = append(docs, "<html><head><script>"+strings.Repeat("v", 70<<10)+"</script></head>"+f)
	}
	for _, size := range []int{100, 16 << 10, 16<<10 + 1, 64 << 10, 64<<10 + 3, 100 << 10, MaxBody} {
		for i := 0; i < 40; i++ {
			docs = append(docs, generated(r, size))
		}
		// Multi-byte runes and invalid bytes placed across both probe cuts.
		for shift := 0; shift < 5; shift++ {
			for _, rune := range []string{"中", "K", "İ", "\xff", "\xe2\x82"} {
				pad := strings.Repeat("a ", 3000) + strings.Repeat("<b>", 1)
				doc := pad + strings.Repeat("-", (16<<10)-len(pad)-shift) + strings.Repeat(rune, 4) + strings.Repeat(" z", 30000)
				if len(doc) > size {
					doc = doc[:size]
				}
				docs = append(docs, doc)
			}
		}
	}
	for i, doc := range docs {
		if got, want := Detect(doc), detectPlain(doc); got != want {
			t.Fatalf("doc %d (%d bytes): Detect %q, plain %q", i, len(doc), got, want)
		}
	}
}

func FuzzDetectMatchesThePlainForm(f *testing.F) {
	for _, s := range []string{fixDataDome, fixCloudflareWait, strings.Repeat("a <p>", 5000), "<script>" + strings.Repeat("x", 70000)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, html string) {
		if got, want := Detect(html), detectPlain(html); got != want {
			t.Fatalf("Detect %q, plain %q", got, want)
		}
	})
}

func BenchmarkDetectOrdinaryPage(b *testing.B) {
	r := rand.New(rand.NewSource(3))
	words := strings.Fields("acasa despre noi servicii contact produse firma clienti echipa proiecte cariere home about services")
	var sb strings.Builder
	sb.WriteString("<html><head><title>Firma</title><script>" + strings.Repeat("var x=1;", 400) + "</script></head><body>")
	for sb.Len() < 120<<10 {
		fmt.Fprintf(&sb, "<p class=c%d>", r.Intn(20))
		for n := 4 + r.Intn(10); n > 0; n-- {
			sb.WriteString(words[r.Intn(len(words))] + " ")
		}
		sb.WriteString("</p>\n")
	}
	page := sb.String()
	for _, bench := range []struct {
		name string
		f    func(string) string
	}{{"plain", detectPlain}, {"prefix", Detect}} {
		b.Run(bench.name, func(b *testing.B) {
			b.SetBytes(int64(len(page)))
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if bench.f(page) != "" {
					b.Fatal("an ordinary page was flagged")
				}
			}
		})
	}
}
