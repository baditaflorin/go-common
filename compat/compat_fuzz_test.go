package compat

import "testing"

// FuzzCheck: whatever a go.mod holds, Check and CheckGoMod must not panic and
// must answer with a defined status or an error, never a half-filled result.
func FuzzCheck(f *testing.F) {
	for _, s := range []string{"v0.102.2", "v0.0.0-20260110171042-8921433476a9", "v0.102.3-0.20261002000000-abcdefabcdef", "v1.2.3-rc.1+meta", "", "v", "v99999999999999999999.1.1", "banana"} {
		f.Add(s)
	}
	m, err := Load()
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, ver string) {
		res, err := m.Check(ver)
		if err != nil {
			return
		}
		if res.Status != StatusOK && res.Status != StatusBelowFloor {
			t.Fatalf("Check(%q) = %+v: a successful check must be ok or below the floor", ver, res)
		}
	})
}

func FuzzCheckGoMod(f *testing.F) {
	f.Add([]byte("module x\n\nrequire github.com/baditaflorin/go-common v0.97.0\n"))
	f.Add([]byte("require (\n\tgithub.com/baditaflorin/go-common v0.102.34 // indirect\n)\nreplace github.com/baditaflorin/go-common => ../x\n"))
	f.Add([]byte("github.com/baditaflorin/go-common =>"))
	f.Add([]byte(""))
	m, err := Load()
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, goMod []byte) {
		res, err := m.CheckGoMod(goMod)
		if err != nil {
			return
		}
		switch res.Status {
		case StatusOK, StatusBelowFloor, StatusUnchecked, StatusNotUsed:
		default:
			t.Fatalf("CheckGoMod returned status %q", res.Status)
		}
	})
}
