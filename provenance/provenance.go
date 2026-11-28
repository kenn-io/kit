package provenance

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/kit/internal/binaryprobe"
)

// SchemaVersion is the supported version of the JSON provenance record.
const SchemaVersion = 1

// Info is the machine-readable provenance reported by a binary.
type Info struct {
	SchemaVersion int       `json:"schema_version,omitzero"`
	Version       string    `json:"version,omitzero"`
	Commit        string    `json:"commit,omitzero"`
	BuildTime     time.Time `json:"build_time,omitzero"`
	SourceTime    time.Time `json:"source_time,omitzero"`
	Distribution  string    `json:"distribution,omitzero"`
}

// Check runs the binary and compares its JSON provenance with want. Commit
// hashes compare case-insensitively, and timestamps compare in UTC. All other
// fields must match exactly.
func Check(ctx context.Context, path string, args []string, want Info) error {
	_, hexErr := hex.DecodeString(want.Commit)
	switch {
	case want.SchemaVersion != SchemaVersion:
		return fmt.Errorf("invalid expectation: unsupported schema_version: %d (want %d)", want.SchemaVersion, SchemaVersion)
	case want.Version == "":
		return errors.New("invalid expectation: version is required")
	case (len(want.Commit) != 40 && len(want.Commit) != 64) || hexErr != nil:
		return errors.New("invalid expectation: commit must be a full hex hash")
	case want.Distribution == "":
		return errors.New("invalid expectation: distribution is required")
	case want.BuildTime.IsZero() && want.SourceTime.IsZero():
		return errors.New("invalid expectation: build_time or source_time is required")
	}
	if !filepath.IsAbs(path) && filepath.Dir(path) == "." {
		path = "." + string(os.PathSeparator) + path
	}
	out, err := binaryprobe.Run(ctx, path, args)
	if err != nil {
		return fmt.Errorf("run provenance command: %w", err)
	}
	var got Info
	if err := json.Unmarshal(out, &got); err != nil {
		prefix := out[:min(len(out), 200)]
		return fmt.Errorf("decode provenance JSON: %w; got: %q", err, prefix)
	}
	if got.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported observed schema_version: %d (want %d)", got.SchemaVersion, SchemaVersion)
	}
	got.BuildTime, got.SourceTime = got.BuildTime.UTC(), got.SourceTime.UTC()
	want.BuildTime, want.SourceTime = want.BuildTime.UTC(), want.SourceTime.UTC()
	if strings.EqualFold(got.Commit, want.Commit) {
		got.Commit = want.Commit
	}
	if got != want {
		expected, err := json.Marshal(want)
		if err != nil {
			return fmt.Errorf("encode expectation: %w", err)
		}
		return fmt.Errorf("provenance mismatch:\n got: %s\nwant: %s", out, expected)
	}
	return nil
}
