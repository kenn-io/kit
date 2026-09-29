// Package secretref reads secrets named by a single configuration value.
//
// A Ref says where a secret comes from, so one config key covers every
// source:
//
//	api_key = "env:OPENAI_API_KEY"       # environment variable
//	api_key = "file:~/.config/app.key"   # private file; ~/ is home
//	api_key = "sk-inline-value"          # the secret itself
//
// A value shaped like "scheme:rest" with a scheme this package does not know
// is rejected rather than read as an inline secret, so adding a scheme later
// never changes what an existing value means.
package secretref

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.kenn.io/kit/safefileio"
)

// Ref is a configuration value that holds a secret or names its source.
type Ref string

// Secret is a resolved Ref. Value goes only to the code that uses the
// secret. Source names where it came from ("inline", "env:NAME", or
// "file:PATH") and never contains the value. Reason explains why a source
// produced no value.
type Secret struct {
	Value  string
	Source string
	Reason string
}

// Validate reports a malformed reference without reading any secret.
func (r Ref) Validate() error {
	_, _, err := r.parse()
	return err
}

// IsSet reports whether the reference names any source.
func (r Ref) IsSet() bool { return r != "" }

// Resolve reads the secret. A source that yields nothing is not an error:
// Value is empty and Reason says why, so callers can keep running without
// the secret and report the reason. An empty Ref resolves to an empty
// Secret. Only a malformed reference is an error.
//
// A file must be a regular, private file owned by the current user, as
// safefileio verifies; a symlink or FIFO is refused without blocking.
// Trailing newlines are removed.
func (r Ref) Resolve() (Secret, error) {
	scheme, rest, err := r.parse()
	if err != nil {
		return Secret{}, err
	}
	switch scheme {
	case "":
		if r == "" {
			return Secret{}, nil
		}
		if strings.TrimSpace(rest) == "" {
			return Secret{Source: "inline", Reason: "inline value is empty"}, nil
		}
		return Secret{Value: rest, Source: "inline"}, nil
	case "env":
		source := "env:" + rest
		value := os.Getenv(rest)
		if strings.TrimSpace(value) == "" {
			return Secret{Source: source, Reason: "env " + rest + " is unset or empty"}, nil
		}
		return Secret{Value: value, Source: source}, nil
	default:
		return resolveFile(rest), nil
	}
}

// parse splits a reference into a known scheme and its argument. An inline
// value has no scheme.
func (r Ref) parse() (string, string, error) {
	value := string(r)
	scheme, rest, found := strings.Cut(value, ":")
	if !found || !isScheme(scheme) {
		return "", value, nil
	}
	rest = strings.TrimSpace(rest)
	switch scheme {
	case "env", "file":
		if rest == "" {
			return "", "", fmt.Errorf("secretref: %s: reference is empty", scheme)
		}
		return scheme, rest, nil
	default:
		return "", "", fmt.Errorf("secretref: unknown scheme %q; use env:, file:, or an inline value", scheme)
	}
}

// isScheme matches a lowercase word, the shape of a reference scheme. Inline
// secrets rarely start that way; one that does must not use a colon there.
func isScheme(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

func resolveFile(configured string) Secret {
	unavailable := func(reason string) Secret {
		return Secret{Source: "file:" + configured, Reason: reason}
	}
	path := configured
	if configured == "~" || strings.HasPrefix(configured, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return unavailable("file home directory is unavailable")
		}
		path = filepath.Join(home, strings.TrimPrefix(configured, "~"))
	}
	file, err := safefileio.OpenCurrentUserFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return unavailable("file is missing")
		}
		return unavailable("file must be a regular file owned by the current user")
	}
	defer func() { _ = file.Close() }()
	if err := safefileio.ValidatePrivateCurrentUserFile(file); err != nil {
		return unavailable("file must be private to its owner (mode 0600)")
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return unavailable("file is unreadable")
	}
	value := strings.TrimRight(string(data), "\r\n")
	if strings.TrimSpace(value) == "" {
		return unavailable("file is empty")
	}
	return Secret{Value: value, Source: "file:" + configured}
}
