package gitcmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"go.kenn.io/kit/internal/contextio"
)

type safeDirectoryScope struct {
	paths       []string
	fingerprint [32]byte
	values      []string
	valid       bool
	native      bool
}

type safeDirectoryCache struct {
	mu      sync.Mutex
	current *safeDirectorySnapshot
}

// Admission bounds unfinished trust-cache filesystem checks to one per process.
var safeDirectoryFilesystem = &safeDirectoryFilesystemAdmission{}

type safeDirectoryFilesystemAdmission struct {
	mu         sync.Mutex
	done       chan struct{}
	ownerEnded <-chan struct{}
}

func evaluateFilesystem(ctx context.Context, evaluate func(context.Context)) bool {
	ctx, cancel := context.WithTimeout(ctx, safeDirectoryProbeTimeout)
	defer cancel()
	admission := safeDirectoryFilesystem
	for ctx.Err() == nil {
		admission.mu.Lock()
		if ctx.Err() != nil {
			admission.mu.Unlock()
			break
		}
		pending, ended := admission.done, admission.ownerEnded
		if pending == nil {
			done := make(chan struct{})
			admission.done, admission.ownerEnded = done, ctx.Done()
			admission.mu.Unlock()
			go func() {
				evaluate(ctx)
				admission.mu.Lock()
				admission.done, admission.ownerEnded = nil, nil
				close(done)
				admission.mu.Unlock()
			}()
			select {
			case <-done:
				return ctx.Err() == nil
			case <-ctx.Done():
				return false
			}
		}
		admission.mu.Unlock()
		select {
		case <-pending:
			continue
		case <-ended:
			select {
			case <-pending:
				continue
			default:
				return false
			}
		case <-ctx.Done():
			return false
		}
	}
	return false
}

type safeDirectorySnapshot struct {
	identity   [32]byte
	executable os.FileInfo
	scopes     map[string]safeDirectoryScope
}

func safeDirectoryScopes(env []string) []string {
	if gitEnvBool(env, "GIT_CONFIG_NOSYSTEM") {
		return []string{"global"}
	}
	return []string{"system", "global"}
}

func configPathsReusable(env []string) bool {
	if !gitEnvBool(env, "GIT_CONFIG_NOSYSTEM") {
		system, _ := envValue(env, "GIT_CONFIG_SYSTEM")
		if strings.ContainsAny(system, "\r\n") {
			return false
		}
	}
	global, override := envValue(env, "GIT_CONFIG_GLOBAL")
	paths := []string{global}
	if !override {
		home, _ := envValue(env, "HOME")
		xdg, _ := envValue(env, "XDG_CONFIG_HOME")
		paths = []string{home, xdg}
	}
	for _, path := range paths {
		if strings.ContainsAny(path, "\r\n") {
			return false
		}
		if slices.Contains(strings.FieldsFunc(path, func(r rune) bool { return r == '/' || r == '\\' && os.PathSeparator == '\\' }), "..") {
			return false
		}
	}
	return true
}

func windowsHomeAvailable(ctx context.Context, env []string) (available, applicable bool) {
	_, homeSet := envValue(env, "HOME")
	_, globalSet := envValue(env, "GIT_CONFIG_GLOBAL")
	drive, driveSet := envValue(env, "HOMEDRIVE")
	path, pathSet := envValue(env, "HOMEPATH")
	if runtime.GOOS != "windows" || homeSet || globalSet || !driveSet || !pathSet {
		return false, false
	}
	if ctx.Err() != nil {
		return false, true
	}
	info, err := os.Stat(drive + path)
	if err != nil {
		return false, true
	}
	return info.IsDir(), true
}

// read reuses include-free scopes while their root bytes stay unchanged; includes always run Git.
func (c *safeDirectoryCache) read(ctx context.Context, env []string, dir string) []string {
	if c == nil {
		return readSafeDirectories(ctx, env, dir)
	}
	fresh := func() []string {
		return readSafeDirectories(ctx, env, dir)
	}
	if !configPathsReusable(env) {
		return fresh()
	}
	c.mu.Lock()
	previous := c.current
	c.mu.Unlock()
	var path string
	var info os.FileInfo
	var err error
	var homeAvailable, homeApplicable bool
	var sameFile bool
	if !evaluateFilesystem(ctx, func(checkCtx context.Context) {
		if checkCtx.Err() != nil {
			return
		}
		path = gitCommand(checkCtx, true).Path
		if checkCtx.Err() != nil {
			return
		}
		info, err = os.Stat(path)
		if checkCtx.Err() != nil || err != nil {
			return
		}
		homeAvailable, homeApplicable = windowsHomeAvailable(checkCtx, env)
		if checkCtx.Err() == nil && previous != nil {
			sameFile = os.SameFile(info, previous.executable)
		}
	}) || err != nil || info == nil {
		return fresh()
	}
	homeUnchanged := func() bool {
		if !homeApplicable {
			return true
		}
		var available bool
		return evaluateFilesystem(ctx, func(checkCtx context.Context) { available, _ = windowsHomeAvailable(checkCtx, env) }) && available == homeAvailable
	}
	identity := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%s\x00%t", path, info.Size(), info.ModTime().UnixNano(), strings.Join(env, "\x00"), homeAvailable)))
	if previous == nil {
		values := readSafeDirectories(ctx, env, dir)
		if ctx.Err() != nil {
			return nil
		}
		if !homeUnchanged() {
			return fresh()
		}
		c.mu.Lock()
		if c.current == nil && ctx.Err() == nil {
			c.current = &safeDirectorySnapshot{identity: identity, executable: info}
		}
		c.mu.Unlock()
		return values
	}
	if identity != previous.identity || !sameFile {
		return fresh()
	}
	next := &safeDirectorySnapshot{identity: identity, executable: info, scopes: make(map[string]safeDirectoryScope)}
	if previous.scopes != nil {
		maps.Copy(next.scopes, previous.scopes)
	} else {
		for _, scope := range safeDirectoryScopes(env) {
			out, err := safeDirectoryOutput(ctx, env, dir, "var", "GIT_CONFIG_"+strings.ToUpper(scope))
			if err != nil {
				return fresh()
			}
			paths := strings.TrimSuffix(string(out), "\n")
			next.scopes[scope] = safeDirectoryScope{paths: strings.FieldsFunc(strings.TrimRight(paths, "\r\n"), func(r rune) bool { return r == '\n' || r == '\r' })}
			if scope == "global" && homeApplicable {
				home, _ := envValue(env, "USERPROFILE")
				if homeAvailable {
					drive, _ := envValue(env, "HOMEDRIVE")
					path, _ := envValue(env, "HOMEPATH")
					home = drive + path
				}
				expected := filepath.Join(home, ".gitconfig")
				discovered := next.scopes[scope].paths
				if !filepath.IsAbs(expected) || len(discovered) != 2 || filepath.ToSlash(filepath.Clean(discovered[1])) != filepath.ToSlash(filepath.Clean(expected)) {
					return fresh()
				}
			}
		}
	}
	var values []string
	for _, scope := range safeDirectoryScopes(env) {
		s := next.scopes[scope]
		var cached safeDirectoryScope
		var cacheErr error
		if s.native {
			cached = safeDirectoryScope{paths: s.paths, native: true}
		} else if !evaluateFilesystem(ctx, func(checkCtx context.Context) {
			cached, cacheErr = readSafeDirectorySnapshot(checkCtx, env, dir, scope, s)
		}) {
			return fresh()
		}
		entries := cached.values
		if cacheErr != nil || cached.native {
			if cacheErr != nil {
				cached = safeDirectoryScope{paths: s.paths}
			}
			var probeErr error
			var includes bool
			entries, includes, probeErr = readSafeDirectoryScope(ctx, env, dir, scope)
			if probeErr != nil && !IsExitCode(probeErr, 1) {
				cached = safeDirectoryScope{paths: s.paths, native: true}
				next.scopes[scope] = cached
				continue
			}
			cached = safeDirectoryScope{paths: s.paths, native: includes}
		}
		next.scopes[scope] = cached
		values = append(values, entries...)
	}
	if ctx.Err() != nil {
		return nil
	}
	if !homeUnchanged() {
		return fresh()
	}
	c.mu.Lock()
	if c.current == previous && ctx.Err() == nil {
		c.current = next
	}
	c.mu.Unlock()
	return values
}

const safeDirectoryKeys = `^(safe\.directory|include\.path|includeif\..*\.path)$`

func readSafeDirectoryScope(ctx context.Context, env []string, dir, scope string) ([]string, bool, error) {
	out, err := safeDirectoryOutput(ctx, env, dir, "config", "--"+scope, "--includes", "-z", "--get-regexp", safeDirectoryKeys)
	entries, includes := decodeSafeDirectoryOutput(out)
	return entries, includes, err
}

func decodeSafeDirectoryOutput(out []byte) ([]string, bool) {
	var entries []string
	includes := false
	for entry := range strings.SplitSeq(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		key, value, _ := strings.Cut(entry, "\n")
		if key == "" {
			continue
		}
		if key == "safe.directory" {
			entries = append(entries, value)
		} else {
			includes = true
		}
	}
	return entries, includes
}

func safeDirectoryOutput(ctx context.Context, env []string, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, safeDirectoryProbeTimeout)
	defer cancel()
	cmd := gitCommand(ctx, true, args...)
	cmd.Env, cmd.Dir = env, dir
	out, err := cmd.Output()
	if err != nil {
		return out, &GitError{Err: err}
	}
	return out, nil
}

func readSafeDirectorySnapshot(ctx context.Context, env []string, dir, scope string, previous safeDirectoryScope) (safeDirectoryScope, error) {
	next := safeDirectoryScope{paths: previous.paths}
	expected := 1
	if _, override := envValue(env, "GIT_CONFIG_GLOBAL"); scope == "global" && !override {
		expected = 2
	}
	if len(previous.paths) != expected {
		return next, errors.ErrUnsupported
	}
	var file *os.File
	var path string
	for _, candidate := range slices.Backward(previous.paths) {
		if err := ctx.Err(); err != nil {
			return next, err
		}
		path = candidate
		if !filepath.IsAbs(path) {
			return next, errors.ErrUnsupported
		}
		info, err := os.Stat(path)
		if ctx.Err() != nil {
			return next, ctx.Err()
		}
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return next, err
		}
		if !info.Mode().IsRegular() {
			return next, fmt.Errorf("%s: not a regular file", path)
		}
		file, err = os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return next, err
		}
		break
	}
	if file == nil {
		next.valid = ctx.Err() == nil
		return next, ctx.Err()
	}
	defer file.Close()
	hash := sha256.New()
	size, err := io.Copy(hash, &contextio.Reader{Context: ctx, Reader: file})
	if ctx.Err() != nil {
		return next, ctx.Err()
	}
	if err != nil {
		return next, err
	}
	fingerprint := fingerprintSafeDirectoryBytes(path, size, hash.Sum(nil))
	if previous.valid && previous.fingerprint == fingerprint {
		return previous, nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return next, err
	}
	if ctx.Err() != nil {
		return next, ctx.Err()
	}
	parseCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := gitCommand(parseCtx, true, "config", "--no-includes", "--file", "-", "-z", "--get-regexp", safeDirectoryKeys)
	cmd.Env, cmd.Dir = env, dir
	var output bytes.Buffer
	cmd.Stdout = &output
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return next, err
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		return next, err
	}
	hash.Reset()
	size, copyErr := io.Copy(stdin, io.TeeReader(&contextio.Reader{Context: ctx, Reader: file}, hash))
	copyErr = errors.Join(copyErr, file.Close(), stdin.Close())
	if copyErr != nil {
		cancel()
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return next, ctx.Err()
	}
	if copyErr != nil {
		return next, copyErr
	}
	if waitErr != nil && !IsExitCode(&GitError{Err: waitErr}, 1) {
		return next, waitErr
	}
	next.fingerprint = fingerprintSafeDirectoryBytes(path, size, hash.Sum(nil))
	next.values, next.native = decodeSafeDirectoryOutput(output.Bytes())
	next.valid = true
	return next, nil
}

func fingerprintSafeDirectoryBytes(path string, size int64, digest []byte) [32]byte {
	return sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%x", path, size, digest)))
}
