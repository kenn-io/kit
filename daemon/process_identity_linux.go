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
	return validateLinuxProcessIdentity(identity)
}

func validateLinuxProcessIdentity(identity ProcessIdentity) bool {
	encoded := string(identity)
	if !strings.HasPrefix(encoded, linuxProcessIdentityPrefix) {
		return false
	}
	fields := strings.Split(strings.TrimPrefix(encoded, linuxProcessIdentityPrefix), ":")
	if len(fields) != 3 || !validLinuxBootID(fields[0]) {
		return false
	}
	for _, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil || value == 0 {
			return false
		}
	}
	return true
}

func validLinuxBootID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		switch index {
		case 8, 13, 18, 23:
			if character != '-' {
				return false
			}
		default:
			if !((character >= '0' && character <= '9') ||
				(character >= 'a' && character <= 'f') ||
				(character >= 'A' && character <= 'F')) {
				return false
			}
		}
	}
	return true
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
	bootIdentity := strings.TrimSpace(string(bootID))
	if err != nil || !validLinuxBootID(bootIdentity) {
		return "", false
	}
	namespace, err := readLinuxPIDNamespace(procRoot, pid)
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
		bootIdentity,
		namespace,
		processTicks,
	)), true
}

func readLinuxPIDNamespace(procRoot string, pid int) (string, error) {
	target, err := os.Readlink(filepath.Join(procRoot, strconv.Itoa(pid), "ns/pid"))
	if err != nil {
		return "", err
	}
	const prefix = "pid:["
	if !strings.HasPrefix(target, prefix) || !strings.HasSuffix(target, "]") {
		return "", errors.New("Linux PID namespace has an invalid link target")
	}
	inode := strings.TrimSuffix(strings.TrimPrefix(target, prefix), "]")
	if _, err := strconv.ParseUint(inode, 10, 64); err != nil {
		return "", errors.New("Linux PID namespace has an invalid inode")
	}
	return inode, nil
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
