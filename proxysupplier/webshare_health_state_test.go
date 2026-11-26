package proxysupplier

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// healthPersister exposes a bare tracker as a WebshareHealthPersister.
type healthPersister struct{ h *webshareNetworkHealth }

func (p healthPersister) ExportHealthState(now time.Time) WebshareHealthState {
	return p.h.exportState(now)
}
func (p healthPersister) RestoreHealthState(st WebshareHealthState, now time.Time) (int, int) {
	return p.h.restoreState(st, now)
}

// tripASN records enough failures to quarantine asn at now.
func tripASN(h *webshareNetworkHealth, asn int, now time.Time) {
	for i := 0; i < webshareASNMinSamples; i++ {
		h.Record(asn, "AS name", "", "", false, now)
	}
}

func TestWebshareHealthStateRoundTripKeepsQuarantineWindowAndEscalation(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	src := newWebshareNetworkHealth()
	tripASN(src, 100, now)    // quarantined, 1 trip
	for i := 0; i < 12; i++ { // degraded: 4 of 12 fail
		src.Record(200, "Degraded", "", "", i%3 != 0, now)
	}
	src.Record(0, "", "10.0.0", "10.0.0.1", false, now)
	src.Record(0, "", "10.0.0", "10.0.0.2", false, now) // /24 cooldown
	st := src.exportState(now)

	raw, err := json.Marshal(st) // through JSON, as the file does
	if err != nil {
		t.Fatal(err)
	}
	var back WebshareHealthState
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "10.0.0.1") {
		t.Fatalf("state carries an exit address: %s", raw)
	}

	dst := newWebshareNetworkHealth()
	later := now.Add(time.Minute)
	asns, subnets := dst.restoreState(back, later)
	if asns != 2 || subnets != 1 {
		t.Fatalf("restored asns=%d subnets=%d, want 2 and 1", asns, subnets)
	}
	if !dst.IsQuarantinedOrSubnetCooled(100, "", later.UnixNano()) {
		t.Fatal("ASN 100 lost its quarantine across the restore")
	}
	if !dst.IsQuarantinedOrSubnetCooled(0, "10.0.0", later.UnixNano()) {
		t.Fatal("/24 10.0.0 lost its cooldown across the restore")
	}
	if got, want := dst.asns[200].windowString(), src.asns[200].windowString(); got != want {
		t.Fatalf("ASN 200 window = %q, want %q", got, want)
	}
	if dst.asns[100].cooldownUntil != src.asns[100].cooldownUntil {
		t.Fatalf("cooldown end changed: %d != %d", dst.asns[100].cooldownUntil, src.asns[100].cooldownUntil)
	}
	// Reaching webshareASNProbeFailLimit failed probes after the cooldown trips it a second time: the cooldown escalates as if the process never restarted.
	afterCooldown := time.Unix(0, dst.asns[100].cooldownUntil).Add(time.Second)
	for i := 0; i < webshareASNProbeFailLimit; i++ {
		dst.Record(100, "", "", "", false, afterCooldown)
	}
	if got, want := time.Duration(dst.asns[100].cooldownUntil-afterCooldown.UnixNano()), 2*webshareASNBaseCooldown; got != want {
		t.Fatalf("second trip cooldown = %s, want %s", got, want)
	}
}

func TestWebshareHealthStateRestoreNeverOverwritesLiveStateAndDropsExpiredSubnets(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	src := newWebshareNetworkHealth()
	tripASN(src, 100, now)
	src.Record(0, "", "10.0.0", "10.0.0.1", false, now)
	src.Record(0, "", "10.0.0", "10.0.0.2", false, now)
	st := src.exportState(now)

	dst := newWebshareNetworkHealth()
	dst.Record(100, "", "", "", true, now) // already live and healthy
	asns, subnets := dst.restoreState(st, now.Add(webshareSubnetCooldown+time.Second))
	if asns != 0 || subnets != 0 {
		t.Fatalf("restored asns=%d subnets=%d, want 0 (ASN live, subnet cooldown over)", asns, subnets)
	}
	if dst.asns[100].quarantined {
		t.Fatal("restore overwrote a live ASN")
	}
	st.Version = 99
	if a, s := newWebshareNetworkHealth().restoreState(st, now); a != 0 || s != 0 {
		t.Fatalf("a foreign version restored %d/%d", a, s)
	}
}

func TestWebshareHealthFileSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "health.json")
	first := newWebshareNetworkHealth()
	f, res, err := OpenWebshareHealthFile(healthPersister{first}, path, WebshareHealthFileOptions{Interval: time.Hour})
	if err != nil || res.ASNs != 0 || res.Skipped != "" {
		t.Fatalf("first open: res=%+v err=%v", res, err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file not written with mode 0600: %v %v", fi, err)
	}
	tripASN(first, 100, time.Now())
	if err := f.Close(); err != nil { // the final save carries the trip
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	second := newWebshareNetworkHealth()
	g, res, err := OpenWebshareHealthFile(healthPersister{second}, path, WebshareHealthFileOptions{Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if res.ASNs != 1 || res.QuarantinedASNs != 1 || res.SavedAt.IsZero() {
		t.Fatalf("restore = %+v, want ASN 100 quarantined", res)
	}
	if !second.IsQuarantinedOrSubnetCooled(100, "", time.Now().UnixNano()) {
		t.Fatal("ASN 100 is not quarantined after the restart")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".health.json.tmp-*")); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}

func TestWebshareHealthFileSkipsOldCorruptAndUnwritable(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old.json")
	st := newWebshareNetworkHealth().exportState(time.Now().Add(-3 * time.Hour))
	st.ASNs = []WebshareASNHealthState{{ASN: 1, Quarantined: true, CooldownUntil: time.Now().Add(time.Hour)}}
	raw, _ := json.Marshal(st)
	if err := os.WriteFile(old, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	h := newWebshareNetworkHealth()
	f, res, err := OpenWebshareHealthFile(healthPersister{h}, old, WebshareHealthFileOptions{})
	if err != nil || res.ASNs != 0 || !strings.HasPrefix(res.Skipped, "older than") {
		t.Fatalf("old state: res=%+v err=%v", res, err)
	}
	f.Close()

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, res, err = OpenWebshareHealthFile(healthPersister{newWebshareNetworkHealth()}, bad, WebshareHealthFileOptions{})
	if err != nil || res.Skipped != "unreadable" {
		t.Fatalf("corrupt state: res=%+v err=%v", res, err)
	}
	f.Close()

	if _, _, err := OpenWebshareHealthFile(healthPersister{newWebshareNetworkHealth()}, filepath.Join(dir, "missing-dir", "h.json"), WebshareHealthFileOptions{}); err == nil {
		t.Fatal("an unwritable directory must fail at open")
	}
}
