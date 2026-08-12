//go:build linux

package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const linuxProcessIdentityPrefix = "linux-v1:"

// ReadProcessIdentity returns a Linux process identity derived only from
// monotonic kernel state. It remains stable when the wall clock changes.
func ReadProcessIdentity(pid int) (ProcessIdentity, bool) {
	return readLinuxProcessIdentity("/proc", pid)
}

func processIdentityCompatible(identity ProcessIdentity) bool {
	return strings.HasPrefix(string(identity), linuxProcessIdentityPrefix)
}

func runtimeProcessIdentities(pid int) (ProcessIdentity, ProcessIdentity) {
	identity, _ := ReadProcessIdentity(pid)
	// The legacy field stays empty so clients predating process_identity_v2
	// treat this record as unverifiable instead of deleting it as mismatched.
	return "", identity
}

func readLinuxProcessIdentity(procRoot string, pid int) (ProcessIdentity, bool) {
	if pid <= 0 {
		return "", false
	}
	bootID, err := os.ReadFile(filepath.Join(procRoot, "sys/kernel/random/boot_id"))
	if err != nil || strings.TrimSpace(string(bootID)) == "" {
		return "", false
	}
	initTicks, err := readLinuxProcessStartTicks(procRoot, 1)
	if err != nil {
		return "", false
	}
	processTicks, err := readLinuxProcessStartTicks(procRoot, pid)
	if err != nil {
		return "", false
	}
	return ProcessIdentity(fmt.Sprintf(
		"%s%s:%s:%s",
		linuxProcessIdentityPrefix,
		strings.TrimSpace(string(bootID)),
		initTicks,
		processTicks,
	)), true
}

func readLinuxProcessStartTicks(procRoot string, pid int) (string, error) {
	stat, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", err
	}
	return parseLinuxProcessStartTicks(stat)
}

func parseLinuxProcessStartTicks(stat []byte) (string, error) {
	closing := bytes.LastIndexByte(stat, ')')
	if closing < 0 || closing+1 >= len(stat) {
		return "", errors.New("Linux process stat has no command terminator")
	}
	fields := strings.Fields(string(stat[closing+1:]))
	const startTimeIndex = 19 // Field 22 after PID and command.
	if len(fields) <= startTimeIndex {
		return "", errors.New("Linux process stat has no start time")
	}
	if _, err := strconv.ParseUint(fields[startTimeIndex], 10, 64); err != nil {
		return "", errors.New("Linux process stat has an invalid start time")
	}
	return fields[startTimeIndex], nil
}
