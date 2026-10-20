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
	return compareProcessIdentity(pid, recorded, ReadProcessIdentity, processIdentityScope)
}

func compareProcessIdentity(
	pid int,
	recorded ProcessIdentity,
	read func(int) (ProcessIdentity, bool),
	scope func(ProcessIdentity) identityScope,
) ProcessIdentityStatus {
	if recorded == "" {
		return ProcessIdentityUnknown
	}
	live, ok := read(pid)
	if ok && live == recorded {
		return ProcessIdentityMatch
	}
	if !ok || scope(recorded) == scopeUnjudged {
		return ProcessIdentityUnknown
	}
	return ProcessIdentityMismatch
}

// identityScope is what a caller can conclude from an identity alone about
// the process it names.
type identityScope uint8

const (
	// scopeLocal means the PID names a process this caller can see, so a PID
	// check applies.
	scopeLocal identityScope = iota
	// scopeGone means the identity proves the process gone: it is from an
	// earlier boot, or from a same-boot namespace the caller proved empty.
	scopeGone
	// scopeUnjudged means this caller can't tell: the identity is unreadable,
	// names another PID namespace, or the caller can't read its own.
	scopeUnjudged
)

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
// versioned identity is one this caller can't judge, or it is local and the PID
// is alive here. An identity that proves the process gone ends the question
// whatever the local PID holds (see AGENTS.md). Use it in place of
// ProcessAlive(rec.PID) wherever a record is skipped or deleted.
func RuntimeProcessAlive(rec RuntimeRecord) bool {
	scope := runtimeIdentityScope(rec)
	return scope == scopeUnjudged || scope == scopeLocal && ProcessAlive(rec.PID)
}

// runtimeIdentityScope is scopeLocal for a record with no versioned identity.
func runtimeIdentityScope(rec RuntimeRecord) identityScope {
	if rec.ProcessIdentityV2 == "" {
		return scopeLocal
	}
	return processIdentityScope(rec.ProcessIdentityV2)
}

// runtimeRecordLive is RuntimeProcessAlive without a definite identity
// mismatch. Discover uses it to pick records worth probing.
func runtimeRecordLive(rec RuntimeRecord) bool {
	return RuntimeProcessAlive(rec) && CompareRuntimeProcessIdentity(rec) != ProcessIdentityMismatch
}
