package gitcmd

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
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

// read reuses include-free scopes while their root bytes stay unchanged; includes always run Git.
func (c *safeDirectoryCache) read(ctx context.Context, env []string, dir string) []string {
	if c == nil {
		return readSafeDirectories(ctx, env, dir)
	}
	if ctx.Err() != nil {
		return nil
	}
	if !configPathsReusable(env) {
		return readSafeDirectories(ctx, env, dir)
	}
	cmd := gitCommand(ctx, true)
	info, err := os.Stat(cmd.Path)
	if err != nil {
		return readSafeDirectories(ctx, env, dir)
	}
	identity := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%s", cmd.Path, info.Size(), info.ModTime().UnixNano(), strings.Join(env, "\x00"))))
	c.mu.Lock()
	previous := c.current
	c.mu.Unlock()
	if previous == nil {
		values := readSafeDirectories(ctx, env, dir)
		if ctx.Err() != nil {
			return nil
		}
		c.mu.Lock()
		if c.current == nil {
			c.current = &safeDirectorySnapshot{identity: identity, executable: info}
		}
		c.mu.Unlock()
		return values
	}
	if identity != previous.identity || !os.SameFile(info, previous.executable) {
		return readSafeDirectories(ctx, env, dir)
	}
	next := &safeDirectorySnapshot{identity: identity, executable: info, scopes: make(map[string]safeDirectoryScope)}
	if previous.scopes != nil {
		maps.Copy(next.scopes, previous.scopes)
	} else {
		for _, scope := range safeDirectoryScopes(env) {
			out, err := safeDirectoryOutput(ctx, env, dir, "var", "GIT_CONFIG_"+strings.ToUpper(scope))
			if err != nil {
				return readSafeDirectories(ctx, env, dir)
			}
			paths := strings.TrimSuffix(string(out), "\n")
			next.scopes[scope] = safeDirectoryScope{paths: strings.FieldsFunc(strings.TrimRight(paths, "\r\n"), func(r rune) bool { return r == '\n' || r == '\r' })}
		}
	}
	var values []string
	for _, scope := range safeDirectoryScopes(env) {
		s := next.scopes[scope]
		before, err := safeDirectoryFingerprint(s.paths)
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
		after, afterErr := safeDirectoryFingerprint(s.paths)
		if !includes && err == nil && afterErr == nil && before == after {
			s.fingerprint, s.values, s.valid = after, entries, true
		}
		next.scopes[scope] = s
		values = append(values, entries...)
	}
	if ctx.Err() != nil {
		return nil
	}
	c.mu.Lock()
	if c.current == previous {
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
