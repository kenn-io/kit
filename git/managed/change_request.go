package managedworktree

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitremote "go.kenn.io/kit/git/remote"
	"go.kenn.io/kit/safefileio"
)

// ChangeRequestErrorKind classifies failures from the shared change-request
// Git boundary so applications can map them to their own error contracts.
type ChangeRequestErrorKind string

const (
	ChangeRequestAuthentication      ChangeRequestErrorKind = "authentication"
	ChangeRequestNetwork             ChangeRequestErrorKind = "network"
	ChangeRequestInaccessibleHead    ChangeRequestErrorKind = "inaccessible_head"
	ChangeRequestHeadChanged         ChangeRequestErrorKind = "head_changed"
	ChangeRequestUnsupportedGit      ChangeRequestErrorKind = "unsupported_git"
	ChangeRequestUnsafeConfiguration ChangeRequestErrorKind = "unsafe_configuration"
	ChangeRequestWorktreeCreation    ChangeRequestErrorKind = "worktree_creation"
)

// ChangeRequestError is a credential-safe failure from change-request Git
// preparation. Message never contains a rejected remote URL or config value.
type ChangeRequestError struct {
	Kind    ChangeRequestErrorKind
	Message string
	Cause   error
}

func (e *ChangeRequestError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func (e *ChangeRequestError) Unwrap() error { return e.Cause }

func changeRequestError(kind ChangeRequestErrorKind, message string, cause error) error {
	return &ChangeRequestError{Kind: kind, Message: message, Cause: cause}
}

// RemoteRepository identifies a change request's source repository.
type RemoteRepository struct {
	Identity gitremote.Identity
	CloneURL string
}

// ChangeRequestGitOptions configures the reusable Git trust boundary used to
// fetch and materialize contributor-controlled change-request heads.
type ChangeRequestGitOptions struct {
	ProjectRoot     string
	ProjectIdentity gitremote.Identity
	// ProjectCloneURL is the expected local/file project remote when
	// ProjectIdentity is hostless.
	ProjectCloneURL string
	// ExpectedHeadOID is an independent provenance anchor that permits a
	// hosted project remote when ProjectIdentity is unavailable. Every Fetch
	// through this boundary enforces the configured OID.
	ExpectedHeadOID        string
	RemoteNamePrefix       string
	HookIsolationNamespace string
	Runner                 gitcmd.Runner
	RunGit                 GitRunner
}

// ChangeRequestGit owns provider-neutral remote validation, isolated fetch,
// worktree config validation, and persistent safe push routing.
type ChangeRequestGit struct {
	root             string
	project          RemoteRepository
	expectedHeadOID  string
	remoteNamePrefix string
	hookNamespace    string
	runner           gitcmd.Runner
	runGit           GitRunner

	expectedRemoteURLsMu sync.RWMutex
	expectedRemoteURLs   map[string]remoteURLSnapshot
}

type remoteURLSnapshot struct {
	fetch string
	push  string
}

// NewChangeRequestGit constructs a shared change-request Git boundary.
func NewChangeRequestGit(opts ChangeRequestGitOptions) (*ChangeRequestGit, error) {
	root, err := absRequired(opts.ProjectRoot, "project root")
	if err != nil {
		return nil, err
	}
	projectCloneURL, _, err := canonicalCloneURL(root, opts.ProjectCloneURL)
	if err != nil {
		return nil, fmt.Errorf("resolve project clone URL: %w", err)
	}
	runner := opts.Runner
	runner = normalizeLifecycleRunner(runner, gitcmd.New())
	// Change-request operations are an automation trust boundary. Callers may
	// share an interactive runner elsewhere, but validation, fetch, and
	// configuration here must never prompt or weaken process-tree cancellation.
	runner.TerminalPrompt = false
	prefix := sanitizeRemoteName(opts.RemoteNamePrefix)
	if prefix == "" {
		prefix = "review"
	}
	namespace := sanitizeRemoteName(opts.HookIsolationNamespace)
	if namespace == "" {
		namespace = "kit"
	}
	return &ChangeRequestGit{
		root: root,
		project: RemoteRepository{
			Identity: opts.ProjectIdentity, CloneURL: projectCloneURL,
		},
		expectedHeadOID:  strings.TrimSpace(opts.ExpectedHeadOID),
		remoteNamePrefix: prefix, hookNamespace: namespace,
		runner: runner, runGit: opts.RunGit,
		expectedRemoteURLs: make(map[string]remoteURLSnapshot),
	}, nil
}

// Validate verifies the Git version and effective project configuration before
// any remote or worktree mutation occurs.
func (g *ChangeRequestGit) Validate(ctx context.Context) error {
	output, err := g.run(ctx, g.root, "version")
	if err != nil {
		return changeRequestError(ChangeRequestUnsupportedGit, "failed to determine Git version", err)
	}
	if !supportsChangeRequestGitVersion(string(output), runtime.GOOS) {
		return changeRequestError(ChangeRequestUnsupportedGit,
			"change-request import requires "+safeCheckoutGitVersionRequirement(runtime.GOOS), nil)
	}
	if err := g.validateConfigurationAt(ctx, ""); err != nil {
		return err
	}
	if strings.TrimSpace(g.project.Identity.Host) == "" && g.expectedHeadOID != "" {
		remoteURL, err := g.configuredFetchRemoteURL(ctx, "origin")
		if err != nil {
			return err
		}
		g.rememberExpectedRemoteURL("origin", false, remoteURL)
		return nil
	}
	if strings.TrimSpace(g.project.Identity.Host) == "" &&
		strings.TrimSpace(g.project.CloneURL) == "" {
		for _, push := range []bool{false, true} {
			if err := g.validateLocalProjectRemote(ctx, "origin", push); err != nil {
				return err
			}
		}
		return nil
	}
	for _, push := range []bool{false, true} {
		if err := g.validateProjectRemote(ctx, "origin", push); err != nil {
			return err
		}
	}
	return nil
}

func (g *ChangeRequestGit) validateProjectRemote(
	ctx context.Context, remote string, push bool,
) error {
	args := []string{"remote", "get-url", "--all"}
	label := "fetch"
	if push {
		args = append(args, "--push")
		label = "push"
	}
	args = append(args, remote)
	output, err := g.runSafe(ctx, g.root, args...)
	if err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to inspect the project Git remote", err)
	}
	if remoteURLListUnsafe(string(output)) {
		return changeRequestError(ChangeRequestAuthentication,
			"project Git remote contains credentials or command syntax", nil)
	}
	remoteURL, single := singleRemoteURL(string(output))
	if !single {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			fmt.Sprintf("project Git remote has an unexpected effective %s destination", label), nil)
	}
	canonicalRemoteURL, _, err := canonicalCloneURL(g.root, remoteURL)
	if err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to resolve the project Git remote", err)
	}
	if !remoteMatchesRepository(canonicalRemoteURL, g.project) {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			fmt.Sprintf("project Git remote has an unexpected effective %s destination", label), nil)
	}
	g.rememberExpectedRemoteURL(remote, push, canonicalRemoteURL)
	return nil
}

func (g *ChangeRequestGit) validateLocalProjectRemote(
	ctx context.Context, remote string, push bool,
) error {
	args := []string{"remote", "get-url", "--all"}
	if push {
		args = append(args, "--push")
	}
	args = append(args, remote)
	output, err := g.runSafe(ctx, g.root, args...)
	if err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to inspect the unidentified project Git remote", err)
	}
	remoteURL, single := singleRemoteURL(string(output))
	if !single || gitremote.UnsafeForAutomation(remoteURL) ||
		gitremote.RemoteHost(remoteURL) != "" || gitremote.RemoteRepoPath(remoteURL) != "" {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"hosted project Git remote requires a project identity or expected head OID", nil)
	}
	canonicalURL, _, err := canonicalCloneURL(g.root, remoteURL)
	if err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to resolve the local project Git remote", err)
	}
	g.rememberExpectedRemoteURL(remote, push, canonicalURL)
	return nil
}

// ValidateWorktree verifies that branch/worktree conditional configuration did
// not change the trust boundary after the isolated checkout selected a branch.
func (g *ChangeRequestGit) ValidateWorktree(ctx context.Context, worktreePath string) error {
	if err := g.validateConfigurationAt(ctx, worktreePath); err != nil {
		return err
	}
	mainConfig, err := g.effectiveConfigRecords(ctx, "")
	if err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration, "failed to inspect main Git configuration", err)
	}
	worktreeConfig, err := g.effectiveConfigRecords(ctx, worktreePath)
	if err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration, "failed to inspect change-request worktree Git configuration", err)
	}
	if !slices.Equal(mainConfig, worktreeConfig) {
		return changeRequestError(ChangeRequestAuthentication,
			"change-request worktree activates different Git configuration; remove branch- or worktree-conditional includes", nil)
	}
	return nil
}

func (g *ChangeRequestGit) validateConfigurationAt(ctx context.Context, worktreePath string) error {
	configOutput, err := g.runAt(ctx, worktreePath, "config", "--includes", "--null", "--list")
	if err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration, "failed to inspect effective Git configuration", err)
	}
	if configHasUnsafeRemote(string(configOutput)) {
		return changeRequestError(ChangeRequestAuthentication,
			"change-request import requires credential-free Git remote URLs; use a credential helper or SSH agent", nil)
	}
	if configHasExecutableTransportOverride(string(configOutput)) {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"change-request import does not allow custom Git transport commands", nil)
	}
	if configHasUnsafeHTTPConfiguration(string(configOutput)) {
		return changeRequestError(ChangeRequestAuthentication,
			"change-request import does not allow security-sensitive HTTP configuration", nil)
	}
	remotes, err := g.runAt(ctx, worktreePath, "remote")
	if err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration, "failed to enumerate effective Git remotes", err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(remotes)), "\n") {
		remote := strings.TrimSpace(line)
		if remote == "" {
			continue
		}
		for _, args := range [][]string{
			{"remote", "get-url", "--all", remote},
			{"remote", "get-url", "--all", "--push", remote},
		} {
			urls, urlErr := g.runAt(ctx, worktreePath, args...)
			if urlErr != nil {
				return changeRequestError(ChangeRequestUnsafeConfiguration, "failed to inspect effective Git remote URLs", urlErr)
			}
			if remoteURLListUnsafe(string(urls)) {
				return changeRequestError(ChangeRequestAuthentication,
					"change-request import requires credential-free Git remote URLs; use a credential helper or SSH agent", nil)
			}
		}
	}
	return nil
}

func (g *ChangeRequestGit) effectiveConfigRecords(ctx context.Context, worktreePath string) ([]string, error) {
	output, err := g.runAt(ctx, worktreePath, "config", "--includes", "--null", "--list")
	if err != nil {
		return nil, err
	}
	records := make([]string, 0)
	for record := range strings.SplitSeq(string(output), "\x00") {
		key, value, _ := strings.Cut(record, "\n")
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		if record == "" || key == "core.hookspath" ||
			key == "core.fsmonitor" && strings.EqualFold(value, "false") ||
			isPackageIsolationFilterSetting(key, value) {
			continue
		}
		records = append(records, record)
	}
	return records, nil
}

func isPackageIsolationFilterSetting(key, value string) bool {
	if !strings.HasPrefix(key, "filter.") {
		return false
	}
	switch {
	case strings.HasSuffix(key, ".clean"),
		strings.HasSuffix(key, ".smudge"),
		strings.HasSuffix(key, ".process"):
		return value == ""
	case strings.HasSuffix(key, ".required"):
		return strings.EqualFold(value, "false")
	default:
		return false
	}
}

// EnsureRemote returns a single-destination, credential-free remote for the
// source repository, adding a deterministic remote when no safe match exists.
func (g *ChangeRequestGit) EnsureRemote(ctx context.Context, repository RemoteRepository) (string, error) {
	ctx, unlock, err := acquireRepositoryMutationLock(ctx, g.root)
	if err != nil {
		return "", changeRequestError(ChangeRequestUnsafeConfiguration, "failed to lock the Git repository", err)
	}
	remote, ensureErr := g.ensureRemote(ctx, repository)
	return remote, errors.Join(ensureErr, unlock())
}

func (g *ChangeRequestGit) ensureRemote(ctx context.Context, repository RemoteRepository) (string, error) {
	if gitremote.UnsafeForAutomation(strings.TrimSpace(repository.CloneURL)) {
		return "", changeRequestError(ChangeRequestAuthentication,
			"change-request head repository URL contains credentials or command syntax", nil)
	}
	output, err := g.runSafe(ctx, g.root, "remote")
	if err != nil {
		return "", changeRequestError(ChangeRequestUnsafeConfiguration, "failed to list Git remotes", err)
	}
	existing := make(map[string]bool)
	remoteNames := make([]string, 0)
	for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
		name := strings.TrimSpace(line)
		if name != "" {
			existing[name] = true
			remoteNames = append(remoteNames, name)
		}
	}
	remoteURL, _, err := canonicalCloneURL(g.root, repository.CloneURL)
	if err != nil {
		return "", changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to resolve the change-request clone URL", err)
	}
	repository.CloneURL = remoteURL
	projectSSHURL := g.projectPushSSHURL(ctx, existing)
	if projectSSHURL != "" {
		remoteURL, err = forkSSHURL(projectSSHURL, repository.Identity)
		if err != nil {
			return "", changeRequestError(ChangeRequestUnsafeConfiguration,
				"failed to preserve the project's SSH transport for the change-request remote", err)
		}
		if !remoteMatchesRepository(remoteURL, repository) {
			return "", changeRequestError(ChangeRequestUnsafeConfiguration,
				"derived change-request SSH remote does not match the source repository", nil)
		}
	}
	if remoteURL == "" {
		return "", changeRequestError(ChangeRequestInaccessibleHead,
			"change-request head repository has no clone URL", nil)
	}
	if gitremote.UnsafeForAutomation(remoteURL) {
		return "", changeRequestError(ChangeRequestAuthentication,
			"change-request head repository URL contains credentials or command syntax", nil)
	}
	if !remoteMatchesRepository(remoteURL, repository) {
		return "", changeRequestError(ChangeRequestUnsafeConfiguration,
			"change-request clone URL does not match the source repository identity", nil)
	}
	for _, name := range remoteNames {
		fetchURLs, fetchErr := g.runSafe(ctx, g.root, "remote", "get-url", "--all", name)
		pushURLs, pushErr := g.runSafe(ctx, g.root, "remote", "get-url", "--all", "--push", name)
		if fetchErr != nil || pushErr != nil || remoteURLListUnsafe(string(fetchURLs)) || remoteURLListUnsafe(string(pushURLs)) {
			continue
		}
		fetchURL, singleFetch := singleRemoteURL(string(fetchURLs))
		pushURL, singlePush := singleRemoteURL(string(pushURLs))
		fetchMatches := singleFetch && remoteMatchesRepository(fetchURL, repository)
		pushMatches := singlePush && remoteMatchesRepository(pushURL, repository)
		if projectSSHURL != "" {
			fetchMatches = singleFetch && remoteURLsEqual(fetchURL, remoteURL)
			pushMatches = singlePush && remoteURLsEqual(pushURL, remoteURL)
		}
		if fetchMatches && pushMatches && !g.remoteHasCustomPushRefspec(ctx, name) {
			canonicalFetchURL, _, canonicalErr := canonicalCloneURL(g.root, fetchURL)
			if canonicalErr != nil {
				continue
			}
			canonicalPushURL, _, canonicalErr := canonicalCloneURL(g.root, pushURL)
			if canonicalErr != nil {
				continue
			}
			g.rememberExpectedRemoteURLs(name, canonicalFetchURL, canonicalPushURL)
			return name, nil
		}
	}
	base := g.remoteNamePrefix + "-" + sanitizeRemoteName(repository.Identity.Owner)
	if strings.HasSuffix(base, "-") {
		base += "head"
	}
	name := base
	for suffix := 2; existing[name]; suffix++ {
		name = fmt.Sprintf("%s-%d", base, suffix)
	}
	if _, err := g.runSafe(ctx, g.root, "remote", "add", name, remoteURL); err != nil {
		return "", changeRequestError(ChangeRequestUnsafeConfiguration, "failed to add change-request Git remote", err)
	}
	g.rememberExpectedRemoteURLs(name, remoteURL, remoteURL)
	if validateErr := g.validateEffectiveRemote(ctx, "", name, repository); validateErr != nil {
		g.forgetExpectedRemoteURL(name)
		_, removeErr := g.runSafe(context.WithoutCancel(ctx), g.root, "remote", "remove", name)
		if removeErr != nil {
			return "", changeRequestError(ChangeRequestUnsafeConfiguration,
				"new change-request Git remote was unsafe and could not be removed", errors.Join(validateErr, removeErr))
		}
		return "", validateErr
	}
	return name, nil
}

// Fetch imports sourceRef into destinationRef with hooks and prompts disabled,
// enforces the configured expected head OID, and returns the resolved commit
// OID.
func (g *ChangeRequestGit) Fetch(ctx context.Context, remote, sourceRef, destinationRef string) (string, error) {
	return g.fetchAndPublish(ctx, remote, sourceRef, destinationRef, "")
}

func (g *ChangeRequestGit) fetchAndPublish(
	ctx context.Context, remote, sourceRef, destinationRef, expectedOID string,
) (string, error) {
	ctx, unlock, err := acquireRepositoryMutationLock(ctx, g.root)
	if err != nil {
		return "", changeRequestError(ChangeRequestUnsafeConfiguration, "failed to lock the Git repository", err)
	}
	if validationErr := g.validateFetchPublication(ctx, remote, sourceRef, destinationRef); validationErr != nil {
		return "", errors.Join(validationErr, unlock())
	}
	temporaryRef, err := newTemporaryFetchRef()
	if err != nil {
		return "", errors.Join(changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to allocate a private fetch ref", err), unlock())
	}
	oid, fetchErr := g.fetchHead(ctx, remote, sourceRef, temporaryRef)
	if fetchErr == nil {
		fetchErr = verifyExpectedHeadOID(oid, g.expectedHeadOID)
	}
	if fetchErr == nil {
		fetchErr = verifyExpectedHeadOID(oid, expectedOID)
	}
	if fetchErr == nil {
		if _, updateErr := g.runSafe(ctx, g.root, "update-ref", "--no-deref", destinationRef, oid); updateErr != nil {
			fetchErr = changeRequestError(ChangeRequestUnsafeConfiguration,
				"failed to publish the verified change-request head", updateErr)
		}
	}
	cleanupErr := g.deleteTemporaryFetchRef(context.WithoutCancel(ctx), temporaryRef, oid)
	return oid, errors.Join(fetchErr, cleanupErr, unlock())
}

func (g *ChangeRequestGit) fetchHead(
	ctx context.Context, remote, sourceRef, temporaryRef string,
) (string, error) {
	refspec := "+" + sourceRef + ":" + temporaryRef
	if _, err := g.runSafe(ctx, g.root, "fetch", "--no-tags", "--", remote, refspec); err != nil {
		message := strings.ToLower(err.Error())
		switch {
		case isGitAuthenticationFailure(message):
			return "", changeRequestError(ChangeRequestAuthentication,
				"Git authentication failed while fetching the change-request head", err)
		case isGitNetworkFailure(message):
			return "", changeRequestError(ChangeRequestNetwork,
				"network failure while fetching the change-request head", err)
		default:
			return "", changeRequestError(ChangeRequestInaccessibleHead,
				"change-request head ref is unavailable", err)
		}
	}
	sha, err := g.runSafe(ctx, g.root, "rev-parse", "--verify", temporaryRef+"^{commit}")
	if err != nil {
		return "", changeRequestError(ChangeRequestInaccessibleHead,
			"fetched change-request head is not a commit", err)
	}
	return strings.TrimSpace(string(sha)), nil
}

func newTemporaryFetchRef() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "refs/kit/tmp/fetch-" + hex.EncodeToString(random[:]), nil
}

func (g *ChangeRequestGit) deleteTemporaryFetchRef(
	ctx context.Context, temporaryRef, expectedOID string,
) error {
	args := []string{"update-ref", "--no-deref", "-d", temporaryRef}
	if strings.TrimSpace(expectedOID) != "" {
		args = append(args, expectedOID)
	}
	if _, err := g.runSafe(ctx, g.root, args...); err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to remove the private fetch ref", err)
	}
	return nil
}

func (g *ChangeRequestGit) validateFetchPublication(
	ctx context.Context, remote, sourceRef, destinationRef string,
) error {
	if remote == "" || remote != strings.TrimSpace(remote) ||
		strings.HasPrefix(remote, "-") || strings.ContainsAny(remote, " \t\r\n\x00") {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"change-request fetch remote name is unsafe", nil)
	}
	if sourceRef == "" || sourceRef != strings.TrimSpace(sourceRef) ||
		strings.HasPrefix(sourceRef, "-") || strings.ContainsAny(sourceRef, "\r\n\x00") {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"change-request source ref is unsafe", nil)
	}
	if _, err := g.runSafe(ctx, g.root, "check-ref-format", sourceRef); err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"change-request source ref is invalid", err)
	}
	if _, err := g.runSafe(ctx, g.root, "check-ref-format", destinationRef); err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"change-request destination ref is invalid", err)
	}
	if !isManagedMergeRequestRef(destinationRef) {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"change-request destination is outside the managed ref namespace", nil)
	}
	refs, err := g.runSafe(ctx, g.root, "for-each-ref", "--format=%(refname)%00%(symref)")
	if err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to inspect change-request destination refs", err)
	}
	for record := range strings.SplitSeq(strings.TrimSpace(string(refs)), "\n") {
		refName, symbolicTarget, _ := strings.Cut(record, "\x00")
		if !strings.EqualFold(refName, destinationRef) {
			continue
		}
		if refName != destinationRef {
			return changeRequestError(ChangeRequestUnsafeConfiguration,
				"change-request destination aliases an existing ref", nil)
		}
		if strings.TrimSpace(symbolicTarget) != "" {
			return changeRequestError(ChangeRequestUnsafeConfiguration,
				"change-request destination is symbolic", nil)
		}
	}
	expectedRemoteURL, ok := g.expectedRemoteURL(remote, false)
	if !ok {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"change-request fetch remote was not previously validated", nil)
	}
	effectiveRemoteURL, err := g.configuredFetchRemoteURL(ctx, remote)
	if err != nil {
		return err
	}
	if !remoteURLsEqual(effectiveRemoteURL, expectedRemoteURL) {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"change-request fetch remote changed after validation", nil)
	}
	return nil
}

func isManagedMergeRequestRef(ref string) bool {
	rest, ok := strings.CutPrefix(ref, "refs/kit/merge-requests/")
	if !ok {
		return false
	}
	number, leaf, ok := strings.Cut(rest, "/")
	if !ok || strings.Contains(leaf, "/") || leaf != "head" && leaf != "tracking" {
		return false
	}
	parsed, err := strconv.Atoi(number)
	return err == nil && parsed > 0
}

func (g *ChangeRequestGit) configuredFetchRemoteURL(
	ctx context.Context, remote string,
) (string, error) {
	if err := g.validateConfigurationAt(ctx, ""); err != nil {
		return "", err
	}
	output, err := g.runSafe(ctx, g.root, "remote", "get-url", "--all", remote)
	if err != nil {
		return "", changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to inspect the validated change-request remote", err)
	}
	if remoteURLListUnsafe(string(output)) {
		return "", changeRequestError(ChangeRequestAuthentication,
			"change-request fetch remote contains credentials or command syntax", nil)
	}
	remoteURL, single := singleRemoteURL(string(output))
	if !single {
		return "", changeRequestError(ChangeRequestUnsafeConfiguration,
			"change-request fetch remote has multiple destinations", nil)
	}
	canonicalURL, _, err := canonicalCloneURL(g.root, remoteURL)
	if err != nil {
		return "", changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to resolve the validated change-request remote", err)
	}
	return canonicalURL, nil
}

// FetchExpected fetches a change-request ref and rejects it when the resolved
// commit differs from the provider metadata the caller selected. An empty
// expected OID retains Fetch behavior for providers without that metadata.
func (g *ChangeRequestGit) FetchExpected(
	ctx context.Context, remote, sourceRef, destinationRef, expectedOID string,
) (string, error) {
	return g.fetchAndPublish(ctx, remote, sourceRef, destinationRef, expectedOID)
}

func verifyExpectedHeadOID(actual, expected string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" || strings.EqualFold(strings.TrimSpace(actual), expected) {
		return nil
	}
	return changeRequestError(ChangeRequestHeadChanged,
		"change-request head changed while it was being imported; retry", nil)
}

// ConfigurePush persists worktree-scoped routing to the contributor's source
// branch and installs a non-contributor-controlled empty hooks directory. If
// routing setup fails, it restores the affected worktree configuration while
// retaining the safe hook isolation.
func (g *ChangeRequestGit) ConfigurePush(
	ctx context.Context,
	created CreateWorktreeResult,
	remote string,
	repository RemoteRepository,
	sourceBranch string,
) error {
	ctx, unlock, err := acquireRepositoryMutationLock(ctx, g.root)
	if err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration, "failed to lock the Git repository", err)
	}
	return errors.Join(g.configurePush(ctx, created, remote, repository, sourceBranch), unlock())
}

func (g *ChangeRequestGit) configurePush(
	ctx context.Context,
	created CreateWorktreeResult,
	remote string,
	repository RemoteRepository,
	sourceBranch string,
) error {
	if err := g.validateEffectiveRemote(ctx, created.Path, remote, repository); err != nil {
		return err
	}
	hooksPath, err := g.configureWorktreeIsolation(ctx, created)
	if err != nil {
		return err
	}
	commands := [][]string{
		{"config", "--worktree", "branch." + created.Branch + ".remote", remote},
		{"config", "--worktree", "branch." + created.Branch + ".pushRemote", remote},
		{"config", "--worktree", "branch." + created.Branch + ".merge", "refs/heads/" + sourceBranch},
		{"config", "--worktree", "remote." + remote + ".push", "HEAD:refs/heads/" + sourceBranch},
		{"config", "--worktree", "remote." + remote + ".mirror", "false"},
		{"config", "--worktree", "push.default", "upstream"},
		{"config", "--worktree", "push.followTags", "false"},
	}
	keys := make([]string, 0, len(commands))
	for _, args := range commands {
		keys = append(keys, args[2])
	}
	snapshot, err := g.snapshotWorktreeConfig(ctx, created.Path, keys)
	if err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to snapshot change-request push configuration", err)
	}
	if err := g.validateCreatedWorktreeOwnership(ctx, created); err != nil {
		return err
	}
	var configureErr error
	for _, args := range commands {
		if _, err := g.runSafe(ctx, created.Path, args...); err != nil {
			configureErr = err
			break
		}
	}
	if configureErr == nil {
		configureErr = g.validatePushRouting(
			ctx, created, remote, repository, sourceBranch, hooksPath,
		)
	}
	if configureErr == nil {
		return nil
	}
	restoreCtx := context.WithoutCancel(ctx)
	if ownershipErr := g.validateCreatedWorktreeOwnership(restoreCtx, created); ownershipErr != nil {
		return errors.Join(configureErr, fmt.Errorf(
			"%w: change-request push configuration was not restored after worktree ownership changed: %w",
			ErrWorktreeCleanupIncomplete, ownershipErr,
		))
	}
	restoreErr := g.restoreWorktreeConfig(restoreCtx, created.Path, keys, snapshot)
	if restoreErr != nil {
		restoreErr = changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to restore change-request push configuration", restoreErr)
	}
	return errors.Join(configureErr, restoreErr)
}

func (g *ChangeRequestGit) snapshotWorktreeConfig(
	ctx context.Context, worktreePath string, keys []string,
) (map[string][]string, error) {
	output, err := g.runSafe(ctx, worktreePath, "config", "--worktree", "--null", "--list")
	if err != nil {
		return nil, err
	}
	snapshot := make(map[string][]string, len(keys))
	for record := range strings.SplitSeq(string(output), "\x00") {
		key, value, found := strings.Cut(record, "\n")
		if !found {
			continue
		}
		for _, wanted := range keys {
			if strings.EqualFold(strings.TrimSpace(key), wanted) {
				snapshot[wanted] = append(snapshot[wanted], value)
				break
			}
		}
	}
	return snapshot, nil
}

func (g *ChangeRequestGit) restoreWorktreeConfig(
	ctx context.Context, worktreePath string, keys []string, snapshot map[string][]string,
) error {
	var errs []error
	for index := len(keys) - 1; index >= 0; index-- {
		key := keys[index]
		if _, err := g.runSafe(
			ctx, worktreePath, "config", "--worktree", "--unset-all", key,
		); err != nil && !gitcmd.IsExitCode(err, 5) {
			errs = append(errs, fmt.Errorf("clear %s: %w", key, err))
			continue
		}
		for _, value := range snapshot[key] {
			if _, err := g.runSafe(
				ctx, worktreePath, "config", "--worktree", "--add", key, value,
			); err != nil {
				errs = append(errs, fmt.Errorf("restore %s: %w", key, err))
				break
			}
		}
	}
	return errors.Join(errs...)
}

// ConfigureWorktreeIsolation persists a non-contributor-controlled empty
// hooks directory for a newly created change-request worktree, even when no
// safe upstream is available for push routing.
func (g *ChangeRequestGit) ConfigureWorktreeIsolation(
	ctx context.Context, created CreateWorktreeResult,
) error {
	ctx, unlock, err := acquireRepositoryMutationLock(ctx, g.root)
	if err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration, "failed to lock the Git repository", err)
	}
	_, configureErr := g.configureWorktreeIsolation(ctx, created)
	return errors.Join(configureErr, unlock())
}

func (g *ChangeRequestGit) configureWorktreeIsolation(
	ctx context.Context, created CreateWorktreeResult,
) (string, error) {
	if err := g.validateCreatedWorktreeOwnership(ctx, created); err != nil {
		return "", err
	}
	if err := g.validateWorkspaceHead(ctx, created); err != nil {
		return "", err
	}
	if err := g.ValidateWorktree(ctx, created.Path); err != nil {
		return "", err
	}
	if _, err := g.runSafe(ctx, g.root, "config", "extensions.worktreeConfig", "true"); err != nil {
		return "", changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to enable worktree-scoped Git configuration", err)
	}
	return g.configurePersistentWorktreeIsolation(ctx, created.Path)
}

func (g *ChangeRequestGit) validateCreatedWorktreeOwnership(
	ctx context.Context, created CreateWorktreeResult,
) error {
	if !created.snapshotted || created.pathInfo == nil ||
		!lifecyclePathsEqual(g.root, created.projectRoot) {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"change-request worktree lacks a matching creation snapshot", nil)
	}
	ctx = withLifecycleExecution(ctx, g.runner, g.runGit, nil)
	if err := verifyRegisteredWorktree(
		ctx, g.root, created.Path, created.Branch, created.pathInfo,
	); err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"change-request worktree ownership changed before configuration", err)
	}
	return nil
}

func (g *ChangeRequestGit) validateWorkspaceHead(ctx context.Context, created CreateWorktreeResult) error {
	branch, err := g.runSafe(ctx, created.Path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || strings.TrimSpace(string(branch)) != created.Branch {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			fmt.Sprintf("change-request worktree is no longer on generated branch %q", created.Branch), err)
	}
	return nil
}

func (g *ChangeRequestGit) configurePersistentWorktreeIsolation(
	ctx context.Context, worktreePath string,
) (string, error) {
	configOutput, err := g.runSafe(
		ctx, worktreePath, "config", "--includes", "--null", "--list",
	)
	if err != nil {
		return "", changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to inspect Git filters for persistent isolation", err)
	}
	filterDrivers := lifecycleFilterDrivers(string(configOutput))
	output, err := g.runSafe(ctx, worktreePath, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to locate Git metadata for hook isolation", err)
	}
	commonDir := strings.TrimSpace(string(output))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(worktreePath, commonDir)
	}
	commonDir, err = filepath.Abs(commonDir)
	if err != nil {
		return "", changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to resolve Git metadata for hook isolation", err)
	}
	hooksPath := filepath.Join(filepath.Clean(commonDir), g.hookNamespace, "disabled-hooks")
	for _, directory := range []string{filepath.Dir(hooksPath), hooksPath} {
		if err := safefileio.EnsurePrivateDir(directory); err != nil {
			return "", changeRequestError(ChangeRequestUnsafeConfiguration,
				"failed to prepare persistent hook isolation", err)
		}
	}
	entries, err := os.ReadDir(hooksPath)
	if err != nil {
		return "", changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to inspect persistent hook isolation", err)
	}
	if len(entries) != 0 {
		return "", changeRequestError(ChangeRequestUnsafeConfiguration,
			"persistent hook isolation directory is not empty", nil)
	}
	if _, err := g.runSafe(ctx, worktreePath, "config", "--worktree", "core.hooksPath", hooksPath); err != nil {
		return "", changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to persist hook isolation", err)
	}
	commands := [][]string{
		{"config", "--worktree", "core.fsmonitor", "false"},
	}
	for _, driver := range filterDrivers {
		prefix := "filter." + driver + "."
		commands = append(commands,
			[]string{"config", "--worktree", prefix + "clean", ""},
			[]string{"config", "--worktree", prefix + "smudge", ""},
			[]string{"config", "--worktree", prefix + "process", ""},
			[]string{"config", "--worktree", prefix + "required", "false"},
		)
	}
	for _, args := range commands {
		if _, err := g.runSafe(ctx, worktreePath, args...); err != nil {
			return "", changeRequestError(ChangeRequestUnsafeConfiguration,
				"failed to persist filter and fsmonitor isolation", err)
		}
	}
	if err := g.validatePersistentWorktreeIsolation(
		ctx, worktreePath, hooksPath, filterDrivers,
	); err != nil {
		return "", err
	}
	return hooksPath, nil
}

func (g *ChangeRequestGit) validatePersistentWorktreeIsolation(
	ctx context.Context, worktreePath, expectedHooksPath string, filterDrivers []string,
) error {
	expected := map[string]string{
		"core.hooksPath": expectedHooksPath,
		"core.fsmonitor": "false",
	}
	for _, driver := range filterDrivers {
		prefix := "filter." + driver + "."
		expected[prefix+"clean"] = ""
		expected[prefix+"smudge"] = ""
		expected[prefix+"process"] = ""
		expected[prefix+"required"] = "false"
	}
	for key, want := range expected {
		output, err := g.run(
			ctx, worktreePath, "config", "--includes", "--get", key,
		)
		got := strings.TrimSpace(string(output))
		matches := got == want
		if key == "core.hooksPath" {
			matches = filepath.Clean(got) == filepath.Clean(want)
		}
		if err != nil || !matches {
			return changeRequestError(ChangeRequestUnsafeConfiguration,
				"change-request worktree does not retain persistent Git execution isolation", err)
		}
	}
	return nil
}

func (g *ChangeRequestGit) validatePushRouting(
	ctx context.Context,
	created CreateWorktreeResult,
	remote string,
	repository RemoteRepository,
	sourceBranch, expectedHooksPath string,
) error {
	if err := g.validateConfigurationAt(ctx, created.Path); err != nil {
		return err
	}
	if err := g.validateWorkspaceHead(ctx, created); err != nil {
		return err
	}
	if err := g.validateEffectiveRemote(ctx, created.Path, remote, repository); err != nil {
		return err
	}
	configOutput, err := g.runSafe(
		ctx, created.Path, "config", "--includes", "--null", "--list",
	)
	if err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to inspect persistent Git execution isolation", err)
	}
	if err := g.validatePersistentWorktreeIsolation(
		ctx, created.Path, expectedHooksPath,
		lifecycleFilterDrivers(string(configOutput)),
	); err != nil {
		return err
	}
	pushKey := "remote." + remote + ".push"
	pushOutput, err := g.runSafe(ctx, created.Path, "config", "--includes", "--get-all", pushKey)
	if err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to validate change-request push configuration", err)
	}
	pushValue, single := singleRemoteURL(string(pushOutput))
	if !single || pushValue != "HEAD:refs/heads/"+sourceBranch {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			fmt.Sprintf("change-request worktree has unsafe push configuration for %s", pushKey), nil)
	}
	expectedValues := map[string]string{
		"branch." + created.Branch + ".remote":     remote,
		"branch." + created.Branch + ".pushRemote": remote,
		"branch." + created.Branch + ".merge":      "refs/heads/" + sourceBranch,
		"remote." + remote + ".mirror":             "false",
		"push.default":                             "upstream",
		"push.followTags":                          "false",
	}
	for key, expected := range expectedValues {
		output, err := g.runSafe(ctx, created.Path, "config", "--includes", "--get", key)
		if err != nil {
			return changeRequestError(ChangeRequestUnsafeConfiguration,
				"failed to validate change-request push configuration", err)
		}
		if strings.TrimSpace(string(output)) != expected {
			return changeRequestError(ChangeRequestUnsafeConfiguration,
				fmt.Sprintf("change-request worktree has unsafe push configuration for %s", key), nil)
		}
	}
	return nil
}

func (g *ChangeRequestGit) validateEffectiveRemote(
	ctx context.Context, worktreePath, remote string, repository RemoteRepository,
) error {
	if gitremote.UnsafeForAutomation(strings.TrimSpace(repository.CloneURL)) {
		return changeRequestError(ChangeRequestAuthentication,
			"change-request head repository URL contains credentials or command syntax", nil)
	}
	canonicalURL, _, err := canonicalCloneURL(g.root, repository.CloneURL)
	if err != nil {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"failed to resolve the change-request clone URL", err)
	}
	repository.CloneURL = canonicalURL
	for _, test := range []struct {
		label string
		push  bool
		args  []string
	}{
		{label: "fetch", args: []string{"remote", "get-url", "--all", remote}},
		{label: "push", push: true, args: []string{"remote", "get-url", "--all", "--push", remote}},
	} {
		expectedURL, hasExpectedURL := g.expectedRemoteURL(remote, test.push)
		if !hasExpectedURL && strings.TrimSpace(repository.CloneURL) != "" &&
			gitremote.RemoteHost(repository.CloneURL) == "" {
			expectedURL = strings.TrimSpace(repository.CloneURL)
			hasExpectedURL = true
		}
		output, err := g.runAt(ctx, worktreePath, test.args...)
		if err != nil {
			return changeRequestError(ChangeRequestUnsafeConfiguration,
				"failed to validate the change-request Git remote", err)
		}
		if remoteURLListUnsafe(string(output)) {
			return changeRequestError(ChangeRequestAuthentication,
				"change-request import requires credential-free Git remote URLs; use a credential helper or SSH agent", nil)
		}
		effectiveURL, single := singleRemoteURL(string(output))
		if !single {
			return changeRequestError(ChangeRequestUnsafeConfiguration,
				fmt.Sprintf("change-request Git remote has an unsafe effective %s destination", test.label), nil)
		}
		canonicalEffectiveURL, _, err := canonicalCloneURL(g.root, effectiveURL)
		if err != nil {
			return changeRequestError(ChangeRequestUnsafeConfiguration,
				"failed to resolve the effective change-request Git remote", err)
		}
		matches := remoteMatchesRepository(canonicalEffectiveURL, repository)
		if hasExpectedURL {
			matches = matches && remoteURLsEqual(canonicalEffectiveURL, expectedURL)
		}
		if !matches {
			return changeRequestError(ChangeRequestUnsafeConfiguration,
				fmt.Sprintf("change-request Git remote has an unsafe effective %s destination", test.label), nil)
		}
	}
	return nil
}

func (g *ChangeRequestGit) projectPushSSHURL(ctx context.Context, remotes map[string]bool) string {
	selected := ""
	branch, branchErr := g.runSafe(ctx, g.root, "symbolic-ref", "--quiet", "--short", "HEAD")
	if branchErr == nil {
		selected = g.configuredRemote(ctx, "branch."+strings.TrimSpace(string(branch))+".pushRemote")
	}
	if selected == "" {
		selected = g.configuredRemote(ctx, "remote.pushDefault")
	}
	if selected == "" && branchErr == nil {
		selected = g.configuredRemote(ctx, "branch."+strings.TrimSpace(string(branch))+".remote")
	}
	if selected == "" && remotes["origin"] {
		selected = "origin"
	}
	if selected == "" && len(remotes) == 1 {
		for remote := range remotes {
			selected = remote
		}
	}
	if !remotes[selected] {
		return ""
	}
	pushURLs, err := g.runSafe(ctx, g.root, "remote", "get-url", "--all", "--push", selected)
	if err != nil || remoteURLListUnsafe(string(pushURLs)) {
		return ""
	}
	pushURL, single := singleRemoteURL(string(pushURLs))
	if !single || !isSSHRemoteURL(pushURL) ||
		!remoteMatchesRepository(pushURL, g.project) {
		return ""
	}
	return pushURL
}

func (g *ChangeRequestGit) configuredRemote(ctx context.Context, key string) string {
	output, err := g.runSafe(ctx, g.root, "config", "--includes", "--get", key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func (g *ChangeRequestGit) remoteHasCustomPushRefspec(ctx context.Context, remote string) bool {
	output, err := g.runSafe(ctx, g.root, "config", "--includes", "--null", "--list")
	if err != nil {
		return true
	}
	want := "remote." + remote + ".push"
	for record := range strings.SplitSeq(string(output), "\x00") {
		key, _, _ := strings.Cut(record, "\n")
		if strings.EqualFold(key, want) {
			return true
		}
	}
	return false
}

func (g *ChangeRequestGit) runAt(ctx context.Context, worktreePath string, args ...string) ([]byte, error) {
	dir := g.root
	if strings.TrimSpace(worktreePath) != "" {
		dir = worktreePath
	}
	return g.runSafe(ctx, dir, args...)
}

func (g *ChangeRequestGit) runSafe(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx = withLifecycleExecution(ctx, g.runner, g.runGit, nil)
	runner, err := lifecycleHooksRunner(ctx)
	if err != nil {
		return nil, err
	}
	return g.runWithRunner(ctx, runner, dir, args...)
}

func (g *ChangeRequestGit) run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return g.runWithRunner(ctx, g.runner, dir, args...)
}

func (g *ChangeRequestGit) runWithRunner(
	ctx context.Context, runner gitcmd.Runner, dir string, args ...string,
) ([]byte, error) {
	if g.runGit != nil {
		return g.runGit(ctx, runner, dir, args...)
	}
	return runner.Output(ctx, dir, args...)
}

func (g *ChangeRequestGit) rememberExpectedRemoteURL(remote string, push bool, expectedURL string) {
	g.expectedRemoteURLsMu.Lock()
	defer g.expectedRemoteURLsMu.Unlock()
	snapshot := g.expectedRemoteURLs[remote]
	if push {
		snapshot.push = expectedURL
	} else {
		snapshot.fetch = expectedURL
	}
	g.expectedRemoteURLs[remote] = snapshot
}

func (g *ChangeRequestGit) rememberExpectedRemoteURLs(remote, fetchURL, pushURL string) {
	g.expectedRemoteURLsMu.Lock()
	defer g.expectedRemoteURLsMu.Unlock()
	g.expectedRemoteURLs[remote] = remoteURLSnapshot{fetch: fetchURL, push: pushURL}
}

func (g *ChangeRequestGit) forgetExpectedRemoteURL(remote string) {
	g.expectedRemoteURLsMu.Lock()
	defer g.expectedRemoteURLsMu.Unlock()
	delete(g.expectedRemoteURLs, remote)
}

func (g *ChangeRequestGit) expectedRemoteURL(remote string, push bool) (string, bool) {
	g.expectedRemoteURLsMu.RLock()
	defer g.expectedRemoteURLsMu.RUnlock()
	snapshot, ok := g.expectedRemoteURLs[remote]
	if !ok {
		return "", false
	}
	if push {
		return snapshot.push, snapshot.push != ""
	}
	return snapshot.fetch, snapshot.fetch != ""
}

func configHasUnsafeRemote(output string) bool {
	for record := range strings.SplitSeq(output, "\x00") {
		key, value, found := strings.Cut(record, "\n")
		if !found {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if strings.HasPrefix(key, "remote.") &&
			(strings.HasSuffix(key, ".url") || strings.HasSuffix(key, ".pushurl")) &&
			gitremote.UnsafeForAutomation(strings.TrimSpace(value)) {
			return true
		}
	}
	return false
}

func configHasExecutableTransportOverride(output string) bool {
	for record := range strings.SplitSeq(output, "\x00") {
		key, _, _ := strings.Cut(record, "\n")
		key = strings.ToLower(strings.TrimSpace(key))
		if key == "core.gitproxy" || key == "core.sshcommand" {
			return true
		}
		if strings.HasPrefix(key, "remote.") &&
			(strings.HasSuffix(key, ".receivepack") ||
				strings.HasSuffix(key, ".uploadpack") ||
				strings.HasSuffix(key, ".vcs")) {
			return true
		}
	}
	return false
}

func configHasUnsafeHTTPConfiguration(output string) bool {
	for record := range strings.SplitSeq(output, "\x00") {
		key, value, _ := strings.Cut(record, "\n")
		key = strings.ToLower(strings.TrimSpace(key))
		if strings.HasPrefix(key, "remote.") &&
			(strings.HasSuffix(key, ".proxy") || strings.HasSuffix(key, ".proxyauthmethod")) {
			return true
		}
		if !strings.HasPrefix(key, "http.") {
			continue
		}
		if strings.HasSuffix(key, ".sslverify") {
			enabled, err := strconv.ParseBool(strings.TrimSpace(value))
			if err != nil || !enabled {
				return true
			}
		}
		for _, suffix := range []string{
			".extraheader", ".cookiefile", ".savecookies",
			".sslcert", ".sslkey", ".sslcertpasswordprotected",
			".proxy", ".proxyauthmethod", ".proxysslcert", ".proxysslkey",
			".proxysslcertpasswordprotected", ".proxysslcainfo",
			".emptyauth", ".delegation", ".proactiveauth",
			".sslcipherlist", ".sslversion", ".sslbackend", ".pinnedpubkey",
			".schannelcheckrevoke", ".schannelusesslcainfo", ".followredirects",
		} {
			if strings.HasSuffix(key, suffix) {
				return true
			}
		}
		if strings.HasSuffix(key, ".sslcainfo") ||
			strings.HasSuffix(key, ".sslcapath") {
			return true
		}
	}
	return false
}

func remoteURLListUnsafe(output string) bool {
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		if gitremote.UnsafeForAutomation(strings.TrimSpace(line)) {
			return true
		}
	}
	return false
}

func singleRemoteURL(output string) (string, bool) {
	remoteURL := ""
	for line := range strings.SplitSeq(strings.TrimSpace(output), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if remoteURL != "" {
			return "", false
		}
		remoteURL = line
	}
	return remoteURL, remoteURL != ""
}

func remoteURLsEqual(left, right string) bool {
	return strings.TrimSpace(left) == strings.TrimSpace(right)
}

func remoteMatchesRepository(remoteURL string, repository RemoteRepository) bool {
	if repository.Identity.Host == "" {
		return remoteURLsEqual(remoteURL, repository.CloneURL)
	}
	if gitremote.RemoteHost(remoteURL) == "" || gitremote.RemoteRepoPath(remoteURL) == "" {
		return false
	}
	if gitremote.ValidateRemoteIdentity(repository.Identity, remoteURL) == nil {
		return true
	}
	parsed, err := url.Parse(strings.TrimSpace(remoteURL))
	if err != nil || !isSSHRemoteURL(remoteURL) || parsed.Hostname() == "" {
		return false
	}
	if gitremote.NormalizeHost(parsed.Hostname()) !=
		gitremote.NormalizeHost(repository.Identity.Host) {
		return false
	}
	wantRepo := strings.Trim(strings.TrimSpace(repository.Identity.Owner)+"/"+
		strings.TrimSpace(repository.Identity.Name), "/")
	return strings.EqualFold(gitremote.RemoteRepoPath(remoteURL), wantRepo)
}

func forkSSHURL(projectURL string, repository gitremote.Identity) (string, error) {
	projectURL = strings.TrimSpace(projectURL)
	repositoryPath := strings.Trim(repository.Owner, "/") + "/" + strings.Trim(repository.Name, "/") + ".git"
	if strings.Contains(projectURL, "://") {
		parsed, err := url.Parse(projectURL)
		if err != nil || parsed.Scheme == "" || parsed.Host == "" {
			return "", fmt.Errorf("invalid project SSH URL")
		}
		parsed.Path = "/" + repositoryPath
		parsed.RawPath = ""
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String(), nil
	}
	colon := strings.IndexByte(projectURL, ':')
	slash := strings.IndexAny(projectURL, `/\`)
	if colon < 0 || slash >= 0 && colon > slash {
		return "", fmt.Errorf("invalid project SSH URL")
	}
	return projectURL[:colon+1] + repositoryPath, nil
}

func isSSHRemoteURL(remoteURL string) bool {
	remoteURL = strings.TrimSpace(remoteURL)
	if gitremote.IsLocal(remoteURL) || gitremote.IsWindowsDrivePath(remoteURL) {
		return false
	}
	lower := strings.ToLower(remoteURL)
	if strings.HasPrefix(lower, "ssh://") || strings.HasPrefix(lower, "git+ssh://") ||
		strings.HasPrefix(lower, "ssh+git://") {
		return true
	}
	if strings.Contains(lower, "://") {
		return false
	}
	colon := strings.IndexByte(remoteURL, ':')
	slash := strings.IndexAny(remoteURL, `/\`)
	return colon >= 0 && (slash < 0 || colon < slash)
}

// canonicalCloneURL makes local clone paths stable across commands run from
// the project and its managed worktrees. Git interprets any URL without a
// scheme or SCP-style host prefix as a path relative to the current command.
func canonicalCloneURL(projectRoot, raw string) (string, bool, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false, nil
	}
	if filepath.IsAbs(trimmed) || gitremote.IsLocal(trimmed) {
		if strings.HasPrefix(strings.ToLower(trimmed), "file:") {
			return trimmed, true, nil
		}
		absolute, err := filepath.Abs(trimmed)
		if err != nil {
			return "", true, err
		}
		return filepath.Clean(absolute), true, nil
	}
	if strings.Contains(trimmed, "://") || cloneURLHasSCPHost(trimmed) {
		return trimmed, false, nil
	}
	absolute, err := filepath.Abs(filepath.Join(projectRoot, trimmed))
	if err != nil {
		return "", true, err
	}
	return filepath.Clean(absolute), true, nil
}

func cloneURLHasSCPHost(value string) bool {
	colon := strings.IndexByte(value, ':')
	slash := strings.IndexAny(value, `/\`)
	return colon >= 0 && (slash < 0 || colon < slash)
}

func isGitAuthenticationFailure(message string) bool {
	patterns := []string{
		"authentication failed", "permission denied", "could not read username", "could not read password",
		"terminal prompts disabled", "access denied", "returned error: 401", "returned error: 403",
		"http 401", "http 403",
	}
	for _, pattern := range patterns {
		if strings.Contains(message, pattern) {
			return true
		}
	}
	return false
}

func isGitNetworkFailure(message string) bool {
	patterns := []string{
		"could not resolve", "unable to access", "connection timed out", "connection refused",
		"failed to connect", "network is unreachable", "connection reset",
	}
	for _, pattern := range patterns {
		if strings.Contains(message, pattern) {
			return true
		}
	}
	return false
}

var changeRequestGitVersionPattern = regexp.MustCompile(
	`(?i)git version (\d+)\.(\d+)(?:\.(\d+))?(?:\.windows\.(\d+))?(?:\s|$)`,
)

func supportsChangeRequestGitVersion(output, goos string) bool {
	match := changeRequestGitVersionPattern.FindStringSubmatch(strings.TrimSpace(output))
	if len(match) == 0 {
		return false
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	patch, _ := strconv.Atoi(match[3])
	if major < 2 || major == 2 && (minor < 39 || minor == 39 && patch < 1) {
		return false
	}
	if goos != "windows" {
		return true
	}
	if major != 2 || minor != 53 || patch != 0 {
		return major > 2 || major == 2 && (minor > 53 || minor == 53 && patch > 0)
	}
	windowsPatch, err := strconv.Atoi(match[4])
	return err == nil && windowsPatch >= 3
}

func safeCheckoutGitVersionRequirement(goos string) string {
	if goos == "windows" {
		return "Git for Windows 2.53.0.windows.3 or newer"
	}
	return "Git 2.39.1 or newer"
}
