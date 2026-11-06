package agenthook

import (
	"errors"
	"runtime"
)

func hookExecutable(handler map[string]any) (string, error) {
	command, ok := handler["command"].(string)
	if !ok || command == "" {
		return "", errors.New("hook has no command executable")
	}
	if raw, exists := handler["args"]; exists {
		args, ok := raw.([]any)
		if !ok {
			return "", errors.New("hook arguments must be an array")
		}
		for _, arg := range args {
			if _, ok := arg.(string); !ok {
				return "", errors.New("hook arguments must be strings")
			}
		}
		return command, nil
	}
	if runtime.GOOS == "windows" {
		if override, ok := handler["commandWindows"].(string); ok && override != "" {
			command = override
		}
	}
	return rawHookExecutable(command)
}
