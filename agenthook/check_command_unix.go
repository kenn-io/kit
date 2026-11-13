//go:build !windows

package agenthook

func rawHookWords(command, _ string) ([]string, error) {
	return posixHookWords(command)
}
