package logx

import "testing"

// FuzzRedactString runs the redaction patterns over arbitrary text. They must not
// panic, and redacting twice must change nothing: if the output of one pass could
// still be matched, a value could be half redacted.
func FuzzRedactString(f *testing.F) {
	for _, s := range []string{
		"Authorization: Bearer abcdefgh12345678", "https://user:pw@host/x", "GET /v1?api_key=abc&x=1",
		"token=token=abc", "password=", "://:@", "Bearer Bearer Bearer abcdefghij", "a=b&token=&c=d", "[redacted]",
		"https://[redacted]@host", "api-key=" + Redacted,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		once := RedactString(s)
		// The pre-check that skips the patterns must never skip one that would
		// have changed the string.
		if full := redactAll(s); once != full {
			t.Fatalf("the fast path disagrees with the full patterns:\n in:   %q\n fast: %q\n full: %q", s, once, full)
		}
		if twice := RedactString(once); twice != once {
			t.Fatalf("not idempotent:\n in:    %q\n once:  %q\n twice: %q", s, once, twice)
		}
	})
}
