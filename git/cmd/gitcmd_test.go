package gitcmd

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRunnerCommandUsesDefensiveEnvironment(t *testing.T) {
	runner := New()
	runner.Env = []string{
		"PATH=/bin",
		"GIT_DIR=/parent/.git",
		"GIT_SSL_NO_VERIFY=1",
		"SSH_ASKPASS=/tmp/askpass",
	}

	cmd := runner.Command(context.Background(), "", "status")

	if slices.Contains(cmd.Env, "GIT_DIR=/parent/.git") {
		t.Fatalf("GIT_DIR should have been stripped: %#v", cmd.Env)
	}
	if slices.Contains(cmd.Env, "GIT_SSL_NO_VERIFY=1") {
		t.Fatalf("GIT_SSL_NO_VERIFY should have been stripped: %#v", cmd.Env)
	}
	if !slices.Contains(cmd.Env, "GIT_TERMINAL_PROMPT=0") {
		t.Fatalf("terminal prompts should be disabled: %#v", cmd.Env)
	}
	if !slices.Contains(cmd.Env, "GIT_CONFIG_GLOBAL="+nullGlobalConfigPath()) {
		t.Fatalf("global config should be nulled: %#v", cmd.Env)
	}
	if !containsPrefix(cmd.Env, "GIT_CONFIG_COUNT=") {
		t.Fatalf("temporary git config should be injected: %#v", cmd.Env)
	}
}

func TestNullGlobalConfigPathIsReadableEmptyFile(t *testing.T) {
	// Regression test: GIT_CONFIG_GLOBAL must point at a real, readable, empty
	// file rather than os.DevNull. On Windows os.DevNull is "NUL", which some
	// Git for Windows builds (notably ARM64) refuse to read as global config,
	// failing every git command with "unable to access 'NUL'".
	p := nullGlobalConfigPath()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("GIT_CONFIG_GLOBAL path %q must be accessible: %v", p, err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("GIT_CONFIG_GLOBAL path %q must be a regular file, not a device: %v", p, info.Mode())
	}
	if info.Size() != 0 {
		t.Fatalf("GIT_CONFIG_GLOBAL file %q should be empty, got %d bytes", p, info.Size())
	}
}

// safeDirectoryTestEnv builds a hermetic environment for safe.directory
// reads: global config comes from the given file and the system scope is
// pinned to an empty file so entries baked into the host's real system config
// (for example "safe.directory = *" on GitHub-hosted runners) cannot leak in.
func safeDirectoryTestEnv(t *testing.T, globalConfig string) []string {
	t.Helper()
	emptySystemConfig := filepath.Join(t.TempDir(), "system-gitconfig")
	require.NoError(t, os.WriteFile(emptySystemConfig, nil, 0o600))
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+globalConfig,
		"GIT_CONFIG_SYSTEM="+emptySystemConfig,
		"GIT_CONFIG_NOSYSTEM=0",
	)
}

func TestReadSafeDirectories(t *testing.T) {
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig, []byte("[safe]\n\tdirectory = *\n\tdirectory = /srv/repo\n"), 0o600))

	got := readSafeDirectories(context.Background(), safeDirectoryTestEnv(t, globalConfig), "")

	assert.Equal(t, []string{"*", "/srv/repo"}, got)
}

func TestReadSafeDirectoriesUnset(t *testing.T) {
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig, nil, 0o600))

	assert.Empty(t, readSafeDirectories(context.Background(), safeDirectoryTestEnv(t, globalConfig), ""))
}

func TestReadSafeDirectoriesSystemScope(t *testing.T) {
	dir := t.TempDir()
	systemConfig := filepath.Join(dir, "system-gitconfig")
	require.NoError(t, os.WriteFile(systemConfig, []byte("[safe]\n\tdirectory = /etc/repo\n"), 0o600))
	globalConfig := filepath.Join(dir, "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig, []byte("[safe]\n\tdirectory = /home/repo\n"), 0o600))
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+globalConfig,
		"GIT_CONFIG_SYSTEM="+systemConfig,
		"GIT_CONFIG_NOSYSTEM=0",
	)

	got := readSafeDirectories(context.Background(), env, "")

	assert.Equal(t, []string{"/etc/repo", "/home/repo"}, got, "system entries must come before global entries")
}

func TestReadSafeDirectoriesHonorsNoSystem(t *testing.T) {
	// Regression test: "git config --system" reads the system file even when
	// GIT_CONFIG_NOSYSTEM is set, so readSafeDirectories must skip the system
	// scope itself. Without that, entries git would never honor (for example
	// "safe.directory = *" baked into CI runner images) get forwarded.
	dir := t.TempDir()
	systemConfig := filepath.Join(dir, "system-gitconfig")
	require.NoError(t, os.WriteFile(systemConfig, []byte("[safe]\n\tdirectory = /etc/repo\n"), 0o600))
	globalConfig := filepath.Join(dir, "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig, []byte("[safe]\n\tdirectory = /home/repo\n"), 0o600))
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+globalConfig,
		"GIT_CONFIG_SYSTEM="+systemConfig,
		"GIT_CONFIG_NOSYSTEM=1",
	)

	got := readSafeDirectories(context.Background(), env, "")

	assert.Equal(t, []string{"/home/repo"}, got)
}

func TestReadSafeDirectoriesConditionalInclude(t *testing.T) {
	// Regression test: the probes must run in the command's directory with
	// --includes so includeIf "gitdir:..." entries resolve for the repository
	// the command targets, not for the calling process's working directory.
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	// git matches gitdir patterns against resolved paths, so the pattern must
	// use the symlink-free form (t.TempDir is a symlink on macOS).
	realRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)

	included := filepath.Join(dir, "trusted-gitconfig")
	require.NoError(t, os.WriteFile(included, []byte("[safe]\n\tdirectory = /srv/conditional\n"), 0o600))
	globalConfig := filepath.Join(dir, "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig, []byte(
		"[includeIf \"gitdir:"+filepath.ToSlash(realRepo)+"/\"]\n\tpath = "+filepath.ToSlash(included)+"\n"), 0o600))
	env := safeDirectoryTestEnv(t, globalConfig)

	runner := New()
	runner.Env = env
	_, _, err = runner.Run(context.Background(), repo, nil, "init")
	require.NoError(t, err)

	assert.Equal(t, []string{"/srv/conditional"}, readSafeDirectories(context.Background(), env, repo),
		"include conditional on the target repo must apply")
	assert.Empty(t, readSafeDirectories(context.Background(), env, dir),
		"include conditional on another repo must not apply")
}

func TestCommandEnvForwardsSafeDirectory(t *testing.T) {
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig, []byte("[safe]\n\tdirectory = *\n"), 0o600))

	runner := New()
	runner.Env = safeDirectoryTestEnv(t, globalConfig)
	cmd := runner.Command(context.Background(), "", "status")

	assert.Equal(t, "*", gitConfigValue(strings.Join(cmd.Env, "\n"), "safe.directory"))
	// The sanitized environment must still hide the user's global config from
	// everything except the forwarded safe.directory entries.
	assert.Contains(t, cmd.Env, "GIT_CONFIG_GLOBAL="+nullGlobalConfigPath())
}

func TestCommandEnvForwardsSafeDirectoryForRunnerLiterals(t *testing.T) {
	// Forwarding must be on by default for callers that build a Runner
	// literal instead of using New(); a zero DisableSafeDirectoryForward
	// keeps isolation flags from silently dropping the user's trust entries.
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig, []byte("[safe]\n\tdirectory = /srv/repo\n"), 0o600))

	runner := Runner{
		Env:              safeDirectoryTestEnv(t, globalConfig),
		StripEnv:         true,
		NullGlobalConfig: true,
		NoSystemConfig:   true,
	}
	cmd := runner.Command(context.Background(), "", "status")

	assert.Equal(t, "/srv/repo", gitConfigValue(strings.Join(cmd.Env, "\n"), "safe.directory"))
}

func TestCommandEnvReadsSafeDirectoryFromRunnerEnv(t *testing.T) {
	// The forwarded entries must come from the runner's configured Env, not
	// from the process environment, and one runner's entries must not leak
	// into a runner with a different environment.
	dir := t.TempDir()
	trusted := filepath.Join(dir, "trusted-gitconfig")
	require.NoError(t, os.WriteFile(trusted, []byte("[safe]\n\tdirectory = /trusted/repo\n"), 0o600))
	empty := filepath.Join(dir, "empty-gitconfig")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))

	trustedRunner := New()
	trustedRunner.Env = safeDirectoryTestEnv(t, trusted)
	emptyRunner := New()
	emptyRunner.Env = safeDirectoryTestEnv(t, empty)

	trustedCmd := trustedRunner.Command(context.Background(), "", "status")
	emptyCmd := emptyRunner.Command(context.Background(), "", "status")

	assert.Equal(t, "/trusted/repo", gitConfigValue(strings.Join(trustedCmd.Env, "\n"), "safe.directory"))
	assert.Empty(t, gitConfigValue(strings.Join(emptyCmd.Env, "\n"), "safe.directory"))
}

func TestCommandEnvSkipsSafeDirectoryWhenDisabled(t *testing.T) {
	globalConfig := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(globalConfig, []byte("[safe]\n\tdirectory = *\n"), 0o600))

	runner := New()
	runner.Env = safeDirectoryTestEnv(t, globalConfig)
	runner.DisableSafeDirectoryForward = true
	cmd := runner.Command(context.Background(), "", "status")

	assert.Empty(t, gitConfigValue(strings.Join(cmd.Env, "\n"), "safe.directory"))
}

func TestWithBasicAuthKeepsSecretOutOfCommandEnvironment(t *testing.T) {
	env := captureGitEnv(t, New().WithBasicAuth("alice", "secret-token"))

	for _, secret := range []string{
		"alice",
		"secret-token",
		base64.StdEncoding.EncodeToString([]byte("alice:secret-token")),
		"Authorization: Basic",
		"http.extraHeader",
	} {
		if strings.Contains(env, secret) {
			t.Fatalf("command environment leaked %q:\n%s", secret, env)
		}
	}
	if !strings.Contains(env, "credential.helper") {
		t.Fatalf("basic auth should be supplied through a credential helper:\n%s", env)
	}
}

func TestWithBasicAuthRejectsCommand(t *testing.T) {
	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("Command with basic auth did not panic")
		}
		message := got.(string)
		for _, secret := range []string{"alice", "secret-token", base64.StdEncoding.EncodeToString([]byte("alice:secret-token"))} {
			if strings.Contains(message, secret) {
				t.Fatalf("panic leaked %q: %s", secret, message)
			}
		}
	}()

	New().WithBasicAuth("alice", "secret-token").Command(context.Background(), "", "status")
}

func TestWithBasicAuthRemovesCredentialHelperAfterRun(t *testing.T) {
	env := captureGitEnv(t, New().WithBasicAuth("alice", "secret-token"))
	helper := gitConfigValue(env, "credential.helper")
	if helper == "" {
		t.Fatalf("credential.helper not found in env:\n%s", env)
	}
	if _, err := os.Stat(helper); !os.IsNotExist(err) {
		t.Fatalf("credential helper file still exists after Run: stat err = %v", err)
	}
}

func captureGitEnv(t *testing.T, runner Runner) string {
	t.Helper()
	binDir := t.TempDir()
	envPath := filepath.Join(t.TempDir(), "env")
	gitPath := filepath.Join(binDir, "git")
	script := "#!/bin/sh\nenv > " + shellSingleQuote(envPath) + "\n"
	if os.PathSeparator == '\\' {
		gitPath += ".bat"
		script = "@echo off\r\nset > " + shellDoubleQuote(envPath) + "\r\n"
	}
	if err := os.WriteFile(gitPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	pathEnv := binDir + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", pathEnv)
	runner.Env = []string{"PATH=" + pathEnv}
	if _, _, err := runner.Run(context.Background(), "", nil, "version"); err != nil {
		t.Fatal(err)
	}
	envBytes, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(envBytes)
}

func gitConfigValue(env, key string) string {
	values := map[string]string{}
	keys := map[string]string{}
	for line := range strings.SplitSeq(env, "\n") {
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSuffix(value, "\r")
		if index, ok := strings.CutPrefix(name, "GIT_CONFIG_KEY_"); ok {
			keys[index] = value
		}
		if index, ok := strings.CutPrefix(name, "GIT_CONFIG_VALUE_"); ok {
			values[index] = value
		}
	}
	for index, gotKey := range keys {
		if gotKey == key {
			return values[index]
		}
	}
	return ""
}

func containsPrefix(values []string, prefix string) bool {
	return slices.ContainsFunc(values, func(value string) bool {
		return strings.HasPrefix(value, prefix)
	})
}

func shellDoubleQuote(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}
