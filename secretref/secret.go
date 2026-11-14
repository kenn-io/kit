package secretref

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// Secret holds resolved bytes. Access Value explicitly to use the secret;
// direct formatting, JSON encoding and structured logging redact it. Formatting
// an unexported field containing a Secret can expose its value. Other encoders
// that inspect exported fields, such as TOML and YAML, can also expose it.
type Secret struct {
	Value  string
	Source string
	// Reason describes why a secret is unavailable.
	Reason string
}

func (s Secret) String() string   { return s.Source + ": [redacted]" }
func (s Secret) GoString() string { return s.String() }
func (s Secret) Format(state fmt.State, verb rune) {
	if verb == 'q' {
		_, _ = fmt.Fprintf(state, "%q", s.String())
		return
	}
	_, _ = state.Write([]byte(s.String()))
}

func (s Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Source string `json:"source"`
		Value  string `json:"value"`
		Reason string `json:"reason,omitempty"`
	}{s.Source, "[redacted]", s.Reason})
}

func (s Secret) LogValue() slog.Value {
	attrs := []slog.Attr{slog.String("source", s.Source), slog.String("value", "[redacted]")}
	if s.Reason != "" {
		attrs = append(attrs, slog.String("reason", s.Reason))
	}
	return slog.GroupValue(attrs...)
}

// SourceError separates a loggable source selector from a public-safe reason.
// Reason never contains a path, environment variable name or secret value.
type SourceError struct {
	Source string
	Reason string
}

func (e *SourceError) Error() string { return fmt.Sprintf("secretref: %s: %s", e.Source, e.Reason) }
