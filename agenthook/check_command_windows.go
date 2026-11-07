package agenthook

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

func rawHookExecutable(command, commandWindows string) (string, error) {
	if commandWindows != "" {
		return windowsHookExecutable(commandWindows)
	}
	return posixHookExecutable(command)
}

func windowsHookExecutable(command string) (string, error) {
	quoted, backslashes := false, 0
	for _, char := range command {
		if char == '"' && backslashes%2 == 0 {
			quoted = !quoted
		}
		if (!quoted && strings.ContainsRune(";|&<>~*?{}[]()\n", char)) || strings.ContainsRune("$`%", char) {
			return "", errors.New("hook command requires shell interpretation")
		}
		if char == '\\' {
			backslashes++
		} else {
			backslashes = 0
		}
	}

	words, err := windows.DecomposeCommandLine(command)
	if err != nil || len(words) == 0 {
		return "", fmt.Errorf("cannot identify hook executable in %q", command)
	}
	return words[0], nil
}
