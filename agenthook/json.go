package agenthook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

func planNestedJSONConfig(
	path, marker, command, commandWindows string,
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

	hooksObject, err := jsonHooksObject(root, path, !uninstall)
	if err != nil {
		return nil, false, err
	}
	if hooksObject != nil {
		if err := removeOwnedJSONHooks(hooksObject, marker, path); err != nil {
			return nil, false, err
		}
	}
	if !uninstall {
		if hooksObject == nil {
			return nil, false, fmt.Errorf("agent hook config %s has no hooks object", path)
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
			if commandWindows != "" {
				handler["commandWindows"] = commandWindows
			}
			if hook.timeout > 0 {
				handler["timeout"] = hook.timeout
			}
			entry["hooks"] = []any{handler}
			event := hook.name
			entries, err := jsonEventEntries(hooksObject, event, path)
			if err != nil {
				return nil, false, err
			}
			hooksObject[event] = append(entries, entry)
		}
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

func readJSONConfig(path string) (map[string]any, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read agent hook config %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]any{}, true, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, false, fmt.Errorf("decode agent hook config %s: %w", path, err)
	}
	if root == nil {
		root = map[string]any{}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("multiple JSON values")
		}
		return nil, false, fmt.Errorf("decode agent hook config %s: %w", path, err)
	}
	return root, true, nil
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

func removeOwnedJSONHooks(hooks map[string]any, marker, path string) error {
	for event, raw := range hooks {
		entries, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("agent hook config %s event %q must be an array", path, event)
		}
		keptEntries := make([]any, 0, len(entries))
		for _, rawEntry := range entries {
			entry, ok := rawEntry.(map[string]any)
			if !ok {
				keptEntries = append(keptEntries, rawEntry)
				continue
			}
			rawHandlers, ok := entry["hooks"]
			if !ok || rawHandlers == nil {
				keptEntries = append(keptEntries, rawEntry)
				continue
			}
			handlers, ok := rawHandlers.([]any)
			if !ok {
				return fmt.Errorf("agent hook config %s event %q entry hooks must be an array", path, event)
			}
			keptHandlers := make([]any, 0, len(handlers))
			for _, rawHandler := range handlers {
				handler, ok := rawHandler.(map[string]any)
				command, _ := handler["command"].(string)
				if ok && strings.Contains(command, marker) {
					continue
				}
				keptHandlers = append(keptHandlers, rawHandler)
			}
			if len(keptHandlers) == 0 {
				continue
			}
			entry["hooks"] = keptHandlers
			keptEntries = append(keptEntries, entry)
		}
		if len(keptEntries) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = keptEntries
		}
	}
	return nil
}
