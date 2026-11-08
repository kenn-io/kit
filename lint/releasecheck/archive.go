package releasecheck

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// Bound memory/entry counts while accommodating large native runtime bundles.
const (
	maxMemberBytes  int64 = 512 << 20
	maxPayloadBytes int64 = 2 << 30
	maxEntries            = 100000
)

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

func openRegular(name string) (*os.File, error) {
	stat, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !stat.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", name)
	}
	return os.Open(name)
}

func readChecksums(ctx context.Context, dir string, artifacts []Artifact) (map[string]string, error) {
	f, err := openRegular(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return nil, fmt.Errorf("open SHA256SUMS: %w", err)
	}
	defer f.Close()
	expected := map[string]bool{}
	for _, a := range artifacts {
		expected[a.Name] = true
	}
	sums := map[string]string{}
	scanner := bufio.NewScanner(contextReader{ctx, f})
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		candidate := strings.TrimPrefix(strings.TrimPrefix(fields[1], "*"), "./")
		if !expected[candidate] {
			continue
		} // Ancillary source/signature checksums belong to the consumer.
		if _, ok := sums[candidate]; ok {
			return nil, fmt.Errorf("duplicate checksum for %s", candidate)
		}
		if len(line) < 66 || len(fields) != 2 || line[64] != ' ' || (line[65] != ' ' && line[65] != '*') {
			return nil, fmt.Errorf("invalid checksum entry for %s", candidate)
		}
		name := strings.TrimPrefix(line[66:], "./")
		digest, err := hex.DecodeString(line[:64])
		if err != nil || len(digest) != sha256.Size || name != candidate {
			return nil, fmt.Errorf("invalid checksum entry for %s", candidate)
		}
		sums[candidate] = strings.ToLower(line[:64])
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read SHA256SUMS: %w", err)
	}
	for _, a := range artifacts {
		if sums[a.Name] == "" {
			return nil, fmt.Errorf("missing checksum for %s", a.Name)
		}
	}
	return sums, nil
}

type member struct {
	data []byte
	mode fs.FileMode
}

type archiveMembers struct {
	ctx             context.Context
	files           map[string]member
	seen            map[string]bool
	pathKinds       map[string]bool
	size            int64
	windows         bool
	caseInsensitive bool
}

func (m *archiveMembers) add(name string, mode fs.FileMode, size int64, r io.Reader) error {
	if err := m.ctx.Err(); err != nil {
		return err
	}
	if name == "" || path.IsAbs(name) {
		return fmt.Errorf("unsafe archive path %q", name)
	}
	for strings.HasPrefix(name, "./") {
		name = strings.TrimPrefix(name, "./")
	}
	if mode.IsDir() {
		name = strings.TrimSuffix(name, "/")
		if name == "" || name == "." {
			if m.seen["."] {
				return errors.New("duplicate archive root directory")
			}
			m.seen["."] = true
			return nil
		}
	}
	if !targetMemberPath(name, m.windows) {
		return fmt.Errorf("unsafe archive path %q", name)
	}
	canonical := name
	if m.caseInsensitive {
		canonical = strings.ToLower(name)
	}
	if m.seen[canonical] {
		return fmt.Errorf("duplicate archive path %q", name)
	}
	m.seen[canonical] = true
	if len(m.seen) > maxEntries {
		return errors.New("archive has too many entries")
	}
	if !mode.IsRegular() && !mode.IsDir() {
		return fmt.Errorf("links and special archive members are forbidden: %s", name)
	}
	// A regular file cannot also be a directory prefix, even through case aliases.
	for parent := path.Dir(canonical); parent != "."; parent = path.Dir(parent) {
		if isDir, ok := m.pathKinds[parent]; ok && !isDir {
			return fmt.Errorf("file is also directory prefix: %s", name)
		}
		m.pathKinds[parent] = true
	}
	if !mode.IsDir() && m.pathKinds[canonical] {
		return fmt.Errorf("file is also directory prefix: %s", name)
	}
	m.pathKinds[canonical] = mode.IsDir()
	fileKey := name
	if m.windows {
		fileKey = canonical
	}
	if mode.IsDir() {
		m.files[fileKey] = member{mode: mode}
		return nil
	}
	if size < 0 || size > maxMemberBytes || m.size+size > maxPayloadBytes {
		return fmt.Errorf("archive payload limit exceeded: %s", name)
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{m.ctx, r}, size+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	if int64(len(data)) != size {
		return fmt.Errorf("invalid size for %s", name)
	}
	m.size += size
	m.files[fileKey] = member{data: data, mode: mode}
	return nil
}

func checkArchive(ctx context.Context, dir string, a Artifact, sum string) error {
	f, err := openRegular(filepath.Join(dir, a.Name))
	if err != nil {
		return err
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx, f}); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != sum {
		return errors.New("SHA-256 checksum mismatch")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	members := archiveMembers{
		ctx:             ctx,
		files:           map[string]member{},
		seen:            map[string]bool{},
		pathKinds:       map[string]bool{},
		windows:         a.OS == "windows",
		caseInsensitive: a.OS == "windows" || a.OS == "darwin",
	}
	if a.OS == "windows" {
		stat, err := f.Stat()
		if err != nil {
			return err
		}
		z, err := zip.NewReader(f, stat.Size())
		if err != nil {
			return err
		}
		if len(z.File) > maxEntries {
			return errors.New("archive has too many entries")
		}
		for _, entry := range z.File {
			if entry.UncompressedSize64 > uint64(maxMemberBytes) {
				return fmt.Errorf("member exceeds payload limit: %s", entry.Name)
			}
			r, err := entry.Open()
			if err != nil {
				return err
			}
			err = members.add(entry.Name, entry.Mode(), int64(entry.UncompressedSize64), r)
			closeErr := r.Close()
			if err != nil {
				return err
			}
			if closeErr != nil {
				return closeErr
			}
		}
	} else {
		gz, err := gzip.NewReader(contextReader{ctx, f})
		if err != nil {
			return err
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			mode := h.FileInfo().Mode()
			if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
				return fmt.Errorf("links and special archive members are forbidden: %s", h.Name)
			}
			// Extraction follows Typeflag, so mode bits must not make a file look
			// like a directory and skip payload inspection.
			if mode.IsDir() != (h.Typeflag == tar.TypeDir) {
				return fmt.Errorf("tar mode bits disagree with entry type: %s", h.Name)
			}
			if err := members.add(h.Name, mode, h.Size, tr); err != nil {
				return err
			}
		}
		// Read through the gzip trailer so truncated/corrupt checksums fail too.
		if _, err := io.Copy(io.Discard, contextReader{ctx, gz}); err != nil {
			return err
		}
	}
	return checkMembers(ctx, a, members.files)
}

func checkMembers(ctx context.Context, a Artifact, files map[string]member) error {
	key := func(s string) string {
		if a.OS == "windows" {
			return strings.ToLower(s)
		}
		return s
	}
	required := append([]string{a.Executable}, a.RuntimeFiles...)
	required = append(required, a.RequiredFiles...)
	for _, name := range required {
		m, ok := files[key(name)]
		if !ok || !m.mode.IsRegular() || len(m.data) == 0 {
			return fmt.Errorf("required regular nonempty member missing: %s", name)
		}
		// Extraction preserves Unix modes, so an unreadable member stays unusable.
		if a.OS != "windows" && m.mode.Perm()&0o400 == 0 {
			return fmt.Errorf("%s lacks Unix owner read permission", name)
		}
	}
	declared := map[string]bool{key(a.Executable): true}
	for _, name := range a.RuntimeFiles {
		declared[key(name)] = true
	}
	native := map[string]nativeInfo{}
	for name, m := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if m.mode.IsDir() {
			continue
		}
		if !declared[name] && !nativeMagic(m.data) {
			continue
		}
		info, err := inspectNative(m.data)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if !declared[name] {
			return fmt.Errorf("undeclared native companion: %s", name)
		}
		if info.os != a.OS || info.arch != a.Arch {
			return fmt.Errorf("%s: target %s/%s does not match %s/%s", name, info.os, info.arch, a.OS, a.Arch)
		}
		if name == key(a.Executable) && !info.executable {
			return fmt.Errorf("%s is not an executable image", name)
		}
		if name != key(a.Executable) && !info.executable && !info.library {
			return fmt.Errorf("%s is not a native executable or library", name)
		}
		if a.OS != "windows" && (name == key(a.Executable) || info.executable) && m.mode.Perm()&0o100 == 0 {
			return fmt.Errorf("%s lacks Unix owner execute permission", name)
		}
		if a.Kind == "static" && (info.os != "linux" || info.interp != "" || len(info.imports) > 0) {
			return errors.New("static executable has an interpreter or dynamic imports")
		}
		native[name] = info
	}
	system := map[string]bool{}
	for _, lib := range a.SystemLibraries {
		system[key(lib)] = true
	}
	runtimeMembers := map[string]bool{}
	for _, runtime := range a.RuntimeFiles {
		runtimeMembers[key(runtime)] = true
	}
	var executableDirs []string
	for name, info := range native {
		if info.executable {
			executableDirs = append(executableDirs, path.Dir(name))
		}
	}
	slices.Sort(executableDirs)
	for name, info := range native {
		// A helper runs as its own process. A library can load into any
		// executable in the bundle; this does not trace which one loads it.
		dirs := executableDirs
		if info.executable {
			dirs = []string{path.Dir(name)}
		}
		for _, lib := range info.imports {
			if system[key(lib)] {
				continue
			}
			if a.Kind != "bundle" {
				return fmt.Errorf("%s: import %q is not a declared baseline system library", name, lib)
			}
			found := false
			for _, dir := range dirs {
				candidate := key(bundledImport(a.OS, name, lib, dir))
				if candidate == "" || !runtimeMembers[candidate] {
					continue
				}
				if provider, ok := native[candidate]; !ok || !provider.sharedLibrary {
					return fmt.Errorf("%s: import %q resolves to runtime member %s, which is not a shared-library image", name, lib, candidate)
				}
				found = true
				break
			}
			if !found {
				return fmt.Errorf("%s: unresolved import %q (declare an exact baseline import or runtime member)", name, lib)
			}
		}
	}
	return nil
}

func bundledImport(goos, loader, lib, executableDir string) string {
	// Resolve target-specific loader-relative spellings against the image that
	// imports them, and executable-relative spellings against executableDir.
	// Bare Windows DLL names resolve in the application directory.
	// On Darwin and Linux, @rpath and bare names remain direct archive-root
	// member checks; this does not simulate loader search paths.
	if strings.HasPrefix(lib, "/") || strings.ContainsAny(lib, "\\:\x00") {
		return ""
	}
	base := "."
	loaderRelative := false
	switch {
	case goos == "darwin" && strings.HasPrefix(lib, "@rpath/"):
		lib = strings.TrimPrefix(lib, "@rpath/")
	case goos == "darwin" && strings.HasPrefix(lib, "@loader_path/"):
		base = path.Dir(loader)
		loaderRelative = true
		lib = strings.TrimPrefix(lib, "@loader_path/")
	case goos == "darwin" && strings.HasPrefix(lib, "@executable_path/"):
		base = executableDir
		loaderRelative = true
		lib = strings.TrimPrefix(lib, "@executable_path/")
	case goos == "linux" && strings.HasPrefix(lib, "$ORIGIN/"):
		base = path.Dir(loader)
		loaderRelative = true
		lib = strings.TrimPrefix(lib, "$ORIGIN/")
	case goos == "linux" && strings.HasPrefix(lib, "${ORIGIN}/"):
		base = path.Dir(loader)
		loaderRelative = true
		lib = strings.TrimPrefix(lib, "${ORIGIN}/")
	case goos == "windows":
		base = executableDir
	}
	if lib == "" || strings.HasPrefix(lib, "/") || !loaderRelative && !memberPath(lib) {
		return ""
	}
	candidate := path.Join(base, lib)
	if !memberPath(candidate) {
		return ""
	}
	return candidate
}
