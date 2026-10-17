package env

import "testing"

// FuzzValidate checks the registry against arbitrary environments. Validate and
// Snapshot must not panic, and a secret's value must not influence anything they
// return: that is the property the whole redaction design stands on.
//
// Searching the output for the value would misfire, because a short value can be a
// substring of the constant error text ("ot a n" in "not a number"). So the check
// is structural: the snapshot shows exactly the redaction marker or nothing, and
// the problem messages for a secret are identical whatever the value was.
func FuzzValidate(f *testing.F) {
	f.Add("hunter2-value", uint8(0))
	f.Add("not a number", uint8(2))
	f.Add("https://user:pass@host", uint8(0))
	f.Add("=", uint8(4))
	f.Add("", uint8(1))
	kinds := []Kind{KindString, KindBool, KindInt, KindFloat, KindDuration, KindList}
	f.Fuzz(func(t *testing.T, value string, k uint8) {
		r := NewRegistry()
		r.Declare(Var{Name: "FUZZ_SECRET", Kind: kinds[int(k)%len(kinds)], Secret: true, Default: "default-secret"})
		r.Declare(Var{Name: "FUZZ_PLAIN", Kind: kinds[int(k)%len(kinds)]})
		r.Removed("FUZZ_OLD", "v1", "gone")

		messages := func(v string) (msgs []string) {
			environ := []string{"FUZZ_SECRET=" + v, "FUZZ_PLAIN=" + v, "FUZZ_OLD=" + v, "FUZZ_SECERT=" + v, v}
			for _, p := range r.Validate(environ) {
				if p.Name == "FUZZ_SECRET" {
					msgs = append(msgs, p.Message)
				}
			}
			for _, i := range r.Snapshot(environ) {
				if i.Name == "FUZZ_SECRET" {
					if i.Value != "" && i.Value != Redacted {
						t.Fatalf("snapshot value of a secret = %q, want empty or the redaction marker", i.Value)
					}
					if i.Default != "" {
						t.Fatalf("a secret's default was shown: %q", i.Default)
					}
				}
			}
			return msgs
		}

		a := messages(value)
		b := messages(value + "\x00") // a different value; may or may not parse
		if len(a) == 1 && len(b) == 1 && a[0] != b[0] {
			t.Fatalf("the messages for a secret depend on its value:\n %q\n %q", a[0], b[0])
		}
	})
}
