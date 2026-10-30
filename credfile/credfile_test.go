package credfile

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	secretA = "ak_credfile_test_SECRET_A_0123456789"
	secretB = "ak_credfile_test_SECRET_B_9876543210"
	secretC = "ak_credfile_test_SECRET_C_5555555555"
)

// writeAtomic replaces path the way the contract says writers must: a
// temporary file in the same directory, then rename.
func writeAtomic(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := tmp.Chmod(mode); err != nil {
		t.Fatal(err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		t.Fatal(err)
	}
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// openFake builds an unregistered Source on a fake clock.
func openFake(t *testing.T, path string, opts Options) (*Source, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)}
	s := newSource("test_key", path, opts, clk.now)
	if err := s.load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	return s, clk
}

func keyPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "fleet_test_key")
}

func TestOpenReadsTrimmedValue(t *testing.T) {
	p := keyPath(t)
	writeAtomic(t, p, "  "+secretA+"\n", 0o400)
	s, err := Open("open_trim", p, Options{Protected: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Value(); got != secretA {
		t.Fatalf("Value = %q", got)
	}
	if s.Name() != "open_trim" || s.Path() != p {
		t.Fatalf("Name/Path = %q %q", s.Name(), s.Path())
	}
}

func TestOpenFailsAtStart(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name   string
		setup  func(p string)
		opts   Options
		reason string
	}{
		{"missing", func(string) {}, Options{}, ReasonMissing},
		{"empty", func(p string) { writeAtomic(t, p, " \n\t", 0o400) }, Options{}, ReasonEmpty},
		{"oversized", func(p string) { writeAtomic(t, p, strings.Repeat("a", 65), 0o400) }, Options{MaxBytes: 64}, ReasonOversized},
		{"group-readable", func(p string) { writeAtomic(t, p, secretA, 0o640) }, Options{Protected: true}, ReasonUnsafe},
		{"symlink", func(p string) {
			target := p + ".target"
			writeAtomic(t, target, secretA, 0o400)
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
		}, Options{Protected: true}, ReasonUnsafe},
		{"directory", func(p string) {
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
		}, Options{Protected: true}, ReasonUnsafe},
		{"invalid", func(p string) { writeAtomic(t, p, secretA, 0o400) },
			Options{Validate: func(v string) error { return fmt.Errorf("bad value %q", v) }}, ReasonInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, tc.name)
			tc.setup(p)
			s, err := Open("start_"+tc.name, p, tc.opts)
			if err == nil || s != nil {
				t.Fatalf("Open = %v, %v; want an error and no Source", s, err)
			}
			var e *Error
			if !errors.As(err, &e) || e.Reason != tc.reason {
				t.Fatalf("err = %v; want reason %s", err, tc.reason)
			}
			if strings.Contains(err.Error(), secretA) {
				t.Fatalf("error carries the value: %v", err)
			}
		})
	}
	if _, err := Open("start_missing2", filepath.Join(dir, "nope"), Options{}); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file error does not unwrap to fs.ErrNotExist: %v", err)
	}
	if _, err := Open("", "x", Options{}); err == nil {
		t.Fatal("Open with an empty name succeeded")
	}
}

func TestAtomicReplacePickedUpWithinInterval(t *testing.T) {
	p := keyPath(t)
	writeAtomic(t, p, secretA, 0o400)
	s, clk := openFake(t, p, Options{Protected: true, CheckInterval: 30 * time.Second})
	_, gen1 := s.Current()

	writeAtomic(t, p, secretB+"\n", 0o400)
	clk.advance(10 * time.Second)
	if got := s.Value(); got != secretA {
		t.Fatalf("before the interval Value = %q, want the old key", got)
	}
	clk.advance(21 * time.Second)
	got, gen2 := s.Current()
	if got != secretB {
		t.Fatalf("after the interval Value = %q, want the new key", got)
	}
	if gen2 == gen1 {
		t.Fatal("generation did not change with the value")
	}
	st := s.Stats()
	if st.ReloadOK != 1 || st.ReloadError != 0 || st.Stale || st.StaleUses != 0 {
		t.Fatalf("stats = %+v", st)
	}
	// Nothing changed: later looks neither re-read nor count.
	clk.advance(time.Minute)
	if s.Value() != secretB || s.Stats().ReloadOK != 1 {
		t.Fatalf("an unchanged file was re-read: %+v", s.Stats())
	}
}

func TestCheckIntervalCapped(t *testing.T) {
	p := keyPath(t)
	writeAtomic(t, p, secretA, 0o400)
	s, clk := openFake(t, p, Options{CheckInterval: time.Hour})
	writeAtomic(t, p, secretB, 0o400)
	clk.advance(MaxCheckInterval)
	if got := s.Value(); got != secretB {
		t.Fatalf("a renewed key was not seen within %s: %q", MaxCheckInterval, got)
	}
}

func TestMissingFileKeepsLastGood(t *testing.T) {
	p := keyPath(t)
	writeAtomic(t, p, secretA, 0o400)
	s, clk := openFake(t, p, Options{Protected: true})

	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	clk.advance(DefaultCheckInterval)
	for i := 0; i < 3; i++ {
		if got := s.Value(); got != secretA {
			t.Fatalf("Value with the file missing = %q, want the last good key", got)
		}
	}
	st := s.Stats()
	if !st.Stale || st.StaleUses != 3 || st.ReloadError != 1 || st.Errors[ReasonMissing] != 1 {
		t.Fatalf("stats with the file missing = %+v", st)
	}

	// Still missing at the next look: one more error.
	clk.advance(DefaultCheckInterval)
	s.Value()
	if st := s.Stats(); st.ReloadError != 2 || st.Errors[ReasonMissing] != 2 {
		t.Fatalf("second look: %+v", st)
	}

	// The renewed file appears: it is used, and nothing is stale any more.
	writeAtomic(t, p, secretB, 0o400)
	clk.advance(DefaultCheckInterval)
	if got := s.Value(); got != secretB {
		t.Fatalf("Value after the file came back = %q", got)
	}
	if st := s.Stats(); st.Stale || st.StaleUses != 4 || st.ReloadOK != 1 {
		t.Fatalf("stats after recovery = %+v", st)
	}
}

func TestBadContentKeepsLastGoodAndIsNotReread(t *testing.T) {
	p := keyPath(t)
	writeAtomic(t, p, secretA, 0o400)
	s, clk := openFake(t, p, Options{Validate: func(v string) error {
		if !strings.HasPrefix(v, "ak_") {
			return errors.New("prefix")
		}
		return nil
	}})
	writeAtomic(t, p, "not-a-fleet-key", 0o400)
	clk.advance(DefaultCheckInterval)
	if got := s.Value(); got != secretA {
		t.Fatalf("Value = %q, want the last good key", got)
	}
	clk.advance(DefaultCheckInterval)
	s.Value()
	if st := s.Stats(); st.Errors[ReasonInvalid] != 1 || !st.Stale || st.StaleUses != 2 {
		t.Fatalf("an unchanged bad file should count once and stay stale: %+v", st)
	}
	writeAtomic(t, p, "", 0o400)
	clk.advance(DefaultCheckInterval)
	s.Value()
	if st := s.Stats(); st.Errors[ReasonEmpty] != 1 {
		t.Fatalf("empty replacement not counted: %+v", st)
	}
}

func TestUnsafePermissionsAfterStart(t *testing.T) {
	p := keyPath(t)
	writeAtomic(t, p, secretA, 0o400)
	s, clk := openFake(t, p, Options{Protected: true})
	writeAtomic(t, p, secretB, 0o644)
	clk.advance(DefaultCheckInterval)
	if got := s.Value(); got != secretA {
		t.Fatalf("a world-readable replacement was used: %q", got)
	}
	if st := s.Stats(); st.Errors[ReasonUnsafe] != 1 || !st.Stale {
		t.Fatalf("stats = %+v", st)
	}
}

func TestRejectedReloadsOnceAndReportsChange(t *testing.T) {
	p := keyPath(t)
	writeAtomic(t, p, secretA, 0o400)
	s, clk := openFake(t, p, Options{})

	// The file holds the key that was refused: nothing better, no retry.
	if v, changed := s.Rejected(secretA); changed || v != secretA {
		t.Fatalf("Rejected with an unchanged file = %q, %v", v, changed)
	}

	// The renewal landed just now, inside the check interval: a 401 picks it
	// up at once, but only after the reject interval has passed.
	writeAtomic(t, p, secretB, 0o400)
	if v, changed := s.Rejected(secretA); changed || v != secretA {
		t.Fatalf("a second forced re-read inside RejectInterval happened: %q, %v", v, changed)
	}
	clk.advance(DefaultRejectInterval)
	if v, changed := s.Rejected(secretA); !changed || v != secretB {
		t.Fatalf("Rejected after a renewal = %q, %v; want the new key and changed", v, changed)
	}
	// A caller that already holds the new key and is refused again gets no
	// "changed": the retry is bounded to one.
	if _, changed := s.Rejected(secretB); changed {
		t.Fatal("Rejected reported a change for the current key")
	}
	if st := s.Stats(); st.ReloadOK != 2 {
		t.Fatalf("forced re-reads: %+v", st)
	}
}

func TestReloadOff(t *testing.T) {
	for _, v := range []string{"off", "false", "0", "no", "disabled", " OFF "} {
		t.Run(v, func(t *testing.T) {
			t.Setenv(ReloadEnv, v)
			p := keyPath(t)
			writeAtomic(t, p, secretA, 0o400)
			s, clk := openFake(t, p, Options{})
			writeAtomic(t, p, secretB, 0o400)
			clk.advance(time.Hour)
			if got := s.Value(); got != secretA {
				t.Fatalf("reload off: Value = %q", got)
			}
			if got, changed := s.Rejected(secretA); changed || got != secretA {
				t.Fatalf("reload off: Rejected = %q, %v", got, changed)
			}
			if st := s.Stats(); st.ReloadOK != 0 || st.ReloadError != 0 {
				t.Fatalf("reload off looked at the file: %+v", st)
			}
		})
	}
	for _, v := range []string{"", "on", "true", "1", "yes", "enabled", "bogus"} {
		t.Setenv(ReloadEnv, v)
		if !ReloadEnabled() {
			t.Fatalf("%q turned reloading off", v)
		}
	}
	if _, ok := reloadSetting("bogus"); ok {
		t.Fatal("bogus accepted as a known setting")
	}
}

func writeMeta(t *testing.T, keyFile string, m Meta) {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	writeAtomic(t, keyFile+MetaSuffix, string(b), 0o644)
}

func TestMetaExpiry(t *testing.T) {
	p := keyPath(t)
	writeAtomic(t, p, secretA, 0o400)
	expA := time.Date(2027, 1, 29, 12, 0, 0, 0, time.UTC)
	writeMeta(t, p, Meta{KeyID: "k_a", ExpiresAt: expA, KeySHA256: KeySHA256(secretA)})
	s, clk := openFake(t, p, Options{Protected: true})
	if exp, ok := s.Expiry(); !ok || !exp.Equal(expA) {
		t.Fatalf("Expiry = %v %v", exp, ok)
	}

	// Renewal: key first, then the sidecar (the contract's order). A look in
	// between sees a mismatch; it is not an error unless it lasts.
	expB := expA.Add(90 * 24 * time.Hour)
	writeAtomic(t, p, secretB, 0o400)
	clk.advance(DefaultCheckInterval)
	if s.Value() != secretB {
		t.Fatal("renewed key not read")
	}
	if _, ok := s.Expiry(); ok {
		t.Fatal("the old key's expiry is still reported for the new key")
	}
	writeMeta(t, p, Meta{KeyID: "k_b", ExpiresAt: expB, SuccessorOf: "k_a", KeySHA256: KeySHA256(secretB)})
	clk.advance(DefaultCheckInterval)
	s.Value()
	if exp, ok := s.Expiry(); !ok || !exp.Equal(expB) {
		t.Fatalf("Expiry after the sidecar = %v %v", exp, ok)
	}
	if st := s.Stats(); len(st.Errors) != 0 || !st.Expiry.Equal(expB) {
		t.Fatalf("a normal renewal counted errors: %+v", st)
	}

	// A sidecar that stays wrong is counted once.
	writeMeta(t, p, Meta{ExpiresAt: expB, KeySHA256: KeySHA256(secretC)})
	for i := 0; i < 4; i++ {
		clk.advance(DefaultCheckInterval)
		s.Value()
	}
	if st := s.Stats(); st.Errors[ReasonMetaMismatch] != 1 || !st.Expiry.IsZero() {
		t.Fatalf("lasting mismatch: %+v", st)
	}

	// Garbage sidecar: counted, key unaffected.
	writeAtomic(t, p+MetaSuffix, "{not json", 0o644)
	clk.advance(DefaultCheckInterval)
	if s.Value() != secretB {
		t.Fatal("a bad sidecar changed the key")
	}
	if st := s.Stats(); st.Errors[ReasonMeta] != 1 || st.Stale {
		t.Fatalf("bad sidecar: %+v", st)
	}

	// No sidecar at all: no expiry, no error.
	if err := os.Remove(p + MetaSuffix); err != nil {
		t.Fatal(err)
	}
	clk.advance(DefaultCheckInterval)
	s.Value()
	if _, ok := s.Expiry(); ok {
		t.Fatal("expiry without a sidecar")
	}
	if st := s.Stats(); st.Errors[ReasonMeta] != 1 {
		t.Fatalf("a missing sidecar counted as an error: %+v", st)
	}
}

func TestMetaWithoutDigestIsTrusted(t *testing.T) {
	p := keyPath(t)
	writeAtomic(t, p, secretA, 0o400)
	writeAtomic(t, p+MetaSuffix, `{"expires_at": 1800000000}`, 0o644)
	s, _ := openFake(t, p, Options{})
	if exp, ok := s.Expiry(); !ok || exp.Unix() != 1800000000 {
		t.Fatalf("Expiry = %v %v", exp, ok)
	}
}

func TestParseMeta(t *testing.T) {
	good := []struct {
		in   string
		want time.Time
	}{
		{`{"schema":1,"key_id":"k","expires_at":"2027-01-29T12:00:00Z"}`, time.Date(2027, 1, 29, 12, 0, 0, 0, time.UTC)},
		{`{"expires_at":"2027-01-29T14:00:00+02:00"}`, time.Date(2027, 1, 29, 12, 0, 0, 0, time.UTC)},
		{`{"expires_at":1800000000}`, time.Unix(1800000000, 0).UTC()},
		{`{"expires_at":null}`, time.Time{}},
		{`{"expires_at":""}`, time.Time{}},
		{`{"expires_at":0}`, time.Time{}},
		{`{"future_field":{"x":1}}`, time.Time{}},
	}
	for _, g := range good {
		m, err := ParseMeta([]byte(g.in))
		if err != nil {
			t.Fatalf("%s: %v", g.in, err)
		}
		if !m.ExpiresAt.Equal(g.want) || m.Schema != MetaSchema && !strings.Contains(g.in, "schema") {
			t.Fatalf("%s: %+v", g.in, m)
		}
	}
	for _, bad := range []string{
		``, `null`, `[]`, `"x"`, `{`, `{"expires_at":"tomorrow"}`, `{"expires_at":-5}`,
		`{"expires_at":true}`, `{"key_sha256":"abc"}`, `{"key_sha256":"` + strings.Repeat("g", 64) + `"}`,
	} {
		if _, err := ParseMeta([]byte(bad)); err == nil {
			t.Fatalf("ParseMeta(%q) accepted", bad)
		}
	}

	in := Meta{KeyID: "k_1", ExpiresAt: time.Date(2027, 1, 29, 14, 0, 0, 0, time.FixedZone("x", 7200)),
		IssuedAt: time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), SuccessorOf: "k_0", KeySHA256: strings.ToUpper(KeySHA256(secretA))}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(`"expires_at":"2027-01-29T12:00:00Z"`)) || bytes.Contains(b, []byte("renew_after")) || !bytes.Contains(b, []byte(`"schema":1`)) {
		t.Fatalf("canonical form: %s", b)
	}
	var out Meta
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !out.ExpiresAt.Equal(in.ExpiresAt) || out.KeyID != "k_1" || out.SuccessorOf != "k_0" || out.KeySHA256 != KeySHA256(secretA) {
		t.Fatalf("round trip: %+v", out)
	}
	if bytes.Contains(b, []byte(secretA)) {
		t.Fatal("the sidecar carries the key")
	}
}

// TestValueNeverLogged drives every path (start, reload, missing, invalid,
// reject, stats) with log output captured and checks that no rendering of
// anything this package returns or prints contains the value.
func TestValueNeverLogged(t *testing.T) {
	var logBuf bytes.Buffer
	prev, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prev); log.SetFlags(prevFlags) })

	p := keyPath(t)
	writeAtomic(t, p, secretA, 0o600)
	writeMeta(t, p, Meta{KeyID: "k_a", ExpiresAt: time.Now().Add(time.Hour), KeySHA256: KeySHA256(secretA)})
	s, err := Open("never_logged", p, Options{Protected: true, CheckInterval: time.Nanosecond, RejectInterval: time.Nanosecond,
		Validate: func(v string) error {
			if v == "ak_reject_me" {
				return fmt.Errorf("refusing %q", v)
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	var rendered []string
	add := func(v ...any) {
		for _, x := range v {
			rendered = append(rendered, fmt.Sprintf("%v|%+v|%#v|%s", x, x, x, x))
		}
	}
	add(s, s.Stats())
	writeAtomic(t, p, secretB, 0o600)
	s.Value()
	add(s.Stats())
	writeAtomic(t, p, "ak_reject_me", 0o600)
	s.Value()
	add(s.Stats())
	_ = os.Remove(p)
	s.Value()
	_, changed := s.Rejected(secretA)
	add(s, s.Stats(), changed, Snapshot())
	for _, bad := range []Options{{Protected: true}, {Validate: func(v string) error { return errors.New(v) }}} {
		writeAtomic(t, p, secretC, 0o644)
		_, err := Open("never_logged_bad", p, bad)
		add(err)
	}

	all := strings.Join(rendered, "\n") + logBuf.String()
	for _, v := range []string{secretA, secretB, secretC, "ak_reject_me"} {
		if strings.Contains(all, v) {
			t.Fatalf("a credential value leaked into output:\n%s", all)
		}
	}
}

func TestOpenSharesSourcesAndSnapshot(t *testing.T) {
	p := keyPath(t)
	writeAtomic(t, p, secretA, 0o400)
	a, err := Open("shared_key", p, Options{Protected: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open("shared_key", p, Options{Protected: true})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("a second Open of the same file returned another Source")
	}
	if c, _ := Open("shared_key", p, Options{}); c == a {
		t.Fatal("a Source with other options was shared")
	}
	found := false
	for _, st := range Snapshot() {
		if st.Name == "shared_key" && st.Path == p {
			found = true
		}
	}
	if !found {
		t.Fatal("Snapshot does not list the opened Source")
	}
}

func TestConcurrentUseDuringRenewal(t *testing.T) {
	p := keyPath(t)
	writeAtomic(t, p, secretA, 0o400)
	s, err := Open("concurrent_key", p, Options{Protected: true, CheckInterval: time.Microsecond, RejectInterval: time.Microsecond})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				v := s.Value()
				if v != secretA && v != secretB {
					t.Errorf("torn or foreign value read (len %d)", len(v))
					return
				}
				s.Rejected(v)
				_ = s.Stats()
			}
		}()
	}
	for i := 0; i < 50; i++ {
		if i%2 == 0 {
			writeAtomic(t, p, secretB, 0o400)
		} else {
			writeAtomic(t, p, secretA, 0o400)
		}
	}
	close(stop)
	wg.Wait()
}
