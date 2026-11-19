package agenthook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

func planNestedJSONConfig(
	spec profileSpec,
	path, marker, command, commandWindows string,
	argv []string,
	hooks []nativeHook,
	uninstall bool, dir string,
) ([]byte, bool, error) {
	root, exists, err := readJSONConfig(path)
	if err != nil {
		return nil, false, err
	}
	before, err := marshalJSONConfig(root)
	if err != nil {
		return nil, false, fmt.Errorf("encode existing agent hook config %s: %w", path, err)
	}
	if !uninstall && len(root) == 0 {
		maps.Copy(root, spec.newConfigFields)
	}
	if err := applyNestedJSONHooks(
		root, path, marker, command, commandWindows, argv, hooks, uninstall, dir, spec.profile.Agent == AgentClaude || spec.profile.Agent == AgentCodex,
	); err != nil {
		return nil, false, err
	}
	after, err := marshalJSONConfig(root)
	if err != nil {
		return nil, false, fmt.Errorf("encode agent hook config %s: %w", path, err)
	}
	changed := !bytes.Equal(before, after)
	if uninstall && !exists {
		changed = false
	}
	return after, changed, nil
}

// applyNestedJSONHooks replaces the commands owned by marker in root's
// Claude-style hooks object, or removes them on uninstall.
func applyNestedJSONHooks(
	root map[string]any,
	path, marker, command, commandWindows string,
	argv []string,
	hooks []nativeHook,
	uninstall bool, dir string, preserve bool,
) error {
	hooksObject, err := jsonHooksObject(root, path, !uninstall)
	if err != nil {
		return err
	}
	if hooksObject == nil {
		return nil
	}
	desired := map[string][]map[string]any{}
	if uninstall {
		return reconcileOwnedJSONHooks(hooksObject, desired, marker, path, dir, preserve)
	}
	for _, hook := range hooks {
		entry := map[string]any{}
		if hook.matcher != "" {
			entry["matcher"] = hook.matcher
		}
		handler := map[string]any{
			"type":    "command",
			"command": command,
		}
		if len(argv) > 0 {
			handler["command"] = argv[0]
			handler["args"] = argv[1:]
		} else if commandWindows != "" {
			handler["commandWindows"] = commandWindows
		}
		if hook.timeout > 0 {
			handler["timeout"] = hook.timeout
		}
		entry["hooks"] = []any{handler}
		desired[hook.name] = append(desired[hook.name], entry)
	}
	return reconcileOwnedJSONHooks(hooksObject, desired, marker, path, dir, preserve)
}

func readJSONConfig(path string) (map[string]any, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read agent hook config %s: %w", path, err)
	}
	root, err := decodeJSONConfig(path, data)
	if err != nil {
		return nil, false, err
	}
	return root, true, nil
}

func decodeJSONConfig(path string, data []byte) (map[string]any, error) {
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, fmt.Errorf("decode agent hook config %s: %w", path, err)
	}
	if root == nil {
		root = map[string]any{}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return nil, fmt.Errorf("decode agent hook config %s: %w", path, err)
	}
	return root, nil
}

func marshalJSONConfig(root map[string]any) ([]byte, error) {
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func jsonHooksObject(root map[string]any, path string, create bool) (map[string]any, error) {
	raw, ok := root["hooks"]
	if !ok || raw == nil {
		if !create {
			return nil, nil
		}
		hooks := map[string]any{}
		root["hooks"] = hooks
		return hooks, nil
	}
	hooks, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("agent hook config %s field %q must be an object", path, "hooks")
	}
	return hooks, nil
}

func jsonEventEntries(hooks map[string]any, event, path string) ([]any, error) {
	raw, ok := hooks[event]
	if !ok || raw == nil {
		return nil, nil
	}
	entries, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("agent hook config %s event %q must be an array", path, event)
	}
	return entries, nil
}

// reconcileOwnedJSONHooks matches event/matcher registrations first, then uses
// owned single-handler groups for changed matchers before appending registrations.
// A slot keeps its handler when only a resolving executable path differs.
func reconcileOwnedJSONHooks(hooks map[string]any, desired map[string][]map[string]any, marker, path, dir string, preserve bool) error {
	for event, raw := range hooks {
		entries, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("agent hook config %s event %q must be an array", path, event)
		}
		type slot struct {
			entry    map[string]any
			handlers []any
			index    int
			desired  map[string]any
		}
		var slots []slot
		removed := new(struct{})
		used := make([]bool, len(desired[event]))
		for _, rawEntry := range entries {
			entry, ok := rawEntry.(map[string]any)
			if !ok || entry["hooks"] == nil {
				continue
			}
			handlers, ok := entry["hooks"].([]any)
			if !ok {
				return fmt.Errorf("agent hook config %s event %q entry hooks must be an array", path, event)
			}
			for i, handler := range handlers {
				if !ownsJSONHandler(handler, marker) {
					continue
				}
				slots = append(slots, slot{entry: entry, handlers: handlers, index: i})
			}
		}
		// Reserve matching moved handlers before assigning free slots.
		for _, strict := range []bool{preserve, false} {
			for i := range slots {
				owned := &slots[i]
				if owned.desired != nil {
					continue
				}
				existing, _ := owned.handlers[owned.index].(map[string]any)
				for j, hook := range desired[event] {
					matcher, _ := hook["matcher"].(string)
					existingMatcher, _ := owned.entry["matcher"].(string)
					desiredHandler := hook["hooks"].([]any)[0].(map[string]any)
					if !used[j] && matcher == existingMatcher && (!strict || sameMovedHook(existing, desiredHandler, dir)) {
						owned.desired, used[j] = hook, true
						break
					}
				}
			}
		}
		for _, owned := range slots {
			if owned.desired == nil && len(owned.handlers) == 1 {
				for j, hook := range desired[event] {
					if !used[j] {
						owned.desired = hook
						used[j] = true
						if matcher, ok := hook["matcher"]; ok {
							owned.entry["matcher"] = matcher
						} else {
							delete(owned.entry, "matcher")
						}
						break
					}
				}
			}
			existing, _ := owned.handlers[owned.index].(map[string]any)
			owned.handlers[owned.index] = removed
			if owned.desired != nil {
				desiredHandler := owned.desired["hooks"].([]any)[0].(map[string]any)
				if preserve && sameMovedHook(existing, desiredHandler, dir) {
					owned.handlers[owned.index] = existing
				} else {
					owned.handlers[owned.index] = desiredHandler
				}
			}
		}
		keptEntries := make([]any, 0, len(entries))
		for _, rawEntry := range entries {
			entry, ok := rawEntry.(map[string]any)
			if ok && entry["hooks"] != nil {
				handlers := entry["hooks"].([]any)
				kept := make([]any, 0, len(handlers))
				for _, handler := range handlers {
					if handler != removed {
						kept = append(kept, handler)
					}
				}
				if len(kept) == 0 && len(handlers) > 0 {
					continue
				}
				entry["hooks"] = kept
			}
			keptEntries = append(keptEntries, rawEntry)
		}
		for i, hook := range desired[event] {
			if !used[i] {
				keptEntries = append(keptEntries, hook)
			}
		}
		if len(keptEntries) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = keptEntries
		}
		delete(desired, event)
	}
	for event, entries := range desired {
		for _, entry := range entries {
			current, _ := hooks[event].([]any)
			hooks[event] = append(current, entry)
		}
	}
	return nil
}

func ownsJSONHandler(raw any, marker string) bool {
	handler, ok := raw.(map[string]any)
	if !ok {
		return false
	}
	command, _ := handler["command"].(string)
	// Exec-form hooks keep their arguments in a separate array.
	if args, ok := handler["args"].([]any); ok {
		var joined strings.Builder
		joined.WriteString(command)
		for _, arg := range args {
			text, _ := arg.(string)
			joined.WriteString(" " + text)
		}
		command = joined.String()
	}
	return strings.Contains(command, marker)
}

// lookupHookExecutable resolves a relative executable path from dir, or from
// the process directory when dir is empty, and other names through PATH,
// whose relative entries also resolve from dir.
func lookupHookExecutable(dir, executable string) error {
	if !filepath.IsAbs(executable) && strings.ContainsAny(executable, `/\`) {
		path, err := filepath.Abs(filepath.Join(dir, executable))
		if err != nil {
			return err
		}
		executable = path
	}
	return searchHookPath(dir, executable)
}

// searchHookPath looks up executable as given when it has a path separator,
// and otherwise in each PATH entry, resolving relative entries from dir.
func searchHookPath(dir, executable string) error {
	if strings.ContainsAny(executable, `/\`) {
		_, err := exec.LookPath(executable)
		return err
	}
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		path := filepath.Join(entry, executable)
		if !filepath.IsAbs(entry) {
			abs, err := filepath.Abs(filepath.Join(dir, path))
			if err != nil {
				continue
			}
			path = abs
		}
		if _, err := exec.LookPath(path); err == nil {
			return nil
		}
	}
	return &exec.Error{Name: executable, Err: exec.ErrNotFound}
}

// sameMovedHook keeps an owned handler whose only difference from desired is
// an executable path that resolves from dir.
func sameMovedHook(existing, desired map[string]any, dir string) bool {
	oldWords, err := hookWords(existing)
	if err != nil {
		return false
	}
	newWords, err := hookWords(desired)
	if err != nil || !slices.Equal(oldWords[1:], newWords[1:]) {
		return false
	}
	if !sameInactiveArguments(existing, desired, oldWords[0], newWords[0]) {
		return false
	}
	oldCopy, newCopy := maps.Clone(existing), maps.Clone(desired)
	for _, key := range []string{"command", "commandWindows", "args"} {
		delete(oldCopy, key)
		delete(newCopy, key)
	}
	oldJSON, oldErr := json.Marshal(oldCopy)
	newJSON, newErr := json.Marshal(newCopy)
	if oldErr != nil || newErr != nil || !bytes.Equal(oldJSON, newJSON) {
		return false
	}
	return lookupHookExecutable(dir, oldWords[0]) == nil
}

// sameInactiveArguments reports whether the other platform's command, which
// this platform never runs, is unchanged or names the same moved executable as
// the active command with the same arguments, so a stale or replaced override
// gets rewritten.
func sameInactiveArguments(existing, desired map[string]any, oldExecutable, newExecutable string) bool {
	if _, exec := existing["args"]; exec {
		// An exec-form handler has no shell command to compare.
		return true
	}
	key, words := "commandWindows", powerShellHookWords
	if runtime.GOOS == "windows" {
		key, words = "command", posixHookWords
	}
	oldCommand, _ := existing[key].(string)
	newCommand, _ := desired[key].(string)
	if oldCommand == newCommand {
		return true
	}
	oldWords, oldErr := words(oldCommand)
	newWords, newErr := words(newCommand)
	return oldErr == nil && newErr == nil && slices.Equal(oldWords[1:], newWords[1:]) &&
		oldWords[0] == oldExecutable && newWords[0] == newExecutable
}

// hookWords returns a handler's executable followed by its arguments.
func hookWords(handler map[string]any) ([]string, error) {
	command, _ := handler["command"].(string)
	args, ok := handler["args"]
	if !ok {
		windows, _ := handler["commandWindows"].(string)
		return rawHookWords(command, windows)
	}
	words := []string{command}
	switch values := args.(type) {
	case []string:
		words = append(words, values...)
	case []any:
		for _, value := range values {
			arg, ok := value.(string)
			if !ok {
				return nil, errors.New("hook arguments must be strings")
			}
			words = append(words, arg)
		}
	default:
		return nil, errors.New("hook arguments must be an array")
	}
	return words, nil
}
