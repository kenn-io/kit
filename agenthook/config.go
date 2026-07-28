package agenthook

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Hook describes one command registration using Claude Code event and matcher
// names. An empty matcher observes every invocation of the event. A zero timeout
// omits the timeout field and lets the harness use its default.
type Hook struct {
	Event   Event
	Matcher string
	Timeout time.Duration
}

// InstallOptions describes an application's hooks. Set Executable and Arguments
// to have kit construct commands for every supported shell. Command,
// CommandWindows, and CommandPowerShell are raw overrides for the native,
// Win32 argv, and PowerShell forms respectively; do not combine them with
// Executable. Marker must be a stable, application-namespaced substring unique
// to commands the caller owns; it identifies those commands across binary path
// changes. Hooks defaults to every event supported by the selected profile.
type InstallOptions struct {
	ConfigPath        string
	Executable        string
	Arguments         []string
	Command           string
	CommandWindows    string
	CommandPowerShell string
	Marker            string
	Hooks             []Hook
}

// Result reports the planned or completed config mutation. Data contains the
// complete resulting config and can be used by command-line dump operations.
type Result struct {
	Agent      Agent
	ConfigPath string
	Changed    bool
	Data       []byte
}

// PlanInstall computes an install without writing the config file.
func PlanInstall(agent Agent, opts InstallOptions) (Result, error) {
	opts.Marker = strings.TrimSpace(opts.Marker)
	commands, err := resolveCommands(opts)
	if err != nil {
		return Result{}, err
	}
	spec, ok := profiles[agent]
	if !ok {
		return Result{}, fmt.Errorf("unsupported agent hook integration %q", agent)
	}
	opts.Command, opts.CommandWindows = profileCommands(spec, commands)
	spec, path, hooks, err := prepareInstall(agent, opts)
	if err != nil {
		return Result{}, err
	}
	data, changed, err := planConfig(
		spec, path, opts.Marker, opts.Command, opts.CommandWindows, hooks, false,
	)
	if err != nil {
		return Result{}, err
	}
	return Result{Agent: agent, ConfigPath: path, Changed: changed, Data: data}, nil
}

// Install merges the application's hooks into the selected agent config.
// Callers must serialize concurrent mutations of the same config path.
func Install(agent Agent, opts InstallOptions) (Result, error) {
	result, err := PlanInstall(agent, opts)
	if err != nil || !result.Changed {
		return result, err
	}
	if err := writeConfig(result.ConfigPath, result.Data); err != nil {
		return Result{}, err
	}
	return result, nil
}

// PlanUninstall computes removal of every command containing marker without
// writing the config file. A missing config is a successful no-op.
func PlanUninstall(agent Agent, configPath, marker string) (Result, error) {
	spec, ok := profiles[agent]
	if !ok {
		return Result{}, fmt.Errorf("unsupported agent hook integration %q", agent)
	}
	marker = strings.TrimSpace(marker)
	if marker == "" {
		return Result{}, errors.New("agent hook marker is required")
	}
	path, err := resolveConfigPath(agent, configPath)
	if err != nil {
		return Result{}, err
	}
	data, changed, err := planConfig(spec, path, marker, "", "", nil, true)
	if err != nil {
		return Result{}, err
	}
	return Result{Agent: agent, ConfigPath: path, Changed: changed, Data: data}, nil
}

// Uninstall removes every command containing marker from the selected agent
// config while preserving hooks owned by other applications. Callers must
// serialize concurrent mutations of the same config path.
func Uninstall(agent Agent, configPath, marker string) (Result, error) {
	result, err := PlanUninstall(agent, configPath, marker)
	if err != nil || !result.Changed {
		return result, err
	}
	if err := writeConfig(result.ConfigPath, result.Data); err != nil {
		return Result{}, err
	}
	return result, nil
}

func prepareInstall(agent Agent, opts InstallOptions) (profileSpec, string, []nativeHook, error) {
	spec, ok := profiles[agent]
	if !ok {
		return profileSpec{}, "", nil, fmt.Errorf("unsupported agent hook integration %q", agent)
	}
	command := strings.TrimSpace(opts.Command)
	if command == "" {
		return profileSpec{}, "", nil, errors.New("agent hook command is required")
	}
	marker := strings.TrimSpace(opts.Marker)
	if marker == "" {
		return profileSpec{}, "", nil, errors.New("agent hook marker is required")
	}
	if !strings.Contains(command, marker) {
		return profileSpec{}, "", nil, errors.New("agent hook command must contain its ownership marker")
	}
	path, err := resolveConfigPath(agent, opts.ConfigPath)
	if err != nil {
		return profileSpec{}, "", nil, err
	}

	hooks := opts.Hooks
	if len(hooks) == 0 {
		hooks = make([]Hook, 0, len(spec.profile.SupportedEvents))
		for _, event := range spec.profile.SupportedEvents {
			hooks = append(hooks, Hook{Event: event})
		}
	}
	nativeHooks := make([]nativeHook, 0, len(hooks))
	seen := make(map[string]struct{}, len(hooks))
	for _, hook := range hooks {
		if !slices.Contains(spec.profile.SupportedEvents, hook.Event) {
			return profileSpec{}, "", nil, fmt.Errorf("%s does not support %s hooks", spec.profile.DisplayName, hook.Event)
		}
		if hook.Timeout < 0 || hook.Timeout%spec.timeoutUnit != 0 {
			return profileSpec{}, "", nil, fmt.Errorf(
				"%s hook timeout must be a non-negative multiple of %s",
				hook.Event, spec.timeoutUnit,
			)
		}
		timeout := int(hook.Timeout / spec.timeoutUnit)
		if spec.format == formatHermesYAML && hook.Timeout > 300*time.Second {
			return profileSpec{}, "", nil, fmt.Errorf("Hermes hook timeout must not exceed 300 seconds")
		}
		matcher := nativeMatcher(spec, strings.TrimSpace(hook.Matcher))
		if spec.format == formatHermesYAML && matcher != "" &&
			hook.Event != EventPreToolUse && hook.Event != EventPostToolUse {
			return profileSpec{}, "", nil, fmt.Errorf(
				"Hermes only supports matchers on PreToolUse and PostToolUse hooks",
			)
		}
		key := string(hook.Event) + "\x00" + matcher
		if _, exists := seen[key]; exists {
			return profileSpec{}, "", nil, fmt.Errorf("duplicate %s hook matcher %q", hook.Event, hook.Matcher)
		}
		seen[key] = struct{}{}
		nativeHooks = append(nativeHooks, nativeHook{
			event: hook.Event, name: nativeEventName(spec, hook.Event),
			matcher: matcher, timeout: timeout,
		})
	}
	return spec, path, nativeHooks, nil
}

type nativeHook struct {
	event   Event
	name    string
	matcher string
	timeout int
}

func resolveConfigPath(agent Agent, override string) (string, error) {
	if path := strings.TrimSpace(override); path != "" {
		return path, nil
	}
	return ConfigPath(agent)
}

func planConfig(
	spec profileSpec,
	path, marker, command, commandWindows string,
	hooks []nativeHook,
	uninstall bool,
) ([]byte, bool, error) {
	switch spec.format {
	case formatNestedJSON:
		return planNestedJSONConfig(path, marker, command, commandWindows, hooks, uninstall)
	case formatDirectJSON:
		return planDirectJSONConfig(
			spec, path, marker, command, commandWindows, hooks, uninstall,
		)
	case formatHermesYAML:
		return planHermesConfig(path, marker, command, hooks, uninstall)
	default:
		return nil, false, errors.New("unsupported agent hook config format")
	}
}

func writeConfig(path string, data []byte) error {
	writePath := path
	info, err := os.Lstat(path)
	mode := os.FileMode(0o600)
	switch {
	case err == nil && info.Mode()&os.ModeSymlink != 0:
		writePath, err = filepath.EvalSymlinks(path)
		if err != nil {
			return fmt.Errorf("resolve agent hook config symlink %s: %w", path, err)
		}
		if targetInfo, statErr := os.Stat(writePath); statErr == nil {
			mode = targetInfo.Mode().Perm()
		}
	case err == nil:
		mode = info.Mode().Perm()
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("inspect agent hook config %s: %w", path, err)
	}
	dir := filepath.Dir(writePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create agent hook config directory %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(writePath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary agent hook config: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	closeWithError := func(writeErr error) error {
		_ = tmp.Close()
		return writeErr
	}
	if err := tmp.Chmod(mode); err != nil {
		return closeWithError(fmt.Errorf("set temporary agent hook config mode: %w", err))
	}
	if _, err := bytes.NewReader(data).WriteTo(tmp); err != nil {
		return closeWithError(fmt.Errorf("write temporary agent hook config: %w", err))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary agent hook config: %w", err)
	}
	if err := replaceConfigFile(tmpPath, writePath); err != nil {
		return fmt.Errorf("replace agent hook config %s: %w", path, err)
	}
	return nil
}
