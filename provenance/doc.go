// Package provenance defines the JSON version record a release binary prints
// and checks the final executable against values its caller expects.
//
// The encoding of [Info] is the contract. It writes the supported
// [SchemaVersion], encodes both timestamps in UTC, and omits zero-valued
// fields. Commit is the full SHA-1 or SHA-256 hash. BuildTime and SourceTime
// are separate timestamps: BuildTime is when the binary was built, and
// SourceTime is the time of the source revision it was built from.
// Distribution names the channel the binary was packaged for.
//
// Adoption is backward compatible: keep the CLI's existing plain version
// output and flags, and add a flag, such as version --json, that writes only
// an encoded [Info] to stdout. Stamp every field from the release pipeline's
// trusted inputs, such as $RELEASE_COMMIT for the commit, rather than from the
// local checkout.
//
// [Check] runs the executable and compares the record it prints with the whole
// record the pipeline stamped, which the pipeline supplies independently of
// the binary. The expected record names the version, the full commit, the
// distribution and at least one timestamp. Every defined field must match.
// Timestamps match as instants, so equivalent RFC 3339 offsets agree, and a
// zero expected time requires the binary to report no such timestamp: a
// pipeline that stamps only a source time passes no BuildTime. Key order and
// additional schema 1 keys leave the result unchanged. A mismatch reports what
// the binary printed alongside the expected record.
//
// Which channels exist and which timestamps to stamp stay policy of each
// pipeline, as do extracting the archive or installing the package. Check the
// resulting release executable after packaging:
//
//	func TestReleaseBinary(t *testing.T) {
//		err := provenance.Check(t.Context(), bin, []string{"version", "--json"}, provenance.Info{
//			Version: tag, Commit: releaseCommit, BuildTime: buildTime, Distribution: "tap",
//		})
//		if err != nil {
//			t.Fatal(err)
//		}
//	}
package provenance
