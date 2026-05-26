package gitcmd

import (
	"context"
	"encoding/base64"
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
	if !slices.Contains(cmd.Env, "GIT_CONFIG_GLOBAL=/dev/null") {
		t.Fatalf("global config should be nulled: %#v", cmd.Env)
	}
	if !containsPrefix(cmd.Env, "GIT_CONFIG_COUNT=") {
		t.Fatalf("temporary git config should be injected: %#v", cmd.Env)
	}
}

func TestWithBasicAuthKeepsSecretOutOfCommandEnvironment(t *testing.T) {
	runner := New().WithBasicAuth("alice", "secret-token")
	runner.Env = []string{"PATH=/bin"}

	cmd := runner.Command(context.Background(), "", "ls-remote", "https://github.com/acme/widget.git")
	env := strings.Join(cmd.Env, "\n")

	for _, secret := range []string{
		"alice",
		"secret-token",
		base64.StdEncoding.EncodeToString([]byte("alice:secret-token")),
		"Authorization: Basic",
		"http.extraHeader",
	} {
		if strings.Contains(env, secret) {
			t.Fatalf("command environment leaked %q: %#v", secret, cmd.Env)
		}
	}
	if !strings.Contains(env, "credential.helper") {
		t.Fatalf("basic auth should be supplied through a credential helper: %#v", cmd.Env)
	}
}

func containsPrefix(values []string, prefix string) bool {
	return slices.ContainsFunc(values, func(value string) bool {
		return strings.HasPrefix(value, prefix)
	})
}
