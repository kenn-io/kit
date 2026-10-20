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
// An identity from another PID namespace is Unknown: its PID names nothing here.
func CompareProcessIdentity(pid int, recorded ProcessIdentity) ProcessIdentityStatus {
	if recorded == "" || !processIdentityCompatible(recorded) ||
		processIdentityForeign(recorded) {
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

// RuntimeProcessAlive reports whether rec may still have a live process. It
// is true when rec.PID is alive here, or when rec's versioned identity is one
// this caller can't judge: it names another PID namespace, or is nonempty and
// unreadable (malformed or from a newer version). Use it in place of
// ProcessAlive(rec.PID) wherever a record is skipped or deleted.
func RuntimeProcessAlive(rec RuntimeRecord) bool {
	if ProcessAlive(rec.PID) {
		return true
	}
	id := rec.ProcessIdentityV2
	return id != "" && (!processIdentityCompatible(id) || processIdentityForeign(id))
}
