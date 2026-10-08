package gitcmd

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
)

type safeDirectoryScope struct {
	paths       []string
	fingerprint [32]byte
	values      []string
	valid       bool
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

func evaluateFilesystem(ctx context.Context, evaluate func()) bool {
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
				evaluate()
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
	identity        [32]byte
	executable      os.FileInfo
	scopes          map[string]safeDirectoryScope
	discoveryFailed bool
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

func windowsHomeAvailable(env []string) bool {
	_, homeSet := envValue(env, "HOME")
	_, globalSet := envValue(env, "GIT_CONFIG_GLOBAL")
	drive, driveSet := envValue(env, "HOMEDRIVE")
	path, pathSet := envValue(env, "HOMEPATH")
	if runtime.GOOS != "windows" || homeSet || globalSet || !driveSet || !pathSet {
		return false
	}
	info, err := os.Stat(drive + path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// read reuses include-free scopes while their root bytes stay unchanged; includes always run Git.
func (c *safeDirectoryCache) read(ctx context.Context, env []string, dir string) []string {
	if c == nil {
		return readSafeDirectories(ctx, env, dir)
	}
	fresh := func() []string {
		if ctx.Err() != nil {
			return nil
		}
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
	var homeAvailable bool
	var sameFile bool
	if !evaluateFilesystem(ctx, func() {
		path = gitCommand(ctx, true).Path
		info, err = os.Stat(path)
		homeAvailable = windowsHomeAvailable(env)
		if err == nil && previous != nil {
			sameFile = os.SameFile(info, previous.executable)
		}
	}) || err != nil || info == nil {
		return fresh()
	}
	homeUnchanged := func() bool {
		var available bool
		return evaluateFilesystem(ctx, func() { available = windowsHomeAvailable(env) }) && available == homeAvailable
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
	if previous.discoveryFailed {
		return fresh()
	}
	next := &safeDirectorySnapshot{identity: identity, executable: info, scopes: make(map[string]safeDirectoryScope)}
	if previous.scopes != nil {
		maps.Copy(next.scopes, previous.scopes)
	} else {
		for _, scope := range safeDirectoryScopes(env) {
			out, err := safeDirectoryOutput(ctx, env, dir, "var", "GIT_CONFIG_"+strings.ToUpper(scope))
			if err != nil {
				next.discoveryFailed = true
				break
			}
			paths := strings.TrimSuffix(string(out), "\n")
			next.scopes[scope] = safeDirectoryScope{paths: strings.FieldsFunc(strings.TrimRight(paths, "\r\n"), func(r rune) bool { return r == '\n' || r == '\r' })}
		}
	}
	var values []string
	scopes := safeDirectoryScopes(env)
	if next.discoveryFailed {
		values = readSafeDirectories(ctx, env, dir)
		scopes = nil
	}
	for _, scope := range scopes {
		s := next.scopes[scope]
		var before [32]byte
		if !evaluateFilesystem(ctx, func() { before, err = safeDirectoryFingerprint(s.paths) }) {
			return fresh()
		}
		if err == nil && s.valid && before == s.fingerprint {
			values = append(values, s.values...)
			continue
		}
		s = safeDirectoryScope{paths: s.paths}
		entries, includes, probeErr := readSafeDirectoryScope(ctx, env, dir, scope)
		if probeErr != nil && !IsExitCode(probeErr, 1) {
			next.scopes[scope] = s
			continue
		}
		var after [32]byte
		var afterErr error
		if !evaluateFilesystem(ctx, func() { after, afterErr = safeDirectoryFingerprint(s.paths) }) {
			return fresh()
		}
		if !includes && err == nil && afterErr == nil && before == after {
			s.fingerprint, s.values, s.valid = after, entries, true
		}
		next.scopes[scope] = s
		values = append(values, entries...)
	}
	if next.discoveryFailed {
		next.scopes = nil
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

func readSafeDirectoryScope(ctx context.Context, env []string, dir, scope string) ([]string, bool, error) {
	out, err := safeDirectoryOutput(ctx, env, dir, "config", "--"+scope, "--includes", "-z", "--get-regexp", `^(safe\.directory|include\.path|includeif\..*\.path)$`)
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
	return entries, includes, err
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

func safeDirectoryFingerprint(paths []string) ([32]byte, error) {
	h := sha256.New()
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			return [32]byte{}, fmt.Errorf("%s: not an absolute path", path)
		}
		info, err := os.Stat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return [32]byte{}, err
		}
		if err == nil && !info.Mode().IsRegular() {
			return [32]byte{}, fmt.Errorf("%s: not a regular file", path)
		}
		contents, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return [32]byte{}, err
		}
		fmt.Fprintf(h, "%s\x00%t\x00%d\x00", path, err == nil, len(contents))
		h.Write(contents)
	}
	return [32]byte(h.Sum(nil)), nil
}
