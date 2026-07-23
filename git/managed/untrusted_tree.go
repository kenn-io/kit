package managedworktree

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	gitcmd "go.kenn.io/kit/git/cmd"
)

// untrustedTreeIsolation neutralizes Git programs that a fetched tree can
// select through tracked hooks or attributes. The repository and its
// configuration remain trusted; only the fetched commit is treated as
// untrusted.
type untrustedTreeIsolation struct {
	runner gitcmd.Runner
	config []gitcmd.Config
}

// Git invokes these through its compiled-in shell because they contain shell
// syntax. They use only shell builtins, so an untrusted worktree cannot steer
// them through PATH. The diff replacement emits a simple old/new rendering;
// the merge replacement declines the custom driver so Git reports a conflict.
const (
	safeExternalDiffCommand = `f() { emit() { prefix=$1; file=$2; line=; while IFS= read -r line; do printf '%s%s\n' "$prefix" "$line"; line=; done < "$file"; case "$line" in "") ;; *) printf '%s%s\n' "$prefix" "$line"; printf '%s\n' '\ No newline at end of file' ;; esac; }; emit - "$2"; emit + "$5"; }; f`
	safeTextconvCommand     = `f() { line=; while IFS= read -r line; do printf '%s\n' "$line"; line=; done < "$1"; case "$line" in "") ;; *) printf '%s' "$line" ;; esac; }; f`
	safeMergeDriverCommand  = `f() { return 1; }; f`
)

var untrustedTreeGitVersionPattern = regexp.MustCompile(
	`(?i)git version (\d+)\.(\d+)(?:\.(\d+))?(?:\.windows\.(\d+))?(?:\s|$)`,
)

func validateUntrustedTreeCheckoutGitVersion(
	ctx context.Context, root string,
) error {
	out, err := runLifecycleGit(ctx, root, "version")
	if err != nil {
		return &ChangeRequestError{
			Kind: ChangeRequestUnsupportedGit,
			Message: fmt.Sprintf(
				"determine Git version before untrusted-tree checkout: %s",
				strings.TrimSpace(string(out)),
			),
			Cause: err,
		}
	}
	if supportsUntrustedTreeCheckoutGitVersion(string(out), runtime.GOOS) {
		return nil
	}
	requirement := "Git 2.39.1 or newer"
	if runtime.GOOS == "windows" || isGitForWindowsVersion(string(out)) {
		requirement = "Git for Windows 2.53.0.windows.3 or newer"
	}
	return &ChangeRequestError{
		Kind:    ChangeRequestUnsupportedGit,
		Message: "untrusted-tree checkout requires " + requirement,
	}
}

func supportsUntrustedTreeCheckoutGitVersion(output, goos string) bool {
	match := untrustedTreeGitVersionPattern.FindStringSubmatch(
		strings.TrimSpace(output),
	)
	if len(match) == 0 {
		return false
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	patch, _ := strconv.Atoi(match[3])
	if major < 2 || major == 2 && (minor < 39 || minor == 39 && patch < 1) {
		return false
	}
	if goos != "windows" && match[4] == "" {
		return true
	}
	if major != 2 || minor != 53 || patch != 0 {
		return major > 2 ||
			major == 2 && (minor > 53 || minor == 53 && patch > 0)
	}
	windowsPatch, err := strconv.Atoi(match[4])
	return err == nil && windowsPatch >= 3
}

func isGitForWindowsVersion(output string) bool {
	match := untrustedTreeGitVersionPattern.FindStringSubmatch(
		strings.TrimSpace(output),
	)
	return len(match) != 0 && match[4] != ""
}

func validateWorktreeConfigCompatibility(
	ctx context.Context, root string,
) error {
	if value, present, err := localGitConfigValue(
		ctx, root, "core.worktree",
	); err != nil {
		return err
	} else if present {
		return fmt.Errorf(
			"cannot enable worktree-scoped configuration while shared core.worktree is set to %q",
			value,
		)
	}
	if value, present, err := localGitConfigValue(
		ctx, root, "core.bare",
	); err != nil {
		return err
	} else if present && strings.EqualFold(value, "true") {
		return fmt.Errorf(
			"cannot enable worktree-scoped configuration while shared core.bare=true",
		)
	}
	return nil
}

func localGitConfigValue(
	ctx context.Context, root, key string,
) (string, bool, error) {
	args := []string{"config", "--local", "--get", key}
	if key == "core.bare" {
		args = []string{"config", "--local", "--type=bool", "--get", key}
	}
	out, err := runLifecycleGit(ctx, root, args...)
	if err == nil {
		return strings.TrimSpace(string(out)), true, nil
	}
	if gitcmd.IsExitCode(err, 1) {
		return "", false, nil
	}
	return "", false, fmt.Errorf(
		"inspect shared %s configuration: %w: %s",
		key, err, strings.TrimSpace(string(out)),
	)
}

func prepareUntrustedTreeIsolation(
	ctx context.Context, root string,
) (untrustedTreeIsolation, error) {
	if err := rejectCommandScopeIsolationOverrides(
		ctx, root, lifecycleRunner(ctx),
	); err != nil {
		return untrustedTreeIsolation{}, err
	}
	hooksPath, err := managedEmptyHooksPath(ctx, root)
	if err != nil {
		return untrustedTreeIsolation{}, err
	}

	config := []gitcmd.Config{
		{Key: "core.hooksPath", Value: hooksPath},
		{Key: "core.fsmonitor", Value: "false"},
		{Key: "submodule.recurse", Value: "false"},
		{Key: "fetch.recurseSubmodules", Value: "false"},
	}
	runner := lifecycleRunner(ctx)
	for _, entry := range config {
		runner = runner.WithConfig(entry.Key, entry.Value)
	}
	return untrustedTreeIsolation{runner: runner, config: config}, nil
}

func rejectCommandScopeIsolationOverrides(
	ctx context.Context, root string, runner gitcmd.Runner,
) error {
	runner.Env = withoutGitRepositoryBindings(runner.Env)
	runner.StripEnv = false
	runner.NullGlobalConfig = false
	runner.NoSystemConfig = false
	out, err := runLifecycleGitWithRunner(
		ctx, runner, root,
		"config", "--null", "--show-scope", "--show-origin", "--name-only",
		"--includes", "--list",
	)
	if err != nil {
		return fmt.Errorf(
			"inspect command-scope Git configuration: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	fields := bytes.Split(out, []byte{0})
	var overrides []string
	for index := 0; index+2 < len(fields); index += 3 {
		if string(fields[index]) != "command" {
			continue
		}
		key := strings.TrimSpace(string(fields[index+2]))
		if isolationSensitiveConfigKey(key) {
			overrides = append(overrides, key)
		}
	}
	if len(overrides) == 0 {
		return nil
	}
	sort.Strings(overrides)
	return fmt.Errorf(
		"command-scope Git configuration cannot be isolated: %s",
		strings.Join(overrides, ", "),
	)
}

func isolationSensitiveConfigKey(key string) bool {
	lower := strings.ToLower(key)
	switch lower {
	case "core.hookspath", "core.fsmonitor", "core.worktree",
		"submodule.recurse", "fetch.recursesubmodules":
		return true
	}
	if strings.HasPrefix(lower, "hook.") &&
		(strings.HasSuffix(lower, ".command") ||
			strings.HasSuffix(lower, ".event") ||
			strings.HasSuffix(lower, ".enabled")) {
		return true
	}
	if strings.HasPrefix(lower, "submodule.") &&
		strings.HasSuffix(lower, ".fetchrecursesubmodules") {
		return true
	}
	return len(neutralizeAttributeDrivers([]string{key})) != 0
}

func managedEmptyHooksPath(ctx context.Context, root string) (string, error) {
	out, err := runLifecycleGitWithRunner(
		ctx, lifecycleRunner(ctx).WithConfig("core.hooksPath", os.DevNull),
		root, "rev-parse", "--git-common-dir",
	)
	if err != nil {
		return "", fmt.Errorf(
			"resolve common Git directory: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	commonDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	hooksPath := filepath.Join(commonDir, "kit-managed", "empty-hooks")
	if err := os.MkdirAll(hooksPath, 0o700); err != nil {
		return "", fmt.Errorf("create managed empty hooks directory: %w", err)
	}
	if err := os.Chmod(hooksPath, 0o700); err != nil {
		return "", fmt.Errorf("secure managed empty hooks directory: %w", err)
	}
	entries, err := os.ReadDir(hooksPath)
	if err != nil {
		return "", fmt.Errorf("inspect managed empty hooks directory: %w", err)
	}
	if len(entries) != 0 {
		return "", fmt.Errorf(
			"managed empty hooks directory is not empty: %s", hooksPath,
		)
	}
	return hooksPath, nil
}

func completeUntrustedTreeIsolation(
	ctx context.Context, worktreePath string, isolation untrustedTreeIsolation,
) (untrustedTreeIsolation, error) {
	checkoutKeys, err := gitConfigKeys(
		ctx, worktreePath, isolation.runner,
	)
	if err != nil {
		return untrustedTreeIsolation{}, err
	}
	ambientKeys, err := ambientGitConfigKeys(
		ctx, worktreePath, isolation.runner,
	)
	if err != nil {
		return untrustedTreeIsolation{}, err
	}
	keys := append(checkoutKeys, ambientKeys...)
	if hooks := configuredGitHooks(keys); len(hooks) != 0 {
		return untrustedTreeIsolation{}, fmt.Errorf(
			"configured Git hooks are unsupported for untrusted tree imports: %s",
			strings.Join(hooks, ", "),
		)
	}
	drivers := neutralizeAttributeDrivers(keys)
	submodules, err := submoduleFetchRecurseConfig(
		ctx, worktreePath, isolation.runner,
	)
	if err != nil {
		return untrustedTreeIsolation{}, err
	}
	drivers = append(drivers, submodules...)
	existing := make(map[string]struct{}, len(isolation.config))
	for _, entry := range isolation.config {
		existing[entry.Key] = struct{}{}
	}
	for _, entry := range drivers {
		if _, ok := existing[entry.Key]; ok {
			continue
		}
		existing[entry.Key] = struct{}{}
		isolation.config = append(isolation.config, entry)
		isolation.runner = isolation.runner.WithConfig(entry.Key, entry.Value)
	}
	return isolation, nil
}

func configuredGitHooks(keys []string) []string {
	hooks := make(map[string]struct{})
	for _, key := range keys {
		if name, ok := configuredSubsectionName(
			key, "hook", []string{".command", ".event"},
		); ok {
			hooks[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(hooks))
	for name := range hooks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func submoduleFetchRecurseConfig(
	ctx context.Context, worktreePath string, runner gitcmd.Runner,
) ([]gitcmd.Config, error) {
	treeEntry, err := runLifecycleGitWithRunner(
		ctx, runner, worktreePath,
		"ls-tree", "--name-only", "-z", "HEAD", "--", ".gitmodules",
	)
	if err != nil {
		return nil, fmt.Errorf(
			"inspect merge request tree for submodules: %w: %s",
			err, strings.TrimSpace(string(treeEntry)),
		)
	}
	if len(treeEntry) == 0 {
		return nil, nil
	}
	out, err := runLifecycleGitWithRunner(
		ctx, runner, worktreePath,
		"config", "--null", "--name-only", "--blob", "HEAD:.gitmodules",
		"--list",
	)
	if err != nil {
		if gitcmd.IsExitCode(err, 1) {
			return nil, nil
		}
		return nil, fmt.Errorf(
			"inspect committed submodule recursion configuration: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	fields := bytes.Split(out, []byte{0})
	config := make([]gitcmd.Config, 0, len(fields))
	for _, field := range fields {
		if name, ok := configuredSubmoduleName(string(field)); ok {
			config = append(config, gitcmd.Config{
				Key:   "submodule." + name + ".fetchRecurseSubmodules",
				Value: "false",
			})
		}
	}
	return config, nil
}

func configuredSubmoduleName(key string) (string, bool) {
	return configuredSubsectionName(
		key, "submodule", []string{".path", ".fetchrecursesubmodules"},
	)
}

func configuredSubsectionName(
	key, section string, suffixes []string,
) (string, bool) {
	lower := strings.ToLower(key)
	prefix := strings.ToLower(section) + "."
	if !strings.HasPrefix(lower, prefix) {
		return "", false
	}
	start := len(prefix)
	for _, suffix := range suffixes {
		if !strings.HasSuffix(lower, suffix) {
			continue
		}
		end := len(key) - len(suffix)
		if end <= start {
			return "", false
		}
		return key[start:end], true
	}
	return "", false
}

func gitConfigKeys(
	ctx context.Context, worktreePath string, runner gitcmd.Runner,
) ([]string, error) {
	out, err := runLifecycleGitWithRunner(
		ctx, runner, worktreePath,
		"config", "--null", "--name-only", "--includes", "--list",
	)
	if err != nil {
		return nil, fmt.Errorf(
			"inspect effective Git configuration: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	fields := bytes.Split(out, []byte{0})
	keys := make([]string, 0, len(fields))
	for _, field := range fields {
		if key := strings.TrimSpace(string(field)); key != "" {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

func ambientGitConfigKeys(
	ctx context.Context, worktreePath string, runner gitcmd.Runner,
) ([]string, error) {
	// The first scan exactly matches checkout. This second scan preserves
	// explicit global/system and command-scope selectors so persistent
	// isolation also covers ordinary Git commands run outside the
	// application's stripped environment.
	runner.Env = withoutGitRepositoryBindings(runner.Env)
	runner.StripEnv = false
	runner.NullGlobalConfig = false
	runner.NoSystemConfig = false
	return gitConfigKeys(ctx, worktreePath, runner)
}

func rejectConfigOriginsInsideWorktree(
	ctx context.Context, worktreePath string, runner gitcmd.Runner,
) error {
	runner.Env = withoutGitRepositoryBindings(runner.Env)
	runner.StripEnv = false
	runner.NullGlobalConfig = false
	runner.NoSystemConfig = false
	out, err := runLifecycleGitWithRunner(
		ctx, runner, worktreePath,
		"config", "--null", "--show-origin", "--name-only",
		"--includes", "--list",
	)
	if err != nil {
		return fmt.Errorf(
			"inspect Git configuration origins: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	fields := bytes.Split(out, []byte{0})
	worktree := comparableWorktreePath(worktreePath)
	lexicalWorktree := lexicalWorktreePath(worktreePath)
	for index := 0; index+1 < len(fields); index += 2 {
		origin := string(fields[index])
		if !strings.HasPrefix(origin, "file:") {
			continue
		}
		configPath := strings.TrimPrefix(origin, "file:")
		if !filepath.IsAbs(configPath) {
			configPath = filepath.Join(worktreePath, configPath)
		}
		if pathWithinRoot(
			lexicalWorktree, lexicalWorktreePath(configPath),
		) || pathWithinRoot(
			worktree, comparableWorktreePath(configPath),
		) || pathWithinRootByIdentity(worktreePath, configPath) {
			return fmt.Errorf(
				"Git configuration inside merge request worktree is not allowed: %s",
				configPath,
			)
		}
	}
	return nil
}

func pathWithinRootByIdentity(root, path string) bool {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false
	}
	for current := path; ; current = filepath.Dir(current) {
		if info, statErr := os.Stat(current); statErr == nil &&
			os.SameFile(rootInfo, info) {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
	}
}

func lexicalWorktreePath(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		absolute = path
	}
	absolute = filepath.Clean(absolute)
	if filepath.Separator == '\\' {
		absolute = strings.ToLower(absolute)
	}
	return absolute
}

func withoutGitRepositoryBindings(env []string) []string {
	clean := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(key) {
		case "GIT_DIR",
			"GIT_WORK_TREE",
			"GIT_INDEX_FILE",
			"GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES",
			"GIT_COMMON_DIR",
			"GIT_NAMESPACE",
			"GIT_PREFIX":
			continue
		}
		clean = append(clean, entry)
	}
	return clean
}

func neutralizeAttributeDrivers(keys []string) []gitcmd.Config {
	filters := make(map[string]struct{})
	diffs := make(map[string]struct{})
	merges := make(map[string]struct{})
	for _, key := range keys {
		lower := strings.ToLower(key)
		switch {
		case strings.HasPrefix(lower, "filter."):
			if driver, ok := configuredDriverName(
				key, []string{".clean", ".smudge", ".process", ".required"},
			); ok {
				filters[driver] = struct{}{}
			}
		case strings.HasPrefix(lower, "diff."):
			if driver, ok := configuredDriverName(
				key, []string{".command", ".textconv"},
			); ok {
				diffs[driver] = struct{}{}
			}
		case strings.HasPrefix(lower, "merge."):
			if driver, ok := configuredDriverName(
				key, []string{".driver"},
			); ok {
				merges[driver] = struct{}{}
			}
		}
	}

	var config []gitcmd.Config
	for _, driver := range sortedDriverNames(filters) {
		prefix := "filter." + driver
		config = append(config,
			gitcmd.Config{Key: prefix + ".clean", Value: ""},
			gitcmd.Config{Key: prefix + ".smudge", Value: ""},
			gitcmd.Config{Key: prefix + ".process", Value: ""},
			gitcmd.Config{Key: prefix + ".required", Value: "false"},
		)
	}
	for _, driver := range sortedDriverNames(diffs) {
		prefix := "diff." + driver
		config = append(config,
			gitcmd.Config{Key: prefix + ".command", Value: safeExternalDiffCommand},
			gitcmd.Config{Key: prefix + ".textconv", Value: safeTextconvCommand},
		)
	}
	for _, driver := range sortedDriverNames(merges) {
		config = append(config, gitcmd.Config{
			Key: "merge." + driver + ".driver", Value: safeMergeDriverCommand,
		})
	}
	return config
}

func configuredDriverName(key string, suffixes []string) (string, bool) {
	lower := strings.ToLower(key)
	start := strings.IndexByte(key, '.') + 1
	if start == 0 {
		return "", false
	}
	for _, suffix := range suffixes {
		if strings.HasSuffix(lower, suffix) {
			end := len(key) - len(suffix)
			if end <= start {
				return "", false
			}
			return key[start:end], true
		}
	}
	return "", false
}

func sortedDriverNames(drivers map[string]struct{}) []string {
	names := make([]string, 0, len(drivers))
	for name := range drivers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func persistUntrustedTreeIsolation(
	ctx context.Context, root, worktreePath string,
	isolation untrustedTreeIsolation,
) error {
	if out, err := runLifecycleGitWithRunner(
		ctx, isolation.runner, root,
		"config", "extensions.worktreeConfig", "true",
	); err != nil {
		return fmt.Errorf(
			"enable worktree configuration: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	for _, entry := range isolation.config {
		if out, err := runLifecycleGitWithRunner(
			ctx, isolation.runner, worktreePath,
			"config", "--worktree", entry.Key, entry.Value,
		); err != nil {
			return fmt.Errorf(
				"persist untrusted-tree isolation for %s: %w: %s",
				entry.Key, err, strings.TrimSpace(string(out)),
			)
		}
	}
	return nil
}

func materializeUntrustedTree(
	ctx context.Context, worktreePath string,
	isolation untrustedTreeIsolation,
) error {
	out, err := runLifecycleGitWithRunner(
		ctx, isolation.runner, worktreePath,
		"reset", "--hard", "HEAD",
	)
	if err != nil {
		return fmt.Errorf(
			"materialize merge request worktree: %w: %s",
			err, strings.TrimSpace(string(out)),
		)
	}
	return nil
}
