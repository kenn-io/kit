package secretref

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// Secret holds resolved bytes. Access Value explicitly to use the secret;
// default formatting, JSON encoding and structured logging redact it.
type Secret struct {
	Value  string
	Source string
	// Reason describes why a secret is unavailable.
	Reason string
}

func (s Secret) String() string   { return s.Source + ": [redacted]" }
func (s Secret) GoString() string { return s.String() }
func (s Secret) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Source string `json:"source"`
		Value  string `json:"value"`
	}{s.Source, "[redacted]"})
}

func (s Secret) LogValue() slog.Value {
	return slog.GroupValue(slog.String("source", s.Source), slog.String("value", "[redacted]"))
}

// SourceError separates a loggable source selector from a public-safe reason.
// Reason never contains a path, environment variable name or secret value.
type SourceError struct {
	Source string
	Reason string
}

func (e *SourceError) Error() string { return fmt.Sprintf("secretref: %s: %s", e.Source, e.Reason) }
