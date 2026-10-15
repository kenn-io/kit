package objectstore

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// ValidateKey accepts slash-separated, relative object names without empty,
// dot, or parent segments. Backslashes, colons, NULs, and platform-reserved
// paths are rejected so a key cannot select an absolute path or alternate
// stream when mapped to disk. Validation does not resolve filesystem links.
func ValidateKey(key string) error {
	if !fs.ValidPath(key) || key == "." || strings.ContainsAny(key, "\\:\x00") || !filepath.IsLocal(filepath.FromSlash(key)) {
		return fmt.Errorf("objectstore: invalid key %q", key)
	}
	return nil
}

// ValidatePrefix accepts an empty prefix, a partial key, or a key followed by
// a slash. Prefix matching compares names literally without path cleaning.
func ValidatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	return ValidateKey(strings.TrimSuffix(prefix, "/"))
}
