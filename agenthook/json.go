package agenthook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"strings"
)

func planNestedJSONConfig(
	spec profileSpec,
	path, marker, command, commandWindows string,
	argv []string,
	hooks []nativeHook,
	uninstall bool,
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
		root, path, marker, command, commandWindows, argv, hooks, uninstall,
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
	uninstall bool,
) error {
	hooksObject, err := jsonHooksObject(root, path, !uninstall)
	if err != nil {
		return err
	}
	desired := map[string][]map[string]any{}
	if uninstall {
		return reconcileOwnedJSONHooks(hooksObject, desired, marker, path, false)
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
	return reconcileOwnedJSONHooks(hooksObject, desired, marker, path, true)
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
func reconcileOwnedJSONHooks(hooks map[string]any, desired map[string][]map[string]any, marker, path string, preservePositions bool) error {
	for event, raw := range hooks {
		entries, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("agent hook config %s event %q must be an array", path, event)
		}
		type slot struct {
			entry    map[string]any
			handlers []any
			index    int
			desired  int
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
				owned := slot{entry: entry, handlers: handlers, index: i, desired: -1}
				for j, hook := range desired[event] {
					matcher, _ := hook["matcher"].(string)
					existingMatcher, _ := entry["matcher"].(string)
					if !used[j] && matcher == existingMatcher {
						owned.desired = j
						used[j] = true
						break
					}
				}
				slots = append(slots, owned)
			}
		}
		for _, owned := range slots {
			if owned.desired < 0 && len(owned.handlers) == 1 {
				for j := range used {
					if !used[j] {
						owned.desired = j
						used[j] = true
						break
					}
				}
			}
			owned.handlers[owned.index] = removed
			if owned.desired >= 0 {
				hook := desired[event][owned.desired]
				if matcher, ok := hook["matcher"]; ok {
					owned.entry["matcher"] = matcher
				} else {
					delete(owned.entry, "matcher")
				}
				owned.handlers[owned.index] = hook["hooks"].([]any)[0]
			}
		}
		keptEntries := make([]any, 0, len(entries))
		shiftError := fmt.Errorf("agent hook config %s event %q: reinstall would shift existing hook positions", path, event)
		for entryIndex, rawEntry := range entries {
			entry, ok := rawEntry.(map[string]any)
			if ok && entry["hooks"] != nil {
				handlers := entry["hooks"].([]any)
				kept := make([]any, 0, len(handlers))
				for handlerIndex, handler := range handlers {
					if handler != removed {
						if preservePositions && len(kept) != handlerIndex {
							return shiftError
						}
						kept = append(kept, handler)
					}
				}
				if len(kept) == 0 && len(handlers) > 0 {
					continue
				}
				entry["hooks"] = kept
			}
			if preservePositions && len(keptEntries) != entryIndex {
				return shiftError
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
