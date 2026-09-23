package fslink

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Kind classifies a directory entry by whether it redirects to another path.
type Kind uint8

const (
	// NotLink is a directory entry that does not redirect elsewhere.
	NotLink Kind = iota
	// Symlink is a symbolic link.
	Symlink
	// Junction is a Windows directory junction.
	Junction
	// OtherLink is another Windows name-surrogate reparse point, such as a
	// volume mount point or an unrecognized name-surrogate tag.
	OtherLink
)

// String returns a lower-case name for k.
func (k Kind) String() string {
	switch k {
	case NotLink:
		return "not-link"
	case Symlink:
		return "symlink"
	case Junction:
		return "junction"
	case OtherLink:
		return "other-link"
	default:
		return fmt.Sprintf("Kind(%d)", uint8(k))
	}
}

// ErrIsLink reports that an open refused to follow a link. Functions return
// it wrapped in an *fs.PathError naming the path.
var ErrIsLink = errors.New("fslink: path is a symlink or junction")

var (
	errNotLink    = errors.New("fslink: not a symlink or junction")
	errNotLocal   = errors.New("fslink: path is not local to the root")
	errNotRegular = errors.New("fslink: not a regular file")
	errChanged    = errors.New("fslink: path changed while it was being opened")
)

// Classify reports the link kind of path's final component without
// following it. Links in earlier components are followed. A missing path
// returns an error wrapping fs.ErrNotExist.
func Classify(path string) (Kind, error) {
	return classify(path)
}

// IsLink reports whether path's final component is any kind of link.
func IsLink(path string) (bool, error) {
	kind, err := Classify(path)
	if err != nil {
		return false, err
	}
	return kind != NotLink, nil
}

// Readlink returns the destination of the link at path. It returns an error
// when path is not a link on every platform.
func Readlink(path string) (string, error) {
	kind, err := Classify(path)
	if err != nil {
		return "", err
	}
	if kind == NotLink {
		return "", &fs.PathError{Op: "readlink", Path: path, Err: errNotLink}
	}
	return os.Readlink(path)
}

// CreateJunction creates a Windows directory junction at link that points to
// target. target is made absolute with filepath.Abs and must be an existing
// directory on a local volume. Creating a junction requires no special
// privilege. On other platforms it returns an error wrapping
// errors.ErrUnsupported.
func CreateJunction(target, link string) error {
	return createJunction(target, link)
}

// LinkDir creates a link at link to the directory target and reports the
// kind it created. On Unix it creates a symbolic link. On Windows it always
// creates a directory symbolic link, even when target does not exist yet, and
// falls back to a junction when the process lacks the symlink privilege. The
// junction fallback resolves target the way the symlink would have (relative
// to link's parent directory, or a rooted `\dir` on link's volume) and, like
// CreateJunction, requires target to be an existing directory.
func LinkDir(target, link string) (Kind, error) {
	return linkDir(target, link)
}

// OpenFile is like os.OpenFile but refuses to follow a link in path's final
// component, returning an error wrapping ErrIsLink. Links in earlier
// components are followed. O_TRUNC never truncates a link's destination.
func OpenFile(path string, flag int, perm fs.FileMode) (*os.File, error) {
	return openFile(path, flag, perm)
}

// OpenRegular opens path read-only without following a link in its final
// component and requires the opened file to be a regular file. Opening a
// FIFO does not block.
func OpenRegular(path string) (*os.File, error) {
	return openRegular(path)
}

// ReadFile reads the regular file at path without following a link in its
// final component.
func ReadFile(path string) (data []byte, err error) {
	file, err := OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		err = errors.Join(err, file.Close())
	}()
	return io.ReadAll(file)
}

// OpenInRoot opens name inside root like root.OpenFile, but refuses a link
// in any component of name, returning an error wrapping ErrIsLink. name must
// satisfy filepath.IsLocal.
//
// Each component is inspected without following it, opened through root,
// and then compared with the inspected entry by file identity. A link swapped
// in between the two steps makes the open fail instead of silently following
// it. O_TRUNC is applied only after that check, and O_CREATE creates only
// with O_EXCL semantics, so a racing link never causes a truncation or a
// create through it. Residual races: a swapped-in link is still followed for
// the open itself before the identity check rejects it, so an in-root
// destination may observe an open (for example, a FIFO may block the caller),
// and an O_CREATE open fails rather than retries when an entry appears
// between inspection and creation.
func OpenInRoot(root *os.Root, name string, flag int, perm fs.FileMode) (*os.File, error) {
	parts, err := localParts(name)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return root.OpenFile(".", flag, perm)
	}
	parent, err := openDirs(root, parts[:len(parts)-1])
	if err != nil {
		return nil, pathError("open", name, err)
	}
	if parent != root {
		defer func() { _ = parent.Close() }()
	}
	file, err := openFinal(parent, parts[len(parts)-1], flag, perm)
	if err != nil {
		return nil, pathError("open", name, err)
	}
	return file, nil
}

// OpenRootNoFollow opens the directory name inside root as a new root,
// refusing a link in any component of name. name must satisfy
// filepath.IsLocal. The same identity check and residual race described on
// OpenInRoot apply to every component.
func OpenRootNoFollow(root *os.Root, name string) (*os.Root, error) {
	parts, err := localParts(name)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return root.OpenRoot(".")
	}
	dir, err := openDirs(root, parts)
	if err != nil {
		return nil, pathError("openroot", name, err)
	}
	return dir, nil
}

func localParts(name string) ([]string, error) {
	if !filepath.IsLocal(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: errNotLocal}
	}
	clean := filepath.Clean(name)
	if clean == "." {
		return nil, nil
	}
	return strings.Split(clean, string(filepath.Separator)), nil
}

// openDirs walks parts below root, returning root itself when parts is empty.
// Intermediate roots are closed; the caller owns a returned root that is not
// root.
func openDirs(root *os.Root, parts []string) (*os.Root, error) {
	dir := root
	for _, part := range parts {
		next, err := openDir(dir, part)
		if dir != root {
			_ = dir.Close()
		}
		if err != nil {
			return nil, err
		}
		dir = next
	}
	return dir, nil
}

func openDir(dir *os.Root, name string) (*os.Root, error) {
	ent, err := lstatAt(dir, name)
	if err != nil {
		return nil, err
	}
	if !ent.isDir() {
		return nil, syscall.ENOTDIR
	}
	sub, err := dir.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	self, err := sub.Open(".")
	if err == nil {
		err = checkSame(ent, self)
		err = errors.Join(err, self.Close())
	}
	if err != nil {
		_ = sub.Close()
		return nil, err
	}
	return sub, nil
}

func openFinal(dir *os.Root, name string, flag int, perm fs.FileMode) (*os.File, error) {
	if flag&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL {
		// An exclusive create fails on any existing entry, links included.
		return dir.OpenFile(name, flag, perm)
	}
	ent, err := lstatAt(dir, name)
	if errors.Is(err, fs.ErrNotExist) && flag&os.O_CREATE != 0 {
		return dir.OpenFile(name, flag|os.O_EXCL, perm)
	}
	if err != nil {
		return nil, err
	}
	file, err := dir.OpenFile(name, flag&^(os.O_CREATE|os.O_TRUNC), perm)
	if err != nil {
		return nil, err
	}
	if err := checkSame(ent, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	if flag&os.O_TRUNC != 0 {
		if err := file.Truncate(0); err != nil {
			_ = file.Close()
			return nil, err
		}
	}
	return file, nil
}

func checkSame(ent entry, file *os.File) error {
	same, err := ent.sameFile(file)
	if err != nil {
		return err
	}
	if !same {
		return errChanged
	}
	return nil
}

// pathError reports err against the caller's full name, keeping the
// underlying cause rather than an inner component's PathError.
func pathError(op, name string, err error) error {
	if pathErr, ok := errors.AsType[*fs.PathError](err); ok {
		err = pathErr.Err
	}
	return &fs.PathError{Op: op, Path: name, Err: err}
}
