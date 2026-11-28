//go:build !linux

package embedserver

func cgroupCPULimit() (int, error) { return 0, nil }
