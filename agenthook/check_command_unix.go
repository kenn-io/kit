//go:build !windows

package agenthook

import (
	"errors"
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/shell"
)

func rawHookExecutable(command string) (string, error) {
	// Shell expansion, redirects, and compound commands cannot be inspected by
	// splitting words. Never evaluate them while checking a hook.
	if strings.ContainsAny(command, "$`;|&<>~*?{}[]()\n") {
		return "", errors.New("hook command requires shell interpretation")
	}
	words, err := shell.Fields(command, func(string) string { return "" })
	if err != nil || len(words) == 0 {
		return "", fmt.Errorf("cannot identify hook executable in %q", command)
	}
	return words[0], nil
}
