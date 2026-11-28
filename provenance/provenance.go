// Package provenance defines a release binary's version record and checks it.
package provenance

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/kit/internal/binaryprobe"
)

// Info is the machine-readable provenance reported by a binary.
type Info struct {
	Version      string    `json:"version,omitzero"`
	Commit       string    `json:"commit,omitzero"`
	BuildTime    time.Time `json:"build_time,omitzero"`
	SourceTime   time.Time `json:"source_time,omitzero"`
	Distribution string    `json:"distribution,omitzero"`
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
}

// Check runs the binary and compares its JSON provenance with want.
func Check(ctx context.Context, path string, args []string, want Want) error {
	out, err := binaryprobe.Run(ctx, path, args)
	if err != nil {
		return fmt.Errorf("run provenance command: %w", err)
	}
	var raw struct {
		Version      string          `json:"version"`
		Commit       string          `json:"commit"`
		BuildTime    json.RawMessage `json:"build_time"`
		SourceTime   json.RawMessage `json:"source_time"`
		Distribution string          `json:"distribution"`
	}
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
	if raw.Distribution != want.Distribution {
		problems = append(problems, "distribution mismatch")
	}
	parseTime := func(name string, value json.RawMessage, required bool) time.Time {
		if len(value) == 0 || string(value) == "null" {
			if required {
				problems = append(problems, name+" is missing")
			}
			return time.Time{}
		}
		var text string
		if json.Unmarshal(value, &text) != nil || !strings.HasSuffix(text, "Z") {
			problems = append(problems, name+" must be UTC RFC 3339")
			return time.Time{}
		}
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil || parsed.IsZero() {
			problems = append(problems, name+" must be UTC RFC 3339")
			return time.Time{}
		}
		return parsed
	}
	if want.Time != BuildTime && want.Time != SourceTime {
		problems = append(problems, "invalid time kind")
	}
	build := parseTime("build_time", raw.BuildTime, want.Time == BuildTime)
	source := parseTime("source_time", raw.SourceTime, want.Time == SourceTime)
	if !build.IsZero() && !source.IsZero() && build.Equal(source) {
		problems = append(problems, "build_time and source_time must differ")
	}
	if len(problems) > 0 {
		return fmt.Errorf("invalid provenance: %s", strings.Join(problems, "; "))
	}
	return nil
}
