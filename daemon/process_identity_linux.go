//go:build linux

package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const linuxProcessIdentityPrefix = "linux-v1:"

// ReadProcessIdentity returns a Linux process identity derived only from
// monotonic kernel state. It remains stable when the wall clock changes.
func ReadProcessIdentity(pid int) (ProcessIdentity, bool) {
	return readLinuxProcessIdentity("/proc", pid)
}

// scopeGone means the identity proves the process gone: it is from an
// earlier boot, or from a same-boot namespace the caller proved empty. Only
// Linux identities carry the boot ID and namespace that can prove this.
const scopeGone = scopeUnjudged + 1

// processIdentityScope judges identity for the caller's own /proc.
func processIdentityScope(identity ProcessIdentity) identityScope {
	return linuxProcessIdentityScope("/proc", identity)
}

// linuxProcessIdentityScope is scopeUnjudged when identity is malformed or
// procRoot's boot ID or self namespace is unreadable. Otherwise it is
// scopeGone for an identity from an earlier boot or a provably empty
// namespace, scopeLocal for procRoot's own namespace, and scopeUnjudged for
// any other.
func linuxProcessIdentityScope(procRoot string, identity ProcessIdentity) identityScope {
	bootID, namespace, ok := parseLinuxProcessIdentity(identity)
	ownBootID, bootOK := readLinuxBootID(procRoot)
	own, err := readLinuxPIDNamespaceAt(procRoot, "self")
	switch {
	case !ok || !bootOK || err != nil:
		return scopeUnjudged
	case bootID != ownBootID || linuxPIDNamespaceProvedGone(procRoot, own, namespace):
		return scopeGone
	case namespace == own:
		return scopeLocal
	}
	return scopeUnjudged
}

// initPIDNamespace is the inode of the kernel's initial PID namespace.
const initPIDNamespace = "4026531836"

// linuxPIDNamespaceProvedGone is true when namespace differs from own and no
// process of the caller's user is in it. Only a caller in the initial PID
// namespace can see every process, so only it can prove a namespace is gone.
func linuxPIDNamespaceProvedGone(procRoot, own, namespace string) bool {
	return namespace != own && own == initPIDNamespace && linuxPIDNamespaceGone(procRoot, namespace)
}

// linuxPIDNamespaceGone is true when no process owned by the caller's user is
// in the PID namespace with the given inode. A process that exits during the
// walk doesn't count; any other read failure means the answer is unknown, so
// it is false.
func linuxPIDNamespaceGone(procRoot, inode string) bool {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return false
	}
	want := "pid:[" + inode + "]"
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		dir := filepath.Join(procRoot, entry.Name())
		info, err := os.Stat(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return false
		}
		if stat, ok := info.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
			continue
		}
		target, err := os.Readlink(filepath.Join(dir, "ns/pid"))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || target == want {
			return false
		}
	}
	return true
}

func readLinuxBootID(procRoot string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(procRoot, "sys/kernel/random/boot_id"))
	bootID := strings.TrimSpace(string(raw))
	return bootID, err == nil && validLinuxBootID(bootID)
}

// parseLinuxProcessIdentity returns the boot ID and PID namespace of a
// well-formed linux-v1 identity.
func parseLinuxProcessIdentity(identity ProcessIdentity) (bootID, namespace string, ok bool) {
	encoded := string(identity)
	if !strings.HasPrefix(encoded, linuxProcessIdentityPrefix) {
		return "", "", false
	}
	fields := strings.Split(strings.TrimPrefix(encoded, linuxProcessIdentityPrefix), ":")
	if len(fields) != 3 || !validLinuxBootID(fields[0]) {
		return "", "", false
	}
	for _, field := range fields[1:] {
		if _, valid := parseCanonicalLinuxIdentityUint(field); !valid {
			return "", "", false
		}
	}
	return fields[0], fields[1], true
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
			if (character < '0' || character > '9') &&
				(character < 'a' || character > 'f') {
				return false
			}
		}
	}
	return true
}

func parseCanonicalLinuxIdentityUint(encoded string) (uint64, bool) {
	value, err := strconv.ParseUint(encoded, 10, 64)
	if err != nil || value == 0 || strconv.FormatUint(value, 10) != encoded {
		return 0, false
	}
	return value, true
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
	bootIdentity, ok := readLinuxBootID(procRoot)
	if !ok {
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
	return readLinuxPIDNamespaceAt(procRoot, strconv.Itoa(pid))
}

func readLinuxPIDNamespaceAt(procRoot, process string) (string, error) {
	target, err := os.Readlink(filepath.Join(procRoot, process, "ns/pid"))
	if err != nil {
		return "", err
	}
	const prefix = "pid:["
	if !strings.HasPrefix(target, prefix) || !strings.HasSuffix(target, "]") {
		return "", errors.New("linux PID namespace has an invalid link target")
	}
	inode := strings.TrimSuffix(strings.TrimPrefix(target, prefix), "]")
	value, ok := parseCanonicalLinuxIdentityUint(inode)
	if !ok {
		return "", errors.New("linux PID namespace has an invalid inode")
	}
	return strconv.FormatUint(value, 10), nil
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
		return "", errors.New("linux process stat has no command terminator")
	}
	fields := strings.Fields(string(stat[closing+1:]))
	const startTimeIndex = 19 // Field 22 after PID and command.
	if len(fields) <= startTimeIndex {
		return "", errors.New("linux process stat has no start time")
	}
	value, ok := parseCanonicalLinuxIdentityUint(fields[startTimeIndex])
	if !ok {
		return "", errors.New("linux process stat has an invalid start time")
	}
	return strconv.FormatUint(value, 10), nil
}
