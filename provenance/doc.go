// Package provenance defines the JSON version record a release binary prints
// and checks the final executable against values its caller expects.
//
// [Info] encodes with encoding/json and omits zero-valued fields. Set its
// SchemaVersion field to the [SchemaVersion] constant, the only record version
// [Check] accepts.
// Commit is the full SHA-1 or SHA-256 hash. BuildTime and SourceTime are
// separate UTC timestamps; never report a source revision's time as the build
// time. Distribution names the channel the binary was packaged for.
//
// Adoption is backward compatible: keep the CLI's existing plain version
// output and flags, and add a flag, such as version --json, that writes only
// an encoded [Info] to stdout. Stamp the commit from the release pipeline's
// trusted commit input, here $RELEASE_COMMIT, not from the local checkout.
//
// [Check] establishes that the executable reports the supported schema
// version, the expected version and full commit, the explicitly expected
// distribution, and the timestamp of the selected [TimeKind] equal to an
// independently known ExpectedTime. Every expectation in [Want] comes from the
// caller; which channels exist and which timestamp to stamp stay policy of each
// caller. After packaging, test the extracted or installed executable:
//
//	func TestReleaseBinary(t *testing.T) {
//		err := provenance.Check(t.Context(), bin, []string{"version", "--json"}, provenance.Want{
//			Version: tag, Commit: releaseCommit, Distribution: "tap",
//			Time: provenance.BuildTime, ExpectedTime: buildTime,
//		})
//		if err != nil {
//			t.Fatal(err)
//		}
//	}
package provenance
