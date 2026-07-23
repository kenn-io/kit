package managedworktree

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitremote "go.kenn.io/kit/git/remote"
)

// ChangeRequestErrorKind classifies failures callers commonly present
// differently from local worktree errors.
type ChangeRequestErrorKind string

const (
	ChangeRequestAuthentication   ChangeRequestErrorKind = "authentication"
	ChangeRequestNetwork          ChangeRequestErrorKind = "network"
	ChangeRequestInaccessibleHead ChangeRequestErrorKind = "inaccessible_head"
	ChangeRequestHeadChanged      ChangeRequestErrorKind = "head_changed"
	// ChangeRequestUnsupportedGit is retained for callers that map older Kit
	// errors. The trusted-repository lifecycle does not impose a Git version
	// policy.
	ChangeRequestUnsupportedGit ChangeRequestErrorKind = "unsupported_git"
)

// ChangeRequestError describes a merge-request fetch or verification failure.
type ChangeRequestError struct {
	Kind    ChangeRequestErrorKind
	Message string
	Cause   error
}

func (e *ChangeRequestError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return string(e.Kind)
}

func (e *ChangeRequestError) Unwrap() error { return e.Cause }

// MergeRequestWorktreeOptions parameterizes CreateWorktreeFromMergeRequest.
// ProjectRoot, Branch, and Number are required. HeadBranch and
// HeadRepoCloneURL come from the synced merge-request metadata;
// ProjectRepoIdentity is the CloneURLIdentity-normalized identity of the
// project's own repository, used to recognize same-repo merge requests.
type MergeRequestWorktreeOptions struct {
	ProjectRoot string
	Branch      string
	Path        string
	BaseDir     string
	// SetupScript is an explicitly configured, caller-trusted script. It runs
	// after the untrusted request tree is materialized; callers must sandbox
	// it if the script evaluates or executes content from that tree. It must
	// already exist as a regular file outside the worktree destination.
	SetupScript           string
	WorktreeName          string
	HookEnvironmentPrefix string
	Runner                gitcmd.Runner
	RunGit                GitRunner
	RunHook               HookRunner

	Number     int
	HeadBranch string
	// HeadRepoCloneURL must not contain HTTP credentials, passwords, query
	// parameters, or fragments. Supply transport authentication through the
	// configured runner instead.
	HeadRepoCloneURL string
	// ExpectedHeadSHA, when set, must match the fetched request head before
	// any worktree or local branch is created.
	ExpectedHeadSHA string
	// ProjectRepoIdentity is normally a CloneURLIdentity value. Explicit
	// relative local paths such as ../origin.git resolve against ProjectRoot.
	ProjectRepoIdentity string
	// Platform is the project's platform kind ("github", "gitlab", ...);
	// it selects the remote ref that carries the merge request head when
	// the head must be fetched by number rather than by branch.
	Platform string
}

// mergeRequestRemoteTarget describes how to materialize a merge request
// head locally: the fetch that makes the checkout ref available, the ref
// the new branch starts at, an optional second fetch that makes upstream
// tracking possible, and the tracking remote/merge-ref pair.
type mergeRequestRemoteTarget struct {
	checkoutFetch        []string
	checkoutRef          string
	temporaryCheckoutRef string
	trackingFetch        []string
	trackingRef          string
	trackingRemote       string
	trackingMergeRef     string
}

// CreateWorktreeFromMergeRequest materializes an untrusted merge request head
// as a new worktree. It verifies the expected head when supplied, creates the
// worktree without checkout-time hooks or configured attribute programs,
// persists that isolation for later Git commands, and disables implicit
// submodule recursion. The existing repository, its configuration, provider
// metadata, remotes, and explicitly configured setup hook remain trusted.
//
// The function also configures upstream tracking when possible, non-fatally
// skipping it when the fork cannot be fetched. Failures after the worktree
// exists roll it back.
func CreateWorktreeFromMergeRequest(
	ctx context.Context, opts MergeRequestWorktreeOptions,
) (CreateWorktreeResult, error) {
	ctx = withLifecycleExecution(ctx, opts.Runner, opts.RunGit, opts.RunHook)
	root, branch, err := requireRootAndBranch(
		opts.ProjectRoot, opts.Branch,
	)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	if opts.Number < 1 {
		return CreateWorktreeResult{}, fmt.Errorf(
			"merge request number is required",
		)
	}
	if err := validateWorktreeConfigCompatibility(ctx, root); err != nil {
		return CreateWorktreeResult{}, err
	}
	if err := validateBranchName(ctx, root, branch); err != nil {
		return CreateWorktreeResult{}, err
	}
	if err := validateUntrustedTreeCheckoutGitVersion(ctx, root); err != nil {
		return CreateWorktreeResult{}, err
	}
	path, err := resolveWorktreeDestination(
		root, branch, opts.Path, opts.BaseDir,
	)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	branchExisted, err := localBranchExists(ctx, root, branch)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	if branchExisted {
		return CreateWorktreeResult{}, fmt.Errorf(
			"%w: %s", ErrBranchAlreadyExists, branch,
		)
	}
	hookScript, err := resolveMergeRequestHookScript(
		root, path, opts.SetupScript,
	)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	opts.HeadRepoCloneURL, err = canonicalizeMergeRequestCloneURL(
		root, opts.HeadRepoCloneURL,
	)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	opts.ProjectRepoIdentity, err = canonicalizeMergeRequestProjectIdentity(
		root, opts.ProjectRepoIdentity,
	)
	if err != nil {
		return CreateWorktreeResult{}, err
	}

	isolation, err := prepareUntrustedTreeIsolation(ctx, root)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	target, err := prepareMergeRequestRemote(ctx, root, opts)
	if err != nil {
		return CreateWorktreeResult{}, err
	}

	if out, err := runLifecycleGit(
		ctx, root, target.checkoutFetch...,
	); err != nil {
		cleanupErr := clearTemporaryMergeRequestRef(
			context.WithoutCancel(ctx), root, target.temporaryCheckoutRef,
		)
		if ctx.Err() != nil {
			return CreateWorktreeResult{}, errors.Join(ctx.Err(), cleanupErr)
		}
		return CreateWorktreeResult{}, errors.Join(
			classifyChangeRequestFetchError(out, err), cleanupErr,
		)
	}
	checkoutOID, err := resolveMergeRequestOID(ctx, root, target.checkoutRef)
	cleanupErr := clearTemporaryMergeRequestRef(
		context.WithoutCancel(ctx), root, target.temporaryCheckoutRef,
	)
	if err != nil {
		if ctx.Err() != nil {
			return CreateWorktreeResult{}, errors.Join(ctx.Err(), cleanupErr)
		}
		return CreateWorktreeResult{}, &ChangeRequestError{
			Kind:    ChangeRequestInaccessibleHead,
			Message: "resolve fetched merge request head",
			Cause:   errors.Join(err, cleanupErr),
		}
	}
	if cleanupErr != nil {
		return CreateWorktreeResult{}, cleanupErr
	}
	if expected := strings.TrimSpace(opts.ExpectedHeadSHA); expected != "" {
		expectedOID, resolveErr := resolveMergeRequestOID(ctx, root, expected)
		if resolveErr != nil || !strings.EqualFold(checkoutOID, expectedOID) {
			if ctx.Err() != nil {
				return CreateWorktreeResult{}, ctx.Err()
			}
			return CreateWorktreeResult{}, &ChangeRequestError{
				Kind: ChangeRequestHeadChanged,
				Message: fmt.Sprintf(
					"merge request head changed: expected %s, fetched %s",
					expected, checkoutOID,
				),
				Cause: resolveErr,
			}
		}
	}
	trackingEnabled := target.trackingRemote != "" &&
		target.trackingMergeRef != ""
	if trackingEnabled && len(target.trackingFetch) > 0 {
		// The tracking fetch is best-effort: a fork that has vanished
		// or is unreachable must not block importing via the pull ref.
		if _, err := runLifecycleGit(
			ctx, root, target.trackingFetch...,
		); err != nil {
			if ctx.Err() != nil {
				return CreateWorktreeResult{}, ctx.Err()
			}
			trackingEnabled = false
		} else if trackingOID, resolveErr := resolveMergeRequestOID(
			ctx, root, target.trackingRef,
		); resolveErr != nil || !strings.EqualFold(trackingOID, checkoutOID) {
			if ctx.Err() != nil {
				return CreateWorktreeResult{}, ctx.Err()
			}
			trackingEnabled = false
		}
	}

	// --no-track: tracking is configured explicitly below; without it git
	// auto-tracks the remote-tracking start point (e.g. the read-only
	// pull ref), which is wrong for pushes.
	if out, err := runLifecycleGitWithRunner(
		ctx, isolation.runner, root,
		"worktree", "add", "--no-checkout", "--no-track", "-b", branch, path,
		"--", checkoutOID,
	); err != nil {
		return CreateWorktreeResult{}, classifyWorktreeGitError(out, err)
	}
	if err := rejectCommandScopeIsolationOverrides(
		ctx, path, lifecycleRunner(ctx),
	); err != nil {
		_, cleanupErr := rollbackCreatedWorktreeWithResult(
			context.WithoutCancel(ctx), root, path, branch, true,
		)
		return CreateWorktreeResult{}, errors.Join(err, cleanupErr)
	}
	isolation, err = completeUntrustedTreeIsolation(ctx, path, isolation)
	if err != nil {
		_, cleanupErr := rollbackCreatedWorktreeWithResult(
			context.WithoutCancel(ctx), root, path, branch, true,
		)
		return CreateWorktreeResult{}, errors.Join(err, cleanupErr)
	}
	if err := persistUntrustedTreeIsolation(
		ctx, root, path, isolation,
	); err != nil {
		_, cleanupErr := rollbackCreatedWorktreeWithResult(
			context.WithoutCancel(ctx), root, path, branch, true,
		)
		return CreateWorktreeResult{}, errors.Join(err, cleanupErr)
	}
	if err := materializeUntrustedTree(ctx, path, isolation); err != nil {
		_, cleanupErr := rollbackCreatedWorktreeWithResult(
			context.WithoutCancel(ctx), root, path, branch, true,
		)
		return CreateWorktreeResult{}, errors.Join(err, cleanupErr)
	}
	if err := rejectConfigOriginsInsideWorktree(
		ctx, path, isolation.runner,
	); err != nil {
		_, cleanupErr := rollbackCreatedWorktreeWithResult(
			context.WithoutCancel(ctx), root, path, branch, true,
		)
		return CreateWorktreeResult{}, errors.Join(err, cleanupErr)
	}
	isolatedCtx := withLifecycleExecution(
		ctx, isolation.runner, lifecycleGitRunner(ctx), lifecycleHookRunner(ctx),
	)
	result, err := snapshotCreateWorktreeResult(
		isolatedCtx, root, path, branch, true,
	)
	if err != nil {
		_, cleanupErr := rollbackCreatedWorktreeWithResult(
			context.WithoutCancel(ctx), root, path, branch, true,
		)
		return CreateWorktreeResult{}, errors.Join(err, cleanupErr)
	}

	if trackingEnabled {
		if err := configureMergeRequestTracking(
			ctx, root, path, branch, target,
		); err != nil {
			_, cleanupErr := rollbackCreatedWorktreeWithResult(
				context.WithoutCancel(ctx), root, path, branch, true,
			)
			return result, errors.Join(err, cleanupErr)
		}
	}

	if hookScript != "" {
		if hookErr := runLifecycleHook(
			ctx, hookScript, root, path, branch, opts.WorktreeName,
			opts.HookEnvironmentPrefix,
		); hookErr != nil {
			_, cleanupErr := rollbackCreatedWorktreeWithResult(
				context.WithoutCancel(ctx), root, path, branch, true,
			)
			return result, errors.Join(hookErr, cleanupErr)
		}
		result.HookRan = true
		result.HookScript = hookScript
	}
	return result, nil
}

// prepareMergeRequestRemote decides how to fetch the merge request head.
// Same-repo heads fetch the branch from origin directly with tracking;
// fork heads fetch the checkout from the platform pull ref while tracking
// a dedicated fork remote; heads with no clone URL fetch the pull ref with
// no tracking.
func prepareMergeRequestRemote(
	ctx context.Context, root string, opts MergeRequestWorktreeOptions,
) (mergeRequestRemoteTarget, error) {
	headBranch := strings.TrimSpace(opts.HeadBranch)

	cloneURL := strings.TrimSpace(opts.HeadRepoCloneURL)
	sameRepo := headBranch != "" && cloneURL != "" &&
		opts.ProjectRepoIdentity != "" &&
		mergeRequestRepositoriesEqual(cloneURL, opts.ProjectRepoIdentity)
	if sameRepo {
		return mergeRequestRemoteTarget{
			checkoutFetch: []string{
				"fetch", "--no-tags", "--no-write-fetch-head",
				"--no-recurse-submodules", "origin",
				fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s",
					headBranch, headBranch),
			},
			checkoutRef:      "refs/remotes/origin/" + headBranch,
			trackingRemote:   "origin",
			trackingMergeRef: "refs/heads/" + headBranch,
		}, nil
	}

	// The platform's merge-request head ref (refs/pull/<n>/head on GitHub,
	// Forgejo, and Gitea; refs/merge-requests/<n>/head on GitLab) carries
	// the head commit regardless of where the head branch lives.
	headRef := mergeRequestHeadRef(opts.Platform, opts.Number)
	localRef, err := temporaryMergeRequestRef(opts.Number)
	if err != nil {
		return mergeRequestRemoteTarget{}, err
	}
	pullRefFetch := []string{
		"fetch", "--no-tags", "--no-write-fetch-head",
		"--no-recurse-submodules", "origin",
		fmt.Sprintf("+%s:%s", headRef, localRef),
	}
	pullRef := localRef

	if cloneURL == "" || headBranch == "" {
		return mergeRequestRemoteTarget{
			checkoutFetch:        pullRefFetch,
			checkoutRef:          pullRef,
			temporaryCheckoutRef: pullRef,
		}, nil
	}

	preferredName := sanitizeRemoteName(ownerFromCloneURL(cloneURL))
	if preferredName == "" {
		preferredName = "mr-head"
	}
	remoteName, err := ensureFetchRemote(ctx, root, preferredName, cloneURL)
	if err != nil {
		return mergeRequestRemoteTarget{}, err
	}
	return mergeRequestRemoteTarget{
		checkoutFetch:        pullRefFetch,
		checkoutRef:          pullRef,
		temporaryCheckoutRef: pullRef,
		trackingFetch: []string{
			"fetch", "--no-tags", "--no-write-fetch-head",
			"--no-recurse-submodules", remoteName,
			fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s",
				headBranch, remoteName, headBranch),
		},
		trackingRef:      "refs/remotes/" + remoteName + "/" + headBranch,
		trackingRemote:   remoteName,
		trackingMergeRef: "refs/heads/" + headBranch,
	}, nil
}

func temporaryMergeRequestRef(number int) (string, error) {
	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("create temporary merge request ref: %w", err)
	}
	return fmt.Sprintf(
		"refs/kit/merge-requests/%d/%x", number, suffix,
	), nil
}

func clearTemporaryMergeRequestRef(
	ctx context.Context, root, ref string,
) error {
	if ref == "" {
		return nil
	}
	out, err := runLifecycleGit(
		ctx, root, "update-ref", "--no-deref", "-d", ref,
	)
	if err != nil {
		return fmt.Errorf(
			"remove temporary merge request ref: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	return nil
}

func mergeRequestRepositoriesEqual(left, right string) bool {
	leftPath, leftLocal := localClonePath(left)
	rightPath, rightLocal := localClonePath(right)
	if leftLocal || rightLocal {
		if !leftLocal || !rightLocal {
			return false
		}
		leftInfo, leftErr := os.Stat(leftPath)
		rightInfo, rightErr := os.Stat(rightPath)
		if leftErr == nil && rightErr == nil {
			return os.SameFile(leftInfo, rightInfo)
		}
		return filepath.Clean(leftPath) == filepath.Clean(rightPath)
	}
	leftIdentity := CloneURLIdentity(left)
	right = strings.TrimSpace(right)
	return leftIdentity == right || leftIdentity == CloneURLIdentity(right)
}

func localClonePath(raw string) (string, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", false
	}
	if parsed, err := url.Parse(value); err == nil &&
		strings.EqualFold(parsed.Scheme, "file") &&
		(parsed.Host == "" || strings.EqualFold(parsed.Host, "localhost")) {
		path := parsed.Path
		if runtime.GOOS == "windows" && len(path) >= 3 &&
			path[0] == '/' && path[2] == ':' {
			path = path[1:]
		}
		return filepath.FromSlash(path), true
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value), true
	}
	return "", false
}

func mergeRequestHeadRef(platform string, number int) string {
	if strings.EqualFold(strings.TrimSpace(platform), "gitlab") {
		return fmt.Sprintf("refs/merge-requests/%d/head", number)
	}
	return fmt.Sprintf("refs/pull/%d/head", number)
}

// configureMergeRequestTracking points the imported branch at its upstream
// using worktree-scoped config, so `git push`/`git pull` in the worktree
// target the merge request head without affecting the primary checkout.
func configureMergeRequestTracking(
	ctx context.Context,
	root, worktreePath, branch string,
	target mergeRequestRemoteTarget,
) error {
	steps := []struct {
		dir  string
		args []string
	}{
		{root, []string{"config", "extensions.worktreeConfig", "true"}},
		{worktreePath, []string{
			"config", "--worktree",
			"branch." + branch + ".remote", target.trackingRemote,
		}},
		{worktreePath, []string{
			"config", "--worktree",
			"branch." + branch + ".merge", target.trackingMergeRef,
		}},
		{worktreePath, []string{
			"config", "--worktree",
			"branch." + branch + ".pushRemote", target.trackingRemote,
		}},
		{worktreePath, []string{
			"config", "--worktree", "push.default", "upstream",
		}},
	}
	for _, step := range steps {
		if out, err := runLifecycleGit(
			ctx, step.dir, step.args...,
		); err != nil {
			return fmt.Errorf(
				"configure merge request tracking (git %s): %w: %s",
				strings.Join(step.args, " "), err,
				strings.TrimSpace(string(out)),
			)
		}
	}
	return nil
}

func canonicalizeMergeRequestCloneURL(root, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if strings.Contains(value, "://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return "", errors.New("invalid merge request clone URL")
		}
		scheme := strings.ToLower(parsed.Scheme)
		sshUsernameOnly := scheme == "ssh" ||
			scheme == "git+ssh" || scheme == "ssh+git"
		_, hasPassword := parsed.User.Password()
		if hasPassword || parsed.RawQuery != "" || parsed.Fragment != "" ||
			(parsed.User != nil && !sshUsernameOnly) {
			return "", errors.New(
				"merge request clone URL must not contain embedded credentials, query parameters, or fragments",
			)
		}
	}
	if value == "" || strings.Contains(value, "://") ||
		looksLikeSCPRemote(value) || filepath.IsAbs(value) {
		return value, nil
	}
	absolute, err := filepath.Abs(filepath.Join(root, value))
	if err != nil {
		return "", fmt.Errorf("resolve merge request clone URL: %w", err)
	}
	return filepath.Clean(absolute), nil
}

func canonicalizeMergeRequestProjectIdentity(
	root, raw string,
) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "." || value == ".." ||
		strings.HasPrefix(value, "./") ||
		strings.HasPrefix(value, "../") ||
		strings.HasPrefix(value, `.\`) ||
		strings.HasPrefix(value, `..\`) {
		return canonicalizeMergeRequestCloneURL(root, value)
	}
	return value, nil
}

func looksLikeSCPRemote(value string) bool {
	prefix, _, found := strings.Cut(value, ":")
	return found && prefix != "" &&
		!strings.ContainsAny(prefix, `/\`) &&
		!(len(prefix) == 1 &&
			((prefix[0] >= 'a' && prefix[0] <= 'z') ||
				(prefix[0] >= 'A' && prefix[0] <= 'Z')))
}

func resolveMergeRequestOID(ctx context.Context, root, ref string) (string, error) {
	out, err := runLifecycleGit(ctx, root, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", fmt.Errorf(
			"resolve %s: %w: %s", ref, err, strings.TrimSpace(string(out)),
		)
	}
	return strings.TrimSpace(string(out)), nil
}

func classifyChangeRequestFetchError(out []byte, err error) error {
	detail := strings.TrimSpace(string(out))
	lower := strings.ToLower(detail)
	kind := ChangeRequestInaccessibleHead
	switch {
	case strings.Contains(lower, "authentication failed"),
		strings.Contains(lower, "permission denied"),
		strings.Contains(lower, "could not read username"),
		strings.Contains(lower, "repository not found"):
		kind = ChangeRequestAuthentication
	case strings.Contains(lower, "could not resolve host"),
		strings.Contains(lower, "failed to connect"),
		strings.Contains(lower, "connection timed out"),
		strings.Contains(lower, "network is unreachable"):
		kind = ChangeRequestNetwork
	}
	message := "fetch merge request head"
	if detail != "" {
		message += ": " + detail
	}
	return &ChangeRequestError{Kind: kind, Message: message, Cause: err}
}

// ensureFetchRemote returns the name of a remote pointing at cloneURL,
// adding one when absent. Name collisions with other URLs fall back to
// numbered suffixes.
func ensureFetchRemote(
	ctx context.Context, root, preferredName, cloneURL string,
) (string, error) {
	const maxAttempts = 100
	out, err := runLifecycleGit(ctx, root, "remote")
	if err != nil {
		return "", fmt.Errorf(
			"list configured remotes: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	remoteNames := make(map[string]struct{})
	for name := range strings.SplitSeq(string(out), "\n") {
		if name = strings.TrimSpace(name); name != "" {
			remoteNames[name] = struct{}{}
		}
	}
	candidate := preferredName
	for attempt := 2; attempt < maxAttempts+2; attempt++ {
		if _, exists := remoteNames[candidate]; !exists {
			// The remote does not exist yet; claim the name.
			if out, addErr := runLifecycleGit(
				ctx, root, "remote", "add", candidate, cloneURL,
			); addErr != nil {
				return "", fmt.Errorf(
					"add remote %s: %w: %s", candidate, addErr,
					strings.TrimSpace(string(out)),
				)
			}
			return candidate, nil
		}
		existing, err := runLifecycleGit(
			ctx, root, "config", "--get",
			"remote."+candidate+".url",
		)
		if err != nil {
			if gitcmd.IsExitCode(err, 1) {
				candidate = fmt.Sprintf("%s-%d", preferredName, attempt)
				continue
			}
			return "", fmt.Errorf(
				"inspect remote %s URL: %w: %s", candidate, err,
				strings.TrimSpace(string(existing)),
			)
		}
		existingURL := strings.TrimSpace(string(existing))
		if existingURL == cloneURL ||
			CloneURLIdentity(existingURL) == CloneURLIdentity(cloneURL) {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", preferredName, attempt)
	}
	return "", fmt.Errorf(
		"no unique remote name for %s after %d attempts",
		cloneURL, maxAttempts,
	)
}

var remoteNameSanitizer = regexp.MustCompile(`[^A-Za-z0-9]+`)

// sanitizeRemoteName folds a repository owner into a git-safe remote name.
func sanitizeRemoteName(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	return strings.Trim(
		remoteNameSanitizer.ReplaceAllString(trimmed, "-"), "-",
	)
}

// CloneURLIdentity normalizes a clone URL to a comparable identity:
// "host/owner/name" for URL and scp-like forms (lowercased host, ".git"
// stripped), or the trimmed input for anything else (such as local paths).
func CloneURLIdentity(rawURL string) string {
	return gitremote.CloneURLIdentity(rawURL)
}

// ownerFromCloneURL extracts the repository owner segment from a clone
// URL identity ("host/owner/name"). Identities without that shape (local
// paths) yield "".
func ownerFromCloneURL(rawURL string) string {
	identity := CloneURLIdentity(rawURL)
	if _, after, ok := strings.Cut(identity, "/"); ok {
		if owner, _, ok := strings.Cut(after, "/"); ok && owner != "" {
			return owner
		}
	}
	return ""
}
