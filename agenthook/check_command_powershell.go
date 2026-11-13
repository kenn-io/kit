package agenthook

import (
	"errors"
	"strings"
)

// powerShellHookWords accepts literal words without evaluating shell syntax.
func powerShellHookWords(command string) ([]string, error) {
	const shellSyntax = "$`\";|&<>(){}@,#\r\n"
	fail := func() ([]string, error) {
		return nil, errors.New("hook command requires shell interpretation")
	}
	command = strings.TrimLeft(command, " \t")
	if strings.HasPrefix(command, "& ") || strings.HasPrefix(command, "&\t") {
		command = strings.TrimLeft(command[1:], " \t")
	}
	var words []string
	for len(command) > 0 {
		var word strings.Builder
		started := false
		for len(command) > 0 && command[0] != ' ' && command[0] != '\t' {
			char := command[0]
			if char == '\'' {
				started = true
				command = command[1:]
				closed := false
				for len(command) > 0 {
					if command[0] == '\'' {
						command = command[1:]
						if len(command) > 0 && command[0] == '\'' {
							word.WriteByte('\'')
							command = command[1:]
							continue
						}
						closed = true
						break
					}
					word.WriteByte(command[0])
					command = command[1:]
				}
				if !closed {
					return fail()
				}
				continue
			}
			if strings.ContainsRune(shellSyntax, rune(char)) || (!started && char == '~') {
				return fail()
			}
			started = true
			word.WriteByte(char)
			command = command[1:]
		}
		words = append(words, word.String())
		command = strings.TrimLeft(command, " \t")
	}
	if len(words) == 0 || words[0] == "" {
		return fail()
	}
	return words, nil
}
