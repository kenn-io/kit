package daemon

import (
	"strconv"

	"github.com/shirou/gopsutil/v4/process"
)

// ProcessIdentity is an opaque OS process-creation identity. Values are only
// meaningful for exact comparison against the live process holding a PID.
type ProcessIdentity string

// ProcessIdentityStatus describes whether a recorded identity can be matched.
type ProcessIdentityStatus uint8

const (
	ProcessIdentityUnknown ProcessIdentityStatus = iota
	ProcessIdentityMatch
	ProcessIdentityMismatch
)

// ReadProcessIdentity returns pid's OS-reported creation time as an opaque
// identity. ok is false when pid is gone or the platform cannot inspect it.
func ReadProcessIdentity(pid int) (ProcessIdentity, bool) {
	if pid <= 0 {
		return "", false
	}
	proc, err := process.NewProcess(int32(pid))
	if err != nil {
		return "", false
	}
	created, err := proc.CreateTime()
	if err != nil || created <= 0 {
		return "", false
	}
	return ProcessIdentity(strconv.FormatInt(created, 10)), true
}

// CompareProcessIdentity compares a recorded creation identity with the live
// process currently holding pid. Unknown never authorizes destructive action.
func CompareProcessIdentity(pid int, recorded ProcessIdentity) ProcessIdentityStatus {
	if recorded == "" {
		return ProcessIdentityUnknown
	}
	live, ok := ReadProcessIdentity(pid)
	if !ok {
		return ProcessIdentityUnknown
	}
	if live == recorded {
		return ProcessIdentityMatch
	}
	return ProcessIdentityMismatch
}
