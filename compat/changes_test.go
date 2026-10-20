package compat

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestEmbeddedChangesAreConsistent(t *testing.T) {
	t.Parallel()
	f, err := LoadChanges()
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Changes) == 0 {
		t.Fatal("the feed holds no change")
	}
	for _, c := range f.Changes {
		if c.Class != "breaking" && c.Class != "behavioural" {
			t.Errorf("%s: class %q is not one the feed knows", c.Version, c.Class)
		}
		if (c.Class == "behavioural") != (c.Reason != "") {
			t.Errorf("%s: a reason belongs to a behavioural (patch-safe) change, and to it only", c.Version)
		}
		if c.Summary == "" || utf8.RuneCountInString(c.Summary) > 200 {
			t.Errorf("%s: summary must be 1 to 200 characters", c.Version)
		}
		if utf8.RuneCountInString(c.OffSwitch) > 200 {
			t.Errorf("%s: off_switch is longer than 200 characters", c.Version)
		}
		if strings.TrimSpace(c.Summary) != c.Summary || strings.TrimSpace(c.OffSwitch) != c.OffSwitch {
			t.Errorf("%s: a field has leading or trailing space", c.Version)
		}
		if d, _ := time.Parse(time.DateOnly, c.Date); d.After(time.Now().Add(48 * time.Hour)) {
			t.Errorf("%s: dated in the future", c.Version)
		}
	}
}

func TestParseChangesRejectsBadFeeds(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"not json":          `{`,
		"wrong schema":      `{"schema":2,"changes":[]}`,
		"no schema":         `{"changes":[]}`,
		"bad version":       `{"schema":1,"changes":[{"version":"0.1.0","date":"2026-01-01","class":"breaking","summary":"x"}]}`,
		"a pre-release":     `{"schema":1,"changes":[{"version":"v0.1.0-rc.1","date":"2026-01-01","class":"breaking","summary":"x"}]}`,
		"a pseudo-version":  `{"schema":1,"changes":[{"version":"v0.1.1-0.20260101000000-abcdefabcdef","date":"2026-01-01","class":"breaking","summary":"x"}]}`,
		"bad date":          `{"schema":1,"changes":[{"version":"v0.1.0","date":"yesterday","class":"breaking","summary":"x"}]}`,
		"an impossible day": `{"schema":1,"changes":[{"version":"v0.1.0","date":"2026-02-30","class":"breaking","summary":"x"}]}`,
	} {
		if _, err := ParseChanges([]byte(doc)); err == nil {
			t.Errorf("%s: ParseChanges accepted %s", name, doc)
		}
	}
}

// An older consumer has to be able to read a newer feed: fields and classes it does
// not know are ignored, not refused.
func TestParseChangesToleratesANewerFeed(t *testing.T) {
	t.Parallel()
	f, err := ParseChanges([]byte(`{"schema":1,"future":{"a":1},"changes":[
		{"version":"v0.9.0","date":"2026-01-01","class":"a-class-from-the-future","qualifier":"q","summary":"s","off_switch":"o","affects":["x"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Changes) != 1 || f.Changes[0].Class != "a-class-from-the-future" || f.Changes[0].OffSwitch != "o" {
		t.Fatalf("got %+v", f.Changes)
	}
}

func feedOfVersions(vs ...string) ChangeFeed {
	f := ChangeFeed{Schema: 1}
	for _, v := range vs {
		f.Changes = append(f.Changes, Change{Version: v, Date: "2026-01-01", Class: "breaking", Summary: v})
	}
	return f
}

func versionsOf(cs []Change) string {
	var vs []string
	for _, c := range cs {
		vs = append(vs, c.Version)
	}
	return strings.Join(vs, " ")
}

func TestBetween(t *testing.T) {
	t.Parallel()
	// Newest first, two changes in one release, as in the real feed.
	f := feedOfVersions("v0.105.0", "v0.104.0", "v0.103.0", "v0.102.70", "v0.102.70", "v0.102.60", "v0.63.0")
	for _, tc := range []struct{ from, to, want string }{
		{"v0.102.60", "v0.104.0", "v0.104.0 v0.103.0 v0.102.70 v0.102.70"}, // from is exclusive, to is inclusive
		{"v0.102.70", "", "v0.105.0 v0.104.0 v0.103.0"},
		{"v0.105.0", "", ""},
		{"", "v0.102.60", "v0.102.60 v0.63.0"},
		{"", "", "v0.105.0 v0.104.0 v0.103.0 v0.102.70 v0.102.70 v0.102.60 v0.63.0"},
		{"v0.103.0", "v0.103.0", ""},
		{"v0.102.65", "v0.102.69", ""},                               // no release with a change in between
		{"v0.102.65", "v0.102.70", "v0.102.70 v0.102.70"},            // to counts itself
		{"v0.0.0-20260110171042-8921433476a9", "v0.63.0", "v0.63.0"}, // a pseudo-version of v0.0.0 sorts below everything
		{"v0.102.3-0.20261002000000-abcdefabcdef", "v0.102.60", "v0.102.60"},
		{"v0.102.60-0.20261002000000-abcdefabcdef", "v0.102.60", "v0.102.60"},          // a pre-release sorts below its own release: that release is included
		{"v0.102.60", "v0.103.0-0.20261002000000-abcdefabcdef", "v0.102.70 v0.102.70"}, // an upper pseudo-version stops before its own release
		{"v0.102.70+incompatible", "v0.103.0", "v0.103.0"},
	} {
		got, err := f.Between(tc.from, tc.to)
		if err != nil {
			t.Errorf("Between(%q, %q): %v", tc.from, tc.to, err)
			continue
		}
		if g := versionsOf(got); g != tc.want {
			t.Errorf("Between(%q, %q) = %q, want %q", tc.from, tc.to, g, tc.want)
		}
	}
	for _, bad := range [][2]string{{"banana", ""}, {"", "banana"}, {"0.1.0", ""}, {"v0.9.0", "v0.1.0"}, {"v1", "v2"}} {
		if _, err := f.Between(bad[0], bad[1]); err == nil {
			t.Errorf("Between(%q, %q) accepted its arguments", bad[0], bad[1])
		}
	}
}
