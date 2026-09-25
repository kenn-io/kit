// Package pathresolve canonicalizes filesystem paths for identity and
// containment checks.
package pathresolve

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// maxLinks bounds reparse-point hops so a link cycle fails instead of spinning.
const maxLinks = 255

// EvalSymlinks returns the cleaned, absolute form of path with symbolic links
// and Windows directory junctions resolved.
//
// Without a reparse point anywhere in the path, and on every platform other
// than Windows, the result is identical to filepath.EvalSymlinks, including
// its volume case normalization. filepath.EvalSymlinks still owns that
// canonicalization here; this package only decides what it is handed.
//
// On Windows the difference is deliberate. A directory junction is a reparse
// point tagged IO_REPARSE_TAG_MOUNT_POINT, and Go 1.23 stopped reporting it as
// one: with the winsymlink default, os.Lstat calls it ModeIrregular — neither a
// symlink (that tag is IO_REPARSE_TAG_SYMLINK) nor a directory — while
// os.Readlink still reads its target. filepath.walkSymlinks follows that
// classification, so it refuses to walk through a junction at all: every path
// *below* one fails with a bare syscall.ENOTDIR, which Windows renders as "The
// system cannot find the path specified", for a directory the OS opens without
// complaint. The same classification leaves a trailing junction unresolved. See
// https://go.dev/issue/63703 for that change and its intent.
//
// A caller that canonicalizes a path for identity or containment needs the
// location the OS opens, not the spelling it was given, so this package
// resolves the junction wherever it sits in the path. It follows reparse points
// with os.Readlink, which reads both tags. A caller that must not follow a link
// should pick something else: resolving is the point here.
//
// The walk is bounded, so reparse points that refer to each other cannot make
// it spin. A bound is reached only for a cycle, and the caller then gets
// whatever filepath.EvalSymlinks reports for the path it asked about — never a
// partial rewrite.
//
// A path that does not exist, or that has a non-directory element, reports the
// same error filepath.EvalSymlinks reports for the caller's own path.
func EvalSymlinks(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if runtime.GOOS != "windows" {
		return resolved, err
	}
	junctionFree, followed, resolveErr := resolveReparsePoints(path)
	if resolveErr != nil || !followed {
		return resolved, err
	}
	if canonical, canonicalErr := filepath.EvalSymlinks(junctionFree); canonicalErr == nil {
		return canonical, nil
	}
	return resolved, err
}

// resolveReparsePoints rewrites path element by element, following each
// reparse point os.Readlink can read until no element is one. It reports
// whether it followed at least one, so callers can tell a junction-free path
// from a resolved one.
func resolveReparsePoints(path string) (string, bool, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", false, err
	}
	current := filepath.Clean(absolute)
	followed := false
	for range maxLinks {
		next, ok := followFirstReparsePoint(current)
		if !ok {
			return current, followed, nil
		}
		current = next
		followed = true
	}
	return "", false, errors.New("pathresolve: too many levels of reparse points")
}

// followFirstReparsePoint returns current with its first reparse-point element
// replaced by that element's target. It reports false when no element is a
// reparse point.
//
// os.Lstat cannot see a junction, so the reparse points here are found with
// os.Readlink, which reads the target of both tag kinds.
func followFirstReparsePoint(current string) (string, bool) {
	volume := filepath.VolumeName(current)
	tail := current[len(volume):]
	prefix := volume
	if tail != "" && os.IsPathSeparator(tail[0]) {
		prefix += string(filepath.Separator)
	}
	for i := 0; i < len(tail); {
		for i < len(tail) && os.IsPathSeparator(tail[i]) {
			i++
		}
		start := i
		for i < len(tail) && !os.IsPathSeparator(tail[i]) {
			i++
		}
		if start == i {
			break
		}
		if len(prefix) > 0 && !os.IsPathSeparator(prefix[len(prefix)-1]) {
			prefix += string(filepath.Separator)
		}
		prefix += tail[start:i]

		target, err := os.Readlink(prefix)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(prefix), target)
		}
		return filepath.Clean(target + tail[i:]), true
	}
	return current, false
}
