package agenthook

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// planAgyJSONConfig writes agy's hooks file, whose top-level keys name hook
// groups. Tool events hold matcher groups and the other events hold handlers
// directly: https://antigravity.google/docs/hooks/
// Install keeps its hooks in one group named after the marker; uninstall
// removes marker commands from every group.
func planAgyJSONConfig(path, marker, command string, hooks []nativeHook, uninstall bool) ([]byte, bool, error) {
	root, exists, err := readJSONConfig(path)
	if err != nil {
		return nil, false, err
	}
	before, err := marshalJSONConfig(root)
	if err != nil {
		return nil, false, fmt.Errorf("encode existing agent hook config %s: %w", path, err)
	}
	for name, raw := range root {
		group, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		removed, err := removeOwnedAgyHooks(group, marker, path, name)
		if err != nil {
			return nil, false, err
		}
		// Install refills its own group, which keeps the user's enabled
		// setting.
		if removed && agyGroupEmpty(group) && (uninstall || name != agyGroupName(marker)) {
			delete(root, name)
		}
	}
	if !uninstall {
		name := agyGroupName(marker)
		group := map[string]any{}
		if raw, exists := root[name]; exists {
			var ok bool
			if group, ok = raw.(map[string]any); !ok {
				return nil, false, fmt.Errorf("agent hook config %s hook %q must be an object", path, name)
			}
		}
		root[name] = group
		for _, hook := range hooks {
			handler := map[string]any{"type": "command", "command": command}
			if hook.timeout > 0 {
				handler["timeout"] = hook.timeout
			}
			entry := handler
			if agyToolEvent(hook.name) {
				matcher := hook.matcher
				if matcher == "" {
					matcher = "*"
				}
				entry = map[string]any{"matcher": matcher, "hooks": []any{handler}}
			} else if hook.matcher != "" {
				return nil, false, errors.New("agy only supports matchers on PreToolUse and PostToolUse hooks")
			}
			entries, _ := group[hook.name].([]any)
			group[hook.name] = append(entries, entry)
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

func agyToolEvent(name string) bool {
	return name == "PreToolUse" || name == "PostToolUse"
}

// removeOwnedAgyHooks drops marker commands from one group, both direct
// handlers and handlers inside matcher groups, and reports whether it
// removed any.
func removeOwnedAgyHooks(group map[string]any, marker, path, name string) (bool, error) {
	removed := false
	for event, raw := range group {
		if event == "enabled" {
			continue
		}
		entries, ok := raw.([]any)
		if !ok {
			return false, fmt.Errorf("agent hook config %s hook %q event %q must be an array", path, name, event)
		}
		kept := make([]any, 0, len(entries))
		for _, rawEntry := range entries {
			entry, _ := rawEntry.(map[string]any)
			handlers, nested := entry["hooks"].([]any)
			if !nested {
				if ownsJSONHandler(rawEntry, marker) {
					removed = true
					continue
				}
				kept = append(kept, rawEntry)
				continue
			}
			keptHandlers := make([]any, 0, len(handlers))
			for _, handler := range handlers {
				if ownsJSONHandler(handler, marker) {
					removed = true
					continue
				}
				keptHandlers = append(keptHandlers, handler)
			}
			if len(keptHandlers) == 0 && len(handlers) > 0 {
				continue
			}
			entry["hooks"] = keptHandlers
			kept = append(kept, entry)
		}
		if len(kept) == 0 {
			delete(group, event)
		} else {
			group[event] = kept
		}
	}
	return removed, nil
}

func agyGroupEmpty(group map[string]any) bool {
	for key := range group {
		if key != "enabled" {
			return false
		}
	}
	return true
}

// agyGroupName turns a marker such as "--source example" into a readable
// group name such as "source-example".
func agyGroupName(marker string) string {
	var name strings.Builder
	dash := false
	for _, r := range strings.ToLower(marker) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if dash && name.Len() > 0 {
				name.WriteByte('-')
			}
			name.WriteRune(r)
			dash = false
			continue
		}
		dash = true
	}
	if name.Len() == 0 {
		return "agenthook"
	}
	return name.String()
}
