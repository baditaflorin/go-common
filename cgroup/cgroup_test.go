package cgroup

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadV2Snapshot(t *testing.T) {
	root := t.TempDir()
	write := func(name, value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("cgroup.controllers", "cpu memory")
	write("memory.current", "1048576\n")
	write("memory.max", "4194304\n")
	write("cpu.max", "150000 100000\n")
	write("cpu.stat", "usage_usec 2300000\nnr_periods 10\nnr_throttled 2\nthrottled_usec 300000\n")
	write("cpu.pressure", "some avg10=12.50 avg60=5.00 avg300=2.50 total=450000\n")
	write("memory.pressure", "some avg10=1.25 avg60=1.00 avg300=0.50 total=200000\nfull avg10=0.25 avg60=0.10 avg300=0.05 total=40000\n")

	s, err := readAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := *s.MemoryUsageBytes; got != 1048576 {
		t.Errorf("memory usage=%d", got)
	}
	if got := *s.MemoryLimitBytes; got != 4194304 {
		t.Errorf("memory limit=%d", got)
	}
	if got := *s.CPUQuotaCores; got != 1.5 {
		t.Errorf("cpu quota=%v", got)
	}
	if got := *s.CPUUsageSeconds; got != 2.3 {
		t.Errorf("cpu usage=%v", got)
	}
	if got := *s.CPUThrottledPeriods; got != 2 {
		t.Errorf("throttled periods=%d", got)
	}
	if got := *s.CPUThrottledSeconds; got != 0.3 {
		t.Errorf("throttled seconds=%v", got)
	}
	if got := *s.CPUPressureSome.Avg10Ratio; got != 0.125 {
		t.Errorf("cpu pressure=%v", got)
	}
	if got := *s.MemoryPressureFull.TotalSeconds; got != 0.04 {
		t.Errorf("memory pressure=%v", got)
	}
}

func TestReadV2UnlimitedResources(t *testing.T) {
	root := t.TempDir()
	for name, value := range map[string]string{
		"cgroup.controllers": "cpu memory", "memory.current": "500", "memory.max": "max",
		"cpu.max": "max 100000", "cpu.stat": "usage_usec 1000000\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := readAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if s.MemoryLimitBytes != nil || s.CPUQuotaCores != nil {
		t.Fatal("unlimited resources should have nil limits")
	}
}

func TestReadV1Snapshot(t *testing.T) {
	root := t.TempDir()
	for name, value := range map[string]string{
		"memory/memory.usage_in_bytes": "2048", "memory/memory.limit_in_bytes": "8192",
		"cpuacct/cpuacct.usage": "5000000000", "cpu/cpu.cfs_quota_us": "50000",
		"cpu/cpu.cfs_period_us": "100000", "cpu/cpu.stat": "nr_throttled 3\nthrottled_time 900000000\n",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := readAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if *s.MemoryUsageBytes != 2048 || *s.CPUUsageSeconds != 5 || *s.CPUQuotaCores != 0.5 || *s.CPUThrottledSeconds != 0.9 {
		t.Fatalf("unexpected v1 snapshot: %+v", s)
	}
}

func TestReadUnavailableCgroup(t *testing.T) {
	if _, err := readAt(t.TempDir()); err == nil {
		t.Fatal("expected unavailable-cgroup error")
	}
}

func TestResolveCgroupV2Path(t *testing.T) {
	dir := t.TempDir()
	cgroupFile := filepath.Join(dir, "cgroup")
	mountInfoFile := filepath.Join(dir, "mountinfo")
	write := func(path, value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(cgroupFile, "0::/system.slice/docker-abc.scope\n")
	write(mountInfoFile, "29 22 0:26 / /sys/fs/cgroup rw,nosuid,nodev,noexec,relatime - cgroup2 cgroup rw\n")
	got, err := resolveCgroupV2Path(cgroupFile, mountInfoFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/sys/fs/cgroup/system.slice/docker-abc.scope"; got != want {
		t.Fatalf("resolved cgroup path=%q, want %q", got, want)
	}

	write(cgroupFile, "0::/\n")
	write(mountInfoFile, "29 22 0:26 /system.slice/docker-abc.scope /sys/fs/cgroup rw - cgroup2 cgroup rw\n")
	got, err = resolveCgroupV2Path(cgroupFile, mountInfoFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := "/sys/fs/cgroup"; got != want {
		t.Fatalf("private cgroup namespace path=%q, want %q", got, want)
	}
}
