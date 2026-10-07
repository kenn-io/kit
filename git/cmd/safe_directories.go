package gitcmd

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type safeDirectoryScope struct {
	paths       []string
	fingerprint [32]byte
	values      []string
	valid       bool
}

type safeDirectoryCache struct {
	gate       chan struct{}
	identity   [32]byte
	executable os.FileInfo
	scopes     map[string]*safeDirectoryScope
}

func (c *safeDirectoryCache) read(ctx context.Context, env []string, dir string) []string {
	if c == nil {
		return readSafeDirectories(ctx, env, dir)
	}
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return nil
	}
	if ctx.Err() != nil {
		return nil
	}
	cmd := gitCommand(ctx, true)
	info, err := os.Stat(cmd.Path)
	if err != nil {
		return readSafeDirectories(ctx, env, dir)
	}
	identity := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%s", cmd.Path, info.Size(), info.ModTime().UnixNano(), strings.Join(env, "\x00"))))
	if identity != c.identity || c.executable == nil || !os.SameFile(info, c.executable) {
		c.identity, c.scopes = identity, nil
		c.executable = info
	}
	if c.scopes == nil {
		scopes := make(map[string]*safeDirectoryScope)
		for _, scope := range []string{"system", "global"} {
			if scope == "system" && gitEnvBool(env, "GIT_CONFIG_NOSYSTEM") {
				continue
			}
			out, err := safeDirectoryOutput(ctx, env, dir, "var", "GIT_CONFIG_"+strings.ToUpper(scope))
			if err != nil || ctx.Err() != nil {
				return readSafeDirectories(ctx, env, dir)
			}
			scopes[scope] = &safeDirectoryScope{paths: strings.FieldsFunc(strings.TrimRight(string(out), "\r\n"), func(r rune) bool { return r == '\n' || r == '\r' })}
		}
		c.scopes = scopes
	}
	var values []string
	for _, scope := range []string{"system", "global"} {
		s := c.scopes[scope]
		if s == nil || ctx.Err() != nil {
			continue
		}
		before, err := safeDirectoryFingerprint(s.paths, dir)
		if ctx.Err() != nil {
			return nil
		}
		if err == nil && s.valid && before == s.fingerprint {
			values = append(values, s.values...)
			continue
		}
		s.valid = false
		out, probeErr := safeDirectoryOutput(ctx, env, dir, "config", "--"+scope, "--includes", "-z", "--get-regexp", `^(safe\.directory|include\.path|includeif\..*\.path)$`)
		if probeErr != nil && !(IsExitCode(probeErr, 1) && len(out) == 0) {
			continue
		}
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
		after, afterErr := safeDirectoryFingerprint(s.paths, dir)
		if ctx.Err() != nil {
			return nil
		}
		if !includes && err == nil && afterErr == nil && before == after {
			s.fingerprint, s.values, s.valid = after, entries, true
		}
		values = append(values, entries...)
	}
	return values
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

func safeDirectoryFingerprint(paths []string, dir string) ([32]byte, error) {
	h := sha256.New()
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		path, err := filepath.Abs(path)
		if err != nil {
			return [32]byte{}, err
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
