// Package pathresolve canonicalizes filesystem paths for identity and
// containment checks.
package pathresolve

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
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

// EvalSymlinksAllowMissing resolves existing symbolic links and Windows junctions
// to an absolute path, retaining any missing target or trailing components.
// Unlike EvalSymlinks, it can compare paths before their files are created.
// Cycles, inaccessible paths, and non-directory parent components still fail.
func EvalSymlinksAllowMissing(path string) (string, error) {
	resolved, _, err := resolveReparsePoints(path)
	if err != nil {
		return "", err
	}
	canonical, originalErr := EvalSymlinks(resolved)
	if originalErr == nil {
		return canonical, nil
	}
	var tail string
	for current := resolved; ; current = filepath.Dir(current) {
		info, statErr := os.Lstat(current)
		if statErr == nil {
			if tail != "" && !info.IsDir() {
				return "", originalErr
			}
			canonical, err := EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			return filepath.Join(canonical, tail), nil
		}
		if !errors.Is(statErr, os.ErrNotExist) || filepath.Dir(current) == current {
			return "", statErr
		}
		tail = filepath.Join(filepath.Base(current), tail)
	}
}

// resolveReparsePoints rewrites path element by element, following each
// reparse point os.Readlink can read until no element is one. It reports
// whether it followed at least one, so callers can tell a junction-free path
// from a resolved one.
func resolveReparsePoints(path string) (string, bool, error) {
	absolute, err := absoluteWithoutCleaning(path)
	if err != nil {
		return "", false, err
	}
	current := absolute
	followed := false
	for range maxLinks {
		next, ok, err := followFirstReparsePoint(current)
		if err != nil {
			return "", false, err
		}
		if !ok {
			return next, followed, nil
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
func followFirstReparsePoint(current string) (string, bool, error) {
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
		prefix = filepath.Join(prefix, tail[start:i])
		info, err := os.Lstat(prefix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", false, err
		}
		target, err := os.Readlink(prefix)
		if err != nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return "", false, err
			}
			if i < len(tail) && !info.IsDir() {
				return "", false, &os.PathError{Op: "lstat", Path: prefix, Err: syscall.ENOTDIR}
			}
			continue
		}
		switch {
		case filepath.IsAbs(target):
		case filepath.VolumeName(target) == "" && target != "" && os.IsPathSeparator(target[0]):
			// A rooted target such as `\shared` names the link's own volume.
			target = volume + target
		case filepath.VolumeName(target) != "":
			target, err = absoluteWithoutCleaning(target)
			if err != nil {
				return "", false, err
			}
		default:
			target = filepath.Dir(prefix) + string(filepath.Separator) + target
		}
		// Keep target and suffix components intact until their links are followed.
		return target + tail[i:], true, nil
	}
	return prefix, false, nil
}

// absoluteWithoutCleaning anchors relative paths without collapsing link/.. .
func absoluteWithoutCleaning(path string) (string, error) {
	if filepath.IsAbs(path) {
		return path, nil
	}
	volume := filepath.VolumeName(path)
	tail := path[len(volume):]
	if tail != "" && os.IsPathSeparator(tail[0]) {
		root, err := filepath.Abs(string(filepath.Separator))
		return filepath.VolumeName(root) + tail, err
	}
	base, err := filepath.Abs(volume + ".")
	if err != nil {
		return "", err
	}
	return base + string(filepath.Separator) + tail, nil
}
