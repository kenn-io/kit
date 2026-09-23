// Package fsname checks and cleans file and directory names so they work on
// every modern operating system and file system, not just the host's.
//
// Use Check to reject a name a caller supplied, Clean to derive a usable name
// from an outside string such as an attachment or export title, and Join to
// build a root-relative path from untrusted parts for fslink.OpenInRoot.
// Check never renames: a name that is not portable is an error. The rules come
// from github.com/spf13/pathologize, plus Windows console device names it does
// not yet cover.
package fsname
