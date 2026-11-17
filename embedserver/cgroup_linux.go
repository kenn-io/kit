package embedserver

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func cgroupCPULimit() (int, error) {
	return cpuQuota("/proc/self/cgroup", "/proc/self/mountinfo")
}

func cpuQuota(cgroupFile, mountinfoFile string) (int, error) {
	data, err := os.ReadFile(cgroupFile)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var v1, v2 string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) != 3 || fields[2] == "" {
			return 0, fmt.Errorf("malformed cgroup line %q", line)
		}
		if fields[0] == "0" && fields[1] == "" {
			v2 = fields[2]
		}
		for _, controller := range strings.Split(fields[1], ",") {
			if controller == "cpu" {
				v1 = fields[2]
			}
		}
	}
	group, kind := v1, "cgroup"
	if group == "" {
		group, kind = v2, "cgroup2"
	}
	if group == "" {
		return 0, nil
	}
	data, err = os.ReadFile(mountinfoFile)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			return 0, fmt.Errorf("malformed mountinfo line %q", line)
		}
		before, after := strings.Fields(parts[0]), strings.Fields(parts[1])
		if len(before) < 6 || len(after) < 3 {
			return 0, fmt.Errorf("malformed mountinfo line %q", line)
		}
		if after[0] != kind {
			continue
		}
		if kind == "cgroup" && !containsController(after[2], "cpu") {
			continue
		}
		root, mount := unescape.Replace(before[3]), unescape.Replace(before[4])
		rel, err := filepath.Rel(root, group)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return readCPUQuota(filepath.Join(mount, rel), kind)
	}
	return 0, nil
}

func containsController(options, controller string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == controller {
			return true
		}
	}
	return false
}

func readCPUQuota(dir, kind string) (int, error) {
	name := "cpu.max"
	if kind == "cgroup" {
		name = "cpu.cfs_quota_us"
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if kind == "cgroup" {
		if len(fields) != 1 {
			return 0, fmt.Errorf("invalid CPU quota %q", data)
		}
		if fields[0] == "-1" {
			return 0, nil
		}
		period, err := os.ReadFile(filepath.Join(dir, "cpu.cfs_period_us"))
		if err != nil {
			return 0, err
		}
		fields = append(fields, strings.Fields(string(period))...)
	}
	if len(fields) != 2 {
		return 0, fmt.Errorf("invalid CPU quota %q", data)
	}
	period, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || period <= 0 {
		return 0, fmt.Errorf("invalid CPU period %q", fields[1])
	}
	if kind == "cgroup2" && fields[0] == "max" {
		return 0, nil
	}
	quota, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil || quota <= 0 {
		return 0, fmt.Errorf("invalid CPU quota %q", fields[0])
	}
	count := quota / period
	if quota%period != 0 {
		count++
	}
	return int(count), nil
}
