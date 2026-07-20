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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

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
	// DisableSafeDirectoryForward turns off re-injecting the user's
	// safe.directory entries from system and global git config as
	// command-scope config. Forwarding is the default because NullGlobalConfig
	// and NoSystemConfig hide those entries and git then refuses to operate on
	// repositories owned by another user ("detected dubious ownership"), even
	// though plain git works for the same user.
	DisableSafeDirectoryForward bool

	basicAuth *basicAuth
}

// New returns a Runner with safe automation defaults.
func New() Runner {
	return Runner{
		Env:              os.Environ(),
		StripEnv:         true,
		NullGlobalConfig: true,
		NoSystemConfig:   true,
	}
}

// WithConfig returns a copy of r with an additional temporary git config value.
func (r Runner) WithConfig(key, value string) Runner {
	r.Config = append(append([]Config(nil), r.Config...), Config{Key: key, Value: value})
	return r
}

// WithBasicAuth returns a copy of r with credentials supplied through a
// short-lived git credential helper. The reusable secret is written to a
// user-only, non-executable response file instead of being exposed in the git
// process environment.
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
	cmd := gitCommand(ctx, !r.TerminalPrompt, args...)
	cmd.Dir = dir
	cmd.Env, _ = r.commandEnv(ctx, dir)
	return cmd
}

// Output runs git and returns stdout.
func (r Runner) Output(ctx context.Context, dir string, args ...string) ([]byte, error) {
	stdout, _, err := r.Run(ctx, dir, nil, args...)
	return stdout, err
}

// Run runs git and returns stdout, stderr, and a *GitError on failure.
func (r Runner) Run(ctx context.Context, dir string, stdin io.Reader, args ...string) ([]byte, []byte, error) {
	cmd := gitCommand(ctx, !r.TerminalPrompt, args...)
	cmd.Dir = dir
	var cleanup func()
	cmd.Env, cleanup = r.commandEnv(ctx, dir)
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

func gitCommand(ctx context.Context, hideConsoleWindow bool, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	prepareGitCommand(cmd, hideConsoleWindow)
	return cmd
}

type basicAuth struct {
	username string
	password string
}

func validateCredentialValue(field, value string) error {
	if strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s contains a character unsupported by the git credential protocol", field)
	}
	return nil
}

func (a basicAuth) credentialResponse() (string, func(), error) {
	noCleanup := func() {}
	if err := validateCredentialValue("username", a.username); err != nil {
		return "", noCleanup, err
	}
	if err := validateCredentialValue("password", a.password); err != nil {
		return "", noCleanup, err
	}

	file, err := os.CreateTemp("", "gitcmd-credential-response-*")
	if err != nil {
		return "", noCleanup, err
	}
	path := file.Name()
	cleanup := func() {
		_ = os.Remove(path)
	}

	_, writeErr := fmt.Fprintf(file, "username=%s\npassword=%s\n", a.username, a.password)
	closeErr := file.Close()
	if writeErr != nil {
		cleanup()
		return "", noCleanup, writeErr
	}
	if closeErr != nil {
		cleanup()
		return "", noCleanup, closeErr
	}
	return path, cleanup, nil
}

func credentialHelper(path string) string {
	return `!f() { ` +
		`while IFS= read -r line && [ -n "$line" ]; do :; done; ` +
		`if [ "$1" = get ]; then ` +
		`while IFS= read -r line; do printf '%s\n' "$line"; done < ` + shellSingleQuote(path) + `; ` +
		`fi; ` +
		`}; f`
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

var (
	emptyGlobalConfigOnce sync.Once
	emptyGlobalConfigPath string
)

var safeDirectoryProbeTimeout = 2 * time.Second

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

// readSafeDirectories reads safe.directory entries from system and global git
// config using env, in git's evaluation order. git only honors safe.directory
// from protected configuration (system, global, and command scope), so these
// are the entries the sanitized environment would otherwise hide. Entries are
// read fresh on every call, like git itself reads config on every invocation:
// no cache means no stale trust entries in long-lived processes and no
// retained copies of caller environments. Best effort: scopes that are unset
// or unreadable contribute nothing. Empty values are kept because an empty
// safe.directory resets the list, and replaying entries in order preserves
// that semantic at command scope.
//
// The probes run in dir, the same directory as the git command being built,
// so conditional includes (includeIf "gitdir:...") resolve exactly as they
// would for that command rather than against the calling process's working
// directory.
//
// "git config --system" reads the system file even when GIT_CONFIG_NOSYSTEM
// is set (the variable only affects git's default config sequence), so the
// system scope is skipped here explicitly: git running with this env would
// not honor those entries, and they must not be forwarded on its behalf.
func readSafeDirectories(ctx context.Context, env []string, dir string) []string {
	scopes := []string{"--system", "--global"}
	if gitEnvBool(env, "GIT_CONFIG_NOSYSTEM") {
		scopes = scopes[1:]
	}
	var dirs []string
	for _, scope := range scopes {
		// --includes is required for explicit-scope reads to honor include.path
		// and includeIf directives the way git's default config sequence does.
		probeCtx, cancel := context.WithTimeout(ctx, safeDirectoryProbeTimeout)
		cmd := gitCommand(probeCtx, true, "config", scope, "--includes", "-z", "--get-all", "safe.directory")
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.Output()
		cancel()
		if err != nil || len(out) == 0 {
			continue
		}
		dirs = append(dirs, strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")...)
	}
	return dirs
}

// gitEnvBool reports whether env sets key to a value git's boolean parsing
// treats as true. An empty value is false, matching git's handling of boolean
// environment variables (unlike valueless config keys, which are true).
func gitEnvBool(env []string, key string) bool {
	value, ok := envValue(env, key)
	if !ok {
		return false
	}
	switch strings.ToLower(value) {
	case "true", "yes", "on":
		return true
	case "", "false", "no", "off":
		return false
	}
	n, err := strconv.Atoi(value)
	return err == nil && n != 0
}

// envValue returns the value of key in env, honoring exec.Cmd semantics where
// the last duplicate entry wins.
func envValue(env []string, key string) (string, bool) {
	for _, entry := range slices.Backward(env) {
		k, v, ok := strings.Cut(entry, "=")
		if ok && k == key {
			return v, true
		}
	}
	return "", false
}

func (r Runner) commandEnv(ctx context.Context, dir string) ([]string, func()) {
	cleanup := func() {}
	base := r.Env
	if base == nil {
		base = os.Environ()
	}
	env := base
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
	if !r.DisableSafeDirectoryForward {
		// Read from the runner's base env before stripping, so the entries come
		// from the configuration this runner's environment would see, not from
		// the process environment.
		for _, trusted := range readSafeDirectories(ctx, base, dir) {
			config = append(config, Config{Key: "safe.directory", Value: trusted})
		}
	}
	config = append(config, r.Config...)
	if r.basicAuth != nil {
		responsePath, cleanupResponse, err := r.basicAuth.credentialResponse()
		var helper string
		if err != nil {
			helper = `!f() { echo "gitcmd basic auth helper setup failed" >&2; exit 1; }; f`
		} else {
			helper = credentialHelper(responsePath)
			cleanup = cleanupResponse
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
