package daemon

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

// CompareProcessIdentity compares a recorded creation identity with the live
// process currently holding pid. Unknown never authorizes destructive action.
func CompareProcessIdentity(pid int, recorded ProcessIdentity) ProcessIdentityStatus {
	if recorded == "" || !processIdentityCompatible(recorded) {
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

// CompareRuntimeProcessIdentity compares the strongest process identity stored
// in rec with the live process. Versioned identities take precedence; legacy
// identities remain readable only on platforms where they are reliable.
func CompareRuntimeProcessIdentity(rec RuntimeRecord) ProcessIdentityStatus {
	if rec.ProcessIdentityV2 != "" {
		return CompareProcessIdentity(rec.PID, rec.ProcessIdentityV2)
	}
	return CompareProcessIdentity(rec.PID, rec.ProcessIdentity)
}
