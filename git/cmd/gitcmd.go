// Package gitcmd runs git subprocesses with defensive defaults.
//
// The default Runner strips inherited GIT_* environment variables, disables
// interactive prompts, ignores global and system git config, and injects
// temporary config through GIT_CONFIG_* variables. This prevents child git
// commands from accidentally binding to a parent repository or writing into a
// developer's global config during automation and tests.
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
	// NullGlobalConfig points GIT_CONFIG_GLOBAL at os.DevNull when true.
	NullGlobalConfig bool
	// NoSystemConfig sets GIT_CONFIG_NOSYSTEM=1 when true.
	NoSystemConfig bool

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
// user-only helper file instead of being exposed in the git process environment.
func (r Runner) WithBasicAuth(username, password string) Runner {
	r.basicAuth = &basicAuth{username: username, password: password}
	return r
}

// Command constructs a git command in dir.
func (r Runner) Command(ctx context.Context, dir string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = r.commandEnv()
	return cmd
}

// Output runs git and returns stdout.
func (r Runner) Output(ctx context.Context, dir string, args ...string) ([]byte, error) {
	stdout, _, err := r.Run(ctx, dir, nil, args...)
	return stdout, err
}

// Run runs git and returns stdout, stderr, and a *GitError on failure.
func (r Runner) Run(ctx context.Context, dir string, stdin io.Reader, args ...string) ([]byte, []byte, error) {
	cmd := r.Command(ctx, dir, args...)
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

func (a basicAuth) credentialHelper() (string, error) {
	file, err := os.CreateTemp("", "gitcmd-credential-helper-*")
	if err != nil {
		return "", err
	}
	path := file.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(path)
		}
	}()

	_, writeErr := file.WriteString("#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"get)\n" +
		"\tprintf '%s\\n' " + shellSingleQuote("username="+a.username) + " " + shellSingleQuote("password="+a.password) + "\n" +
		"\t;;\n" +
		"esac\n")
	closeErr := file.Close()
	if writeErr != nil {
		return "", writeErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return "", err
	}
	cleanup = false
	return path, nil
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func (r Runner) commandEnv() []string {
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
		env = append(env, "GIT_CONFIG_GLOBAL="+os.DevNull)
	}
	config := append([]Config{{Key: "gc.auto", Value: "0"}, {Key: "maintenance.auto", Value: "false"}}, r.Config...)
	if r.basicAuth != nil {
		helper, err := r.basicAuth.credentialHelper()
		if err != nil {
			helper = `!f() { echo "gitcmd basic auth helper setup failed" >&2; exit 1; }; f`
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
	return env
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
