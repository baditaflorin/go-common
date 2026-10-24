package botwall

import (
	"strings"
	"testing"
)

// FuzzDetect throws arbitrary documents at the detector. It must terminate (the fuzzer treats a hang as a failure), never
// panic on malformed markup (unterminated tags, stray '<', huge runs of '<'), answer only a listed vendor or "", answer ""
// for anything over MaxBody, and agree with itself on a second call.
func FuzzDetect(f *testing.F) {
	for _, s := range []string{
		fixDataDome, fixCloudflareWait, fixCloudflareBlock, fixPerimeterX, fixAkamai, fixImperva, fixAWSWAF, fixSucuri, fixGoogleSorry,
		"", "<", "<<<<", "<script", "<script>", "<style>x", "</script>", "<scr<ipt>", "text<unterminated",
		"<script>" + strings.Repeat("<", 64), strings.Repeat("<script>", 32),
	} {
		f.Add(s)
	}
	listed := map[string]bool{"": true}
	for _, v := range Vendors() {
		listed[v] = true
	}
	f.Fuzz(func(t *testing.T, html string) {
		got := Detect(html)
		if !listed[got] {
			t.Fatalf("Detect returned %q, which is not in Vendors()", got)
		}
		if len(html) > MaxBody && got != "" {
			t.Fatalf("a document of %d bytes (> MaxBody) was flagged as %q", len(html), got)
		}
		if again := Detect(html); again != got {
			t.Fatalf("Detect is not deterministic: %q then %q", got, again)
		}
	})
}
