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

func processIdentityCompatible(identity ProcessIdentity) bool {
	return validateLinuxProcessIdentity(identity)
}

// processIdentityForeign reports whether this caller can't judge identity's
// PID: it names another PID namespace on this boot, or the caller can't read
// its own boot ID or namespace.
func processIdentityForeign(identity ProcessIdentity) bool {
	return linuxProcessIdentityForeign("/proc", identity)
}

// linuxProcessIdentityForeign is true when procRoot's boot ID or self
// namespace is unreadable, or when identity has procRoot's boot ID and a PID
// namespace other than procRoot's self namespace. An identity from an earlier
// boot names a process that is gone, so it is false. Callers must pass an
// identity that processIdentityCompatible accepted.
func linuxProcessIdentityForeign(procRoot string, identity ProcessIdentity) bool {
	ownBootID, ok := readLinuxBootID(procRoot)
	if !ok {
		return true
	}
	own, err := readLinuxPIDNamespaceAt(procRoot, "self")
	if err != nil {
		return true
	}
	bootID, namespace, _ := parseLinuxProcessIdentity(identity)
	return bootID == ownBootID && linuxPIDNamespaceForeign(procRoot, own, namespace)
}

// initPIDNamespace is the inode of the kernel's initial PID namespace.
const initPIDNamespace = "4026531836"

// linuxPIDNamespaceForeign is true when namespace differs from own and isn't
// provably gone. Only a caller in the initial PID namespace can see every
// process, so only it can prove a namespace is gone.
func linuxPIDNamespaceForeign(procRoot, own, namespace string) bool {
	return namespace != own && (own != initPIDNamespace || !linuxPIDNamespaceGone(procRoot, namespace))
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

func validateLinuxProcessIdentity(identity ProcessIdentity) bool {
	_, _, ok := parseLinuxProcessIdentity(identity)
	return ok
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
