// Package packstore provides a mixed loose-and-packed content-addressed store.
//
// Packstore owns physical storage, validation, and crash-ordered maintenance.
// Applications retain catalog membership and product reachability behind the
// Resolver and Catalog interfaces. A file or pack entry is never sufficient by
// itself to grant read authority.
//
// Store.OpenStream exposes loose and plain packed content through one
// verification-on-EOF contract. A prefix is not authoritative: callers must
// observe terminal io.EOF, call Verify successfully, or check Verified before
// trusting the complete object. Store.CopyVerified consumes that contract into
// caller-owned private staging but deliberately does not sync, close, publish,
// or grant catalog authority. Packed streams lease cached descriptors; eviction
// and Store.Close retire them without racing active reads, and the last stream
// close releases the physical handle.
//
// Applications must share one Coordinator between the Maintainer and every
// application mutation that changes content membership or physical state.
// Coordinator is process-local; cross-process exclusion remains an application
// responsibility. Maintenance publishes and verifies physical data before a
// Catalog transaction grants authority, and removes old physical data only
// after authority has advanced.
//
// PrepareImport supports crash-ordered reuse of compatible immutable packs
// during restore. It copies and verifies source packs within configured Limits,
// publishes them without replacing an existing same-ID file, and returns a
// PreparedImport that still grants no authority. Applications must durably
// materialize every reported fallback before committing the prepared records
// and selected mappings through one RestoreCatalog transaction. Whole-pack
// totals describe the immutable footer; only selected, application-live hashes
// receive authority. The transaction is intentionally application-owned so its
// liveness rules remain separate from physical pack validity.
//
// Import is optional. Applications that do not supply packed restore policy can
// materialize the same content loose, and applications may mix representations
// when a pack or entry exceeds target limits or the filesystem cannot provide
// atomic no-clobber publication. A compatibility fallback is never an integrity
// fallback: declined content still requires an authenticated, hash-verified
// loose read.
//
// Physical storage operations are supported on Unix and Windows. Other Go
// targets compile but fail closed because their file APIs do not provide the
// atomic no-follow and nonblocking opens required for race-safe content access.
// On Windows, regular files are flushed and handles are closed before immutable
// packs are published or reopened; directory sync is a no-op consistent with
// Kit's wider durability policy. Publication uses hard links on Unix and
// Windows and falls back loose when a new destination cannot be created this
// way, rather than introducing a replacement-rename race.
//
// DefaultLimits retains a 64 MiB policy for ReadBounded, packed OpenStream,
// and maintenance. Store.Open is the buffered compatibility path and does not
// enforce those limits; larger authorized loose objects also remain available
// through OpenStream. Streaming removes the former object-sized heap
// requirement but does not raise packing policy automatically. Callers must
// budget scratch (about twice the largest concurrently prepared plain object),
// decoder windows, active streams, and descriptors before increasing limits.
// RetirePack can return ErrPackRetirementDeferred when a physical file remains
// in use; this is retryable cleanup and never permission to restore retired
// catalog authority.
package packstore
