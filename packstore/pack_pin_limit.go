package packstore

import "go.kenn.io/kit/pack"

const (
	// maxPackedSourcePins permits a default 32 MiB pack to reach its target
	// with 8 KiB average objects. Even 100,000 tiny entries therefore require
	// at most 25 pin-driven packs when the process descriptor budget allows it.
	maxPackedSourcePins = pack.DefaultTargetSize / (8 << 10)
	// packedSourcePinReserve leaves descriptors for the runtime, catalog,
	// caller, cached pack readers, and the pack writer itself.
	packedSourcePinReserve = 128
	// fallbackPackedSourcePins is deliberately conservative on platforms that
	// cannot report a process descriptor ceiling.
	fallbackPackedSourcePins = 128
)

func defaultPackedSourcePinLimit(packEntries int) int {
	return max(1, min(packEntries, platformPackedSourcePinLimit()))
}

func packedSourcePinLimitForSoftLimit(soft uint64) int {
	if soft <= packedSourcePinReserve {
		return 1
	}
	// Maintenance owns at most half the descriptors left after the reserve.
	// This headroom matters because source packing is not the only concurrent
	// descriptor consumer in an embedding application.
	budget := (soft - packedSourcePinReserve) / 2
	return max(1, min(maxPackedSourcePins, int(min(budget, uint64(maxPackedSourcePins)))))
}
