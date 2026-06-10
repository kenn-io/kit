// Package gitcmd runs git subprocesses with defensive defaults.
//
// The default Runner strips inherited GIT_* environment variables, disables
// interactive prompts, ignores global and system git config, and injects
// temporary config through GIT_CONFIG_* variables. This prevents child git
// commands from accidentally binding to a parent repository or writing into a
// developer's global config during automation and tests. The user's
// safe.directory entries are forwarded into the sanitized environment so
// config isolation does not make git reject repositories the user already
// trusts (for example root-squashed or container-mounted checkouts owned by
// another user).
package gitcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	gitenv "go.kenn.io/kit/git/env"
)

// Config is one temporary git config entry injected through GIT_CONFIG_* env.
type Config struct {
	// Key is the git config key, for example "gc.auto".
	Key string
	// Value is the git config value.
	Value string
}

// Runner builds git commands with a sanitized environment.
type Runner struct {
	// Env is the base process environment. When nil, os.Environ is used.
	Env []string
	// Config is appended to the default temporary git config entries.
	Config []Config
	// StripEnv removes inherited GIT_* variables before running git.
	StripEnv bool
	// TerminalPrompt allows interactive git prompts when true.
	TerminalPrompt bool
	// NullGlobalConfig makes git read an empty global config when true, by
	// pointing GIT_CONFIG_GLOBAL at an empty file (not os.DevNull, which is the
	// unreadable "NUL" device on some Windows builds).
	NullGlobalConfig bool
	// NoSystemConfig sets GIT_CONFIG_NOSYSTEM=1 when true.
	NoSystemConfig bool
	// ForwardSafeDirectory re-injects the user's safe.directory entries from
	// system and global git config as command-scope config. Without this,
	// NullGlobalConfig and NoSystemConfig hide those entries and git refuses
	// to operate on repositories owned by another user ("detected dubious
	// ownership"), even though plain git works for the same user.
	ForwardSafeDirectory bool

	basicAuth *basicAuth
}

// New returns a Runner with safe automation defaults.
func New() Runner {
	return Runner{
		Env:                  os.Environ(),
		StripEnv:             true,
		NullGlobalConfig:     true,
		NoSystemConfig:       true,
		ForwardSafeDirectory: true,
	}
}

// WithConfig returns a copy of r with an additional temporary git config value.
func (r Runner) WithConfig(key, value string) Runner {
	r.Config = append(append([]Config(nil), r.Config...), Config{Key: key, Value: value})
	return r
}

// WithBasicAuth returns a copy of r with credentials supplied through a
// short-lived git credential helper. The reusable secret is written to a
// user-only helper file instead of being exposed in the git process environment.
func (r Runner) WithBasicAuth(username, password string) Runner {
	r.basicAuth = &basicAuth{username: username, password: password}
	return r
}

// Command constructs a git command in dir.
//
// Command cannot be used with WithBasicAuth because callers would not have a
// way to clean up the temporary credential helper. Use Run or Output instead.
func (r Runner) Command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	if r.basicAuth != nil {
		panic("gitcmd: Command cannot be used with WithBasicAuth; use Run or Output so credentials can be cleaned up")
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env, _ = r.commandEnv()
	return cmd
}

// Output runs git and returns stdout.
func (r Runner) Output(ctx context.Context, dir string, args ...string) ([]byte, error) {
	stdout, _, err := r.Run(ctx, dir, nil, args...)
	return stdout, err
}

// Run runs git and returns stdout, stderr, and a *GitError on failure.
func (r Runner) Run(ctx context.Context, dir string, stdin io.Reader, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var cleanup func()
	cmd.Env, cleanup = r.commandEnv()
	defer cleanup()
	cmd.Stdin = stdin

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		return stdout.Bytes(), stderr.Bytes(), &GitError{
			Dir:    dir,
			Args:   append([]string(nil), args...),
			Stderr: strings.TrimSpace(stderr.String()),
			Err:    err,
		}
	}
	return stdout.Bytes(), stderr.Bytes(), nil
}

type basicAuth struct {
	username string
	password string
}

func (a basicAuth) credentialHelper() (string, func(), error) {
	file, err := os.CreateTemp("", "gitcmd-credential-helper-*")
	if err != nil {
		return "", nil, err
	}
	path := file.Name()
	cleanup := func() {
		_ = os.Remove(path)
	}

	_, writeErr := file.WriteString("#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"get)\n" +
		"\tprintf '%s\\n' " + shellSingleQuote("username="+a.username) + " " + shellSingleQuote("password="+a.password) + "\n" +
		"\t;;\n" +
		"esac\n")
	closeErr := file.Close()
	if writeErr != nil {
		cleanup()
		return "", nil, writeErr
	}
	if closeErr != nil {
		cleanup()
		return "", nil, closeErr
	}
	if err := os.Chmod(path, 0o700); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

var (
	emptyGlobalConfigOnce sync.Once
	emptyGlobalConfigPath string
)

// nullGlobalConfigPath returns a path suitable for GIT_CONFIG_GLOBAL that makes
// git read an empty (no-op) global config.
//
// It must not be os.DevNull. On Windows os.DevNull is "NUL", the null device,
// and some Git for Windows builds — notably on ARM64 — fail with
// "fatal: unable to access 'NUL': Invalid argument" when told to read their
// global config from that device, which breaks every git invocation. An empty
// regular file is an equivalent no-op global config on every platform. The file
// is created once per process and reused; it is intentionally not removed so the
// path stays valid for the process lifetime (the OS reclaims the temp dir). If
// the file cannot be created, fall back to os.DevNull, which still works on
// platforms where the device is readable.
func nullGlobalConfigPath() string {
	emptyGlobalConfigOnce.Do(func() {
		f, err := os.CreateTemp("", "gitcmd-empty-global-*.gitconfig")
		if err != nil {
			emptyGlobalConfigPath = os.DevNull
			return
		}
		emptyGlobalConfigPath = f.Name()
		_ = f.Close()
	})
	return emptyGlobalConfigPath
}

var (
	safeDirectoriesOnce sync.Once
	safeDirectoriesList []string
)

// safeDirectories returns the safe.directory entries from the user's protected
// git config, cached for the process lifetime. git only honors safe.directory
// from protected configuration (system, global, and command scope), so these
// are the entries the sanitized environment would otherwise hide.
func safeDirectories() []string {
	safeDirectoriesOnce.Do(func() {
		safeDirectoriesList = readSafeDirectories(os.Environ())
	})
	return safeDirectoriesList
}

// readSafeDirectories reads safe.directory entries from system and global git
// config using env, in git's evaluation order. Best effort: scopes that are
// unset or unreadable contribute nothing. Empty values are kept because an
// empty safe.directory resets the list, and replaying entries in order
// preserves that semantic at command scope.
func readSafeDirectories(env []string) []string {
	var dirs []string
	for _, scope := range []string{"--system", "--global"} {
		cmd := exec.Command("git", "config", scope, "-z", "--get-all", "safe.directory")
		cmd.Env = env
		out, err := cmd.Output()
		if err != nil || len(out) == 0 {
			continue
		}
		dirs = append(dirs, strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")...)
	}
	return dirs
}

func (r Runner) commandEnv() ([]string, func()) {
	cleanup := func() {}
	env := r.Env
	if env == nil {
		env = os.Environ()
	}
	if r.StripEnv {
		env = gitenv.StripAll(env)
	} else {
		env = append([]string(nil), env...)
	}
	if !r.TerminalPrompt {
		env = append(env, "GIT_TERMINAL_PROMPT=0")
	}
	if r.NoSystemConfig {
		env = append(env, "GIT_CONFIG_NOSYSTEM=1")
	}
	if r.NullGlobalConfig {
		env = append(env, "GIT_CONFIG_GLOBAL="+nullGlobalConfigPath())
	}
	config := []Config{{Key: "gc.auto", Value: "0"}, {Key: "maintenance.auto", Value: "false"}}
	if r.ForwardSafeDirectory {
		for _, dir := range safeDirectories() {
			config = append(config, Config{Key: "safe.directory", Value: dir})
		}
	}
	config = append(config, r.Config...)
	if r.basicAuth != nil {
		helper, cleanupHelper, err := r.basicAuth.credentialHelper()
		if err != nil {
			helper = `!f() { echo "gitcmd basic auth helper setup failed" >&2; exit 1; }; f`
		} else {
			cleanup = cleanupHelper
		}
		config = append(config, Config{Key: "credential.helper", Value: helper})
	}
	for i, c := range config {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, c.Key),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, c.Value),
		)
	}
	env = append(env, fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(config)))
	return env, cleanup
}

// GitError wraps a failed git command with stderr.
type GitError struct {
	// Dir is the working directory used for the git command.
	Dir string
	// Args are the git arguments without the leading "git" executable.
	Args []string
	// Stderr is the trimmed stderr captured from git.
	Stderr string
	// Err is the underlying process error.
	Err error
}

func (e *GitError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("git %s: %v", strings.Join(e.Args, " "), e.Err)
	}
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, e.Stderr)
}

func (e *GitError) Unwrap() error {
	return e.Err
}

// ExitCode returns git's process exit code when available.
func (e *GitError) ExitCode() (int, bool) {
	var exitErr *exec.ExitError
	if errors.As(e.Err, &exitErr) {
		return exitErr.ExitCode(), true
	}
	return 0, false
}

// IsExitCode reports whether err is a GitError with code.
func IsExitCode(err error, code int) bool {
	var gitErr *GitError
	return errors.As(err, &gitErr) && func() bool {
		got, ok := gitErr.ExitCode()
		return ok && got == code
	}()
}
