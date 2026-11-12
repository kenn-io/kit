package agenthook

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

func rawHookWords(command, commandWindows string) ([]string, error) {
	if commandWindows == "" {
		return posixHookWords(command)
	}
	quoted, backslashes := false, 0
	for _, char := range commandWindows {
		if char == '"' && backslashes%2 == 0 {
			quoted = !quoted
		}
		if (!quoted && strings.ContainsRune(";|&<>~*?{}[]()\n", char)) || strings.ContainsRune("$`%", char) {
			return nil, errors.New("hook command requires shell interpretation")
		}
		if char == '\\' {
			backslashes++
		} else {
			backslashes = 0
		}
	}

	words, err := windows.DecomposeCommandLine(commandWindows)
	if err != nil || len(words) == 0 {
		return nil, fmt.Errorf("cannot identify hook executable in %q", commandWindows)
	}
	return words, nil
}
