package gitenv

import (
	"slices"
	"testing"
)

func TestStripAllRemovesEveryGitVariable(t *testing.T) {
	env := []string{
		"PATH=/bin",
		"GIT_DIR=/parent/.git",
		"GIT_SSL_NO_VERIFY=1",
		"GIT_DEFAULT_HASH=sha256",
		"SSH_ASKPASS=/tmp/askpass",
	}

	got := StripAll(env)

	if slices.Contains(got, "PATH=/bin") == false {
		t.Fatalf("PATH should be preserved: %#v", got)
	}
	for _, removed := range env[1:] {
		if slices.Contains(got, removed) {
			t.Fatalf("%q should have been removed from %#v", removed, got)
		}
	}
}

func TestStripInheritedPreservesDiagnosticsButRemovesContext(t *testing.T) {
	env := []string{
		"GIT_DIR=/parent/.git",
		"GIT_CONFIG_COUNT=1",
		"GIT_TRACE=1",
		"GIT_SSL_NO_VERIFY=1",
	}

	got := StripInherited(env)

	if slices.Contains(got, "GIT_DIR=/parent/.git") {
		t.Fatalf("GIT_DIR should have been removed: %#v", got)
	}
	if slices.Contains(got, "GIT_CONFIG_COUNT=1") {
		t.Fatalf("GIT_CONFIG_COUNT should have been removed: %#v", got)
	}
	if !slices.Contains(got, "GIT_TRACE=1") || !slices.Contains(got, "GIT_SSL_NO_VERIFY=1") {
		t.Fatalf("diagnostic/transport env should be preserved: %#v", got)
	}
}
