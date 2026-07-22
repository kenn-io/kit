package gitremote

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestClonePathRejectsTraversalAndSeparators(t *testing.T) {
	tests := []Identity{
		{Host: "", Owner: "acme", Name: "widget"},
		{Host: "github.com", Owner: "", Name: "widget"},
		{Host: "github.com", Owner: "acme/../evil", Name: "widget"},
		{Host: "github.com", Owner: "/acme", Name: "widget"},
		{Host: "github.com", Owner: `acme\evil`, Name: "widget"},
		{Host: "github.com", Owner: "acme", Name: "nested/widget"},
	}
	for _, id := range tests {
		if _, err := ClonePath(t.TempDir(), id); err == nil {
			t.Fatalf("ClonePath(%+v) succeeded, want error", id)
		}
	}
}

func TestClonePathPartitionsByHost(t *testing.T) {
	base := t.TempDir()
	path, err := ClonePath(base, Identity{Host: "github.com", Owner: "acme", Name: "widget"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "github.com", "acme", "widget.git")
	if path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
}

func TestValidateRemoteIdentity(t *testing.T) {
	id := Identity{Host: "github.com", Owner: "acme", Name: "widget"}
	if err := ValidateRemoteIdentity(id, "git@github.com:acme/widget.git"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRemoteIdentity(id, "https://evil.example.com/acme/widget.git"); err == nil {
		t.Fatal("expected host mismatch")
	}
	if err := ValidateRemoteIdentity(id, "https://github.com/other/widget.git"); err == nil {
		t.Fatal("expected repo mismatch")
	}
	if err := ValidateRemoteIdentity(id, "/tmp/widget.git"); err != nil {
		t.Fatalf("local paths should be accepted: %v", err)
	}
}

func TestUnsafeForAutomationRejectsCredentialAndCommandSurfaces(t *testing.T) {
	tests := []struct {
		remoteURL string
		want      bool
	}{
		{remoteURL: "https://github.com/acme/widget.git"},
		{remoteURL: "http://github.com/acme/widget.git", want: true},
		{remoteURL: "git://github.com/acme/widget.git", want: true},
		{remoteURL: "https://token@github.com/acme/widget.git", want: true},
		{remoteURL: "git@github.com:acme/widget.git"},
		{remoteURL: "ssh://git@github.com/acme/widget.git"},
		{remoteURL: "ssh://git:secret@github.com/acme/widget.git", want: true},
		{remoteURL: "git:secret@github.com:acme/widget.git", want: true},
		{remoteURL: "https://github.com/acme/widget.git?access_token=secret", want: true},
		{remoteURL: "https://github.com/acme/widget.git#token", want: true},
		{remoteURL: "https://github.com/%zz", want: true},
		{remoteURL: "git@github.com:acme/widget.git?access_token=secret", want: true},
		{remoteURL: "corp::--token=secret", want: true},
		{remoteURL: "::--token=secret", want: true},
		{remoteURL: "evil://github.com/acme/widget.git", want: true},
		{remoteURL: "git+ssh://git@github.com/acme/widget.git"},
		{remoteURL: "ssh://git@[2001:db8::1]/acme/widget.git"},
		{remoteURL: "/tmp/widget.git"},
	}

	for _, test := range tests {
		t.Run(test.remoteURL, func(t *testing.T) {
			assert.Equal(t, test.want, UnsafeForAutomation(test.remoteURL))
		})
	}
}
