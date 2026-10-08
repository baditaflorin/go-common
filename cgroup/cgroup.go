// Package cgroup reads resource counters and limits for the current Linux cgroup.
// Host-wide capacity should be read from the host metrics agent instead.
package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const defaultRoot = "/sys/fs/cgroup"

// Snapshot contains the resource signals available for the current cgroup.
// Pointer fields are nil when a value is unsupported or the resource is unlimited.
type Snapshot struct {
	MemoryUsageBytes    *uint64
	MemoryLimitBytes    *uint64
	CPUUsageSeconds     *float64
	CPUQuotaCores       *float64
	CPUThrottledPeriods *uint64
	CPUThrottledSeconds *float64
	CPUPressureSome     *Pressure
	MemoryPressureSome  *Pressure
	MemoryPressureFull  *Pressure
}

// Pressure contains Linux PSI stall fractions (0..1) and cumulative stall time.
type Pressure struct {
	Avg10Ratio   *float64
	Avg60Ratio   *float64
	Avg300Ratio  *float64
	TotalSeconds *float64
}

// Read returns the current process cgroup resource snapshot.
func Read() (Snapshot, error) {
	root := defaultRoot
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err == nil {
		if resolved, err := resolveCgroupV2Path("/proc/self/cgroup", "/proc/self/mountinfo"); err == nil {
			root = resolved
		}
	}
	return readAt(root)
}

func resolveCgroupV2Path(cgroupFile, mountInfoFile string) (string, error) {
	cgroupData, err := os.ReadFile(cgroupFile)
	if err != nil {
		return "", err
	}
	current := ""
	for _, line := range strings.Split(string(cgroupData), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) == 3 && fields[0] == "0" && fields[1] == "" {
			current = filepath.Clean(fields[2])
			break
		}
	}
	if current == "" || !filepath.IsAbs(current) {
		return "", errors.New("unified cgroup path unavailable")
	}

	mountData, err := os.ReadFile(mountInfoFile)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(mountData), "\n") {
		fields := strings.Fields(line)
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if separator < 6 || separator+1 >= len(fields) || fields[separator+1] != "cgroup2" {
			continue
		}
		mountRoot := filepath.Clean(fields[3])
		mountPoint := filepath.Clean(fields[4])
		relative := ""
		switch {
		case current == mountRoot:
			relative = ""
		case strings.HasPrefix(current, strings.TrimSuffix(mountRoot, string(filepath.Separator))+string(filepath.Separator)):
			relative = strings.TrimPrefix(current, strings.TrimSuffix(mountRoot, string(filepath.Separator)))
		case mountRoot == string(filepath.Separator):
			relative = current
		case current == string(filepath.Separator):
			// A private cgroup namespace exposes the container root as / even
			// when mountinfo retains a host-side cgroup path.
			relative = ""
		default:
			return mountPoint, nil
		}
		target := filepath.Join(mountPoint, strings.TrimPrefix(relative, string(filepath.Separator)))
		rel, err := filepath.Rel(mountPoint, target)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", errors.New("resolved cgroup path escapes its mount")
		}
		return target, nil
	}
	return "", errors.New("cgroup2 mount unavailable")
}

func readAt(root string) (Snapshot, error) {
	if _, err := os.Stat(filepath.Join(root, "cgroup.controllers")); err == nil {
		return readV2(root)
	}
	return readV1(root)
}

func readV2(root string) (Snapshot, error) {
	var s Snapshot
	found := false
	if v, err := readUint(filepath.Join(root, "memory.current")); err != nil {
		return s, err
	} else if v != nil {
		s.MemoryUsageBytes, found = v, true
	}
	if v, err := readLimit(filepath.Join(root, "memory.max"), false); err != nil {
		return s, err
	} else {
		s.MemoryLimitBytes = v
	}
	if stats, err := readValues(filepath.Join(root, "cpu.stat")); err != nil {
		return s, err
	} else if stats != nil {
		if v, ok := stats["usage_usec"]; ok {
			seconds := float64(v) / 1e6
			s.CPUUsageSeconds, found = &seconds, true
		}
		if v, ok := stats["nr_throttled"]; ok {
			s.CPUThrottledPeriods = &v
		}
		if v, ok := stats["throttled_usec"]; ok {
			seconds := float64(v) / 1e6
			s.CPUThrottledSeconds = &seconds
		}
	}
	if v, err := readQuotaV2(filepath.Join(root, "cpu.max")); err != nil {
		return s, err
	} else {
		s.CPUQuotaCores = v
	}
	s.CPUPressureSome = readPressure(filepath.Join(root, "cpu.pressure"), "some")
	s.MemoryPressureSome = readPressure(filepath.Join(root, "memory.pressure"), "some")
	s.MemoryPressureFull = readPressure(filepath.Join(root, "memory.pressure"), "full")
	if !found {
		return s, errors.New("cgroup v2 usage counters unavailable")
	}
	return s, nil
}

func readV1(root string) (Snapshot, error) {
	var s Snapshot
	found := false
	memoryUsage := firstFile(root, "memory/memory.usage_in_bytes", "memory.usage_in_bytes")
	if memoryUsage != "" {
		if v, err := readUint(memoryUsage); err != nil {
			return s, err
		} else if v != nil {
			s.MemoryUsageBytes, found = v, true
		}
		if v, err := readLimit(filepath.Join(filepath.Dir(memoryUsage), "memory.limit_in_bytes"), true); err != nil {
			return s, err
		} else {
			s.MemoryLimitBytes = v
		}
	}
	cpuUsage := firstFile(root, "cpuacct/cpuacct.usage", "cpu,cpuacct/cpuacct.usage", "cpuacct.usage")
	if cpuUsage != "" {
		if v, err := readUint(cpuUsage); err != nil {
			return s, err
		} else if v != nil {
			seconds := float64(*v) / 1e9
			s.CPUUsageSeconds, found = &seconds, true
		}
	}
	quotaFile := firstFile(root, "cpu/cpu.cfs_quota_us", "cpu,cpuacct/cpu.cfs_quota_us", "cpu.cfs_quota_us")
	if quotaFile != "" {
		quota, err := readInt(quotaFile)
		if err != nil {
			return s, err
		}
		period, err := readUint(filepath.Join(filepath.Dir(quotaFile), "cpu.cfs_period_us"))
		if err != nil {
			return s, err
		}
		if quota != nil && *quota > 0 && period != nil && *period > 0 {
			cores := float64(*quota) / float64(*period)
			s.CPUQuotaCores = &cores
		}
		stats, err := readValues(filepath.Join(filepath.Dir(quotaFile), "cpu.stat"))
		if err != nil {
			return s, err
		}
		if v, ok := stats["nr_throttled"]; ok {
			s.CPUThrottledPeriods = &v
		}
		if v, ok := stats["throttled_time"]; ok {
			seconds := float64(v) / 1e9
			s.CPUThrottledSeconds = &seconds
		}
	}
	if !found {
		return s, errors.New("cgroup v1 usage counters unavailable")
	}
	return s, nil
}

func firstFile(root string, paths ...string) string {
	for _, path := range paths {
		full := filepath.Join(root, path)
		if _, err := os.Stat(full); err == nil {
			return full
		}
	}
	return ""
}

func readUint(path string) (*uint64, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &v, nil
}

func readInt(path string) (*int64, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &v, nil
}

func readLimit(path string, v1 bool) (*uint64, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	text := strings.TrimSpace(string(b))
	if text == "max" {
		return nil, nil
	}
	v, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if v1 && v >= 1<<60 {
		return nil, nil
	}
	return &v, nil
}

func readQuotaV2(path string) (*float64, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 {
		return nil, fmt.Errorf("parse %s: expected quota and period", path)
	}
	if fields[0] == "max" {
		return nil, nil
	}
	quota, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse %s quota: %w", path, err)
	}
	period, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse %s period: %w", path, err)
	}
	if quota == 0 || period == 0 {
		return nil, nil
	}
	cores := float64(quota) / float64(period)
	return &cores, nil
}

func readValues(path string) (map[string]uint64, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	values := make(map[string]uint64)
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse %s field %s: %w", path, fields[0], err)
		}
		values[fields[0]] = v
	}
	return values, nil
}

func readPressure(path, category string) *Pressure {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != category {
			continue
		}
		p := &Pressure{}
		for _, field := range fields[1:] {
			name, text, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			if name == "total" {
				if micros, err := strconv.ParseUint(text, 10, 64); err == nil {
					seconds := float64(micros) / 1e6
					p.TotalSeconds = &seconds
				}
				continue
			}
			value, err := strconv.ParseFloat(text, 64)
			if err != nil {
				continue
			}
			value /= 100
			switch name {
			case "avg10":
				p.Avg10Ratio = &value
			case "avg60":
				p.Avg60Ratio = &value
			case "avg300":
				p.Avg300Ratio = &value
			}
		}
		if p.Avg10Ratio != nil || p.Avg60Ratio != nil || p.Avg300Ratio != nil || p.TotalSeconds != nil {
			return p
		}
		return nil
	}
	return nil
}
