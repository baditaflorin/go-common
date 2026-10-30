package promx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/credfile"
	"github.com/prometheus/client_golang/prometheus"
)

// credSeries gathers reg into "metric|label=value,..." (labels sorted, service
// left out) -> value.
func credSeries(t *testing.T, reg *prometheus.Registry) (map[string]float64, string) {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]float64{}
	var dump strings.Builder
	for _, f := range families {
		dump.WriteString(f.String())
		for _, m := range f.Metric {
			var labels []string
			for _, l := range m.Label {
				if l.GetName() != "service" {
					labels = append(labels, l.GetName()+"="+l.GetValue())
				}
			}
			sort.Strings(labels)
			v := m.GetCounter().GetValue() + m.GetGauge().GetValue()
			out[f.GetName()+"|"+strings.Join(labels, ",")] = v
		}
	}
	return out, dump.String()
}

// TestCredfileCollectorsExportARealFile goes through credfile.Open and the
// process registry, as a service does.
func TestCredfileCollectorsExportARealFile(t *testing.T) {
	const secret = "ak_promx_credfile_SECRET_value"
	dir := t.TempDir()
	p := filepath.Join(dir, "fleet_promx_test_key")
	if err := os.WriteFile(p, []byte(secret+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	exp := time.Date(2027, 1, 29, 12, 0, 0, 0, time.UTC)
	meta, _ := json.Marshal(credfile.Meta{KeyID: "k_1", ExpiresAt: exp, KeySHA256: credfile.KeySHA256(secret)})
	if err := os.WriteFile(p+credfile.MetaSuffix, meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := credfile.Open("fleet_promx_test_key", p, credfile.Options{Protected: true}); err != nil {
		t.Fatal(err)
	}

	reg := prometheus.NewRegistry()
	NewCredfileCollectors(reg, "svc-cred")
	series, dump := credSeries(t, reg)
	if got, ok := series["fleet_credential_expiry_timestamp_seconds|credential=fleet_promx_test_key"]; !ok || got != float64(exp.Unix()) {
		t.Fatalf("expiry metric = %v, %v", got, ok)
	}
	for _, k := range []string{
		"fleet_credential_reload_total|credential=fleet_promx_test_key,result=ok",
		"fleet_credential_reload_total|credential=fleet_promx_test_key,result=error",
		"fleet_credential_reload_errors_total|credential=fleet_promx_test_key,reason=missing",
		"fleet_credential_stale_use_total|credential=fleet_promx_test_key",
	} {
		if _, ok := series[k]; !ok {
			t.Errorf("no series %s", k)
		}
	}
	if !strings.Contains(dump, `value:"svc-cred"`) {
		t.Error("the service label is missing")
	}
	if strings.Contains(dump, secret) {
		t.Fatal("the metrics carry the credential value")
	}
}

func TestCredfileCollectorsAggregateAndBound(t *testing.T) {
	early, late := time.Unix(1800000000, 0), time.Unix(1900000000, 0)
	stats := []credfile.Stats{
		{Name: "fleet_api_key", Expiry: late, ReloadOK: 1, Errors: map[string]uint64{credfile.ReasonMissing: 2}, ReloadError: 2, StaleUses: 3},
		{Name: "fleet_api_key", Expiry: early, ReloadOK: 2},
		{Name: "no_meta_key"},
		{Name: "Bad Name!"},
	}
	reg := prometheus.NewRegistry()
	reg.MustRegister(newCredfileCollectors("svc", func() []credfile.Stats { return stats }))
	series, _ := credSeries(t, reg)
	for k, want := range map[string]float64{
		"fleet_credential_expiry_timestamp_seconds|credential=fleet_api_key":           float64(early.Unix()),
		"fleet_credential_reload_total|credential=fleet_api_key,result=ok":             3,
		"fleet_credential_reload_total|credential=fleet_api_key,result=error":          2,
		"fleet_credential_reload_errors_total|credential=fleet_api_key,reason=missing": 2,
		"fleet_credential_stale_use_total|credential=fleet_api_key":                    3,
		"fleet_credential_stale_use_total|credential=_other":                           0,
		"fleet_credential_reload_total|credential=no_meta_key,result=ok":               0,
	} {
		if got, ok := series[k]; !ok || got != want {
			t.Errorf("%s = %v (present %v), want %v", k, got, ok, want)
		}
	}
	if _, ok := series["fleet_credential_expiry_timestamp_seconds|credential=no_meta_key"]; ok {
		t.Error("expiry exported for a credential without one")
	}
}
