package gitworktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	gittest "github.com/kenn-io/kit/git/test"
)

func TestCreateCaptureAndApplyPatch(t *testing.T) {
	ctx := context.Background()
	repo := gittest.NewRepoWithCommit(t)

	wt, err := Create(ctx, repo.Root, "HEAD", Options{ParentDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wt.Close(ctx) })

	if err := os.WriteFile(filepath.Join(wt.Dir, "base.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch, err := wt.CapturePatch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if patch == "" {
		t.Fatal("expected non-empty patch")
	}
	if err := CheckPatch(ctx, repo.Root, patch); err != nil {
		t.Fatalf("patch should apply cleanly: %v", err)
	}
	if err := ApplyPatch(ctx, repo.Root, patch); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(repo.Root, "base.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "changed\n" {
		t.Fatalf("base.txt = %q, want changed", got)
	}
	if err := CheckPatch(ctx, repo.Root, patch); err == nil {
		t.Fatal("patch should conflict after being applied")
	} else {
		var conflict *PatchConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("err = %T %v, want PatchConflictError", err, err)
		}
	}
}

func TestGitmodulesFileProtocolDetection(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{`url = "../local.git"`, true},
		{`url = file:///tmp/local.git`, true},
		{`url = https://github.com/acme/widget.git`, false},
		{`path = deps/widget`, false},
	}
	for _, tt := range tests {
		url, ok := ParseGitmodulesURL(tt.line)
		got := ok && IsFileProtocolURL(url)
		if got != tt.want {
			t.Fatalf("line %q got %v want %v", tt.line, got, tt.want)
		}
	}
}
