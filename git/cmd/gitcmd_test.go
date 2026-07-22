package gitcmd

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

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

func TestReadSafeDirectoriesBoundsProbeRuntime(t *testing.T) {
	origTimeout := safeDirectoryProbeTimeout
	safeDirectoryProbeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { safeDirectoryProbeTimeout = origTimeout })

	binDir := buildSleepingGit(t)
	pathEnv := binDir + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", pathEnv)
	env := append(os.Environ(), "PATH="+pathEnv, "GIT_CONFIG_NOSYSTEM=0")

	start := time.Now()
	got := readSafeDirectories(context.Background(), env, "")

	assert.Empty(t, got)
	assert.Less(t, time.Since(start), 3*time.Second, "safe.directory probes are best-effort and must not stall git commands")
}

func TestReadSafeDirectoriesConditionalInclude(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	// Regression test: the probes must run in the command's directory with
	// --includes so includeIf "gitdir:..." entries resolve for the repository
	// the command targets, not for the calling process's working directory.
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	require.NoError(os.Mkdir(repo, 0o755))
	// git matches gitdir patterns against resolved paths, so the pattern must
	// use the symlink-free form (t.TempDir is a symlink on macOS).
	realRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(err)

	included := filepath.Join(dir, "trusted-gitconfig")
	require.NoError(os.WriteFile(included, []byte("[safe]\n\tdirectory = /srv/conditional\n"), 0o600))
	globalConfig := filepath.Join(dir, "gitconfig")
	require.NoError(os.WriteFile(globalConfig, []byte(
		"[includeIf \"gitdir:"+filepath.ToSlash(realRepo)+"/\"]\n\tpath = "+filepath.ToSlash(included)+"\n"), 0o600))
	env := safeDirectoryTestEnv(t, globalConfig)

	runner := New()
	runner.Env = env
	_, _, err = runner.Run(context.Background(), repo, nil, "init")
	require.NoError(err)

	assert.Equal([]string{"/srv/conditional"}, readSafeDirectories(context.Background(), env, repo),
		"include conditional on the target repo must apply")
	assert.Empty(readSafeDirectories(context.Background(), env, dir),
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
	require := require.New(t)
	assert := assert.New(t)
	// The forwarded entries must come from the runner's configured Env, not
	// from the process environment, and one runner's entries must not leak
	// into a runner with a different environment.
	dir := t.TempDir()
	trusted := filepath.Join(dir, "trusted-gitconfig")
	require.NoError(os.WriteFile(trusted, []byte("[safe]\n\tdirectory = /trusted/repo\n"), 0o600))
	empty := filepath.Join(dir, "empty-gitconfig")
	require.NoError(os.WriteFile(empty, nil, 0o600))

	trustedRunner := New()
	trustedRunner.Env = safeDirectoryTestEnv(t, trusted)
	emptyRunner := New()
	emptyRunner.Env = safeDirectoryTestEnv(t, empty)

	trustedCmd := trustedRunner.Command(context.Background(), "", "status")
	emptyCmd := emptyRunner.Command(context.Background(), "", "status")

	assert.Equal("/trusted/repo", gitConfigValue(strings.Join(trustedCmd.Env, "\n"), "safe.directory"))
	assert.Empty(gitConfigValue(strings.Join(emptyCmd.Env, "\n"), "safe.directory"))
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

func TestCredentialResponseIsPrivateDataAndCleanupIsIdempotent(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	path, cleanup, err := (basicAuth{
		username: "alice",
		password: "secret-token",
	}).credentialResponse()
	require.NoError(err)
	t.Cleanup(cleanup)

	info, err := os.Stat(path)
	require.NoError(err)
	assert.True(info.Mode().IsRegular())
	assert.Zero(info.Mode() & 0o111)
	if runtime.GOOS != "windows" {
		assert.Equal(os.FileMode(0o600), info.Mode().Perm())
	}
	contents, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal("username=alice\npassword=secret-token\n", string(contents))

	cleanup()
	cleanup()
	_, err = os.Stat(path)
	assert.ErrorIs(err, os.ErrNotExist)
}

func TestCredentialResponseRejectsProtocolDelimitersBeforeCreatingFile(t *testing.T) {
	tests := []struct {
		name     string
		username string
		password string
		field    string
		secret   string
	}{
		{name: "username LF", username: "ali\nce", password: "token", field: "username", secret: "ali\nce"},
		{name: "username CR", username: "ali\rce", password: "token", field: "username", secret: "ali\rce"},
		{name: "username NUL", username: "ali\x00ce", password: "token", field: "username", secret: "ali\x00ce"},
		{name: "password LF", username: "alice", password: "token\nquit=1", field: "password", secret: "token\nquit=1"},
		{name: "password CR", username: "alice", password: "token\rquit=1", field: "password", secret: "token\rquit=1"},
		{name: "password NUL", username: "alice", password: "token\x00quit=1", field: "password", secret: "token\x00quit=1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require := require.New(t)
			assert := assert.New(t)
			tempDir := t.TempDir()
			t.Setenv("TMPDIR", tempDir)
			t.Setenv("TMP", tempDir)
			t.Setenv("TEMP", tempDir)

			path, cleanup, err := (basicAuth{
				username: tt.username,
				password: tt.password,
			}).credentialResponse()
			require.Error(err)
			assert.Empty(path)
			assert.Contains(err.Error(), tt.field)
			assert.NotContains(err.Error(), tt.secret)
			assert.NotPanics(cleanup)

			entries, readErr := os.ReadDir(tempDir)
			require.NoError(readErr)
			assert.Empty(entries)
		})
	}
}

func TestWithBasicAuthKeepsSecretOutOfCommandEnvironment(t *testing.T) {
	assert := assert.New(t)
	env := captureGitEnv(t, New().WithBasicAuth("alice", "secret-token"))

	for _, secret := range []string{
		"alice",
		"secret-token",
		base64.StdEncoding.EncodeToString([]byte("alice:secret-token")),
		"Authorization: Basic",
		"http.extraHeader",
	} {
		assert.NotContains(env, secret)
	}
	helper := gitConfigValue(env, "credential.helper")
	assert.NotEmpty(helper)
	assert.True(strings.HasPrefix(helper, "!"), "credential helper must be an inline shell snippet, got %q", helper)
}

func TestWithBasicAuthRoundTripsCredentialProtocol(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	username := `al ice'$\"`
	password := `sec ret'\"$\\;|&()`
	request := "protocol=https\nhost=example.invalid\n\n"

	stdout, stderr, err := New().WithBasicAuth(username, password).Run(
		context.Background(), "", strings.NewReader(request), "credential", "fill",
	)

	require.NoError(err, string(stderr))
	assert.Empty(stderr)
	assert.Contains(string(stdout), "username="+username+"\n")
	assert.Contains(string(stdout), "password="+password+"\n")
}

func TestWithBasicAuthStoreAndEraseDoNotDiscloseCredentials(t *testing.T) {
	request := "protocol=https\nhost=example.invalid\nusername=alice\npassword=secret-token\n\n"
	for _, operation := range []string{"approve", "reject"} {
		t.Run(operation, func(t *testing.T) {
			stdout, stderr, err := New().WithBasicAuth("alice", "secret-token").Run(
				context.Background(), "", strings.NewReader(request), "credential", operation,
			)
			require.NoError(t, err, string(stderr))
			assert.Empty(t, stdout)
			assert.Empty(t, stderr)
		})
	}
}

func TestWithBasicAuthRejectsCredentialProtocolInjection(t *testing.T) {
	password := "secret-token\nusername=mallory"
	request := "protocol=https\nhost=example.invalid\n\n"

	stdout, stderr, err := New().WithBasicAuth("alice", password).Run(
		context.Background(), "", strings.NewReader(request), "credential", "fill",
	)

	require.Error(t, err)
	assert.Empty(t, stdout)
	assert.NotContains(t, string(stderr), password)
	assert.NotContains(t, err.Error(), password)
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

func TestWithBasicAuthRemovesCredentialResponseAfterRun(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("TMP", tempDir)
	t.Setenv("TEMP", tempDir)

	captureGitEnv(t, New().WithBasicAuth("alice", "secret-token"))

	responses, err := filepath.Glob(filepath.Join(tempDir, "gitcmd-credential-response-*"))
	require.NoError(t, err)
	assert.Empty(t, responses)
}

func TestWithBasicAuthRemovesCredentialResponseAfterGitFailure(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("TMP", tempDir)
	t.Setenv("TEMP", tempDir)

	binDir := t.TempDir()
	gitPath := filepath.Join(binDir, "git")
	script := "#!/bin/sh\nexit 1\n"
	if runtime.GOOS == "windows" {
		gitPath += ".bat"
		script = "@exit /b 1\r\n"
	}
	require.NoError(t, os.WriteFile(gitPath, []byte(script), 0o700))

	pathEnv := binDir + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", pathEnv)
	runner := New().WithBasicAuth("alice", "secret-token")
	runner.Env = []string{"PATH=" + pathEnv}
	_, _, err := runner.Run(context.Background(), "", nil, "version")
	require.Error(t, err)

	responses, globErr := filepath.Glob(filepath.Join(tempDir, "gitcmd-credential-response-*"))
	require.NoError(t, globErr)
	assert.Empty(t, responses)
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

func buildSleepingGit(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	exeName := "git"
	if runtime.GOOS == "windows" {
		exeName += ".exe"
	}
	exePath := filepath.Join(binDir, exeName)
	srcPath := filepath.Join(t.TempDir(), "main.go")
	require.NoError(t, os.WriteFile(srcPath, []byte(`package main

import "time"

func main() {
	time.Sleep(10 * time.Second)
}
`), 0o600))
	cmd := exec.Command("go", "build", "-o", exePath, srcPath)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return binDir
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
