package apikey

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baditaflorin/go-common/credfile"
)

func TestCriticalRemediationNeverMintsNeverExpiringKeys(t *testing.T) {
	t.Setenv("CRIT_NE_A", "")
	t.Setenv("CRIT_NE_B", "not-a-fleet-key")
	t.Setenv("CRIT_NE_FILE", "")
	var errs []error
	_, err := ResolveCritical("svc-ne", "CRIT_NE_A")
	errs = append(errs, err)
	_, err = ResolveCritical("svc-ne", "CRIT_NE_B")
	errs = append(errs, err)
	_, err = ResolveCriticalFile("svc-ne", "CRIT_NE_FILE", "CRIT_NE_A")
	errs = append(errs, err)
	for _, err := range errs {
		if err == nil {
			t.Fatal("expected an error")
		}
		if strings.Contains(err.Error(), "never-expires") || strings.Contains(err.Error(), "never_expires") {
			t.Fatalf("remediation still mints a never-expiring key: %s", err)
		}
		if !strings.Contains(err.Error(), "fix=`fleet-runner key provision svc-ne") {
			t.Fatalf("remediation is not key provision: %s", err)
		}
	}
}

func writeCriticalKey(t *testing.T, path, value string, mode os.FileMode) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(value+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tmp, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func TestResolveCriticalFile_EnvOnlyIsResolveCritical(t *testing.T) {
	want := "ak_" + strings.Repeat("e", 40)
	t.Setenv("CRIT_FILE_UNSET", "")
	t.Setenv("CRIT_ENV_KEY", want)
	k, err := ResolveCriticalFile("svc-env", "CRIT_FILE_UNSET", "CRIT_ENV_KEY")
	if err != nil {
		t.Fatal(err)
	}
	if k.Value() != want || k.FileBacked() || k.Source() != "CRIT_ENV_KEY" {
		t.Fatalf("env key: value ok=%v file=%v source=%s", k.Value() == want, k.FileBacked(), k.Source())
	}
	if v, changed := k.Rejected(want); changed || v != want {
		t.Fatal("an env key changed on Rejected")
	}
	if _, ok := k.Expiry(); ok {
		t.Fatal("an env key has an expiry")
	}

	// Same failures as ResolveCritical.
	t.Setenv("CRIT_ENV_KEY", "legacy-value")
	_, err = ResolveCriticalFile("svc-env", "CRIT_FILE_UNSET", "CRIT_ENV_KEY")
	if err == nil || !strings.Contains(err.Error(), "reason=unknown_prefix") || strings.Contains(err.Error(), "legacy-value") {
		t.Fatalf("unknown prefix: %v", err)
	}
	t.Setenv("CRIT_ENV_KEY", "")
	_, err = ResolveCriticalFile("svc-env", "CRIT_FILE_UNSET", "CRIT_ENV_KEY")
	if err == nil || !strings.Contains(err.Error(), "reason=unset") || !strings.Contains(err.Error(), "env=CRIT_FILE_UNSET,CRIT_ENV_KEY") {
		t.Fatalf("unset: %v", err)
	}
	if _, err := ResolveCriticalFile("", "X"); err == nil {
		t.Fatal("empty slug accepted")
	}
	if _, err := ResolveCriticalFile("svc", ""); err == nil {
		t.Fatal("empty fileEnv accepted")
	}
}

func TestResolveCriticalFile_FileWinsAndFollowsRenewal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fleet_api_key")
	first, second := "ak_"+strings.Repeat("1", 40), "ak_"+strings.Repeat("2", 40)
	writeCriticalKey(t, path, first, 0o400)
	exp := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)
	meta, _ := json.Marshal(credfile.Meta{ExpiresAt: exp, KeySHA256: credfile.KeySHA256(first)})
	if err := os.WriteFile(path+credfile.MetaSuffix, meta, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRIT_FILE_KEY_FILE", path)
	t.Setenv("CRIT_FILE_ENV", "ak_"+strings.Repeat("e", 40))

	k, err := ResolveCriticalFile("svc-file", "CRIT_FILE_KEY_FILE", "CRIT_FILE_ENV")
	if err != nil {
		t.Fatal(err)
	}
	if k.Value() != first || !k.FileBacked() || k.Source() != "CRIT_FILE_KEY_FILE" {
		t.Fatalf("file key: first=%v file=%v source=%s", k.Value() == first, k.FileBacked(), k.Source())
	}
	if got, ok := k.Expiry(); !ok || !got.Equal(exp) {
		t.Fatalf("Expiry = %v %v", got, ok)
	}

	// A renewal is picked up at once after a 401.
	writeCriticalKey(t, path, second, 0o400)
	if v, changed := k.Rejected(first); !changed || v != second {
		t.Fatalf("Rejected after renewal: changed=%v new=%v", changed, v == second)
	}
	if k.Value() != second {
		t.Fatal("Value does not return the renewed key")
	}

	for _, s := range []string{fmt.Sprint(k), fmt.Sprintf("%+v", k), fmt.Sprintf("%#v", k)} {
		if strings.Contains(s, first) || strings.Contains(s, second) {
			t.Fatalf("CriticalKey printed its value: %s", s)
		}
	}
}

func TestResolveCriticalFile_FileErrors(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, content string
		mode          os.FileMode
		reason        string
	}{
		{"missing", "", 0, "reason=file_missing"},
		{"unsafe", "ak_" + strings.Repeat("u", 40), 0o644, "reason=file_unsafe"},
		{"empty", "  ", 0o400, "reason=file_empty"},
		{"prefix", "AKIAIOSFODNN7EXAMPLE", 0o400, "reason=unknown_prefix"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			if tc.mode != 0 {
				writeCriticalKey(t, path, tc.content, tc.mode)
			}
			t.Setenv("CRIT_ERR_FILE", path)
			t.Setenv("CRIT_ERR_ENV", "ak_"+strings.Repeat("f", 40))
			_, err := ResolveCriticalFile("svc-err", "CRIT_ERR_FILE", "CRIT_ERR_ENV")
			if err == nil || !strings.Contains(err.Error(), tc.reason) || !strings.Contains(err.Error(), "env=CRIT_ERR_FILE") {
				t.Fatalf("err = %v, want %s", err, tc.reason)
			}
			if tc.content != "" && strings.Contains(err.Error(), tc.content) {
				t.Fatalf("the error carries the file's content: %v", err)
			}
		})
	}
}
