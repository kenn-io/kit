package provenance

import (
	"context"
	"encoding/hex"
	"encoding/json"
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

// TimeKind selects the timestamp required by Check.
type TimeKind uint8

const (
	// BuildTime requires the time at which the binary was built.
	BuildTime TimeKind = iota + 1
	// SourceTime requires the source revision's time.
	SourceTime
)

// Want specifies the expected values for a release binary.
type Want struct {
	Version, Commit, Distribution string
	Time                          TimeKind
	// ExpectedTime is the nonzero expected instant for the selected Time.
	ExpectedTime time.Time
}

// Check runs the binary and compares its JSON provenance with want.
func Check(ctx context.Context, path string, args []string, want Want) error {
	if !filepath.IsAbs(path) && filepath.Dir(path) == "." {
		path = "." + string(os.PathSeparator) + path
	}
	out, err := binaryprobe.Run(ctx, path, args)
	if err != nil {
		return fmt.Errorf("run provenance command: %w", err)
	}
	var raw Info
	if err := json.Unmarshal(out, &raw); err != nil {
		return fmt.Errorf("decode provenance JSON: %w", err)
	}
	var problems []string
	if strings.TrimPrefix(want.Version, "v") == "" || strings.TrimPrefix(raw.Version, "v") != strings.TrimPrefix(want.Version, "v") {
		problems = append(problems, "version mismatch")
	}
	_, hexErr := hex.DecodeString(want.Commit)
	if (len(want.Commit) != 40 && len(want.Commit) != 64) || hexErr != nil || raw.Commit != want.Commit {
		problems = append(problems, "commit must match the full expected hash")
	}
	if raw.SchemaVersion != SchemaVersion {
		problems = append(problems, "schema_version must be 1")
	}
	if want.Distribution == "" {
		problems = append(problems, "expected distribution is required")
	} else if raw.Distribution != want.Distribution {
		problems = append(problems, "distribution mismatch")
	}
	var selected time.Time
	var name string
	switch want.Time {
	case BuildTime:
		name, selected = "build_time", raw.BuildTime
	case SourceTime:
		name, selected = "source_time", raw.SourceTime
	default:
		problems = append(problems, "time kind must select build_time or source_time")
	}
	if want.ExpectedTime.IsZero() {
		problems = append(problems, "expected time is required")
	}
	if name != "" {
		switch {
		case selected.IsZero():
			problems = append(problems, name+" is missing or zero")
		case !want.ExpectedTime.IsZero() && !selected.Equal(want.ExpectedTime):
			problems = append(problems, name+" does not match expected time")
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid provenance: %s", strings.Join(problems, "; "))
	}
	return nil
}
