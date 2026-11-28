// Package provenance defines the JSON version record a release binary prints
// and checks the final executable against the values its release expects.
//
// [Info] encodes with encoding/json and omits zero-valued fields. Commit is
// the full SHA-1 or SHA-256 hash. BuildTime and SourceTime are separate UTC
// timestamps; never report a source revision's time as the build time.
//
// Adoption is backward compatible: keep the CLI's existing version output and
// add a flag, such as version --json, that writes only an encoded [Info] to
// stdout. Stamp the commit from the release pipeline's trusted commit input,
// here $RELEASE_COMMIT, not from the local checkout:
//
//	// go build -ldflags "-X main.commit=$RELEASE_COMMIT -X main.buildTime=$BUILD_TIME"
//	info := provenance.Info{Version: version, Commit: commit}
//	if t, err := time.Parse(time.RFC3339, buildTime); err == nil {
//		info.BuildTime = t.UTC()
//	}
//	return json.NewEncoder(os.Stdout).Encode(info)
//
// The caller owns every expectation in [Want]. After packaging, run [Check]
// on the extracted or installed executable as a smoke check:
//
//	err := provenance.Check(ctx, bin, []string{"version", "--json"}, provenance.Want{
//		Version: tag, Commit: releaseCommit, Distribution: "tap", Time: provenance.BuildTime,
//	})
package provenance
