// Package atomicfile writes files so readers see either the old content or
// the new content, never a partial file.
//
// WriteFile and Create stage the new content in a temporary file next to the
// target, fsync it, and rename it over the target. WriteNew publishes only
// when nothing exists at the target. Both refuse to replace a symlink or
// junction at the target unless WithFollowLink asks to write through it, and
// neither ever falls back to copying across volumes.
package atomicfile
