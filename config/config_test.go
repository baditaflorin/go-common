package config

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func inDir(t *testing.T, yaml string) {
	t.Helper()
	dir := t.TempDir()
	if yaml != "" {
		if err := os.WriteFile(filepath.Join(dir, "service.yaml"), []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
}

func TestLoadDefaults(t *testing.T) {
	inDir(t, "")
	t.Setenv("PORT", "")
	cfg := Load("go_example", "v1.2.3")
	if cfg.Port != "8080" || cfg.AppName != "go_example" || cfg.Version != "v1.2.3" {
		t.Fatalf("Load = %+v, want port 8080, name go_example, version v1.2.3", cfg)
	}
}

func TestLoadPortFromEnv(t *testing.T) {
	inDir(t, "")
	t.Setenv("PORT", "9191")
	if got := Load("x", "v").Port; got != "9191" {
		t.Fatalf("Port = %q, want 9191", got)
	}
}

func TestLoadReadsTopLevelServiceYAML(t *testing.T) {
	inDir(t, "id: go_biz\nname: Biz Classifier\nversion: \"1.4.0\"\nport: 8277\n")
	cfg := Load("", "dev")
	if cfg.AppName != "Biz Classifier" || cfg.Version != "1.4.0" {
		t.Fatalf("Load = %+v, want name 'Biz Classifier' and version 1.4.0 from service.yaml", cfg)
	}
}

func TestExplicitAppNameWinsButFileVersionWins(t *testing.T) {
	inDir(t, "name: From File\nversion: 2.0.0\n")
	cfg := Load("explicit", "dev")
	if cfg.AppName != "explicit" {
		t.Fatalf("AppName = %q, want the explicit argument", cfg.AppName)
	}
	if cfg.Version != "2.0.0" {
		t.Fatalf("Version = %q, want the service.yaml version", cfg.Version)
	}
}

// The service template has owner.name and a list of named params. They describe
// other things and must never replace the service's own name or version.
func TestNestedKeysDoNotOverrideTopLevel(t *testing.T) {
	inDir(t, `id: go_x
name: Real Name
version: "3.0.0"
owner:
  name: "Florin"
  version: 9
api:
  params:
    - name: target
`)
	cfg := Load("", "dev")
	if cfg.AppName != "Real Name" || cfg.Version != "3.0.0" {
		t.Fatalf("Load = %+v, want the top-level name and version", cfg)
	}
}

func TestTrailingCommentsAndQuotesAreStripped(t *testing.T) {
	inDir(t, "name: \"Quoted Name\"   # shown in the catalog\nversion: 1.0.0 # semver\n")
	cfg := Load("", "dev")
	if cfg.AppName != "Quoted Name" || cfg.Version != "1.0.0" {
		t.Fatalf("Load = %+v, want comments and quotes removed", cfg)
	}
}

// Outside a service checkout there is no service.yaml; that is not worth a line
// on stdout.
func TestMissingServiceYAMLIsSilent(t *testing.T) {
	inDir(t, "")
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	Load("x", "v")
	os.Stdout = orig
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if len(out) != 0 {
		t.Fatalf("Load wrote %q to stdout for a missing service.yaml", out)
	}
}
