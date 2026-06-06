package gitcmd

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
