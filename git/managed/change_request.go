package managedworktree

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitremote "go.kenn.io/kit/git/remote"
)

// ChangeRequestErrorKind classifies failures from the shared change-request
// Git boundary so applications can map them to their own error contracts.
type ChangeRequestErrorKind string

const (
	ChangeRequestAuthentication      ChangeRequestErrorKind = "authentication"
	ChangeRequestNetwork             ChangeRequestErrorKind = "network"
	ChangeRequestInaccessibleHead    ChangeRequestErrorKind = "inaccessible_head"
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
	ProjectRoot            string
	ProjectIdentity        gitremote.Identity
	RemoteNamePrefix       string
	HookIsolationNamespace string
	Runner                 gitcmd.Runner
	RunGit                 GitRunner
}

// ChangeRequestGit owns provider-neutral remote validation, isolated fetch,
// worktree config validation, and persistent safe push routing.
type ChangeRequestGit struct {
	root             string
	project          gitremote.Identity
	remoteNamePrefix string
	hookNamespace    string
	runner           gitcmd.Runner
	runGit           GitRunner

	expectedRemoteURLsMu sync.RWMutex
	expectedRemoteURLs   map[string]string
}

// NewChangeRequestGit constructs a shared change-request Git boundary.
func NewChangeRequestGit(opts ChangeRequestGitOptions) (*ChangeRequestGit, error) {
	root, err := absRequired(opts.ProjectRoot, "project root")
	if err != nil {
		return nil, err
	}
	runner := opts.Runner
	if runner.Env == nil {
		runner = gitcmd.New()
	}
	prefix := sanitizeRemoteName(opts.RemoteNamePrefix)
	if prefix == "" {
		prefix = "review"
	}
	namespace := sanitizeRemoteName(opts.HookIsolationNamespace)
	if namespace == "" {
		namespace = "kit"
	}
	return &ChangeRequestGit{
		root: root, project: opts.ProjectIdentity,
		remoteNamePrefix: prefix, hookNamespace: namespace,
		runner: runner, runGit: opts.RunGit,
		expectedRemoteURLs: make(map[string]string),
	}, nil
}

// Validate verifies the Git version and effective project configuration before
// any remote or worktree mutation occurs.
func (g *ChangeRequestGit) Validate(ctx context.Context) error {
	output, err := g.run(ctx, g.root, "version")
	if err != nil {
		return changeRequestError(ChangeRequestUnsupportedGit, "failed to determine Git version", err)
	}
	if !supportsChangeRequestWorktreeConfig(string(output)) {
		return changeRequestError(ChangeRequestUnsupportedGit, "change-request import requires Git 2.20 or newer", nil)
	}
	return g.validateConfigurationAt(ctx, "")
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
	if configHasCustomReceivePack(string(configOutput)) {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"change-request import does not allow custom Git receive-pack commands", nil)
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
		key, _, _ := strings.Cut(record, "\n")
		if strings.EqualFold(strings.TrimSpace(key), "core.hookspath") || record == "" {
			continue
		}
		records = append(records, record)
	}
	return records, nil
}

// EnsureRemote returns a single-destination, credential-free remote for the
// source repository, adding a deterministic remote when no safe match exists.
func (g *ChangeRequestGit) EnsureRemote(ctx context.Context, repository RemoteRepository) (string, error) {
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
	remoteURL := strings.TrimSpace(repository.CloneURL)
	projectSSHURL := g.projectPushSSHURL(ctx, existing)
	if projectSSHURL != "" {
		remoteURL, err = forkSSHURL(projectSSHURL, repository.Identity)
		if err != nil {
			return "", changeRequestError(ChangeRequestUnsafeConfiguration,
				"failed to preserve the project's SSH transport for the change-request remote", err)
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
	for _, name := range remoteNames {
		fetchURLs, fetchErr := g.runSafe(ctx, g.root, "remote", "get-url", "--all", name)
		pushURLs, pushErr := g.runSafe(ctx, g.root, "remote", "get-url", "--all", "--push", name)
		if fetchErr != nil || pushErr != nil || remoteURLListUnsafe(string(fetchURLs)) || remoteURLListUnsafe(string(pushURLs)) {
			continue
		}
		fetchURL, singleFetch := singleRemoteURL(string(fetchURLs))
		fetchMatches := singleFetch && remoteMatchesRepository(fetchURL, repository)
		pushMatches := singleRemoteMatchesRepository(string(pushURLs), repository)
		if projectSSHURL != "" {
			fetchMatches = singleFetch && remoteURLsEqual(fetchURL, remoteURL)
			pushMatches = singleRemoteURLEquals(string(pushURLs), remoteURL)
		}
		if fetchMatches && pushMatches && !g.remoteHasCustomPushRefspec(ctx, name) {
			if projectSSHURL != "" {
				g.rememberExpectedRemoteURL(name, remoteURL)
			}
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
	g.rememberExpectedRemoteURL(name, remoteURL)
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

// Fetch imports sourceRef into destinationRef with hooks and prompts disabled
// and returns the resolved commit OID.
func (g *ChangeRequestGit) Fetch(ctx context.Context, remote, sourceRef, destinationRef string) (string, error) {
	refspec := "+" + sourceRef + ":" + destinationRef
	if _, err := g.runSafe(ctx, g.root, "fetch", "--no-tags", remote, refspec); err != nil {
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
	sha, err := g.runSafe(ctx, g.root, "rev-parse", "--verify", destinationRef+"^{commit}")
	if err != nil {
		return "", changeRequestError(ChangeRequestInaccessibleHead,
			"fetched change-request head is not a commit", err)
	}
	return strings.TrimSpace(string(sha)), nil
}

// ConfigurePush persists worktree-scoped routing to the contributor's source
// branch and installs a non-contributor-controlled empty hooks directory.
func (g *ChangeRequestGit) ConfigurePush(
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
	for _, args := range commands {
		if _, err := g.runSafe(ctx, created.Path, args...); err != nil {
			return err
		}
	}
	return g.validatePushRouting(ctx, created, remote, repository, sourceBranch, hooksPath)
}

// ConfigureWorktreeIsolation persists a non-contributor-controlled empty
// hooks directory for a newly created change-request worktree, even when no
// safe upstream is available for push routing.
func (g *ChangeRequestGit) ConfigureWorktreeIsolation(
	ctx context.Context, created CreateWorktreeResult,
) error {
	_, err := g.configureWorktreeIsolation(ctx, created)
	return err
}

func (g *ChangeRequestGit) configureWorktreeIsolation(
	ctx context.Context, created CreateWorktreeResult,
) (string, error) {
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
	return g.configureDisabledHooks(ctx, created.Path)
}

func (g *ChangeRequestGit) validateWorkspaceHead(ctx context.Context, created CreateWorktreeResult) error {
	branch, err := g.runSafe(ctx, created.Path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || strings.TrimSpace(string(branch)) != created.Branch {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			fmt.Sprintf("change-request worktree is no longer on generated branch %q", created.Branch), err)
	}
	return nil
}

func (g *ChangeRequestGit) configureDisabledHooks(ctx context.Context, worktreePath string) (string, error) {
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
		if err := ensurePrivateDirectory(directory); err != nil {
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
	return hooksPath, nil
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
	hooksOutput, err := g.run(ctx, created.Path, "config", "--includes", "--path", "--get", "core.hooksPath")
	if err != nil || filepath.Clean(strings.TrimSpace(string(hooksOutput))) != filepath.Clean(expectedHooksPath) {
		return changeRequestError(ChangeRequestUnsafeConfiguration,
			"change-request worktree does not retain persistent hook isolation", err)
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
	expectedURL, hasExpectedURL := g.expectedRemoteURL(remote)
	if !hasExpectedURL && strings.TrimSpace(repository.CloneURL) != "" &&
		gitremote.RemoteHost(repository.CloneURL) == "" {
		expectedURL = strings.TrimSpace(repository.CloneURL)
		hasExpectedURL = true
	}
	for _, test := range []struct {
		label string
		args  []string
	}{
		{label: "fetch", args: []string{"remote", "get-url", "--all", remote}},
		{label: "push", args: []string{"remote", "get-url", "--all", "--push", remote}},
	} {
		output, err := g.runAt(ctx, worktreePath, test.args...)
		if err != nil {
			return changeRequestError(ChangeRequestUnsafeConfiguration,
				"failed to validate the change-request Git remote", err)
		}
		if remoteURLListUnsafe(string(output)) {
			return changeRequestError(ChangeRequestAuthentication,
				"change-request import requires credential-free Git remote URLs; use a credential helper or SSH agent", nil)
		}
		matches := singleRemoteMatchesRepository(string(output), repository)
		if hasExpectedURL {
			matches = singleRemoteURLEquals(string(output), expectedURL)
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
	if !single || !isSSHRemoteURL(pushURL) {
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

func (g *ChangeRequestGit) rememberExpectedRemoteURL(remote, expectedURL string) {
	g.expectedRemoteURLsMu.Lock()
	defer g.expectedRemoteURLsMu.Unlock()
	g.expectedRemoteURLs[remote] = expectedURL
}

func (g *ChangeRequestGit) forgetExpectedRemoteURL(remote string) {
	g.expectedRemoteURLsMu.Lock()
	defer g.expectedRemoteURLsMu.Unlock()
	delete(g.expectedRemoteURLs, remote)
}

func (g *ChangeRequestGit) expectedRemoteURL(remote string) (string, bool) {
	g.expectedRemoteURLsMu.RLock()
	defer g.expectedRemoteURLsMu.RUnlock()
	expectedURL, ok := g.expectedRemoteURLs[remote]
	return expectedURL, ok
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

func configHasCustomReceivePack(output string) bool {
	for record := range strings.SplitSeq(output, "\x00") {
		key, _, _ := strings.Cut(record, "\n")
		key = strings.ToLower(strings.TrimSpace(key))
		if strings.HasPrefix(key, "remote.") && strings.HasSuffix(key, ".receivepack") {
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

func singleRemoteURLEquals(output, expected string) bool {
	remoteURL, single := singleRemoteURL(output)
	return single && remoteURLsEqual(remoteURL, expected)
}

func remoteURLsEqual(left, right string) bool {
	return strings.TrimSpace(left) == strings.TrimSpace(right)
}

func singleRemoteMatchesRepository(output string, repository RemoteRepository) bool {
	remoteURL, single := singleRemoteURL(output)
	return single && remoteMatchesRepository(remoteURL, repository)
}

func remoteMatchesRepository(remoteURL string, repository RemoteRepository) bool {
	if repository.Identity.Host == "" {
		return remoteURLsEqual(remoteURL, repository.CloneURL)
	}
	return gitremote.ValidateRemoteIdentity(repository.Identity, remoteURL) == nil
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
	lower := strings.ToLower(remoteURL)
	if strings.HasPrefix(lower, "ssh://") || strings.HasPrefix(lower, "git+ssh://") {
		return true
	}
	if strings.Contains(lower, "://") {
		return false
	}
	colon := strings.IndexByte(remoteURL, ':')
	slash := strings.IndexAny(remoteURL, `/\`)
	return colon >= 0 && (slash < 0 || colon < slash)
}

func ensurePrivateDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is not a trusted directory", path)
	}
	return os.Chmod(path, 0o700)
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

var changeRequestGitVersionPattern = regexp.MustCompile(`(?i)git version (\d+)\.(\d+)(?:\.(\d+))?`)

func supportsChangeRequestWorktreeConfig(output string) bool {
	match := changeRequestGitVersionPattern.FindStringSubmatch(strings.TrimSpace(output))
	if len(match) == 0 {
		return false
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	return major > 2 || major == 2 && minor >= 20
}
