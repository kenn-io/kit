package agenthook

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/windows"
)

func rawHookExecutable(command string) (string, error) {
	if strings.ContainsAny(command, "$`;|&<>~*?{}[]()\n%") {
		return "", errors.New("hook command requires shell interpretation")
	}
	words, err := windows.DecomposeCommandLine(command)
	if err != nil || len(words) == 0 {
		return "", fmt.Errorf("cannot identify hook executable in %q", command)
	}
	return words[0], nil
}
