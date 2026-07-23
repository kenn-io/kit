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

// Git appends the standard seven external-diff arguments to this command.
// The nested diff explicitly disables attributes and text conversion, while
// the wrapper translates diff's ordinary "different" exit code to success.
const safeExternalDiffCommand = `sh -c 'git diff --no-index --no-ext-diff --no-textconv -- "$2" "$5"; status=$?; test "$status" -le 1' --`

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
	if runtime.GOOS == "windows" {
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
	if goos != "windows" {
		return true
	}
	if major != 2 || minor != 53 || patch != 0 {
		return major > 2 ||
			major == 2 && (minor > 53 || minor == 53 && patch > 0)
	}
	windowsPatch, err := strconv.Atoi(match[4])
	return err == nil && windowsPatch >= 3
}

func prepareUntrustedTreeIsolation(
	ctx context.Context, root string,
) (untrustedTreeIsolation, error) {
	hooksPath, err := managedEmptyHooksPath(ctx, root)
	if err != nil {
		return untrustedTreeIsolation{}, err
	}

	config := []gitcmd.Config{
		{Key: "core.hooksPath", Value: hooksPath},
		{Key: "core.fsmonitor", Value: "false"},
		{Key: "submodule.recurse", Value: "false"},
	}
	runner := lifecycleRunner(ctx)
	for _, entry := range config {
		runner = runner.WithConfig(entry.Key, entry.Value)
	}
	return untrustedTreeIsolation{runner: runner, config: config}, nil
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
	keys, err := effectiveGitConfigKeys(ctx, worktreePath, isolation.runner)
	if err != nil {
		return untrustedTreeIsolation{}, err
	}
	drivers := neutralizeAttributeDrivers(keys)
	isolation.config = append(isolation.config, drivers...)
	for _, entry := range drivers {
		isolation.runner = isolation.runner.WithConfig(entry.Key, entry.Value)
	}
	return isolation, nil
}

func effectiveGitConfigKeys(
	ctx context.Context, worktreePath string, runner gitcmd.Runner,
) ([]string, error) {
	// Inspect after the linked worktree exists so branch- and gitdir-conditional
	// includes match the same checkout ordinary Git will use. Preserve explicit
	// global/system config selectors while removing only repository bindings.
	runner.Env = withoutGitRepositoryBindings(runner.Env)
	runner.StripEnv = false
	runner.NullGlobalConfig = false
	runner.NoSystemConfig = false
	out, err := runLifecycleGitWithRunner(
		ctx, runner, worktreePath, "config", "--null", "--name-only", "--list",
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
			gitcmd.Config{Key: prefix + ".textconv", Value: "cat"},
		)
	}
	for _, driver := range sortedDriverNames(merges) {
		config = append(config, gitcmd.Config{
			Key: "merge." + driver + ".driver", Value: "false",
		})
	}
	return config
}

func configuredDriverName(key string, suffixes []string) (string, bool) {
	lower := strings.ToLower(key)
	for _, suffix := range suffixes {
		if strings.HasSuffix(lower, suffix) {
			driver := key[strings.IndexByte(key, '.')+1 : len(key)-len(suffix)]
			return driver, driver != ""
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
