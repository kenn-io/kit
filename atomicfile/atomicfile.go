package atomicfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"go.kenn.io/kit/fslink"
	"go.kenn.io/kit/safefileio"
)

// maxLinkHops bounds WithFollowLink resolution, matching the common Unix
// SYMLOOP_MAX.
const maxLinkHops = 40

var (
	errTooManyLinks = errors.New("too many levels of symlinks or junctions")
	errIsDir        = errors.New("target is a directory")
	errFinished     = errors.New("file already committed or aborted")
)

// WriteFile atomically replaces the file at path with data. It stages data in
// a temporary file, fsyncs and closes it, renames it over path, and fsyncs
// path's directory. Readers see the old or the new content, never a partial
// file. Parent directories are not created.
func WriteFile(path string, data []byte, opts ...Option) (err error) {
	file, err := Create(path, opts...)
	if err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, file.Abort())
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("atomicfile: write %s: %w", path, err)
	}
	return file.Commit()
}

// File is a staged replacement for a target file. Content written to it
// becomes visible at the target only when Commit succeeds. Callers should
// defer Abort, which is a no-op after Commit.
type File struct {
	file   *os.File
	path   string
	target string
	cfg    config
	done   bool
}

// Create stages a replacement for the file at path. See WriteFile for the
// publication guarantees. The target is checked now and again at Commit: a
// link (unless WithFollowLink) or a directory at the target fails. The
// rename itself never follows a link, so a link swapped in after the Commit
// check is replaced rather than written through.
func Create(path string, opts ...Option) (*File, error) {
	cfg, err := newConfig(opts)
	if err != nil {
		return nil, fmt.Errorf("atomicfile: create %s: %w", path, err)
	}
	target, err := resolveTarget(path, cfg.followLink)
	if err != nil {
		return nil, fmt.Errorf("atomicfile: create %s: %w", path, err)
	}
	info, err := checkTarget(target)
	if err != nil {
		return nil, fmt.Errorf("atomicfile: create %s: %w", path, err)
	}
	perm := cfg.perm
	if cfg.preserveMode && info != nil && info.Mode().IsRegular() {
		perm = info.Mode().Perm()
	}
	staged, err := stage(cfg, target, perm)
	if err != nil {
		return nil, fmt.Errorf("atomicfile: create %s: %w", path, err)
	}
	return &File{file: staged, path: path, target: target, cfg: cfg}, nil
}

// Name returns the target path passed to Create.
func (f *File) Name() string { return f.path }

// TempName returns the path of the staging file.
func (f *File) TempName() string { return f.file.Name() }

// Write writes to the staging file.
func (f *File) Write(p []byte) (int, error) { return f.file.Write(p) }

// WriteString writes s to the staging file.
func (f *File) WriteString(s string) (int, error) { return f.file.WriteString(s) }

// ReadFrom copies r into the staging file.
func (f *File) ReadFrom(r io.Reader) (int64, error) { return f.file.ReadFrom(r) }

// Commit fsyncs and closes the staging file, renames it over the target, and
// fsyncs the target's directory. On failure before the rename the staging
// file is removed and the target is untouched. Commit may be called once;
// later calls, or a call after Abort, return an error.
func (f *File) Commit() error {
	if f.done {
		return fmt.Errorf("atomicfile: commit %s: %w", f.path, errFinished)
	}
	f.done = true
	if err := f.publish(); err != nil {
		return fmt.Errorf("atomicfile: commit %s: %w", f.path, err)
	}
	return nil
}

func (f *File) publish() error {
	err := finishStaged(f.file, f.cfg)
	if err == nil {
		_, err = checkTarget(f.target)
	}
	if err == nil {
		err = replaceFile(f.file.Name(), f.target)
	}
	if err != nil {
		return discardStaged(f.file, err)
	}
	if f.cfg.noSync {
		return nil
	}
	return syncPublishedDir(f.target)
}

// syncPublishedDir reports a directory sync failure as one that happened
// after publication, so callers do not treat the published file as absent.
func syncPublishedDir(path string) error {
	if err := SyncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("published, but directory sync failed (entry may not be durable): %w", err)
	}
	return nil
}

// Abort closes and removes the staging file, leaving the target untouched.
// It is a no-op after Commit or a previous Abort, so callers can defer it.
func (f *File) Abort() error {
	if f.done {
		return nil
	}
	f.done = true
	if err := discardStaged(f.file, nil); err != nil {
		return fmt.Errorf("atomicfile: abort %s: %w", f.path, err)
	}
	return nil
}

// WriteNew atomically creates the file at path with data only when nothing
// exists there. The staged file is fsynced and published with
// PublishNoReplace, the leftover staging name is removed, and path's
// directory is fsynced. An existing entry at path, including a link, fails
// with an error wrapping fs.ErrExist and is left untouched. WithFollowLink is
// rejected.
func WriteNew(path string, data []byte, opts ...Option) error {
	if err := writeNew(path, data, opts); err != nil {
		return fmt.Errorf("atomicfile: write new %s: %w", path, err)
	}
	return nil
}

func writeNew(path string, data []byte, opts []Option) error {
	cfg, err := newConfig(opts)
	if err != nil {
		return err
	}
	if cfg.followLink {
		return errors.New("WithFollowLink cannot be used with WriteNew")
	}
	staged, err := stage(cfg, path, cfg.perm)
	if err != nil {
		return err
	}
	if _, err := staged.Write(data); err != nil {
		return discardStaged(staged, err)
	}
	if err := finishStaged(staged, cfg); err != nil {
		return discardStaged(staged, err)
	}
	if err := PublishNoReplace(staged.Name(), path); err != nil {
		return discardStaged(staged, err)
	}
	if err := os.Remove(staged.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove staging file after publishing: %w", err)
	}
	if cfg.noSync {
		return nil
	}
	return syncPublishedDir(path)
}

// PublishNoReplace publishes staging at final only when final does not
// exist. It first hard-links staging to final, which leaves staging in place
// for the caller to remove. Where hard links are unavailable it falls back to
// RenameNoReplace. Neither step replaces or copies over an existing final;
// when both fail the joined error wraps fs.ErrExist if final existed.
func PublishNoReplace(staging, final string) error {
	linkErr := linkFile(staging, final)
	if linkErr == nil {
		return nil
	}
	if renameErr := RenameNoReplace(staging, final); renameErr != nil {
		return errors.Join(
			fmt.Errorf("atomicfile: publish %s: hard link: %w", final, linkErr),
			fmt.Errorf("atomicfile: publish %s: no-replace rename: %w", final, renameErr),
		)
	}
	return nil
}

// linkFile is replaced by tests to force the PublishNoReplace fallback.
var linkFile = os.Link

// resolveTarget returns the path whose entry a replace should swap. A link at
// path's final component is refused unless follow is set, in which case the
// chain is resolved.
func resolveTarget(path string, follow bool) (string, error) {
	current := path
	for range maxLinkHops + 1 {
		kind, err := fslink.Classify(current)
		if errors.Is(err, fs.ErrNotExist) {
			return current, nil
		}
		if err != nil {
			return "", err
		}
		if kind == fslink.NotLink {
			return current, nil
		}
		if !follow {
			return "", &fs.PathError{Op: "replace", Path: current, Err: fslink.ErrIsLink}
		}
		current, err = linkDest(current)
		if err != nil {
			return "", err
		}
	}
	return "", &fs.PathError{Op: "replace", Path: path, Err: errTooManyLinks}
}

// linkDest returns the path the link at link names, resolving a relative
// destination the way the system does: against the link's real parent
// directory, not its lexical one, since ".." in the destination climbs out of
// wherever links in the parent lead. A rooted destination without a volume
// (`\dir` on Windows) names a path on the link's volume.
func linkDest(link string) (string, error) {
	dest, err := fslink.Readlink(link)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(dest) {
		return dest, nil
	}
	dir, err := realDir(filepath.Dir(link))
	if err != nil {
		return "", err
	}
	if filepath.VolumeName(dest) == "" && dest != "" && os.IsPathSeparator(dest[0]) {
		return filepath.VolumeName(dir) + dest, nil
	}
	return filepath.Join(dir, dest), nil
}

// checkTarget fails when target's entry is a link or a directory and returns
// its Lstat info, or nil when it does not exist.
func checkTarget(target string) (fs.FileInfo, error) {
	kind, err := fslink.Classify(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if kind != fslink.NotLink {
		return nil, &fs.PathError{Op: "replace", Path: target, Err: fslink.ErrIsLink}
	}
	info, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, &fs.PathError{Op: "replace", Path: target, Err: errIsDir}
	}
	return info, nil
}

// stage creates the staging file for target with perm, or a private file
// when cfg.private is set.
func stage(cfg config, target string, perm fs.FileMode) (*os.File, error) {
	dir, pattern := filepath.Dir(target), "."+filepath.Base(target)+".tmp-*"
	if cfg.stagingDir != "" {
		dir, pattern = cfg.stagingDir, filepath.Base(target)+".tmp-*"
	}
	if cfg.private {
		return safefileio.CreatePrivateTemp(dir, pattern)
	}
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(perm); err != nil {
		return nil, discardStaged(file, err)
	}
	return file, nil
}

func finishStaged(file *os.File, cfg config) error {
	if !cfg.noSync {
		if err := file.Sync(); err != nil {
			return err
		}
	}
	return file.Close()
}

// discardStaged closes and removes a staging file, joining cleanup failures
// with cause. The file may already be closed.
func discardStaged(file *os.File, cause error) error {
	closeErr := file.Close()
	if errors.Is(closeErr, os.ErrClosed) {
		closeErr = nil
	}
	removeErr := os.Remove(file.Name())
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(cause, closeErr, removeErr)
}
