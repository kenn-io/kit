package managedworktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	gitlock "go.kenn.io/kit/git/lock"
	"go.kenn.io/kit/safefileio"
)

var repositoryMutationLocks = gitlock.New("repository.lock")

type repositoryMutationLockContextKey struct{}

func acquireRepositoryMutationLock(
	ctx context.Context, root string,
) (context.Context, func() error, error) {
	canonicalRoot, err := repositoryMutationIdentity(root)
	if err != nil {
		return ctx, nil, fmt.Errorf("resolve repository lock root: %w", err)
	}
	if heldRoot, _ := ctx.Value(repositoryMutationLockContextKey{}).(string); heldRoot == canonicalRoot {
		return ctx, func() error { return nil }, nil
	}

	userID, err := safefileio.CurrentUserID()
	if err != nil {
		return ctx, nil, fmt.Errorf("identify repository lock owner: %w", err)
	}
	base := filepath.Join(os.TempDir(), "kit-git-locks-"+userID)
	if err := safefileio.EnsurePrivateDir(base); err != nil {
		return ctx, nil, fmt.Errorf("prepare repository lock directory: %w", err)
	}
	sum := sha256.Sum256([]byte(canonicalRoot))
	lockRoot := filepath.Join(base, hex.EncodeToString(sum[:]))
	if err := safefileio.EnsurePrivateDir(lockRoot); err != nil {
		return ctx, nil, fmt.Errorf("prepare repository-specific lock directory: %w", err)
	}
	held, err := repositoryMutationLocks.Acquire(ctx, lockRoot)
	if err != nil {
		return ctx, nil, err
	}
	return context.WithValue(ctx, repositoryMutationLockContextKey{}, canonicalRoot), held.Unlock, nil
}

func repositoryMutationIdentity(root string) (string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	root = filepath.Clean(root)
	gitDir := filepath.Join(root, ".git")
	info, statErr := os.Lstat(gitDir)
	switch {
	case statErr == nil && info.IsDir():
	case statErr == nil && info.Mode().IsRegular():
		contents, readErr := os.ReadFile(gitDir)
		if readErr != nil {
			return "", readErr
		}
		line := strings.TrimSpace(string(contents))
		value, found := strings.CutPrefix(line, "gitdir:")
		if !found || strings.TrimSpace(value) == "" {
			return "", fmt.Errorf("invalid .git file in %s", root)
		}
		gitDir = strings.TrimSpace(value)
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(root, gitDir)
		}
	case statErr != nil && !os.IsNotExist(statErr):
		return "", statErr
	case statErr == nil:
		return "", fmt.Errorf("unsupported .git entry in %s", root)
	default:
		// Bare repositories store their administrative files at the root.
		gitDir = root
	}
	gitDir = filepath.Clean(gitDir)
	commonPath := filepath.Join(gitDir, "commondir")
	if contents, readErr := os.ReadFile(commonPath); readErr == nil {
		commonDir := strings.TrimSpace(string(contents))
		if commonDir == "" {
			return "", fmt.Errorf("empty Git common directory in %s", commonPath)
		}
		if !filepath.IsAbs(commonDir) {
			commonDir = filepath.Join(gitDir, commonDir)
		}
		gitDir = filepath.Clean(commonDir)
	} else if !os.IsNotExist(readErr) {
		return "", readErr
	}
	if resolved, resolveErr := filepath.EvalSymlinks(gitDir); resolveErr == nil {
		gitDir = resolved
	}
	identity, err := repositoryFilesystemIdentity(gitDir)
	if err != nil {
		return "", fmt.Errorf("identify Git common directory %s: %w", gitDir, err)
	}
	return identity, nil
}
