//go:build unix

package packstore

import "golang.org/x/sys/unix"

func platformPackedSourcePinLimit() int {
	var processLimit unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &processLimit); err != nil {
		return fallbackPackedSourcePins
	}
	// Rlimit fields are uint64 on some Unix targets and int64 on others.
	// Check positivity before conversion so a signed infinity sentinel (-1)
	// cannot become an effectively unlimited descriptor budget.
	positive := processLimit.Cur > 0
	return packedSourcePinLimitForReportedSoftLimit(uint64(processLimit.Cur), positive)
}
