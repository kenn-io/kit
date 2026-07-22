package managedworktree

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitremote "go.kenn.io/kit/git/remote"
)

// MergeRequestWorktreeOptions parameterizes CreateWorktreeFromMergeRequest.
// ProjectRoot, Branch, and Number are required. HeadBranch and
// HeadRepoCloneURL come from the synced merge-request metadata;
// ProjectRepoIdentity is the CloneURLIdentity-normalized identity of the
// project's own repository, used to recognize same-repo merge requests.
type MergeRequestWorktreeOptions struct {
	ProjectRoot           string
	Branch                string
	Path                  string
	BaseDir               string
	SetupScript           string
	WorktreeName          string
	HookEnvironmentPrefix string
	Runner                gitcmd.Runner
	RunGit                GitRunner
	RunHook               HookRunner

	Number           int
	HeadBranch       string
	HeadRepoCloneURL string
	// ExpectedHeadSHA is an independent provenance anchor. A hosted origin
	// requires either this value or ProjectRepoIdentity.
	ExpectedHeadSHA string
	// ProjectRepoIdentity is the normalized hosted identity or exact local
	// clone URL expected for origin.
	ProjectRepoIdentity string
	// Platform is the project's platform kind ("github", "gitlab", ...);
	// it selects the remote ref that carries the merge request head when
	// the head must be fetched by number rather than by branch.
	Platform string
}

// mergeRequestRemoteTarget describes how to materialize a merge request
// head locally: the fetch that resolves the immutable checkout OID, an
// optional second fetch that makes upstream tracking possible, and the
// tracking remote/merge-ref pair.
type mergeRequestRemoteTarget struct {
	checkoutRemote         string
	checkoutSourceRef      string
	checkoutDestinationRef string
	trackingRemote         string
	trackingRepository     RemoteRepository
	trackingSourceRef      string
	trackingDestinationRef string
	trackingMergeRef       string
}

// CreateWorktreeFromMergeRequest materializes a merge request head as a new
// worktree: it fetches the head (from the project's own branch, the fork,
// or the platform pull ref), creates a new local branch on it, configures
// upstream tracking when possible (non-fatally skipping it when the fork
// cannot be fetched), and runs the optional setup hook. Failures after the
// worktree exists roll it back.
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
	if err := validateBranchName(ctx, root, branch); err != nil {
		return CreateWorktreeResult{}, err
	}
	hookScript, err := resolveHookScript(root, opts.SetupScript)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	path, err := resolveWorktreeDestination(
		root, branch, opts.Path, opts.BaseDir,
	)
	if err != nil {
		return CreateWorktreeResult{}, err
	}

	changeRequestGit, err := NewChangeRequestGit(ChangeRequestGitOptions{
		ProjectRoot:            root,
		ProjectIdentity:        repositoryIdentity(opts.ProjectRepoIdentity),
		ProjectCloneURL:        opts.ProjectRepoIdentity,
		ExpectedHeadOID:        opts.ExpectedHeadSHA,
		RemoteNamePrefix:       "mr",
		HookIsolationNamespace: opts.HookEnvironmentPrefix,
		Runner:                 opts.Runner,
		RunGit:                 opts.RunGit,
	})
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	if err := changeRequestGit.Validate(ctx); err != nil {
		return CreateWorktreeResult{}, err
	}
	target, err := prepareMergeRequestRemote(ctx, changeRequestGit, opts)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	checkoutOID, err := changeRequestGit.FetchExpected(
		ctx, target.checkoutRemote, target.checkoutSourceRef,
		target.checkoutDestinationRef, opts.ExpectedHeadSHA,
	)
	if err != nil {
		return CreateWorktreeResult{}, err
	}
	trackingEnabled := target.trackingRemote != "" &&
		target.trackingMergeRef != ""
	if trackingEnabled && target.trackingSourceRef != "" {
		// The tracking fetch is best-effort: a fork that has vanished
		// or is unreachable must not block importing via the pull ref.
		trackingOID, fetchErr := changeRequestGit.Fetch(
			ctx, target.trackingRemote, target.trackingSourceRef,
			target.trackingDestinationRef,
		)
		if fetchErr != nil || !strings.EqualFold(trackingOID, checkoutOID) {
			trackingEnabled = false
		}
	}

	result, err := CreateWorktreeOnDisk(ctx, CreateWorktreeOptions{
		ProjectRoot: root, Path: path, Branch: branch,
		BaseRef: checkoutOID, Runner: opts.Runner,
		RunGit: opts.RunGit, IsolatedCheckout: true, NoTrack: true,
		BeforeCheckout: changeRequestGit.ValidateWorktree,
	})
	if err != nil {
		return result, err
	}

	if trackingEnabled {
		if err := changeRequestGit.ConfigurePush(
			ctx, result, target.trackingRemote,
			target.trackingRepository,
			strings.TrimPrefix(target.trackingMergeRef, "refs/heads/"),
		); err != nil {
			_, cleanupErr := result.Rollback(context.WithoutCancel(ctx))
			return result, errors.Join(err, cleanupErr)
		}
	} else if err := changeRequestGit.ConfigureWorktreeIsolation(ctx, result); err != nil {
		_, cleanupErr := result.Rollback(context.WithoutCancel(ctx))
		return result, errors.Join(err, cleanupErr)
	}

	if hookScript.path != "" {
		if hookErr := runLifecycleHook(
			ctx, hookScript, root, path, branch, opts.WorktreeName,
			opts.HookEnvironmentPrefix,
		); hookErr != nil {
			_, cleanupErr := result.rollbackOwned(
				context.WithoutCancel(ctx), false,
			)
			return result, errors.Join(hookErr, cleanupErr)
		}
		result.HookRan = true
		result.HookScript = hookScript.requested
	}
	return result, nil
}

// prepareMergeRequestRemote decides how to fetch the merge request head.
// Same-repo heads fetch the branch from origin directly with tracking;
// fork heads fetch the checkout from the platform pull ref while tracking
// a dedicated fork remote; heads with no clone URL fetch the pull ref with
// no tracking.
func prepareMergeRequestRemote(
	ctx context.Context, changeRequestGit *ChangeRequestGit,
	opts MergeRequestWorktreeOptions,
) (mergeRequestRemoteTarget, error) {
	headBranch := strings.TrimSpace(opts.HeadBranch)
	hasHeadBranch := headBranch != ""
	if headBranch == "" {
		headBranch = "merge-request"
	}

	cloneURL := strings.TrimSpace(opts.HeadRepoCloneURL)
	sameRepo := cloneURL != "" && opts.ProjectRepoIdentity != "" &&
		strings.EqualFold(
			CloneURLIdentity(cloneURL), opts.ProjectRepoIdentity,
		)
	if sameRepo && hasHeadBranch {
		destination := "refs/remotes/origin/" + headBranch
		return mergeRequestRemoteTarget{
			checkoutRemote: "origin", checkoutSourceRef: "refs/heads/" + headBranch,
			checkoutDestinationRef: destination,
			trackingRemote:         "origin",
			trackingRepository: RemoteRepository{
				Identity: repositoryIdentity(opts.ProjectRepoIdentity), CloneURL: cloneURL,
			},
			trackingMergeRef: "refs/heads/" + headBranch,
		}, nil
	}

	// The platform's merge-request head ref (refs/pull/<n>/head on GitHub,
	// Forgejo, and Gitea; refs/merge-requests/<n>/head on GitLab) carries
	// the head commit regardless of where the head branch lives.
	headRef := mergeRequestHeadRef(opts.Platform, opts.Number)
	localRef := "refs/remotes/origin/" + strings.TrimPrefix(headRef, "refs/")

	if cloneURL == "" || sameRepo {
		return mergeRequestRemoteTarget{
			checkoutRemote: "origin", checkoutSourceRef: headRef,
			checkoutDestinationRef: localRef,
		}, nil
	}

	repository := RemoteRepository{
		Identity: repositoryIdentity(CloneURLIdentity(cloneURL)),
		CloneURL: cloneURL,
	}
	remoteName, err := changeRequestGit.EnsureRemote(ctx, repository)
	if err != nil {
		return mergeRequestRemoteTarget{}, err
	}
	return mergeRequestRemoteTarget{
		checkoutRemote: "origin", checkoutSourceRef: headRef,
		checkoutDestinationRef: localRef,
		trackingRemote:         remoteName, trackingRepository: repository,
		trackingSourceRef:      "refs/heads/" + headBranch,
		trackingDestinationRef: "refs/remotes/" + remoteName + "/" + headBranch,
		trackingMergeRef:       "refs/heads/" + headBranch,
	}, nil
}

func repositoryIdentity(identity string) gitremote.Identity {
	trimmed := strings.TrimSpace(identity)
	if filepath.IsAbs(trimmed) || strings.HasPrefix(trimmed, "file://") {
		return gitremote.Identity{}
	}
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) < 3 {
		return gitremote.Identity{}
	}
	return gitremote.Identity{
		Host: parts[0], Owner: strings.Join(parts[1:len(parts)-1], "/"),
		Name: parts[len(parts)-1],
	}
}

func mergeRequestHeadRef(platform string, number int) string {
	if strings.EqualFold(strings.TrimSpace(platform), "gitlab") {
		return fmt.Sprintf("refs/merge-requests/%d/head", number)
	}
	return fmt.Sprintf("refs/pull/%d/head", number)
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
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return ""
	}
	host := gitremote.RemoteHost(trimmed)
	repositoryPath := gitremote.RemoteRepoPath(trimmed)
	if host != "" && repositoryPath != "" {
		return gitremote.NormalizeHost(host) + "/" + repositoryPath
	}
	return trimmed
}
