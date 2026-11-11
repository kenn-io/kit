package secretref

import (
	"fmt"
	"os"
	"strings"
)

// Fields is the legacy sibling-key representation of one secret.
// New configuration keys should use Ref directly.
type Fields struct{ Value, File, Env string }

// FromFields converts base, base_file and base_env without resolving them.
// Multiple nonempty fields are an error, never a precedence choice.
func FromFields(base string, fields Fields) (Ref, error) {
	ref := Ref{Value: fields.Value, File: fields.File, Env: fields.Env}
	if err := ref.Validate(); err != nil {
		return ref, fmt.Errorf("secretref: %s: %w", base, err)
	}
	return ref, nil
}

// FromEnv selects NAME or NAME_FILE using presence, including empty values.
// Both present is an error; neither present returns an unset Ref. Selection
// does no file I/O. Resolve reports an empty selected NAME as a source error.
// Names must be nonempty, have no surrounding whitespace, and contain no = or NUL.
// File selectors must have no surrounding whitespace.
func FromEnv(name string) (Ref, error) {
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, "=\x00") {
		return Ref{}, &SourceError{Source: "environment", Reason: "invalid environment variable name"}
	}
	_, set := os.LookupEnv(name)
	path, fileSet := os.LookupEnv(name + "_FILE")
	if set && fileSet {
		return Ref{}, fmt.Errorf("secretref: %s and %s_FILE cannot both be set", name, name)
	}
	if set {
		return Ref{Env: name}, nil
	}
	if !fileSet {
		return Ref{}, nil
	}
	if path == "" {
		return Ref{}, &SourceError{Source: "env:" + name + "_FILE", Reason: "file selector is empty"}
	}
	if strings.TrimSpace(path) != path {
		return Ref{}, &SourceError{Source: "env:" + name + "_FILE", Reason: "file selector has surrounding whitespace"}
	}
	return Ref{File: path}, nil
}
