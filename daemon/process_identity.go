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
// process currently holding pid. Unknown never authorizes destructive action,
// and an identity from another PID namespace is Unknown unless it matches the
// live process exactly (see AGENTS.md).
func CompareProcessIdentity(pid int, recorded ProcessIdentity) ProcessIdentityStatus {
	return compareProcessIdentity(pid, recorded, ReadProcessIdentity, processIdentityForeign)
}

func compareProcessIdentity(
	pid int,
	recorded ProcessIdentity,
	read func(int) (ProcessIdentity, bool),
	foreign func(ProcessIdentity) bool,
) ProcessIdentityStatus {
	if recorded == "" || !processIdentityCompatible(recorded) {
		return ProcessIdentityUnknown
	}
	live, ok := read(pid)
	if ok && live == recorded {
		return ProcessIdentityMatch
	}
	if !ok || foreign(recorded) {
		return ProcessIdentityUnknown
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

// RuntimeProcessAlive reports whether rec may still have a live process: its
// PID is alive here, or its versioned identity is one this caller can't judge
// (see AGENTS.md). Use it in place of ProcessAlive(rec.PID) wherever a record
// is skipped or deleted.
func RuntimeProcessAlive(rec RuntimeRecord) bool {
	return ProcessAlive(rec.PID) || runtimeIdentityUnjudged(rec)
}

// runtimeIdentityUnjudged reports whether rec's versioned identity is nonempty
// and either unreadable, from another PID namespace, or one this caller can't
// compare because its own identity is unreadable.
func runtimeIdentityUnjudged(rec RuntimeRecord) bool {
	id := rec.ProcessIdentityV2
	return id != "" && (!processIdentityCompatible(id) || processIdentityForeign(id))
}

// runtimeRecordLive is RuntimeProcessAlive without a definite identity
// mismatch. Discover uses it to pick records worth probing.
func runtimeRecordLive(rec RuntimeRecord) bool {
	return RuntimeProcessAlive(rec) && CompareRuntimeProcessIdentity(rec) != ProcessIdentityMismatch
}
